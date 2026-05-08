package wrapper

import (
	"sync"
	"sync/atomic"
	"time"
)

// autoYes holds the per-session toggle plus a debounce gate so that bursty
// transcript events don't cause us to fire \r more than once per pending
// permission.
type autoYes struct {
	on atomic.Bool

	mu       sync.Mutex
	lastFire time.Time
}

func (a *autoYes) Set(on bool) { a.on.Store(on) }
func (a *autoYes) Enabled() bool { return a.on.Load() }

// claim returns true if a fire is allowed right now. It resets the debounce
// window unconditionally so callers don't need to track it.
func (a *autoYes) claim(window time.Duration) bool {
	if !a.on.Load() {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.lastFire) < window {
		return false
	}
	a.lastFire = time.Now()
	return true
}
