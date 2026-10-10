package tui

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

func TestModel_View(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{failing: map[string]string{"dashboard.crm.test": "app down: nothing answers on 127.0.1.1:3000"}}
	m := ready(b, sample)
	v := m.View()
	if !v.AltScreen {
		t.Error("the view does not use the alternate screen")
	}
	golden.RequireEqual(t, v.Content)
}

func TestModel_StatusLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend *fakeBackend
		keys    []string
		want    string
	}{
		{name: "all good", backend: &fakeBackend{}, want: ""},
		{
			name:    "the selected name fails",
			backend: &fakeBackend{failing: map[string]string{"crm.test": "macOS: no address"}},
			want:    "crm.test: macOS: no address",
		},
		{
			name:    "another name fails",
			backend: &fakeBackend{failing: map[string]string{"flowy.test": "macOS: no address"}},
			want:    "",
		},
		{
			name:    "a system part fails",
			backend: &fakeBackend{failingChecks: map[int]string{1: "not running", 3: "job not loaded"}},
			want:    "dnsmasq, loopback need attention: run lodo doctor",
		},
		{
			name:    "a failed change",
			backend: &fakeBackend{applyErr: errors.New("restart dnsmasq: exit status 1")},
			keys:    []string{"space"},
			want:    "✗ restart dnsmasq: exit status 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(tt.backend, sample)
			for _, k := range tt.keys {
				m = send(m, k)
			}
			if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != tt.want {
				t.Errorf("status line = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModel_StatusLineSkipsWhatAServiceTurnedOffBreaks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend *fakeBackend
		keys    []string
		want    string
	}{
		{name: "dnsmasq off", backend: &fakeBackend{off: map[string]bool{"dnsmasq": true}}, want: ""},
		{
			name:    "caddy off, a name on a port",
			backend: &fakeBackend{off: map[string]bool{"caddy": true}},
			keys:    []string{"down"}, // dashboard.crm.test, on port 3000
			want:    "",
		},
		{
			name:    "caddy off, a name failing dns anyway",
			backend: &fakeBackend{off: map[string]bool{"caddy": true}, failing: map[string]string{"crm.test": "macOS: no address"}},
			want:    "crm.test: macOS: no address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(tt.backend, sample)
			for _, k := range tt.keys {
				m = send(m, k)
			}
			if d, _ := m.selected(); m.results[d.Name].OK() {
				t.Fatalf("%s resolves; the case needs it failing", d.Name)
			}
			if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != tt.want {
				t.Errorf("status line = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModel_OneHighlightAtATime(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	highlighted := func(m Model) bool { return strings.Contains(m.table.View(), "\x1b[1;38;5;212m") }
	if !highlighted(m) {
		t.Fatal("the selected row is not highlighted in the list")
	}
	m = send(m, "tab")
	if highlighted(m) {
		t.Error("the selected row keeps its highlight while the keys act on the services")
	}
	if !strings.Contains(m.statusBar(), m.styles.table.Selected.Render("caddy")) {
		t.Error("the picked service is not highlighted")
	}
	m = send(m, "tab")
	if !highlighted(m) {
		t.Error("the row highlight did not come back after tab")
	}
}

func TestModel_KeysFitAnEightyColumnTerminal(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}) // narrower than minBoxWidth: the box takes it all
	m = next.(Model)
	all := map[string]string{"services": servicesHelp, "add row": addRowHelp, "scan with only fixes": scanFixesHelp}
	for md, keys := range help {
		all["mode "+strconv.Itoa(int(md))] = keys
	}
	for name, keys := range all {
		lines := strings.Split(keys, "\n")
		if len(lines) != keyLines {
			t.Errorf("%s keys take %d lines, want %d, so the box keeps its height", name, len(lines), keyLines)
		}
		for _, line := range lines {
			if w := ansi.StringWidth(line); w > m.innerWidth() {
				t.Errorf("%s keys line %q is %d wide, the box %d inside", name, line, w, m.innerWidth())
			}
		}
	}
}

func TestModel_StatusLineFitsTheWidth(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{failing: map[string]string{"crm.test": strings.Repeat("very long detail ", 20)}}
	m := ready(b, sample)
	if w := ansi.StringWidth(m.statusLine()); w > m.innerWidth() {
		t.Errorf("status line is %d wide, box %d inside", w, m.innerWidth())
	}
}
