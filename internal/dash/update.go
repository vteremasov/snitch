package dash

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"snitch/internal/discover"
)

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		discoverCmd(),
	)
}

func discoverCmd() tea.Cmd {
	return tea.Tick(800*time.Millisecond, func(time.Time) tea.Msg {
		return discoverTickMsg{}
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateViewportHeight()
		m.refreshLogs()
		return m, nil

	case discoverTickMsg:
		m.reconcileWrappers()
		m.updateViewportHeight()
		m.refreshLogs()
		m.updateKeepAwake()
		return m, discoverCmd()

	case stateMsg:
		m.sessions[msg.pid] = msg.session
		m.rebuildOrder()
		if s := m.selected(); s != nil && s.WrapperPID == msg.pid {
			m.refreshLogs()
		}
		m.updateKeepAwake()
		return m, nil

	case removeMsg:
		delete(m.sessions, msg.pid)
		if c, ok := m.clients[msg.pid]; ok {
			c.Close()
			delete(m.clients, msg.pid)
		}
		m.rebuildOrder()
		m.updateViewportHeight()
		m.refreshLogs()
		m.updateKeepAwake()
		return m, nil

	case tea.MouseMsg:
		if msg.Button == tea.MouseButtonWheelUp {
			m.autoScroll = false
			m.logScrollOffset -= 3
			if m.logScrollOffset < 0 {
				m.logScrollOffset = 0
			}
			return m, nil
		} else if msg.Button == tea.MouseButtonWheelDown {
			m.logScrollOffset += 3
			maxOffset := len(m.logLines) - m.logViewportHeight
			if maxOffset < 0 {
				maxOffset = 0
			}
			if m.logScrollOffset >= maxOffset {
				m.logScrollOffset = maxOffset
				m.autoScroll = true
			}
			return m, nil
		}

	case tea.KeyMsg:
		model, cmd := m.handleKey(msg)
		m.updateViewportHeight()
		m.refreshLogs()
		return model, cmd
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.stopCaffeinate()
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.autoScroll = true
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.order)-1 {
			m.cursor++
			m.autoScroll = true
		}
		return m, nil

	case "pgup", "ctrl+u":
		m.autoScroll = false
		m.logScrollOffset -= m.logViewportHeight / 2
		if m.logScrollOffset < 0 {
			m.logScrollOffset = 0
		}
		return m, nil

	case "pgdn", "ctrl+d":
		m.logScrollOffset += m.logViewportHeight / 2
		maxOffset := len(m.logLines) - m.logViewportHeight
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.logScrollOffset >= maxOffset {
			m.logScrollOffset = maxOffset
			m.autoScroll = true
		}
		return m, nil

	case "shift+up":
		m.autoScroll = false
		m.logScrollOffset--
		if m.logScrollOffset < 0 {
			m.logScrollOffset = 0
		}
		return m, nil

	case "shift+down":
		m.logScrollOffset++
		maxOffset := len(m.logLines) - m.logViewportHeight
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.logScrollOffset >= maxOffset {
			m.logScrollOffset = maxOffset
			m.autoScroll = true
		}
		return m, nil

	case " ", "space":
		s, c := m.selected(), m.selectedClient()
		if s == nil || c == nil {
			return m, nil
		}
		next := !s.AutoYes
		_ = c.SetAutoYes(next)
		// Optimistic update only on the selected session. The wrapper will
		// broadcast its real state back; this just removes the visible lag
		// while we wait for the round trip.
		s.AutoYes = next
		if next {
			m.statusLine = "auto-yes ON for pid " + itoa(s.WrapperPID)
		} else {
			m.statusLine = "auto-yes off for pid " + itoa(s.WrapperPID)
		}
		return m, nil

	case "enter":
		s, c := m.selected(), m.selectedClient()
		if s != nil && c != nil {
			_ = c.ApproveNow()
			m.statusLine = "approve sent to pid " + itoa(s.WrapperPID)
		}
		return m, nil

	case "A":
		for pid, c := range m.clients {
			_ = c.SetAutoYes(true)
			if s, ok := m.sessions[pid]; ok {
				s.AutoYes = true // optimistic, per-session
			}
		}
		m.statusLine = "auto-yes ON for all wrappers"
		return m, nil

	case "N":
		for pid, c := range m.clients {
			_ = c.SetAutoYes(false)
			if s, ok := m.sessions[pid]; ok {
				s.AutoYes = false
			}
		}
		m.statusLine = "auto-yes off for all wrappers"
		return m, nil

	case "p":
		m.pendingOnly = !m.pendingOnly
		m.rebuildOrder()
		if m.pendingOnly {
			m.statusLine = "filter: pending only"
		} else {
			m.statusLine = "filter: all sessions"
		}
		return m, nil

	case "w", "c":
		m.keepAwake = !m.keepAwake
		if m.keepAwake {
			m.statusLine = "keep-awake ON (caffeinate when busy)"
		} else {
			m.statusLine = "keep-awake off"
		}
		m.updateKeepAwake()
		return m, nil
	}
	return m, nil
}

// reconcileWrappers compares the on-disk registry with the connected clients,
// dialing new ones and pruning dead ones. Subscription goroutines deliver
// stateMsg/removeMsg back to the program.
func (m *Model) reconcileWrappers() {
	live, _ := discover.List()
	livePIDs := map[int]bool{}
	for _, s := range live {
		livePIDs[s.WrapperPID] = true
		if _, ok := m.clients[s.WrapperPID]; ok {
			continue
		}
		c, err := Dial(s.SocketPath, s.WrapperPID)
		if err != nil {
			continue
		}
		m.clients[s.WrapperPID] = c
		// Seed an initial snapshot from the on-disk state so the table renders
		// immediately even before the first push arrives.
		seed := s
		m.sessions[s.WrapperPID] = &seed
		go m.pumpClient(c)
	}

	// Remove clients whose registration disappeared.
	for pid, c := range m.clients {
		if !livePIDs[pid] {
			c.Close()
			delete(m.clients, pid)
			delete(m.sessions, pid)
		}
	}
	m.rebuildOrder()
}

func (m *Model) pumpClient(c *Client) {
	for s := range c.Updates() {
		m.sendProgram(stateMsg{pid: c.PID(), session: s})
	}
	m.sendProgram(removeMsg{pid: c.PID()})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	idx := len(buf)
	for i > 0 {
		idx--
		buf[idx] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		idx--
		buf[idx] = '-'
	}
	return string(buf[idx:])
}
