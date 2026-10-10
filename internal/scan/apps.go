package scan

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sangdth/lodo/internal/fsutil"
)

// maxApps is the most apps Find lists in one project.
const maxApps = 50

// App is a folder of a project whose dev script starts a dev server.
type App struct {
	Dir  string `json:"dir"`            // from the project's root, with slashes; . for the root
	Tool string `json:"tool,omitempty"` // next, vite, astro, nuxt or wrangler; empty when lodo doesn't know the command
	Port int    `json:"port,omitempty"` // 0 when lodo can't tell
}

// findApps returns the apps in the project at root. Without workspaces, the
// root is the app when its package.json has a dev script. In a workspace, an
// app is a package whose dev script starts a dev server lodo knows, or any
// package under apps/ with a dev script; the root counts only when its own
// dev script starts one, not when it runs turbo.
func findApps(root string) []App {
	dirs := workspaces(root)
	var apps []App
	if srv, known, hasDev := devServer(root); known || (hasDev && len(dirs) == 0) {
		apps = append(apps, newApp(".", srv))
	}
	for _, dir := range dirs {
		srv, known, hasDev := devServer(filepath.Join(root, filepath.FromSlash(dir)))
		if known || (hasDev && strings.HasPrefix(dir, "apps/")) {
			apps = append(apps, newApp(dir, srv))
		}
		if len(apps) == maxApps {
			break
		}
	}
	return apps
}

func newApp(dir string, srv server) App {
	a := App{Dir: dir, Port: srv.port}
	if srv.tool != nil {
		a.Tool = srv.tool.name
	}
	return a
}

// workspaces returns the package folders the root's pnpm-workspace.yaml or
// package.json workspaces name, from root, with slashes, sorted. ** counts as
// one folder, and exclusions are skipped: a folder they would drop is only
// one more app to look at.
func workspaces(root string) []string {
	var patterns []string
	if data, err := fsutil.ReadRegular(filepath.Join(root, "pnpm-workspace.yaml"), fsutil.MaxProjectFile); err == nil {
		var ws struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(data, &ws) == nil {
			patterns = append(patterns, ws.Packages...)
		}
	}
	if pkg, _, ok := readPackage(root); ok && len(pkg.Workspaces) > 0 {
		var list []string
		var obj struct {
			Packages []string `json:"packages"`
		}
		if json.Unmarshal(pkg.Workspaces, &list) == nil {
			patterns = append(patterns, list...)
		} else if json.Unmarshal(pkg.Workspaces, &obj) == nil {
			patterns = append(patterns, obj.Packages...)
		}
	}
	found := map[string]bool{}
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		if p == "" || strings.HasPrefix(p, "!") {
			continue
		}
		p = strings.ReplaceAll(p, "**", "*")
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		for _, m := range matches {
			rel, err := filepath.Rel(root, m)
			if err != nil || !filepath.IsLocal(rel) || slices.Contains(strings.Split(rel, string(filepath.Separator)), "node_modules") {
				continue
			}
			if info, err := os.Stat(filepath.Join(m, "package.json")); err == nil && info.Mode().IsRegular() {
				found[filepath.ToSlash(rel)] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}
