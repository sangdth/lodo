package scan

import (
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/store"
)

func TestFind_Env(t *testing.T) {
	t.Parallel()

	root := project(t, "shop", map[string]string{
		"package.json":              `{"scripts": {"dev": "turbo dev"}}`,
		"pnpm-workspace.yaml":       "packages: [apps/*]\n",
		".env":                      "DATABASE_URL=postgres://localhost:5434/shop\n", // feeds compose: the root is no app
		"apps/web/package.json":     `{"scripts": {"dev": "next dev -p 3303"}}`,
		"apps/web/.env.example":     "API_URL=http://localhost:3000\n",
		"apps/landing/package.json": `{"scripts": {"dev": "next dev -p 3302"}}`,
		"apps/landing/.env":         "DASHBOARD_URL=http://localhost:3303\n",
		"apps/server/package.json":  `{"scripts": {"dev": "nest start"}}`,
		"apps/server/.env":          "PORT=3300\n",
		"apps/web/.env": strings.Join([]string{
			`DATABASE_URL="postgres://u:p@LOCALHOST:5434/shop"`,
			"# AUTH_URL=http://localhost:3303",
			"export AUTH_URL=http://127.0.0.1:3303 # auth\r",
			"SECRET=abc",
			`CERT="line one`,
			`http://localhost:9999"`,
		}, "\n"),
	})
	var asked []string
	p, _ := Find(root, func(path string) bool {
		asked = append(asked, path)
		return strings.HasSuffix(path, "/apps/landing/.env")
	})
	want := []EnvLine{
		{File: "apps/web/.env", Line: 1, Key: "DATABASE_URL", Text: `DATABASE_URL="postgres://u:p@LOCALHOST:5434/shop"`},
		{File: "apps/web/.env", Line: 3, Key: "AUTH_URL", Text: "export AUTH_URL=http://127.0.0.1:3303 # auth"},
	}
	if !slices.Equal(p.Env, want) {
		t.Errorf("Env =\n%+v\nwant\n%+v", p.Env, want)
	}
	if !slices.Equal(p.Shared, []string{"apps/landing/.env"}) {
		t.Errorf("Shared = %q, want apps/landing/.env", p.Shared)
	}
	if len(asked) != 2 || !strings.HasSuffix(asked[0], "/apps/landing/.env") || !strings.HasSuffix(asked[1], "/apps/web/.env") {
		t.Errorf("tracked asked about %q, want the two files with a localhost line", asked)
	}

	// The root's .env is an app's when the root is the app.
	root = project(t, "blog", map[string]string{"package.json": `{"scripts": {"dev": "next dev"}}`, ".env": "API_URL=http://localhost:3000\n"})
	p, _ = Find(root, nil)
	if want := []EnvLine{{File: ".env", Line: 1, Key: "API_URL", Text: "API_URL=http://localhost:3000"}}; !slices.Equal(p.Env, want) {
		t.Errorf("root app: Env = %+v, want %+v", p.Env, want)
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
		name     string
		text     string // line 1 of apps/web/.env
		bound    map[int]bool
		address  string // ComposeAddress
		linked   string // the compose file shop.test links; compose when empty
		old, new string // the fix; empty for none
		notes    []string
	}{
		{name: "a named port", text: "API_URL=http://localhost:3000/v1", old: "API_URL=http://localhost:3000/v1", new: "API_URL=http://web.shop.test/v1"},
		{name: "HTTPS", text: "API_URL=http://localhost:4000", old: "API_URL=http://localhost:4000", new: "API_URL=https://api.shop.test"},
		{name: "ws", text: "WS_URL=ws://127.0.0.1:4000/socket", old: "WS_URL=ws://127.0.0.1:4000/socket", new: "WS_URL=wss://api.shop.test/socket"},
		{name: "user info", text: "AUTH=http://u:hunter2@localhost:3000", old: "AUTH=http://***@localhost:3000", new: "AUTH=http://***@web.shop.test"},
		{
			name: "a web port no name has", text: "NEXT_PUBLIC_URL=http://u:hunter2@localhost:3300",
			notes: []string{"apps/web/.env:1 NEXT_PUBLIC_URL: no name of shop.test has port 3300"},
		},
		{name: "https without a port", text: "X=https://localhost"},
		{name: "https on a named port", text: "X=https://localhost:3000"},
		{
			name: "a bound port", text: "DATABASE_URL=postgres://u:hunter2@localhost:5434/x", bound: bound, address: "127.0.1.4",
			old: "DATABASE_URL=postgres://***@localhost:5434/x", new: "DATABASE_URL=postgres://***@shop.test:5434/x",
		},
		{name: "compose runs with another address", text: "DATABASE_URL=postgres://localhost:5434/x", bound: bound, address: "127.0.1.9"},
		{name: "a port compose doesn't bind", text: "DATABASE_URL=postgres://localhost:5432/x", bound: bound, address: "127.0.1.4"},
		{name: "another compose file runs", text: "DATABASE_URL=postgres://localhost:5434/x", bound: bound, address: "127.0.1.4", linked: prod},
		{name: "a disabled name", text: "X=http://localhost:5000", notes: []string{"apps/web/.env:1 X: no name of shop.test has port 5000"}},
		{
			name: "quotes, export and a comment stay", text: `export API_URL="http://localhost:3000" # api`,
			old: `export API_URL="http://localhost:3000" # api`, new: `export API_URL="http://web.shop.test" # api`,
		},
		{
			name: "two URLs, one fix", text: "ORIGINS=http://localhost:3000,http://localhost:4000",
			old: "ORIGINS=http://localhost:3000,http://localhost:4000", new: "ORIGINS=http://web.shop.test,https://api.shop.test",
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
				Bound: tt.bound, ComposeAddress: tt.address,
			}
			got := Propose(p, list, "", gone)
			var want []Fix
			if tt.new != "" {
				want = []Fix{{File: "apps/web/.env", Line: 1, Old: tt.old, New: tt.new}}
			}
			if !slices.EqualFunc(got.Env, want, func(a, b Fix) bool { return a.File == b.File && a.Line == b.Line && a.Old == b.Old && a.New == b.New }) {
				t.Errorf("Env = %+v, want %+v", got.Env, want)
			}
			if !slices.Equal(got.Notes, tt.notes) {
				t.Errorf("Notes = %q, want %q", got.Notes, tt.notes)
			}
			if got.CanAccept() != (len(want) > 0) {
				t.Errorf("CanAccept = %v with Env %+v", got.CanAccept(), got.Env)
			}
			shown := slices.Clone(got.Notes)
			for _, f := range got.Env {
				shown = append(shown, f.Old, f.New)
			}
			for _, s := range shown {
				if strings.Contains(s, "hunter2") {
					t.Errorf("%q shows the password", s)
				}
			}
			if len(got.Env) == 0 {
				return
			}
			out, err := ApplyEnv([]byte(tt.text+"\nB=2\n"), got.Env)
			if want := strings.ReplaceAll(tt.new, "***@", "u:hunter2@") + "\nB=2\n"; err != nil || string(out) != want {
				t.Errorf("ApplyEnv = %q, %v; want %q", out, err, want)
			}
		})
	}

	got := Propose(Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Port: 3000}}, Shared: []string{"apps/web/.env"}}, domains, "", gone)
	if want := []string{"apps/web/.env: git tracks it, so lodo leaves its localhost URLs"}; !slices.Equal(got.Notes, want) {
		t.Errorf("shared: Notes = %q, want %q", got.Notes, want)
	}
}

