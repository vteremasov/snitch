package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"snitch/internal/notify"
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

	pset := newPendingSet()
	app := newApprover(pset, w, ay, sess, &sessMu, logger)

	// Permission-prompt detection via pty scanning. The transcript path
	// only sees `tool_use` *after* claude has resolved the permission, so
	// it can't be the trigger for auto-yes. The pty byte stream is the
	// only signal that arrives while the prompt is still up. Debounced so
	// one rendered prompt fires at most once.
	var (
		promptMu   sync.Mutex
		promptLast time.Time
	)
	w.SetOnPrompt(func() {
		promptMu.Lock()
		if time.Since(promptLast) < 500*time.Millisecond {
			promptMu.Unlock()
			return
		}
		promptLast = time.Now()
		promptMu.Unlock()

		ay.SetInPermissionPrompt(true)

		if ay.Enabled() {
			if err := w.Inject([]byte{'\r'}); err != nil {
				logger.Printf("pty auto-yes inject failed: %v", err)
				return
			}
			logger.Printf("auto-yes fired (pty pattern)")
			return
		}
		// Auto-yes off — surface the prompt to the user via notification.
		sessMu.Lock()
		cwd := sess.CWD
		sessMu.Unlock()
		notify.Notify(
			fmt.Sprintf("Claude needs permission · %s", sessionLabel(cwd, wrapperPID)),
			shortCWD(cwd),
		)
		logger.Printf("notify: permission (pty pattern)")
	})

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	go cs.serve(bgCtx)
	go writeRegistrationLoop(bgCtx, snapshot, wrapperPID, logger)
	go app.run(bgCtx)
	go enrich(bgCtx, sess, &sessMu, cs, w, ay, pset, app, logger)

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
//
// enrich also fires the "claude is waiting" OS notification on every
// busy → waiting transition where no permission is pending, throttled per
// session so consecutive idle blips don't spam.
func enrich(
	ctx context.Context,
	sess *state.Session,
	mu *sync.Mutex,
	cs *controlServer,
	pw *PTYWrapper,
	ay *autoYes,
	pset *pendingSet,
	app *approver,
	logger *log.Logger,
) {
	cf := state.ClaudeSessionFile(sess.ClaudePID)

	gate := &tailGate{}
	defer gate.stop()

	var (
		currentSessID  string
		prevStatus     string
		lastIdleNotify time.Time
	)

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

		if f.Status == "busy" {
			ay.SetInPermissionPrompt(false)
		}

		mu.Lock()
		statusChanged := sess.Status != f.Status
		idChanged := f.SessionID != "" && f.SessionID != currentSessID
		hasPending := sess.Pending != nil
		sess.Status = f.Status
		if idChanged {
			sess.SessionID = f.SessionID
			sess.CWD = f.CWD
		}
		if statusChanged || idChanged {
			sess.UpdatedAt = time.Now()
		}
		cwd := sess.CWD
		wrapperPID := sess.WrapperPID
		mu.Unlock()

		if statusChanged || idChanged {
			cs.Broadcast(snapshotFromMu(sess, mu))
		}

		// Idle notification: claude just stopped working and isn't blocked on
		// a permission prompt. Fire at most once per 30s per wrapper.
		if statusChanged && f.Status == "waiting" && prevStatus != "waiting" && !hasPending && !ay.InPermissionPrompt() {
			if time.Since(lastIdleNotify) > 30*time.Second {
				notify.Notify(
					fmt.Sprintf("Claude waiting for input · %s", sessionLabel(cwd, wrapperPID)),
					shortCWD(cwd),
				)
				lastIdleNotify = time.Now()
				logger.Printf("notify: idle (status=waiting, no pending)")
			}
		}
		prevStatus = f.Status

		if !idChanged {
			continue
		}

		currentSessID = f.SessionID
		path := transcript.File(f.CWD, f.SessionID)
		logger.Printf("tailing transcript: %s", path)
		gate.restart(ctx, func(tCtx context.Context) {
			runTail(tCtx, path, sess, mu, cs, pw, ay, pset, app, logger)
		})
	}
}

// shortCWD collapses $HOME to ~ for a tighter notification body. Works on
// any account (not just /Users/<user>) and renders the home root as "~".
func shortCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if cwd == home {
			return "~"
		}
		if strings.HasPrefix(cwd, home+string(os.PathSeparator)) {
			return "~" + cwd[len(home):]
		}
	}
	return cwd
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	c, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(c)) + s[size:]
}

// sessionLabel identifies a session in notifications as "<folder>:<wrapperPID>"
// so the user can tell which wrapper (matching the dash) fired the banner.
func sessionLabel(cwd string, wrapperPID int) string {
	return fmt.Sprintf("%s:%d", projectName(cwd), wrapperPID)
}

// projectName returns a short human-friendly label for a cwd. Returns
// "Home" for $HOME and "Root" for "/" so notifications never expose the
// username and never panic on edge paths.
func projectName(cwd string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && cwd == home {
		return "Home"
	}
	parts := splitPath(cwd)
	if len(parts) == 0 {
		return "Root"
	}
	return capFirst(parts[len(parts)-1])
}

func splitPath(p string) []string {
	clean := filepath.Clean(p)
	var parts []string
	for clean != "/" && clean != "." {
		parts = append([]string{filepath.Base(clean)}, parts...)
		clean = filepath.Dir(clean)
	}
	return parts
}

