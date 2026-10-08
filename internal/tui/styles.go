package tui

import (
	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
)

// styles holds every style the TUI draws with.
type styles struct {
	title lipgloss.Style
	ok    lipgloss.Style
	bad   lipgloss.Style
	dim   lipgloss.Style
	box   lipgloss.Style
	table table.Styles
}

func newStyles() styles {
	t := table.DefaultStyles()
	t.Header = t.Header.Foreground(lipgloss.Color("8"))
	return styles{
		title: lipgloss.NewStyle().Bold(true),
		ok:    lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		bad:   lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		dim:   lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		box:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")),
		table: t,
	}
}
