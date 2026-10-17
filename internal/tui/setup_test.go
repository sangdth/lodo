package tui

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// unlisted is sample without flowy.test.
var unlisted = slices.DeleteFunc(slices.Clone(sample), func(d store.Domain) bool { return d.Name == "flowy.test" })

// inFlowyApps is a backend for flowy as in the plan: a Next app in apps/web
// on 3000, a NestJS app in apps/api on 4000, an app in apps/docs whose port
// the scan can't tell, Mailpit's inbox on 8025 from compose.dev.yaml, and two
// lines in apps/api/.env the scan sets: Postgres on a port the compose file
// binds to DOCKER_HOST_IP, with a password, and a URL on the web app's port.
func inFlowyApps() *fakeBackend {
	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: "apps/web", Port: 3000}, {Dir: "apps/api", Port: 4000}, {Dir: "apps/docs"}}
	b.project.Pages = []scan.Page{{Label: "mail", Port: 8025}}
	b.project.Env = []scan.EnvLine{
		{File: "apps/api/.env", Line: 1, Key: "DATABASE_URL", Text: "DATABASE_URL=postgres://u:hunter2@localhost:5432/flowy"},
		{File: "apps/api/.env", Line: 2, Key: "WEB_URL", Text: "WEB_URL=http://localhost:3000"},
	}
	b.project.Bound, b.project.ComposeAddress = map[int]bool{5432: true}, "127.0.1.2"
	return b
}

