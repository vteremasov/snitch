package dash

import "snitch/internal/state"

// stateMsg is delivered when a wrapper pushes a new snapshot.
type stateMsg struct {
	pid     int
	session *state.Session
}

// removeMsg is delivered when a wrapper's subscription closes.
type removeMsg struct {
	pid int
}

// discoverTickMsg fires periodically to scan ~/.snitch/sessions/ for new
// wrappers (or to prune stale on-disk entries).
type discoverTickMsg struct{}
