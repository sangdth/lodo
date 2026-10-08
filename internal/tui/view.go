package tui

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/oo/internal/check"
	"github.com/sangdth/oo/internal/store"
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

// The modal takes 70% of the terminal's width, but never less than
// minBoxWidth columns, so the table and the keys fit, and never more than the
// terminal. It grows with the names up to 80% of the height; the log takes
// all of that.
const (
	boxWidthPercent  = 70
	boxHeightPercent = 80
	minBoxWidth      = 84
)

// formHeight is the lines the form needs: a title between two blank lines,
// and each field with a line for its error.
const formHeight = 3 + 2*fieldCount

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

// The keys each mode takes, shown on the last line.
var help = map[mode]string{
	modeList:    " a add  e edit  d delete  space on/off  c copy env  l log  r apply  q quit",
	modeForm:    " enter save  tab next field  esc cancel",
	modeConfirm: " y delete  any other key keeps it",
	modeLog:     " l or esc back to the list  up/down scroll  q quit",
}

// View draws the status bar, a rule, the table or the form, the status line
// and the keys in a bordered box at the middle of the terminal.
func (m Model) View() tea.View {
	body := m.table.View()
	switch m.mode {
	case modeForm:
		body = m.formView()
	case modeLog:
		body = m.logView()
	}
	lines := []string{
		m.statusBar(),
		m.styles.dim.Render(strings.Repeat("─", m.innerWidth())),
		body,
		m.statusLine(),
		m.styles.dim.Render(help[m.mode]),
	}
	box := m.styles.box.Width(m.boxWidth()).Render(strings.Join(lines, "\n"))
	v := tea.NewView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box))
	v.AltScreen = true
	return v
}

// layout sizes the table to the box: the name column takes what the fixed
// columns leave.
func (m *Model) layout() {
	name := max(m.innerWidth()-addressWidth-portWidth-ownWidth-checkWidth-2*columns, minNameWidth)
	m.table.SetColumns([]table.Column{
		{Title: "name", Width: name},
		{Title: "address", Width: addressWidth},
		{Title: "port", Width: portWidth},
		{Title: "own", Width: ownWidth},
		{Title: "check", Width: checkWidth},
	})
	m.table.SetWidth(m.innerWidth())
	m.table.SetHeight(m.bodyHeight())
	m.table.SetRows(m.rows())
	m.log.SetWidth(m.innerWidth())
	m.log.SetHeight(m.logHeight() - 1) // the log's title takes a line
}

// boxWidth is the modal's width with its border.
func (m Model) boxWidth() int {
	return min(max(m.width*boxWidthPercent/100, minBoxWidth), m.width)
}

// innerWidth is the columns inside the modal's border.
func (m Model) innerWidth() int {
	return max(m.boxWidth()-2, 1)
}

// bodyHeight is the lines the table gets: its header and one per name, at
// least one row, at most the log's lines.
func (m Model) bodyHeight() int {
	return min(max(len(m.domains), 1)+1, m.logHeight())
}

// logHeight is the most lines the body gets: the border takes two, and the
// status bar, the rule under it, the status line and the keys take four.
func (m Model) logHeight() int {
	return max(m.height*boxHeightPercent/100-6, 3)
}

// rows renders one table row per domain.
func (m Model) rows() []table.Row {
	rows := make([]table.Row, len(m.domains))
	for i, d := range m.domains {
		mark := "○"
		switch {
		case m.spins(d):
			mark = m.spinner.View()
		case d.Enabled:
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

// spins says whether d's row shows the spinner: the name a change is for,
// whether it is on or off, or every enabled name while the first check or a
// reload runs.
func (m Model) spins(d store.Domain) bool {
	if !m.busy {
		return false
	}
	if m.pending == "" {
		return d.Enabled
	}
	return d.Name == m.pending
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
	parts := []string{m.styles.title.Render("oo")}
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

// statusLine says what a delete waits for, what failed, what was just done,
// why the selected name fails, or which system part needs oo doctor, in that
// order.
func (m Model) statusLine() string {
	line := ""
	switch {
	case m.mode == modeConfirm:
		line = m.styles.title.Render("delete " + m.target + "? y/n")
	case m.err != nil:
		line = m.styles.bad.Render("✗ " + oneLine(m.err.Error()))
	case m.note != "":
		line = m.styles.ok.Render(m.note)
	default:
		line = m.problem()
	}
	return ansi.Truncate(" "+line, m.innerWidth(), "…")
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
		return m.styles.bad.Render(strings.Join(failed, ", ") + " need attention: run oo doctor")
	}
	return ""
}

// formView draws the add or edit form at a fixed height, so the box stays put
// while errors come and go.
func (m Model) formView() string {
	f := m.form
	title := "Add a name"
	if f.editing != "" {
		title = "Edit " + f.editing
	}
	lines := []string{"", " " + m.styles.title.Render(title), ""}
	labels := [fieldCount]string{"name", "address", "port"}
	hints := [fieldCount]string{"", f.hint, m.portHint()}
	for i := range fieldCount {
		input := f.inputs[i].View()
		if i == fieldName {
			input = m.nameInput()
		}
		line := fmt.Sprintf(" %-8s %s", labels[i], input)
		if hints[i] != "" {
			line += "  " + m.styles.dim.Render(hints[i])
		}
		lines = append(lines, ansi.Truncate(line, m.innerWidth(), "…"))
		if f.errs[i] != "" {
			lines = append(lines, ansi.Truncate("          "+m.styles.bad.Render("✗ "+f.errs[i]), m.innerWidth(), "…"))
		}
	}
	for len(lines) < formHeight {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// logView draws dnsmasq's query log under a title line.
func (m Model) logView() string {
	title := " " + m.styles.title.Render("dnsmasq query log")
	if len(m.logLines) == 0 {
		lines := []string{title, " " + m.styles.dim.Render("no log yet: dnsmasq writes a line for every query it answers")}
		for len(lines) < m.logHeight() {
			lines = append(lines, "")
		}
		return strings.Join(lines, "\n")
	}
	return title + "\n" + m.log.View()
}

// nameInput draws the name field as wide as its text, or its placeholder, and
// the dimmed suffix after it, which can't be edited.
func (m Model) nameInput() string {
	in := m.form.inputs[fieldName]
	width := ansi.StringWidth(in.Value())
	if width == 0 {
		width = ansi.StringWidth(in.Placeholder)
	}
	in.SetWidth(width)
	return in.View() + m.styles.dim.Render(nameSuffix)
}

// portHint says what a port does for the name being typed.
func (m Model) portHint() string {
	name := cmp.Or(m.form.name(), "<name>")
	port := strings.TrimSpace(m.form.inputs[fieldPort].Value())
	if port == "" {
		return "optional: Caddy then forwards http://" + name + " to this port"
	}
	address := strings.TrimSpace(m.form.inputs[fieldAddress].Value())
	return "http://" + name + " then reaches " + address + ":" + port
}

// oneLine joins the lines of a multi-line error.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}
