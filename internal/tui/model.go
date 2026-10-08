package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/store"
)

// The terminal size before the first WindowSizeMsg.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// The log view reads dnsmasq's log this often and keeps this many lines.
const (
	logEvery    = 500 * time.Millisecond
	maxLogLines = 2000
)

// mode is what the keys act on.
type mode int

const (
	modeList    mode = iota // the table
	modeForm                // the add or edit form
	modeConfirm             // a delete waiting for y
	modeLog                 // dnsmasq's query log
)

// Model is the TUI's state. Build it with New.
type Model struct {
	ctx     context.Context
	backend Backend

	mode       mode
	form       form
	target     string // the name a delete waits on
	note       string // a short message, such as what copy env copied
	selectName string // the name to put the cursor on once a change lands

	domains []store.Domain          // in store.Sort order, as the table shows them
	checks  []check.Check           // the last doctor run; nil until the first one finishes
	results map[string]check.Result // the last probe of each enabled name, by name

	log        viewport.Model
	logLines   []string
	logOffset  int64
	logSession int           // counts openings of the log, so a timer from an earlier one stops
	logEvery   time.Duration // how often the open log is read

	table   table.Model
	spinner spinner.Model
	busy    string // what runs now, such as "applying"; empty when idle
	err     error  // the last failure, shown until the next change starts

	width, height int
	styles        styles
}

// New returns the TUI for domains, which the caller loaded from domains.json.
// ctx bounds every command the TUI runs.
func New(ctx context.Context, b Backend, domains []store.Domain) Model {
	m := Model{
		ctx:      ctx,
		backend:  b,
		domains:  store.Sort(domains),
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		log:      viewport.New(),
		logEvery: logEvery,
		busy:     "checking",
		width:    defaultWidth,
		height:   defaultHeight,
		styles:   newStyles(),
	}
	m.table = table.New(table.WithFocused(true), table.WithStyles(m.styles.table), table.WithKeyMap(tableKeys()))
	m.layout()
	return m
}

// tableKeys moves the cursor with arrows, j/k, page and home/end keys only,
// so the table never takes a key lcd uses, such as space.
func tableKeys() table.KeyMap {
	return table.KeyMap{
		LineUp:     key.NewBinding(key.WithKeys("up", "k")),
		LineDown:   key.NewBinding(key.WithKeys("down", "j")),
		PageUp:     key.NewBinding(key.WithKeys("pgup")),
		PageDown:   key.NewBinding(key.WithKeys("pgdown")),
		GotoTop:    key.NewBinding(key.WithKeys("home")),
		GotoBottom: key.NewBinding(key.WithKeys("end")),
	}
}

// Init checks the system and every name while the spinner runs.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.report(m.domains), m.spinner.Tick)
}

// reportMsg is a finished doctor run.
type reportMsg struct {
	checks  []check.Check
	results []check.Result
}

// logTickMsg asks the open log to read what dnsmasq added.
type logTickMsg struct{ session int }

// logMsg is what dnsmasq added to its log since the last read.
type logMsg struct {
	session int
	text    string
	offset  int64
	err     error
}

// copiedMsg is a finished copy to the clipboard.
type copiedMsg struct {
	text string
	err  error
}

// changedMsg is a finished change. When stored is false nothing was saved and
// the list stays as it was.
type changedMsg struct {
	domains []store.Domain
	stored  bool
	err     error
	checks  []check.Check
	results []check.Result
}

func (m Model) report(domains []store.Domain) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		checks, results := b.Report(ctx, domains)
		return reportMsg{checks: checks, results: results}
	}
}

// change saves domains, applies them and checks the result, in that order. An
// apply failure still keeps the saved list: r applies it again.
func (m Model) change(domains []store.Domain) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		if err := b.Save(domains); err != nil {
			return changedMsg{err: err}
		}
		err := b.Apply(ctx, domains)
		checks, results := b.Report(ctx, domains)
		return changedMsg{domains: domains, stored: true, err: err, checks: checks, results: results}
	}
}

// reload reads domains.json again, so hand edits count, then applies it.
func (m Model) reload() tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		domains, err := b.Load()
		if err != nil {
			return changedMsg{err: err}
		}
		err = b.Apply(ctx, domains)
		checks, results := b.Report(ctx, domains)
		return changedMsg{domains: domains, stored: true, err: err, checks: checks, results: results}
	}
}

func (m Model) copyText(text string) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		return copiedMsg{text: text, err: b.Copy(ctx, text)}
	}
}

func (m Model) tailLog() tea.Cmd {
	b, session, offset := m.backend, m.logSession, m.logOffset
	return func() tea.Msg {
		text, next, err := b.Tail(offset)
		return logMsg{session: session, text: text, offset: next, err: err}
	}
}

func (m Model) logTick() tea.Cmd {
	session := m.logSession
	return tea.Tick(m.logEvery, func(time.Time) tea.Msg { return logTickMsg{session: session} })
}

// openLog shows dnsmasq's log, read again from its last lines.
func (m Model) openLog() (Model, tea.Cmd) {
	m.mode = modeLog
	m.logSession++
	m.logLines, m.logOffset = nil, 0
	m.log.SetContentLines(nil)
	return m, m.tailLog()
}

// appendLog adds what dnsmasq logged and follows the end unless the user
// scrolled up.
func (m *Model) appendLog(text string, offset int64) {
	m.logOffset = offset
	if text == "" {
		return
	}
	follow := m.log.AtBottom()
	m.logLines = append(m.logLines, strings.Split(strings.TrimRight(text, "\n"), "\n")...)
	if extra := len(m.logLines) - maxLogLines; extra > 0 {
		m.logLines = m.logLines[extra:]
	}
	m.log.SetContentLines(m.logLines)
	if follow {
		m.log.GotoBottom()
	}
}

// start marks the model busy with what and runs cmd with the spinner going.
func (m Model) start(what string, cmd tea.Cmd) (Model, tea.Cmd) {
	m.busy, m.err = what, nil
	return m, tea.Batch(cmd, m.spinner.Tick)
}

// setReport stores a doctor run and redraws the rows' checks.
func (m *Model) setReport(checks []check.Check, results []check.Result) {
	m.checks = checks
	m.results = make(map[string]check.Result, len(results))
	for _, r := range results {
		m.results[r.Name] = r
	}
	m.table.SetRows(m.rows())
	m.placeCursor()
}

// placeCursor moves the cursor to the name a change asked for, and keeps it
// on a row when rows went away.
func (m *Model) placeCursor() {
	for i, d := range m.domains {
		if d.Name == m.selectName {
			m.table.SetCursor(i)
		}
	}
	m.selectName = ""
	if n := len(m.domains); n > 0 && m.table.Cursor() >= n {
		m.table.SetCursor(n - 1)
	}
}

// selected returns the domain under the cursor.
func (m Model) selected() (store.Domain, bool) {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.domains) {
		return store.Domain{}, false
	}
	return m.domains[i], true
}
