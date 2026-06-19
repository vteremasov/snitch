// Package notify produces best-effort OS notifications.
//
// macOS: two parallel paths run for maximum coverage —
//
//  1. OSC 9 escape sequence written to /dev/tty. Ghostty (and other
//     iTerm2-protocol terminals) translate this into a native macOS
//     notification through the terminal's own notification permission.
//     This is the reliable visual path on modern macOS where
//     `osascript display notification` is frequently silently dropped.
//
//  2. `osascript -e "display notification … sound name "default""` for the
//     sound. Even when the visual half is suppressed, the sound clause
//     often still plays. Works as a notification fallback in terminals
//     that don't speak OSC 9.
//
// Linux: OSC 9 to /dev/tty plus `notify-send` if installed.
//
// Other platforms: no-op.
package notify

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Disabled reports whether notifications have been turned off via SNITCH_NOTIFY=0.
func Disabled() bool {
	return os.Getenv("SNITCH_NOTIFY") == "0"
}

// Notify shows a desktop notification (and on macOS, plays a short sound).
// Errors are swallowed.
func Notify(title, message string) {
	if Disabled() {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		notifyDarwin(title, message)
	case "linux":
		notifyLinux(title, message)
	}
}

func notifyDarwin(title, message string) {
	// 1. OSC 9 — fires the visible banner reliably in Ghostty / iTerm2.
	writeOSC9(title, message)

	// 2. osascript — covers terminals that don't speak OSC 9 and provides
	// the audible cue via the "default" alert sound. Often silently
	// dropped on Sonoma+ for visuals, but the sound clause still tends to
	// play through.
	script := "display notification " + appleScriptString(message) +
		" with title " + appleScriptString(title) +
		` sound name "default"`
	_ = exec.Command("osascript", "-e", script).Start()
}

// writeOSC9 emits the iTerm2 OSC 9 sequence to the controlling terminal.
// The sequence is a single atomic write (well under PIPE_BUF), so racing
// with claude's own tty writes can't tear it.
func writeOSC9(title, message string) {
	body := title
	if message != "" {
		body = title + " — " + message
	}
	seq := "\x1b]9;" + sanitizeForOSC(body) + "\x07"
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write([]byte(seq))
}

// appleScriptString quotes a Go string for safe inclusion in an
// AppleScript double-quoted literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func notifyLinux(title, message string) {
	writeOSC9(title, message)
	if path, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command(path, title, message).Start()
	}
}

func sanitizeForOSC(s string) string {
	s = strings.ReplaceAll(s, "\x07", " ")
	s = strings.ReplaceAll(s, "\x1b", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// Throttler suppresses repeated notifications for the same logical event.
// Each Key has its own cooldown window. Safe for concurrent use.
type Throttler struct {
	mu       sync.Mutex
	last     map[string]time.Time
	cooldown time.Duration
}

func NewThrottler(cooldown time.Duration) *Throttler {
	return &Throttler{last: make(map[string]time.Time), cooldown: cooldown}
}

// Allow returns true if a notification with the given key may fire now. A
// successful call records the time so subsequent Allow calls within the
// cooldown window return false.
func (t *Throttler) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if prev, ok := t.last[key]; ok && now.Sub(prev) < t.cooldown {
		return false
	}
	t.last[key] = now
	return true
}
