package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// A project in ~/Projects/flowy with a dev and a prod compose file.
const (
	home       = "/Users/me"
	flowyRoot  = home + "/Projects/flowy"
	flowyDev   = flowyRoot + "/compose.dev.yaml"
	flowyProd  = flowyRoot + "/compose.prod.yaml"
	flowyStart = flowyRoot + "/apps/web" // lodo can start below the project's root
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

// inFlowy is a backend for an lodo started inside the flowy project.
func inFlowy() *fakeBackend {
	return &fakeBackend{
		project:      scan.Project{Root: flowyRoot, Name: "flowy.test", Compose: []string{flowyDev, flowyProd}},
		composeFiles: map[string]string{flowyDev: flowyCompose, flowyProd: flowyCompose},
	}
}

var flowyOrigin = Start{Dir: flowyStart, Home: home}

func TestForm_Compose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		typed    string
		want     string // flowy.test's compose once saved
		wantErr  string // under the compose field; the form stays open
		wantSave bool   // saved without an apply
	}{
		{name: "relative to where lodo started", typed: "../../compose.dev.yaml", want: flowyDev},
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
			m := send(send(send(readyIn(b, sample, flowyOrigin), "down"), "down"), "e") // flowy.test
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
			if got := composeOf(t, b, "flowy.test"); got != tt.want {
				t.Errorf("flowy.test's compose = %q, want %q; form errors %q", got, tt.want, m.form.errs)
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
	m = send(send(send(m, "down"), "down"), "p") // flowy.test
	if m.mode != modePreview {
		t.Fatalf("mode = %v after p, err %v; want the preview", m.mode, m.err)
	}
	if m.previewTitle != "compose.dev.yaml for flowy.test · 2 changes" || m.previewEnv != "DOCKER_HOST_IP=127.0.1.3" {
		t.Errorf("title %q, env %q", m.previewTitle, m.previewEnv)
	}
	golden.RequireEqual(t, m.View().Content)

	m = send(m, "l")
	if got := composeOf(t, b, "flowy.test"); got != flowyDev {
		t.Errorf("flowy.test's compose = %q, want %q", got, flowyDev)
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
	domains[0].Compose = flowyDev                           // crm.test's, for this test
	m := send(send(ready(inFlowy(), domains), "down"), "p") // dashboard.crm.test, on port 3000
	if m.mode != modePreview {
		t.Fatalf("mode = %v, err %v; want the preview", m.mode, m.err)
	}
	if !strings.HasPrefix(m.previewTitle, "compose.dev.yaml for crm.test") {
		t.Errorf("title = %q, want crm.test's file", m.previewTitle)
	}
	if content := ansi.Strip(m.preview.GetContent()); !strings.Contains(content, "APP_URL: http://dashboard.crm.test") {
		t.Errorf("preview:\n%s\nwant localhost:3000 sent to dashboard.crm.test, which Caddy serves on port 3000", content)
	}
}

func TestPreview_HTTPSName(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[0].Compose = flowyDev
	domains[1].HTTPS = true                                 // dashboard.crm.test
	m := send(send(ready(inFlowy(), domains), "down"), "p") // dashboard.crm.test, on port 3000
	if content := ansi.Strip(m.preview.GetContent()); !strings.Contains(content, "APP_URL: https://dashboard.crm.test") {
		t.Errorf("preview:\n%s\nwant localhost:3000 sent to https://dashboard.crm.test", content)
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
		{name: "no compose file", domains: sample, downs: 3, wantErr: "old.test has no compose file: e sets one"},
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
	next, _ := m.Update(press("space")) // crm.test turns off; the change has not landed
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
	if want := []string{"name", "enabled", "address", "port", "compose", "check"}; !slices.Equal(titles, want) {
		t.Errorf("columns = %q, want %q", titles, want)
	}
	if got, want := m.table.Columns()[0].Width, ansi.StringWidth("  ● dashboard.crm.test"); got != want {
		t.Errorf("name column = %d wide, want %d, the longest row", got, want)
	}
	if got := m.table.Rows()[2][4]; got != "~/Projects/flowy/compose.dev.yaml" {
		t.Errorf("flowy.test's compose cell = %q, want the path from home", got)
	}
	if got := m.table.Rows()[3][4]; got != "–" {
		t.Errorf("old.test's compose cell = %q, want – for no", got)
	}
}

func TestComposeCell_CutsFromTheLeft(t *testing.T) {
	t.Parallel()

	m := Model{deps: deps{origin: Start{Home: home}}}
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
		wantCompose string // flowy.test's compose once linked; empty when nothing is saved
		wantErr     string
		wantNote    string
	}{
		{
			name: "in a project: the project's best file", backend: inFlowy(), domains: withFile, origin: flowyOrigin,
			wantCompose: flowyDev, wantNote: "linked flowy.test to ~/Projects/flowy/compose.dev.yaml · lodo dev sets DOCKER_HOST_IP=127.0.1.3",
		},
		{
			name: "outside a project: its own file", backend: &fakeBackend{}, domains: withFile, origin: Start{Home: home},
			wantCompose: flowyProd, wantNote: "linked flowy.test to ~/Projects/flowy/compose.prod.yaml · lodo dev sets DOCKER_HOST_IP=127.0.1.3",
		},
		{
			name: "no file at all", backend: &fakeBackend{}, domains: sample, origin: Start{Home: home},
			wantErr: "no compose file to link: start lodo ps in the project, or e to set one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := readyIn(tt.backend, tt.domains, tt.origin)
			m = send(send(send(m, "down"), "down"), "l") // flowy.test
			if tt.wantCompose == "" {
				if len(tt.backend.saved) != 0 {
					t.Errorf("saved %v; want nothing", tt.backend.saved)
				}
			} else if got := composeOf(t, tt.backend, "flowy.test"); got != tt.wantCompose {
				t.Errorf("flowy.test's compose = %q, want %q", got, tt.wantCompose)
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
