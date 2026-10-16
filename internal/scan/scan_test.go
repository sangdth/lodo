package scan

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
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
				{Dir: "apps/api", Tool: "nest"},
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
			p, ok := Find(project(t, "shop", tt.files), nil)
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
			p, _ := Find(project(t, "shop", tt.files), nil)
			if p.EnvAddress != tt.want {
				t.Errorf("EnvAddress = %q, want %q", p.EnvAddress, tt.want)
			}
		})
	}
}

func TestFind_Pages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  []Page
	}{
		{
			name: "Mailpit bound to DOCKER_HOST_IP",
			files: map[string]string{"docker/compose.dev.yaml": composeFile("axllent/mailpit:latest",
				"${DOCKER_HOST_IP:-127.0.0.1}:1025:1025", "${DOCKER_HOST_IP:-127.0.0.1}:8025:8025")},
			want: []Page{{Label: "mail", Port: 8025}},
		},
		{
			name:  "from another registry, on another port",
			files: map[string]string{"compose.yaml": composeFile("ghcr.io/axllent/mailpit:v1.21@sha256:0abc", "18025:8025")},
			want:  []Page{{Label: "mail", Port: 18025}},
		},
		{name: "SMTP only", files: map[string]string{"compose.yaml": composeFile("axllent/mailpit", "1025:1025")}},
		{name: "an image lodo doesn't know", files: map[string]string{"compose.yaml": composeFile("mailhog/mailhog", "8025:8025")}},
		{name: "a file that doesn't parse", files: map[string]string{"compose.yaml": "services: ["}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := Find(project(t, "shop", tt.files), nil)
			if !slices.Equal(p.Pages, tt.want) {
				t.Errorf("Pages = %+v, want %+v", p.Pages, tt.want)
			}
		})
	}
}

// composeFile is a compose file with one service that runs image and
// publishes ports.
func composeFile(image string, ports ...string) string {
	return "services:\n  mail:\n    image: " + image + "\n    ports: [\"" + strings.Join(ports, `", "`) + "\"]\n"
}

