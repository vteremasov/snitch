package wrapper

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// PTYWrapper owns a child process attached to a pseudo-terminal. It copies
// bytes between the parent terminal and the child byte-for-byte; nothing in
// the data path is interpreted, transformed, or buffered beyond what io.Copy
// itself does. Writes to the master are serialized so that auto-yes injection
// in Phase 4 can't tear into a paste from the parent.
type PTYWrapper struct {
	cmd    *exec.Cmd
	master *os.File

	writeMu sync.Mutex

	// Whether the parent stdin is currently inside a bracketed-paste window
	// (\x1b[200~ ... \x1b[201~). Auto-yes injection should not fire while
	// this is true.
	inPaste atomic.Bool

	// Permission-prompt detection state. The transcript jsonl doesn't show
	// `tool_use` until *after* the prompt is resolved, so it can't be the
	// trigger for auto-yes. Instead we sniff the pty master output for the
	// signature claude paints when a permission UI is up. The rolling
	// buffer is touched only from the output read loop, so it needs no
	// mutex.
	promptBuf []byte
	onPrompt  func()
}

// SetOnPrompt installs a callback that fires whenever the permission-prompt
// signature is detected in the pty output. The callback runs synchronously
// from the output read loop, so it must be quick (or spawn its own
// goroutine).
func (w *PTYWrapper) SetOnPrompt(fn func()) { w.onPrompt = fn }

// Start spawns argv[0] with argv[1:] under a freshly allocated pty. The
// child inherits the parent's environment unchanged.
func Start(argv []string) (*PTYWrapper, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	master, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &PTYWrapper{cmd: cmd, master: master}, nil
}

// Run sets the parent terminal to raw mode, wires SIGWINCH/SIGTERM, copies
// bytes both directions, and waits for the child. The returned error is the
// child's wait error (so callers can propagate exit codes).
func (w *PTYWrapper) Run() error {
	parentFD := int(os.Stdin.Fd())

	var prevState *term.State
	if term.IsTerminal(parentFD) {
		s, err := term.MakeRaw(parentFD)
		if err != nil {
			return err
		}
		prevState = s
		defer term.Restore(parentFD, prevState)
	}

	// Initial size sync + SIGWINCH forwarding.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, w.master)
		}
	}()
	winch <- syscall.SIGWINCH

	// Forward terminating signals to the child. Ctrl-C in raw mode does not
	// generate SIGINT for us — it goes through stdin as 0x03 and the child's
	// pty handles it — so we only forward signals delivered to snitch itself.
	// SIGINT is forwarded too so an external `kill -INT` to snitch terminates
	// claude (which closes the pty master and unblocks the foreground copy).
	// Ctrl-C typed at the keyboard never reaches us as a signal — raw mode
	// passes the 0x03 byte through stdin into the pty.
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(term)
	go func() {
		for s := range term {
			if w.cmd.Process != nil {
				_ = w.cmd.Process.Signal(s)
			}
		}
	}()

	// stdin -> pty master, with paste-window tracking + write mutex.
	go w.copyStdinToMaster()

	// pty master -> stdout, sniffing the byte stream for the permission-
	// prompt signature inline. Bytes flow to the terminal unchanged.
	w.copyMasterToStdoutAndScan()

	return w.cmd.Wait()
}

// Permission-prompt signature. Claude paints a small menu like
//
//	❯ 1. Yes
//	  2. Yes, and don't ask again (or similar)
//	  3. No
//
// We require all three of `❯`, `Yes`, and `No` to be present in the
// rolling buffer simultaneously. The trio is unlikely to coincide outside
// the permission UI, and any number of intervening color/positioning
// escapes between them is fine — we only check substring presence.
//
// Multiple "Yes" lines may appear in the menu; we don't care which we
// matched, since pressing Enter always selects option 1 (the first "Yes").
var (
	sigCursor = []byte("❯")
	sigYes    = []byte("Yes")
	sigNo     = []byte("No")
)

const promptBufCap = 4096

func (w *PTYWrapper) copyMasterToStdoutAndScan() {
	buf := make([]byte, 8192)
	for {
		n, err := w.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = os.Stdout.Write(chunk)
			w.scanForPrompt(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (w *PTYWrapper) scanForPrompt(chunk []byte) {
	if w.onPrompt == nil {
		return
	}
	w.promptBuf = append(w.promptBuf, chunk...)
	if len(w.promptBuf) > promptBufCap {
		w.promptBuf = w.promptBuf[len(w.promptBuf)-promptBufCap:]
	}
	if bytes.Contains(w.promptBuf, sigCursor) &&
		bytes.Contains(w.promptBuf, sigYes) &&
		bytes.Contains(w.promptBuf, sigNo) {
		// Clear so the same render doesn't fire again on the next chunk.
		// A new prompt will repaint the signature into a fresh buffer.
		w.promptBuf = w.promptBuf[:0]
		w.onPrompt()
	}
}

func (w *PTYWrapper) copyStdinToMaster() {
	buf := make([]byte, 8192)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			w.observePasteWindow(buf[:n])
			w.writeMu.Lock()
			_, werr := w.master.Write(buf[:n])
			w.writeMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Bracketed paste delimiters. We only *observe* the byte stream — we never
// transform it — so the user's paste reaches claude verbatim.
var (
	pasteOn  = []byte("\x1b[200~")
	pasteOff = []byte("\x1b[201~")
)

func (w *PTYWrapper) observePasteWindow(b []byte) {
	if bytes.Contains(b, pasteOn) {
		w.inPaste.Store(true)
	}
	if bytes.Contains(b, pasteOff) {
		w.inPaste.Store(false)
	}
}

// Inject writes raw bytes to the pty master. Used by auto-yes / approve-now.
// Refuses to write while the parent stdin is inside a bracketed-paste window.
func (w *PTYWrapper) Inject(b []byte) error {
	if w.inPaste.Load() {
		return errors.New("inject suppressed: stdin is mid-paste")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_, err := w.master.Write(b)
	return err
}

// PID returns the child process pid (the claude process).
func (w *PTYWrapper) PID() int {
	if w.cmd.Process != nil {
		return w.cmd.Process.Pid
	}
	return 0
}

// ExitCode is meaningful after Run returns.
func (w *PTYWrapper) ExitCode() int {
	if w.cmd.ProcessState != nil {
		return w.cmd.ProcessState.ExitCode()
	}
	return -1
}
