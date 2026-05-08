package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"snitch/internal/state"
	"snitch/internal/transcript"
)

// Run is the entry point for `snitch run`. extraArgs is the argv passed to the
// claude binary.
func Run(ctx context.Context, extraArgs []string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude binary not found in PATH: %w", err)
	}

	w, err := Start(append([]string{bin}, extraArgs...))
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}

	wrapperPID := os.Getpid()
	claudePID := w.PID()
	socketPath := state.SocketPath(wrapperPID)

	logger, closeLog := openDebugLog(wrapperPID)
	defer closeLog()

	sess := &state.Session{
		WrapperPID: wrapperPID,
		ClaudePID:  claudePID,
		SocketPath: socketPath,
		StartedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	var sessMu sync.Mutex
	snapshot := func() *state.Session {
		sessMu.Lock()
		defer sessMu.Unlock()
		c := *sess
		if sess.LastActivity != nil {
			la := *sess.LastActivity
			c.LastActivity = &la
		}
		if sess.Pending != nil {
			p := *sess.Pending
			c.Pending = &p
		}
		return &c
	}

	cs, err := newControlServer(socketPath)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer cs.Close()

	ay := &autoYes{}

	cs.getState = snapshot
	cs.onSetAutoYes = func(on bool) {
		sessMu.Lock()
		sess.AutoYes = on
		sess.UpdatedAt = time.Now()
		sessMu.Unlock()
		ay.Set(on)
		cs.Broadcast(snapshot())
		logger.Printf("auto-yes set to %v", on)
	}
	cs.onApproveNow = func() {
		if err := w.Inject([]byte{'\r'}); err != nil {
			logger.Printf("approve_now inject error: %v", err)
			return
		}
		logger.Printf("approve_now: injected \\r")
	}

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	go cs.serve(bgCtx)
	go writeRegistrationLoop(bgCtx, snapshot, wrapperPID, logger)
	go enrich(bgCtx, sess, &sessMu, cs, w, ay, logger)

	// Foreground: pty I/O. Returns when claude exits.
	runErr := w.Run()

	bgCancel()
	_ = os.Remove(state.SessionFile(wrapperPID))

	if runErr != nil {
		// Pass through the child's exit code where possible.
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return runErr
	}
	return nil
}

func writeRegistrationLoop(ctx context.Context, snapshot func() *state.Session, wrapperPID int, logger *log.Logger) {
	write := func() {
		path := state.SessionFile(wrapperPID)
		tmp := path + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			logger.Printf("registration write: %v", err)
			return
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snapshot()); err != nil {
			f.Close()
			os.Remove(tmp)
			logger.Printf("registration encode: %v", err)
			return
		}
		f.Close()
		if err := os.Rename(tmp, path); err != nil {
			logger.Printf("registration rename: %v", err)
		}
	}
	write()

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

