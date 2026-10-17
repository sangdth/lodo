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
	project, proposal, ok := b.Scan(t.Context(), "", "", nil, unlisted)
	next, _ := m.Update(scanMsg{project: project, proposal: proposal, ok: ok})
	m = next.(Model)
	if m.mode != modeList {
		t.Fatalf("mode = %v while the first check runs, want the list", m.mode)
	}
	next, _ = m.Update(reportMsg{checks: []check.Check{{ID: check.IDDnsmasq, OK: true}}})
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
	d, ok := store.Find(b.saved[len(b.saved)-1], name)
	if !ok {
		t.Fatalf("%s not saved", name)
	}
	return d.Root
}

// inFlowyWithEnv is inNewFlowy with two lines in apps/api/.env the scan
// sets: Postgres on a port the compose file binds to DOCKER_HOST_IP, with a
// password, and a URL on the root app's port.
func inFlowyWithEnv() *fakeBackend {
	b := inNewFlowy()
	b.project.Env = []scan.EnvLine{
		{File: "apps/api/.env", Line: 1, Key: "DATABASE_URL", Text: "DATABASE_URL=postgres://u:hunter2@localhost:5432/flowy"},
		{File: "apps/api/.env", Line: 2, Key: "WEB_URL", Text: "WEB_URL=http://localhost:3000"},
	}
	b.project.Bound, b.project.ComposeAddress = map[int]bool{5432: true}, "127.0.1.2"
	return b
}

func TestScan_SetsEnvLines(t *testing.T) {
	t.Parallel()

	b := inFlowyWithEnv()
	m := readyIn(b, unlisted, flowyOrigin)
	if got := ansi.Strip(m.View().Content); !strings.Contains(got, "set   apps/api/.env:2  WEB_URL=http://flowy.test") {
		t.Fatalf("review lacks the .env line:\n%s", got)
	}
	m = send(m, "y")
	if want := []string{flowyRoot + " apps/api/.env:1", flowyRoot + " apps/api/.env:2"}; !slices.Equal(b.envFixed, want) {
		t.Errorf("set %q, want %q", b.envFixed, want)
	}
	if !strings.HasPrefix(m.note, "set 2 .env lines, restart the dev servers · linked flowy.test") {
		t.Errorf("note = %q, want the .env lines first", m.note)
	}
}

func TestScan_ReviewWithEnv(t *testing.T) {
	t.Parallel()

	m := readyIn(inFlowyWithEnv(), unlisted, flowyOrigin)
	got := m.View().Content
	if !strings.Contains(ansi.Strip(got), "DATABASE_URL=postgres://***@flowy.test:5432/flowy") {
		t.Errorf("review lacks the masked URL:\n%s", ansi.Strip(got))
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("review shows the password:\n%s", ansi.Strip(got))
	}
	golden.RequireEqual(t, got)
}

func TestScan_EnvError(t *testing.T) {
	t.Parallel()

	b := inFlowyWithEnv()
	b.envErr = errors.New("apps/api/.env:2 changed since the scan: i scans again")
	m := send(readyIn(b, unlisted, flowyOrigin), "y")
	if got := ansi.Strip(m.statusLine()); !strings.Contains(got, "changed since the scan") {
		t.Errorf("status line = %q, want the .env error", got)
	}
	if len(b.saved) != 1 || len(b.linked) != 1 {
		t.Errorf("saved %v, linked %v; want the list saved and linked anyway", b.saved, b.linked)
	}
}

func TestScan_OnlyEnv(t *testing.T) {
	t.Parallel()

	b := inFlowy()
	b.project.Env = []scan.EnvLine{{File: "apps/api/.env", Line: 1, Key: "DATABASE_URL", Text: "DATABASE_URL=postgres://localhost:5432/flowy"}}
	b.project.Bound, b.project.ComposeAddress = map[int]bool{5432: true}, "127.0.1.3"
	linked := slices.Clone(sample)
	linked[2].Compose = flowyDev
	m := readyIn(b, linked, flowyOrigin)
	if want := "scan: 1 .env line to set · i reviews"; m.note != want {
		t.Errorf("note = %q, want %q", m.note, want)
	}
	m = send(m, "i")
	if m.mode != modeScan || !m.proposal.CanAccept() {
		t.Fatalf("mode %v, CanAccept %v; want a review y accepts", m.mode, m.proposal.CanAccept())
	}
	m = send(m, "y")
	if want := []string{flowyRoot + " apps/api/.env:1"}; !slices.Equal(b.envFixed, want) {
		t.Errorf("set %q, want %q", b.envFixed, want)
	}
	if want := "set 1 .env line, restart the dev servers"; m.note != want || m.mode != modeList {
		t.Errorf("mode %v, note %q; want the list and %q", m.mode, m.note, want)
	}
	if len(b.applied) != 0 {
		t.Errorf("applied %v; the list didn't change", b.applied)
	}
}

// inFlowyApps is a backend for flowy as a monorepo: a Next app in apps/web
// on 3000 and a NestJS app in apps/api on 3300, none at the root.
func inFlowyApps() *fakeBackend {
	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: "apps/web", Tool: "next", Port: 3000}, {Dir: "apps/api", Tool: "nest", Port: 3300}}
	return b
}

// retype empties the name field and types s.
func retype(m Model, s string) Model {
	for m.rename.Position() > 0 {
		m = send(m, "backspace")
	}
	return typeText(m, s)
}

