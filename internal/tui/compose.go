package tui

// The compose file of a project: the compose cell and field, linking, and the
// preview of the file rewritten for lodo.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// previewMsg is owner's compose file, rewritten for its project.
type previewMsg struct {
	owner   store.Domain
	out     []byte
	changes []compose.Change
	fixes   []scan.Fix // dev server lines that need a host
	err     error
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
		fixes := b.HostFixes(compose.ProjectDir(owner.Compose), owner.Address)
		return previewMsg{owner: owner, out: out, changes: changes, fixes: fixes, err: err}
	}
}

// composeValues are what owner's compose file gets: owner's name, the names
// Caddy serves on owner's address, by port, and which of them have HTTPS on.
func (m Model) composeValues(owner store.Domain) compose.Values {
	names, secure := map[int]string{}, map[int]bool{}
	for _, d := range m.domains {
		if d.Enabled && d.Port > 0 && d.Address == owner.Address && store.Project(d.Name) == store.Project(owner.Name) {
			names[d.Port] = d.Name
			secure[d.Port] = d.HTTPS
		}
	}
	return compose.Values{Domain: owner.Name, Names: names, Secure: secure}
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
// .env line when the file's ports bind EnvVar. The next dev lines that need
// -H come first, marked the same way.
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
	m.preview.SetContentLines(append(m.fixLines(msg.fixes), lines...))
	m.preview.GotoTop()
	m.preview.SetXOffset(0)
}

// fixLines shows each dev server fix under its file and line, then a line
// before what follows. lodo leaves those files to the user.
func (m Model) fixLines(fixes []scan.Fix) []string {
	if len(fixes) == 0 {
		return nil
	}
	lines := []string{m.styles.dim.Render("  these listen on every address; add the host yourself:")}
	for _, f := range fixes {
		lines = append(lines,
			m.styles.dim.Render("  "+f.File+":"+strconv.Itoa(f.Line)),
			m.styles.ok.Render("~ "+f.New))
	}
	return append(lines, "")
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

// find returns the listed domain called name.
func (m Model) find(name string) (store.Domain, bool) { return findIn(m.domains, name) }

// findIn returns the domain called name in domains.
func findIn(domains []store.Domain, name string) (store.Domain, bool) {
	i := slices.IndexFunc(domains, func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		return store.Domain{}, false
	}
	return domains[i], true
}

// link links d to the compose file of the project lodo started in, or, outside
// one, to its own: d's compose path is saved and d's address is written into
// the .env that file runs with.
func (m Model) link(d store.Domain) (Model, tea.Cmd) {
	path := d.Compose
	if len(m.project.Compose) > 0 {
		path = m.project.Compose[0]
	}
	if path == "" || path == store.NoCompose {
		m.err = errors.New("no compose file to link: start lodo in the project, or e to set one")
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
// project's, rewritten for lodo.
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

// previewKey scrolls the preview; l links, p or esc goes back.
func (m Model) previewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "p", "esc":
		m.mode = modeList
		return m, nil
	case "q":
		return m, tea.Quit
	case "l":
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
	env := "no port binds " + compose.EnvVar + ", so the project's .env needs nothing from lodo"
	if m.previewEnv != "" {
		env = "the ports need " + m.previewEnv + " in the project's .env: l writes it"
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
