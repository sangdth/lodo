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
	"github.com/sangdth/lodo/internal/store"
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

func TestApp_PsRefusesBeforeSetup(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	a := newApp(paths.ForTest(t.TempDir()), run.NewFake(), &stdout, &stderr)
	if code := a.command(context.Background(), "ps", options{}); code != 1 {
		t.Errorf("exit code = %d on a Mac with nothing set up, want 1", code)
	}
	if !strings.Contains(stderr.String(), "✗ 1 dnsmasq") || !strings.Contains(stderr.String(), "lodo ps opens once these pass") {
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
			{
				Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Direct: true, System: true, Down: true,
				Detail: "app down: nothing answers on 127.0.1.1:3000",
			},
			{
				Name: "docs.crm.test", Address: "127.0.1.1", Port: 3001, Direct: true, System: true,
				Detail: "an app listens on 127.0.0.1:3001, not on the name's address: start it with lodo dev in its project",
			},
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

func TestProbeCode(t *testing.T) {
	t.Parallel()

	up := check.Result{Name: "crm.test", Direct: true, System: true}
	down := check.Result{Name: "dashboard.crm.test", Port: 3000, Direct: true, System: true, Down: true}
	broken := check.Result{Name: "flowy.test", Direct: true}
	tests := []struct {
		name    string
		results []check.Result
		want    int
	}{
		{name: "none", results: nil, want: 0},
		{name: "every name passes", results: []check.Result{up}, want: 0},
		{name: "an app isn't running", results: []check.Result{up, down}, want: 0},
		{name: "another failure next to it", results: []check.Result{down, broken}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := probeCode(tt.results); got != tt.want {
				t.Errorf("probeCode = %d, want %d", got, tt.want)
			}
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
		"apps/web/.env":           "API_URL=http://localhost:3001\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n",
		".env":                    "DOCKER_HOST_IP=127.0.1.1\n",
		"docker/compose.dev.yaml": `services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}`,
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
	realWeb, err := filepath.EvalSymlinks(filepath.Join(root, "apps", "web"))
	if err != nil {
		t.Fatal(err)
	}
	r := run.NewFake()
	r.Fail(run.Line(p.Git, "--literal-pathspecs", "-c", "safe.bareRepository=explicit", "-c", "core.fsmonitor=false",
		"-C", realWeb, "ls-files", "--error-unmatch", "--", ".env"), "") // untracked; Fake says tracked otherwise

	var stdout, stderr strings.Builder
	a := newApp(p, r, &stdout, &stderr)
	if code := a.scan(t.Context(), filepath.Join(root, "apps", "web"), options{}); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	got := strings.ReplaceAll(stdout.String(), root, "<root>")
	for _, want := range []string{"set   apps/web/.env:1", "+ API_URL=http://web.shop.test\n", "+ DATABASE_URL=postgres://***@shop.test:5432/shop\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("scan lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("scan shows the password:\n%s", got)
	}
	golden.RequireEqual(t, got)

	stdout.Reset()
	if code := a.scan(t.Context(), root, options{json: true}); code != 0 {
		t.Fatalf("--json: exit code = %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "hunter2") {
		t.Errorf("--json shows the password:\n%s", stdout.String())
	}
	var proposal scan.Proposal
	if err := json.Unmarshal([]byte(stdout.String()), &proposal); err != nil || len(proposal.Add) != 3 || len(proposal.Env) != 2 {
		t.Errorf("--json gave %+v, %v; want 3 names to add and 2 .env lines to set", proposal, err)
	}
	if _, err := os.Stat(p.DomainsJSON); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scan wrote domains.json: %v", err)
	}

	stdout.Reset()
	if code := a.scan(t.Context(), root, options{json: true, name: "store"}); code != 0 {
		t.Fatalf("--name: exit code = %d, stderr %q", code, stderr.String())
	}
	if err := json.Unmarshal([]byte(stdout.String()), &proposal); err != nil || proposal.Name != "store.test" ||
		proposal.Add[0].Root != root || !strings.HasSuffix(proposal.Add[1].Name, ".store.test") {
		t.Errorf("--name store gave %+v, %v; want store.test with its subdomains", proposal, err)
	}
	if code := a.scan(t.Context(), root, options{name: "web.store"}); code != 2 {
		t.Errorf("--name web.store: exit code = %d, want 2", code)
	}

	stderr.Reset()
	if code := a.scan(t.Context(), tmp, options{}); code != 1 || !strings.Contains(stderr.String(), "not in a project") {
		t.Errorf("outside a project: exit code %d, stderr %q", code, stderr.String())
	}

	t.Run("tracked", func(t *testing.T) {
		var stdout, stderr strings.Builder
		a := newApp(p, run.NewFake(), &stdout, &stderr) // Fake's ls-files succeeds: git tracks apps/web/.env
		if code := a.scan(t.Context(), root, options{}); code != 0 {
			t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
		}
		got := stdout.String()
		if strings.Contains(got, "set   ") || !strings.Contains(got, "note  apps/web/.env: git tracks it") {
			t.Errorf("a tracked .env gave:\n%s\nwant no set lines and its note", got)
		}
	})
}

// TestApp_ScanNameTaken scans shop under --name shop while shop.test is
// listed for another folder: the scan claims it only when that folder is
// unset or gone, and otherwise says whose it is and offers nothing.
func TestApp_ScanNameTaken(t *testing.T) {
	t.Parallel()

	root, other := writeTree(t, shopTree), writeTree(t, shopTree)
	tests := []struct {
		name string
		root string // shop.test's folder
		want string
	}{
		{name: "another clone, still there", root: other, want: "note  shop.test belongs to " + other + ": give this project another name\n"},
		{name: "a folder that is gone", root: filepath.Join(filepath.Dir(other), "moved"), want: "save  this folder as shop.test's project\n"},
		{name: "no folder", want: "save  this folder as shop.test's project\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := paths.ForTest(t.TempDir())
			if err := store.Save(p.DomainsJSON, []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: tt.root}}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr strings.Builder
			if code := newApp(p, run.NewFake(), &stdout, &stderr).scan(t.Context(), root, options{name: "shop"}); code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
			}
			got := stdout.String()
			if !strings.Contains(got, tt.want) {
				t.Errorf("scan lacks %q:\n%s", tt.want, got)
			}
			if tt.root == other && (strings.Contains(got, "add   ") || strings.Contains(got, "run lodo dev --edit")) {
				t.Errorf("scan offers changes for another project's name:\n%s", got)
			}
		})
	}
}
