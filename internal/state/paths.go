package state

import (
	"fmt"
	"os"
	"path/filepath"
)

func snitchHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".snitch")
}

func SessionsDir() string { return filepath.Join(snitchHome(), "sessions") }
func SocketsDir() string  { return filepath.Join(snitchHome(), "sock") }
func LogDir() string      { return filepath.Join(snitchHome(), "log") }

func SessionFile(wrapperPID int) string {
	return filepath.Join(SessionsDir(), fmt.Sprintf("%d.json", wrapperPID))
}

func SocketPath(wrapperPID int) string {
	return filepath.Join(SocketsDir(), fmt.Sprintf("%d.sock", wrapperPID))
}

func LogFile(wrapperPID int) string {
	return filepath.Join(LogDir(), fmt.Sprintf("%d.log", wrapperPID))
}

func ClaudeProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func ClaudeSessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

func ClaudeSessionFile(claudePID int) string {
	return filepath.Join(ClaudeSessionsDir(), fmt.Sprintf("%d.json", claudePID))
}

func EnsureDirs() error {
	for _, d := range []string{SessionsDir(), SocketsDir(), LogDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
