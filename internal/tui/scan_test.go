package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// inNewFlowy is a backend for an lodo started in flowy, a Next app on port
// 3000 with a compose file.
func inNewFlowy() *fakeBackend {
	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: ".", Tool: "next", Port: 3000}, {Dir: "apps/api"}}
	return b
}

// unlisted is sample without flowy.test.
var unlisted = slices.DeleteFunc(slices.Clone(sample), func(d store.Domain) bool { return d.Name == "flowy.test" })

func TestScan_ReviewsANewProject(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	m := readyIn(b, unlisted, flowyOrigin)
	if m.mode != modeScan {
		t.Fatalf("mode = %v after the first report, want the review", m.mode)
	}
	golden.RequireEqual(t, m.View().Content)

	m = send(m, "y")
	if m.mode != modeList || m.err != nil {
		t.Fatalf("mode %v, err %v after y; want the list", m.mode, m.err)
	}
	want := []store.Domain{
		{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Compose: flowyDev, Root: flowyRoot},
		{Name: "api.flowy.test", Address: "127.0.1.2", Enabled: true},
	}
	for _, d := range want {
		if len(b.saved) != 1 || !slices.Contains(b.saved[0], d) {
			t.Errorf("saved %v, want it to hold %+v", b.saved, d)
		}
	}
	if len(b.applied) != 1 {
		t.Errorf("applied %d times, want once for every name", len(b.applied))
	}
	if want := []string{flowyDev + " 127.0.1.2"}; !slices.Equal(b.linked, want) {
		t.Errorf("linked %q, want %q", b.linked, want)
	}
	if got := m.cursorName(); got != "flowy.test" {
		t.Errorf("cursor on %q, want the project's name", got)
	}
}

func TestScan_NoGoesBack(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"n", "esc"} {
		b := inNewFlowy()
		m := send(readyIn(b, unlisted, flowyOrigin), k)
		if m.mode != modeList || len(b.saved) != 0 || len(b.linked) != 0 {
			t.Errorf("%s: mode %v, saved %v, linked %v; want the list and no change", k, m.mode, b.saved, b.linked)
		}
	}
}

func TestScan_ListedProjectGetsANote(t *testing.T) {
	t.Parallel()

	b := inFlowy() // flowy.test is listed without a compose file
	m := readyIn(b, sample, flowyOrigin)
	if m.mode != modeList {
		t.Fatalf("mode = %v, want the list: a listed project never blocks it", m.mode)
	}
	if want := "scan: a compose file to link · i reviews"; m.note != want {
		t.Errorf("note = %q, want %q", m.note, want)
	}
	m = send(m, "i")
	if m.mode != modeScan {
		t.Fatalf("mode = %v after i, want the review", m.mode)
	}
	m = send(m, "y")
	if got := composeOf(t, b, "flowy.test"); got != flowyDev {
		t.Errorf("flowy.test's compose = %q, want %q", got, flowyDev)
	}
	if len(b.applied) != 0 {
		t.Errorf("applied %v; only a compose path changed", b.applied)
	}
}

func TestScan_NotOffered(t *testing.T) {
	t.Parallel()

	linked := slices.Clone(sample)
	linked[2].Compose = flowyProd
	tests := []struct {
		name    string
		backend *fakeBackend
		domains []store.Domain
		origin  Start
	}{
		{name: "set up", backend: inFlowy(), domains: linked, origin: flowyOrigin},
		{name: "nothing to serve", backend: &fakeBackend{project: scan.Project{Root: flowyRoot, Name: "flowy.test"}}, domains: unlisted, origin: flowyOrigin},
		{name: "not in a project", backend: &fakeBackend{}, domains: unlisted, origin: flowyOrigin},
		{name: "no folder to look in", backend: inNewFlowy(), domains: unlisted, origin: Start{Home: home}},
		{
			name:    "a folder name that makes no label",
			backend: &fakeBackend{project: scan.Project{Root: "/tmp/___", Compose: []string{"/tmp/___/compose.yml"}}},
			domains: unlisted, origin: Start{Dir: "/tmp/___"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if m := readyIn(tt.backend, tt.domains, tt.origin); m.mode != modeList || m.note != "" {
				t.Errorf("mode %v, note %q; want the list and no note", m.mode, m.note)
			}
		})
	}
}

func TestScan_WaitsForTheFirstCheck(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	m := New(t.Context(), b, unlisted, flowyOrigin)
	project, proposal, ok := b.Scan("", "", unlisted)
	next, _ := m.Update(scanMsg{project: project, proposal: proposal, ok: ok})
	m = next.(Model)
	if m.mode != modeList {
		t.Fatalf("mode = %v while the first check runs, want the list", m.mode)
	}
	next, _ = m.Update(reportMsg{checks: []check.Check{{ID: 1, OK: true}}})
	if m = next.(Model); m.mode != modeScan {
		t.Errorf("mode = %v once the check is done, want the review", m.mode)
	}
}