func TestApplyEnv(t *testing.T) {
	t.Parallel()

	fix := func(line int, before, after string) Fix {
		return Fix{File: "apps/web/.env", Line: line, Old: before, New: after, before: before, after: after}
	}
	tests := []struct {
		name    string
		content string
		fixes   []Fix
		want    string // empty for an error
		err     string
	}{
		{name: "LF", content: "A=1\nB=old\nC=3\n", fixes: []Fix{fix(2, "B=old", "B=new")}, want: "A=1\nB=new\nC=3\n"},
		{name: "CRLF", content: "A=1\r\nB=old\r\n", fixes: []Fix{fix(2, "B=old", "B=new")}, want: "A=1\r\nB=new\r\n"},
		{name: "no line end at the end", content: "A=1\nB=old", fixes: []Fix{fix(2, "B=old", "B=new")}, want: "A=1\nB=new"},
		{
			name: "two in one file", content: "A=old\nB=old\n", fixes: []Fix{fix(1, "A=old", "A=new"), fix(2, "B=old", "B=new")},
			want: "A=new\nB=new\n",
		},
		{name: "applied already", content: "A=1\nB=new\n", fixes: []Fix{fix(2, "B=old", "B=new")}, want: "A=1\nB=new\n"},
		{name: "changed since the scan", content: "A=1\nB=mine\n", fixes: []Fix{fix(2, "B=old", "B=new")}, err: "changed since the scan"},
		{name: "past the end", content: "A=1\n", fixes: []Fix{fix(2, "B=old", "B=new")}, err: "past the file's end"},
		{
			name: "a package.json line", content: "{}\n", err: "no .env line",
			fixes: []Fix{{File: "package.json", Line: 1, Old: "{}", New: "{ }"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ApplyEnv([]byte(tt.content), tt.fixes)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || got != nil {
					t.Errorf("ApplyEnv = %q, %v; want nil and an error with %q", got, err, tt.err)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Errorf("ApplyEnv = %q, %v; want %q", got, err, tt.want)
			}
		})
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
