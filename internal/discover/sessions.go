package discover

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"snitch/internal/state"
)

// List scans ~/.snitch/sessions/, drops orphan files for dead pids, and
// returns the live registrations.
func List() ([]state.Session, error) {
	dir := state.SessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []state.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, err := readRegistration(path)
		if err != nil {
			continue
		}
		if !pidAlive(s.WrapperPID) {
			os.Remove(path)
			os.Remove(state.SocketPath(s.WrapperPID))
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func readRegistration(path string) (state.Session, error) {
	var s state.Session
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.WrapperPID == 0 {
		// Try to read the pid from the filename so very-fresh registrations
		// (where the wrapper hasn't flushed yet) still surface.
		base := filepath.Base(path)
		base = strings.TrimSuffix(base, ".json")
		if pid, err := strconv.Atoi(base); err == nil {
			s.WrapperPID = pid
		}
	}
	return s, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
	}
	return true
}

// PrintLs is the implementation of `snitch ls`. It dials each wrapper's
// socket to get a fresh state snapshot rather than relying on the on-disk
// registration alone.
func PrintLs(w io.Writer) error {
	sessions, err := List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(w, "no active snitch wrappers")
		return nil
	}
	fmt.Fprintf(w, "%-7s %-7s %-9s %-5s %-50s %s\n",
		"WRAP", "CLAUDE", "STATUS", "AUTO", "CWD", "ACTIVITY")
	for _, s := range sessions {
		fresh, err := dialAndGet(s.SocketPath)
		if err == nil && fresh != nil {
			s = *fresh
		}
		auto := "off"
		if s.AutoYes {
			auto = "ON"
		}
		activity := ""
		if s.Pending != nil {
			activity = "PENDING " + s.Pending.Tool + " " + s.Pending.InputPreview
		} else if s.LastActivity != nil {
			activity = s.LastActivity.Summary
		}
		cwd := s.CWD
		if len(cwd) > 50 {
			cwd = "…" + cwd[len(cwd)-49:]
		}
		fmt.Fprintf(w, "%-7d %-7d %-9s %-5s %-50s %s\n",
			s.WrapperPID, s.ClaudePID, s.Status, auto, cwd, activity)
	}
	return nil
}

func dialAndGet(socketPath string) (*state.Session, error) {
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	req, _ := json.Marshal(state.Request{Op: state.OpGetState})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(conn)
	var resp state.Response
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Session, nil
}
