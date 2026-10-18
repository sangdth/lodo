package tui

import (
	"cmp"
	"errors"
	"slices"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/sangdth/lodo/internal/store"
)

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case reportMsg:
		m.busy, m.err = false, msg.err
		here := m.cursorName()
		m.setReport(msg.checks, msg.results)
		m.layout()
		m.placeCursor(here)
		return m, nil
	case changedMsg:
		m.busy, m.err = false, msg.err
		here := m.cursorName()    // before the rows change under the cursor
		m.adding = store.Domain{} // saved, or gone when the save failed
		if msg.note != "" {
			m.note = msg.note
		}
		if msg.stored {
			m.domains = store.Sort(msg.domains)
		}
		if msg.checks != nil {
			m.setReport(msg.checks, msg.results)
		}
		m.layout()
		m.placeCursor(here)
		return m, nil
	case setupScanMsg:
		if m.mode != modeSetup {
			return m, nil // the form closed while it scanned
		}
		s, cmd := m.setup.update(msg)
		return m.afterSetup(s, cmd)
	case linkScanMsg:
		d, ok := store.Find(m.domains, msg.name)
		switch {
		case !ok:
			return m, nil // deleted while the scan ran
		case m.busy:
			m.err = errors.New("a change is still running: press l again in a moment")
			return m, nil
		}
		return m.linkProject(d, msg.files)
	case previewMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.showPreview(msg)
		return m, nil
	case logMsg:
		if msg.session != m.logSession || m.mode != modeLog {
			return m, nil // the log closed or reopened since this read started
		}
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.appendLog(msg.text, msg.offset)
		}
		return m, m.logTick()
	case logTickMsg:
		if msg.session != m.logSession || m.mode != modeLog {
			return m, nil
		}
		return m, m.tailLog()
	case spinner.TickMsg:
		if !m.busy {
			return m, nil // stop animating once idle
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.table.SetRows(m.rows())
		return m, cmd
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

// key handles a key press. ctrl+c always quits. In the list, while a change
// runs, only moving the cursor works: a second change must not start before
// the first one ends.
func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	m.note = ""
	switch m.mode {
	case modeForm:
		return m.formKey(msg)
	case modeConfirm:
		return m.confirmKey(k)
	case modeLog:
		return m.logKey(msg)
	case modeSetup:
		s, cmd := m.setup.update(msg)
		return m.afterSetup(s, cmd)
	case modePreview:
		return m.previewKey(msg)
	}
	if k == "g" {
		return m.openLog() // reading the log is safe while a change runs
	}
	if k == "tab" {
		m.onServices = !m.onServices
		styles := m.styles.table
		if m.onServices {
			m.table.Blur()
			styles.Selected = lipgloss.NewStyle() // one highlight at a time: the service's
		} else {
			m.table.Focus()
		}
		m.table.SetStyles(styles)
		m.table.SetRows(m.rows()) // the row under the cursor draws its color only while it isn't highlighted
		return m, nil
	}
	if m.onServices {
		return m.serviceKey(k)
	}
	if k == "p" {
		return m.openPreview() // reading the compose file is safe while a change runs
	}
	if k == "i" && m.origin.Dir != "" {
		m.mode, m.err = modeSetup, nil
		m.setup = NewSetup(m.ctx, m.backend, m.domains, m.origin)
		m.setup.embedded = true
		return m, m.setup.Init() // so is opening the setup form, which scans the project
	}
	if !m.busy {
		if next, cmd, ok := m.listKey(k); ok {
			return next, cmd
		}
	}
	var cmd tea.Cmd
	cursor := m.table.Cursor()
	m.table, cmd = m.table.Update(msg)
	if m.table.Cursor() != cursor {
		m.table.SetRows(m.rows()) // the row under the cursor draws its mark and the add row without color
	}
	return m, cmd
}

// afterSetup stores the setup form after it handled a message. Once the form
// is done, the list shows again, and a form that saved hands over its change,
// which runs with the spinner on the project's name: a done form returns no
// command unless it saved. A second change must not start before the first
// one ends, so the form stays open while one runs.
func (m Model) afterSetup(s Setup, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	switch {
	case !s.done:
		m.setup = s
		return m, cmd
	case cmd != nil && m.busy:
		s.done, s.err = false, errors.New("a change is still running: press enter again in a moment")
		m.setup = s
		return m, nil
	}
	m.setup, m.mode = s, modeList
	if cmd == nil {
		return m, nil
	}
	m.selectName = s.projectName()
	return m.start(m.selectName, cmd)
}

// openAdd opens the add form, for a subdomain of parent when it is set.
func (m Model) openAdd(parent string) Model {
	m.mode, m.err, m.form = modeForm, nil, newAddForm(m.domains, parent, m.origin)
	return m
}

// openEdit opens the edit form for d.
func (m Model) openEdit(d store.Domain) Model {
	m.mode, m.err, m.form = modeForm, nil, newEditForm(m.domains, d, m.origin)
	return m
}

