package tui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/oo/internal/check"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
)

// A project in ~/Projects/flowy with a dev and a prod compose file.
const (
	home       = "/Users/me"
	flowyRoot  = home + "/Projects/flowy"
	flowyDev   = flowyRoot + "/compose.dev.yaml"
	flowyProd  = flowyRoot + "/compose.prod.yaml"
	flowyStart = flowyRoot + "/apps/web" // oo can start below the project's root
)

const flowyCompose = `services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:5432"]
  web:
    image: flowy-web
    environment:
      APP_URL: http://localhost:3000
`

// inFlowy is a backend for an oo started inside the flowy project.
func inFlowy() *fakeBackend {
	return &fakeBackend{
		projectRoot:  flowyRoot,
		projectFiles: []string{flowyDev, flowyProd},
		composeFiles: map[string]string{flowyDev: flowyCompose, flowyProd: flowyCompose},
	}
}

var flowyOrigin = Start{Dir: flowyStart, Home: home}

func TestQuestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		domains    []store.Domain
		key        string
		wantMode   mode
		wantStatus string // the question, before the key
		wantSaved  string // flowy.oo's compose value once saved; empty when nothing is saved
	}{
		{
			name: "y links the file", domains: sample, key: "y", wantMode: modeList,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n", wantSaved: flowyDev,
		},
		{
			name: "enter is yes", domains: sample, key: "enter", wantMode: modeList,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n", wantSaved: flowyDev,
		},
		{
			name: "n saves no", domains: sample, key: "n", wantMode: modeList,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n", wantSaved: store.NoCompose,
		},
		{
			name: "esc is no", domains: sample, key: "esc", wantMode: modeList,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n", wantSaved: store.NoCompose,
		},
		{
			name: "e edits the path first", domains: sample, key: "e", wantMode: modeForm,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n",
		},
		{
			name: "another key waits for an answer", domains: sample, key: "x", wantMode: modeAsk,
			wantStatus: "use ./compose.dev.yaml for flowy.oo? Y/n",
		},
		{
			name: "an unlisted name: enter opens the add form", domains: sample[:2], key: "enter", wantMode: modeForm,
			wantStatus: "add flowy.oo with ./compose.dev.yaml? Y/n",
		},
		{
			name: "an unlisted name: n saves nothing", domains: sample[:2], key: "n", wantMode: modeList,
			wantStatus: "add flowy.oo with ./compose.dev.yaml? Y/n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := inFlowy()
			m := readyIn(b, tt.domains, flowyOrigin)
			if m.mode != modeAsk {
				t.Fatalf("mode = %v after the first report, want the question", m.mode)
			}
			if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != tt.wantStatus {
				t.Errorf("status line = %q, want %q", got, tt.wantStatus)
			}
			m = send(m, tt.key)
			if m.mode != tt.wantMode {
				t.Fatalf("mode = %v after %s, want %v", m.mode, tt.key, tt.wantMode)
			}
			if len(b.applied) != 0 {
				t.Errorf("applied %v; a compose path changes no generated file", b.applied)
			}
			if tt.wantSaved == "" {
				if len(b.saved) != 0 {
					t.Errorf("saved %v, want nothing", b.saved)
				}
				return
			}
			if got := composeOf(t, b, "flowy.oo"); got != tt.wantSaved {
				t.Errorf("flowy.oo's compose = %q, want %q", got, tt.wantSaved)
			}
			if d, _ := m.find("flowy.oo"); d.Compose != tt.wantSaved {
				t.Errorf("the list holds %q, want %q", d.Compose, tt.wantSaved)
			}
		})
	}
}

