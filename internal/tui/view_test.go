package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

func TestModel_View(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{failing: map[string]string{"dashboard.crm.oo": "app down: nothing answers on 127.0.1.1:3000"}}
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
			backend: &fakeBackend{failing: map[string]string{"crm.oo": "macOS: no address"}},
			want:    "crm.oo: macOS: no address",
		},
		{
			name:    "another name fails",
			backend: &fakeBackend{failing: map[string]string{"flowy.oo": "macOS: no address"}},
			want:    "",
		},
		{
			name:    "a system part fails",
			backend: &fakeBackend{failingChecks: map[int]string{1: "not running", 3: "job not loaded"}},
			want:    "dnsmasq, loopback need attention: run oo doctor",
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

func TestModel_KeysFitTheNarrowestBox(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	next, _ := m.Update(tea.WindowSizeMsg{Width: minBoxWidth, Height: 24})
	m = next.(Model)
	for md, keys := range help {
		if w := ansi.StringWidth(keys); w > m.innerWidth() {
			t.Errorf("mode %v keys are %d wide, the narrowest box %d inside", md, w, m.innerWidth())
		}
	}
	if w := ansi.StringWidth(servicesHelp); w > m.innerWidth() {
		t.Errorf("services keys are %d wide, the narrowest box %d inside", w, m.innerWidth())
	}
}

func TestModel_StatusLineFitsTheWidth(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{failing: map[string]string{"crm.oo": strings.Repeat("very long detail ", 20)}}
	m := ready(b, sample)
	if w := ansi.StringWidth(m.statusLine()); w > m.innerWidth() {
		t.Errorf("status line is %d wide, box %d inside", w, m.innerWidth())
	}
}