// listKey handles the list's action keys, and reports whether k was one.
func (m Model) listKey(k string) (Model, tea.Cmd, bool) {
	switch k {
	case "q":
		return m, tea.Quit, true
	case "r":
		next, cmd := m.start("", m.reload())
		return next, cmd, true
	case "A", "shift+a": // terminals that report keys without their text send shift+a
		return m.openAdd(""), nil, true
	}
	d, ok := m.selected()
	if !ok {
		if k == "a" || k == "space" || k == "enter" { // on the add row
			return m.openAdd(""), nil, true
		}
		return m, nil, false
	}
	switch k {
	case "space":
		next, err := store.Toggle(m.domains, d.Name)
		if err != nil {
			m.err = err
			return m, nil, true
		}
		m.selectName = d.Name
		started, cmd := m.start(d.Name, m.change(next, true))
		return started, cmd, true
	case "s":
		next, cmd := m.toggleHTTPS(d)
		return next, cmd, true
	case "a":
		return m.openAdd(d.Name), nil, true
	case "e":
		return m.openEdit(d), nil, true
	case "d":
		m.mode, m.err, m.target = modeConfirm, nil, d.Name
		return m, nil, true
	case "l":
		next, cmd := m.link(d)
		return next, cmd, true
	}
	return m, nil, false
}

// toggleHTTPS turns HTTPS on or off for d and applies the list. store
// refuses it for a name without a port.
func (m Model) toggleHTTPS(d store.Domain) (Model, tea.Cmd) {
	changed := d
	changed.HTTPS = !d.HTTPS
	next, err := store.Update(m.domains, d.Name, changed)
	if err != nil {
		m.err = err
		return m, nil
	}
	m.selectName = d.Name
	return m.start(d.Name, m.change(next, true))
}

// serviceKey handles the keys while they act on the status bar's services:
// left and right, or h and l, pick one; space turns it on or off.
func (m Model) serviceKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q":
		return m, tea.Quit
	case "left", "h":
		m.service = max(m.service-1, 0)
	case "right", "l":
		m.service = min(m.service+1, len(services)-1)
	case "space":
		if m.busy {
			return m, nil
		}
		service := services[m.service]
		c, ok := m.check(serviceCheck(service))
		if !ok {
			return m, nil // no report yet, so on or off is unknown
		}
		return m.start(service, m.setService(service, c.Off))
	}
	return m, nil
}

// formKey types into the form, saves it on enter and drops it on esc.
func (m Model) formKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		return m, nil
	case "enter":
		return m.submit()
	}
	m.form = m.form.update(msg, m.domains)
	return m, nil
}

// submit checks the form against every rule and, when it passes, saves and
// applies the new list.
func (m Model) submit() (tea.Model, tea.Cmd) {
	if m.busy {
		m.form.setError(errors.New("a change is still running: press enter again in a moment"))
		return m, nil
	}
	d, err := m.form.domain()
	if err == nil && d.Port > 0 && (m.form.editing == "" || d.Port != m.form.original.Port) {
		if notReady := m.backend.PortsReady(); notReady != nil {
			err = &store.FieldError{Field: store.FieldPort, Msg: notReady.Error()}
		}
	}
	if err == nil && d.Compose != m.form.original.Compose {
		err = m.checkCompose(d.Compose)
	}
	var next []store.Domain
	if err == nil {
		if m.form.editing == "" {
			next, err = store.Add(m.domains, d)
		} else {
			next, err = store.Update(m.domains, m.form.editing, d)
		}
	}
	if err != nil {
		m.form.setError(err)
		return m, nil
	}
	m.mode, m.selectName = modeList, d.Name
	if m.form.editing == "" {
		m.adding = d
	}
	o := m.form.original
	cmd := m.change(next, true)
	if m.form.editing != "" && d.Name == o.Name && d.Address == o.Address && d.Port == o.Port {
		cmd = m.save(next) // only the compose file changed, which nothing applies
	}
	if store.HasCompose(d.Compose) && (d.Compose != o.Compose || d.Address != o.Address) {
		cmd = m.thenLink(cmd, d) // the .env follows the file and the address
	}
	return m.start(cmp.Or(m.form.editing, d.Name), cmd)
}

// neighbor returns the name the cursor goes to once name is deleted, with the
// names under it: the next name left in after, or the one before when none
// follows. It is empty when nothing is left, so the cursor lands on the add
// row.
func neighbor(before, after []store.Domain, name string) string {
	left := func(d store.Domain) bool {
		return slices.ContainsFunc(after, func(a store.Domain) bool { return a.Name == d.Name })
	}
	i := slices.IndexFunc(before, func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		return ""
	}
	for _, d := range before[i+1:] {
		if left(d) {
			return d.Name
		}
	}
	for j := i - 1; j >= 0; j-- {
		if left(before[j]) {
			return before[j].Name
		}
	}
	return ""
}

// logKey scrolls the log; g or esc goes back to the list.
func (m Model) logKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "g", "esc":
		m.mode = modeList
		return m, nil
	case "q":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.log, cmd = m.log.Update(msg)
	return m, cmd
}

// confirmKey deletes the waiting name on y; any other key keeps it.
func (m Model) confirmKey(k string) (tea.Model, tea.Cmd) {
	m.mode = modeList
	if k != "y" || m.busy {
		return m, nil
	}
	next, err := store.Remove(m.domains, m.target)
	if err != nil {
		m.err = err
		return m, nil
	}
	m.selectName = neighbor(m.domains, next, m.target)
	return m.start(m.target, m.change(next, true))
}
