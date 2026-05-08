package dash

import (
	"sort"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"snitch/internal/state"
)

type Model struct {
	width, height int

	// pid → most recent snapshot
	sessions map[int]*state.Session
	// stable order: sorted PIDs
	order []int
	// index into order
	cursor int

	// pid → control client (lifecycle managed in Update via discoverTickMsg)
	clients map[int]*Client

	// only show sessions with a Pending != nil
	pendingOnly bool

	// when set, we drew a transient status line (e.g. "approved", "all on")
	statusLine string

	// program is needed so subscription goroutines can Send messages.
	program *tea.Program
	progMu  sync.Mutex
}

func New() *Model {
	return &Model{
		sessions: make(map[int]*state.Session),
		clients:  make(map[int]*Client),
	}
}

func (m *Model) SetProgram(p *tea.Program) {
	m.progMu.Lock()
	m.program = p
	m.progMu.Unlock()
}

func (m *Model) sendProgram(msg tea.Msg) {
	m.progMu.Lock()
	p := m.program
	m.progMu.Unlock()
	if p != nil {
		p.Send(msg)
	}
}

// rebuildOrder regenerates the sorted PID slice. Cursor is clamped to the
// new bounds.
func (m *Model) rebuildOrder() {
	pids := make([]int, 0, len(m.sessions))
	for pid, s := range m.sessions {
		if m.pendingOnly && s.Pending == nil {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	m.order = pids
	if m.cursor >= len(m.order) {
		m.cursor = len(m.order) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *Model) selected() *state.Session {
	if len(m.order) == 0 {
		return nil
	}
	return m.sessions[m.order[m.cursor]]
}

func (m *Model) selectedClient() *Client {
	if len(m.order) == 0 {
		return nil
	}
	return m.clients[m.order[m.cursor]]
}
