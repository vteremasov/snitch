package dash

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func getTTYOfPID(pid int) (string, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "tty=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" || tty == "??" {
		return "", fmt.Errorf("no tty")
	}
	// Prefix with /dev/ if needed
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "/dev/" + tty
	}
	return tty, nil
}

func findAncestorAppBundleIDOf(pid int) (string, bool) {
	curr := pid
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

// FocusSession attempts to bring the terminal window/tab running the wrapper to the foreground.
func FocusSession(pid int) error {
	switch runtime.GOOS {
	case "darwin":
		return focusDarwin(pid)
	case "linux":
		return focusLinux(pid)
	default:
		return fmt.Errorf("focus is not supported on %s", runtime.GOOS)
	}
}

func focusDarwin(pid int) error {
	tty, ttyErr := getTTYOfPID(pid)
	bundleID, hasBundleID := findAncestorAppBundleIDOf(pid)

	if !hasBundleID {
		bundleID = "com.apple.Terminal"
	}

	// Try application-specific AppleScript if TTY is available
	if ttyErr == nil && tty != "" {
		var script string
		switch bundleID {
		case "com.apple.Terminal":
			script = fmt.Sprintf(`
tell application "Terminal"
    repeat with tWindow in windows
        repeat with tTab in tabs of tWindow
            if tty of tTab is "%s" then
                set frontmost of tWindow to true
                set selected tab of tWindow to tTab
                activate
                return
            end if
        end repeat
    end repeat
end tell`, tty)
		case "com.googlecode.iterm2":
			script = fmt.Sprintf(`
tell application "iTerm"
    repeat with aWindow in windows
        repeat with aTab in tabs of aWindow
            repeat with aSession in sessions of aTab
                if (tty of aSession) is "%s" then
                    set frontmost of aWindow to true
                    select aTab
                    select aSession
                    activate
                    return
                end if
            end repeat
        end repeat
    end repeat
end tell`, tty)
		}

		if script != "" {
			cmd := exec.Command("osascript", "-e", script)
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
	}

	// Fallback/other terminal apps: activate using bundle ID
	script := fmt.Sprintf(`tell application id "%s" to activate`, bundleID)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err == nil {
		return nil
	}

	// Last resort: open command
	return exec.Command("open", "-b", bundleID).Run()
}

func focusLinux(pid int) error {
	// Find all ancestor PIDs
	curr := pid
	var ancestors []int
	for depth := 0; depth < 15 && curr > 1; depth++ {
		ancestors = append(ancestors, curr)
		cmd := exec.Command("ps", "-p", strconv.Itoa(curr), "-o", "ppid=")
		output, err := cmd.Output()
		if err != nil {
			break
		}
		ppidStr := strings.TrimSpace(string(output))
		ppid, err := strconv.Atoi(ppidStr)
		if err != nil {
			break
		}
		curr = ppid
	}

	hasXdotool := false
	if _, err := exec.LookPath("xdotool"); err == nil {
		hasXdotool = true
	}
	hasWmctrl := false
	if _, err := exec.LookPath("wmctrl"); err == nil {
		hasWmctrl = true
	}

	if !hasXdotool && !hasWmctrl {
		return fmt.Errorf("neither 'xdotool' nor 'wmctrl' is installed. Please install one of them (e.g. 'sudo apt install xdotool')")
	}

	// Try to activate the window of the first ancestor that has a graphical window
	for _, ancestorPID := range ancestors {
		if hasXdotool {
			cmd := exec.Command("xdotool", "search", "--pid", strconv.Itoa(ancestorPID))
			out, err := cmd.Output()
			if err == nil {
				lines := strings.Split(strings.TrimSpace(string(out)), "\n")
				for _, line := range lines {
					windowID := strings.TrimSpace(line)
					if windowID != "" {
						activateCmd := exec.Command("xdotool", "windowactivate", windowID)
						if err := activateCmd.Run(); err == nil {
							return nil
						}
					}
				}
			}
		}

		if hasWmctrl {
			cmd := exec.Command("wmctrl", "-lp")
			out, err := cmd.Output()
			if err == nil {
				lines := strings.Split(string(out), "\n")
				for _, line := range lines {
					fields := strings.Fields(line)
					if len(fields) >= 3 {
						windowID := fields[0]
						windowPIDStr := fields[2]
						if windowPIDStr == strconv.Itoa(ancestorPID) {
							activateCmd := exec.Command("wmctrl", "-i", "-a", windowID)
							if err := activateCmd.Run(); err == nil {
								return nil
							}
						}
					}
				}
			}
		}
	}

	return fmt.Errorf("could not find a graphical window associated with process PID or any of its ancestors")
}