func TestQuestion_PointsAtItsRow(t *testing.T) {
	t.Parallel()

	listed := readyIn(inFlowy(), sample, flowyOrigin)
	if got := listed.cursorName(); listed.mode != modeAsk || got != "flowy.oo" {
		t.Errorf("mode %v, cursor on %q; want the question with flowy.oo highlighted", listed.mode, got)
	}
	unlisted := readyIn(inFlowy(), sample[:2], flowyOrigin)
	if unlisted.mode != modeAsk || !unlisted.onAddRow() {
		t.Errorf("mode %v, cursor on %q; want the question with the add row highlighted", unlisted.mode, unlisted.cursorName())
	}
	if got := unlisted.table.Rows()[len(sample[:2])][0]; got != addRowText {
		t.Errorf("add row = %q, want it plain so it takes the highlight", got)
	}
}

func TestQuestion_EditFillsThePath(t *testing.T) {
	t.Parallel()

	b := inFlowy()
	m := send(readyIn(b, sample, flowyOrigin), "e")
	if m.form.editing != "flowy.oo" || m.form.focus != fieldCompose {
		t.Fatalf("editing %q with field %d focused, want flowy.oo's compose field", m.form.editing, m.form.focus)
	}
	if got := m.form.inputs[fieldCompose].Value(); got != "~/Projects/flowy/compose.dev.yaml" {
		t.Errorf("compose field = %q, want the file found, from home", got)
	}
	m = send(m, "enter")
	if got := composeOf(t, b, "flowy.oo"); got != flowyDev {
		t.Errorf("flowy.oo's compose = %q, want %q", got, flowyDev)
	}
	if len(b.applied) != 0 {
		t.Errorf("applied %v; only the compose path changed", b.applied)
	}
}

func TestQuestion_AddFillsTheForm(t *testing.T) {
	t.Parallel()

	b := inFlowy()
	m := send(readyIn(b, sample[:2], flowyOrigin), "y")
	if got := m.form.inputs[fieldName].Value(); got != "flowy" {
		t.Errorf("name field = %q, want the project's folder", got)
	}
	if got := m.form.inputs[fieldCompose].Value(); got != "~/Projects/flowy/compose.dev.yaml" {
		t.Errorf("compose field = %q, want the file found", got)
	}
	m = send(m, "enter")
	want := store.Domain{Name: "flowy.oo", Address: "127.0.1.2", Enabled: true, Compose: flowyDev}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Errorf("saved %v, want it to hold %+v; form errors %q", b.saved, want, m.form.errs)
	}
}

func TestQuestion_NotAsked(t *testing.T) {
	t.Parallel()

	answered := slices.Clone(sample)
	answered[2].Compose = flowyProd
	declined := slices.Clone(sample)
	declined[2].Compose = store.NoCompose
	otherName := slices.Clone(sample)
	otherName[0].Compose = flowyDev // crm.oo links flowy's file
	tests := []struct {
		name    string
		backend *fakeBackend
		domains []store.Domain
		origin  Start
	}{
		{name: "the name has a compose file", backend: inFlowy(), domains: answered, origin: flowyOrigin},
		{name: "the name said no", backend: inFlowy(), domains: declined, origin: flowyOrigin},
		{name: "another name links a file in the project", backend: inFlowy(), domains: otherName, origin: flowyOrigin},
		{name: "no compose file in the project", backend: &fakeBackend{projectRoot: flowyRoot}, domains: sample, origin: flowyOrigin},
		{name: "not in a project", backend: &fakeBackend{}, domains: sample, origin: flowyOrigin},
		{name: "no folder to look in", backend: inFlowy(), domains: sample, origin: Start{Home: home}},
		{name: "a folder name that makes no label", backend: &fakeBackend{projectRoot: "/tmp/___", projectFiles: []string{"/tmp/___/compose.yml"}}, domains: sample, origin: Start{Dir: "/tmp/___"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if m := readyIn(tt.backend, tt.domains, tt.origin); m.mode != modeList {
				t.Errorf("mode = %v, want the list without a question", m.mode)
			}
		})
	}
}

