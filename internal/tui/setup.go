package tui

// The setup form: a project's name and address, the names of its apps and of
// the web pages its compose file publishes, and the compose link and .env
// lines lodo writes for it. lodo dev runs it on its own; the dashboard's i
// opens it in the box.

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// setupScanMsg is a scan for the setup form. seq is the form's scan count when
// it started, so a scan a later one overtook is dropped.
type setupScanMsg struct {
	seq      int
	project  scan.Project
	proposal scan.Proposal
	ok       bool
}

// setupRow is a name of the project other than its own: an app's or a web
// page's. An app whose label is empty takes the project's name alone.
type setupRow struct {
	what    string       // the app's folder or the page's label; empty for a listed name with neither
	page    bool         // a web page of the compose file, listed under Services
	listed  store.Domain // the listed name the row edits; Name is empty for a new one
	address string       // the address the scan gave it
	start   string       // the label the scan gave it
	https   bool         // the HTTPS of the listed name the row came from; it follows the row's name
	label   textinput.Model
	port    textinput.Model
}

// The setup form's first focus stops. Each row then takes two, its label's
// and its port's, and the toggles shown come last.
const (
	stopName = iota
	stopAddress
	stopRows
)

func rowStop(i int) int { return stopRows + 2*i }

// The setup form's toggles.
const (
	toggleLink = iota // link the compose file
	toggleEnv         // set the .env lines
)

// Setup is the setup form for the project lodo started in. Build it with
// NewSetup. On its own, it quits once it saved or was canceled; Saved and
// Err tell which.
type Setup struct {
	deps

	domains  []store.Domain // the list the form changes
	embedded bool           // in the dashboard, which goes back to the list and runs the change once done

	seq         int  // counts the scans; each scan's message carries it
	got         int  // the seq of the last scan that came back
	refill      bool // the next scan takes over a listed project: it fills the fields again
	scanned     bool
	found       bool // the scan found a project with something to set up
	project     scan.Project
	proposal    scan.Proposal // the last scan's
	listed      store.Domain  // the project's name as listed; Name is empty for a new project
	baseAddress string        // the address the first scan gave the project; rows on it follow the address field

	name    textinput.Model // the project's label, before .test
	address textinput.Model
	rows    []setupRow
	off     [2]bool // each toggle switched off
	focus   int     // the focus stop the keys act on
	dirty   bool    // a field changed since the last scan
	errAt   int     // the focus stop errMsg is about; -1 for none
	errMsg  string

	submitting bool   // enter waits for the scan of the fields as typed
	saving     bool   // the change runs; on its own only
	done       bool   // saved or canceled
	saved      bool   // the list holds the project's names
	note       string // what the change did, such as what linking wrote
	err        error
	width      int
	styles     styles
}

// NewSetup returns the setup form for the project start.Dir is in, which
// changes domains, the list the caller loaded from domains.json. ctx bounds
// every command it runs. Init scans the project; the form fills from the
// project's listed names and the names the scan proposes to add.
func NewSetup(ctx context.Context, b Backend, domains []store.Domain, start Start) Setup {
	return Setup{
		deps:    deps{ctx: ctx, backend: b, origin: start},
		domains: domains,
		errAt:   -1,
		width:   defaultWidth,
		styles:  newStyles(),
	}
}

// Saved reports whether the form saved the project's names: lodo dev then
// starts the apps.
func (s Setup) Saved() bool { return s.saved }

// Err returns why the form couldn't set the project up: lodo didn't start in
// one (scan.ErrNoProject), or saving, applying, linking or setting the .env
// lines failed. A failed link or .env line still leaves the list saved.
func (s Setup) Err() error { return s.err }

// Init scans the project.
func (s Setup) Init() tea.Cmd { return s.scan("", s.domains) }

// Update handles one message.
func (s Setup) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return s.update(msg) }

