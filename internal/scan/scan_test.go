package scan

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sangdth/lodo/internal/store"
)

// project writes files under a new project folder named name, with a .git
// folder, and returns its root.
func project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/master\n")
	for p, content := range files {
		write(t, filepath.Join(root, filepath.FromSlash(p)), content)
	}
	return root
}

func TestFind_Apps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  []App
	}{
		{
			name:  "plain next app",
			files: map[string]string{"package.json": `{"scripts": {"dev": "next dev --turbo"}}`},
			want:  []App{{Dir: ".", Tool: "next", Port: 3000}},
		},
		{
			name: "dev.sh that reads the host from .env",
			files: map[string]string{
				"package.json":   `{"scripts": {"dev": "./scripts/dev.sh"}}`,
				"scripts/dev.sh": "docker compose -f docker/compose.dev.yaml up -d\nexec next dev ${HOST_IP:+--hostname \"$HOST_IP\"}\n",
			},
			want: []App{{Dir: ".", Tool: "next", Port: 3000}},
		},
		{
			name:  "a dev script lodo doesn't know",
			files: map[string]string{"package.json": `{"scripts": {"dev": "tsx watch src/server.ts"}}`},
			want:  []App{{Dir: "."}},
		},
		{
			name: "pnpm monorepo",
			files: map[string]string{
				"package.json":                 `{"scripts": {"dev": "turbo dev"}}`,
				"pnpm-workspace.yaml":          "packages:\n  - apps/*\n  - packages/*\n  - '!**/test/**'\n",
				"apps/storefront/package.json": `{"scripts": {"dev": "next dev -p 3002"}}`,
				"apps/admin/package.json":      `{"scripts": {"dev": "vite --host 0.0.0.0 --port 3001"}}`,
				"apps/api/package.json":        `{"scripts": {"dev": "nest start --watch"}}`,
				"apps/docs/package.json":       `{"scripts": {"build": "mintlify build"}}`,
				"packages/ui/package.json":     `{"scripts": {"dev": "tsc --watch"}}`,
				"packages/webapp/package.json": `{"scripts": {"dev": "vite"}}`,
			},
			want: []App{
				{Dir: "apps/admin", Tool: "vite", Port: 3001},
				{Dir: "apps/api", Tool: "nest", Port: 3000},
				{Dir: "apps/storefront", Tool: "next", Port: 3002},
				{Dir: "packages/webapp", Tool: "vite", Port: 5173},
			},
		},
		{
			name: "package.json workspaces object",
			files: map[string]string{
				"package.json":               `{"workspaces": {"packages": ["apps/*"]}, "scripts": {"dev": "turbo dev"}}`,
				"apps/client/package.json":   `{"scripts": {"dev": "vite"}}`,
				"apps/client/vite.config.ts": "export default { server: { port: 5175 } }",
			},
			want: []App{{Dir: "apps/client", Tool: "vite", Port: 5175}},
		},
		{
			name: "single app with a workspace file for pnpm settings",
			files: map[string]string{
				"package.json":        `{"scripts": {"dev": "next dev"}}`,
				"pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n",
			},
			want: []App{{Dir: ".", Tool: "next", Port: 3000}},
		},
		{name: "no package.json", files: map[string]string{"go.mod": "module x\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, ok := Find(project(t, "shop", tt.files))
			if !ok {
				t.Fatal("Find: not a project")
			}
			if !slices.Equal(p.Apps, tt.want) {
				t.Errorf("Apps =\n%+v\nwant\n%+v", p.Apps, tt.want)
			}
		})
	}
}

func TestFind_EnvAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: ".env", files: map[string]string{".env": "SECRET=x\nDOCKER_HOST_IP=127.0.1.2\n"}, want: "127.0.1.2"},
		{name: ".env first", files: map[string]string{".env": "DOCKER_HOST_IP=127.0.1.4\n", ".env.example": "DOCKER_HOST_IP=127.0.1.2\n"}, want: "127.0.1.4"},
		{name: ".env.example", files: map[string]string{".env.example": "export DOCKER_HOST_IP=\"127.0.1.7\"\n"}, want: "127.0.1.7"},
		{name: "not an own address", files: map[string]string{".env": "DOCKER_HOST_IP=127.0.0.1\n"}},
		{name: "commented out", files: map[string]string{".env": "# DOCKER_HOST_IP=127.0.1.2\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := Find(project(t, "shop", tt.files))
			if p.EnvAddress != tt.want {
				t.Errorf("EnvAddress = %q, want %q", p.EnvAddress, tt.want)
			}
		})
	}
}