func TestQuestion_AskedWhenOnlyAFolderBesideItIsLinked(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[0].Compose = flowyRoot + "-old/compose.dev.yaml" // a sibling folder whose name starts like flowy's
	if m := readyIn(inFlowy(), domains, flowyOrigin); m.mode != modeAsk {
		t.Errorf("mode = %v, want the question: flowy itself has no linked file", m.mode)
	}
}

func TestQuestion_WaitsForTheFirstCheck(t *testing.T) {
	t.Parallel()

	m := New(t.Context(), inFlowy(), sample, flowyOrigin)
	next, _ := m.Update(projectMsg{root: flowyRoot, files: []string{flowyDev}})
	m = next.(Model)
	if m.mode != modeList {
		t.Fatalf("mode = %v while the first check runs, want the list", m.mode)
	}
	next, _ = m.Update(reportMsg{checks: []check.Check{{ID: 1, OK: true}}})
	if m = next.(Model); m.mode != modeAsk {
		t.Errorf("mode = %v once the check is done, want the question", m.mode)
	}
}

func TestProjectName(t *testing.T) {
	t.Parallel()

	for root, want := range map[string]string{
		"/Users/me/Projects/flowy":      "flowy.oo",
		"/Users/me/Projects/My_App":     "my-app.oo",
		"/Users/me/Projects/web.v2":     "web-v2.oo",
		"/Users/me/Projects/--x--":      "x.oo",
		"/Users/me/Projects/___":        "",
		"/Users/me/Projects/" + "émoji": "moji.oo",
	} {
		if got := projectName(root); got != want {
			t.Errorf("projectName(%q) = %q, want %q", root, got, want)
		}
	}
}

