package scan

import (
	"encoding/json"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/store"
)

// devSh is the line of fastbackr's scripts/dev.sh that runs turbo.
const devSh = `turbo run dev --ui=stream "$@" 2>&1 | tee -i logs/terminal.log`

func TestTurboLine(t *testing.T) {
	t.Parallel()

	export := hostIPFromEnv + " "
	tests := []struct {
		name, line string
		shell      bool
		want       string // empty when lodo leaves the line
	}{
		{name: "fastbackr's dev.sh", line: devSh, shell: true, want: export + devSh},
		{name: "after exec and pnpm", line: "\texec pnpm turbo dev", shell: true, want: "\t" + export + "exec pnpm turbo dev"},
		{name: "after another command", line: "cd .. && turbo dev", shell: true, want: "cd .. && " + export + "turbo dev"},
		{name: "in a block", line: "if [ -f .env ]; then turbo dev; fi", shell: true, want: "if [ -f .env ]; then " + export + "turbo dev; fi"},
		{name: "in quotes", line: `sh -c "turbo dev"`, shell: true},
		{name: "set already", line: "DOCKER_HOST_IP=127.0.1.4 turbo dev", shell: true},
		{
			name: "a package.json script", line: `    "dev": "turbo run dev",`,
			want: `    "dev": "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(export) + `turbo run dev",`,
		},
		{name: "in escaped quotes", line: `    "dev": "concurrently \"turbo dev\" \"stripe listen\"",`},
		{name: "in quotes after &&", line: `sh -c 'cd apps && turbo dev'`, shell: true},
		{name: "in double quotes after ;", line: `bash -c "cd apps; turbo dev"`, shell: true},
		{name: "after closed quotes", line: `echo 'a;b' && turbo dev`, shell: true, want: `echo 'a;b' && ` + export + "turbo dev"},
		{name: "a package.json script in quotes", line: `    "dev": "sh -c 'cd apps && turbo dev'",`},
		{name: "in escaped quotes after &&", line: `    "dev": "concurrently \"cd x && turbo dev\"",`},
		{name: "an argument", line: "echo starting turbo dev", shell: true},
		{name: "after npx", line: "npx turbo dev", shell: true, want: export + "npx turbo dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, ok := turboLine("x", 1, tt.line, tt.shell)
			if ok != (tt.want != "") || ok && (f.New != tt.want || f.Old != tt.line) {
				t.Errorf("turboLine(%q) = %+v, %v; want %q", tt.line, f, ok, tt.want)
			}
		})
	}
}

// TestHostIPFromEnv runs the fix in both places it goes, a package.json
// script and a shell script, with /bin/sh against .env files. turbo's place
// goes to a child shell that prints compose.HostIP, as an app's -H gets it.
func TestHostIPFromEnv(t *testing.T) {
	t.Parallel()

	script, ok := turboLine("package.json", 1, `  "dev": "turbo run dev",`, false)
	if !ok {
		t.Fatal("no fix for the package.json script")
	}
	var pkg map[string]string
	if err := json.Unmarshal([]byte("{"+strings.TrimSuffix(script.New, ",")+"}"), &pkg); err != nil {
		t.Fatalf("the fixed script isn't JSON: %v", err)
	}
	shell, ok := turboLine("scripts/dev.sh", 1, devSh, true)
	if !ok {
		t.Fatal("no fix for the shell script")
	}
	app := `sh -c 'echo "` + compose.HostIP + `"'`
	lines := map[string]string{"package.json": pkg["dev"], "dev.sh": shell.New}
	for name, line := range lines {
		cmd := strings.Replace(line, "turbo run dev", app, 1)
		if name == "dev.sh" {
			cmd = strings.Replace(line, devSh, app, 1)
		}
		if cmd == line {
			t.Fatalf("%s: no turbo command in %q", name, line)
		}
		lines[name] = cmd
	}
	tests := []struct {
		name, env string // the .env; empty for none
		want      string // what the app's -H gets
	}{
		{name: "plain", env: "SECRET=x\nDOCKER_HOST_IP=127.0.1.4\n", want: "127.0.1.4"},
		{name: "double quotes", env: "DOCKER_HOST_IP=\"127.0.1.5\"\n", want: "127.0.1.5"},
		{name: "single quotes", env: "DOCKER_HOST_IP='127.0.1.6'\n", want: "127.0.1.6"},
		{name: "export", env: "export DOCKER_HOST_IP=127.0.1.7\n", want: "127.0.1.7"},
		{name: "two lines and a comment", env: "# DOCKER_HOST_IP=127.0.1.9\nDOCKER_HOST_IP=127.0.1.2\r\nDOCKER_HOST_IP=127.0.1.3\n", want: "127.0.1.2"},
		{name: "empty", env: "DOCKER_HOST_IP=\nDOCKER_HOST_IP=127.0.1.3\n", want: "127.0.1.3"},
		{name: "a variable", env: "DOCKER_HOST_IP=${X}\nDOCKER_HOST_IP=127.0.1.3\n", want: "127.0.1.3"},
		{name: "a lan address", env: "DOCKER_HOST_IP=192.168.1.20\nDOCKER_HOST_IP=127.0.1.3\n", want: "127.0.0.1"},
		{name: "every address", env: "DOCKER_HOST_IP=0.0.0.0\n", want: "127.0.0.1"},
		{name: "another key", env: "OLD_DOCKER_HOST_IP=127.0.1.8\n", want: "127.0.0.1"},
		{name: "no .env", want: "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.env != "" {
				write(t, filepath.Join(dir, ".env"), tt.env)
			}
			for name, line := range lines {
				cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", line)
				cmd.Dir = dir
				out, err := cmd.Output()
				if got := strings.TrimSuffix(string(out), "\n"); err != nil || got != tt.want {
					t.Errorf("%s: -H %q, %v; want %q", name, got, err, tt.want)
				}
			}
		})
	}
}

