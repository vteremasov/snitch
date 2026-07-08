//go:build darwin

package dash

import (
	"os"
	"os/exec"
	"strconv"
)

// startInhibitor starts caffeinate on macOS and binds its lifecycle to the
// current snitch process using the -w flag.
func startInhibitor() (*exec.Cmd, error) {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