func (s Setup) update(msg tea.Msg) (Setup, tea.Cmd) {
	s.rows = slices.Clone(s.rows) // the fields change in place; an earlier model keeps its own
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
	case setupScanMsg:
		return s.setScan(msg)
	case changedMsg: // on its own: the dashboard runs the change in the box
		s.saving, s.done, s.saved, s.err, s.note = false, true, msg.stored, msg.err, msg.note
		return s, tea.Quit
	case tea.KeyPressMsg:
		return s.key(msg)
	}
	return s, nil
}

// scan scans the project lodo started in against domains, under name when it
// is set.
func (s Setup) scan(name string, domains []store.Domain) tea.Cmd {
	ctx, b, dir, seq := s.ctx, s.backend, s.origin.Dir, s.seq
	return func() tea.Msg {
		project, proposal, ok := b.Scan(ctx, dir, name, nil, domains)
		return setupScanMsg{seq: seq, project: project, proposal: proposal, ok: ok}
	}
}

// rescan scans again with the fields as typed, against the list as saving
// would leave it, so the .env lines and notes follow the names and ports.
// Fields that break a rule wait for enter, which says why.
func (s Setup) rescan() (Setup, tea.Cmd) {
	next, _, _, err := s.build(false)
	if err != nil {
		return s, nil
	}
	s.seq++
	s.dirty = false
	name := s.projectName()
	_, listed := store.Find(s.domains, name)
	s.refill = listed && name != s.listed.Name // build let it through, so its folder is unset, gone or this one
	if s.refill {
		return s, s.scan(name, s.domains) // scan it as listed, so its address and names stay
	}
	return s, s.scan(name, next)
}

// setScan stores a scan. The first one, and one of a listed project taken
// over, fill the fields; others keep them and only bring the compose file,
// the .env lines and the notes.
func (s Setup) setScan(msg setupScanMsg) (Setup, tea.Cmd) {
	if msg.seq != s.seq {
		return s, nil
	}
	s.got = msg.seq
	if !msg.ok { // the project may also be gone since the first scan
		s.scanned, s.found, s.submitting, s.err = true, false, false, scan.ErrNoProject
		return s, nil
	}
	first := !s.scanned
	s.scanned, s.project, s.proposal = true, msg.project, msg.proposal
	if first {
		s.found = s.proposal.CanAccept() || s.proposal.Listed
	}
	if first || s.refill {
		s.refill = false
		s.fill()
	}
	s.focusOn(min(s.focus, s.stops()-1)) // the toggles shown may have changed
	if s.submitting {
		s.submitting = false
		return s.submit()
	}
	return s, nil
}

// fill fills the fields from the scan: the project's listed names, then the
// names it proposes to add; apps first, then pages. A listed name is the
// project's when its folder is unset, gone or the project's own; another
// project's still on disk stays that project's, so build refuses it.
func (s *Setup) fill() {
	p := s.proposal
	d, listed := store.Find(s.domains, p.Name)
	mine := !listed || d.Root == "" || d.Root == p.Root || s.backend.Gone(d.Root)
	s.listed = store.Domain{}
	if listed && mine {
		s.listed = d
	}
	s.baseAddress = p.Address
	s.errAt, s.errMsg = -1, "" // the rows it was about are gone
	inputs := newInputs(store.Suffix)
	s.name, s.address = inputs[fieldName], inputs[fieldAddress]
	s.name.Placeholder = "flowy"
	s.name.SetValue(strings.TrimSuffix(p.Name, store.Suffix))
	s.address.SetValue(p.Address)

	var rows []setupRow
	if s.listed.Port > 0 {
		rows = append(rows, s.row(s.listed, false)) // the project's name serves an app
	}
	if mine {
		for _, d := range store.Under(s.domains, p.Name) {
			rows = append(rows, s.row(d, true))
		}
	}
	for _, d := range p.Add {
		if d.Name != p.Name || d.Port > 0 || p.Dirs[d.Name] != "" {
			rows = append(rows, s.row(d, false))
		}
	}
	apps := slices.DeleteFunc(slices.Clone(rows), func(r setupRow) bool { return r.page })
	pages := slices.DeleteFunc(rows, func(r setupRow) bool { return !r.page })
	s.rows = append(apps, pages...)
}