// pendingSet tracks every tool_use_id that has appeared without a matching
// tool_result. A single sess.Pending pointer can't be used for auto-fire
// decisions because claude can emit multiple parallel tool_use blocks in
// one assistant turn — the second tool_use would overwrite the first's
// Pending and the goroutine for the first would falsely conclude its tool
// had already resolved.
type pendingSet struct {
	mu  sync.Mutex
	set map[string]struct{}
}

func newPendingSet() *pendingSet {
	return &pendingSet{set: make(map[string]struct{})}
}

func (p *pendingSet) add(tuid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.set[tuid] = struct{}{}
}

func (p *pendingSet) remove(tuid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.set[tuid]; !ok {
		return false
	}
	delete(p.set, tuid)
	return true
}

func (p *pendingSet) has(tuid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.set[tuid]
	return ok
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
	pset *pendingSet,
	app *approver,
	logger *log.Logger,
) {
	err := transcript.Tail(ctx, path, func(m map[string]any) {
		ev := transcript.Classify(m)
		if ev.Kind == transcript.KindOther {
			if t, ok := m["type"].(string); ok {
				switch t {
				case "summary", "ai-title", "last-prompt", "permission-mode",
					"attachment", "assistant", "system":
					// Known shapes we don't classify but expect to see — skip log.
				default:
					logger.Printf("unclassified event type=%q", t)
				}
			}
			return
		}
		mu.Lock()
		now := time.Now()
		sess.LastActivity = &state.Activity{
			Kind:    ev.Kind.String(),
			Summary: ev.Summary(),
			At:      now,
		}
		var resultCleared bool
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
			resultCleared = pset.remove(ev.ToolUseID)
		}
		sess.UpdatedAt = now
		mu.Unlock()
		cs.Broadcast(snapshotFromMu(sess, mu))

		switch ev.Kind {
		case transcript.KindToolUse:
			pset.add(ev.ToolUseID)
			logger.Printf("tool_use seen id=%s tool=%s", ev.ToolUseID, ev.Tool)
			app.enqueue(ev.ToolUseID)
		case transcript.KindToolResult:
			if resultCleared {
				logger.Printf("tool_result cleared pending id=%s", ev.ToolUseID)
			}
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

// approver serializes auto-yes injections. Claude shows permission prompts
// one at a time — only after the previous tool's prompt is accepted does
// the next render. So we must fire one \r, wait for that tool_use's
// tool_result (= prompt accepted, tool ran), and only then fire the next.
// Sending all \r's in parallel only ever approves the first prompt; the
// rest land in claude's stdin before the next prompt is up and get
// consumed as no-op chat input.
type approver struct {
	queue  chan string
	pset   *pendingSet
	pw     *PTYWrapper
	ay     *autoYes
	sess   *state.Session
	sessMu *sync.Mutex
	logger *log.Logger
}

func newApprover(pset *pendingSet, pw *PTYWrapper, ay *autoYes, sess *state.Session, sessMu *sync.Mutex, logger *log.Logger) *approver {
	return &approver{
		queue:  make(chan string, 64),
		pset:   pset,
		pw:     pw,
		ay:     ay,
		sess:   sess,
		sessMu: sessMu,
		logger: logger,
	}
}

func (a *approver) enqueue(tuid string) {
	select {
	case a.queue <- tuid:
	default:
		a.logger.Printf("auto-yes queue full, dropping id=%s", tuid)
	}
}

func (a *approver) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tuid := <-a.queue:
			a.process(ctx, tuid)
		}
	}
}

func (a *approver) process(ctx context.Context, tuid string) {
	const grace = 300 * time.Millisecond
	const resultWait = 10 * time.Second

	select {
	case <-ctx.Done():
		return
	case <-time.After(grace):
	}

	if !a.pset.has(tuid) {
		a.logger.Printf("auto-yes skip id=%s reason=cleared-before-grace on=%v", tuid, a.ay.Enabled())
		return
	}

	on := a.ay.Enabled()
	if !on {
		a.logger.Printf("auto-yes skip id=%s reason=toggle-off", tuid)
		a.sessMu.Lock()
		cwd := a.sess.CWD
		wrapperPID := a.sess.WrapperPID
		a.sessMu.Unlock()
		notify.Notify(
			fmt.Sprintf("Claude needs permission · %s", sessionLabel(cwd, wrapperPID)),
			shortCWD(cwd),
		)
		return
	}

	if err := a.pw.Inject([]byte{'\r'}); err != nil {
		a.logger.Printf("auto-yes inject suppressed id=%s err=%v", tuid, err)
		return
	}
	a.logger.Printf("auto-yes fired for tool_use_id=%s", tuid)

	// Block here until the tool_result for this tuid removes it from the
	// set, OR the timeout fires. Holding here serializes the queue so the
	// next \r is only sent when claude is ready for the next prompt.
	deadline := time.Now().Add(resultWait)
	for {
		if !a.pset.has(tuid) {
			return
		}
		if time.Now().After(deadline) {
			a.logger.Printf("auto-yes wait timeout id=%s — moving on", tuid)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func openDebugLog(wrapperPID int) (*log.Logger, func()) {
	path := state.LogFile(wrapperPID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	return log.New(f, fmt.Sprintf("[snitch %d] ", wrapperPID), log.LstdFlags), func() { f.Close() }
}