func TestScan_I(t *testing.T) {
	t.Parallel()

	linked := slices.Clone(sample)
	linked[2].Compose = flowyProd
	tests := []struct {
		name     string
		backend  *fakeBackend
		domains  []store.Domain
		wantErr  string
		wantNote string
	}{
		{name: "set up", backend: inFlowy(), domains: linked, wantNote: "flowy.test is set up: nothing to add, link or change"},
		{
			name: "not in a project", backend: &fakeBackend{}, domains: linked,
			wantErr: "not in a project: lodo scans the git root, or a folder with a lock file, it starts in",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := send(readyIn(tt.backend, tt.domains, flowyOrigin), "i")
			gotErr := ""
			if m.err != nil {
				gotErr = m.err.Error()
			}
			if m.mode != modeList || gotErr != tt.wantErr || m.note != tt.wantNote {
				t.Errorf("mode %v, err %q, note %q; want the list, %q, %q", m.mode, gotErr, m.note, tt.wantErr, tt.wantNote)
			}
		})
	}
}

func TestScan_PortNeedsCaddy(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	b.portsErr = errors.New("caddy is not installed: brew install caddy, then lodo setup")
	m := send(readyIn(b, unlisted, flowyOrigin), "y")
	if m.mode != modeScan || len(b.saved) != 0 {
		t.Errorf("mode %v, saved %v; want the review kept and nothing saved", m.mode, b.saved)
	}
	if got := ansi.Strip(m.statusLine()); !strings.Contains(got, "brew install caddy") {
		t.Errorf("status line = %q, want why", got)
	}
}

func TestScan_Rename(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	m := send(readyIn(b, unlisted, flowyOrigin), "e")
	if !m.renaming || m.rename.Value() != "flowy" {
		t.Fatalf("renaming %v with %q, want the name field holding flowy", m.renaming, m.rename.Value())
	}
	for range len("flowy") {
		m = send(m, "backspace")
	}
	for _, k := range []string{"w", "e", "b", ".", "x"} {
		m = send(m, k)
	}
	m = send(m, "enter")
	if !m.renaming || m.renameErr != "a project's name has one label before .test" {
		t.Fatalf("renaming %v, error %q; want the field kept with why", m.renaming, m.renameErr)
	}
	golden.RequireEqual(t, m.View().Content)

	m = send(send(send(m, "backspace"), "backspace"), "enter") // web
	if m.renaming || m.mode != modeScan || m.proposal.Name != "web.test" {
		t.Fatalf("renaming %v, mode %v, name %q; want the review again for web.test", m.renaming, m.mode, m.proposal.Name)
	}
	if got := m.proposal.Add[1].Name; got != "api.web.test" {
		t.Errorf("subdomain = %q, want it to follow the name", got)
	}
	m = send(m, "y")
	want := store.Domain{Name: "web.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Compose: flowyDev, Root: flowyRoot}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Errorf("saved %v, want it to hold %+v", b.saved, want)
	}
	if m.scanName != "" {
		t.Errorf("scanName = %q after y, want it cleared: the root keeps the name now", m.scanName)
	}

	// The next start finds web.test by its folder, and offers nothing.
	if m := readyIn(inNewFlowy(), b.saved[0], flowyOrigin); m.mode != modeList || m.note != "" {
		t.Errorf("next start: mode %v, note %q; want the list and no offer", m.mode, m.note)
	}
}

func TestScan_RenameEscKeepsTheName(t *testing.T) {
	t.Parallel()

	m := send(send(send(readyIn(inNewFlowy(), unlisted, flowyOrigin), "e"), "x"), "esc")
	if m.renaming || m.mode != modeScan || m.proposal.Name != "flowy.test" {
		t.Errorf("renaming %v, mode %v, name %q; want the review for flowy.test", m.renaming, m.mode, m.proposal.Name)
	}
}

func TestScan_RenameToAListedName(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	b.project.Compose = nil
	m := send(readyIn(b, unlisted, flowyOrigin), "e")
	for range len("flowy") {
		m = send(m, "backspace")
	}
	m = send(send(send(send(m, "c"), "r"), "m"), "enter") // crm.test, listed without a folder
	if !m.proposal.Claim || !strings.Contains(ansi.Strip(m.View().Content), "this folder to save as crm.test's") {
		t.Fatalf("claim %v; want the review to offer crm.test the folder:\n%s", m.proposal.Claim, ansi.Strip(m.View().Content))
	}
	send(m, "y")
	if got := rootOf(t, b, "crm.test"); got != flowyRoot {
		t.Errorf("crm.test's root = %q, want %q", got, flowyRoot)
	}
}

func rootOf(t *testing.T, b *fakeBackend, name string) string {
	t.Helper()
	if len(b.saved) == 0 {
		t.Fatalf("nothing saved")
	}
	d, ok := findIn(b.saved[len(b.saved)-1], name)
	if !ok {
		t.Fatalf("%s not saved", name)
	}
	return d.Root
}
