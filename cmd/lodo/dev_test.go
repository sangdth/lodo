package main

import (
	"bytes"
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

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
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

// listedShop lists shop.lodo with root as its folder, web's name with HTTPS,
// a name that is off, and another project on web's port.
func listedShop(root string) []store.Domain {
	return []store.Domain{
		{Name: "shop.lodo", Address: "127.0.1.1", Enabled: true, Root: root},
		{Name: "web.shop.lodo", Address: "127.0.1.1", Port: 3001, Enabled: true, HTTPS: true},
		{Name: "docs.shop.lodo", Address: "127.0.1.1", Port: 3002},
		{Name: "crm.lodo", Address: "127.0.1.2", Port: 3001, Enabled: true},
	}
}

// newDevApp returns an app on temp paths whose stdout and stderr both go to
// out, with every command found in /opt/bin and a form that fails the test.
func newDevApp(t *testing.T) (a app, p paths.Paths, r *run.Fake, out *strings.Builder) {
	t.Helper()
	p, r, out = paths.ForTest(t.TempDir()), run.NewFake(), &strings.Builder{}
	a = newApp(p, r, out, out)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.form = func(context.Context, []store.Domain, string, options) (bool, error) {
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

// setupFake wraps Fake so sudo install writes staged files onto temp paths,
// letting system.Setup satisfy check.Env.Prerequisites in tests.
type setupFake struct {
	*run.Fake
	sudo    string
	install string
}

func (f setupFake) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == f.sudo && len(args) == 10 && args[0] == "-n" && args[1] == f.install {
		if data, err := os.ReadFile(args[8]); err == nil {
			if err := os.MkdirAll(filepath.Dir(args[9]), 0o755); err == nil {
				_ = os.WriteFile(args[9], data, 0o755)
			}
		}
	}
	return f.Fake.Run(ctx, name, args...)
}

func TestApp_Dev(t *testing.T) {
	t.Parallel()

	t.Run("auto runs setup then starts dev", func(t *testing.T) {
		t.Parallel()
		root := writeTree(t, shopTree)
		p, r, out := paths.ForTest(t.TempDir()), run.NewFake(), &strings.Builder{}
		for path, content := range map[string]string{
			p.Brew:       "",
			p.Dnsmasq:    "",
			p.SystemConf: "# dnsmasq\n",
			filepath.Join(p.LaunchAgents, "sh.brew.dnsmasq.plist"): "",
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, label := range system.DnsmasqSystemLabels {
			r.Fail(run.Line(p.Launchctl, "print", "system/"+label), "Could not find service")
		}
		r.Set(run.Line(p.Launchctl, "print", "gui/501/sh.brew.dnsmasq"), "\tstate = running\n\tpid = 42\n")
		var lo0 strings.Builder
		for i := store.OwnFirst; i <= store.OwnLast; i++ {
			lo0.WriteString("\tinet " + store.OwnAddress(i) + " netmask 0xff000000\n")
		}
		r.Set(run.Line(p.Ifconfig, "lo0"), lo0.String())

		a := newApp(p, setupFake{Fake: r, sudo: p.Sudo, install: p.Install}, out, out)
		a.env.RootUID = uint32(os.Getuid())
		a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
		opened := 0
		a.form = func(_ context.Context, _ []store.Domain, dir string, _ options) (bool, error) {
			opened++
			if dir != root {
				t.Errorf("form dir = %q, want %q", dir, root)
			}
			saved := listedShop(root)
			if err := os.MkdirAll(p.ResolverDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, d := range saved {
				if d.Enabled {
					if err := os.WriteFile(filepath.Join(p.ResolverDir, d.Name), []byte(system.ResolverFile()), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			return true, store.Save(p.DomainsJSON, saved)
		}

		if code := a.dev(t.Context(), root, options{}); code != 0 {
			t.Fatalf("exit code = %d, want 0:\n%s", code, out.String())
		}
		if !slices.Contains(r.Calls(), run.Line(p.Sudo, "-v")) {
			t.Errorf("setup did not prompt with sudo -v; calls: %q", r.Calls())
		}
		if !strings.Contains(out.String(), "✓ resolver script installed\n") {
			t.Errorf("output lacks setup progress:\n%s", out.String())
		}
		if opened != 1 {
			t.Errorf("form opened %d times, want once", opened)
		}
		checkStarted(t, r, root, filepath.Join(p.DevDir, "shop.lodo.cjs"))

		// Second run skips setup because SetupDone is now true.
		sudoCalls := slices.Clone(r.Calls())
		if code := a.dev(t.Context(), root, options{}); code != 0 {
			t.Fatalf("second dev exit code = %d, want 0:\n%s", code, out.String())
		}
		sudoCount := 0
		for _, c := range r.Calls() {
			if c == run.Line(p.Sudo, "-v") {
				sudoCount++
			}
		}
		if sudoCount != 1 {
			t.Errorf("sudo -v ran %d times across two dev runs, want once (initial calls: %q)", sudoCount, sudoCalls)
		}
	})

	t.Run("outside a project before setup", func(t *testing.T) {
		t.Parallel()
		a, _, r, out := newDevApp(t)
		if code := a.dev(t.Context(), t.TempDir(), options{}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		if !strings.Contains(out.String(), scan.ErrNoProject.Error()) {
			t.Errorf("output = %q, want ErrNoProject", out.String())
		}
		if len(r.Calls()) > 0 {
			t.Errorf("ran commands outside a project: %q", r.Calls())
		}
	})

	t.Run("setup fails when password canceled", func(t *testing.T) {
		t.Parallel()
		root := writeTree(t, shopTree)
		a, p, r, out := newDevApp(t)
		for _, path := range []string{p.Brew, p.Dnsmasq, p.SystemConf} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		r.Fail(run.Line(p.Sudo, "-v"), "Sorry, try again.")
		if code := a.dev(t.Context(), root, options{}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		if !strings.Contains(out.String(), "✗ admin password") {
			t.Errorf("output lacks setup error:\n%s", out.String())
		}
		if _, err := os.Stat(p.DomainsJSON); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("domains.json written after canceled password: %v", err)
		}
		if dir, _ := r.Replaced(); dir != "" {
			t.Errorf("Replace ran in %s after failed setup", dir)
		}
	})

	t.Run("refuses when prerequisite fails after setup", func(t *testing.T) {
		t.Parallel()
		root := writeTree(t, shopTree)
		a, p, r, out := newDevApp(t)
		setUpFakeMac(t, p, r)
		if code := a.dev(t.Context(), root, options{}); code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		if slices.Contains(r.Calls(), run.Line(p.Sudo, "-v")) {
			t.Errorf("re-ran setup when SetupDone was already true: %q", r.Calls())
		}
		if !strings.Contains(out.String(), "✗ 1 dnsmasq") || !strings.Contains(out.String(), "lodo dev starts once these pass") {
			t.Errorf("output does not list the failing checks:\n%s", out.String())
		}
		if dir, _ := r.Replaced(); dir != "" {
			t.Errorf("Replace ran in %s with failing checks", dir)
		}
	})
}

func TestApp_StartDev(t *testing.T) {
	t.Parallel()

	root := writeTree(t, shopTree)
	a, p, r, out := newDevApp(t)
	if err := a.startDev(t.Context(), filepath.Join(root, "apps", "web"), listedShop(root), options{}); err != nil {
		t.Fatal(err)
	}
	golden.RequireEqual(t, out.String())

	hook := filepath.Join(p.DevDir, "shop.lodo.cjs")
	data, err := os.ReadFile(hook)
	if err != nil || !strings.Contains(string(data), `{"3001":"127.0.1.1"}`) {
		t.Errorf("hook file = %q, %v; want web's port on shop.lodo's address", data, err)
	}
	if _, err := os.Stat(p.BindHook); err != nil {
		t.Errorf("listen hook: %v", err)
	}
	checkStarted(t, r, root, hook)
	_, env := r.Replaced()
	if !slices.Contains(env, "DOCKER_HOST_IP=127.0.1.1") {
		t.Errorf("env lacks DOCKER_HOST_IP=127.0.1.1, shop.lodo's address: %q", env)
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "COMPOSE_PROJECT_NAME=") }) {
		t.Errorf("env sets COMPOSE_PROJECT_NAME outside a worktree: %q", env)
	}
}

// TestApp_DevAutoRealForm runs lodo dev -y through the real tui.Setup form
// and verifies it saves domains.json, applies configs, and starts the app.
func TestApp_DevAutoRealForm(t *testing.T) {
	t.Parallel()

	tree := maps.Clone(shopTree)
	tree["apps/api/package.json"] = `{"scripts": {"dev": "nest start --watch"}}`
	tree["apps/api/.env"] = "PORT=4001\n"
	root := writeTree(t, tree)

	p, r, out := paths.ForTest(t.TempDir()), run.NewFake(), &strings.Builder{}
	setUpFakeMac(t, p, r)
	for path, content := range map[string]string{
		p.Caddy:           "",
		p.SystemCaddyfile: system.RewriteSystemCaddyfile("", p),
		filepath.Join(p.LaunchAgents, "sh.brew.caddy.plist"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.Set(run.Line(p.Launchctl, "print", "gui/501/sh.brew.caddy"), "\tstate = running\n\tpid = 43\n")

	a := newApp(p, r, out, out)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.teaOpts = []tea.ProgramOption{
		tea.WithInput(&bytes.Buffer{}),
		tea.WithOutput(&bytes.Buffer{}),
		tea.WithoutSignals(),
		tea.WithWindowSize(80, 24),
	}
	a.form = a.setupForm // rebind after setting teaOpts on value receiver a

	if err := a.startDev(t.Context(), root, nil, options{auto: true}); err != nil {
		t.Fatalf("startDev(-y): %v\n%s", err, out.String())
	}
	saved, err := store.Load(p.DomainsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Find(saved, "shop.lodo"); !ok {
		t.Errorf("saved = %+v, want shop.lodo", saved)
	}
	if _, ok := store.Find(saved, "web.shop.lodo"); !ok {
		t.Errorf("saved = %+v, want web.shop.lodo", saved)
	}
	if _, ok := store.Find(saved, "api.shop.lodo"); !ok {
		t.Errorf("saved = %+v, want api.shop.lodo", saved)
	}
	checkStarted(t, r, root, filepath.Join(p.DevDir, "shop.lodo.cjs"))

	// Re-running with -y --name custom.domain renames the project and starts on the new hook.
	if err := a.startDev(t.Context(), root, saved, options{auto: true, name: "custom.domain"}); err != nil {
		t.Fatalf("startDev(-y --name custom.domain): %v\n%s", err, out.String())
	}
	saved, err = store.Load(p.DomainsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Find(saved, "web.custom.domain"); !ok {
		t.Errorf("saved = %+v, want web.custom.domain", saved)
	}
	if _, ok := store.Find(saved, "shop.lodo"); ok {
		t.Errorf("saved = %+v, want old shop.lodo removed", saved)
	}
}

func TestApp_StartDevAuto(t *testing.T) {
	t.Parallel()

	for _, auto := range []bool{false, true} {
		root := writeTree(t, shopTree)
		a, p, _, _ := newDevApp(t)
		got := !auto
		a.form = func(_ context.Context, _ []store.Domain, _ string, o options) (bool, error) {
			got = o.auto
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
	const dotenv = "DATABASE_URL=postgres://u:hunter2@shop.lodo:5432/shop\nAPI_URL=http://web.shop.lodo\nSECRET=abc\n"
	tree["apps/web/.env"] = dotenv
	worktree := writeTree(t, tree)
	domains := append(listedShop(main),
		store.Domain{Name: "shop2.lodo", Address: "127.0.1.3", Enabled: true, Root: worktree},
		store.Domain{Name: "web.shop2.lodo", Address: "127.0.1.3", Port: 3001, Enabled: true},
	)

	a, _, r, out := newDevApp(t)
	if err := a.startDev(t.Context(), worktree, domains, options{}); err != nil {
		t.Fatal(err)
	}
	_, env := r.Replaced()
	for _, want := range []string{
		"DOCKER_HOST_IP=127.0.1.3",
		"COMPOSE_PROJECT_NAME=shop2",
		"DATABASE_URL=postgres://u:hunter2@shop2.lodo:5432/shop",
		"API_URL=http://web.shop2.lodo",
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
	domains := []store.Domain{{Name: "shop.lodo", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: root}}
	a, _, _, _ := newDevApp(t)
	_, proposal, ok := tui.NewBackend(a.paths, a.runner).Scan(t.Context(), root, "", domains)
	if !ok {
		t.Fatal("no project")
	}
	env := []string{"PATH=/bin", "DOCKER_HOST_IP=127.0.0.9", "API_URL=http://old"}
	got := projectEnv(env, proposal)
	if want := []string{"PATH=/bin", "DOCKER_HOST_IP=127.0.1.1", "API_URL=http://shop.lodo"}; !slices.Equal(got, want) {
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
				return strings.HasPrefix(kv, "NODE_OPTIONS=") && strings.Contains(kv, "shop.lodo.cjs")
			}
			if slices.ContainsFunc(env, hooked) {
				t.Errorf("lodo exec loaded the listen hook: %q", env)
			}
		})
	}
}

func TestApp_StartDevForm(t *testing.T) {
	t.Parallel()

	errApply := errors.New("update /etc/resolver: sudo: a password is required")
	tests := []struct {
		name      string
		listed    bool  // shop.lodo is listed before lodo dev
		other     bool  // shop.lodo belongs to another clone of shop, still on disk
		edit      bool  // lodo dev --edit
		saved     bool  // the form saves listedShop
		formErr   error // the form's Err
		wantErr   error
		wantOut   string
		wantStart bool
	}{
		{name: "first run, canceled", wantOut: "nothing saved\n"},
		{name: "first run, saved", saved: true, wantOut: "starting: pnpm run dev", wantStart: true},
		{name: "saved, apply failed", saved: true, formErr: errApply, wantErr: errApply},
		{name: "saving failed", formErr: errApply, wantErr: errApply},
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
			a.form = func(_ context.Context, got []store.Domain, dir string, _ options) (bool, error) {
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
				checkStarted(t, r, root, filepath.Join(p.DevDir, "shop.lodo.cjs"))
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
			hook := `--require "` + filepath.Join(p.DevDir, "shop.lodo.cjs") + `"`
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
