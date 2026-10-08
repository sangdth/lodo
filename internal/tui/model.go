package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/oo/internal/check"
	"github.com/sangdth/oo/internal/compose"
	"github.com/sangdth/oo/internal/store"
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
	modeAsk                 // the compose question at startup
	modePreview             // a compose file with oo's changes
)

// Start is where oo started: the folder the compose question looks for a
// project in, empty to skip it, and the home folder that ~ stands for.
type Start struct {
	Dir  string
	Home string
}

// Model is the TUI's state. Build it with New.
type Model struct {
	ctx     context.Context
	backend Backend
	origin  Start // where oo started

	project  projectMsg // the project oo started in; empty outside one
	asked    bool       // the compose question has run, or had nothing to ask
	question question   // what the compose question offers, while it is open

	mode       mode
	form       form
	target     string       // the name a delete waits on
	note       string       // a short message, such as what linking wrote
	selectName string       // the name to put the cursor on once a change lands
	startedOn  string       // the name under the cursor when the change started; empty on the add row
	adding     store.Domain // the name being added, listed with the spinner until it is saved; Name is empty otherwise

	domains []store.Domain          // in store.Sort order, as the table shows them
	checks  []check.Check           // the last doctor run; nil until the first one finishes
	results map[string]check.Result // the last probe of each enabled name, by name

	log        viewport.Model
	logLines   []string
	logOffset  int64
	logSession int           // counts openings of the log, so a timer from an earlier one stops
	logEvery   time.Duration // how often the open log is read

	preview      viewport.Model
	previewTitle string // the file, its name and how many changes, such as compose.dev.yaml for flowy.oo · 3 changes
	previewEnv   string // the .env line the file's ports need, such as DOCKER_HOST_IP=127.0.1.3; empty when none binds it
	previewOwner string // the name whose compose file the preview shows

	table   table.Model
	spinner spinner.Model
	busy    bool   // a check or a change runs
	pending string // the name or service the running change is for; empty for every enabled name

	onServices bool  // tab moved the keys to the status bar's services
	service    int   // the service the keys act on, in services
	err        error // the last failure, shown until the next change starts

	width, height int
	styles        styles
}

// New returns the TUI for domains, which the caller loaded from domains.json.
// ctx bounds every command the TUI runs; start says where oo started.
func New(ctx context.Context, b Backend, domains []store.Domain, start Start) Model {
	m := Model{
		ctx:      ctx,
		backend:  b,
		origin:   start,
		domains:  store.Sort(domains),
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		log:      viewport.New(),
		preview:  viewport.New(),
		logEvery: logEvery,
		busy:     true,
		width:    defaultWidth,
		height:   defaultHeight,
		styles:   newStyles(),
	}
	m.table = table.New(table.WithFocused(true), table.WithStyles(m.styles.table), table.WithKeyMap(tableKeys()))
	m.layout()
	return m
}

// tableKeys moves the cursor with arrows, j/k, page and home/end keys only,
// so the table never takes a key oo uses, such as space.
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

// Init checks the system and every name while the spinner runs, and looks
// for the project oo started in.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.report(m.domains), m.spinner.Tick}
	if m.origin.Dir != "" {
		cmds = append(cmds, m.findProject())
	}
	return tea.Batch(cmds...)
}

// reportMsg is a finished doctor run, after a service change when err is
// that change's failure.
type reportMsg struct {
	checks  []check.Check
	results []check.Result
	err     error
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

// changedMsg is a finished change. When stored is false nothing was saved and
// the list stays as it was; checks is nil when the change ran no check.
type changedMsg struct {
	domains []store.Domain
	stored  bool
	err     error
	note    string // what to say once it is done, such as what linking wrote
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

// setService turns service on or off, then checks the result.
func (m Model) setService(service string, on bool) tea.Cmd {
	ctx, b, domains := m.ctx, m.backend, m.domains
	return func() tea.Msg {
		err := b.SetService(ctx, service, on)
		checks, results := b.Report(ctx, domains)
		return reportMsg{checks: checks, results: results, err: err}
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

// save writes domains without applying them: a compose path changes no
// generated file, so nothing needs a restart or a check.
func (m Model) save(domains []store.Domain) tea.Cmd {
	b := m.backend
	return func() tea.Msg {
		if err := b.Save(domains); err != nil {
			return changedMsg{err: err}
		}
		return changedMsg{domains: domains, stored: true}
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

// thenLink runs cmd, then, once it saved the list, writes d's address into the
// .env that d's compose file runs with.
func (m Model) thenLink(cmd tea.Cmd, d store.Domain) tea.Cmd {
	ctx, b, home := m.ctx, m.backend, m.origin.Home
	return func() tea.Msg {
		msg, ok := cmd().(changedMsg)
		if !ok || !msg.stored {
			return msg
		}
		env, err := b.LinkEnv(ctx, d.Compose, d.Address)
		if err != nil {
			msg.err = errors.Join(msg.err, err)
			return msg
		}
		msg.note = "linked " + d.Name + " · " + compose.EnvVar + "=" + d.Address + " in " + shortPath(env, home)
		return msg
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
	padded := make([]string, len(m.logLines))
	for i, line := range m.logLines {
		padded[i] = " " + line // the same margin as the log's title
	}
	m.log.SetContentLines(padded)
	if follow {
		m.log.GotoBottom()
	}
}

// start marks the model busy with a change to name, or to every enabled name
// when name is empty, and runs cmd with the spinner going on its row.
func (m Model) start(name string, cmd tea.Cmd) (Model, tea.Cmd) {
	m.busy, m.pending, m.err = true, name, nil
	m.startedOn = m.cursorName()
	m.layout() // a name being added takes a row
	return m, tea.Batch(cmd, m.spinner.Tick)
}

// setReport stores a doctor run.
func (m *Model) setReport(checks []check.Check, results []check.Result) {
	m.checks = checks
	m.results = make(map[string]check.Result, len(results))
	for _, r := range results {
		m.results[r.Name] = r
	}
}

// placeCursor puts the cursor on the name the change asked for. When the
// cursor moved while the change ran, it stays on here, the name it moved to,
// wherever the change put that row: the change runs in the background. A
// name that is gone leaves the cursor where it is, on a row that exists.
func (m *Model) placeCursor(here string) {
	want := here
	if here == m.startedOn && m.selectName != "" {
		want = m.selectName
	}
	m.selectName, m.startedOn = "", ""
	if want == "" {
		m.table.SetCursor(len(m.listed())) // the add row
	}
	for i, d := range m.listed() {
		if d.Name == want {
			m.table.SetCursor(i)
		}
	}
	if last := len(m.listed()); m.table.Cursor() > last {
		m.table.SetCursor(last) // the add row
	}
	m.table.SetRows(m.rows()) // the add row looks different under the cursor
}

// cursorName returns the name under the cursor, or empty on the add row.
func (m Model) cursorName() string {
	d, _ := m.selected()
	return d.Name
}

// onAddRow reports whether the cursor is on the add row, after the names.
func (m Model) onAddRow() bool {
	return m.table.Cursor() >= len(m.listed())
}

// listed is what the rows show: the names, and the one being added while it
// saves. The cursor counts rows in this list.
func (m Model) listed() []store.Domain {
	if m.adding.Name == "" {
		return m.domains
	}
	return store.Sort(append(slices.Clone(m.domains), m.adding))
}

// selected returns the domain under the cursor; none on the add row.
func (m Model) selected() (store.Domain, bool) {
	listed := m.listed()
	i := m.table.Cursor()
	if i < 0 || i >= len(listed) {
		return store.Domain{}, false
	}
	return listed[i], true
}