// claudeSessionFile is the subset of fields snitch reads out of
// ~/.claude/sessions/<pid>.json. The file may have additional fields we
// ignore.
type claudeSessionFile struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Status    string `json:"status"`
	Version   string `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
}

// enrich polls claude's session file and keeps the transcript tail aligned
// with the live sessionId. When claude resumes a previous conversation it
// rewrites its session file with a different sessionId; without this loop we
// would tail a never-existing transcript and never see any tool_use events.
//
// On every change of sessionId we cancel the in-flight tail goroutine and
// start a fresh one against the new transcript. The tail itself seeks to end
// so historical events from a resumed session do not replay.
func enrich(
	ctx context.Context,
	sess *state.Session,
	mu *sync.Mutex,
	cs *controlServer,
	pw *PTYWrapper,
	ay *autoYes,
	logger *log.Logger,
) {
	cf := state.ClaudeSessionFile(sess.ClaudePID)

	gate := &tailGate{}
	defer gate.stop()

	var currentSessID string

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		f, ok := readClaudeSessionFile(cf)
		if !ok {
			continue
		}

		mu.Lock()
		statusChanged := sess.Status != f.Status
		idChanged := f.SessionID != "" && f.SessionID != currentSessID
		sess.Status = f.Status
		if idChanged {
			sess.SessionID = f.SessionID
			sess.CWD = f.CWD
		}
		if statusChanged || idChanged {
			sess.UpdatedAt = time.Now()
		}
		mu.Unlock()

		if statusChanged || idChanged {
			cs.Broadcast(snapshotFromMu(sess, mu))
		}

		if !idChanged {
			continue
		}

		currentSessID = f.SessionID
		path := transcript.File(f.CWD, f.SessionID)
		logger.Printf("tailing transcript: %s", path)
		gate.restart(ctx, func(tCtx context.Context) {
			runTail(tCtx, path, sess, mu, cs, pw, ay, logger)
		})
	}
}

// tailGate keeps at most one transcript-tail goroutine running. Stashing the
// CancelFunc in a struct (instead of a local variable inside enrich) keeps
// the vet lostcancel checker happy while still ensuring we cancel any prior
// tail when a new sessionId arrives, and on shutdown.
type tailGate struct {
	cancel context.CancelFunc
}

func (g *tailGate) restart(parent context.Context, run func(context.Context)) {
	if g.cancel != nil {
		g.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	go run(ctx)
}

func (g *tailGate) stop() {
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
}

func runTail(
	ctx context.Context,
	path string,
	sess *state.Session,
	mu *sync.Mutex,
	cs *controlServer,
	pw *PTYWrapper,
	ay *autoYes,
	logger *log.Logger,
) {
	err := transcript.Tail(ctx, path, func(m map[string]any) {
		ev := transcript.Classify(m)
		if ev.Kind == transcript.KindOther {
			return
		}
		mu.Lock()
		now := time.Now()
		sess.LastActivity = &state.Activity{
			Kind:    ev.Kind.String(),
			Summary: ev.Summary(),
			At:      now,
		}
		switch ev.Kind {
		case transcript.KindToolUse:
			sess.Pending = &state.Pending{
				ToolUseID:    ev.ToolUseID,
				Tool:         ev.Tool,
				InputPreview: ev.InputPreview,
				DetectedAt:   now,
			}
		case transcript.KindToolResult:
			if sess.Pending != nil && sess.Pending.ToolUseID == ev.ToolUseID {
				sess.Pending = nil
			}
		}
		sess.UpdatedAt = now
		mu.Unlock()
		cs.Broadcast(snapshotFromMu(sess, mu))

		if ev.Kind == transcript.KindToolUse {
			go maybeAutoFire(ctx, sess, mu, cs, pw, ay, ev.ToolUseID, logger)
		}
	})
	if err != nil && ctx.Err() == nil {
		logger.Printf("tail error: %v", err)
	}
}

func readClaudeSessionFile(path string) (claudeSessionFile, bool) {
	var f claudeSessionFile
	b, err := os.ReadFile(path)
	if err != nil {
		return f, false
	}
	if json.Unmarshal(b, &f) != nil {
		return f, false
	}
	return f, true
}

func snapshotFromMu(sess *state.Session, mu *sync.Mutex) *state.Session {
	mu.Lock()
	defer mu.Unlock()
	c := *sess
	if sess.LastActivity != nil {
		la := *sess.LastActivity
		c.LastActivity = &la
	}
	if sess.Pending != nil {
		p := *sess.Pending
		c.Pending = &p
	}
	return &c
}

// maybeAutoFire is launched per tool_use event. After a grace window, if the
// pending tool_use still has no tool_result and auto-yes is on, inject \r.
func maybeAutoFire(
	ctx context.Context,
	sess *state.Session,
	mu *sync.Mutex,
	cs *controlServer,
	pw *PTYWrapper,
	ay *autoYes,
	tuid string,
	logger *log.Logger,
) {
	const grace = 300 * time.Millisecond
	const debounce = 800 * time.Millisecond

	select {
	case <-ctx.Done():
		return
	case <-time.After(grace):
	}

	mu.Lock()
	stillPending := sess.Pending != nil && sess.Pending.ToolUseID == tuid
	mu.Unlock()

	if !stillPending {
		return
	}
	if !ay.claim(debounce) {
		return
	}
	if err := pw.Inject([]byte{'\r'}); err != nil {
		logger.Printf("auto-yes inject suppressed: %v", err)
		return
	}
	logger.Printf("auto-yes fired for tool_use_id=%s", tuid)
}

func openDebugLog(wrapperPID int) (*log.Logger, func()) {
	path := state.LogFile(wrapperPID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	return log.New(f, fmt.Sprintf("[snitch %d] ", wrapperPID), log.LstdFlags), func() { f.Close() }
}
