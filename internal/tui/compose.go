package tui

// The compose file of a project: the question at startup, the compose cell
// and field, and the preview of the file rewritten for oo.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/oo/internal/compose"
	"github.com/sangdth/oo/internal/store"
)

// question is the compose file oo offers at startup for a project's name.
type question struct {
	name   string // the name the project's folder suggests, such as flowy.oo
	path   string // the compose file found, absolute
	rel    string // the same file from the project's root, for the prompt
	listed bool   // name is in the list; otherwise yes adds it
}

// previewMsg is owner's compose file, rewritten for its project.
type previewMsg struct {
	owner   store.Domain
	out     []byte
	changes []compose.Change
	err     error
}

// projectMsg is the project oo started in: its root and its compose files,
// best first. files is empty outside a project.
type projectMsg struct {
	root  string
	files []string
}

// readPreview reads owner's compose file and rewrites it for owner's project.
func (m Model) readPreview(owner store.Domain) tea.Cmd {
	b, values := m.backend, m.composeValues(owner)
	return func() tea.Msg {
		src, err := b.ReadCompose(owner.Compose)
		if err != nil {
			return previewMsg{err: err}
		}
		out, changes, err := compose.Rewrite(src, values)
		return previewMsg{owner: owner, out: out, changes: changes, err: err}
	}
}

// composeValues are what owner's compose file gets: owner's name, and the
// names Caddy serves on owner's address, by port.
func (m Model) composeValues(owner store.Domain) compose.Values {
	names := map[int]string{}
	for _, d := range m.domains {
		if d.Enabled && d.Port > 0 && d.Address == owner.Address && store.Project(d.Name) == store.Project(owner.Name) {
			names[d.Port] = d.Name
		}
	}
	return compose.Values{Domain: owner.Name, Names: names}
}

// composeOwner returns the name whose compose file d uses: its own, or its
// nearest parent's.
func (m Model) composeOwner(d store.Domain) (store.Domain, bool) {
	for d.Compose == "" || d.Compose == store.NoCompose {
		parent, ok := store.Parent(m.domains, d.Name)
		if !ok {
			return store.Domain{}, false
		}
		d = parent
	}
	return d, true
}

// showPreview fills the preview: changed lines marked in green, and the
// .env line when the file's ports bind EnvVar.
func (m *Model) showPreview(msg previewMsg) {
	changed := make(map[int]bool, len(msg.changes))
	for _, c := range msg.changes {
		changed[c.Line] = true
	}
	lines := strings.Split(strings.TrimRight(string(msg.out), "\n"), "\n")
	for i, line := range lines {
		if changed[i+1] {
			lines[i] = m.styles.ok.Render("~ " + line)
		} else {
			lines[i] = "  " + line
		}
	}
	count := strconv.Itoa(len(msg.changes)) + " changes"
	switch len(msg.changes) {
	case 0:
		count = "no changes"
	case 1:
		count = "1 change"
	}
	m.mode = modePreview
	m.previewTitle = filepath.Base(msg.owner.Compose) + " for " + msg.owner.Name + " · " + count
	m.previewOwner, m.previewEnv = msg.owner.Name, ""
	if strings.Contains(string(msg.out), "${"+compose.EnvVar) {
		m.previewEnv = compose.EnvVar + "=" + msg.owner.Address
	}
	m.preview.SetContentLines(lines)
	m.preview.GotoTop()
	m.preview.SetXOffset(0)
}

func (m Model) findProject() tea.Cmd {
	b, dir := m.backend, m.origin.Dir
	return func() tea.Msg {
		root, files := b.Project(dir)
		return projectMsg{root: root, files: files}
	}
}

// checkCompose refuses a compose path that breaks store's rule or names no
// readable file.
func (m Model) checkCompose(path string) error {
	if err := store.ValidateCompose(path); err != nil || path == "" || path == store.NoCompose {
		return err
	}
	if _, err := m.backend.ReadCompose(path); err != nil {
		msg := "can't read it: " + oneLine(err.Error())
		if errors.Is(err, fs.ErrNotExist) {
			msg = "no file at " + m.shortPath(path)
		}
		return &store.FieldError{Field: store.FieldCompose, Msg: msg}
	}
	return nil
}

// maybeAsk opens the compose question once the first check is done, when oo
// started in a project that no listed name links yet and whose folder's name
// has no answer. It asks once.
func (m *Model) maybeAsk() {
	if m.asked || m.busy || m.mode != modeList || len(m.project.files) == 0 {
		return
	}
	m.asked = true
	root, path := m.project.root, m.project.files[0]
	name := projectName(root)
	if name == "" || m.projectLinked(root) {
		return
	}
	d, listed := m.find(name)
	if listed && d.Compose != "" {
		return
	}
	rel := path
	if r, err := filepath.Rel(root, path); err == nil {
		rel = "./" + r
	}
	m.mode, m.question = modeAsk, question{name: name, path: path, rel: rel, listed: listed}
	m.point(name)
}

// projectLinked reports whether a listed name, whatever it is called, links a
// compose file inside the project at root: oo asks about the project, and the
// folder's name only suggests a name for it.
func (m Model) projectLinked(root string) bool {
	return slices.ContainsFunc(m.domains, func(d store.Domain) bool {
		return linked(d) && strings.HasPrefix(d.Compose, root+string(filepath.Separator))
	})
}

