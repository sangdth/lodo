package tui

import (
	"cmp"
	"errors"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
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
		m.setReport(msg.checks, msg.results)
		return m, nil
	case changedMsg:
		m.busy, m.err = false, msg.err
		if msg.stored {
			m.domains = store.Sort(msg.domains)
			m.setReport(msg.checks, msg.results)
		}
		m.table.SetRows(m.rows()) // the spinner leaves the rows
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
	case copiedMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.note = "copied " + msg.text
		}
		return m, nil
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
	}
	if k == "l" {
		return m.openLog() // reading the log is safe while a change runs
	}
	if k == "tab" {
		m.onServices = !m.onServices
		if m.onServices {
			m.table.Blur()
		} else {
			m.table.Focus()
		}
		return m, nil
	}
	if m.onServices {
		return m.serviceKey(k)
	}
	if !m.busy {
		if next, cmd, ok := m.listKey(k); ok {
			return next, cmd
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// listKey handles the list's action keys, and reports whether k was one.
func (m Model) listKey(k string) (Model, tea.Cmd, bool) {
	switch k {
	case "q":
		return m, tea.Quit, true
	case "r":
		next, cmd := m.start("", m.reload())
		return next, cmd, true
	case "a":
		m.mode, m.err, m.form = modeForm, nil, newAddForm(m.domains)
		return m, nil, true
	}
	d, ok := m.selected()
	if !ok {
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
		started, cmd := m.start(d.Name, m.change(next))
		return started, cmd, true
	case "e":
		m.mode, m.err, m.form = modeForm, nil, newEditForm(m.domains, d)
		return m, nil, true
	case "d":
		m.mode, m.err, m.target = modeConfirm, nil, d.Name
		return m, nil, true
	case "c":
		return m, m.copyText("DOCKER_HOST_IP=" + d.Address), true
	}
	return m, nil, false
}

// serviceKey handles the keys while they act on the status bar's services:
// left and right pick one, space turns it on or off.
func (m Model) serviceKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q":
		return m, tea.Quit
	case "left":
		m.service = max(m.service-1, 0)
	case "right":
		m.service = min(m.service+1, len(system.Services)-1)
	case "space":
		if m.busy {
			return m, nil
		}
		service := system.Services[m.service]
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
	return m.start(cmp.Or(m.form.editing, d.Name), m.change(next))
}

// logKey scrolls the log; l or esc goes back to the list.
func (m Model) logKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "l", "esc":
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
	return m.start(m.target, m.change(next))
}
