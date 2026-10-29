package main

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/tui"
)

// shopTree is a pnpm project with two apps: web's dev server takes port 3001,
// and api has no name in listedShop.
var shopTree = map[string]string{
	".git/HEAD":             "ref: refs/heads/master\n",
	"package.json":          `{"scripts": {"dev": "turbo dev"}}`,
	"pnpm-lock.yaml":        "",
	"pnpm-workspace.yaml":   "packages: [apps/*]\n",
	"apps/web/package.json": `{"scripts": {"dev": "next dev -p 3001"}}`,
	"apps/api/package.json": `{"scripts": {"dev": "nest start --watch"}}`,
}

// listedShop lists shop.test with root as its folder, web's name with HTTPS,
// a name that is off, and another project on web's port.
func listedShop(root string) []store.Domain {
	return []store.Domain{
		{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: root},
		{Name: "web.shop.test", Address: "127.0.1.1", Port: 3001, Enabled: true, HTTPS: true},
		{Name: "docs.shop.test", Address: "127.0.1.1", Port: 3002},
		{Name: "crm.test", Address: "127.0.1.2", Port: 3001, Enabled: true},
	}
}

// newDevApp returns an app on temp paths whose stdout and stderr both go to
// out, with every command found in /opt/bin and a form that fails the test.
func newDevApp(t *testing.T) (a app, p paths.Paths, r *run.Fake, out *strings.Builder) {
	t.Helper()
	p, r, out = paths.ForTest(t.TempDir()), run.NewFake(), &strings.Builder{}
	a = newApp(p, r, out, out)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.form = func(context.Context, []store.Domain, string, bool) (bool, error) {
		t.Error("the setup form opened")
		return false, nil
	}
	return a, p, r, out
}

// writeTree writes files into a new project folder called shop and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "shop")
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// checkStarted fails t unless lodo replaced itself exactly once with pnpm run
// dev in root, with NODE_OPTIONS requiring hook.
func checkStarted(t *testing.T, r *run.Fake, root, hook string) {
	t.Helper()
	line, n := run.Line("/opt/bin/pnpm", "run", "dev"), 0
	for _, c := range r.Calls() {
		if c == line {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%q ran %d times, want once; calls: %q", line, n, r.Calls())
	}
	dir, env := r.Replaced()
	if dir != root {
		t.Errorf("Replace ran in %q, want the project root %q", dir, root)
	}
	var options []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "NODE_OPTIONS="); ok {
			options = append(options, v)
		}
	}
	if len(options) != 1 || !strings.Contains(options[0], `--require "`+hook+`"`) {
		t.Errorf("NODE_OPTIONS = %q, want one that requires %s", options, hook)
	}
}

func TestApp_DevRefusesBeforeSetup(t *testing.T) {
	t.Parallel()

	a, _, r, out := newDevApp(t)
	if code := a.command(t.Context(), "dev", options{}); code != 1 {
		t.Errorf("exit code = %d on a Mac with nothing set up, want 1", code)
	}
	if !strings.Contains(out.String(), "✗ 1 dnsmasq") || !strings.Contains(out.String(), "lodo dev starts once these pass") {
		t.Errorf("output does not list the failing checks:\n%s", out.String())
	}
	if dir, _ := r.Replaced(); dir != "" {
		t.Errorf("Replace ran in %s before setup", dir)
	}
}

func TestApp_StartDev(t *testing.T) {
	t.Parallel()

	root := writeTree(t, shopTree)
	a, p, r, out := newDevApp(t)
	if err := a.startDev(t.Context(), filepath.Join(root, "apps", "web"), listedShop(root), options{}); err != nil {
		t.Fatal(err)
	}
	golden.RequireEqual(t, out.String())

	hook := filepath.Join(p.DevDir, "shop.test.cjs")
	data, err := os.ReadFile(hook)
	if err != nil || !strings.Contains(string(data), `{"3001":"127.0.1.1"}`) {
		t.Errorf("hook file = %q, %v; want web's port on shop.test's address", data, err)
	}
	if _, err := os.Stat(p.BindHook); err != nil {
		t.Errorf("listen hook: %v", err)
	}
	checkStarted(t, r, root, hook)
	_, env := r.Replaced()
	if !slices.Contains(env, "DOCKER_HOST_IP=127.0.1.1") {
		t.Errorf("env lacks DOCKER_HOST_IP=127.0.1.1, shop.test's address: %q", env)
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "COMPOSE_PROJECT_NAME=") }) {
		t.Errorf("env sets COMPOSE_PROJECT_NAME outside a worktree: %q", env)
	}
}

