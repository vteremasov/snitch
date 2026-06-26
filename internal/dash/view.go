package dash

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cursorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	autoOnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	autoOffStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	statusStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}

	var b strings.Builder

	header := fmt.Sprintf("snitch — %d active claude sessions", len(m.order))
	b.WriteString(headerStyle.Render(header))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	if len(m.order) == 0 {
		b.WriteString(dimStyle.Render("no wrappers found. start one with: snitch run"))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("q quit"))
		return b.String()
	}

	colWrap := 7
	colClaude := 7
	colStatus := 7
	colAuto := 5
	colCWD := minInt(40, m.width/3)
	used := colWrap + colClaude + colStatus + colAuto + colCWD + 5 // separators
	colActivity := m.width - used
	if colActivity < 20 {
		colActivity = 20
	}

	headerRow := fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s %s",
		colWrap, "WRAP",
		colClaude, "CLAUDE",
		colStatus, "STATUS",
		colAuto, "AUTO",
		colCWD, "CWD",
		"PENDING / ACTIVITY",
	)
	b.WriteString(dimStyle.Render(headerRow))
	b.WriteString("\n")

	for i, pid := range m.order {
		s := m.sessions[pid]
		marker := "  "
		if i == m.cursor {
			marker = cursorStyle.Render("▶ ")
		}
		auto := autoOffStyle.Render("off")
		if s.AutoYes {
			auto = autoOnStyle.Render("ON ")
		}
		activity := ""
		if s.Pending != nil {
			activity = pendingStyle.Render(fmt.Sprintf("⏸ %s: %s", s.Pending.Tool, s.Pending.InputPreview))
		} else if s.LastActivity != nil {
			activity = dimStyle.Render(fmt.Sprintf("%s: %s",
				s.LastActivity.Kind, truncate(s.LastActivity.Summary, colActivity-12)))
		}

		row := fmt.Sprintf("%-*d %-*d %-*s %-*s %-*s %s",
			colWrap, s.WrapperPID,
			colClaude, s.ClaudePID,
			colStatus, status(s.Status),
			colAuto, auto,
			colCWD, truncate(shortCWD(s.CWD), colCWD),
			activity,
		)
		b.WriteString(marker)
		b.WriteString(row)
		b.WriteString("\n")
	}

	b.WriteString(dimStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Render Selected Session's Wrapper Log
	s := m.selected()
	if s == nil {
		b.WriteString(dimStyle.Render("no active session selected"))
		b.WriteString("\n")
		// Pad remaining space
		for i := 0; i < m.logViewportHeight; i++ {
			b.WriteString("\n")
		}
	} else {
		logTitle := fmt.Sprintf(" WRAPPER LOG (PID %d) · CWD: %s ", s.WrapperPID, shortCWD(s.CWD))
		titleBar := drawTitleBar(logTitle, m.width)
		b.WriteString(dimStyle.Render(titleBar))
		b.WriteString("\n")

		start := m.logScrollOffset
		end := start + m.logViewportHeight
		if end > len(m.logLines) {
			end = len(m.logLines)
		}

		printed := 0
		for i := start; i < end; i++ {
			b.WriteString(m.logLines[i])
			b.WriteString("\n")
			printed++
		}
		for printed < m.logViewportHeight {
			b.WriteString("\n")
			printed++
		}
	}

	b.WriteString(dimStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	if m.statusLine != "" {
		b.WriteString(statusStyle.Render(m.statusLine))
		b.WriteString("\n")
	}

	help := "↑/↓ navigate · space toggle auto-yes · enter approve pending · A/N auto-yes all/none · p pending-only · pgup/pgdn scroll logs · q quit"
	b.WriteString(helpStyle.Render(help))

	return b.String()
}

func drawTitleBar(title string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(title) >= width {
		return title[:width]
	}
	leftLen := 2
	rightLen := width - len(title) - leftLen
	if rightLen < 0 {
		rightLen = 0
	}
	return strings.Repeat("─", leftLen) + title + strings.Repeat("─", rightLen)
}

func status(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func shortCWD(cwd string) string {
	// Drop the leading /Users/<user>/ prefix to save horizontal space.
	parts := strings.Split(strings.TrimPrefix(cwd, "/"), "/")
	if len(parts) >= 3 && parts[0] == "Users" {
		return strings.Join(parts[2:], "/")
	}
	return cwd
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
