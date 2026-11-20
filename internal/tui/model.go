package tui

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/store"
)

// queuedOp is one user action in the dashboard's background change queue.
// Multiple actions pressed while an apply runs coalesce into a single save,
// apply and probe pass.
type queuedOp struct {
	names      []string // domain or service names whose spinner runs until this op finishes
	mutate     func([]store.Domain) ([]store.Domain, error)
	optimistic bool // replay mutate onto m.domains while queued
	apply      bool
	report     bool
	note       func([]store.Domain) string
	adding     *store.Domain
	deleting   string
	reload     bool
	service    string
	serviceOn  bool
}

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
	modeSetup               // the setup form for the project lodo started in
	modePreview             // a compose file with lodo's changes
)

// Start is where lodo started: the folder the scan looks for a project in,
// empty to skip it, and the home folder that ~ stands for.
type Start struct {
	Dir  string
	Home string
	Name string // the project name --name gave; empty lets the scan pick it
	Auto bool   // the setup form saves what its first scan proposes without waiting for enter: lodo dev -y
}

// deps is what the dashboard and the setup form run backend calls with: the
// context that bounds them, the backend, and where lodo started.
type deps struct {
	ctx     context.Context
	backend Backend
	origin  Start
}

// Model is the TUI's state. Build it with New.
type Model struct {
	deps

	setup Setup // the setup form i opened

	mode       mode
	form       form
	target     string         // the name a delete waits on
	note       string         // a short message, such as what linking wrote
	selectName string         // the name to put the cursor on once a change lands
	startedOn  string         // the name under the cursor when the change started; empty on the add row
	adding     []store.Domain // names being added, listed with the spinner until saved

	domains   []store.Domain          // in store.Sort order, as the table shows them
	checks    []check.Check           // the last doctor run; nil until the first one finishes
	results   map[string]check.Result // the last probe of each enabled name, by name
	menubarOn bool                    // whether the macOS menu bar icon helper is running

	log        viewport.Model
	logLines   []string
	logOffset  int64
	logSession int           // counts openings of the log, so a timer from an earlier one stops
	logEvery   time.Duration // how often the open log is read

	preview      viewport.Model
	previewTitle string // the file, its name and how many changes, such as compose.dev.yaml for flowy.test · 3 changes
	previewEnv   string // the .env line the file's ports need, such as DOCKER_HOST_IP=127.0.1.3; empty when none binds it

	table     table.Model
	spinner   spinner.Model
	busy      bool           // any probe or change runs (drives spinner ticks)
	probing   bool           // the startup read-only report runs
	applying  bool           // a change batch runs
	seq       int            // monotonic counter for reports and batches
	reportSeq int            // seq of the latest report stored in checks/results
	pending   map[string]int // reference count of active ops per domain or service name
	deleting  map[string]int // reference count of active deletes per domain name
	reloadAll int            // count of active full reloads (r)
	inFlight  []queuedOp     // ops in the currently running batch
	queue     []queuedOp     // ops pressed while inFlight runs, flushed in one batch next

	onServices bool  // tab moved the keys to the status bar's services
	service    int   // the service the keys act on, in services
	err        error // the last failure, shown until the next change starts

	width, height int
	styles        styles
}

// New returns the TUI for domains, which the caller loaded from domains.json.
// ctx bounds every command the TUI runs; start says where lodo started.
func New(ctx context.Context, b Backend, domains []store.Domain, start Start) Model {
	m := Model{
		deps:     deps{ctx: ctx, backend: b, origin: start},
		domains:  store.Sort(domains),
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		log:      viewport.New(),
		preview:  previewPort(),
		logEvery: logEvery,
		busy:     true,
		probing:  true,
		pending:  map[string]int{},
		deleting: map[string]int{},
		width:    defaultWidth,
		height:   defaultHeight,
		styles:   newStyles(),
	}
	m.table = table.New(table.WithFocused(true), table.WithStyles(m.styles.table), table.WithKeyMap(tableKeys()))
	m.layout()
	return m
}

