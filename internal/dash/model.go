package dash

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

	// Log viewer state
	logLines          []string
	logScrollOffset   int
	logViewportHeight int
	autoScroll        bool

	keepAwake     bool
	caffeinateCmd *exec.Cmd
}

func New() *Model {
	return &Model{
		sessions:   make(map[int]*state.Session),
		clients:    make(map[int]*Client),
		autoScroll: true,
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

func (m *Model) updateViewportHeight() {
	overhead := 8 + len(m.order)
	if m.statusLine != "" {
		overhead++
	}
	m.logViewportHeight = m.height - overhead
	if m.logViewportHeight < 3 {
		m.logViewportHeight = 3
	}
}

func (m *Model) refreshLogs() {
	s := m.selected()
	if s == nil {
		m.logLines = nil
		m.logScrollOffset = 0
		return
	}

	pid := s.WrapperPID
	path := filepath.Join(state.LogDir(), fmt.Sprintf("%d.out", pid))
	lines, err := readPTYOutTail(path)
	if err != nil {
		m.logLines = []string{dimStyle.Render(fmt.Sprintf("No active output found at %s", path))}
		m.logScrollOffset = 0
		return
	}

	if len(lines) > 500 {
		lines = lines[len(lines)-500:]
	}

	m.logLines = lines

	maxOffset := len(m.logLines) - m.logViewportHeight
	if maxOffset < 0 {
		maxOffset = 0
	}

	if m.autoScroll {
		m.logScrollOffset = maxOffset
	} else {
		if m.logScrollOffset > maxOffset {
			m.logScrollOffset = maxOffset
		}
		if m.logScrollOffset < 0 {
			m.logScrollOffset = 0
		}
	}
}

type Style struct {
	Bold      bool
	Dim       bool
	Underline bool
	Inverse   bool
	FgColor   string
	BgColor   string
}

func (s Style) Equal(other Style) bool {
	return s.Bold == other.Bold &&
		s.Dim == other.Dim &&
		s.Underline == other.Underline &&
		s.Inverse == other.Inverse &&
		s.FgColor == other.FgColor &&
		s.BgColor == other.BgColor
}

func (s Style) ANSI() string {
	var parts []string
	if s.Bold {
		parts = append(parts, "1")
	}
	if s.Dim {
		parts = append(parts, "2")
	}
	if s.Underline {
		parts = append(parts, "4")
	}
	if s.Inverse {
		parts = append(parts, "7")
	}
	if s.FgColor != "" {
		parts = append(parts, s.FgColor)
	}
	if s.BgColor != "" {
		parts = append(parts, s.BgColor)
	}
	if len(parts) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

func (s *Style) Update(params []int) {
	if len(params) == 0 {
		*s = Style{}
		return
	}
	i := 0
	for i < len(params) {
		p := params[i]
		switch p {
		case 0:
			*s = Style{}
		case 1:
			s.Bold = true
		case 2:
			s.Dim = true
		case 4:
			s.Underline = true
		case 7:
			s.Inverse = true
		case 22:
			s.Bold = false
			s.Dim = false
		case 24:
			s.Underline = false
		case 27:
			s.Inverse = false
		case 39:
			s.FgColor = ""
		case 49:
			s.BgColor = ""
		default:
			if p >= 30 && p <= 37 {
				s.FgColor = fmt.Sprintf("%d", p)
			} else if p >= 40 && p <= 47 {
				s.BgColor = fmt.Sprintf("%d", p)
			} else if p >= 90 && p <= 97 {
				s.FgColor = fmt.Sprintf("%d", p)
			} else if p >= 100 && p <= 107 {
				s.BgColor = fmt.Sprintf("%d", p)
			} else if p == 38 {
				if i+1 < len(params) {
					mode := params[i+1]
					if mode == 5 && i+2 < len(params) {
						s.FgColor = fmt.Sprintf("38;5;%d", params[i+2])
						i += 2
					} else if mode == 2 && i+4 < len(params) {
						s.FgColor = fmt.Sprintf("38;2;%d;%d;%d", params[i+2], params[i+3], params[i+4])
						i += 4
					}
				}
			} else if p == 48 {
				if i+1 < len(params) {
					mode := params[i+1]
					if mode == 5 && i+2 < len(params) {
						s.BgColor = fmt.Sprintf("48;5;%d", params[i+2])
						i += 2
					} else if mode == 2 && i+4 < len(params) {
						s.BgColor = fmt.Sprintf("48;2;%d;%d;%d", params[i+2], params[i+3], params[i+4])
						i += 4
					}
				}
			}
		}
		i++
	}
}

type Cell struct {
	Rune  rune
	Style Style
}

type VirtualTerminal struct {
	Scrollback   [][]Cell
	Lines        [][]Cell
	CursorRow    int
	CursorCol    int
	Height       int
	CurrentStyle Style
	SavedRow     int
	SavedCol     int
	SavedStyle   Style
}

func NewVirtualTerminal(height int) *VirtualTerminal {
	if height <= 0 {
		height = 40
	}
	vt := &VirtualTerminal{
		Height: height,
	}
	vt.Lines = make([][]Cell, height)
	return vt
}

func (vt *VirtualTerminal) clampCursor() {
	if vt.CursorRow < 0 {
		vt.CursorRow = 0
	}
	for vt.CursorRow >= vt.Height {
		vt.Scrollback = append(vt.Scrollback, vt.Lines[0])
		vt.Lines = append(vt.Lines[1:], nil)
		vt.CursorRow--
	}
}

func (vt *VirtualTerminal) Write(data string) {
	runes := []rune(data)
	n := len(runes)
	i := 0

	for i < n {
		r := runes[i]
		if r == '\x1b' {
			if i+1 < n {
				if runes[i+1] == '7' {
					vt.SavedRow = vt.CursorRow
					vt.SavedCol = vt.CursorCol
					vt.SavedStyle = vt.CurrentStyle
					i += 2
					continue
				} else if runes[i+1] == '8' {
					vt.CursorRow = vt.SavedRow
					vt.CursorCol = vt.SavedCol
					vt.CurrentStyle = vt.SavedStyle
					vt.clampCursor()
					i += 2
					continue
				}
			}
			if i+1 < n && (runes[i+1] == '[' || runes[i+1] == ']') {
				j := i + 1
				if runes[i+1] == '[' {
					j++
					for j < n {
						c := runes[j]
						if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
							break
						}
						j++
					}
					if j < n {
						cmd := runes[j]
						paramsStr := string(runes[i+2 : j])

						var params []int
						if paramsStr != "" {
							for _, p := range strings.Split(paramsStr, ";") {
								val, err := strconv.Atoi(p)
								if err == nil {
									params = append(params, val)
								}
							}
						}

						switch cmd {
						case 'm': // SGR Colors
							vt.CurrentStyle.Update(params)
						case 'A': // Cursor Up
							steps := 1
							if len(params) > 0 {
								steps = params[0]
							}
							vt.CursorRow -= steps
							vt.clampCursor()
						case 'B': // Cursor Down
							steps := 1
							if len(params) > 0 {
								steps = params[0]
							}
							vt.CursorRow += steps
							vt.clampCursor()
						case 'C': // Cursor Forward
							steps := 1
							if len(params) > 0 && params[0] > 0 {
								steps = params[0]
							}
							vt.CursorCol += steps
						case 'D': // Cursor Backward
							steps := 1
							if len(params) > 0 && params[0] > 0 {
								steps = params[0]
							}
							vt.CursorCol -= steps
							if vt.CursorCol < 0 {
								vt.CursorCol = 0
							}
						case 'G': // Cursor Horizontal Absolute
							col := 0
							if len(params) > 0 && params[0] > 0 {
								col = params[0] - 1
							}
							vt.CursorCol = col
							if vt.CursorCol < 0 {
								vt.CursorCol = 0
							}
						case 'H', 'f': // Cursor Position
							vt.CursorRow = 0
							vt.CursorCol = 0
							if len(params) >= 1 {
								vt.CursorRow = params[0] - 1
							}
							if len(params) >= 2 {
								vt.CursorCol = params[1] - 1
							}
							vt.clampCursor()
						case 'K': // Clear Line
							mode := 0
							if len(params) > 0 {
								mode = params[0]
							}
							vt.clampCursor()
							if vt.CursorRow < len(vt.Lines) {
								line := vt.Lines[vt.CursorRow]
								if mode == 0 { // Clear from cursor to end
									if vt.CursorCol < len(line) {
										vt.Lines[vt.CursorRow] = line[:vt.CursorCol]
									}
								} else if mode == 1 { // Clear from beginning to cursor
									for col := 0; col <= vt.CursorCol && col < len(line); col++ {
										line[col] = Cell{Rune: ' '}
									}
								} else if mode == 2 { // Clear entire line
									vt.Lines[vt.CursorRow] = nil
								}
							}
						case 'J': // Clear Screen
							mode := 0
							if len(params) > 0 {
								mode = params[0]
							}
							if mode == 2 {
								for idx := range vt.Lines {
									vt.Lines[idx] = nil
								}
								vt.CursorRow = 0
								vt.CursorCol = 0
							}
						}
						i = j + 1
						continue
					}
				} else if runes[i+1] == ']' {
					j++
					for j < n {
						if runes[j] == '\x07' {
							break
						}
						if runes[j] == '\x1b' && j+1 < n && runes[j+1] == '\\' {
							j++
							break
						}
						j++
					}
					if j < n {
						i = j + 1
						continue
					}
				}
			}
		}

		if r == '\n' {
			vt.CursorRow++
			vt.CursorCol = 0
			vt.clampCursor()
		} else if r == '\r' {
			vt.CursorCol = 0
		} else if r == '\t' {
			steps := 8 - (vt.CursorCol % 8)
			for step := 0; step < steps; step++ {
				vt.writeCell(Cell{Rune: ' ', Style: vt.CurrentStyle})
			}
		} else if r >= 32 && r != 127 {
			vt.writeCell(Cell{Rune: r, Style: vt.CurrentStyle})
		}
		i++
	}
}

func (vt *VirtualTerminal) writeCell(cell Cell) {
	vt.clampCursor()
	line := vt.Lines[vt.CursorRow]
	if len(line) <= vt.CursorCol {
		newCells := make([]Cell, vt.CursorCol+1)
		copy(newCells, line)
		for j := len(line); j <= vt.CursorCol; j++ {
			newCells[j] = Cell{Rune: ' '}
		}
		line = newCells
		vt.Lines[vt.CursorRow] = line
	}
	line[vt.CursorCol] = cell
	vt.CursorCol++
}

func (vt *VirtualTerminal) StringifyLines() []string {
	var res []string
	allLines := append(vt.Scrollback, vt.Lines...)
	for _, line := range allLines {
		var sb strings.Builder
		activeStyle := Style{}
		for _, cell := range line {
			if !cell.Style.Equal(activeStyle) {
				if !activeStyle.Equal(Style{}) {
					sb.WriteString("\x1b[0m")
				}
				sb.WriteString(cell.Style.ANSI())
				activeStyle = cell.Style
			}
			sb.WriteRune(cell.Rune)
		}
		if !activeStyle.Equal(Style{}) {
			sb.WriteString("\x1b[0m")
		}
		res = append(res, sb.String())
	}
	return res
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1B(?:[@-Z\\-_]|\[[0-?]*[ -/]*[@-~])`)
	return re.ReplaceAllString(s, "")
}

func readPTYOutTail(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := fi.Size()
	const maxRead = 200 * 1024 // 200 KB
	var offset int64
	if size > maxRead {
		offset = size - maxRead
	}

	buf := make([]byte, size-offset)
	_, err = f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}

	content := string(buf)
	if offset > 0 {
		if idx := strings.IndexByte(content, '\n'); idx != -1 {
			content = content[idx+1:]
		}
	}

	vt := NewVirtualTerminal(40)
	vt.Write(content)

	lines := vt.StringifyLines()

	// Trim trailing blank lines
	for len(lines) > 0 {
		last := lines[len(lines)-1]
		if strings.TrimSpace(stripANSI(last)) == "" {
			lines = lines[:len(lines)-1]
		} else {
			break
		}
	}

	return lines, nil
}

func (m *Model) updateKeepAwake() {
	if !m.keepAwake {
		m.stopCaffeinate()
		return
	}

	hasBusy := false
	for _, s := range m.sessions {
		if s.Status == "busy" {
			hasBusy = true
			break
		}
	}

	if hasBusy {
		m.startCaffeinate()
	} else {
		m.stopCaffeinate()
	}
}

func (m *Model) startCaffeinate() {
	if m.caffeinateCmd != nil {
		return
	}
	if cmd, err := startInhibitor(); err == nil {
		m.caffeinateCmd = cmd
	}
}

func (m *Model) stopCaffeinate() {
	if m.caffeinateCmd == nil {
		return
	}
	if m.caffeinateCmd.Process != nil {
		_ = m.caffeinateCmd.Process.Kill()
		_ = m.caffeinateCmd.Wait()
	}
	m.caffeinateCmd = nil
}