func TestApp_StartDevAuto(t *testing.T) {
	t.Parallel()

	for _, auto := range []bool{false, true} {
		root := writeTree(t, shopTree)
		a, p, _, _ := newDevApp(t)
		got := !auto
		a.form = func(_ context.Context, _ []store.Domain, _ string, a bool) (bool, error) {
			got = a
			return true, store.Save(p.DomainsJSON, listedShop(root))
		}
		if err := a.startDev(t.Context(), root, nil, options{auto: auto}); err != nil {
			t.Fatal(err)
		}
		if got != auto {
			t.Errorf("auto %v: the form got auto %v", auto, got)
		}
	}
}

// TestApp_StartDevWorktree starts a git worktree of shop whose .env, copied
// from the main checkout, names the main checkout's names: the dev script
// gets the worktree's, and the .env stays as it is.
func TestApp_StartDevWorktree(t *testing.T) {
	t.Parallel()

	main := writeTree(t, shopTree)
	tree := maps.Clone(shopTree)
	delete(tree, ".git/HEAD")
	tree[".git"] = "gitdir: " + filepath.Join(main, ".git", "worktrees", "wt") + "\n"
	const dotenv = "DATABASE_URL=postgres://u:hunter2@shop.test:5432/shop\nAPI_URL=http://web.shop.test\nSECRET=abc\n"
	tree["apps/web/.env"] = dotenv
	worktree := writeTree(t, tree)
	domains := append(listedShop(main),
		store.Domain{Name: "shop2.test", Address: "127.0.1.3", Enabled: true, Root: worktree},
		store.Domain{Name: "web.shop2.test", Address: "127.0.1.3", Port: 3001, Enabled: true},
	)

	a, _, r, out := newDevApp(t)
	if err := a.startDev(t.Context(), worktree, domains, options{}); err != nil {
		t.Fatal(err)
	}
	_, env := r.Replaced()
	for _, want := range []string{
		"DOCKER_HOST_IP=127.0.1.3",
		"COMPOSE_PROJECT_NAME=shop2",
		"DATABASE_URL=postgres://u:hunter2@shop2.test:5432/shop",
		"API_URL=http://web.shop2.test",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("lodo dev printed the password:\n%s", out.String())
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "apps", "web", ".env")); err != nil || string(data) != dotenv {
		t.Errorf(".env = %q, %v; want it as it was", data, err)
	}
}

func TestProjectEnv(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{
		".git/HEAD":    "ref: refs/heads/master\n",
		"package.json": `{"scripts": {"dev": "next dev"}}`,
		".env":         "API_URL=http://localhost:3000\n",
	})
	domains := []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: root}}
	a, _, _, _ := newDevApp(t)
	_, proposal, ok := tui.NewBackend(a.paths, a.runner).Scan(t.Context(), root, "", domains)
	if !ok {
		t.Fatal("no project")
	}
	env := []string{"PATH=/bin", "DOCKER_HOST_IP=127.0.0.9", "API_URL=http://old"}
	got := projectEnv(env, proposal)
	if want := []string{"PATH=/bin", "DOCKER_HOST_IP=127.0.1.1", "API_URL=http://shop.test"}; !slices.Equal(got, want) {
		t.Errorf("projectEnv = %q, want %q", got, want)
	}
	if env[1] != "DOCKER_HOST_IP=127.0.0.9" {
		t.Errorf("projectEnv changed its input: %q", env)
	}
}

