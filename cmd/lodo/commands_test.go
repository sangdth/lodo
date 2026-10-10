package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/scan"
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

func TestApp_TUIRefusesBeforeSetup(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	a := newApp(paths.ForTest(t.TempDir()), run.NewFake(), &stdout, &stderr)
	if code := a.tui(context.Background()); code != 1 {
		t.Errorf("exit code = %d on a Mac with nothing set up, want 1", code)
	}
	if !strings.Contains(stderr.String(), "✗ 1 dnsmasq") || !strings.Contains(stderr.String(), "lodo opens once these pass") {
		t.Errorf("stderr does not list the failing checks:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), " 7 ") {
		t.Errorf("the gate ran past check 5:\n%s", stderr.String())
	}
}

func TestFormatChecks(t *testing.T) {
	t.Parallel()

	golden.RequireEqual(t, formatChecks([]check.Check{
		{ID: 1, Name: "dnsmasq", OK: true, Detail: "running as tester, pid 42"},
		{ID: 2, Name: "dnsmasq config", Detail: "conf-file points at /old.conf", Fix: "lodo setup"},
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
			{Name: "crm.test", Address: "127.0.1.1", Direct: true, System: true},
			{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Direct: true, System: true, Detail: "app down: nothing answers on 127.0.1.1:3000"},
			{Name: "flowy.test", Address: "127.0.1.3", Direct: true, Detail: "macOS: no address"},
			{Name: "secure.crm.test", Address: "127.0.1.1", Port: 3004, Direct: true, System: true, HTTP: true, Secure: true, HTTPS: true},
			{
				Name: "shop.crm.test", Address: "127.0.1.1", Port: 3005, Direct: true, System: true, HTTP: true, Secure: true,
				Detail: "https: Caddy's certificate isn't trusted: run lodo setup",
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			golden.RequireEqual(t, formatResults(tt.results))
		})
	}
}

func TestApp_Scan(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := filepath.Join(tmp, "shop")
	for name, content := range map[string]string{
		".git/HEAD":               "ref: refs/heads/master\n",
		"package.json":            `{"scripts": {"dev": "turbo dev"}}`,
		"pnpm-workspace.yaml":     "packages: [apps/*]\n",
		"apps/web/package.json":   "{\n  \"scripts\": {\"dev\": \"next dev -p 3001\"}\n}\n",
		"apps/api/package.json":   `{"scripts": {"dev": "nest start --watch"}}`,
		"docker/compose.dev.yaml": "services: {}\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := paths.ForTest(t.TempDir())

	var stdout, stderr strings.Builder
	a := newApp(p, run.NewFake(), &stdout, &stderr)
	if code := a.scan(filepath.Join(root, "apps", "web"), scanFlags{}); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	got := strings.ReplaceAll(stdout.String(), root, "<root>")
	golden.RequireEqual(t, got)

	stdout.Reset()
	if code := a.scan(root, scanFlags{json: true}); code != 0 {
		t.Fatalf("--json: exit code = %d, stderr %q", code, stderr.String())
	}
	var proposal scan.Proposal
	if err := json.Unmarshal([]byte(stdout.String()), &proposal); err != nil || len(proposal.Add) != 3 {
		t.Errorf("--json gave %+v, %v; want 3 names to add", proposal, err)
	}
	if _, err := os.Stat(p.DomainsJSON); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scan wrote domains.json: %v", err)
	}

	stdout.Reset()
	if code := a.scan(root, scanFlags{json: true, name: "store"}); code != 0 {
		t.Fatalf("--name: exit code = %d, stderr %q", code, stderr.String())
	}
	if err := json.Unmarshal([]byte(stdout.String()), &proposal); err != nil || proposal.Name != "store.test" ||
		proposal.Add[0].Root != root || !strings.HasSuffix(proposal.Add[1].Name, ".store.test") {
		t.Errorf("--name store gave %+v, %v; want store.test with its subdomains", proposal, err)
	}
	if code := a.scan(root, scanFlags{name: "web.store"}); code != 2 {
		t.Errorf("--name web.store: exit code = %d, want 2", code)
	}

	stderr.Reset()
	if code := a.scan(tmp, scanFlags{}); code != 1 || !strings.Contains(stderr.String(), "not in a project") {
		t.Errorf("outside a project: exit code %d, stderr %q", code, stderr.String())
	}
}