func TestForm_Compose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		typed    string
		want     string // flowy.oo's compose once saved
		wantErr  string // under the compose field; the form stays open
		wantSave bool   // saved without an apply
	}{
		{name: "relative to where oo started", typed: "../../compose.dev.yaml", want: flowyDev},
		{name: "from home", typed: "~/Projects/flowy/compose.prod.yaml", want: flowyProd},
		{name: "absolute", typed: flowyDev, want: flowyDev},
		{name: "none", typed: "none", want: store.NoCompose},
		{name: "a missing file", typed: "~/Projects/flowy/nope.yml", wantErr: "no file at ~/Projects/flowy/nope.yml"},
		{name: "not yaml", typed: "~/Projects/flowy/package.json", wantErr: "compose file must end in .yml or .yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{composeFiles: inFlowy().composeFiles}                     // started in flowy's folder, which is no project here
			m := send(send(send(readyIn(b, sample, flowyOrigin), "down"), "down"), "e") // flowy.oo
			for m.form.focus != fieldCompose {
				m = send(m, "tab")
			}
			m = send(typeText(clearField(m), tt.typed), "enter")
			if tt.wantErr != "" {
				if m.mode != modeForm || m.form.errs[fieldCompose] != tt.wantErr {
					t.Errorf("mode %v, compose error %q; want the form open with %q", m.mode, m.form.errs[fieldCompose], tt.wantErr)
				}
				return
			}
			if got := composeOf(t, b, "flowy.oo"); got != tt.want {
				t.Errorf("flowy.oo's compose = %q, want %q; form errors %q", got, tt.want, m.form.errs)
			}
			if len(b.applied) != 0 {
				t.Errorf("applied %v; only the compose path changed", b.applied)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[2].Compose = flowyDev
	b := inFlowy()
	m := readyIn(b, domains, Start{Home: home})
	m = send(send(send(m, "down"), "down"), "p") // flowy.oo
	if m.mode != modePreview {
		t.Fatalf("mode = %v after p, err %v; want the preview", m.mode, m.err)
	}
	if m.previewTitle != "compose.dev.yaml for flowy.oo · 2 changes" || m.previewEnv != "DOCKER_HOST_IP=127.0.1.3" {
		t.Errorf("title %q, env %q", m.previewTitle, m.previewEnv)
	}
	golden.RequireEqual(t, m.View().Content)

	m = send(m, "l")
	if want := []string{flowyDev + " 127.0.1.3"}; !slices.Equal(b.linked, want) {
		t.Errorf("linked %q, want %q", b.linked, want)
	}
	if m.mode != modePreview {
		t.Errorf("mode = %v after l, want the preview still open", m.mode)
	}
	if m = send(m, "esc"); m.mode != modeList {
		t.Errorf("mode = %v after esc, want the list", m.mode)
	}
}

func TestPreview_SubdomainUsesItsProjectsFile(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[0].Compose = flowyDev                           // crm.oo's, for this test
	m := send(send(ready(inFlowy(), domains), "down"), "p") // dashboard.crm.oo, on port 3000
	if m.mode != modePreview {
		t.Fatalf("mode = %v, err %v; want the preview", m.mode, m.err)
	}
	if !strings.HasPrefix(m.previewTitle, "compose.dev.yaml for crm.oo") {
		t.Errorf("title = %q, want crm.oo's file", m.previewTitle)
	}
	if content := ansi.Strip(m.preview.GetContent()); !strings.Contains(content, "APP_URL: http://dashboard.crm.oo") {
		t.Errorf("preview:\n%s\nwant localhost:3000 sent to dashboard.crm.oo, which Caddy serves on port 3000", content)
	}
}

func TestPreview_Refused(t *testing.T) {
	t.Parallel()

	missing := slices.Clone(sample)
	missing[2].Compose = flowyRoot + "/gone.yml"
	tests := []struct {
		name    string
		domains []store.Domain
		downs   int
		wantErr string
	}{
		{name: "no compose file", domains: sample, downs: 3, wantErr: "old.oo has no compose file: e sets one"},
		{name: "the file is gone", domains: missing, downs: 2, wantErr: "open " + flowyRoot + "/gone.yml: file does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(inFlowy(), tt.domains)
			for range tt.downs {
				m = send(m, "down")
			}
			m = send(m, "p")
			if m.mode != modeList || m.err == nil || m.err.Error() != tt.wantErr {
				t.Errorf("mode %v, err %v; want the list and %q", m.mode, m.err, tt.wantErr)
			}
		})
	}
}

func TestPreview_WorksWhileAChangeRuns(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[2].Compose = flowyDev
	m := ready(inFlowy(), domains)
	next, _ := m.Update(press("space")) // crm.oo turns off; the change has not landed
	m = send(send(send(next.(Model), "down"), "down"), "p")
	if !m.busy || m.mode != modePreview {
		t.Errorf("busy %v, mode %v; want the preview while the change runs", m.busy, m.mode)
	}
}

func TestColumns(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[2].Compose = flowyDev
	domains[3].Compose = store.NoCompose
	m := readyIn(&fakeBackend{}, domains, Start{Home: home})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(Model)
	var titles []string
	for _, c := range m.table.Columns() {
		titles = append(titles, c.Title)
	}
	if want := []string{"name", "address", "port", "compose", "own", "check"}; !slices.Equal(titles, want) {
		t.Errorf("columns = %q, want %q", titles, want)
	}
	if got, want := m.table.Columns()[0].Width, ansi.StringWidth("  ● dashboard.crm.oo"); got != want {
		t.Errorf("name column = %d wide, want %d, the longest row", got, want)
	}
	if got := m.table.Rows()[2][3]; got != "~/Projects/flowy/compose.dev.yaml" {
		t.Errorf("flowy.oo's compose cell = %q, want the path from home", got)
	}
	if got := m.table.Rows()[3][3]; got != "–" {
		t.Errorf("old.oo's compose cell = %q, want – for no", got)
	}
}

func TestComposeCell_CutsFromTheLeft(t *testing.T) {
	t.Parallel()

	m := Model{origin: Start{Home: home}}
	d := store.Domain{Compose: flowyRoot + "/apps/web/compose.dev.yaml"}
	for width, want := range map[int]string{
		60: "~/Projects/flowy/apps/web/compose.dev.yaml",
		20: "…eb/compose.dev.yaml",
		8:  "…ev.yaml",
	} {
		if got := m.composeCell(d, width); got != want {
			t.Errorf("composeCell at %d = %q, want %q", width, got, want)
		}
	}
}

func TestLink(t *testing.T) {
	t.Parallel()

	withFile := slices.Clone(sample)
	withFile[2].Compose = flowyProd
	tests := []struct {
		name        string
		backend     *fakeBackend
		domains     []store.Domain
		origin      Start
		wantCompose string // flowy.oo's compose once linked; empty when nothing is saved
		wantErr     string
		wantNote    string
	}{
		{
			name: "in a project: the project's best file", backend: inFlowy(), domains: withFile, origin: flowyOrigin,
			wantCompose: flowyDev, wantNote: "linked flowy.oo · DOCKER_HOST_IP=127.0.1.3 in ~/Projects/flowy/.env",
		},
		{
			name: "outside a project: its own file", backend: &fakeBackend{}, domains: withFile, origin: Start{Home: home},
			wantCompose: flowyProd, wantNote: "linked flowy.oo · DOCKER_HOST_IP=127.0.1.3 in ~/Projects/flowy/.env",
		},
		{
			name: "no file at all", backend: &fakeBackend{}, domains: sample, origin: Start{Home: home},
			wantErr: "no compose file to link: start oo in the project, or e to set one",
		},
		{
			name: "the .env can't be written: the link stays", domains: withFile, origin: flowyOrigin,
			backend:     &fakeBackend{projectRoot: flowyRoot, projectFiles: []string{flowyDev}, linkErr: errors.New("git tracks .env")},
			wantCompose: flowyDev, wantErr: "git tracks .env",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := readyIn(tt.backend, tt.domains, tt.origin)
			m = send(send(send(m, "down"), "down"), "l") // flowy.oo
			if tt.wantCompose == "" {
				if len(tt.backend.saved) != 0 || len(tt.backend.linked) != 0 {
					t.Errorf("saved %v, linked %v; want neither", tt.backend.saved, tt.backend.linked)
				}
			} else {
				if got := composeOf(t, tt.backend, "flowy.oo"); got != tt.wantCompose {
					t.Errorf("flowy.oo's compose = %q, want %q", got, tt.wantCompose)
				}
				if want := []string{tt.wantCompose + " 127.0.1.3"}; !slices.Equal(tt.backend.linked, want) {
					t.Errorf("linked %q, want %q", tt.backend.linked, want)
				}
			}
			if len(tt.backend.applied) != 0 {
				t.Errorf("applied %v; linking changes no generated file", tt.backend.applied)
			}
			gotErr := ""
			if m.err != nil {
				gotErr = m.err.Error()
			}
			if gotErr != tt.wantErr || m.note != tt.wantNote {
				t.Errorf("err %q, note %q; want %q, %q", gotErr, m.note, tt.wantErr, tt.wantNote)
			}
		})
	}
}