// previewPort scrolls sideways with the arrows only, so l stays the link key.
func previewPort() viewport.Model {
	v := viewport.New()
	v.KeyMap.Left = key.NewBinding(key.WithKeys("left"))
	v.KeyMap.Right = key.NewBinding(key.WithKeys("right"))
	return v
}

// tableKeys moves the cursor with arrows, j/k, page and home/end keys only,
// so the table never takes a key lodo uses, such as space.
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
	return tea.Batch(m.report(m.domains, m.seq), m.spinner.Tick)
}

// reportMsg is a finished startup doctor run.
type reportMsg struct {
	seq       int
	checks    []check.Check
	results   []check.Result
	menubarOn bool
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

// changedMsg is a finished change batch. When synced is true, domains holds
// the authoritative list from disk; checks is nil when the batch ran no probe.
type changedMsg struct {
	seq       int
	domains   []store.Domain
	stored    bool
	synced    bool
	err       error
	note      string // what to say once it is done, such as what linking wrote
	checks    []check.Check
	results   []check.Result
	menubarOn bool
}

func (m Model) report(domains []store.Domain, seq int) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		checks, results := b.Report(ctx, domains)
		return reportMsg{seq: seq, checks: checks, results: results, menubarOn: b.MenubarOn(ctx)}
	}
}

// runBatch executes ops in order under one file lock, coalescing domain edits
// into a single Save, Apply and Report pass.
func (m Model) runBatch(seq int, ops []queuedOp) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		msg := changedMsg{seq: seq}
		needReport := false
		if err := b.WithLock(func() error {
			domains, err := b.Load()
			if err != nil {
				msg.err = err
				return nil
			}
			needApply, dirty := false, false
			for _, op := range ops {
				switch {
				case op.service != "":
					if err := b.SetService(ctx, op.service, op.serviceOn); err != nil && msg.err == nil {
						msg.err = err
					}
					needReport = true
				case op.reload:
					if dirty {
						if err := b.Save(domains); err != nil {
							if fresh, lErr := b.Load(); lErr == nil {
								msg.domains, msg.synced = fresh, true
							}
							if msg.err == nil {
								msg.err = err
							}
							return nil
						}
						dirty = false
					}
					domains, err = b.Load()
					if err != nil {
						msg.err = err
						return nil
					}
					msg.domains, msg.stored, msg.synced = domains, true, true
					needApply, needReport = true, true
				default:
					next, mErr := op.mutate(domains)
					if mErr != nil {
						if msg.err == nil {
							msg.err = mErr
						}
						msg.domains, msg.synced = domains, true
						continue
					}
					domains = next
					dirty = true
					needApply = needApply || op.apply
					needReport = needReport || op.report
					if op.note != nil {
						msg.note = op.note(domains)
					}
				}
			}
			if dirty {
				if err := b.Save(domains); err != nil {
					if fresh, lErr := b.Load(); lErr == nil {
						msg.domains, msg.synced = fresh, true
					}
					if msg.err == nil {
						msg.err = err
					}
					return nil
				}
				msg.domains, msg.stored, msg.synced = domains, true, true
			} else if msg.synced {
				msg.domains = domains
			}
			if needApply {
				if err := b.Apply(ctx, domains); err != nil && msg.err == nil {
					msg.err = err
				}
			}
			if needReport && !msg.synced {
				msg.domains = domains
			}
			return nil
		}); err != nil {
			return changedMsg{seq: seq, err: err}
		}
		if needReport {
			msg.checks, msg.results = b.Report(ctx, msg.domains)
			msg.menubarOn = b.MenubarOn(ctx)
		}
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

// cloneState clones Model's mutable maps and slices so value-copied Bubble Tea
// models never alias an earlier state.
func (m *Model) cloneState() {
	m.pending = maps.Clone(m.pending)
	m.deleting = maps.Clone(m.deleting)
	m.adding = slices.Clone(m.adding)
	m.inFlight = slices.Clone(m.inFlight)
	m.queue = slices.Clone(m.queue)
}

