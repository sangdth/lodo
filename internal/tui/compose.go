package tui

// The compose file of a project: the compose cell and field, linking, and the
// preview of the file rewritten for lodo.

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
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
	err     error
}

// readPreview reads owner's compose file and rewrites it for owner's project.
func (m Model) readPreview(owner store.Domain) tea.Cmd {
	b, values := m.backend, scan.ValuesFor(m.domains, owner)
	return func() tea.Msg {
		src, err := b.ReadCompose(owner.Compose)
		if err != nil {
			return previewMsg{err: err}
		}
		out, changes, err := compose.Rewrite(src, values)
		return previewMsg{owner: owner, out: out, changes: changes, err: err}
	}
}

// composeOwner returns the name whose compose file d uses: its own, or its
// nearest parent's.
func (m Model) composeOwner(d store.Domain) (store.Domain, bool) {
	for !store.HasCompose(d.Compose) {
		parent, ok := store.Parent(m.domains, d.Name)
		if !ok {
			return store.Domain{}, false
		}
		d = parent
	}
	return d, true
}

// showPreview fills the preview: changed lines marked in green, and the
// variable lodo dev sets when the file's ports bind EnvVar.
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
	m.previewEnv = ""
	if strings.Contains(string(msg.out), "${"+compose.EnvVar) {
		m.previewEnv = compose.EnvVar + "=" + msg.owner.Address
	}
	m.preview.SetContentLines(lines)
	m.preview.GotoTop()
	m.preview.SetXOffset(0)
}

// checkCompose refuses a compose path that breaks store's rule or names no
// readable file.
func (m Model) checkCompose(path string) error {
	if err := store.ValidateCompose(path); err != nil || !store.HasCompose(path) {
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

// linkScanMsg is a scan for l on the name called name: files are the compose
// files of the project lodo started in, best first.
type linkScanMsg struct {
	name  string
	files []string
}

// link links d to the compose file of the project lodo started in, which it
// scans for first, or, outside one, to its own.
func (m Model) link(d store.Domain) (Model, tea.Cmd) {
	if m.origin.Dir == "" {
		return m.linkProject(d, nil)
	}
	ctx, b, dir, domains := m.ctx, m.backend, m.origin.Dir, m.domains
	return m, func() tea.Msg {
		project, _, _ := b.Scan(ctx, dir, "", domains)
		return linkScanMsg{name: d.Name, files: project.Compose}
	}
}

// linkProject links d to the best of the project's compose files.
func (m Model) linkProject(d store.Domain, files []string) (Model, tea.Cmd) {
	if len(files) == 0 {
		m.err = errors.New("no compose file to link: start lodo ps in the project, or e to set one")
		return m, nil
	}
	return m.linkTo(d, files[0])
}

// linkTo saves path as d's compose file. Nothing is applied: it changes no
// generated file.
func (m Model) linkTo(d store.Domain, path string) (Model, tea.Cmd) {
	d.Compose = path
	if _, err := store.Update(m.domains, d.Name, d); err != nil {
		m.err = err
		return m, nil
	}
	home := m.origin.Home
	cmd := m.save(func(fresh []store.Domain) ([]store.Domain, error) {
		cur, ok := store.Find(fresh, d.Name)
		if !ok {
			return nil, fmt.Errorf("%s is not listed", d.Name)
		}
		cur.Compose = path
		return store.Update(fresh, d.Name, cur)
	})
	return m.start(d.Name, func() tea.Msg {
		msg, ok := cmd().(changedMsg)
		if ok && msg.stored {
			if linked, found := store.Find(msg.domains, d.Name); found {
				msg.note = linkNote(linked, home)
			}
		}
		return msg
	})
}

func linkNote(owner store.Domain, home string) string {
	return "linked " + owner.Name + " to " + ShortPath(owner.Compose, home) + " · lodo dev sets " + compose.EnvVar + "=" + owner.Address
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

// previewKey scrolls the preview; p or esc goes back.
func (m Model) previewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "p", "esc":
		m.mode = modeList
		return m, nil
	case "q":
		return m, tea.Quit
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
func (d deps) shortPath(path string) string {
	return ShortPath(path, d.origin.Home)
}

// previewView draws the rewritten compose file under its title and the
// variable lodo dev sets for its ports.
func (m Model) previewView() string {
	env := "no port binds " + compose.EnvVar + ", so lodo dev gives compose nothing"
	if m.previewEnv != "" {
		env = "lodo dev and lodo exec set " + m.previewEnv + " for the ports"
	}
	title := " " + m.styles.title.Render(m.previewTitle)
	return title + "\n " + ansi.Truncate(m.styles.dim.Render(env), m.innerWidth()-1, "…") + "\n" + m.preview.View()
}

// ShortPath writes path from the home folder as ~/…; other values stay.
func ShortPath(path, home string) string {
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