// flowyListed is unlisted with flowy set up: its name with the folder and the
// compose file, and its web app's name with HTTPS on.
var flowyListed = append(slices.Clone(unlisted),
	store.Domain{Name: "flowy.test", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
	store.Domain{Name: "web.flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
)

// setupIn returns the setup form, on its own, for an lodo dev started in
// flowy, once its first scan is in.
func setupIn(b *fakeBackend, domains []store.Domain) Setup {
	s := NewSetup(context.Background(), b, domains, flowyOrigin)
	return settle(s, s.Init())
}

// retype empties the focused field and types text.
func retype(s Setup, text string) Setup {
	for s.input(s.focus).Position() > 0 {
		s = send(s, "backspace")
	}
	return typeText(s, text)
}

// tabTo moves the focus to stop i.
func tabTo(s Setup, i int) Setup {
	for s.focus != i {
		s = send(s, "tab")
	}
	return s
}

// lastSaved returns the last list b saved.
func lastSaved(t *testing.T, b *fakeBackend) []store.Domain {
	t.Helper()
	if len(b.saved) == 0 {
		t.Fatal("nothing saved")
	}
	return b.saved[len(b.saved)-1]
}

func TestSetup_FirstRun(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	got := s.View().Content
	if strings.Contains(got, "hunter2") {
		t.Errorf("the form shows the password:\n%s", ansi.Strip(got))
	}
	golden.RequireEqual(t, got)

	s = send(s, "enter")
	if !s.Saved() || s.Err() != nil {
		t.Fatalf("saved %v, err %v after enter; want saved", s.Saved(), s.Err())
	}
	want := []store.Domain{
		{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
		{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
		{Name: "flowy.test", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
		{Name: "api.flowy.test", Address: "127.0.1.2", Port: 4000, Enabled: true},
		{Name: "docs.flowy.test", Address: "127.0.1.2", Enabled: true},
		{Name: "mail.flowy.test", Address: "127.0.1.2", Port: 8025, Enabled: true},
		{Name: "web.flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
		{Name: "old.test", Address: "127.0.0.1"},
	}
	if got := lastSaved(t, b); !slices.Equal(got, want) {
		t.Errorf("saved\n%+v\nwant\n%+v", got, want)
	}
	if len(b.saved) != 1 || len(b.applied) != 1 || b.reports != 0 {
		t.Errorf("saved %d times, applied %d times, %d doctor runs; want one save and apply and no doctor run: lodo dev starts the apps",
			len(b.saved), len(b.applied), b.reports)
	}
	if want := []string{flowyDev + " 127.0.1.2"}; !slices.Equal(b.linked, want) {
		t.Errorf("linked %q, want %q", b.linked, want)
	}
	if want := []string{flowyRoot + " apps/api/.env:1", flowyRoot + " apps/api/.env:2"}; !slices.Equal(b.envFixed, want) {
		t.Errorf("set %q, want %q", b.envFixed, want)
	}
	if got := ansi.Strip(s.View().Content); !strings.Contains(got, "set 2 .env lines, restart the dev servers · linked flowy.test") {
		t.Errorf("the last view lacks what the change did:\n%s", got)
	}
}

func TestSetup_Edit(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, flowyListed)
	golden.RequireEqual(t, s.View().Content)

	s = send(retype(tabTo(s, rowStop(0)), "site"), "enter") // web.flowy.test is the first row
	if !s.Saved() {
		t.Fatalf("not saved: %v", s.Err())
	}
	saved := lastSaved(t, b)
	want := store.Domain{Name: "site.flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true}
	if !slices.Contains(saved, want) {
		t.Errorf("saved %+v, want it to hold %+v: a renamed name keeps HTTPS", saved, want)
	}
	if _, ok := store.Find(saved, "web.flowy.test"); ok {
		t.Errorf("saved %+v, want web.flowy.test gone", saved)
	}
	if len(b.linked) != 0 {
		t.Errorf("linked %q; flowy.test links its compose file already", b.linked)
	}
	if want := []string{flowyRoot + " apps/api/.env:1", flowyRoot + " apps/api/.env:2"}; !slices.Equal(b.envFixed, want) {
		t.Errorf("set %q, want %q", b.envFixed, want)
	}
}

// TestSetup_BareNameKeepsHTTPS moves the project's name onto an app and off
// it again: the HTTPS of the listed name follows the row.
func TestSetup_BareNameKeepsHTTPS(t *testing.T) {
	t.Parallel()

	ownApp := append(slices.Clone(unlisted), // flowy.test itself serves the web app, with HTTPS
		store.Domain{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true, Compose: flowyDev, Root: flowyRoot})
	tests := []struct {
		name    string
		domains []store.Domain
		label   string // typed into the first row, the web app's
		want    store.Domain
		gone    string // the name the save drops
	}{
		{
			name: "web gives up its name", domains: flowyListed, label: "",
			want: store.Domain{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true, Compose: flowyDev, Root: flowyRoot},
			gone: "web.flowy.test",
		},
		{
			name: "the project's name gives up its app", domains: ownApp, label: "web",
			want: store.Domain{Name: "web.flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := inFlowyApps()
			s := send(retype(tabTo(setupIn(b, tt.domains), rowStop(0)), tt.label), "enter")
			if !s.Saved() {
				t.Fatalf("not saved: %q %v", s.errMsg, s.Err())
			}
			saved := lastSaved(t, b)
			if !slices.Contains(saved, tt.want) {
				t.Errorf("saved %+v, want it to hold %+v", saved, tt.want)
			}
			if _, ok := store.Find(saved, tt.gone); tt.gone != "" && ok {
				t.Errorf("saved %+v, want %s gone", saved, tt.gone)
			}
		})
	}
}

func TestSetup_TabMovesFocus(t *testing.T) {
	t.Parallel()

	s := setupIn(inFlowyApps(), unlisted)
	s = send(send(s, "tab"), "tab")
	if s.focus != rowStop(0) || !s.rows[0].label.Focused() || s.name.Focused() {
		t.Fatalf("focus %d; want the first row's label focused alone", s.focus)
	}
	golden.RequireEqual(t, s.View().Content)

	if s = send(s, "shift+tab"); s.focus != stopAddress || !s.address.Focused() {
		t.Errorf("focus %d after shift+tab, want the address", s.focus)
	}
	if s = send(send(s, "up"), "up"); s.focus != s.stops()-1 {
		t.Errorf("focus %d after up from the name, want the last toggle %d", s.focus, s.stops()-1)
	}
}

func TestSetup_ToggleOff(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = send(tabTo(s, rowStop(len(s.rows))), "space") // the link toggle
	golden.RequireEqual(t, s.View().Content)

	s = send(send(send(s, "tab"), "space"), "enter") // and the .env lines
	if !s.Saved() || len(b.linked) != 0 || len(b.envFixed) != 0 {
		t.Errorf("saved %v, linked %q, set %q; want saved with neither", s.Saved(), b.linked, b.envFixed)
	}
	if d, _ := store.Find(lastSaved(t, b), "flowy.test"); d.Compose != "" {
		t.Errorf("flowy.test's compose = %q, want none: the link was off", d.Compose)
	}
}

func TestSetup_InlineError(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = send(retype(tabTo(s, rowStop(0)+1), "80"), "enter")
	if s.errAt != rowStop(0)+1 || s.focus != rowStop(0)+1 || len(b.saved) != 0 {
		t.Fatalf("error at %d, focus %d, saved %v; want the port's error and nothing saved", s.errAt, s.focus, b.saved)
	}
	golden.RequireEqual(t, s.View().Content)

	if s = send(s, "backspace"); s.errMsg != "" {
		t.Errorf("error %q after typing, want it gone", s.errMsg)
	}
}

func TestSetup_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stop  int
		typed string
		at    int // the stop the error shows at
		want  string
	}{
		{name: "two labels in the project", stop: stopName, typed: "web.x", want: "a project's name has one label before .test"},
		{name: "no project", stop: stopName, typed: "", want: "name needs a label before .test"},
		{name: "a bad address", stop: stopAddress, typed: "10.0.0.1", at: stopAddress, want: "address must be an IPv4 address in 127.0.0.0/8, like 127.0.1.3"},
		{name: "another project's address", stop: stopAddress, typed: "127.0.1.1", at: stopAddress, want: "127.0.1.1 belongs to crm.test"},
		{name: "a name twice", stop: rowStop(1), typed: "web", at: rowStop(1), want: "web.flowy.test is already listed"},
		{name: "not a port", stop: rowStop(0) + 1, typed: "web", at: rowStop(0) + 1, want: "port must be a number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := inFlowyApps()
			s := send(retype(tabTo(setupIn(b, unlisted), tt.stop), tt.typed), "enter")
			if s.errAt != tt.at || s.errMsg != tt.want || len(b.saved) != 0 {
				t.Errorf("error %q at %d, saved %v; want %q at %d and nothing saved", s.errMsg, s.errAt, b.saved, tt.want, tt.at)
			}
		})
	}
}

// TestSetup_NoPortNoName renames apps/docs, whose port the scan can't tell:
// a later scan couldn't tie the new name to it, so the form refuses it.
func TestSetup_NoPortNoName(t *testing.T) {
	t.Parallel()

	docsListed := append(slices.Clone(flowyListed), store.Domain{Name: "docs.flowy.test", Address: "127.0.1.2", Enabled: true})
	tests := []struct {
		name    string
		domains []store.Domain
		row     int // apps/docs's row
		label   string
	}{
		{name: "a new name", domains: unlisted, row: 2, label: "handbook"},
		{name: "the project's name alone", domains: unlisted, row: 2, label: ""},
		{name: "a listed name", domains: docsListed, row: 1, label: "handbook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := inFlowyApps()
			s := setupIn(b, tt.domains)
			if s.rows[tt.row].what != "apps/docs" {
				t.Fatalf("row %d is %q's, want apps/docs's", tt.row, s.rows[tt.row].what)
			}
			s = send(retype(tabTo(s, rowStop(tt.row)), tt.label), "enter")
			if s.errAt != rowStop(tt.row) || s.errMsg != scan.ErrUnknownPort.Error() || len(b.saved) != 0 {
				t.Errorf("error %q at %d, saved %v; want %q at %d and nothing saved",
					s.errMsg, s.errAt, b.saved, scan.ErrUnknownPort, rowStop(tt.row))
			}
		})
	}
}

func TestSetup_TakeOver(t *testing.T) {
	t.Parallel()

	moved := home + "/Projects/flow" // flowy's folder before it was renamed
	tests := []struct {
		name   string
		root   string // flow.test's folder
		exists bool   // whether that folder is still there
		app    string // the label of flow's listed name on the web app's port
		want   string // the error; empty when flowy takes flow.test over
	}{
		{name: "a folder that is gone", root: moved, app: "web"},
		{name: "no folder", root: "", app: "web"},
		{name: "the web app's own name", root: moved, app: "site"},
		{name: "another project's folder", root: moved, exists: true, app: "web", want: "flow.test belongs to ~/Projects/flow: give this project another name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := append(slices.Clone(unlisted),
				store.Domain{Name: "flow.test", Address: "127.0.1.2", Enabled: true, Root: tt.root},
				store.Domain{Name: "admin.flow.test", Address: "127.0.1.2", Port: 5000, Enabled: true},
				store.Domain{Name: tt.app + ".flow.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
			)
			b := inFlowyApps()
			b.folders = map[string]bool{moved: tt.exists}
			s := setupIn(b, domains)
			if s.name.Value() != "flowy" || s.address.Value() != "127.0.1.3" {
				t.Fatalf("form at %q %q; want the folder's name on a free address", s.name.Value(), s.address.Value())
			}
			s = send(retype(s, "flow"), "enter")
			if tt.want != "" {
				if s.errMsg != tt.want || s.errAt != stopName || len(b.saved) != 0 {
					t.Errorf("error %q at %d, saved %v; want %q at the name and nothing saved", s.errMsg, s.errAt, b.saved, tt.want)
				}
				return
			}
			if !s.Saved() {
				t.Fatalf("not saved: %q %v", s.errMsg, s.Err())
			}
			saved := lastSaved(t, b)
			got := slices.DeleteFunc(slices.Clone(saved), func(d store.Domain) bool { return store.Project(d.Name) != "flow.test" })
			want := store.Sort([]store.Domain{ // every name keeps flow.test's address, and the web app its listed name
				{Name: "flow.test", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
				{Name: "admin.flow.test", Address: "127.0.1.2", Port: 5000, Enabled: true},
				{Name: "api.flow.test", Address: "127.0.1.2", Port: 4000, Enabled: true},
				{Name: "docs.flow.test", Address: "127.0.1.2", Enabled: true},
				{Name: "mail.flow.test", Address: "127.0.1.2", Port: 8025, Enabled: true},
				{Name: tt.app + ".flow.test", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
			})
			if !slices.Equal(got, want) {
				t.Errorf("saved flow.test's names\n%+v\nwant\n%+v", got, want)
			}
			if _, ok := store.Find(saved, "flowy.test"); ok {
				t.Errorf("saved %+v, want no flowy.test", saved)
			}
			if want := []string{flowyDev + " 127.0.1.2"}; !slices.Equal(b.linked, want) {
				t.Errorf("linked %q, want %q: DOCKER_HOST_IP keeps flow.test's address", b.linked, want)
			}
		})
	}
}

// TestSetup_SameFolderName opens the form in ~/Work/flowy while flowy.test
// belongs to ~/Projects/flowy, which is still there: the folder's name is
// refused, and a new name leaves the other project's names alone.
func TestSetup_SameFolderName(t *testing.T) {
	t.Parallel()

	other := home + "/Work/flowy"
	b := inFlowyApps()
	b.project.Root, b.project.Compose = other, []string{other + "/compose.dev.yaml"}
	b.folders = map[string]bool{flowyRoot: true}
	s := send(setupIn(b, flowyListed), "enter")
	if want := "flowy.test belongs to ~/Projects/flowy: give this project another name"; s.errMsg != want || s.errAt != stopName || len(b.saved) != 0 {
		t.Fatalf("error %q at %d, saved %v; want %q at the name and nothing saved", s.errMsg, s.errAt, b.saved, want)
	}

	s = send(retype(tabTo(retype(s, "flowy2"), stopAddress), "127.0.1.3"), "enter")
	if !s.Saved() {
		t.Fatalf("not saved: %q %v", s.errMsg, s.Err())
	}
	saved := lastSaved(t, b)
	for _, want := range flowyListed[len(flowyListed)-2:] {
		if !slices.Contains(saved, want) {
			t.Errorf("saved %+v, want %+v unchanged", saved, want)
		}
	}
	if d, _ := store.Find(saved, "flowy2.test"); d.Root != other || d.Address != "127.0.1.3" {
		t.Errorf("flowy2.test = %+v, want it on 127.0.1.3 with %s as its folder", d, other)
	}
}

func TestSetup_OneBareName(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = retype(tabTo(s, rowStop(0)), "")
	if s = send(s, "enter"); !s.Saved() {
		t.Fatalf("not saved with web's label empty: %s", s.errMsg)
	}
	want := store.Domain{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Compose: flowyDev, Root: flowyRoot}
	if saved := lastSaved(t, b); !slices.Contains(saved, want) {
		t.Errorf("saved %+v, want it to hold %+v: the empty label takes the project's name", saved, want)
	}

	b = inFlowyApps()
	s = setupIn(b, unlisted)
	s = retype(tabTo(retype(tabTo(s, rowStop(0)), ""), rowStop(1)), "")
	s = send(s, "enter")
	if want := "only one app can be flowy.test alone: give this one a label"; s.errMsg != want || s.errAt != rowStop(1) || len(b.saved) != 0 {
		t.Errorf("error %q at %d, saved %v; want %q at the second label", s.errMsg, s.errAt, b.saved, want)
	}
}

func TestSetup_RenameScansAgain(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = send(retype(s, "shop"), "tab")
	if b.scans != 2 {
		t.Fatalf("%d scans, want a second one once the name changed", b.scans)
	}
	got := ansi.Strip(s.View().Content)
	for _, want := range []string{"WEB_URL=http://web.shop.test", "api.shop.test", "DATABASE_URL=postgres://***@shop.test:5432/flowy"} {
		if !strings.Contains(got, want) {
			t.Errorf("the form lacks %q after the rename:\n%s", want, got)
		}
	}
	if send(s, "tab"); b.scans != 2 {
		t.Errorf("%d scans after a tab with nothing changed, want 2", b.scans)
	}
}

func TestSetup_EnterScansFirst(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = send(retype(tabTo(s, rowStop(0)), "site"), "enter") // web's label, saved without leaving it
	if !s.Saved() || b.scans != 2 {
		t.Fatalf("saved %v after %d scans; want saved after a second scan", s.Saved(), b.scans)
	}
	if _, ok := store.Find(lastSaved(t, b), "site.flowy.test"); !ok {
		t.Errorf("saved %+v, want site.flowy.test", lastSaved(t, b))
	}
	if !slices.Contains(b.envFixed, flowyRoot+" apps/api/.env:2") {
		t.Errorf("set %q, want WEB_URL set for site.flowy.test", b.envFixed)
	}
}

// TestSetup_EnterWaitsForTheScan presses enter while the scan of a renamed
// project still runs: the save waits for it, so the .env lines follow the new
// names.
func TestSetup_EnterWaitsForTheScan(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	next, rescan := retype(setupIn(b, unlisted), "shop").Update(press("tab"))
	s := send(next.(Setup), "enter")
	if len(b.saved) != 0 || len(b.envFixed) != 0 {
		t.Fatalf("saved %v, set %q before the scan of shop came back; want nothing yet", b.saved, b.envFixed)
	}
	if s = settle(s, rescan); !s.Saved() {
		t.Fatalf("not saved once the scan came back: %q %v", s.errMsg, s.Err())
	}
	if _, ok := store.Find(lastSaved(t, b), "web.shop.test"); !ok || len(b.envFixed) != 2 {
		t.Errorf("saved %+v, set %q; want web.shop.test and both .env lines", lastSaved(t, b), b.envFixed)
	}
	if got := s.proposal.Env; len(got) != 2 || !strings.Contains(got[1].New, "web.shop.test") {
		t.Errorf(".env lines %+v, want them on shop's names", got)
	}
}

func TestSetup_EscSavesNothing(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := send(retype(setupIn(b, unlisted), "shop"), "esc")
	if !s.done || s.Saved() || s.Err() != nil || len(b.saved) != 0 || len(b.linked) != 0 || len(b.envFixed) != 0 {
		t.Errorf("done %v, saved %v, err %v, saved %v; want canceled with no change", s.done, s.Saved(), s.Err(), b.saved)
	}
}

func TestSetup_PortNeedsCaddy(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	b.portsErr = errors.New("caddy is not installed: brew install caddy, then lodo setup")
	s := send(setupIn(b, unlisted), "enter")
	if s.Saved() || s.done || len(b.saved) != 0 {
		t.Errorf("saved %v, done %v, saved %v; want the form kept and nothing saved", s.Saved(), s.done, b.saved)
	}
	if got := ansi.Strip(s.View().Content); !strings.Contains(got, "✗ caddy is not installed") {
		t.Errorf("the form lacks why:\n%s", got)
	}
}

func TestSetup_NotSetUp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend *fakeBackend
		want    string
		wantErr error
	}{
		{name: "not in a project", backend: &fakeBackend{}, want: "✗ not in a project", wantErr: scan.ErrNoProject},
		{
			name:    "nothing to serve",
			backend: &fakeBackend{project: scan.Project{Root: flowyRoot, Name: "flowy.test"}},
			want:    "scan found no dev server or compose file in ~/Projects/flowy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := setupIn(tt.backend, unlisted)
			if got := ansi.Strip(s.View().Content); !strings.Contains(got, tt.want) || !strings.Contains(got, "esc quit") {
				t.Errorf("the form lacks %q and esc quit:\n%s", tt.want, got)
			}
			s = send(s, "enter")
			if !s.done || s.Saved() || !errors.Is(s.Err(), tt.wantErr) || len(tt.backend.saved) != 0 {
				t.Errorf("done %v, saved %v, err %v; want done, unsaved, %v", s.done, s.Saved(), s.Err(), tt.wantErr)
			}
		})
	}
}