// row returns the row for d, a name of the project: an app's when the scan
// tied it to an app's folder or it has an app's port, a page's when it has a
// page's port.
func (s Setup) row(d store.Domain, listed bool) setupRow {
	r := setupRow{what: s.proposal.Dirs[d.Name], address: d.Address, https: d.HTTPS}
	if listed {
		r.listed = d
	}
	if r.what == "" && d.Port > 0 {
		if i := slices.IndexFunc(s.project.Apps, func(a scan.App) bool { return a.Port == d.Port }); i >= 0 {
			r.what = s.project.Apps[i].Dir
		} else if i := slices.IndexFunc(s.project.Pages, func(pg scan.Page) bool { return pg.Port == d.Port }); i >= 0 {
			r.what, r.page = s.project.Pages[i].Label, true
		}
	}
	inputs := newInputs("." + s.proposal.Name)
	r.label, r.port = inputs[fieldName], inputs[fieldPort]
	r.label.Placeholder = "" // an empty label shows the project's name alone
	r.label.SetValue(strings.TrimSuffix(strings.TrimSuffix(d.Name, s.proposal.Name), "."))
	r.start = r.label.Value()
	if d.Port > 0 {
		r.port.SetValue(strconv.Itoa(d.Port))
	}
	return r
}

// projectName is the project's label as typed, with .test.
func (s Setup) projectName() string {
	return strings.TrimSuffix(strings.TrimSpace(s.name.Value()), store.Suffix) + store.Suffix
}

// build returns the list as saving would leave it and the project's own
// name, or the focus stop of the field that breaks a rule and why. The
// project's listed names are replaced: one renamed or changed keeps its
// Enabled, HTTPS, Compose and Root, and a row's HTTPS follows it onto the
// project's name or off it. The project's name gets the project's folder as
// Root, the port of the app with an empty label, and with link the compose
// file the scan offers. An app whose port lodo can't tell keeps the label the
// scan gave it (scan.ErrUnknownPort). A listed project name typed in, whose
// folder is unset or gone since it was renamed or moved, is taken over: it
// and the names of it the rows give are edited, not refused as listed.
func (s Setup) build(link bool) ([]store.Domain, store.Domain, int, error) {
	project := s.projectName()
	if err := store.ValidateProject(project); err != nil {
		return nil, store.Domain{}, stopName, err
	}
	address := strings.TrimSpace(s.address.Value())
	if err := store.ValidateAddress(address); err != nil {
		return nil, store.Domain{}, stopAddress, err
	}
	owner, claim := s.listed, false
	if d, ok := store.Find(s.domains, project); ok && project != s.listed.Name {
		if d.Root != "" && d.Root != s.proposal.Root && !s.backend.Gone(d.Root) {
			return nil, store.Domain{}, stopName, errors.New(project + " belongs to " + s.shortPath(d.Root) + ": give this project another name")
		}
		owner, claim = d, true
	}
	if owner.Name == "" {
		owner.Enabled = true
	}
	owner.Name, owner.Address, owner.Port, owner.Root = project, address, 0, s.proposal.Root
	if link && s.proposal.Compose != "" {
		owner.Compose = s.proposal.Compose
	}
	bare := -1 // the row with the project's name alone
	ports := make([]int, len(s.rows))
	for i, r := range s.rows {
		port, err := store.ParsePort(r.port.Value())
		if err != nil {
			return nil, owner, rowStop(i) + 1, err
		}
		ports[i] = port
		label := strings.TrimSpace(r.label.Value())
		if label != r.start && !r.page && slices.ContainsFunc(s.project.Apps, func(a scan.App) bool { return a.Dir == r.what && a.Port == 0 }) {
			return nil, owner, rowStop(i), scan.ErrUnknownPort
		}
		if label != "" {
			continue
		}
		if bare >= 0 {
			return nil, owner, rowStop(i), errors.New("only one app can be " + project + " alone: give this one a label")
		}
		bare, owner.Port, owner.HTTPS = i, port, r.https
	}
	owner.HTTPS = owner.HTTPS && owner.Port > 0 // Caddy serves only names with a port

	gone := map[string]bool{s.listed.Name: true, project: true}
	edits := make([]store.Domain, len(s.rows)) // the listed name each row edits
	for i, r := range s.rows {
		edits[i] = r.listed
		if label := strings.TrimSpace(r.label.Value()); claim && r.listed.Name == "" && label != "" {
			edits[i], _ = store.Find(s.domains, label+"."+project) // a name of the project taken over
		}
		gone[edits[i].Name] = true
	}
	others := slices.DeleteFunc(slices.Clone(s.domains), func(d store.Domain) bool { return gone[d.Name] })
	next, err := store.Add(others, owner)
	if err != nil {
		return nil, owner, stopFor(err, stopName, bare), err
	}
	for i, r := range s.rows {
		label := strings.TrimSpace(r.label.Value())
		if label == "" {
			continue
		}
		d := edits[i]
		if d.Name == "" {
			d.Enabled, d.HTTPS = true, r.https
		}
		d.Name, d.Address, d.Port = label+"."+project, r.address, ports[i]
		if r.address == s.baseAddress {
			d.Address = address
		}
		d.HTTPS = d.HTTPS && d.Port > 0
		if next, err = store.Add(next, d); err != nil {
			return nil, owner, stopFor(err, rowStop(i), i), err
		}
	}
	return next, owner, -1, nil
}