func TestLink_FollowsFormChanges(t *testing.T) {
	t.Parallel()

	linked := slices.Clone(sample)
	linked[2].Compose = flowyDev
	tests := []struct {
		name    string
		domains []store.Domain
		field   int
		typed   string
		want    []string // LinkEnv calls
	}{
		{name: "a compose file set", domains: sample, field: fieldCompose, typed: flowyDev, want: []string{flowyDev + " 127.0.1.3"}},
		{name: "a linked name's address changed", domains: linked, field: fieldAddress, typed: "127.0.1.9", want: []string{flowyDev + " 127.0.1.9"}},
		{name: "none", domains: sample, field: fieldCompose, typed: "none"},
		{name: "a port changed", domains: linked, field: fieldPort, typed: "3000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{composeFiles: inFlowy().composeFiles}
			m := send(send(send(ready(b, tt.domains), "down"), "down"), "e") // flowy.oo
			for m.form.focus != tt.field {
				m = send(m, "tab")
			}
			m = send(typeText(clearField(m), tt.typed), "enter")
			if m.mode != modeList || !slices.Equal(b.linked, tt.want) {
				t.Errorf("mode %v, linked %q; want the list and %q; form errors %q", m.mode, b.linked, tt.want, m.form.errs)
			}
		})
	}
}

