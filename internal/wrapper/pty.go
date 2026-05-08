package wrapper

import (
	"bytes"
	"errors"
	"io"
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
}

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

	// pty master -> stdout: pure passthrough.
	_, _ = io.Copy(os.Stdout, w.master)

	return w.cmd.Wait()
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