func TestApp_StartExec(t *testing.T) {
	t.Parallel()

	root := writeTree(t, shopTree)
	web := filepath.Join(root, "apps", "web")
	tests := []struct {
		name    string
		domains []store.Domain
		err     string
	}{
		{name: "listed", domains: listedShop(root)},
		{name: "no names yet", err: "has no names yet: run lodo dev there first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, _, r, _ := newDevApp(t)
			err := a.startExec(t.Context(), web, tt.domains, []string{"pnpm", "db:migrate"})
			dir, env := r.Replaced()
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || dir != "" {
					t.Errorf("err %v, Replace in %q; want %q and nothing run", err, dir, tt.err)
				}
				return
			}
			if err != nil || dir != web || !slices.Contains(r.Calls(), run.Line("/opt/bin/pnpm", "db:migrate")) {
				t.Errorf("err %v, Replace in %q, calls %q; want pnpm db:migrate in %s", err, dir, r.Calls(), web)
			}
			if !slices.Contains(env, "DOCKER_HOST_IP=127.0.1.1") {
				t.Errorf("env lacks DOCKER_HOST_IP=127.0.1.1: %q", env)
			}
			hooked := func(kv string) bool {
				return strings.HasPrefix(kv, "NODE_OPTIONS=") && strings.Contains(kv, "shop.test.cjs")
			}
			if slices.ContainsFunc(env, hooked) {
				t.Errorf("lodo exec loaded the listen hook: %q", env)
			}
		})
	}
}

func TestApp_StartDevForm(t *testing.T) {
	t.Parallel()

	errEnv := errors.New("set apps/web/.env: permission denied")
	tests := []struct {
		name      string
		listed    bool  // shop.test is listed before lodo dev
		other     bool  // shop.test belongs to another clone of shop, still on disk
		edit      bool  // lodo dev --edit
		saved     bool  // the form saves listedShop
		formErr   error // the form's Err
		wantErr   error
		wantOut   string
		wantStart bool
	}{
		{name: "first run, canceled", wantOut: "nothing saved\n"},
		{name: "first run, saved", saved: true, wantOut: "starting: pnpm run dev", wantStart: true},
		{name: "saved, a .env line failed", saved: true, formErr: errEnv, wantOut: "✗ " + errEnv.Error(), wantStart: true},
		{name: "saving failed", formErr: errEnv, wantErr: errEnv},
		{name: "--edit, canceled", listed: true, edit: true, wantOut: "nothing saved\n"},
		{name: "another clone's name, canceled", other: true, wantOut: "nothing saved\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, shopTree)
			a, p, r, out := newDevApp(t)
			var domains []store.Domain
			if tt.listed {
				domains = listedShop(root)
			}
			if tt.other {
				domains = listedShop(writeTree(t, shopTree))
			}
			opened := 0
			a.form = func(_ context.Context, got []store.Domain, dir string, _ bool) (bool, error) {
				opened++
				if len(got) != len(domains) || dir != root {
					t.Errorf("form got %d names in %s, want %d in %s", len(got), dir, len(domains), root)
				}
				if tt.saved {
					if err := store.Save(p.DomainsJSON, listedShop(root)); err != nil {
						t.Fatal(err)
					}
				}
				return tt.saved, tt.formErr
			}
			err := a.startDev(t.Context(), root, domains, options{edit: tt.edit})
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if opened != 1 {
				t.Errorf("the form opened %d times, want once", opened)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output lacks %q:\n%s", tt.wantOut, out.String())
			}
			if tt.wantStart {
				checkStarted(t, r, root, filepath.Join(p.DevDir, "shop.test.cjs"))
			} else if dir, _ := r.Replaced(); dir != "" {
				t.Errorf("Replace ran in %s", dir)
			}
		})
	}
}

