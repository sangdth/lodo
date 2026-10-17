package scan

import (
	"os"
	"path/filepath"
	"testing"
)

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
		{name: "vite dev", script: "vite dev --port 5174", tool: "vite", port: 5174},
		{name: "remix vite:dev", script: "remix vite:dev", tool: "vite", port: 5173},
		{name: "vite build", script: "vite build"},
		{name: "vite-node", script: "vite-node src/main.ts"},
		{name: "vitest", script: "vitest"},
		{name: "next build", script: "next build"},
		{name: "astro", script: "astro dev", tool: "astro", port: 4321},
		{name: "nuxt", script: "nuxt dev", tool: "nuxt", port: 3000},
		{name: "wrangler", script: "wrangler dev --port 8787", tool: "wrangler", port: 8787},
		{name: "nest", script: "nest start --watch --env-file .env", tool: "nest"}, // the Nest CLI has no port of its own
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

// TestDevServer_EnvPort checks which dev servers take PORT from the app's
// .env, then its .env.example.
func TestDevServer_EnvPort(t *testing.T) {
	t.Parallel()

	const nest, cra = `{"scripts": {"dev": "nest start --watch"}}`, `{"scripts": {"dev": "react-scripts start"}}`
	tests := []struct {
		name, pkg   string
		env, sample string // the .env and the .env.example; empty for none
		want        int
	}{
		{name: "nest", pkg: nest, env: "HOST=x\nPORT=3300\n", want: 3300},
		{name: "nest, .env.example only", pkg: nest, sample: "PORT=3301\n", want: 3301},
		{name: ".env first", pkg: nest, env: "PORT=3300\n", sample: "PORT=3301\n", want: 3300},
		{name: "nest without one", pkg: nest},
		{name: "commented out", pkg: nest, env: "# PORT=1\n"},
		{name: "export and quotes", pkg: nest, env: "export PORT=\"3302\"\n", want: 3302},
		{name: "too high", pkg: nest, env: "PORT=99999\n"},
		{name: "a variable", pkg: nest, env: "PORT=${X}\n"},
		{name: "create react app", pkg: cra, env: "PORT=3005\n", want: 3005},
		{name: "create react app without one", pkg: cra, want: 3000},
		{name: "next ignores it", pkg: `{"scripts": {"dev": "next dev"}}`, env: "PORT=4000\n", want: 3000},
		{name: "an unknown command", pkg: `{"scripts": {"dev": "tsx watch src/server.ts"}}`, env: "PORT=4000\n", want: 4000},
		{name: "its -p first", pkg: `{"scripts": {"dev": "tsx watch -p 5000 src/server.ts"}}`, env: "PORT=4000\n", want: 5000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, filepath.Join(dir, "package.json"), tt.pkg)
			for name, content := range map[string]string{".env": tt.env, ".env.example": tt.sample} {
				if content != "" {
					write(t, filepath.Join(dir, name), content)
				}
			}
			if srv, _, _ := devServer(dir); srv.port != tt.want {
				t.Errorf("port = %d, want %d", srv.port, tt.want)
			}
		})
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
