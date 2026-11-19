package main

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sangdth/lodo/internal/dev"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

// setUpPruneMac writes the files SetupDone and CaddySetUp check so Apply runs
// against a fake runner without touching the real Mac.
func setUpPruneMac(t *testing.T) (paths.Paths, *run.Fake) {
	t.Helper()
	p := paths.ForTest(t.TempDir())
	for path, content := range map[string]string{
		p.Script:          "# lodo\n",
		p.SystemConf:      system.ConfBlock(p),
		p.SystemCaddyfile: system.CaddyBlock(p),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p, run.NewFake()
}

func makeWorktree(t *testing.T, main, name string) string {
	t.Helper()
	tree := maps.Clone(shopTree)
	delete(tree, ".git/HEAD")
	tree[".git"] = "gitdir: " + filepath.Join(main, ".git", "worktrees", name) + "\n"
	return writeTree(t, tree)
}

func TestApp_Prune(t *testing.T) {
	t.Parallel()

	main := writeTree(t, shopTree)
	liveWT := makeWorktree(t, main, "wt-live")
	activeWT := makeWorktree(t, main, "wt-active")
	goneWT := filepath.Join(t.TempDir(), "wt-gone")

	initial := []store.Domain{
		{Name: "shop.lodo", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: main},
		{Name: "shop2.lodo", Address: "127.0.1.2", Enabled: true, Root: goneWT},
		{Name: "web.shop2.lodo", Address: "127.0.1.2", Port: 3001, Enabled: true},
		{Name: "shop3.lodo", Address: "127.0.1.3", Enabled: true, Root: liveWT},
		{Name: "web.shop3.lodo", Address: "127.0.1.3", Port: 3001, Enabled: true},
		{Name: "shop4.lodo", Address: "127.0.1.4", Enabled: true, Root: activeWT},
		{Name: "web.shop4.lodo", Address: "127.0.1.4", Port: 3001, Enabled: true},
		{Name: "manual.lodo", Address: "127.0.1.5", Enabled: true},
	}

	tests := []struct {
		name         string
		dir          string
		worktree     bool
		domains      []store.Domain
		wantOut      string
		wantLeft     []string
		wantRemoved  []string // projects whose .cjs hook files must be deleted
		wantNextFree string
	}{
		{
			name:         "bare prune removes only gone folder and its subdomains",
			dir:          activeWT,
			domains:      initial,
			wantOut:      "removed shop2.lodo (" + goneWT + ")\nremoved web.shop2.lodo\n",
			wantLeft:     []string{"manual.lodo", "shop.lodo", "shop3.lodo", "web.shop3.lodo", "shop4.lodo", "web.shop4.lodo"},
			wantRemoved:  []string{"shop2.lodo"},
			wantNextFree: "127.0.1.2",
		},
		{
			name:         "--worktree in a worktree subfolder removes both gone folder and active worktree",
			dir:          filepath.Join(activeWT, "apps", "web"),
			worktree:     true,
			domains:      initial,
			wantOut:      "removed shop2.lodo (" + goneWT + ")\nremoved web.shop2.lodo\nremoved shop4.lodo (" + activeWT + ")\nremoved web.shop4.lodo\n",
			wantLeft:     []string{"manual.lodo", "shop.lodo", "shop3.lodo", "web.shop3.lodo"},
			wantRemoved:  []string{"shop2.lodo", "shop4.lodo"},
			wantNextFree: "127.0.1.2",
		},
		{
			name:         "--worktree in the main checkout keeps the main checkout",
			dir:          main,
			worktree:     true,
			domains:      initial,
			wantOut:      "removed shop2.lodo (" + goneWT + ")\nremoved web.shop2.lodo\n",
			wantLeft:     []string{"manual.lodo", "shop.lodo", "shop3.lodo", "web.shop3.lodo", "shop4.lodo", "web.shop4.lodo"},
			wantRemoved:  []string{"shop2.lodo"},
			wantNextFree: "127.0.1.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, r := setUpPruneMac(t)
			if err := store.Save(p.DomainsJSON, tt.domains); err != nil {
				t.Fatal(err)
			}
			for _, project := range []string{"shop.lodo", "shop2.lodo", "shop3.lodo", "shop4.lodo"} {
				if _, err := dev.WriteHooks(p, project, map[int]string{3001: "127.0.1.1"}); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr strings.Builder
			code := newApp(p, r, &stdout, &stderr).prune(t.Context(), tt.dir, tt.worktree)
			if code != 0 || stderr.Len() > 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if stdout.String() != tt.wantOut {
				t.Errorf("stdout =\n%q\nwant\n%q", stdout.String(), tt.wantOut)
			}

			saved, err := store.Load(p.DomainsJSON)
			if err != nil {
				t.Fatal(err)
			}
			var gotNames []string
			for _, d := range saved {
				gotNames = append(gotNames, d.Name)
			}
			if !slices.Equal(gotNames, tt.wantLeft) {
				t.Errorf("left domains = %q, want %q", gotNames, tt.wantLeft)
			}
			if free, err := store.NextFree(saved); err != nil || free != tt.wantNextFree {
				t.Errorf("NextFree = %q, %v; want %q", free, err, tt.wantNextFree)
			}
			for _, project := range tt.wantRemoved {
				hook := filepath.Join(p.DevDir, project+".cjs")
				if _, err := os.Stat(hook); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("hook %s still exists: %v", hook, err)
				}
			}
			if _, err := os.Stat(filepath.Join(p.DevDir, "shop.lodo.cjs")); err != nil {
				t.Errorf("kept project's hook removed: %v", err)
			}
		})
	}
}