func TestDashboard_NoScanAtStart(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := readyIn(b, unlisted, flowyOrigin)
	if m.mode != modeList || m.note != "" || b.scans != 0 {
		t.Errorf("mode %v, note %q, %d scans; want the list and no scan", m.mode, m.note, b.scans)
	}
}

func TestDashboard_I(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := send(readyIn(b, unlisted, flowyOrigin), "i")
	if m.mode != modeSetup || !m.setup.found {
		t.Fatalf("mode %v, found %v after i; want the setup form", m.mode, m.setup.found)
	}
	golden.RequireEqual(t, m.View().Content)

	m = send(m, "enter")
	if m.mode != modeList || m.err != nil {
		t.Fatalf("mode %v, err %v after enter; want the list", m.mode, m.err)
	}
	if _, ok := store.Find(m.domains, "web.flowy.test"); !ok || len(b.applied) != 1 || b.reports != 2 {
		t.Errorf("listed %+v, applied %d times, %d doctor runs; want web.flowy.test applied once and checked after the first run",
			m.domains, len(b.applied), b.reports)
	}
	if got := m.cursorName(); got != "flowy.test" {
		t.Errorf("cursor on %q, want the project's name", got)
	}
	if !strings.HasPrefix(m.note, "set 2 .env lines, restart the dev servers · linked flowy.test") {
		t.Errorf("note = %q, want what the change did", m.note)
	}
}

