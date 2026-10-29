package scan

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/store"
)

func TestFind_Env(t *testing.T) {
	t.Parallel()

	root := project(t, "shop", map[string]string{
		"package.json":             `{"scripts": {"dev": "turbo dev"}}`,
		"pnpm-workspace.yaml":      "packages: [apps/*]\n",
		".env":                     "DATABASE_URL=postgres://localhost:5434/shop\n", // feeds compose: the root is no app
		"apps/web/package.json":    `{"scripts": {"dev": "next dev -p 3303"}}`,
		"apps/web/.env.example":    "API_URL=http://localhost:3000\n",
		"apps/server/package.json": `{"scripts": {"dev": "nest start"}}`,
		"apps/server/.env":         "PORT=3300\n",
		"apps/web/.env": strings.Join([]string{
			`DATABASE_URL="postgres://u:p@LOCALHOST:5434/shop"`,
			"# AUTH_URL=http://localhost:3303",
			"export AUTH_URL=http://127.0.0.1:3303 # auth\r",
			`CERT="line one`,
			`http://localhost:9999"`,
		}, "\n"),
	})
	p, _ := Find(root)
	want := []EnvLine{
		{File: "apps/server/.env", Line: 1, Key: "PORT", Text: "PORT=3300"},
		{File: "apps/web/.env", Line: 1, Key: "DATABASE_URL", Text: `DATABASE_URL="postgres://u:p@LOCALHOST:5434/shop"`},
		{File: "apps/web/.env", Line: 3, Key: "AUTH_URL", Text: "export AUTH_URL=http://127.0.0.1:3303 # auth"},
		{File: "apps/web/.env", Line: 4, Key: "CERT", Text: `CERT="line one`},
	}
	if !slices.Equal(p.Env, want) {
		t.Errorf("Env =\n%+v\nwant\n%+v", p.Env, want)
	}
	if p.Repo != root {
		t.Errorf("Repo = %q, want the root %q", p.Repo, root)
	}

	// The root's .env is an app's when the root is the app.
	root = project(t, "blog", map[string]string{"package.json": `{"scripts": {"dev": "next dev"}}`, ".env": "API_URL=http://localhost:3000\n"})
	p, _ = Find(root)
	if want := []EnvLine{{File: ".env", Line: 1, Key: "API_URL", Text: "API_URL=http://localhost:3000"}}; !slices.Equal(p.Env, want) {
		t.Errorf("root app: Env = %+v, want %+v", p.Env, want)
	}
}

func TestEnvValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text, want string
		ok         bool
	}{
		{text: "A=http://localhost:3000", want: "http://localhost:3000", ok: true},
		{text: "export A = x # a comment", want: "x", ok: true},
		{text: `A="http://localhost:3000" # api`, want: "http://localhost:3000", ok: true},
		{text: "A='x # not a comment'", want: "x # not a comment", ok: true},
		{text: "A=`x`", want: "x", ok: true},
		{text: "A=", want: "", ok: true},
		{text: `A="line one`},           // goes on over more lines
		{text: `A="a\nb"`},              // an escape
		{text: "A=postgres://u:pa$s@x"}, // Next expands $
		{text: "# A=x"},
	}
	for _, tt := range tests {
		if got, ok := envValue(tt.text); got != tt.want || ok != tt.ok {
			t.Errorf("envValue(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRenameHosts(t *testing.T) {
	t.Parallel()

	names := map[string]string{"hugger.test": "hugger9.test", "mail.hugger.test": "mail.hugger9.test", "api.hugger.test": ""}
	tests := []struct {
		in, want string
		missing  []string
	}{
		{in: "postgresql://u:p@hugger.test:5432/db", want: "postgresql://u:p@hugger9.test:5432/db"},
		{in: "hugger.test", want: "hugger9.test"},
		{in: "http://MAIL.hugger.test,http://hugger.test/x", want: "http://mail.hugger9.test,http://hugger9.test/x"},
		{in: "noreply@hugger.test", want: "noreply@hugger9.test"},
		{in: "http://x-hugger.test http://hugger.test.example.com", want: "http://x-hugger.test http://hugger.test.example.com"},
		{in: "http://api.hugger.test", want: "http://api.hugger.test", missing: []string{"api.hugger.test"}},
	}
	for _, tt := range tests {
		if got, missing := renameHosts(tt.in, names); got != tt.want || !slices.Equal(missing, tt.missing) {
			t.Errorf("renameHosts(%q) = %q, %q; want %q, %q", tt.in, got, missing, tt.want, tt.missing)
		}
	}
}

func TestRepoRoot(t *testing.T) {
	t.Parallel()

	main := project(t, "shop", nil)
	worktree := filepath.Join(t.TempDir(), "wt")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")
	relative := filepath.Join(main, "nested")
	write(t, filepath.Join(relative, ".git"), "gitdir: ../.git/worktrees/nested\n")
	submodule := filepath.Join(t.TempDir(), "sub")
	write(t, filepath.Join(submodule, ".git"), "gitdir: "+filepath.Join(main, ".git", "modules", "sub")+"\n")
	plain := t.TempDir()

	for dir, want := range map[string]string{main: main, worktree: main, relative: main, submodule: "", plain: "", "": ""} {
		if got := repoRoot(dir); got != want {
			t.Errorf("repoRoot(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestPropose_Env(t *testing.T) {
	t.Parallel()

	const compose, prod = "/p/shop/compose.yaml", "/p/shop/compose.prod.yaml"
	domains := []store.Domain{
		{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Root: "/p/shop", Compose: compose},
		{Name: "web.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
		{Name: "api.shop.test", Address: "127.0.1.4", Port: 4000, Enabled: true, HTTPS: true},
		{Name: "off.shop.test", Address: "127.0.1.4", Port: 5000},
	}
	bound := map[int]bool{5434: true}
	tests := []struct {
		name   string
		text   string // line 1 of apps/web/.env
		bound  map[int]bool
		linked string // the compose file shop.test links; compose when empty
		value  string // the variable's masked value; empty for none
		notes  []string
	}{
		{name: "a named port", text: "API_URL=http://localhost:3000/v1", value: "http://web.shop.test/v1"},
		{name: "HTTPS", text: "API_URL=http://localhost:4000", value: "https://api.shop.test"},
		{name: "ws", text: "WS_URL=ws://127.0.0.1:4000/socket", value: "wss://api.shop.test/socket"},
		{name: "user info", text: "AUTH=http://u:hunter2@localhost:3000", value: "http://***@web.shop.test"},
		{
			name: "a web port no name has", text: "NEXT_PUBLIC_URL=http://u:hunter2@localhost:3300",
			notes: []string{"apps/web/.env:1 NEXT_PUBLIC_URL: no name of shop.test has port 3300"},
		},
		{name: "https without a port", text: "X=https://localhost"},
		{name: "https on a named port", text: "X=https://localhost:3000"},
		{name: "a bound port", text: "DATABASE_URL=postgres://u:hunter2@localhost:5434/x", bound: bound, value: "postgres://***@shop.test:5434/x"},
		{name: "a port compose doesn't bind", text: "DATABASE_URL=postgres://localhost:5432/x", bound: bound},
		{name: "another compose file runs", text: "DATABASE_URL=postgres://localhost:5434/x", bound: bound, linked: prod},
		{name: "a disabled name", text: "X=http://localhost:5000", notes: []string{"apps/web/.env:1 X: no name of shop.test has port 5000"}},
		{name: "quotes, export and a comment", text: `export API_URL="http://localhost:3000" # api`, value: "http://web.shop.test"},
		{name: "two URLs", text: "ORIGINS=http://localhost:3000,http://localhost:4000", value: "http://web.shop.test,https://api.shop.test"},
		{name: "nothing local", text: "SECRET=abc"},
		{
			name: "a value lodo can't read", text: "DATABASE_URL=postgres://u:pa$s@localhost:5434/x", bound: bound,
			notes: []string{"apps/web/.env:1 DATABASE_URL: lodo can't read a value over more lines or with a \\ or a $, so lodo dev leaves it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			list := slices.Clone(domains)
			if tt.linked != "" {
				list[0].Compose = tt.linked
			}
			p := Project{
				Root: "/p/shop", Name: "shop.test", Compose: []string{compose, prod}, Apps: []App{{Dir: "apps/web", Port: 3000}},
				Env:   []EnvLine{{File: "apps/web/.env", Line: 1, Key: keyRE.FindStringSubmatch(tt.text)[1], Text: tt.text}},
				Bound: tt.bound,
			}
			got := Propose(p, list, "", gone)
			var want []Var
			if tt.value != "" {
				want = []Var{{File: "apps/web/.env", Line: 1, Key: p.Env[0].Key, Value: tt.value}}
			}
			if !slices.EqualFunc(got.Env, want, func(a, b Var) bool {
				return a.File == b.File && a.Line == b.Line && a.Key == b.Key && a.Value == b.Value
			}) {
				t.Errorf("Env = %+v, want %+v", got.Env, want)
			}
			if !slices.Equal(got.Notes, tt.notes) {
				t.Errorf("Notes = %q, want %q", got.Notes, tt.notes)
			}
			if got.CanAccept() {
				t.Errorf("CanAccept = true with only Env %+v: variables need nothing saved", got.Env)
			}
			shown := slices.Clone(got.Notes)
			for _, v := range got.Env {
				shown = append(shown, v.Value)
			}
			for _, s := range shown {
				if strings.Contains(s, "hunter2") {
					t.Errorf("%q shows the password", s)
				}
			}
			if len(got.Env) == 1 {
				if env, want := got.Env[0].Env(), p.Env[0].Key+"="+strings.ReplaceAll(tt.value, "***@", "u:hunter2@"); env != want {
					t.Errorf("Env() = %q, want %q", env, want)
				}
			}
		})
	}
}

func TestPropose_EnvKeys(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{
		{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Root: "/p/shop"},
		{Name: "web.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
		{Name: "admin.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true},
	}
	lines := func(texts ...string) []EnvLine {
		var l []EnvLine
		for i, text := range texts {
			file := []string{"apps/web/.env", "apps/admin/.env"}[i]
			l = append(l, EnvLine{File: file, Line: 1, Key: keyRE.FindStringSubmatch(text)[1], Text: text})
		}
		return l
	}
	tests := []struct {
		name  string
		env   []EnvLine
		vars  []string // Env()
		notes []string
	}{
		{name: "one value in two files", env: lines("API=http://localhost:3000", "API=http://localhost:3000"), vars: []string{"API=http://web.shop.test"}},
		{
			name: "two values", env: lines("API=http://localhost:3000", "API=http://localhost:3001"),
			notes: []string{"API: apps/web/.env and apps/admin/.env set it to different values, and lodo dev gives every app one environment, so it leaves API as each file has it"},
		},
		{
			name: "a value lodo leaves", env: lines("API=http://localhost:3000", "API=https://shop.example.com"),
			notes: []string{"API: apps/web/.env and apps/admin/.env set it to different values, and lodo dev gives every app one environment, so it leaves API as each file has it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Port: 3000}, {Dir: "apps/admin", Port: 3001}}, Env: tt.env}
			got := Propose(p, domains, "", gone)
			var vars []string
			for _, v := range got.Env {
				vars = append(vars, v.Env())
			}
			if !slices.Equal(vars, tt.vars) || !slices.Equal(got.Notes, tt.notes) {
				t.Errorf("Env = %q, Notes = %q; want %q, %q", vars, got.Notes, tt.vars, tt.notes)
			}
		})
	}
}

// TestPropose_Worktree: a .env copied from the main checkout points at the
// worktree's names, and the main checkout's .env at its own.
func TestPropose_Worktree(t *testing.T) {
	t.Parallel()

	main := project(t, "hugger", nil)
	worktree := filepath.Join(t.TempDir(), "t3-71ba597b")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "t3-71ba597b")+"\n")
	other := project(t, "crm", nil) // another repository
	domains := []store.Domain{
		{Name: "hugger.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: main},
		{Name: "mail.hugger.test", Address: "127.0.1.1", Port: 8025, Enabled: true},
		{Name: "api.hugger.test", Address: "127.0.1.1", Port: 4000, Enabled: true},
		{Name: "hugger9.test", Address: "127.0.1.9", Port: 3000, Enabled: true, Root: worktree},
		{Name: "mail.hugger9.test", Address: "127.0.1.9", Port: 8025, Enabled: true},
		{Name: "crm.test", Address: "127.0.1.3", Port: 3000, Enabled: true, Root: other},
	}
	text := []string{
		"DATABASE_URL=postgresql://postgres:hunter2@hugger.test:5432/postgres",
		"SMTP_HOST=hugger.test",
		"MAILPIT_URL=http://mail.hugger.test",
		"BETTER_AUTH_URL=http://hugger.test",
		"API_URL=http://api.hugger.test",
		"CRM_URL=http://crm.test",
	}
	var env []EnvLine
	for i, s := range text {
		env = append(env, EnvLine{File: ".env", Line: i + 1, Key: keyRE.FindStringSubmatch(s)[1], Text: s})
	}

	p := Project{Root: worktree, Name: "hugger9.test", Apps: []App{{Dir: ".", Port: 3000}}, Env: env, Repo: repoRoot(worktree)}
	got := Propose(p, domains, "", gone)
	var vars []string
	for _, v := range got.Env {
		vars = append(vars, v.Env())
	}
	want := []string{
		"DATABASE_URL=postgresql://postgres:hunter2@hugger9.test:5432/postgres",
		"SMTP_HOST=hugger9.test",
		"MAILPIT_URL=http://mail.hugger9.test",
		"BETTER_AUTH_URL=http://hugger9.test",
	}
	if !slices.Equal(vars, want) {
		t.Errorf("worktree: Env =\n%q\nwant\n%q", vars, want)
	}
	if want := []string{".env:5 API_URL: hugger9.test has no name for api.hugger.test, so lodo dev leaves it"}; !slices.Equal(got.Notes, want) {
		t.Errorf("worktree: Notes = %q, want %q", got.Notes, want)
	}

	// The main checkout keeps its own names.
	p = Project{Root: main, Name: "hugger.test", Apps: []App{{Dir: ".", Port: 3000}}, Env: env, Repo: repoRoot(main)}
	if got := Propose(p, domains, "", gone); len(got.Env) != 0 || len(got.Notes) != 0 {
		t.Errorf("main: Env = %+v, Notes = %q; want none", got.Env, got.Notes)
	}

	if _, err := os.Stat(filepath.Join(worktree, ".env")); err == nil {
		t.Error("the scan wrote a .env")
	}
}

func TestMask(t *testing.T) {
	t.Parallel()

	for s, want := range map[string]string{
		"DATABASE_URL=postgres://u:p@ss@localhost:5434/db":                  "DATABASE_URL=postgres://***@localhost:5434/db",
		"REDIS_URL=redis://:pw@127.0.0.1:6379":                              "REDIS_URL=redis://***@127.0.0.1:6379",
		"REPO=https://TOKEN@github.com/o/r":                                 "REPO=https://***@github.com/o/r",
		"PROFILE=https://x.com/@user":                                       "PROFILE=https://x.com/@user",
		"API_URL=http://localhost:3000":                                     "API_URL=http://localhost:3000",
		"A=http://localhost:3000,postgres://admin:p#ss@db.example.com:5432": "A=http://localhost:3000,postgres://***@db.example.com:5432",
		"A=postgres://u:p?w@db":                                             "A=postgres://***@db",
		`A=postgres://u:p\w@db`:                                             "A=postgres://***@db",
	} {
		if got := mask(s); got != want {
			t.Errorf("mask(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestPropose_WorktreeSlot(t *testing.T) {
	t.Parallel()

	main := project(t, "hugger", nil)
	worktree := filepath.Join(t.TempDir(), "t3-new")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "t3-new")+"\n")
	live := project(t, "t3-live", nil)
	goneDir := filepath.Join(t.TempDir(), "t3-deleted")
	base := []store.Domain{
		{Name: "hugger.test", Address: "127.0.1.1", Port: 3000, Enabled: true, HTTPS: true, Root: main},
		{Name: "mail.hugger.test", Address: "127.0.1.1", Port: 8025, Enabled: true},
		{Name: "crm.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
	}
	slot3 := []store.Domain{
		{Name: "hugger3.test", Address: "127.0.1.3", Port: 3000, Enabled: true, HTTPS: true},
		{Name: "mail.hugger3.test", Address: "127.0.1.3", Port: 8025, Enabled: true},
	}
	tests := []struct {
		name      string
		root3     string // hugger3.test's folder; no hugger3.test when empty
		wantName  string
		wantAddr  string
		wantClaim bool
	}{
		{name: "a deleted worktree's slot", root3: goneDir, wantName: "hugger3.test", wantAddr: "127.0.1.3", wantClaim: true},
		{name: "a live worktree's slot", root3: live, wantName: "hugger4.test", wantAddr: "127.0.1.4"},
		{name: "the lowest free address", wantName: "hugger3.test", wantAddr: "127.0.1.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := slices.Clone(base)
			if tt.root3 != "" {
				s := slices.Clone(slot3)
				s[0].Root = tt.root3
				domains = append(domains, s...)
			}
			p := Project{
				Root: worktree, Name: "t3-new.test", Apps: []App{{Dir: ".", Port: 3000}}, Repo: repoRoot(worktree),
				Compose: []string{filepath.Join(worktree, "compose.yaml")}, Pages: []Page{{Label: "mail", Port: 8025}},
			}
			got := Propose(p, domains, "", gone)
			if got.Name != tt.wantName || got.Address != tt.wantAddr || got.Claim != tt.wantClaim || !got.Worktree || got.Main != "hugger.test" {
				t.Fatalf("Propose = %s on %s, claim %v, worktree %v, main %q; want %s on %s, claim %v, a worktree of hugger.test",
					got.Name, got.Address, got.Claim, got.Worktree, got.Main, tt.wantName, tt.wantAddr, tt.wantClaim)
			}
			if tt.wantClaim {
				if len(got.Add) != 0 || !slices.ContainsFunc(got.Notes, func(n string) bool { return strings.Contains(n, "takes over a gone folder's name") }) {
					t.Errorf("Add %+v, Notes %q; want nothing added and the takeover note", got.Add, got.Notes)
				}
				return
			}
			want := []store.Domain{
				{Name: tt.wantName, Address: tt.wantAddr, Port: 3000, Enabled: true, HTTPS: true, Root: worktree}, // HTTPS like hugger.test
				{Name: "mail." + tt.wantName, Address: tt.wantAddr, Port: 8025, Enabled: true},
			}
			if !slices.Equal(got.Add, want) {
				t.Errorf("Add =\n%+v\nwant\n%+v", got.Add, want)
			}
		})
	}

	// A worktree whose main checkout has no name keeps the folder's name.
	p := Project{Root: worktree, Name: "t3-new.test", Apps: []App{{Dir: ".", Port: 3000}}, Repo: repoRoot(worktree)}
	if got := Propose(p, base[2:], "", gone); got.Name != "t3-new.test" || !got.Worktree || got.Main != "" {
		t.Errorf("no main: Propose = %s, worktree %v, main %q; want t3-new.test, a worktree with no main", got.Name, got.Worktree, got.Main)
	}
}