func TestBackend_LinkEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing string // the .env before; empty for none
		mode     os.FileMode
		tracked  bool
		symlink  bool // .env links to env.real
		want     string
		wantMode os.FileMode
		wantErr  string
	}{
		{name: "a new .env", want: "DOCKER_HOST_IP=127.0.1.3\n", wantMode: 0o644},
		{name: "a line added, mode kept", existing: "SECRET=x\n", mode: 0o600, want: "SECRET=x\nDOCKER_HOST_IP=127.0.1.3\n", wantMode: 0o600},
		{name: "a line replaced", existing: "DOCKER_HOST_IP=127.0.0.1\n", mode: 0o644, want: "DOCKER_HOST_IP=127.0.1.3\n", wantMode: 0o644},
		{name: "through a symlink", existing: "A=1\n", mode: 0o644, symlink: true, want: "A=1\nDOCKER_HOST_IP=127.0.1.3\n", wantMode: 0o644},
		{name: "tracked by git", existing: "A=1\n", mode: 0o644, tracked: true, want: "A=1\n", wantMode: 0o644, wantErr: "git tracks "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := paths.ForTest(t.TempDir())
			tmp, err := filepath.EvalSymlinks(t.TempDir()) // macOS keeps temp folders under the /var link
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(tmp, "app")
			composePath := filepath.Join(root, "compose.yml")
			writeTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
			writeTestFile(t, composePath, "services: {}\n")
			env := filepath.Join(root, ".env")
			if tt.existing != "" {
				target := env
				if tt.symlink {
					target = filepath.Join(root, "env.real")
				}
				writeTestFile(t, target, tt.existing)
				if err := os.Chmod(target, tt.mode); err != nil {
					t.Fatal(err)
				}
				if tt.symlink {
					if err := os.Symlink(target, env); err != nil {
						t.Fatal(err)
					}
				}
			}
			r := run.NewFake() // an unknown command succeeds, which would read as tracked
			for _, name := range []string{".env", "env.real"} {
				r.Fail(run.Line(p.Git, "-C", root, "ls-files", "--error-unmatch", "--", name), "error: pathspec did not match any file(s) known to git")
			}
			if tt.tracked {
				r.Set(run.Line(p.Git, "-C", root, "ls-files", "--error-unmatch", "--", ".env"), ".env\n")
			}
			_, err = NewBackend(p, r).LinkEnv(t.Context(), composePath, "127.0.1.3")
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			real := env
			if tt.symlink {
				real = filepath.Join(root, "env.real")
				if fi, err := os.Lstat(env); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf(".env is no longer a symlink: %v", err)
				}
			}
			got, err := os.ReadFile(real)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf(".env = %q, want %q", got, tt.want)
			}
			if fi, err := os.Stat(real); err != nil || fi.Mode().Perm() != tt.wantMode {
				t.Errorf("mode = %v, want %v", fi.Mode().Perm(), tt.wantMode)
			}
		})
	}
}

// composeOf returns name's compose value in the last list b saved.
func composeOf(t *testing.T, b *fakeBackend, name string) string {
	t.Helper()
	if len(b.saved) == 0 {
		t.Fatalf("nothing saved")
	}
	last := b.saved[len(b.saved)-1]
	i := slices.IndexFunc(last, func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		t.Fatalf("%s not saved: %v", name, last)
	}
	return last[i].Compose
}
