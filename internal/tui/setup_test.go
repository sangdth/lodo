package tui

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// unlisted is sample without flowy.lodo.
var unlisted = slices.DeleteFunc(slices.Clone(sample), func(d store.Domain) bool { return d.Name == "flowy.lodo" })

// inFlowyApps is a backend for flowy as in the plan: a Next app in apps/web
// on 3000, a NestJS app in apps/api on 4000, an app in apps/docs whose port
// the scan can't tell, Mailpit's inbox on 8025 from compose.dev.yaml, and two
// lines in apps/api/.env lodo dev sets variables for: Postgres on a port the
// compose file binds to DOCKER_HOST_IP, with a password, and a URL on the web
// app's port.
func inFlowyApps() *fakeBackend {
	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: "apps/web", Port: 3000}, {Dir: "apps/api", Port: 4000}, {Dir: "apps/docs"}}
	b.project.Pages = []scan.Page{{Label: "mail", Port: 8025}}
	b.project.Env = []scan.EnvLine{
		{File: "apps/api/.env", Line: 1, Key: "DATABASE_URL", Text: "DATABASE_URL=postgres://u:hunter2@localhost:5432/flowy"},
		{File: "apps/api/.env", Line: 2, Key: "WEB_URL", Text: "WEB_URL=http://localhost:3000"},
	}
	b.project.Bound = map[int]bool{5432: true}
	return b
}

