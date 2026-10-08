package tui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/oo/internal/store"
)

func TestPress(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"space", "down", "up", "enter", "esc", "tab", "shift+tab", "backspace", "ctrl+c", "q", "r"} {
		if got := press(k).String(); got != k {
			t.Errorf("press(%q).String() = %q", k, got)
		}
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := New(t.Context(), b, sample)
	if m.busy != "checking" {
		t.Errorf("busy = %q before the first report, want checking", m.busy)
	}
	if got := m.rows()[0][4]; got != "…" {
		t.Errorf("check cell before the first report = %q, want …", got)
	}
	m = settle(m, m.Init())
	if m.busy != "" || b.reports != 1 || len(m.checks) != 8 || len(m.results) != 3 {
		t.Errorf("after Init: busy %q, %d reports, %d checks, %d results", m.busy, b.reports, len(m.checks), len(m.results))
	}
}

func TestModel_Toggle(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	m = send(m, "down") // dashboard.crm.oo
	m = send(m, "space")

	want, err := store.Toggle(sample, "dashboard.crm.oo")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.saved) != 1 || !slices.Equal(b.saved[0], want) {
		t.Errorf("saved %v, want %v", b.saved, want)
	}
	if len(b.applied) != 1 || !slices.Equal(b.applied[0], want) {
		t.Errorf("applied %v, want %v", b.applied, want)
	}
	if m.domains[1].Enabled || m.busy != "" || m.err != nil {
		t.Errorf("after the change: %+v, busy %q, err %v", m.domains[1], m.busy, m.err)
	}
	if got := m.rows()[1][4]; got != "–" {
		t.Errorf("check cell of a disabled name = %q, want –", got)
	}
}

func TestModel_ApplyFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{applyErr: errBoom}
	m := send(ready(b, sample), "space")
	if m.err == nil || m.busy != "" {
		t.Fatalf("err %v, busy %q; want the apply error and idle", m.err, m.busy)
	}
	if m.domains[0].Enabled {
		t.Error("the saved toggle was dropped; it must stay so r can apply it again")
	}
}

func TestModel_SaveFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{saveErr: errBoom}
	m := send(ready(b, sample), "space")
	if m.err == nil {
		t.Fatal("err = nil, want the save error")
	}
	if !m.domains[0].Enabled || len(b.applied) != 0 {
		t.Errorf("list changed or applied after a failed save: %+v, %d applies", m.domains[0], len(b.applied))
	}
}

func TestModel_Reload(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	b.onDisk = []store.Domain{{Name: "new.oo", Address: "127.0.1.9", Enabled: true}}
	m = send(m, "r")
	if len(b.applied) != 1 || b.applied[0][0].Name != "new.oo" {
		t.Errorf("applied %v, want the list on disk", b.applied)
	}
	if len(m.domains) != 1 || m.domains[0].Name != "new.oo" {
		t.Errorf("domains = %v, want the list on disk", m.domains)
	}
}

func TestModel_KeysWhileBusy(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	next, _ := m.Update(press("space")) // starts a change and leaves it running
	m = next.(Model)

	tests := []struct {
		name     string
		key      string
		wantQuit bool
	}{
		{name: "space does nothing", key: "space"},
		{name: "r does nothing", key: "r"},
		{name: "q does not quit", key: "q"},
		{name: "ctrl+c quits", key: "ctrl+c", wantQuit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, cmd := m.Update(press(tt.key))
			if quit := cmd != nil && isQuit(cmd()); quit != tt.wantQuit {
				t.Errorf("quit = %v, want %v", quit, tt.wantQuit)
			}
		})
	}
	if next, _ := m.Update(press("down")); next.(Model).table.Cursor() != 1 {
		t.Error("the cursor does not move while busy")
	}
	if len(b.saved) != 0 {
		t.Errorf("a key started a second change: %v", b.saved)
	}
}

func TestModel_Quit(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	_, cmd := m.Update(press("q"))
	if cmd == nil || !isQuit(cmd()) {
		t.Error("q does not quit when idle")
	}
}

func TestModel_WindowSize(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	if got := m.table.Columns()[0].Width; got != 120-addressWidth-portWidth-ownWidth-checkWidth-2*columns {
		t.Errorf("name column = %d wide at 120 columns", got)
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 2})
	if got := next.(Model).table.Columns()[0].Width; got != minNameWidth {
		t.Errorf("name column = %d wide in a tiny terminal, want %d", got, minNameWidth)
	}
}

func isQuit(msg tea.Msg) bool {
	_, ok := msg.(tea.QuitMsg)
	return ok
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
