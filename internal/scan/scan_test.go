package scan

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/store"
)

// gone reports whether nothing is at path, as lodo's backend does.
func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

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
			want:  []App{{Dir: ".", Port: 3000}},
		},
		{
			name: "dev.sh that reads the host from .env",
			files: map[string]string{
				"package.json":   `{"scripts": {"dev": "./scripts/dev.sh"}}`,
				"scripts/dev.sh": "docker compose -f docker/compose.dev.yaml up -d\nexec next dev ${HOST_IP:+--hostname \"$HOST_IP\"}\n",
			},
			want: []App{{Dir: ".", Port: 3000}},
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
				{Dir: "apps/admin", Port: 3001},
				{Dir: "apps/api"},
				{Dir: "apps/storefront", Port: 3002},
				{Dir: "packages/webapp", Port: 5173},
			},
		},
		{
			name: "package.json workspaces object",
			files: map[string]string{
				"package.json":               `{"workspaces": {"packages": ["apps/*"]}, "scripts": {"dev": "turbo dev"}}`,
				"apps/client/package.json":   `{"scripts": {"dev": "vite"}}`,
				"apps/client/vite.config.ts": "export default { server: { port: 5175 } }",
			},
			want: []App{{Dir: "apps/client", Port: 5175}},
		},
		{
			name: "single app with a workspace file for pnpm settings",
			files: map[string]string{
				"package.json":        `{"scripts": {"dev": "next dev"}}`,
				"pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n",
			},
			want: []App{{Dir: ".", Port: 3000}},
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
	live := filepath.Dir(elsewhere) // another project's folder, still there
	inbox := []Page{{Label: "mail", Port: 8025}}
	monorepo := []App{
		{Dir: "apps/storefront", Port: 3000}, {Dir: "apps/admin", Port: 3000},
		{Dir: "apps/api"}, {Dir: "apps/email", Port: 3003},
	}
	tests := []struct {
		name    string
		project Project
		domains []store.Domain
		chosen  string            // the name the user chose
		dirs    map[string]string // want.Dirs, checked when set
		want    Proposal
	}{
		{
			name:    "new app with a compose file",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "blog.test", Address: "127.0.1.1", Enabled: true}},
			want: Proposal{
				Address: "127.0.1.2",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: "/p/shop"}},
				Compose: compose,
			},
		},
		{
			name:    "the .env's address",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Port: 3000}}, EnvAddress: "127.0.1.7"},
			want: Proposal{
				Address: "127.0.1.7",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.7", Port: 3000, Enabled: true, Root: "/p/shop"}},
			},
		},
		{
			name:    "the .env's address belongs to another project",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Port: 3000}}, EnvAddress: "127.0.1.1"},
			domains: []store.Domain{{Name: "blog.test", Address: "127.0.1.1", Enabled: true}},
			want: Proposal{
				Address: "127.0.1.2",
				Add:     []store.Domain{{Name: "shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: "/p/shop"}},
			},
		},
		{
			name:    "monorepo: an app on a taken port gets the next address, unknown port gets a note",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: monorepo},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "storefront.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
					{Name: "admin.shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
					{Name: "api.shop.test", Address: "127.0.1.1", Enabled: true},
					{Name: "email.shop.test", Address: "127.0.1.1", Port: 3003, Enabled: true},
				},
				Notes: []string{
					"api.shop.test: lodo can't tell its port; set it in lodo dev --edit",
					"storefront.shop.test and admin.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port",
				},
			},
		},
		{
			name:    "listed project: only what is missing",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: monorepo},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: store.NoCompose},
				{Name: "storefront.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 4000, Enabled: true},
			},
			want: Proposal{
				Notes:   []string{"storefront.shop.test and admin.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port"},
				Address: "127.0.1.4",
				Listed:  true,
				Claim:   true,
				Add: []store.Domain{
					{Name: "admin.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
					{Name: "email.shop.test", Address: "127.0.1.4", Port: 3003, Enabled: true},
				},
			},
		},
		{
			name:    "fully listed and linked",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true, Root: "/p/shop", Compose: compose}},
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
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Apps: []App{{Dir: ".", Port: 3000}}, Pages: inbox},
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
			want: Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name:    "another name on the inbox's port serves it",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Pages: inbox},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: compose},
				{Name: "inbox.shop.test", Address: "127.0.1.4", Port: 8025, Enabled: true},
			},
			want: Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name:    "a project set to no compose file gets no inbox",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose}, Pages: inbox},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: store.NoCompose}},
			want:    Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name:    "a project that runs another compose file gets no inbox from the first",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose, prod}, Pages: inbox},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Enabled: true, Compose: prod}},
			want:    Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name:    "nor when another of its names links that file",
			project: Project{Root: "/p/shop", Name: "shop.test", Compose: []string{compose, prod}, Pages: inbox},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 4000, Enabled: true, Compose: prod},
			},
			want: Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
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
			want:    Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name: "a renamed folder: the user's name claims it and links its file",
			project: Project{
				Root: "/p/ai-calling", Name: "ai-calling.test", Compose: []string{"/p/ai-calling/docker/compose.dev.yaml"},
				Apps: []App{{Dir: ".", Port: 3000}},
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
			project: Project{Root: "/p/platform-v2", Name: "platform-v2.test", Compose: []string{"/p/platform-v2/docker/compose.dev.yml"}, Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "hugger.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Compose: "/p/platform-v2/docker/compose.dev.yml"}},
			want:    Proposal{Name: "hugger.test", Address: "127.0.1.1", Listed: true, Claim: true},
		},
		{
			name:    "a compose file in it that another live project links names nothing",
			project: Project{Root: filepath.Dir(live), Name: "umbrella.test", Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "api.test", Address: "127.0.1.1", Enabled: true, Root: live, Compose: elsewhere}},
			want: Proposal{
				Address: "127.0.1.2",
				Add:     []store.Domain{{Name: "umbrella.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: filepath.Dir(live)}},
			},
		},
		{
			name:    "the user's name: subdomains follow it",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Port: 3000}, {Dir: "apps/admin", Port: 5173}}},
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
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: "/p/shop-v2"}},
			want:    Proposal{Name: "shop.test", Address: "127.0.1.1", Listed: true},
		},
		{
			name:    "the user names a listed name: it claims the folder",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true}},
			chosen:  "shop.test",
			want:    Proposal{Name: "shop.test", Address: "127.0.1.1", Listed: true, Claim: true},
		},
		{
			name:    "the user names a name another project's folder still has: only a note",
			project: Project{Root: "/p/shop-v2", Name: "shop-v2.test", Apps: []App{{Dir: ".", Port: 3000}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true, Root: live}},
			chosen:  "shop.test",
			want:    Proposal{Name: "shop.test", Notes: []string{"shop.test belongs to " + live + ": give this project another name"}},
		},
		{
			name:    "a name with two labels",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: ".", Port: 3000}}},
			chosen:  "web.shop.test",
			want:    Proposal{Name: "", Notes: []string{"web.shop.test: a project's name has one label before .test"}},
		},
		{
			name:    "nest gets its port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/server", Port: 3000}}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "server.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
				},
			},
		},
		{
			name:    "two apps on one port get two addresses",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/api", Port: 3000}, {Dir: "apps/mail", Port: 3000}}},
			want: Proposal{
				Notes:   []string{"api.shop.test and mail.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port"},
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "api.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
					{Name: "mail.shop.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
				},
			},
		},
		{name: "nothing to serve", project: Project{Root: "/p/dotfiles", Name: "dotfiles.test"}},
		{
			name:    "nest without a port gets a note",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/server"}}},
			want: Proposal{
				Address: "127.0.1.1",
				Add: []store.Domain{
					{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
					{Name: "server.shop.test", Address: "127.0.1.1", Enabled: true},
				},
				Notes: []string{
					"server.shop.test: lodo can't tell its port; set it in lodo dev --edit",
				},
			},
		},
		{
			name:    "an app listed without a port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Port: 3001}, {Dir: "apps/server"}}},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "web.shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "server.shop.test", Address: "127.0.1.4", Enabled: true},
			},
			want: Proposal{
				Address: "127.0.1.4", Listed: true, Claim: true,
				Notes: []string{
					"web.shop.test: listed without a port; its dev server takes 3001: set its port in lodo dev --edit",
					"server.shop.test: listed without a port, and lodo can't tell its dev server's; set its port in lodo dev --edit",
				},
			},
		},
		{
			name:    "an app keeps the listed name on its port",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/web", Port: 3001}}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3001, Enabled: true}},
			want:    Proposal{Address: "127.0.1.4", Listed: true, Claim: true},
		},
		{
			name: "a name another folder is named after is no alias",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/api", Port: 3000}, {Dir: "apps/web", Port: 3000},
			}},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.4", Enabled: true},
				{Name: "api.shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true},
			},
			want: Proposal{
				Notes:   []string{"api.shop.test and web.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port"},
				Address: "127.0.1.4", Listed: true, Claim: true,
				Add: []store.Domain{{Name: "web.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true}},
			},
		},
		{
			name: "two apps on one listed port take one name",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{
				{Dir: "apps/docs", Port: 3000}, {Dir: "apps/web", Port: 3000},
			}},
			domains: []store.Domain{{Name: "shop.test", Address: "127.0.1.4", Port: 3000, Enabled: true}},
			want: Proposal{
				Notes:   []string{"shop.test and web.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port"},
				Address: "127.0.1.4", Listed: true, Claim: true,
				Add: []store.Domain{{Name: "web.shop.test", Address: "127.0.1.1", Port: 3000, Enabled: true}},
			},
		},
		{
			name:    "a listed name keeps its app's folder",
			project: Project{Root: "/p/shop", Name: "shop.test", Apps: []App{{Dir: "apps/docs"}}},
			domains: []store.Domain{
				{Name: "shop.test", Address: "127.0.1.1", Enabled: true, Root: "/p/shop"},
				{Name: "docs.shop.test", Address: "127.0.1.1", Port: 4100, Enabled: true},
			},
			dirs: map[string]string{"docs.shop.test": "apps/docs"},
			want: Proposal{Address: "127.0.1.1", Listed: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Propose(tt.project, tt.domains, tt.chosen, gone)
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
		slices.Equal(a.Add, b.Add) && a.Compose == b.Compose && slices.Equal(a.Env, b.Env) &&
		slices.Equal(a.Notes, b.Notes)
}

// TestPropose_SameFolderName scans a second folder called flowy while
// flowy.test is listed: when another flowy's folder still has that name, the
// scan proposes the first free flowy-<n>.test on a free address with every
// app; when that folder is gone or unset, it takes flowy.test over.
func TestPropose_SameFolderName(t *testing.T) {
	t.Parallel()

	first := project(t, "flowy", nil)
	second := Project{Root: "/p/work/flowy", Name: "flowy.test", Apps: []App{{Dir: "apps/web", Port: 3000}, {Dir: "apps/api", Port: 4000}}}
	clone := func(name, address string) []store.Domain {
		return []store.Domain{
			{Name: name, Address: address, Enabled: true, Root: second.Root},
			{Name: "web." + name, Address: address, Port: 3000, Enabled: true},
			{Name: "api." + name, Address: address, Port: 4000, Enabled: true},
		}
	}
	tests := []struct {
		name  string
		root  string         // flowy.test's folder
		extra []store.Domain // listed next to flowy.test
		want  Proposal
	}{
		{
			name: "the other flowy is still there",
			root: first,
			want: Proposal{Name: "flowy-2.test", Address: "127.0.1.3", Add: clone("flowy-2.test", "127.0.1.3")},
		},
		{
			name:  "a name of flowy-2 is listed too",
			root:  first,
			extra: []store.Domain{{Name: "api.flowy-2.test", Address: "127.0.1.3", Port: 4000, Enabled: true}},
			want:  Proposal{Name: "flowy-3.test", Address: "127.0.1.4", Add: clone("flowy-3.test", "127.0.1.4")},
		},
		{
			name: "the other flowy is gone",
			root: filepath.Join(filepath.Dir(first), "moved"),
			want: Proposal{Name: "flowy.test", Address: "127.0.1.2", Listed: true, Claim: true, Add: clone("flowy.test", "127.0.1.2")[2:]},
		},
		{
			name: "flowy.test has no folder",
			want: Proposal{Name: "flowy.test", Address: "127.0.1.2", Listed: true, Claim: true, Add: clone("flowy.test", "127.0.1.2")[2:]},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := append([]store.Domain{
				{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
				{Name: "flowy.test", Address: "127.0.1.2", Enabled: true, Root: tt.root},
				{Name: "web.flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true},
			}, tt.extra...)
			got := Propose(second, domains, "", gone)
			tt.want.Root = second.Root
			if !equal(got, tt.want) {
				t.Errorf("Propose =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// TestPropose_AnotherPath scans a listed project through another spelling of
// its folder, as os.Getwd gives it after cd ~/projects/FLOWY: it is the same
// project, with nothing to add and no compose file to link.
func TestPropose_AnotherPath(t *testing.T) {
	t.Parallel()

	root := project(t, "flowy", map[string]string{"package.json": `{"scripts": {"dev": "next dev"}}`, "compose.yaml": "services: {}\n"})
	upper := strings.ToUpper(root)
	if _, err := os.Stat(upper); err != nil {
		t.Skip("this file system tells case apart")
	}
	p, ok := Find(upper, nil)
	if !ok {
		t.Fatal("Find: not a project")
	}
	domains := []store.Domain{{Name: "flowy.test", Address: "127.0.1.2", Port: 3000, Enabled: true, Root: root, Compose: filepath.Join(root, "compose.yaml")}}
	got := Propose(p, domains, "", gone)
	if want := (Proposal{Root: upper, Name: "flowy.test", Address: "127.0.1.2", Listed: true}); !equal(got, want) {
		t.Errorf("Propose =\n%+v\nwant\n%+v", got, want)
	}
}