// stopFor returns the focus stop a rule of store's is about: the address
// field, the port of row when it is set, or else at.
func stopFor(err error, at, row int) int {
	fe, ok := errors.AsType[*store.FieldError](err)
	switch {
	case !ok:
	case fe.Field == store.FieldAddress:
		return stopAddress
	case fe.Field == store.FieldPort && row >= 0:
		return rowStop(row) + 1
	}
	return at
}

// toggles are the toggles the form shows: the link when the scan offers a
// compose file, the .env lines when it has some to set.
func (s Setup) toggles() []int {
	var t []int
	if s.proposal.Compose != "" {
		t = append(t, toggleLink)
	}
	if len(s.proposal.Env) > 0 {
		t = append(t, toggleEnv)
	}
	return t
}

// stops counts the focus stops: the fields, then the toggles shown.
func (s Setup) stops() int { return rowStop(len(s.rows)) + len(s.toggles()) }

// input returns the field at focus stop i, or nil at a toggle.
func (s *Setup) input(i int) *textinput.Model {
	switch {
	case i == stopName:
		return &s.name
	case i == stopAddress:
		return &s.address
	case i >= rowStop(len(s.rows)):
		return nil
	case (i-stopRows)%2 == 0:
		return &s.rows[(i-stopRows)/2].label
	}
	return &s.rows[(i-stopRows)/2].port
}

// focusOn moves the cursor to focus stop i, wrapping around.
func (s *Setup) focusOn(i int) {
	n := s.stops()
	s.focus = (i%n + n) % n
	for j := range rowStop(len(s.rows)) {
		if in := s.input(j); j == s.focus {
			in.Focus() // no blink, so no command to run
		} else {
			in.Blur()
		}
	}
}

// key handles a key press: tab, shift+tab, down and up move between the
// fields and toggles, space switches a toggle, enter saves and esc cancels.
// Leaving a changed field scans again.
func (s Setup) key(msg tea.KeyPressMsg) (Setup, tea.Cmd) {
	k := msg.String()
	if s.found {
		s.err = nil // shown until the next key
	}
	switch {
	case k == "ctrl+c":
		return s.finish()
	case s.saving:
		return s, nil
	case k == "esc" || (k == "enter" && s.scanned && !s.found):
		return s.finish()
	case !s.found || s.submitting:
		return s, nil
	case k == "enter":
		return s.enter()
	case k == "tab" || k == "down":
		return s.move(1)
	case k == "shift+tab" || k == "up":
		return s.move(-1)
	}
	in := s.input(s.focus)
	if in == nil {
		if k == "space" {
			t := s.toggles()[s.focus-rowStop(len(s.rows))]
			s.off[t] = !s.off[t]
		}
		return s, nil
	}
	before := in.Value()
	*in, _ = in.Update(msg) // the cursor doesn't blink, so no command
	if in.Value() != before {
		s.dirty = true
		if s.errAt == s.focus {
			s.errAt, s.errMsg = -1, ""
		}
	}
	return s, nil
}