func TestFind_NotAProject(t *testing.T) {
	t.Parallel()

	if p, ok := Find(t.TempDir(), nil); ok {
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

	const compose, prod = "/p/shop/docker/compose.dev.yaml", "/p/shop/docker/compose.prod.yaml"
	elsewhere := filepath.Join(t.TempDir(), "compose.yaml") // outside the project, and still there
	write(t, elsewhere, "services: {}\n")
	inbox := []Page{{Label: "mail", Port: 8025}}
	monorepo := []App{
		{Dir: "apps/storefront", Tool: "next", Port: 3000}, {Dir: "apps/admin", Tool: "next", Port: 3000},
		{Dir: "apps/api"}, {Dir: "apps/email", Port: 3003},
	}
	tests := []struct {
		name    string
		project Project
		domains []store.Domain
		chosen  string            // the name the user chose
		labels  map[string]string // the labels the user gave apps
		dirs    map[string]string // want.Dirs, checked when set
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
					"api.shop.test: lodo can't tell its dev server's port; set it with e in the list",
					"api.shop.test: lodo doesn't know its dev command; start it on the name's address",
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
			name:    "Mailpit's inbox gets mail.<project>",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Tool: "next", Port: 3000}}, Pages: inbox},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop"},
					{Name: "mail.shop.test", Address: "127.0.1.1", Port: 8025, Enabled: true},
				},
				Compose: compose,
			},
		},
		{
			name:    "a listed inbox stays, on its port",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Pages: inbox},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: compose},
				{Name: "mail.shop.test", Address: "127.0.1.4", Port: 8026, Enabled: true},
			},
			want: Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "another name on the inbox's port serves it",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Pages: inbox},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: compose},
				{Name: "inbox.shop.test", Address: "127.0.1.4", Port: 8025, Enabled: true},
			},
			want: Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "a project set to no compose file gets no inbox",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Pages: inbox},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: store.NoCompose}},
			want:    Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "a project that runs another compose file gets no inbox from the first",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose, prod}, Pages: inbox},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: prod}},
			want:    Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "nor when another of its names links that file",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose, prod}, Pages: inbox},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 4000, Enabled: true, Compose: prod},
			},
			want: Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name:    "a link to a file that moved in the project counts as none",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Root: "/p/shop", Compose: "/p/shop/compose.yaml"}},
			want:    Proposal{Address: "127.0.1.4", Listed: true, Compose: compose},
		},
		{
			name:    "a link to a file elsewhere that is still there stays",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: elsewhere}},
			want:    Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name: "a renamed folder: the user's name claims it and links its file",
			project: Project{
				Root: "/p/ai-calling", Name: "ai-calling.test", Compose: []string{"/p/ai-calling/docker/compose.dev.yaml"},
				Apps: []App{{Dir: ".", Tool: "next", Port: 3000}},
			},
			domains: []store.Domain{
				{Name: "calling.test", Address: "127.0.1.5", Port: 3000, Enabled: true, Root: "/p/client-calling", Compose: "/p/client-calling/compose.yaml"},
			},
			chosen: "calling.test",
			want: Proposal{
				Name: "calling.test", Address: "127.0.1.5", Listed: true, Claim: true, Compose: "/p/ai-calling/docker/compose.dev.yaml",
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
		{
			name:    "nest without a port gets two notes",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/server", Tool: "nest"}}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "server.shop.test", Address: "127.0.1.1", Enabled: true},
				},
				Notes: []string{
					"server.shop.test: lodo can't tell its dev server's port; set it with e in the list",
					"server.shop.test: nest has no host option; make the app listen on the name's address",
				},
			},
		},
		{
			name:    "an app listed without a port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Tool: "next", Port: 3001}, {Dir: "apps/server", Tool: "nest"}}},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "web.shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "server.shop.test", Address: "127.0.1.4", Enabled: true},
			},
			want: Proposal{
				Address: "127.0.1.4", Listed: true,
				Notes: []string{
					"web.shop.test: listed without a port; its dev server takes 3001: set it with e in the list",
					"server.shop.test: listed without a port, and lodo can't tell its dev server's; set it with e in the list",
				},
			},
		},
		{
			name:    "an app keeps the listed name on its port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Tool: "next", Port: 3001}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
			want:    Proposal{Address: "127.0.1.4", Listed: true},
		},
		{
			name: "a name another folder is named after is no alias",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/api", Tool: "nest", Port: 3000}, {Dir: "apps/web", Tool: "next", Port: 3000},
			}},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
			},
			want: Proposal{
				Address: "127.0.1.4", Listed: true,
				Add: []store.Domain{{Name: "web.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
			},
		},
		{
			name:    "a label names an app",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/backend", Tool: "vite", Port: 3300}}},
			labels:  map[string]string{"apps/backend": "api"},
			dirs:    map[string]string{"api.shop.test": "apps/backend"},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Port: 3300, Enabled: true},
				},
			},
		},
		{
			name:    "an app takes the project's name",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Tool: "next", Port: 3000}, {Dir: "apps/api", Tool: "vite", Port: 3300}}},
			labels:  map[string]string{"apps/web": ""},
			dirs:    map[string]string{"shop.test": "apps/web", "api.shop.test": "apps/api"},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Port: 3300, Enabled: true},
				},
			},
		},
		{
			name:    "the root's app keeps the project's name",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}, {Dir: "apps/web", Tool: "next", Port: 3001}}},
			labels:  map[string]string{"apps/web": ""},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop"},
					{Name: "web.shop.test", Address: "127.0.1.1", Port: 3001, Enabled: true},
				},
				Notes: []string{"apps/web: the root's app has shop.test"},
			},
		},
		{
			name:    "a listed project name stays",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Tool: "next", Port: 3001}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true}},
			labels:  map[string]string{"apps/web": ""},
			want: Proposal{
				Address: "127.0.1.4", Listed: true,
				Add:   []store.Domain{{Name: "web.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
				Notes: []string{"apps/web: shop.test is listed: give it the app's port with e in the list"},
			},
		},
		{
			name:    "a label on an app without a port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/api", Tool: "nest"}}},
			labels:  map[string]string{"apps/api": "backend"},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Enabled: true},
				},
				Notes: []string{
					"apps/api: lodo can't tell its port, so the name wouldn't stick: set PORT in its .env or a port in its dev script",
					"api.shop.test: lodo can't tell its dev server's port; set it with e in the list",
					"api.shop.test: nest has no host option; make the app listen on the name's address",
				},
			},
		},
		{
			name: "two apps on one listed port take one name",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/docs", Tool: "next", Port: 3000}, {Dir: "apps/web", Tool: "next", Port: 3000},
			}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true}},
			want: Proposal{
				Address: "127.0.1.4", Listed: true,
				Add: []store.Domain{{Name: "web.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
			},
		},
		{
			name: "a label can't take another app's name",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/api", Tool: "next", Port: 4000}, {Dir: "apps/web", Tool: "next", Port: 3000},
			}},
			labels: map[string]string{"apps/web": "api"},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Port: 4000, Enabled: true},
					{Name: "web.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
				},
				Notes: []string{"apps/web: apps/api has api.shop.test"},
			},
		},
		{
			name: "two apps can't take one label",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/api", Tool: "next", Port: 4000}, {Dir: "apps/web", Tool: "next", Port: 3000},
			}},
			labels: map[string]string{"apps/api": "x", "apps/web": "x"},
			dirs:   map[string]string{"x.shop.test": "apps/api"},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "x.shop.test", Address: "127.0.1.1", Port: 4000, Enabled: true},
				},
				Notes: []string{"apps/web: apps/api has x.shop.test"},
			},
		},
		{
			name: "next apps without allowedDevOrigins",
			project: Project{
				Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Tool: "next", Port: 3000}, {Dir: "apps/web", Tool: "next", Port: 3001}},
				NoDevOrigins: []string{".", "apps/web"},
			},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true}, {Name: "web.shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
			want: Proposal{
				Address: "127.0.1.4", Listed: true,
				Notes: []string{"shop.test, web: Next 16.2+ refuses hot reload from other hosts; add allowedDevOrigins: ['shop.test', '*.shop.test'] to next.config"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Propose(tt.project, tt.domains, tt.chosen, tt.labels)
			tt.want.Root = tt.project.Root
			if tt.want.Name == "" && tt.chosen == "" {
				tt.want.Name = tt.project.Name
			}
			if !equal(got, tt.want) {
				t.Errorf("Propose =\n%+v\nwant\n%+v", got, tt.want)
			}
			if tt.dirs != nil && !maps.Equal(got.Dirs, tt.dirs) {
				t.Errorf("Dirs = %v, want %v", got.Dirs, tt.dirs)
			}
		})
	}
}