// TestApp_StartDevNodeOptions checks that npm's and pnpm's node-options
// setting, which replaces a script's NODE_OPTIONS, keeps the hook: the
// setting goes first, and the variable that beats it holds the same options.
func TestApp_StartDevNodeOptions(t *testing.T) {
	t.Parallel()

	npmTree := maps.Clone(shopTree)
	delete(npmTree, "pnpm-lock.yaml")
	npmTree["package-lock.json"] = "{}"
	tests := []struct {
		name    string
		files   map[string]string
		pm      string
		flag    string // the flag that points pm's config at the project
		setting string // what pm config get node-options prints
		want    string // how the options start; empty when the setting is unset
	}{
		{name: "pnpm, set", files: shopTree, pm: "pnpm", flag: "--dir", setting: "--max-old-space-size=4096\n", want: "--max-old-space-size=4096 "},
		{name: "pnpm, unset", files: shopTree, pm: "pnpm", flag: "--dir", setting: "undefined\n"},
		{name: "npm, set", files: npmTree, pm: "npm", flag: "--prefix", setting: "--max-old-space-size=2048\n", want: "--max-old-space-size=2048 "},
		{name: "npm, unset", files: npmTree, pm: "npm", flag: "--prefix", setting: "null\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := writeTree(t, tt.files)
			a, p, r, _ := newDevApp(t)
			r.Set(run.Line("/opt/bin/"+tt.pm, tt.flag, root, "config", "get", "node-options"), tt.setting)
			if err := a.startDev(t.Context(), root, listedShop(root), options{}); err != nil {
				t.Fatal(err)
			}
			_, env := r.Replaced()
			got := map[string][]string{}
			for _, kv := range env {
				if k, v, ok := strings.Cut(kv, "="); ok && (k == "NODE_OPTIONS" || k == tt.pm+"_config_node_options") {
					got[k] = append(got[k], v)
				}
			}
			options, configVar := got["NODE_OPTIONS"], got[tt.pm+"_config_node_options"]
			hook := `--require "` + filepath.Join(p.DevDir, "shop.test.cjs") + `"`
			if len(options) != 1 || !slices.Equal(configVar, options) || !strings.Contains(options[0], hook) ||
				!strings.HasPrefix(options[0], tt.want) || strings.Contains(options[0], "null") || strings.Contains(options[0], "undefined") {
				t.Errorf("NODE_OPTIONS = %q, %s_config_node_options = %q; want one value in both, starting with %q and requiring the hook",
					options, tt.pm, configVar, tt.want)
			}
		})
	}
}

func TestApp_StartDevNodeOptionsError(t *testing.T) {
	t.Parallel()

	root := writeTree(t, shopTree)
	a, _, r, _ := newDevApp(t)
	r.Fail(run.Line("/opt/bin/pnpm", "--dir", root, "config", "get", "node-options"), "ERR_PNPM_BAD_CONFIG")
	err := a.startDev(t.Context(), root, listedShop(root), options{})
	if err == nil || !strings.Contains(err.Error(), "read pnpm's node-options: ") {
		t.Errorf("err = %v, want why pnpm's node-options can't be read", err)
	}
	if dir, _ := r.Replaced(); dir != "" {
		t.Errorf("Replace ran in %s", dir)
	}
}

func TestApp_StartDevErrors(t *testing.T) {
	t.Parallel()

	noDev := map[string]string{".git/HEAD": "", "package.json": `{"scripts": {"lint": "biome", "build": "tsc"}}`}
	tests := []struct {
		name     string
		files    map[string]string // nil for a folder outside a project
		notFound bool              // the package manager isn't on PATH
		want     string
	}{
		{name: "outside a project", want: scan.ErrNoProject.Error()},
		{name: "no package.json", files: map[string]string{".git/HEAD": "", "apps/web/package.json": "{}"}, want: "has no package.json"},
		{name: "no dev script", files: noDev, want: "package.json has no dev script, only build, lint"},
		{name: "pnpm isn't installed", files: shopTree, notFound: true, want: "find pnpm: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.files != nil {
				root = writeTree(t, tt.files)
			}
			a, p, r, _ := newDevApp(t)
			if tt.notFound {
				a.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			}
			err := a.startDev(t.Context(), root, listedShop(root), options{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
			if _, err := os.Stat(p.DevDir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("hooks written before the error: %v", err)
			}
			if dir, _ := r.Replaced(); dir != "" {
				t.Errorf("Replace ran in %s", dir)
			}
		})
	}
}