// point puts the cursor on name's row, or on the add row when name isn't
// listed, so the row a question is about is the one highlighted.
func (m *Model) point(name string) {
	i := slices.IndexFunc(m.listed(), func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		i = len(m.listed())
	}
	m.table.SetCursor(i)
	m.table.SetRows(m.rows()) // the add row looks different under the cursor
}

// nonLabel matches what a folder's name holds that a DNS label can't.
var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// projectName is the name a project's folder suggests: flowy.oo for
// ~/Projects/flowy. It is empty when the folder's name makes no valid label.
func projectName(root string) string {
	label := strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-"), "-")
	name := label + tldSuffix
	if store.ValidateName(name) != nil {
		return ""
	}
	return name
}

// find returns the listed domain called name.
func (m Model) find(name string) (store.Domain, bool) {
	i := slices.IndexFunc(m.domains, func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		return store.Domain{}, false
	}
	return m.domains[i], true
}

// askKey answers the compose question, which defaults to yes. For a listed
// name, y or enter links the file, e opens the edit form with it, and n or esc
// saves no, so oo stops asking. For a name that isn't listed, y, enter or e
// opens the add form with both filled in. Any other key waits: a stray key
// must not write the project's .env.
func (m Model) askKey(k string) (tea.Model, tea.Cmd) {
	yes := k == "y" || k == "Y" || k == "enter"
	no := k == "n" || k == "N" || k == "esc"
	if !yes && !no && k != "e" {
		return m, nil
	}
	q := m.question
	m.mode, m.question = modeList, question{}
	d, listed := m.find(q.name)
	switch {
	case !listed && !no:
		m = m.openAdd("")
		m.form.inputs[fieldName].SetValue(strings.TrimSuffix(q.name, tldSuffix))
		m.form.prefill(m.domains)
		m.form.inputs[fieldCompose].SetValue(m.shortPath(q.path))
		return m, nil
	case !listed:
		return m, nil
	case k == "e":
		m = m.openEdit(d)
		m.form.inputs[fieldCompose].SetValue(m.shortPath(q.path))
		m.form.focusField(fieldCompose)
		return m, nil
	}
	if yes {
		return m.linkTo(d, q.path)
	}
	d.Compose = store.NoCompose
	next, err := store.Update(m.domains, d.Name, d)
	if err != nil {
		m.err = err
		return m, nil
	}
	return m.start(d.Name, m.save(next))
}

// link links d to the compose file of the project oo started in, or, outside
// one, to its own: d's compose path is saved and d's address is written into
// the .env that file runs with.
func (m Model) link(d store.Domain) (Model, tea.Cmd) {
	path := d.Compose
	if len(m.project.files) > 0 {
		path = m.project.files[0]
	}
	if path == "" || path == store.NoCompose {
		m.err = errors.New("no compose file to link: start oo in the project, or e to set one")
		return m, nil
	}
	return m.linkTo(d, path)
}

// linkTo saves path as d's compose file and writes d's address into the .env
// that file runs with. Nothing is applied: neither changes a generated file.
func (m Model) linkTo(d store.Domain, path string) (Model, tea.Cmd) {
	d.Compose = path
	next, err := store.Update(m.domains, d.Name, d)
	if err != nil {
		m.err = err
		return m, nil
	}
	return m.start(d.Name, m.thenLink(m.save(next), d))
}

// linked reports whether d has a compose file to write a .env for.
func linked(d store.Domain) bool {
	return d.Compose != "" && d.Compose != store.NoCompose
}

// openPreview shows the selected name's compose file, its own or its
// project's, rewritten for oo.
func (m Model) openPreview() (tea.Model, tea.Cmd) {
	d, ok := m.selected()
	if !ok {
		return m, nil
	}
	owner, ok := m.composeOwner(d)
	if !ok {
		m.err = errors.New(d.Name + " has no compose file: e sets one")
		return m, nil
	}
	return m, m.readPreview(owner)
}

// previewKey scrolls the preview; c writes the .env line, p or esc goes back.
func (m Model) previewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "p", "esc":
		m.mode = modeList
		return m, nil
	case "q":
		return m, tea.Quit
	case "c":
		owner, ok := m.find(m.previewOwner)
		if m.busy || !ok || !linked(owner) {
			return m, nil
		}
		return m.linkTo(owner, owner.Compose)
	}
	var cmd tea.Cmd
	m.preview, cmd = m.preview.Update(msg)
	return m, cmd
}

// composeCell shows d's compose file from the home folder, cut from the left
// so the file's name stays, or – after a no.
func (m Model) composeCell(d store.Domain, width int) string {
	switch d.Compose {
	case "":
		return ""
	case store.NoCompose:
		return "–"
	}
	path := []rune(m.shortPath(d.Compose))
	if len(path) <= width {
		return string(path)
	}
	return "…" + string(path[len(path)-width+1:])
}

// shortPath writes path from the home folder as ~/….
func (m Model) shortPath(path string) string {
	return shortPath(path, m.origin.Home)
}

// previewView draws the rewritten compose file under its title and the .env
// line its ports need.
func (m Model) previewView() string {
	env := "no port binds " + compose.EnvVar + ", so the project's .env needs nothing from oo"
	if m.previewEnv != "" {
		env = "the ports need " + m.previewEnv + " in the project's .env: c writes it"
	}
	title := " " + m.styles.title.Render(m.previewTitle)
	return title + "\n " + ansi.Truncate(m.styles.dim.Render(env), m.innerWidth()-1, "…") + "\n" + m.preview.View()
}

// shortPath writes path from the home folder as ~/…; other values stay.
func shortPath(path, home string) string {
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
