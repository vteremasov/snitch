package dash

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Run is the entry point for `snitch dash`.
func Run(ctx context.Context) error {
	m := New()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.SetProgram(p)
	go func() {
		<-ctx.Done()
		p.Quit()
	}()
	_, err := p.Run()
	return err
}