func TestScan_NameApp(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := send(send(readyIn(b, unlisted, flowyOrigin), "e"), "down")
	if m.rename.Value() != "web.flowy" {
		t.Fatalf("name field holds %q after down, want web.flowy", m.rename.Value())
	}
	golden.RequireEqual(t, m.View().Content)
	if m = send(m, "up"); m.rename.Value() != "flowy" {
		t.Errorf("name field holds %q after up, want the project's flowy", m.rename.Value())
	}

	m = send(retype(send(m, "down"), "flowy"), "enter")
	if m.renaming || m.mode != modeScan {
		t.Fatalf("renaming %v, mode %v; want the review again", m.renaming, m.mode)
	}
	want := store.Domain{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: flowyRoot}
	if got := m.proposal.Add; len(got) == 0 || got[0] != want {
		t.Errorf("add = %+v, want %+v first", got, want)
	}
	if slices.ContainsFunc(m.proposal.Add, func(d store.Domain) bool { return d.Name == "web.flowy.test" }) {
		t.Errorf("add = %+v, want no web.flowy.test: web has flowy.test", m.proposal.Add)
	}
	m = send(m, "y")
	want.Compose = flowyDev
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Errorf("saved %v, want it to hold %+v", b.saved, want)
	}
	if m.scanLabels != nil {
		t.Errorf("labels = %v after y, want them cleared", m.scanLabels)
	}
}

func TestScan_NameAppErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend *fakeBackend
		domains []store.Domain
		typed   string
		want    string
	}{
		{
			name: "an app whose port lodo can't tell", backend: inNewFlowy(), domains: unlisted, typed: "server.flowy",
			want: "lodo can't tell its port, so the name wouldn't stick: set PORT in its .env or a port in its dev script",
		},
		{name: "two labels", backend: inFlowyApps(), domains: unlisted, typed: "x.y.flowy", want: "an app's name is <label>.flowy.test or flowy.test"},
		{name: "another project", backend: inFlowyApps(), domains: unlisted, typed: "api.other", want: "an app's name is <label>.flowy.test or flowy.test"},
		{name: "an empty label", backend: inFlowyApps(), domains: unlisted, typed: ".flowy", want: "an app's name is <label>.flowy.test or flowy.test"},
		{name: "the root's app has the bare name", backend: inNewFlowy(), domains: unlisted, typed: "flowy", want: "the root's app has flowy.test"},
		{
			name: "the bare name is listed", backend: inFlowyApps(), domains: sample, typed: "flowy",
			want: "flowy.test is listed: give it the app's port with e in the list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := readyIn(tt.backend, tt.domains, flowyOrigin)
			if m.mode != modeScan {
				m = send(m, "i") // a listed project only gets a note
			}
			m = send(retype(send(send(m, "e"), "down"), tt.typed), "enter")
			if !m.renaming || m.renameErr != tt.want || m.scanLabels != nil {
				t.Errorf("renaming %v, error %q, labels %v; want the field kept with %q", m.renaming, m.renameErr, m.scanLabels, tt.want)
			}
		})
	}
}

// TestScan_RescanClosesNameField checks that a scan landing while the name
// field is open closes it: the field indexed the old proposal's apps, which
// the new one may not have.
func TestScan_RescanClosesNameField(t *testing.T) {
	t.Parallel()

	m := readyIn(inFlowyApps(), unlisted, flowyOrigin)
	next, cmd := retype(send(m, "e"), "shop").Update(press("enter")) // the scan under shop.test runs; the old review stays
	m = send(send(next.(Model), "e"), "down")
	if !m.renaming || m.renameAt != 1 {
		t.Fatalf("renaming %v at %d; want an app in the field", m.renaming, m.renameAt)
	}
	m = settle(m, cmd)
	if m.renaming || m.renameAt != 0 {
		t.Errorf("renaming %v at %d after a scan landed; want the field closed", m.renaming, m.renameAt)
	}
	_ = m.View()
}

func TestScan_EscClearsName(t *testing.T) {
	t.Parallel()

	m := readyIn(inFlowyApps(), unlisted, flowyOrigin)
	m = send(retype(send(m, "e"), "shop"), "enter")
	m = send(retype(send(send(m, "e"), "down"), "site.shop"), "enter")
	if m.proposal.Name != "shop.test" || !slices.ContainsFunc(m.proposal.Add, func(d store.Domain) bool { return d.Name == "site.shop.test" }) {
		t.Fatalf("name %q, add %+v; want shop.test with site.shop.test", m.proposal.Name, m.proposal.Add)
	}
	m = send(send(m, "esc"), "i")
	if m.proposal.Name != "flowy.test" || !slices.ContainsFunc(m.proposal.Add, func(d store.Domain) bool { return d.Name == "web.flowy.test" }) {
		t.Errorf("name %q, add %+v after esc and i; want the names typed forgotten", m.proposal.Name, m.proposal.Add)
	}
}

func TestScan_RenameToEmptyLeavesReview(t *testing.T) {
	t.Parallel()

	b := inNewFlowy()
	m := send(readyIn(b, unlisted, flowyOrigin), "e")
	b.project.Apps, b.project.Compose = nil, nil // the project lost its apps and compose files since the review opened
	m = send(m, "enter")
	if m.mode != modeList || m.note != "scan found no dev server or compose file in ~/Projects/flowy" {
		t.Errorf("mode %v, note %q; want the list and why", m.mode, m.note)
	}
}