func TestFind_NotAProject(t *testing.T) {
	t.Parallel()

	if p, ok := Find(t.TempDir()); ok {
		t.Errorf("Find outside a project = %+v, true", p)
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	for root, want := range map[string]string{
		"/Users/sang/Projects/flowy":      "flowy.test",
		"/Users/sang/Projects/My_App":     "my-app.test",
		"/Users/sang/Projects/opscom.web": "opscom-web.test",
		"/Users/sang/Projects/__":         "",
	} {
		if got := Name(root); got != want {
			t.Errorf("Name(%q) = %q, want %q", root, got, want)
		}
	}
}

func TestPropose(t *testing.T) {
	t.Parallel()

	const compose = "/p/shop/docker/compose.dev.yaml"
	monorepo := []App{
		{Dir: "apps/storefront", Tool: "next", Port: 3000}, {Dir: "apps/admin", Tool: "next", Port: 3000},
		{Dir: "apps/api"}, {Dir: "apps/email", Port: 3003},
	}
	tests := []struct {
		name    string
		project Project
		domains []store.Domain
		chosen  string // the name the user chose
		want    Proposal
	}{
		{
			name:    "new app with a compose file",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			domains: []store.Domain{{Name: "blog.test", Address: "127.0.1.1", Enabled: true}},
			want: Proposal{
				Address: "127.0.1.2",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: "/p/shop"}},
				Compose: compose,
			},
		},
		{
			name:    "the .env's address",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}, EnvAddress: "127.0.1.7"},
			want: Proposal{
				Address: "127.0.1.7",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.7", Port: 3000, Enabled: true, Root: "/p/shop"}},
			},
		},
		{
			name:    "the .env's address belongs to another project",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}, EnvAddress: "127.0.1.1"},
			domains: []store.Domain{{Name: "blog.test", Address: "127.0.1.1", Enabled: true}},
			want: Proposal{
				Address: "127.0.1.2",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: "/p/shop"}},
			},
		},
		{
			name:    "monorepo: same port moves up, unknown port gets a note",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: monorepo},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "storefront.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
					{Name: "admin.shop.test", Address: "127.0.1.1", Port: 3001, Enabled: true},
					{Name: "api.shop.test", Address: "127.0.1.1", Enabled: true},
					{Name: "email.shop.test", Address: "127.0.1.1", Port: 3003, Enabled: true},
				},
				Notes: []string{
					"api.shop.test: lodo can't tell its dev server's port; e sets it",
					"email.shop.test: lodo doesn't know its dev command; start it on the name's address",
				},
			},
		},
		{
			name:    "listed project: only what is missing, on its address",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: monorepo},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: store.NoCompose},
				{Name: "storefront.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 4000, Enabled: true},
			},
			want: Proposal{
				Address: "127.0.1.4",
				Listed:  true,
				Add: []store.Domain{
					{Name: "admin.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true},
					{Name: "email.shop.test", Address: "127.0.1.4", Port: 3003, Enabled: true},
				},
				Notes: []string{"email.shop.test: lodo doesn't know its dev command; start it on the name's address"},
			},
		},
		{
			name:    "fully listed and linked",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true, Compose: compose}},
			want:    Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "a compose file only",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}},
			want: Proposal{
				Address: "127.0.1.1",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"}},
				Compose: compose,
			},
		},
		{
			name:    "a listed name that links a file in the project names it",
			project: Project{Root: "/p/platform-v2", Name: "platform-v2.test", Compose: []string{"/p/platform-v2/docker/compose.dev.yml"}, Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			domains: []store.Domain{{Name: "hugger.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Compose: "/p/platform-v2/docker/compose.dev.yml"}},
			want:    Proposal{Name: "hugger.test", Address: "127.0.1.1", Listed: true},
		},
		{
			name:    "the user's name: subdomains follow it",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}, {Dir: "apps/admin", Tool: "vite", Port: 5173}}},
			chosen:  "shop.test",
			want: Proposal{
				Name:    "shop.test",
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop-v2"},
					{Name: "admin.shop.test", Address: "127.0.1.1", Port: 5173, Enabled: true},
				},
			},
		},
		{
			name:    "the name chosen once sticks: its root names the project",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop-v2"}},
			want:    Proposal{Name: "shop.test", Address: "127.0.1.1", Listed: true},
		},
		{
			name:    "the user names a listed name: it claims the folder",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true}},
			chosen:  "shop.test",
			want:    Proposal{Name: "shop.test", Address: "127.0.1.1", Listed: true, Claim: true},
		},
		{
			name:    "a name with two labels",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}},
			chosen:  "web.shop.test",
			want:    Proposal{Name: "", Notes: []string{"web.shop.test: a project's name has one label before .test"}},
		},
		{
			name:    "a framework without a host option gets its port and a note",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/server", Tool: "nest", Port: 3000}}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "server.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
				},
				Notes: []string{"server.shop.test: nest has no host option; make the app listen on the name's address"},
			},
		},
		{
			name: "nest keeps its port, the frontends move up",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: ".", Tool: "next", Port: 3000}, {Dir: "apps/admin", Tool: "vite", Port: 3000}, {Dir: "apps/server", Tool: "nest", Port: 3000},
			}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Port: 3001, Enabled: true, Root: "/p/shop"},
					{Name: "admin.shop.test", Address: "127.0.1.1", Port: 3002, Enabled: true},
					{Name: "server.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
				},
				Notes: []string{"server.shop.test: nest has no host option; make the app listen on the name's address"},
			},
		},
		{
			name:    "two apps lodo can't move get two addresses",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/api", Tool: "nest", Port: 3000}, {Dir: "apps/mail", Port: 3000}}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
					{Name: "mail.shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
				},
				Notes: []string{
					"api.shop.test: nest has no host option; make the app listen on the name's address",
					"mail.shop.test: lodo doesn't know its dev command; start it on the name's address",
				},
			},
		},
		{name: "nothing to serve", project: Project{Root: "/p/dotfiles", Name: "dotfiles.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Propose(tt.project, tt.domains, tt.chosen)
			tt.want.Root = tt.project.Root
			if tt.want.Name == "" && tt.chosen == "" {
				tt.want.Name = tt.project.Name
			}
			if !equal(got, tt.want) {
				t.Errorf("Propose =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func equal(a, b Proposal) bool {
	return a.Root == b.Root && a.Name == b.Name && a.Address == b.Address && a.Listed == b.Listed && a.Claim == b.Claim &&
		slices.Equal(a.Add, b.Add) && a.Compose == b.Compose && slices.Equal(a.Fixes, b.Fixes) && slices.Equal(a.Notes, b.Notes)
}

func TestPropose_Fixes(t *testing.T) {
	t.Parallel()

	root := project(t, "shop", map[string]string{
		"package.json":             `{"scripts": {"dev": "turbo dev"}}`,
		"pnpm-workspace.yaml":      "packages: [apps/*]\n",
		"apps/web/package.json":    "{\n  \"scripts\": {\"dev\": \"next dev\"}\n}\n",
		"apps/admin/package.json":  "{\n  \"scripts\": {\"dev\": \"next dev -H $HOST\"}\n}\n",
		"apps/server/package.json": "{\n  \"scripts\": {\"dev\": \"nest start --watch\"}\n}\n",
	})
	p, _ := Find(root)
	got := Propose(p, nil, "")
	// server keeps 3000; admin and web move up, in folder order.
	want := []Fix{
		{File: "apps/admin/package.json", Line: 2, Old: `  "scripts": {"dev": "next dev -H $HOST"}`, New: `  "scripts": {"dev": "next dev --port 3001 -H $HOST"}`},
		{File: "apps/web/package.json", Line: 2, Old: `  "scripts": {"dev": "next dev"}`, New: `  "scripts": {"dev": "next dev -H 127.0.1.1 --port 3002"}`},
	}
	if !slices.Equal(got.Fixes, want) {
		t.Errorf("Fixes =\n%+v\nwant\n%+v", got.Fixes, want)
	}

	// Once listed, the scan still shows the port the list gives the app.
	got = Propose(p, got.Add, "")
	if len(got.Add) != 0 || !slices.Equal(got.Fixes, want) {
		t.Errorf("listed: Add = %+v, Fixes =\n%+v\nwant\n%+v", got.Add, got.Fixes, want)
	}
}