// finish ends the form unsaved. On its own the program quits.
func (s Setup) finish() (Setup, tea.Cmd) {
	s.done = true
	if s.embedded {
		return s, nil
	}
	return s, tea.Quit
}

// move moves the focus by delta, and scans again when a field changed.
func (s Setup) move(delta int) (Setup, tea.Cmd) {
	s.focusOn(s.focus + delta)
	if !s.dirty {
		return s, nil
	}
	return s.rescan()
}

// enter saves the form. Fields changed since the last scan are scanned
// first, so the .env lines written follow them.
func (s Setup) enter() (Setup, tea.Cmd) {
	if _, _, at, err := s.build(false); err != nil {
		return s.fail(at, err), nil
	}
	if s.dirty {
		s.submitting = true
		return s.rescan()
	}
	if s.got != s.seq { // a rescan is still running: save once it's in
		s.submitting = true
		return s, nil
	}
	return s.submit()
}

// fail shows err under the field at focus stop at and moves the cursor there.
func (s Setup) fail(at int, err error) Setup {
	s.errAt, s.errMsg = at, oneLine(err.Error())
	s.focusOn(at)
	return s
}

// submit saves the list, links the compose file and sets the .env lines when
// their toggles are on, and applies the list, in one change. A new port needs
// Caddy ready, as in the add form. In the dashboard, the form is done and
// hands the change over; on its own, it runs the change and quits once the
// change is done.
func (s Setup) submit() (Setup, tea.Cmd) {
	link, env := !s.off[toggleLink] && s.proposal.Compose != "", !s.off[toggleEnv] && len(s.proposal.Env) > 0
	next, owner, at, err := s.build(link)
	if err != nil {
		return s.fail(at, err), nil
	}
	if slices.ContainsFunc(next, func(d store.Domain) bool {
		old, ok := store.Find(s.domains, d.Name)
		return d.Port > 0 && (!ok || old.Port != d.Port)
	}) {
		if err := s.backend.PortsReady(); err != nil {
			s.err = err
			return s, nil
		}
	}
	cmd := s.change(next, s.embedded) // on its own, lodo dev starts the apps next: no doctor run
	if link {
		cmd = s.thenLink(cmd, owner)
	}
	if env {
		cmd = s.thenFixEnv(cmd, s.proposal.Root, s.proposal.Env)
	}
	s.done, s.saving = s.embedded, !s.embedded
	return s, cmd
}

// View draws the form and its keys. lodo dev draws it in the terminal, not on
// the alternate screen, so it stays above what the apps print.
func (s Setup) View() tea.View {
	lines := s.lines()
	if help := s.help(); help != "" {
		lines = append(lines, "", s.styles.dim.Render(help))
	}
	return tea.NewView(strings.Join(lines, "\n"))
}

// help is the keys the form takes now.
func (s Setup) help() string {
	back := " esc quit"
	if s.embedded {
		back = " esc back to the list"
	}
	switch {
	case s.done && !s.embedded:
		return ""
	case s.saving:
		return " saving…"
	case !s.found:
		return back
	case s.embedded:
		return " tab/shift+tab move  space toggle  enter save  esc cancel"
	}
	return " tab/shift+tab move  space toggle  enter save + start  esc cancel"
}

