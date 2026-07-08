//go:build linux

package dash

import (
	"os/exec"
	"syscall"
)

// startInhibitor starts systemd-inhibit on Linux and sets Pdeathsig so the
// OS kernel terminates it if the parent snitch process crashes or exits.
func startInhibitor() (*exec.Cmd, error) {
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return nil, err
	}
	cmd := exec.Command("systemd-inhibit", "--what=idle:sleep", "--who=snitch", "--why=Active sessions busy", "sleep", "31536000")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