func TestDashboard_IEscGoesBack(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := send(send(readyIn(b, unlisted, flowyOrigin), "i"), "esc")
	if m.mode != modeList || len(b.saved) != 0 {
		t.Errorf("mode %v, saved %v; want the list and no change", m.mode, b.saved)
	}
}

func TestDashboard_ISavesAfterAChange(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := send(readyIn(b, unlisted, flowyOrigin), "i")
	m.busy = true // a change runs
	m = send(m, "enter")
	if m.mode != modeSetup || len(b.saved) != 0 || m.setup.err == nil {
		t.Errorf("mode %v, saved %v, err %v; want the form kept with why", m.mode, b.saved, m.setup.err)
	}
}

// TestSetup_Program runs the setup form on its own in a real program loop, as
// lodo dev does: enter saves, the program quits, and the final model says so.
func TestSetup_Program(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	tm := teatest.NewTestModel(t, NewSetup(t.Context(), b, unlisted, flowyOrigin), teatest.WithInitialTermSize(80, 24))
	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		return bytes.Contains(out, []byte("enter save + start"))
	}, teatest.WithDuration(5*time.Second))
	tm.Send(press("enter"))

	final, ok := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(Setup)
	if !ok {
		t.Fatal("final model is not a tui.Setup")
	}
	if !final.Saved() || final.Err() != nil || b.appliedCount() != 1 {
		t.Errorf("saved %v, err %v, applied %d times; want saved and applied once", final.Saved(), final.Err(), b.appliedCount())
	}
}