// lines draws the form without its keys, each line cut to the width: the
// title, the project, its apps and pages, the toggles, the notes, and what
// failed or what the change did.
func (s Setup) lines() []string {
	title := "lodo dev · "
	if s.embedded {
		title = "set up "
	}
	lines := []string{" " + s.styles.title.Render(title+s.shortPath(cmp.Or(s.proposal.Root, s.origin.Dir))), ""}
	switch {
	case !s.scanned:
		lines = append(lines, " "+s.styles.dim.Render("scanning…"))
	case s.found:
		lines = append(lines, s.formLines()...)
	case s.err == nil:
		lines = append(lines, " scan found no dev server or compose file in "+s.shortPath(s.proposal.Root))
	}
	for _, n := range s.proposal.Notes {
		lines = append(lines, " "+s.styles.dim.Render("note  "+n))
	}
	switch {
	case s.err != nil:
		lines = append(lines, " "+s.styles.bad.Render("✗ "+oneLine(s.err.Error())))
	case s.note != "":
		lines = append(lines, " "+s.styles.ok.Render(s.note))
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, s.width, "…")
	}
	return lines
}

// formLines draws the fields and the toggles, each error under its field.
// shortcut: no scrolling, so a form taller than the terminal loses its last
// lines; add a viewport when a project needs one.
func (s Setup) formLines() []string {
	project := s.projectName()
	lines := []string{" Project   " + s.styles.suffixed(s.name, store.Suffix) + "   address " + s.styles.suffixed(s.address, "")}
	lines = append(lines, s.errLine(11, stopName, stopAddress)...)

	whatWidth, nameWidth := 0, 0
	names := make([]string, len(s.rows))
	for i, r := range s.rows {
		suffix := "." + project
		if strings.TrimSpace(r.label.Value()) == "" {
			suffix = project
		}
		names[i] = s.styles.suffixed(r.label, suffix)
		whatWidth, nameWidth = max(whatWidth, ansi.StringWidth(r.what)), max(nameWidth, ansi.StringWidth(names[i]))
	}
	for _, page := range []bool{false, true} {
		header := " Apps"
		if page {
			header = " Services"
			if len(s.project.Compose) > 0 {
				header += "  " + s.styles.dim.Render(filepath.Base(s.project.Compose[0]))
			}
		}
		for i, r := range s.rows {
			if r.page != page {
				continue
			}
			if header != "" {
				lines = append(lines, "", header)
				header = ""
			}
			lines = append(lines, "   "+pad(r.what, whatWidth)+"  "+pad(names[i], nameWidth)+"  port "+s.styles.suffixed(r.port, ""))
			lines = append(lines, s.errLine(3+whatWidth+2, rowStop(i), rowStop(i)+1)...)
		}
	}

	if len(s.toggles()) > 0 {
		lines = append(lines, "")
	}
	for j, t := range s.toggles() {
		box := "[x]"
		if s.off[t] {
			box = "[ ]"
		}
		if s.focus == rowStop(len(s.rows))+j {
			box = s.styles.table.Selected.Render(box)
		}
		if t == toggleLink {
			rel, err := filepath.Rel(s.proposal.Root, s.proposal.Compose)
			if err != nil {
				rel = s.proposal.Compose
			}
			env := compose.EnvVar + "=" + strings.TrimSpace(s.address.Value())
			lines = append(lines, " "+box+" link "+filepath.ToSlash(rel)+"  "+s.styles.dim.Render(env))
			continue
		}
		lines = append(lines, " "+box+" set "+plural(len(s.proposal.Env), ".env line"))
		for _, f := range s.proposal.Env { // New shows a URL's user info as ***@
			lines = append(lines, "     "+s.styles.dim.Render(f.File+":"+strconv.Itoa(f.Line)+"  "+strings.TrimSpace(f.New)))
		}
	}
	if len(s.proposal.Notes) > 0 {
		lines = append(lines, "")
	}
	return lines
}

// errLine returns the line for the error under a line whose fields are the
// focus stops from and to, indented by indent; none when it has no error.
func (s Setup) errLine(indent, from, to int) []string {
	if s.errAt < from || s.errAt > to {
		return nil
	}
	return []string{strings.Repeat(" ", indent) + s.styles.bad.Render("✗ "+s.errMsg)}
}

// pad fills s with spaces to width columns.
func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(width-ansi.StringWidth(s), 0))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
