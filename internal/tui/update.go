package tui

import (
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lcd/internal/store"
)

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case reportMsg:
		m.busy = ""
		m.setReport(msg.checks, msg.results)
		return m, nil
	case changedMsg:
		m.busy, m.err = "", msg.err
		if msg.stored {
			m.domains = store.Sort(msg.domains)
			m.setReport(msg.checks, msg.results)
		}
		return m, nil
	case spinner.TickMsg:
		if m.busy == "" {
			return m, nil // stop animating once idle
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

// key handles a key press. While a change runs, only ctrl+c and moving the
// cursor work: a second change must not start before the first one ends.
func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.busy == "" {
		switch k {
		case "q":
			return m, tea.Quit
		case "r":
			return m.start("applying", m.reload())
		case "space":
			d, ok := m.selected()
			if !ok {
				return m, nil
			}
			next, err := store.Toggle(m.domains, d.Name)
			if err != nil {
				m.err = err
				return m, nil
			}
			return m.start("applying", m.change(next))
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}
