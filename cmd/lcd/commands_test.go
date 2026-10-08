package main

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
)

func TestApp_Doctor(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	a := newApp(paths.ForTest(t.TempDir()), run.NewFake(), &stdout, &stderr)
	if code := a.doctor(context.Background()); code != 1 {
		t.Errorf("exit code = %d on a Mac with nothing set up, want 1", code)
	}
	for id := 1; id <= 8; id++ {
		if !strings.Contains(stdout.String(), " "+string(rune('0'+id))+" ") {
			t.Errorf("check %d missing from:\n%s", id, stdout.String())
		}
	}
}

func TestFormatChecks(t *testing.T) {
	t.Parallel()

	golden.RequireEqual(t, formatChecks([]check.Check{
		{ID: 1, Name: "dnsmasq", OK: true, Detail: "running as tester, pid 42"},
		{ID: 2, Name: "dnsmasq config", Detail: "conf-file points at /old.conf", Fix: "lcd setup"},
		{ID: 8, Name: "caddy", Skipped: true, Detail: "no enabled domain has a port"},
	}))
}

func TestFormatResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		results []check.Result
	}{
		{name: "none", results: nil},
		{name: "mixed", results: []check.Result{
			{Name: "crm.local", Address: "127.0.1.1", Direct: true, System: true},
			{Name: "dashboard.crm.local", Address: "127.0.1.1", Port: 3000, Direct: true, System: true, Detail: "app down: nothing answers on 127.0.1.1:3000"},
			{Name: "flowy.local", Address: "127.0.1.3", Direct: true, Detail: "macOS: no address"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			golden.RequireEqual(t, formatResults(tt.results))
		})
	}
}
