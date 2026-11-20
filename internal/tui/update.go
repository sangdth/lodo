package tui

import (
	"fmt"
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
		m.probing = false
		m.busy = m.applying
		here, before := m.cursorName(), m.listed()
		if m.checks == nil || msg.seq >= m.reportSeq {
			m.reportSeq = msg.seq
			m.setReport(msg.checks, msg.results, msg.menubarOn)
		}
		m.layout()
		m.placeCursor(here, before, !m.applying)
		return m, nil
	case changedMsg:
		here, before := m.cursorName(), m.listed() // before the rows change under the cursor
		m.finishInFlight()
		m.err = msg.err
		if msg.note != "" {
			m.note = msg.note
		}
		if msg.synced {
			m.domains = store.Sort(msg.domains)
			for _, q := range m.queue {
				if q.optimistic && q.mutate != nil {
					if next, err := q.mutate(m.domains); err == nil {
						m.domains = next
					}
				}
			}
		}
		if msg.checks != nil && msg.seq >= m.reportSeq {
			m.reportSeq = msg.seq
			m.setReport(msg.checks, msg.results, msg.menubarOn)
		}
		if len(m.queue) > 0 {
			batch := m.queue
			m.queue = nil
			m.inFlight = batch
			m.seq++
			m.layout()
			m.placeCursor(here, before, false)
			return m, m.runBatch(m.seq, batch)
		}
		m.applying = false
		m.busy = m.probing
		m.layout()
		m.placeCursor(here, before, true)
		return m, nil
	case setupScanMsg:
		if m.mode != modeSetup {
			return m, nil // the form closed while it scanned
		}
		s, cmd := m.setup.update(msg)
		return m.afterSetup(s, cmd)
	case linkScanMsg:
		d, ok := store.Find(m.listed(), msg.name)
		if !ok || m.isDeleting(d.Name) {
			return m, nil // deleted while the scan ran
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

// key handles a key press. ctrl+c always quits. Row actions run optimistically
// and queue behind any running apply so acting on one row never blocks others.
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
		m.setup = NewSetup(m.ctx, m.backend, m.listed(), m.origin)
		m.setup.embedded = true
		m.setup.width, m.setup.height = m.innerWidth(), m.logHeight()
		return m, m.setup.Init()
	}
	if next, cmd, ok := m.listKey(k); ok {
		return next, cmd
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
// is done, the list shows again, and a form that saved enqueues its change
// onto Model's batch queue with the spinner on the project's name.
func (m Model) afterSetup(s Setup, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if !s.done {
		m.setup = s
		return m, cmd
	}
	m.setup, m.mode = s, modeList
	if !s.saved {
		return m, nil
	}
	link := !s.unlink && s.proposal.Compose != ""
	home := m.origin.Home
	project := s.projectName()
	var note func([]store.Domain) string
	if link {
		note = func(ds []store.Domain) string {
			if owner, ok := store.Find(ds, project); ok {
				return linkNote(owner, home)
			}
			return ""
		}
	}
	return m.enqueue(queuedOp{
		names:      []string{project},
		optimistic: true,
		apply:      true,
		report:     true,
		note:       note,
		mutate: func(fresh []store.Domain) ([]store.Domain, error) {
			cur := s
			cur.domains = fresh
			built, _, _, err := cur.build(link)
			if err != nil {
				return nil, err
			}
			if err := cur.checkPortsReady(fresh, built); err != nil {
				return nil, err
			}
			return built, nil
		},
	}, project)
}

// openAdd opens the add form, for a subdomain of parent when it is set.
func (m Model) openAdd(parent string) Model {
	m.mode, m.err, m.form = modeForm, nil, newAddForm(m.listed(), parent, m.origin)
	return m
}

// openEdit opens the edit form for d.
func (m Model) openEdit(d store.Domain) Model {
	m.mode, m.err, m.form = modeForm, nil, newEditForm(m.listed(), d, m.origin)
	return m
}

// listKey handles the list's action keys, and reports whether k was one.
func (m Model) listKey(k string) (Model, tea.Cmd, bool) {
	switch k {
	case "q":
		if m.applying {
			return m, nil, true
		}
		return m, tea.Quit, true
	case "r":
		next, cmd := m.enqueue(queuedOp{reload: true}, "")
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
	if m.isDeleting(d.Name) {
		return m, nil, false
	}
	switch k {
	case "space":
		if _, err := store.Toggle(m.listed(), d.Name); err != nil {
			m.err = err
			return m, nil, true
		}
		want := !d.Enabled
		started, cmd := m.enqueue(queuedOp{
			names:      []string{d.Name},
			optimistic: true,
			apply:      true,
			report:     true,
			mutate: func(fresh []store.Domain) ([]store.Domain, error) {
				cur, ok := store.Find(fresh, d.Name)
				if !ok {
					return nil, fmt.Errorf("%s is not listed", d.Name)
				}
				cur.Enabled = want
				return store.Update(fresh, d.Name, cur)
			},
		}, d.Name)
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
	if _, err := store.Update(m.listed(), d.Name, changed); err != nil {
		m.err = err
		return m, nil
	}
	want := changed.HTTPS
	return m.enqueue(queuedOp{
		names:      []string{d.Name},
		optimistic: true,
		apply:      true,
		report:     true,
		mutate: func(fresh []store.Domain) ([]store.Domain, error) {
			cur, ok := store.Find(fresh, d.Name)
			if !ok {
				return nil, fmt.Errorf("%s is not listed", d.Name)
			}
			cur.HTTPS = want
			return store.Update(fresh, d.Name, cur)
		},
	}, d.Name)
}

// serviceKey handles the keys while they act on the status bar's services:
// left and right, or h and l, pick one; space turns it on or off.
func (m Model) serviceKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q":
		if m.applying {
			return m, nil
		}
		return m, tea.Quit
	case "left", "h":
		m.service = max(m.service-1, 0)
	case "right", "l":
		m.service = min(m.service+1, len(services)-1)
	case "space":
		service := services[m.service]
		if m.pending[service] > 0 || m.checks == nil {
			return m, nil
		}
		wantOn := !m.menubarOn
		if id := serviceCheck(service); id > 0 {
			c, ok := m.check(id)
			if !ok {
				return m, nil // no report yet, so on or off is unknown
			}
			wantOn = c.Off
		}
		return m.enqueue(queuedOp{
			names:     []string{service},
			service:   service,
			serviceOn: wantOn,
		}, "")
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
	m.form = m.form.update(msg, m.listed())
	return m, nil
}

// submit checks the form against every rule and, when it passes, saves and
// applies the new list.
func (m Model) submit() (tea.Model, tea.Cmd) {
	d, err := m.form.domain()
	if err == nil && d.Port > 0 && (m.form.editing == "" || d.Port != m.form.original.Port) {
		if notReady := m.backend.PortsReady(); notReady != nil {
			err = &store.FieldError{Field: store.FieldPort, Msg: notReady.Error()}
		}
	}
	if err == nil && d.Compose != m.form.original.Compose {
		err = m.checkCompose(d.Compose)
	}
	listed := m.listed()
	if err == nil {
		if m.form.editing == "" {
			_, err = store.Add(listed, d)
		} else {
			_, err = store.Update(listed, m.form.editing, d)
		}
	}
	if err != nil {
		m.form.setError(err)
		return m, nil
	}
	m.mode = modeList
	editing := m.form.editing
	if editing == "" {
		added := d
		return m.enqueue(queuedOp{
			names:  []string{d.Name},
			adding: &added,
			apply:  true,
			report: true,
			mutate: func(fresh []store.Domain) ([]store.Domain, error) {
				return store.Add(fresh, d)
			},
		}, d.Name)
	}
	o := m.form.original
	needApply := d.Name != o.Name || d.Address != o.Address || d.Port != o.Port
	return m.enqueue(queuedOp{
		names:      []string{editing, d.Name},
		optimistic: true,
		apply:      needApply,
		report:     needApply,
		mutate: func(fresh []store.Domain) ([]store.Domain, error) {
			return store.Update(fresh, editing, d)
		},
	}, d.Name)
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
		if m.applying {
			return m, nil
		}
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.log, cmd = m.log.Update(msg)
	return m, cmd
}

// confirmKey deletes the waiting name on y; any other key keeps it.
func (m Model) confirmKey(k string) (tea.Model, tea.Cmd) {
	m.mode = modeList
	if k != "y" || m.isDeleting(m.target) {
		return m, nil
	}
	listed := m.listed()
	next, err := store.Remove(listed, m.target)
	if err != nil {
		m.err = err
		return m, nil
	}
	target := m.target
	return m.enqueue(queuedOp{
		names:    []string{target},
		deleting: target,
		apply:    true,
		report:   true,
		mutate: func(fresh []store.Domain) ([]store.Domain, error) {
			return store.Remove(fresh, target)
		},
	}, neighbor(listed, next, target))
}