// flowyListed is unlisted with flowy set up: its name with the folder and the
// compose file, and its web app's name with HTTPS on.
var flowyListed = append(slices.Clone(unlisted),
	store.Domain{Name: "flowy.lodo", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
	store.Domain{Name: "web.flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
)

// setupIn returns the setup form, on its own, for an lodo dev started in
// flowy, once its first scan is in.
func setupIn(b *fakeBackend, domains []store.Domain) Setup {
	b.seed(domains)
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
		{Name: "crm.lodo", Address: "127.0.1.1", Enabled: true},
		{Name: "dashboard.crm.lodo", Address: "127.0.1.1", Port: 3000, Enabled: true},
		{Name: "flowy.lodo", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
		{Name: "api.flowy.lodo", Address: "127.0.1.2", Port: 4000, Enabled: true},
		{Name: "docs.flowy.lodo", Address: "127.0.1.2", Enabled: true},
		{Name: "mail.flowy.lodo", Address: "127.0.1.2", Port: 8025, Enabled: true},
		{Name: "web.flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true},
		{Name: "old.lodo", Address: "127.0.0.1"},
	}
	if got := lastSaved(t, b); !slices.Equal(got, want) {
		t.Errorf("saved\n%+v\nwant\n%+v", got, want)
	}
	if len(b.saved) != 1 || len(b.applied) != 1 || b.reports != 0 {
		t.Errorf("saved %d times, applied %d times, %d doctor runs; want one save and apply and no doctor run: lodo dev starts the apps",
			len(b.saved), len(b.applied), b.reports)
	}
	if got := ansi.Strip(s.View().Content); !strings.Contains(got, "linked flowy.lodo to ~/Projects/flowy/compose.dev.yaml") {
		t.Errorf("the last view lacks what the change did:\n%s", got)
	}
}

func TestSetup_Edit(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, flowyListed)
	golden.RequireEqual(t, s.View().Content)

	s = send(retype(tabTo(s, rowStop(0)), "site"), "enter") // web.flowy.lodo is the first row
	if !s.Saved() {
		t.Fatalf("not saved: %v", s.Err())
	}
	saved := lastSaved(t, b)
	want := store.Domain{Name: "site.flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true}
	if !slices.Contains(saved, want) {
		t.Errorf("saved %+v, want it to hold %+v: a renamed name keeps HTTPS", saved, want)
	}
	if _, ok := store.Find(saved, "web.flowy.lodo"); ok {
		t.Errorf("saved %+v, want web.flowy.lodo gone", saved)
	}
	if d, _ := store.Find(saved, "flowy.lodo"); d.Compose != flowyDev {
		t.Errorf("flowy.lodo's compose = %q, want %q kept", d.Compose, flowyDev)
	}
}

// TestSetup_BareNameKeepsHTTPS moves the project's name onto an app and off
// it again: the HTTPS of the listed name follows the row.
func TestSetup_BareNameKeepsHTTPS(t *testing.T) {
	t.Parallel()

	ownApp := append(slices.Clone(unlisted), // flowy.lodo itself serves the web app, with HTTPS
		store.Domain{Name: "flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true, Compose: flowyDev, Root: flowyRoot})
	tests := []struct {
		name    string
		domains []store.Domain
		label   string // typed into the first row, the web app's
		want    store.Domain
		gone    string // the name the save drops
	}{
		{
			name: "web gives up its name", domains: flowyListed, label: "",
			want: store.Domain{Name: "flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true, Compose: flowyDev, Root: flowyRoot},
			gone: "web.flowy.lodo",
		},
		{
			name: "the project's name gives up its app", domains: ownApp, label: "web",
			want: store.Domain{Name: "web.flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
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

	if s = send(s, "enter"); !s.Saved() {
		t.Errorf("not saved: %v", s.Err())
	}
	if d, _ := store.Find(lastSaved(t, b), "flowy.lodo"); d.Compose != "" {
		t.Errorf("flowy.lodo's compose = %q, want none: the link was off", d.Compose)
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

// shortWindow is a terminal too short for flowy's form, which then scrolls.
var shortWindow = tea.WindowSizeMsg{Width: defaultWidth, Height: 12}

func TestSetup_Scrolls(t *testing.T) {
	t.Parallel()

	s, _ := setupIn(inFlowyApps(), unlisted).update(shortWindow)
	got := s.View().Content
	if h := strings.Count(got, "\n") + 1; h != shortWindow.Height {
		t.Errorf("the form is %d lines, want the terminal's %d", h, shortWindow.Height)
	}
	golden.RequireEqual(t, got)
}

func TestSetup_ScrollFollowsTheFocus(t *testing.T) {
	t.Parallel()

	s, _ := setupIn(inFlowyApps(), unlisted).update(shortWindow)
	s = tabTo(s, rowStop(len(s.rows))) // the link toggle, the last stop: the lines under it show
	golden.RequireEqual(t, s.View().Content)

	if got := ansi.Strip(s.View().Content); !strings.Contains(got, "note  docs.flowy.lodo") || strings.Contains(got, "↓ more") {
		t.Errorf("the last stop doesn't show the end of the form:\n%s", got)
	}
	top := s.top
	if s = send(s, "shift+tab"); s.top != top {
		t.Errorf("top %d after shift+tab to a line in sight, want it kept at %d", s.top, top)
	}
}

func TestSetup_FitsUnscrolled(t *testing.T) {
	t.Parallel()

	s := setupIn(inFlowyApps(), unlisted)
	want := s.View().Content // TestSetup_FirstRun's golden
	fits := strings.Count(want, "\n") + 1
	for _, height := range []int{fits, defaultHeight, 60} {
		if got, _ := s.update(tea.WindowSizeMsg{Width: defaultWidth, Height: height}); got.View().Content != want {
			t.Errorf("at %d lines the form is\n%s\nwant\n%s", height, ansi.Strip(got.View().Content), ansi.Strip(want))
		}
	}
	if short, _ := s.update(tea.WindowSizeMsg{Width: defaultWidth, Height: fits - 1}); !strings.Contains(ansi.Strip(short.View().Content), "↓ more") {
		t.Errorf("a line short of the form, it doesn't scroll:\n%s", ansi.Strip(short.View().Content))
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
		{name: "three labels in the project", stop: stopName, typed: "web.flowy.lodo", want: "a project's name has two labels, like flowy.lodo"},
		{name: "no project", stop: stopName, typed: "", want: "name needs a label before .lodo"},
		{name: "a bad address", stop: stopAddress, typed: "10.0.0.1", at: stopAddress, want: "address must be an IPv4 address in 127.0.0.0/8, like 127.0.1.3"},
		{name: "another project's address", stop: stopAddress, typed: "127.0.1.1", at: stopAddress, want: "127.0.1.1 belongs to crm.lodo"},
		{name: "a name twice", stop: rowStop(1), typed: "mail", at: rowStop(3), want: "mail.flowy.lodo is already listed"},
		{name: "another app's folder", stop: rowStop(0), typed: "api", at: rowStop(0), want: "apps/api has api.flowy.lodo"},
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

	docsListed := append(slices.Clone(flowyListed), store.Domain{Name: "docs.flowy.lodo", Address: "127.0.1.2", Enabled: true})
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
		root   string // flow.lodo's folder
		exists bool   // whether that folder is still there
		app    string // the label of flow's listed name on the web app's port
		want   string // the error; empty when flowy takes flow.lodo over
	}{
		{name: "a folder that is gone", root: moved, app: "web"},
		{name: "no folder", root: "", app: "web"},
		{name: "the web app's own name", root: moved, app: "site"},
		{name: "another project's folder", root: moved, exists: true, app: "web", want: "flow.lodo belongs to ~/Projects/flow: give this project another name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := append(slices.Clone(unlisted),
				store.Domain{Name: "flow.lodo", Address: "127.0.1.2", Enabled: true, Root: tt.root},
				store.Domain{Name: "admin.flow.lodo", Address: "127.0.1.2", Port: 5000, Enabled: true},
				store.Domain{Name: tt.app + ".flow.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
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
			got := slices.DeleteFunc(slices.Clone(saved), func(d store.Domain) bool { return store.Project(d.Name) != "flow.lodo" })
			want := store.Sort([]store.Domain{ // every name keeps flow.lodo's address, and the web app its listed name
				{Name: "flow.lodo", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
				{Name: "admin.flow.lodo", Address: "127.0.1.2", Port: 5000, Enabled: true},
				{Name: "api.flow.lodo", Address: "127.0.1.2", Port: 4000, Enabled: true},
				{Name: "docs.flow.lodo", Address: "127.0.1.2", Enabled: true},
				{Name: "mail.flow.lodo", Address: "127.0.1.2", Port: 8025, Enabled: true},
				{Name: tt.app + ".flow.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, HTTPS: true},
			})
			if !slices.Equal(got, want) {
				t.Errorf("saved flow.lodo's names\n%+v\nwant\n%+v", got, want)
			}
			if _, ok := store.Find(saved, "flowy.lodo"); ok {
				t.Errorf("saved %+v, want no flowy.lodo", saved)
			}
		})
	}
}

// TestSetup_TakeOverDropsTheOldName renames listed flowy to site, a listed
// name with no folder: flowy.lodo and its names go, so a later scan names the
// project site.lodo.
func TestSetup_TakeOverDropsTheOldName(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	domains := append(slices.Clone(flowyListed), store.Domain{Name: "site.lodo", Address: "127.0.1.3", Enabled: true})
	s := send(retype(setupIn(b, domains), "site"), "enter")
	if !s.Saved() {
		t.Fatalf("not saved: %q %v", s.errMsg, s.Err())
	}
	saved := lastSaved(t, b)
	for _, name := range []string{"flowy.lodo", "web.flowy.lodo"} {
		if _, ok := store.Find(saved, name); ok {
			t.Errorf("saved %+v, want %s gone", saved, name)
		}
	}
	if got := scan.Propose(b.project, saved, "", b.Gone).Name; got != "site.lodo" {
		t.Errorf("a later scan names the project %s, want site.lodo", got)
	}
}

// TestSetup_SameFolderName opens the form in ~/Work/flowy while flowy.lodo
// belongs to ~/Projects/flowy, which is still there: the form offers
// flowy-2.lodo on a free address with every app, refuses flowy.lodo typed in
// without a scan and keeps its rows, and saves flowy-2.lodo leaving the other
// project's names alone.
func TestSetup_SameFolderName(t *testing.T) {
	t.Parallel()

	other := home + "/Work/flowy"
	b := inFlowyApps()
	b.project.Root, b.project.Compose = other, []string{other + "/compose.dev.yaml"}
	b.folders = map[string]bool{flowyRoot: true}
	s := setupIn(b, flowyListed)
	if s.name.Value() != "flowy-2" || s.address.Value() != "127.0.1.3" || len(s.rows) != 4 {
		t.Fatalf("form at %q %q with %d rows; want flowy-2 on a free address with web, api, docs and mail",
			s.name.Value(), s.address.Value(), len(s.rows))
	}

	s = send(send(retype(s, "flowy"), "tab"), "enter")
	if want := "flowy.lodo belongs to ~/Projects/flowy: give this project another name"; s.errMsg != want || s.errAt != stopName || len(b.saved) != 0 {
		t.Fatalf("error %q at %d, saved %v; want %q at the name and nothing saved", s.errMsg, s.errAt, b.saved, want)
	}
	if b.scans != 1 || len(s.rows) != 4 {
		t.Fatalf("%d scans, %d rows; want no scan of flowy.lodo and the rows kept", b.scans, len(s.rows))
	}

	s = send(retype(s, "flowy-2"), "enter")
	if !s.Saved() {
		t.Fatalf("not saved: %q %v", s.errMsg, s.Err())
	}
	saved := lastSaved(t, b)
	for _, want := range flowyListed[len(flowyListed)-2:] {
		if !slices.Contains(saved, want) {
			t.Errorf("saved %+v, want %+v unchanged", saved, want)
		}
	}
	if d, _ := store.Find(saved, "flowy-2.lodo"); d.Root != other || d.Address != "127.0.1.3" {
		t.Errorf("flowy-2.lodo = %+v, want it on 127.0.1.3 with %s as its folder", d, other)
	}
	if d, _ := store.Find(saved, "web.flowy-2.lodo"); d.Port != 3000 || d.Address != "127.0.1.3" {
		t.Errorf("web.flowy-2.lodo = %+v, want it on 127.0.1.3 port 3000", d)
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
	want := store.Domain{Name: "flowy.lodo", Address: "127.0.1.2", Port: 3000, Enabled: true, Compose: flowyDev, Root: flowyRoot}
	if saved := lastSaved(t, b); !slices.Contains(saved, want) {
		t.Errorf("saved %+v, want it to hold %+v: the empty label takes the project's name", saved, want)
	}

	b = inFlowyApps()
	s = setupIn(b, unlisted)
	s = retype(tabTo(retype(tabTo(s, rowStop(0)), ""), rowStop(1)), "")
	s = send(s, "enter")
	if want := "only one app can be flowy.lodo alone: give this one a label"; s.errMsg != want || s.errAt != rowStop(1) || len(b.saved) != 0 {
		t.Errorf("error %q at %d, saved %v; want %q at the second label", s.errMsg, s.errAt, b.saved, want)
	}
}

// TestSetup_RootAppKeepsTheName gives flowy an app in its root: a later scan
// ties that app to flowy.lodo, so the form refuses a label on its row and the
// project's name alone on another.
func TestSetup_RootAppKeepsTheName(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		row   int
		label string
	}{{row: 0, label: "site"}, {row: 1, label: ""}} {
		b := inFlowyApps()
		b.project.Apps = append([]scan.App{{Dir: ".", Port: 3001}}, b.project.Apps...)
		s := setupIn(b, unlisted)
		if s.rows[0].what != "." {
			t.Fatalf("the first row is %q's, want the root's", s.rows[0].what)
		}
		s = send(retype(tabTo(s, rowStop(tt.row)), tt.label), "enter")
		if want := "the root's app has flowy.lodo"; s.errMsg != want || s.errAt != rowStop(tt.row) || len(b.saved) != 0 {
			t.Errorf("label %q on row %d: error %q at %d, saved %v; want %q there and nothing saved",
				tt.label, tt.row, s.errMsg, s.errAt, b.saved, want)
		}
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
	if got := ansi.Strip(s.View().Content); !strings.Contains(got, "api.shop.lodo") {
		t.Errorf("the form lacks api.shop.lodo after the rename:\n%s", got)
	}
	var vars []string
	for _, v := range s.proposal.Env {
		vars = append(vars, v.Key+"="+v.Value)
	}
	if want := []string{"DATABASE_URL=postgres://***@shop.lodo:5432/flowy", "WEB_URL=http://web.shop.lodo"}; !slices.Equal(vars, want) {
		t.Errorf("variables %q after the rename, want %q", vars, want)
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
	if _, ok := store.Find(lastSaved(t, b), "site.flowy.lodo"); !ok {
		t.Errorf("saved %+v, want site.flowy.lodo", lastSaved(t, b))
	}
	if got := s.proposal.Env; len(got) != 2 || got[1].Env() != "WEB_URL=http://site.flowy.lodo" {
		t.Errorf("variables %+v, want WEB_URL on site.flowy.lodo", got)
	}
}

// TestSetup_EnterWaitsForTheScan presses enter while the scan of a renamed
// project still runs: the save waits for it, so the variables follow the new
// names.
func TestSetup_EnterWaitsForTheScan(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	next, rescan := retype(setupIn(b, unlisted), "shop").Update(press("tab"))
	s := send(next.(Setup), "enter")
	if len(b.saved) != 0 {
		t.Fatalf("saved %v before the scan of shop came back; want nothing yet", b.saved)
	}
	if s = settle(s, rescan); !s.Saved() {
		t.Fatalf("not saved once the scan came back: %q %v", s.errMsg, s.Err())
	}
	if _, ok := store.Find(lastSaved(t, b), "web.shop.lodo"); !ok {
		t.Errorf("saved %+v; want web.shop.lodo", lastSaved(t, b))
	}
	if got := s.proposal.Env; len(got) != 2 || !strings.Contains(got[1].Value, "web.shop.lodo") {
		t.Errorf("variables %+v, want them on shop's names", got)
	}
}

func TestSetup_EscSavesNothing(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := send(retype(setupIn(b, unlisted), "shop"), "esc")
	if !s.done || s.Saved() || s.Err() != nil || len(b.saved) != 0 {
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

// TestSetup_Auto saves the first scan's proposal with no key pressed, as
// lodo dev -y does, and stays open with the reason when the save can't run.
func TestSetup_Auto(t *testing.T) {
	t.Parallel()

	auto := flowyOrigin
	auto.Auto = true
	b := inFlowyApps()
	b.onDisk = slices.Clone(unlisted)
	s := NewSetup(context.Background(), b, unlisted, auto)
	if s = settle(s, s.Init()); !s.Saved() || s.Err() != nil {
		t.Fatalf("saved %v, err %v with Auto; want saved with no key", s.Saved(), s.Err())
	}
	if d, ok := store.Find(lastSaved(t, b), "flowy.lodo"); !ok || d.Compose != flowyDev {
		t.Errorf("flowy.lodo = %+v, %v; want it saved with its compose file", d, ok)
	}

	b = inFlowyApps()
	b.onDisk = slices.Clone(unlisted)
	b.portsErr = errors.New("caddy is not installed: brew install caddy, then lodo setup")
	s = NewSetup(context.Background(), b, unlisted, auto)
	if s = settle(s, s.Init()); s.Saved() || s.done || len(b.saved) != 0 {
		t.Errorf("saved %v, done %v, saved %v; want the form kept open and nothing saved", s.Saved(), s.done, b.saved)
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
			backend: &fakeBackend{project: scan.Project{Root: flowyRoot, Name: "flowy.lodo"}},
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
	if _, ok := store.Find(m.domains, "web.flowy.lodo"); !ok || len(b.applied) != 1 || b.reports != 2 {
		t.Errorf("listed %+v, applied %d times, %d doctor runs; want web.flowy.lodo applied once and checked after the first run",
			m.domains, len(b.applied), b.reports)
	}
	if got := m.cursorName(); got != "flowy.lodo" {
		t.Errorf("cursor on %q, want the project's name", got)
	}
	if !strings.HasPrefix(m.note, "linked flowy.lodo") {
		t.Errorf("note = %q, want what the change did", m.note)
	}
}

// TestDashboard_IScrollsInTheBox checks that the form takes the box's height:
// at the default size it scrolls (TestDashboard_I's golden), and a taller
// terminal shows it whole.
func TestDashboard_IScrollsInTheBox(t *testing.T) {
	t.Parallel()

	m := send(readyIn(inFlowyApps(), unlisted, flowyOrigin), "i")
	if got := ansi.Strip(m.View().Content); !strings.Contains(got, "↓ more") {
		t.Fatalf("the form doesn't scroll in a %d-line terminal:\n%s", defaultHeight, got)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: 40})
	if got := ansi.Strip(next.(Model).View().Content); strings.Contains(got, "↓ more") || !strings.Contains(got, "note  docs.flowy.lodo") {
		t.Errorf("the form doesn't show whole in a 40-line terminal:\n%s", got)
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

func TestDashboard_ISavesWhileAChangeRuns(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	m := readyIn(b, unlisted, flowyOrigin)
	next, cmd1 := m.Update(press("space")) // crm.lodo starts turning off
	m = send(next.(Model), "i")
	m = send(m, "enter") // queues flowy.lodo's setup behind crm.lodo's toggle
	if m.mode != modeList || m.err != nil {
		t.Fatalf("mode %v, err %v; want the setup form closed and queued", m.mode, m.err)
	}
	m = settle(m, cmd1)
	if d, ok := store.Find(m.domains, "crm.lodo"); !ok || d.Enabled {
		t.Errorf("crm.lodo = %+v, %v; want it turned off", d, ok)
	}
	if _, ok := store.Find(m.domains, "web.flowy.lodo"); !ok {
		t.Errorf("domains = %+v; want flowy's names added", m.domains)
	}
}

// TestSetup_Program runs the setup form on its own in a real program loop, as
// lodo dev does: enter saves, the program quits, and the final model says so.
func TestSetup_Program(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	b.onDisk = slices.Clone(unlisted)
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

// TestSetup_EditRootAppWithoutPort checks that lodo dev --edit still shows the
// root app's row when it was saved without a port so the user can set one.
func TestSetup_EditRootAppWithoutPort(t *testing.T) {
	t.Parallel()

	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: "."}}
	domains := append(slices.Clone(unlisted),
		store.Domain{Name: "flowy.lodo", Address: "127.0.1.2", Enabled: true, Compose: flowyDev, Root: flowyRoot},
	)
	s := setupIn(b, domains)
	if len(s.rows) != 1 || s.rows[0].what != "." {
		t.Fatalf("rows = %+v, want one row for the root app", s.rows)
	}
	s = send(typeText(tabTo(s, rowStop(0)+1), "4000"), "enter")
	if !s.Saved() || s.Err() != nil {
		t.Fatalf("saved %v, err %v (%q)", s.Saved(), s.Err(), s.errMsg)
	}
	if d, ok := store.Find(lastSaved(t, b), "flowy.lodo"); !ok || d.Port != 4000 {
		t.Errorf("flowy.lodo = %+v, %v; want port 4000", d, ok)
	}
}

// TestSetup_DuplicatePortRejected checks that two enabled apps in the same
// project cannot be saved on the same port.
func TestSetup_DuplicatePortRejected(t *testing.T) {
	t.Parallel()

	b := inFlowy()
	b.project.Apps = []scan.App{{Dir: "apps/web", Port: 3000}, {Dir: "apps/admin", Port: 3000}}
	s := send(setupIn(b, unlisted), "enter")
	if s.Saved() || len(b.saved) != 0 || s.errAt != rowStop(1)+1 || !strings.Contains(s.errMsg, "port 3000 is already used by") {
		t.Errorf("saved %v, errAt %d, errMsg %q; want second app port rejected", s.Saved(), s.errAt, s.errMsg)
	}
}

// TestSetup_AutoConcurrentReconciles checks that if another process updates
// domains.json between Scan and Save under Auto (-y), saveSetup re-scans under
// the lock so the second project takes the next free address without clobbering.
func TestSetup_AutoConcurrentReconciles(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	b.onDisk = slices.Clone(unlisted)
	auto := flowyOrigin
	auto.Auto = true
	s := NewSetup(context.Background(), b, unlisted, auto)
	next, cmd := s.Update(settleMsg(s.Init()))
	// Simulate another process saving 127.0.1.2 before s's submit acquires the lock.
	b.onDisk = append(slices.Clone(unlisted), store.Domain{Name: "other.lodo", Address: "127.0.1.2", Enabled: true})
	s2 := settle(next.(Setup), cmd)
	if !s2.Saved() || s2.Err() != nil {
		t.Fatalf("saved %v, err %v", s2.Saved(), s2.Err())
	}
	saved := lastSaved(t, b)
	if _, ok := store.Find(saved, "other.lodo"); !ok {
		t.Errorf("other.lodo was clobbered: %+v", saved)
	}
	if d, ok := store.Find(saved, "flowy.lodo"); !ok || d.Address != "127.0.1.3" {
		t.Errorf("flowy.lodo = %+v, %v; want it moved to 127.0.1.3", d, ok)
	}
}

func TestSetup_CustomDomain(t *testing.T) {
	t.Parallel()

	b := inFlowyApps()
	s := setupIn(b, unlisted)
	s = send(retype(s, "shop.custom"), "enter")
	if !s.Saved() || s.Err() != nil {
		t.Fatalf("saved %v, err %v (%q); want saved", s.Saved(), s.Err(), s.errMsg)
	}
	saved := lastSaved(t, b)
	for _, want := range []string{"shop.custom", "web.shop.custom", "api.shop.custom", "docs.shop.custom", "mail.shop.custom"} {
		if _, ok := store.Find(saved, want); !ok {
			t.Errorf("saved %+v, want %s", saved, want)
		}
	}

	// Start.Name with Auto (-y --name) renames an already-listed project and preserves HTTPS.
	b2 := inFlowyApps()
	b2.onDisk = slices.Clone(flowyListed)
	origin := flowyOrigin
	origin.Name, origin.Auto = "shop.custom", true
	s2 := NewSetup(context.Background(), b2, flowyListed, origin)
	if s2 = settle(s2, s2.Init()); !s2.Saved() || s2.Err() != nil {
		t.Fatalf("Auto with Name: saved %v, err %v (%q)", s2.Saved(), s2.Err(), s2.errMsg)
	}
	saved2 := lastSaved(t, b2)
	if d, ok := store.Find(saved2, "web.shop.custom"); !ok || !d.HTTPS {
		t.Errorf("web.shop.custom = %+v, %v; want HTTPS preserved", d, ok)
	}
	if _, ok := store.Find(saved2, "flowy.lodo"); ok {
		t.Errorf("old flowy.lodo kept in %+v", saved2)
	}

	// Start.Name with Auto (-y --name) on a taken domain leaves the form editable, not frozen.
	b3 := inFlowyApps()
	b3.folders = map[string]bool{home + "/Projects/crm": true}
	taken := slices.Clone(unlisted)
	taken[0].Root = home + "/Projects/crm"
	b3.onDisk = slices.Clone(taken)
	origin3 := flowyOrigin
	origin3.Name, origin3.Auto = "crm.lodo", true
	s3 := NewSetup(context.Background(), b3, taken, origin3)
	s3 = settle(s3, s3.Init())
	if s3.Saved() || s3.done || !strings.Contains(s3.errMsg, "belongs to") {
		t.Fatalf("Auto on taken name: saved %v, done %v, errMsg %q", s3.Saved(), s3.done, s3.errMsg)
	}
	s3 = send(retype(s3, "shop.custom"), "enter")
	if !s3.Saved() || s3.Err() != nil {
		t.Fatalf("typing a new name after taken Auto error: saved %v, err %v (%q)", s3.Saved(), s3.Err(), s3.errMsg)
	}

	// Start.Name in a folder whose name makes no label (~/Projects/__) still sets up the project.
	b4 := inFlowyApps()
	b4.project.Root, b4.project.Name = home+"/Projects/__", ""
	b4.onDisk = slices.Clone(unlisted)
	origin4 := Start{Dir: b4.project.Root, Home: home, Name: "shop.custom", Auto: true}
	s4 := NewSetup(context.Background(), b4, unlisted, origin4)
	if s4 = settle(s4, s4.Init()); !s4.Saved() || s4.Err() != nil {
		t.Fatalf("unnameable folder with --name: saved %v, err %v (%q)", s4.Saved(), s4.Err(), s4.errMsg)
	}
	if _, ok := store.Find(lastSaved(t, b4), "web.shop.custom"); !ok {
		t.Errorf("saved %+v, want web.shop.custom", lastSaved(t, b4))
	}
}

func settleMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