func equal(a, b Proposal) bool {
	return a.Root == b.Root && a.Name == b.Name && a.Address == b.Address && a.Listed == b.Listed && a.Claim == b.Claim &&
		slices.Equal(a.Add, b.Add) && a.Compose == b.Compose && slices.Equal(a.Fixes, b.Fixes) && slices.Equal(a.Env, b.Env) &&
		slices.Equal(a.Notes, b.Notes)
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
	p, _ := Find(root, nil)
	got := Propose(p, nil, "", nil)
	// server's port is unknown, so admin keeps 3000 and web moves up; admin's
	// host is a variable lodo leaves.
	want := []Fix{
		{File: "apps/web/package.json", Line: 2, Old: `  "scripts": {"dev": "next dev"}`, New: `  "scripts": {"dev": "next dev -H 127.0.1.1 --port 3001"}`},
	}
	if !slices.Equal(got.Fixes, want) {
		t.Errorf("Fixes =\n%+v\nwant\n%+v", got.Fixes, want)
	}

	// Once listed, the scan still shows the port the list gives the app.
	got = Propose(p, got.Add, "", nil)
	if len(got.Add) != 0 || !slices.Equal(got.Fixes, want) {
		t.Errorf("listed: Add = %+v, Fixes =\n%+v\nwant\n%+v", got.Add, got.Fixes, want)
	}
}