// TestPropose_Turbo checks a project shaped like fastbackr: turbo starts the
// apps, so their scripts read DOCKER_HOST_IP, which dev.sh sets from .env.
// They do only when the root's .env holds the project's address, now or once
// y links the compose file; otherwise they keep a literal host.
func TestPropose_Turbo(t *testing.T) {
	t.Parallel()

	const hostIP = compose.HostIP
	shop := map[string]string{
		"package.json":                 `{"scripts": {"dev": "sh scripts/dev.sh"}}`,
		"turbo.json":                   "{}\n",
		"pnpm-workspace.yaml":          "packages: [apps/*]\n",
		"scripts/dev.sh":               "#!/usr/bin/env sh\n# runs `turbo run dev` at the end\n" + devSh + "\n",
		"apps/web/package.json":        `{"scripts": {"dev": "next dev -p 3303"}}`,
		"apps/landing/package.json":    `{"scripts": {"dev": "next dev -H 127.0.1.4 -p 3302"}}`,
		"apps/admin/package.json":      `{"scripts": {"dev": "next dev -H ${DOCKER_HOST_IP:-127.0.0.1} -p 3304"}}`,
		"apps/admin/next.config.ts":    "export default { allowedDevOrigins: ['shop.test'] }\n",
		"apps/web/next.config.ts":      "export default { allowedDevOrigins: ['shop.test'] }\n",
		"apps/landing/next.config.mjs": "export default { allowedDevOrigins: ['shop.test'] }\n",
	}
	export := Fix{File: "scripts/dev.sh", Line: 3, Old: devSh, New: hostIPFromEnv + " " + devSh}
	fixes := func(host string) []Fix {
		return []Fix{
			{File: "apps/landing/package.json", Line: 1, Old: `{"scripts": {"dev": "next dev -H 127.0.1.4 -p 3302"}}`, New: `{"scripts": {"dev": "next dev -H ` + host + ` -p 3302"}}`},
			{File: "apps/web/package.json", Line: 1, Old: `{"scripts": {"dev": "next dev -p 3303"}}`, New: `{"scripts": {"dev": "next dev -H ` + host + ` -p 3303"}}`},
		}
	}
	withHostIP := append([]Fix{export}, fixes(hostIP)...)
	const composeFile = "services: {db: {image: postgres}}\n"
	tests := []struct {
		name    string
		files   map[string]string // on top of shop's
		domains []store.Domain
		want    []Fix
	}{
		{name: "the .env holds the address", files: map[string]string{".env": "DOCKER_HOST_IP=127.0.1.1\n"}, want: withHostIP},
		{name: "no .env", want: fixes("127.0.1.1")},
		{
			name: "another project holds the .env's address", files: map[string]string{".env": "DOCKER_HOST_IP=127.0.1.1\n"},
			domains: []store.Domain{{Name: "other.test", Address: "127.0.1.1", Enabled: true}}, want: fixes("127.0.1.2"),
		},
		{name: "the compose link writes it", files: map[string]string{"compose.yaml": composeFile}, want: withHostIP},
		{
			name: "compose runs with another .env", want: fixes("127.0.1.1"),
			files: map[string]string{
				"compose.yaml": composeFile,
				"package.json": `{"scripts": {"dev": "sh scripts/dev.sh", "db": "docker compose --env-file docker/.env -f compose.yaml up"}}`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(shop)
			maps.Copy(files, tt.files)
			p, _ := Find(project(t, "shop", files), nil)
			if got := Propose(p, tt.domains, "", nil); !slices.Equal(got.Fixes, tt.want) {
				t.Errorf("Fixes =\n%+v\nwant\n%+v", got.Fixes, tt.want)
			}
		})
	}

	files := maps.Clone(shop)
	files[".env"] = "DOCKER_HOST_IP=127.0.1.1\n"
	root := project(t, "shop", files)
	if got := HostFixes(root, "127.0.1.1"); !slices.Equal(got, withHostIP) {
		t.Errorf("HostFixes =\n%+v\nwant\n%+v", got, withHostIP)
	}
	if got := HostFixes(root, "127.0.1.2"); !slices.Equal(got, fixes("127.0.1.2")) {
		t.Errorf("HostFixes on an address the .env doesn't hold =\n%+v\nwant\n%+v", got, fixes("127.0.1.2"))
	}

	// An app listed on another address keeps a literal host.
	write(t, filepath.Join(root, ".env"), "DOCKER_HOST_IP=127.0.1.4\n")
	p, _ := Find(root, nil)
	got := Propose(p, []store.Domain{
		{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
		{Name: "landing.shop.test", Address: "127.0.1.9", Port: 3302, Enabled: true},
	}, "", nil)
	want := Fix{File: "apps/landing/package.json", Line: 1, Old: `{"scripts": {"dev": "next dev -H 127.0.1.4 -p 3302"}}`, New: `{"scripts": {"dev": "next dev -H 127.0.1.9 -p 3302"}}`}
	if len(got.Fixes) != 3 || got.Fixes[0] != export || got.Fixes[1] != want {
		t.Errorf("another address: Fixes =\n%+v\nwant the turbo line, then landing's\n%+v", got.Fixes, want)
	}

	// A package.json dev script that runs turbo gets the fix in place.
	write(t, filepath.Join(root, "package.json"), "{\n  \"scripts\": {\"dev\": \"turbo run dev\"}\n}\n")
	if f, ok := turboFix(root); !ok || f.File != "package.json" || f.Line != 2 {
		t.Errorf("turboFix = %+v, %v; want package.json:2", f, ok)
	}

	// An echo that names turbo's dev task is no turbo line.
	write(t, filepath.Join(root, "package.json"), `{"scripts": {"dev": "sh scripts/dev.sh"}}`)
	write(t, filepath.Join(root, "scripts", "dev.sh"), "echo \"starting turbo dev\"\necho starting turbo dev\n"+devSh+"\n")
	if f, ok := turboFix(root); !ok || f != (Fix{File: "scripts/dev.sh", Line: 3, Old: devSh, New: hostIPFromEnv + " " + devSh}) {
		t.Errorf("turboFix after echoes = %+v, %v; want scripts/dev.sh:3", f, ok)
	}
}

// TestPropose_TurboNoHost checks that the turbo line comes only with an app
// that reads DOCKER_HOST_IP: nest has no host option, so a listed nest app
// leaves nothing to change.
func TestPropose_TurboNoHost(t *testing.T) {
	t.Parallel()

	root := project(t, "shop", map[string]string{
		"package.json":          `{"scripts": {"dev": "turbo run dev"}}`,
		"turbo.json":            "{}\n",
		"pnpm-workspace.yaml":   "packages: [apps/*]\n",
		".env":                  "DOCKER_HOST_IP=127.0.1.1\n",
		"apps/api/package.json": `{"scripts": {"dev": "nest start --watch"}}`,
		"apps/api/.env":         "PORT=3300\n",
	})
	p, _ := Find(root, nil)
	got := Propose(p, []store.Domain{
		{Name: "shop.test", Address: "127.0.1.1", Enabled: true},
		{Name: "api.shop.test", Address: "127.0.1.1", Port: 3300, Enabled: true},
	}, "", nil)
	if len(got.Fixes) != 0 || !got.Empty() {
		t.Errorf("Fixes = %+v, Empty() = %v; want none and true", got.Fixes, got.Empty())
	}
	if fixes := HostFixes(root, "127.0.1.1"); len(fixes) != 0 {
		t.Errorf("HostFixes = %+v; want none", fixes)
	}
}