// enqueue applies op's optimistic state at once, spins its row(s), and either
// runs it now or queues it behind the running batch so rapid actions never
// block one another.
func (m Model) enqueue(op queuedOp, selectName string) (Model, tea.Cmd) {
	m.cloneState()
	wasBusy := m.busy
	m.err = nil
	m.startedOn = m.cursorName()
	m.selectName = selectName
	if op.optimistic && op.mutate != nil {
		if next, err := op.mutate(m.domains); err == nil {
			m.domains = next
		} else if len(m.adding) > 0 {
			if nextAdd, errAdd := op.mutate(m.adding); errAdd == nil {
				m.adding = nextAdd
			}
		}
	}
	for _, name := range op.names {
		if name != "" {
			m.pending[name]++
		}
	}
	if op.reload {
		m.reloadAll++
	}
	if op.adding != nil {
		m.adding = append(m.adding, *op.adding)
	}
	if op.deleting != "" {
		m.deleting[op.deleting]++
	}
	if m.applying {
		m.queue = append(m.queue, op)
		m.layout()
		return m, nil
	}
	m.applying = true
	m.busy = true
	m.inFlight = []queuedOp{op}
	m.seq++
	m.layout()
	cmd := m.runBatch(m.seq, m.inFlight)
	if !wasBusy {
		return m, tea.Batch(cmd, m.spinner.Tick)
	}
	return m, cmd
}

// finishInFlight drops the spinner and temporary add/delete markers of the
// batch that just finished.
func (m *Model) finishInFlight() {
	m.cloneState()
	for _, op := range m.inFlight {
		for _, name := range op.names {
			if m.pending[name] <= 1 {
				delete(m.pending, name)
			} else {
				m.pending[name]--
			}
		}
		if op.reload && m.reloadAll > 0 {
			m.reloadAll--
		}
		if op.adding != nil {
			m.adding = slices.DeleteFunc(m.adding, func(d store.Domain) bool { return d.Name == op.adding.Name })
		}
		if op.deleting != "" {
			if m.deleting[op.deleting] <= 1 {
				delete(m.deleting, op.deleting)
			} else {
				m.deleting[op.deleting]--
			}
		}
	}
	m.inFlight = nil
}

// isDeleting reports whether name or an ancestor of name has an active delete.
func (m Model) isDeleting(name string) bool {
	for del := range m.deleting {
		if name == del || strings.HasSuffix(name, "."+del) {
			return true
		}
	}
	return false
}

// setReport stores a doctor run.
func (m *Model) setReport(checks []check.Check, results []check.Result, menubarOn bool) {
	m.checks = checks
	m.menubarOn = menubarOn
	m.results = make(map[string]check.Result, len(results))
	for _, r := range results {
		m.results[r.Name] = r
	}
}

// placeCursor puts the cursor on the name the change asked for. When the
// cursor moved while the change ran, it stays on here, the name it moved to,
// wherever the change put that row: the change runs in the background. When
// clear is false, a queued batch still awaits its own cursor target unless it
// matches now.
func (m *Model) placeCursor(here string, before []store.Domain, clear bool) {
	want := here
	if here == m.startedOn && m.selectName != "" {
		want = m.selectName
		clear = true
	}
	if clear {
		m.selectName, m.startedOn = "", ""
	}
	listed := m.listed()
	if want == "" {
		m.table.SetCursor(len(listed)) // the add row
	} else if i := slices.IndexFunc(listed, func(d store.Domain) bool { return d.Name == want }); i >= 0 {
		m.table.SetCursor(i)
	} else if fb := neighbor(before, listed, want); fb != "" {
		if j := slices.IndexFunc(listed, func(d store.Domain) bool { return d.Name == fb }); j >= 0 {
			m.table.SetCursor(j)
		}
	} else if len(before) > 0 && len(listed) == 0 {
		m.table.SetCursor(0)
	}
	if last := len(listed); m.table.Cursor() > last {
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

// listed is what the rows show: the names, and any being added while they
// save. The cursor counts rows in this list.
func (m Model) listed() []store.Domain {
	if len(m.adding) == 0 {
		return m.domains
	}
	out := slices.Clone(m.domains)
	for _, a := range m.adding {
		if _, exists := store.Find(out, a.Name); !exists {
			out = append(out, a)
		}
	}
	return store.Sort(out)
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
