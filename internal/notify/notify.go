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
	"path/filepath"
	"runtime"
	"strconv"
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
	// If terminal-notifier is installed, use it as it provides click-to-focus for all terminals.
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		bundleID := getTerminalBundleID()
		_ = exec.Command(path, "-title", title, "-message", message, "-sender", bundleID, "-activate", bundleID).Start()
		return
	}

	// Otherwise, fallback depending on terminal capabilities:
	termProg := os.Getenv("TERM_PROGRAM")
	if termProg == "iTerm.app" || termProg == "Ghostty" || termProg == "vscode" || termProg == "WezTerm" {
		// 1. OSC 9 — Ghostty / iTerm2 natively handle this escape sequence and focus the terminal tab on click.
		writeOSC9(title, message)
		// Play sound natively without generating a duplicate visual notification.
		_ = exec.Command("osascript", "-e", "beep").Start()
	} else {
		// 2. osascript — for terminals that don't speak OSC 9 (like Apple Terminal).
		script := "display notification " + appleScriptString(message) +
			" with title " + appleScriptString(title) +
			` sound name "default"`
		_ = exec.Command("osascript", "-e", script).Start()
	}
}

func getTerminalBundleID() string {
	termProg := os.Getenv("TERM_PROGRAM")
	switch termProg {
	case "Apple_Terminal":
		return "com.apple.Terminal"
	case "iTerm.app":
		return "com.googlecode.iterm2"
	case "Ghostty":
		return "com.mitchellh.ghostty"
	case "vscode":
		return "com.microsoft.VSCode"
	}

	// Fallback to searching ancestor process hierarchy for an app bundle (like JetBrains IDEs)
	if bundleID, ok := findAncestorAppBundleID(); ok {
		return bundleID
	}

	return "com.apple.Terminal"
}

func findAncestorAppBundleID() (string, bool) {
	curr := os.Getpid()
	// Limit search depth to prevent infinite loops or excessive scanning
	for depth := 0; depth < 15 && curr > 1; depth++ {
		cmd := exec.Command("ps", "-p", strconv.Itoa(curr), "-o", "ppid=", "-o", "comm=")
		output, err := cmd.Output()
		if err != nil {
			break
		}

		parts := strings.Fields(strings.TrimSpace(string(output)))
		if len(parts) < 2 {
			break
		}

		ppid, err := strconv.Atoi(parts[0])
		if err != nil {
			break
		}

		comm := strings.Join(parts[1:], " ")
		if idx := strings.Index(comm, ".app/"); idx != -1 {
			appPath := comm[:idx+4]
			plistPath := filepath.Join(appPath, "Contents", "Info.plist")
			if _, err := os.Stat(plistPath); err == nil {
				defaultsCmd := exec.Command("defaults", "read", plistPath, "CFBundleIdentifier")
				if bundleIDBytes, err := defaultsCmd.Output(); err == nil {
					bundleID := strings.TrimSpace(string(bundleIDBytes))
					if bundleID != "" {
						return bundleID, true
					}
				}
			}
		}
		curr = ppid
	}
	return "", false
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
