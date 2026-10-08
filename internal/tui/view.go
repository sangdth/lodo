package tui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/store"
)

// Column widths, without the one-space padding the table adds on each side.
const (
	addressWidth = 15
	portWidth    = 5
	ownWidth     = 3
	checkWidth   = 13
	minNameWidth = 12
	columns      = 5
)

// statusParts are the doctor checks the status bar shows, by ID.
var statusParts = []struct {
	id    int
	label string
}{
	{1, "dnsmasq"},
	{3, "loopback"},
	{4, "resolvers"},
	{8, "caddy"},
}

// View draws the status bar, the table, the status line and the keys.
func (m Model) View() tea.View {
	lines := []string{
		m.statusBar(),
		m.table.View(),
		m.statusLine(),
		m.styles.dim.Render(" space on/off  r apply  q quit"),
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

// layout sizes the table to the terminal: the name column takes what the
// fixed columns leave.
func (m *Model) layout() {
	name := max(m.width-addressWidth-portWidth-ownWidth-checkWidth-2*columns, minNameWidth)
	m.table.SetColumns([]table.Column{
		{Title: "name", Width: name},
		{Title: "address", Width: addressWidth},
		{Title: "port", Width: portWidth},
		{Title: "own", Width: ownWidth},
		{Title: "check", Width: checkWidth},
	})
	m.table.SetWidth(m.width)
	m.table.SetHeight(max(m.height-3, 3)) // the status bar, status line and keys take three lines
	m.table.SetRows(m.rows())
}

// rows renders one table row per domain.
func (m Model) rows() []table.Row {
	rows := make([]table.Row, len(m.domains))
	for i, d := range m.domains {
		mark := "○"
		if d.Enabled {
			mark = "●"
		}
		indent := strings.Repeat("  ", strings.Count(d.Name, ".")-1)
		port := ""
		if d.Port > 0 {
			port = strconv.Itoa(d.Port)
		}
		own := ""
		if store.IsOwn(d.Address) {
			own = "own"
		}
		rows[i] = table.Row{mark + " " + indent + d.Name, d.Address, port, own, m.checkCell(d)}
	}
	return rows
}

// checkCell says how a domain's probes went.
func (m Model) checkCell(d store.Domain) string {
	if !d.Enabled {
		return "–"
	}
	r, ok := m.results[d.Name]
	if !ok {
		return "…"
	}
	cell := "dns " + mark(r.Direct && r.System)
	if d.Port > 0 {
		cell += "  http " + mark(r.HTTP)
	}
	return cell
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// statusBar shows the system parts doctor checks, each with a dot.
func (m Model) statusBar() string {
	parts := []string{m.styles.title.Render("lcd")}
	for _, p := range statusParts {
		parts = append(parts, p.label+" "+m.dot(p.id))
	}
	return " " + strings.Join(parts, "   ")
}

func (m Model) dot(id int) string {
	c, ok := m.check(id)
	switch {
	case !ok:
		return m.styles.dim.Render("…")
	case c.OK:
		return m.styles.ok.Render("●")
	case c.Skipped:
		return m.styles.dim.Render("○")
	}
	return m.styles.bad.Render("●")
}

func (m Model) check(id int) (check.Check, bool) {
	for _, c := range m.checks {
		if c.ID == id {
			return c, true
		}
	}
	return check.Check{}, false
}

// statusLine says what runs, what failed, why the selected name fails, or
// which system part needs lcd doctor, in that order.
func (m Model) statusLine() string {
	line := ""
	switch {
	case m.busy != "":
		line = m.spinner.View() + " " + m.busy + "…"
	case m.err != nil:
		line = m.styles.bad.Render("✗ " + oneLine(m.err.Error()))
	default:
		line = m.problem()
	}
	return ansi.Truncate(" "+line, m.width, "…")
}

func (m Model) problem() string {
	if d, ok := m.selected(); ok && d.Enabled {
		if r, ok := m.results[d.Name]; ok && !r.OK() {
			return m.styles.bad.Render(d.Name + ": " + r.Detail)
		}
	}
	var failed []string
	for _, p := range statusParts {
		if c, ok := m.check(p.id); ok && !c.OK && !c.Skipped {
			failed = append(failed, p.label)
		}
	}
	if len(failed) > 0 {
		return m.styles.bad.Render(strings.Join(failed, ", ") + " need attention: run lcd doctor")
	}
	return ""
}

// oneLine joins the lines of a multi-line error.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}