func TestApp_PruneNoOp(t *testing.T) {
	t.Parallel()

	main := writeTree(t, shopTree)
	// Before setup and with no domains.json, prune is a clean no-op and creates no config folder.
	p := paths.ForTest(t.TempDir())
	r := run.NewFake()
	var stdout, stderr strings.Builder
	if code := newApp(p, r, &stdout, &stderr).prune(t.Context(), main, true); code != 0 || stdout.String() != "nothing to prune\n" {
		t.Fatalf("missing domains.json: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(p.ConfigDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("prune created %s when domains.json did not exist: %v", p.ConfigDir, err)
	}

	// With only live and manual domains, prune touches nothing and runs no external commands.
	if err := store.Save(p.DomainsJSON, []store.Domain{
		{Name: "shop.lodo", Address: "127.0.1.1", Enabled: true, Root: main},
		{Name: "manual.lodo", Address: "127.0.1.2", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := newApp(p, r, &stdout, &stderr).prune(t.Context(), main, true); code != 0 || stdout.String() != "nothing to prune\n" {
		t.Fatalf("nothing gone: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if len(r.Calls()) > 0 {
		t.Errorf("no-op prune ran commands: %q", r.Calls())
	}

	// When a domain needs pruning before lodo setup, prune fails with ErrNotSetUp and leaves domains.json intact.
	if err := store.Save(p.DomainsJSON, []store.Domain{
		{Name: "gone.lodo", Address: "127.0.1.1", Enabled: true, Root: filepath.Join(t.TempDir(), "missing")},
	}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := newApp(p, r, &stdout, &stderr).prune(t.Context(), main, false); code != 1 || !strings.Contains(stderr.String(), system.ErrNotSetUp.Error()) {
		t.Fatalf("before setup: code %d, stderr %q; want 1 and ErrNotSetUp", code, stderr.String())
	}
	if saved, err := store.Load(p.DomainsJSON); err != nil || len(saved) != 1 {
		t.Errorf("domains.json changed before setup: %+v, %v", saved, err)
	}
}

func TestApp_PruneConcurrent(t *testing.T) {
	t.Parallel()

	p, r := setUpPruneMac(t)
	main := writeTree(t, shopTree)
	wt1 := makeWorktree(t, main, "wt1")
	wt2 := makeWorktree(t, main, "wt2")
	if err := store.Save(p.DomainsJSON, []store.Domain{
		{Name: "shop.lodo", Address: "127.0.1.1", Enabled: true, Root: main},
		{Name: "shop2.lodo", Address: "127.0.1.2", Enabled: true, Root: wt1},
		{Name: "shop3.lodo", Address: "127.0.1.3", Enabled: true, Root: wt2},
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, wt := range []string{wt1, wt2} {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			var out, errOut strings.Builder
			if code := newApp(p, r, &out, &errOut).prune(t.Context(), dir, true); code != 0 {
				t.Errorf("prune(%s) = %d, stderr %q", dir, code, errOut.String())
			}
		}(wt)
	}
	wg.Wait()

	saved, err := store.Load(p.DomainsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Name != "shop.lodo" {
		t.Errorf("saved after concurrent prunes = %+v, want only shop.lodo", saved)
	}
}
