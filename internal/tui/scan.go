package tui

// The scan of the project lodo started in: the review it opens at startup,
// the note when the project is listed already, and i, which scans again.

import (
	"errors"
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

// scanMsg is a scan of the project lodo started in; ok is false outside one.
// review is set when i asked for it, so the review opens whatever it holds.
type scanMsg struct {
	project  scan.Project
	proposal scan.Proposal
	ok       bool
	review   bool
}

// runScan scans the project lodo started in against the list, under the
// name the user chose, if any.
func (m Model) runScan(review bool) tea.Cmd {
	b, dir, name, domains := m.backend, m.origin.Dir, m.scanName, m.domains
	return func() tea.Msg {
		project, proposal, ok := b.Scan(dir, name, domains)
		return scanMsg{project: project, proposal: proposal, ok: ok, review: review}
	}
}

// setScan stores a scan and, when i asked for it, opens the review.
func (m *Model) setScan(msg scanMsg) {
	m.project, m.proposal, m.scanned = msg.project, msg.proposal, true
	switch {
	case !msg.review:
		m.maybeOffer()
	case !msg.ok:
		m.err = errors.New("not in a project: lodo scans the git root, or a folder with a lock file, it starts in")
	case m.proposal.Empty() && !m.proposal.Listed:
		m.note = "scan found no dev server or compose file in " + m.shortPath(m.proposal.Root)
	case m.proposal.Empty():
		m.note = m.proposal.Name + " is set up: nothing to add, link or change"
	default:
		m.openReview()
	}
}

// maybeOffer runs once, after the first check and the first scan. It opens
// the review when the project has no listed name yet, and otherwise only
// says what the scan found, so a listed project never blocks the list.
func (m *Model) maybeOffer() {
	if m.offered || !m.scanned || m.busy || m.mode != modeList {
		return
	}
	m.offered = true
	switch {
	case m.proposal.Empty():
	case !m.proposal.Listed:
		m.openReview()
	default:
		m.note = "scan: " + m.summary() + " · i reviews"
	}
}

// summary counts what the proposal holds, such as 2 names to add, 1 line to
// change.
func (m Model) summary() string {
	var parts []string
	if n := len(m.proposal.Add); n > 0 {
		parts = append(parts, plural(n, "name")+" to add")
	}
	if m.proposal.Compose != "" {
		parts = append(parts, "a compose file to link")
	}
	if m.proposal.Claim {
		parts = append(parts, "this folder to save as "+m.proposal.Name+"'s")
	}
	if n := len(m.proposal.Fixes); n > 0 {
		parts = append(parts, plural(n, "line")+" to change")
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// openReview shows the proposal: the names to add, the compose file to link,
// the lines to change by hand and what lodo couldn't decide.
func (m *Model) openReview() {
	p := m.proposal
	nameWidth := 0
	for _, d := range p.Add {
		nameWidth = max(nameWidth, len(d.Name))
	}
	var lines []string
	for _, d := range p.Add {
		line := "  add   " + d.Name + strings.Repeat(" ", nameWidth-len(d.Name)) + "  " + d.Address
		if d.Port > 0 {
			line += "  :" + strconv.Itoa(d.Port)
		}
		lines = append(lines, m.styles.ok.Render(line))
	}
	if p.Compose != "" {
		rel, err := filepath.Rel(p.Root, p.Compose)
		if err != nil {
			rel = p.Compose
		}
		lines = append(lines, m.styles.ok.Render("  link  ./"+filepath.ToSlash(rel)+" · "+
			compose.EnvVar+"="+p.Address+" in its .env"))
	}
	if len(p.Fixes) > 0 {
		lines = append(append(lines, ""), m.fixLines(p.Fixes)...)
	}
	for _, n := range p.Notes {
		lines = append(lines, m.styles.dim.Render("  note  "+n))
	}
	m.mode = modeScan
	m.previewTitle = "scan of " + m.shortPath(p.Root)
	m.preview.SetContentLines(lines)
	m.preview.GotoTop()
	m.preview.SetXOffset(0)
}

// scanView draws the review under its title and what y does.
func (m Model) scanView() string {
	title := " " + m.styles.title.Render(m.previewTitle)
	if m.renaming {
		line := " name  " + m.suffixed(m.rename, tldSuffix) + "  "
		if m.renameErr != "" {
			line += m.styles.bad.Render("✗ " + m.renameErr)
		} else {
			line += m.styles.dim.Render("subdomains follow it")
		}
		return title + "\n" + ansi.Truncate(line, m.innerWidth(), "…") + "\n" + m.preview.View()
	}
	what := "y adds and links nothing: the lines below are yours to change"
	if m.canAccept() {
		what = "y: " + m.summary()
		if len(m.proposal.Fixes) > 0 {
			what = "y adds and links; the lines below are yours to change"
		}
	}
	return title + "\n " + ansi.Truncate(m.styles.dim.Render(what), m.innerWidth()-1, "…") + "\n" + m.preview.View()
}

// canAccept reports whether y has something to add, link or save.
func (m Model) canAccept() bool {
	return len(m.proposal.Add) > 0 || m.proposal.Compose != "" || m.proposal.Claim
}

// scanKey answers the review: y or enter adds every name and links the
// compose file in one change, e edits the project's name, n or esc goes back
// without a change; the arrows scroll.
func (m Model) scanKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.renaming {
		return m.renameKey(msg)
	}
	switch msg.String() {
	case "y", "enter":
		return m.acceptScan()
	case "e":
		m.renaming, m.renameErr = true, ""
		m.rename = newInputs(tldSuffix)[fieldName]
		m.rename.Placeholder = "flowy"
		m.rename.SetValue(strings.TrimSuffix(m.proposal.Name, tldSuffix))
		m.rename.CursorEnd()
		return m, m.rename.Focus()
	case "n", "esc":
		m.mode = modeList
		return m, nil
	case "q":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.preview, cmd = m.preview.Update(msg)
	return m, cmd
}

// renameKey types the project's name. enter scans again under it, so the
// subdomains, address and fixes follow; esc keeps the old name.
func (m Model) renameKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.renaming = false
		return m, nil
	case "enter":
		name := strings.TrimSuffix(strings.TrimSpace(m.rename.Value()), tldSuffix) + tldSuffix
		if err := scan.ValidName(name); err != nil {
			m.renameErr = err.Error()
			return m, nil
		}
		m.renaming, m.scanName = false, name
		return m, m.runScan(true)
	}
	var cmd tea.Cmd
	m.rename, cmd = m.rename.Update(msg)
	m.renameErr = ""
	return m, cmd
}

// acceptScan adds the proposal's names, links its compose file to the
// project's name and applies the list once. A port needs Caddy ready, as in
// the form.
func (m Model) acceptScan() (tea.Model, tea.Cmd) {
	p := m.proposal
	if !m.canAccept() {
		m.mode = modeList
		return m, nil
	}
	if m.busy {
		m.err = errors.New("a change is still running: press y again in a moment")
		return m, nil
	}
	if slices.ContainsFunc(p.Add, func(d store.Domain) bool { return d.Port > 0 }) {
		if err := m.backend.PortsReady(); err != nil {
			m.err = err
			return m, nil
		}
	}
	next := m.domains
	for _, d := range p.Add {
		var err error
		if next, err = store.Add(next, d); err != nil {
			m.err = err
			return m, nil
		}
	}
	var owner store.Domain
	if p.Compose != "" || p.Claim {
		var err error
		owner, _ = findIn(next, p.Name)
		if p.Compose != "" {
			owner.Compose = p.Compose
		}
		if p.Claim {
			owner.Root = p.Root
		}
		if next, err = store.Update(next, p.Name, owner); err != nil {
			m.err = err
			return m, nil
		}
	}
	cmd := m.change(next)
	if len(p.Add) == 0 {
		cmd = m.save(next) // only the compose path or the folder changed, which nothing applies
	}
	if p.Compose != "" {
		cmd = m.thenLink(cmd, owner)
	}
	m.mode, m.proposal, m.selectName, m.scanName = modeList, scan.Proposal{}, p.Name, ""
	return m.start(p.Name, cmd)
}
