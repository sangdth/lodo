package scan

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFixLine(t *testing.T) {
	t.Parallel()

	const addr = "127.0.1.3"
	tests := []struct {
		name, line, want string
		port             int // the port the dev server must take; 0 keeps it
		shell            bool
	}{
		{name: "a package.json script", line: `    "dev": "next dev",`, want: `    "dev": "next dev -H 127.0.1.3",`},
		{name: "options after it", line: `next dev --turbopack -p 3001`, want: `next dev -H 127.0.1.3 --turbopack -p 3001`},
		{
			name: "a shell line with arguments and a pipe", shell: true,
			line: `next dev "$@" 2>&1 | tee -i logs/terminal.log`,
			want: `next dev -H 127.0.1.3 "$@" 2>&1 | tee -i logs/terminal.log`,
		},
		{name: "another address", line: `next dev -H 127.0.1.9 -p 3000`, want: `next dev -H 127.0.1.3 -p 3000`},
		{name: "--hostname=", line: `next dev --hostname=0.0.0.0`, want: `next dev --hostname=127.0.1.3`},
		{name: "two commands", line: `next dev -p 3100 && next dev`, want: `next dev -H 127.0.1.3 -p 3100 && next dev -H 127.0.1.3`},
		{name: "the address already", line: `next dev -H 127.0.1.3`, want: `next dev -H 127.0.1.3`},
		{name: "a variable", line: `next dev -H $HOST`, want: `next dev -H $HOST`},
		{name: "a quoted host", shell: true, line: `next dev -H "${DOCKER_HOST_IP:-0.0.0.0}"`, want: `next dev -H "${DOCKER_HOST_IP:-0.0.0.0}"`},
		{name: "an escaped quote in package.json", line: `"dev": "next dev -H \"$HOST\""`, want: `"dev": "next dev -H \"$HOST\""`},
		{
			name: "a host only when set", shell: true,
			line: `exec next dev ${HOST_IP:+--hostname "$HOST_IP"}`, want: `exec next dev ${HOST_IP:+--hostname "$HOST_IP"}`,
		},
		{name: "-H of the next command", line: `next dev && curl -H x`, want: `next dev -H 127.0.1.3 && curl -H x`},
		{name: "next build", line: `"build": "next build",`, want: `"build": "next build",`},
		{name: "vite", line: `"dev": "vite",`, want: `"dev": "vite --host 127.0.1.3",`},
		{name: "vite dev", line: `"dev": "vite dev --port 5174",`, want: `"dev": "vite dev --host 127.0.1.3 --port 5174",`},
		{name: "vite with a bare --host", line: `"dev": "vite --host",`, want: `"dev": "vite --host 127.0.1.3",`},
		{name: "vite --host before an option", line: `vite --host --port 3001`, want: `vite --host 127.0.1.3 --port 3001`},
		{name: "vite on every address", line: `vite --host 0.0.0.0 --port 3001`, want: `vite --host 127.0.1.3 --port 3001`},
		{name: "remix vite:dev", line: `"dev": "remix vite:dev",`, want: `"dev": "remix vite:dev --host 127.0.1.3",`},
		{name: "vite build", line: `"build": "vite build",`, want: `"build": "vite build",`},
		{name: "vite-node", line: `"dev": "vite-node src/main.ts",`, want: `"dev": "vite-node src/main.ts",`},
		{name: "vitest", line: `"test": "vitest",`, want: `"test": "vitest",`},
		{name: "astro dev", line: `"dev": "astro dev",`, want: `"dev": "astro dev --host 127.0.1.3",`},
		{name: "nuxt dev", line: `"dev": "nuxt dev",`, want: `"dev": "nuxt dev --host 127.0.1.3",`},
		{name: "wrangler dev", line: `"dev": "wrangler dev --port 8787",`, want: `"dev": "wrangler dev --ip 127.0.1.3 --port 8787",`},
		{name: "ng serve", line: `"dev": "ng serve",`, want: `"dev": "ng serve --host 127.0.1.3",`},
		{name: "gatsby develop", line: `"dev": "gatsby develop -H 0.0.0.0",`, want: `"dev": "gatsby develop -H 127.0.1.3",`},
		{name: "storybook dev", line: `"dev": "storybook dev -p 6006",`, want: `"dev": "storybook dev --host 127.0.1.3 -p 6006",`},
		{name: "nest has no host option", line: `"dev": "nest start --watch",`, want: `"dev": "nest start --watch",`},
		{name: "a new port", line: `"dev": "next dev --turbopack",`, port: 3001, want: `"dev": "next dev -H 127.0.1.3 --port 3001 --turbopack",`},
		{name: "a port in place of another", line: `vite --host 0.0.0.0 --port 3000`, port: 3001, want: `vite --host 127.0.1.3 --port 3001`},
		{name: "a port before the host", line: `next dev -p 3000 -H 0.0.0.0`, port: 3002, want: `next dev -p 3002 -H 127.0.1.3`},
		{name: "the port already", line: `next dev -H 127.0.1.3 -p 3001`, port: 3001, want: `next dev -H 127.0.1.3 -p 3001`},
		{name: "a port variable stays", line: `next dev -p $PORT`, port: 3001, want: `next dev -H 127.0.1.3 -p $PORT`},
		{name: "a variable host still gets the port", shell: true, line: `next dev -H "$HOST"`, port: 3001, want: `next dev --port 3001 -H "$HOST"`},
		{name: "nest gets no port", line: `"dev": "nest start",`, port: 3001, want: `"dev": "nest start",`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fixLine(tt.line, addr, tt.port, tt.shell); got != tt.want {
				t.Errorf("fixLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestServerIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, script string
		files        map[string]string // in the app's folder
		tool         string            // empty when no server starts
		port         int
	}{
		{name: "next", script: "next dev --turbopack", tool: "next", port: 3000},
		{name: "-p", script: "next dev -p 3002", tool: "next", port: 3002},
		{name: "--port=", script: "vite --port=3000", tool: "vite", port: 3000},
		{name: "PORT before it", script: "PORT=3100 next dev", tool: "next", port: 3100},
		{name: "a port variable", script: "next dev -p $PORT", tool: "next"},
		{name: "after another command", script: "next typegen && TZ=UTC next dev -p 3411", tool: "next", port: 3411},
		{name: "vite config", script: "vite", files: map[string]string{"vite.config.ts": "server: { port: 5171, strictPort: true }"}, tool: "vite", port: 5171},
		{name: "vite config from env", script: "vite", files: map[string]string{"vite.config.ts": "server: { port: Number(env.PORT) || 3000 }"}, tool: "vite"},
		{name: "vite config without a port", script: "vite", files: map[string]string{"vite.config.js": "export default {}"}, tool: "vite", port: 5173},
		{name: "astro", script: "astro dev", tool: "astro", port: 4321},
		{name: "wrangler", script: "wrangler dev --port 8787", tool: "wrangler", port: 8787},
		{name: "nest", script: "nest start --watch --env-file .env", tool: "nest", port: 3000},
		{name: "angular", script: "ng serve", tool: "angular", port: 4200},
		{name: "gatsby", script: "gatsby develop -p 8001", tool: "gatsby", port: 8001},
		{name: "storybook", script: "storybook dev -p 6007", tool: "storybook", port: 6007},
		{name: "create react app", script: "react-scripts start", tool: "create-react-app", port: 3000},
		{name: "unknown", script: "tsx watch src/server.ts"},
		{name: "turbo", script: "turbo run dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, content := range tt.files {
				write(t, filepath.Join(dir, name), content)
			}
			srv, ok := serverIn(tt.script, dir, false)
			got := ""
			if ok {
				got = srv.tool.name
			}
			if got != tt.tool || srv.port != tt.port {
				t.Errorf("serverIn(%q) = %q port %d, want %q port %d", tt.script, got, srv.port, tt.tool, tt.port)
			}
		})
	}
}

func TestDevServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		pkg    string
		tool   string
		port   int
		known  bool
		hasDev bool
	}{
		{name: "a script it runs", pkg: `{"scripts": {"dev": "concurrently \"npm:next\" \"npm:stripe\"", "next": "next dev -p 3002"}}`, tool: "next", port: 3002, known: true, hasDev: true},
		{name: "pnpm run", pkg: `{"scripts": {"dev": "pnpm run web", "web": "astro dev"}}`, tool: "astro", port: 4321, known: true, hasDev: true},
		{name: "an unknown tool with a port", pkg: `{"scripts": {"dev": "mintlify dev --port 3004"}}`, port: 3004, hasDev: true},
		{name: "an unknown tool", pkg: `{"scripts": {"dev": "tsx watch src/server.ts"}}`, hasDev: true},
		{name: "no dev script", pkg: `{"scripts": {"start": "next start"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, filepath.Join(dir, "package.json"), tt.pkg)
			srv, known, hasDev := devServer(dir)
			tool := ""
			if srv.tool != nil {
				tool = srv.tool.name
			}
			if tool != tt.tool || srv.port != tt.port || known != tt.known || hasDev != tt.hasDev {
				t.Errorf("devServer = %q port %d known %v hasDev %v; want %q port %d known %v hasDev %v",
					tool, srv.port, known, hasDev, tt.tool, tt.port, tt.known, tt.hasDev)
			}
		})
	}
}

func TestFixesIn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "package.json"), `{
  "scripts": {
    "dev": "./scripts/dev.sh",
    "booking": "PORT=3100 next dev -p 3100",
    "start": "next start",
    "outside": "sh ../other/dev.sh"
  },
  "devDependencies": {
    "next": "15.0.0",
    "vite": "^6.0.0"
  }
}
`)
	write(t, filepath.Join(root, "scripts", "dev.sh"), "#!/usr/bin/env sh\r\n# runs `next dev` at the end\r\nnext dev \"$@\"\r\n")
	write(t, filepath.Join(root, "apps", "admin", "package.json"), `{"scripts": {"dev": "vite --host 0.0.0.0"}}`)

	got := fixesIn(root, ".", "127.0.1.5", 0)
	want := []Fix{
		{File: "package.json", Line: 4, Old: `    "booking": "PORT=3100 next dev -p 3100",`, New: `    "booking": "PORT=3100 next dev -H 127.0.1.5 -p 3100",`},
		{File: "scripts/dev.sh", Line: 3, Old: `next dev "$@"`, New: `next dev -H 127.0.1.5 "$@"`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("fixesIn(.) =\n%+v\nwant\n%+v", got, want)
	}
	// A new port goes to the dev script's shell script, not to booking.
	got = fixesIn(root, ".", "127.0.1.5", 3001)
	want[1].New = `next dev -H 127.0.1.5 --port 3001 "$@"`
	if !slices.Equal(got, want) {
		t.Errorf("fixesIn(., 3001) =\n%+v\nwant\n%+v", got, want)
	}
	got = fixesIn(root, "apps/admin", "127.0.1.6", 0)
	want = []Fix{{File: "apps/admin/package.json", Line: 1, Old: `{"scripts": {"dev": "vite --host 0.0.0.0"}}`, New: `{"scripts": {"dev": "vite --host 127.0.1.6"}}`}}
	if !slices.Equal(got, want) {
		t.Errorf("fixesIn(apps/admin) =\n%+v\nwant\n%+v", got, want)
	}
	if got := fixesIn(t.TempDir(), ".", "127.0.1.5", 0); got != nil {
		t.Errorf("without a package.json: %+v, want none", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