func TestCheckLabel(t *testing.T) {
	t.Parallel()

	apps := []App{{Dir: "apps/web", Tool: "next", Port: 3000}, {Dir: "apps/api", Tool: "nest"}}
	withRoot := append([]App{{Dir: ".", Tool: "next", Port: 3001}}, apps...)
	listed := []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Enabled: true}}
	tests := []struct {
		name    string
		apps    []App
		domains []store.Domain
		dir     string
		label   string
		err     string // empty when the label fits
	}{
		{name: "a label", apps: apps, dir: "apps/web", label: "front"},
		{name: "the bare name", apps: apps, dir: "apps/web"},
		{name: "an invalid name", apps: apps, dir: "apps/web", label: "my_web", err: "name"},
		{name: "the root's app has the bare name", apps: withRoot, dir: "apps/web", err: "the root's app has shop.test"},
		{name: "the bare name is listed", apps: apps, domains: listed, dir: "apps/web", err: "shop.test is listed: give it the app's port with e in the list"},
		{name: "a port lodo can't tell", apps: apps, dir: "apps/api", label: "backend", err: "lodo can't tell its port, so the name wouldn't stick"},
		{name: "another app's name", apps: apps, dir: "apps/web", label: "api", err: "apps/api has api.shop.test"},
		{
			name: "listed on another port", apps: apps, dir: "apps/web", label: "mail", err: "mail.shop.test is listed on port 8025, not this app's 3000",
			domains: []store.Domain{{Name: "mail.shop.test", Address: "127.0.1.1", Port: 8025, Enabled: true}},
		},
		{
			name: "listed on the app's port", apps: apps, dir: "apps/web", label: "front",
			domains: []store.Domain{{Name: "front.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := CheckLabel(Project{Root: "/p/shop", Name: "shop.test", Apps: tt.apps}, tt.domains, "shop.test", tt.dir, tt.label)
			if (err == nil) != (tt.err == "") || err != nil && !strings.Contains(err.Error(), tt.err) {
				t.Errorf("CheckLabel = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestFind_NoDevOrigins(t *testing.T) {
	t.Parallel()

	p, _ := Find(project(t, "shop", map[string]string{
		"package.json":               `{"scripts": {"dev": "turbo dev"}}`,
		"pnpm-workspace.yaml":        "packages: [apps/*]\n",
		"apps/web/package.json":      `{"scripts": {"dev": "next dev"}}`,
		"apps/web/next.config.ts":    "export default { allowedDevOrigins: ['shop.test', '*.shop.test'] }\n",
		"apps/admin/package.json":    `{"scripts": {"dev": "next dev -p 3001"}}`,
		"apps/admin/next.config.mjs": "export default {}\n",
		"apps/blog/package.json":     `{"scripts": {"dev": "next dev -p 3002"}}`,
		"apps/vite/package.json":     `{"scripts": {"dev": "vite"}}`,
	}), nil)
	if want := []string{"apps/admin", "apps/blog"}; !slices.Equal(p.NoDevOrigins, want) {
		t.Errorf("NoDevOrigins = %q, want %q", p.NoDevOrigins, want)
	}
}

// TestPropose_BareName checks that the root's scripts keep the project's
// address when an app takes the project's name.
func TestPropose_BareName(t *testing.T) {
	t.Parallel()

	root := project(t, "shop", map[string]string{
		"package.json":          `{"scripts": {"dev": "turbo dev", "docs": "next dev -p 3100"}}`,
		"pnpm-workspace.yaml":   "packages: [apps/*]\n",
		"apps/web/package.json": `{"scripts": {"dev": "next dev"}}`,
	})
	p, _ := Find(root, nil)
	got := Propose(p, nil, "", map[string]string{"apps/web": ""})
	if want := []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: root}}; !slices.Equal(got.Add, want) {
		t.Errorf("Add = %+v, want %+v", got.Add, want)
	}
	want := []Fix{
		{File: "package.json", Line: 1, Old: `{"scripts": {"dev": "turbo dev", "docs": "next dev -p 3100"}}`, New: `{"scripts": {"dev": "turbo dev", "docs": "next dev -H 127.0.1.1 -p 3100"}}`},
		{File: "apps/web/package.json", Line: 1, Old: `{"scripts": {"dev": "next dev"}}`, New: `{"scripts": {"dev": "next dev -H 127.0.1.1"}}`},
	}
	if !slices.Equal(got.Fixes, want) {
		t.Errorf("Fixes =\n%+v\nwant\n%+v", got.Fixes, want)
	}
}
