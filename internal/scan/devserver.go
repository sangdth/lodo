package scan

import (
	"cmp"
	"encoding/json"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/fsutil"
)

// Fix is a .env line of a project and what it becomes, which lodo writes on
// y (ApplyEnv): Old and New hold it with user info masked, and before and
// after hold the real text.
type Fix struct {
	File string `json:"file"` // from the project's root, as apps/web/.env
	Line int    `json:"line"` // 1-based
	Old  string `json:"old"`  // the line before, without its line end
	New  string `json:"new"`  // the line after

	before, after string
}

// tool is a dev server lodo knows: the command that starts it and the port it
// takes without one.
type tool struct {
	name    string
	cmd     *regexp.Regexp // the command; a group, when it has one, is the word after it
	verbs   []string       // the words cmd's group may hold: vite dev, but not vite build
	port    int            // the port without a port option
	configs []string       // config files that may set the port, from the app's folder
	dotenv  bool           // reads PORT from the app's .env before it listens
}

// configNames returns name with each extension a JavaScript config file uses.
func configNames(name string) []string {
	var names []string
	for _, ext := range []string{".ts", ".js", ".mjs", ".mts", ".cjs", ".cts"} {
		names = append(names, name+ext)
	}
	return names
}

// tools are the dev servers lodo knows, with the port each takes by default.
// vite covers SvelteKit's vite dev and Remix's remix vite:dev.
var tools = []tool{
	{name: "next", cmd: regexp.MustCompile(`\bnext[ \t]+dev\b`), port: 3000},
	{
		name: "vite", cmd: regexp.MustCompile(`\bvite(?::dev)?\b(?:[ \t]+([a-z][\w-]*))?`), verbs: []string{"dev", "serve"},
		port: 5173, configs: configNames("vite.config"),
	},
	{name: "astro", cmd: regexp.MustCompile(`\bastro[ \t]+dev\b`), port: 4321, configs: configNames("astro.config")},
	{name: "nuxt", cmd: regexp.MustCompile(`\bnux[it][ \t]+dev\b`), port: 3000},
	{name: "wrangler", cmd: regexp.MustCompile(`\bwrangler[ \t]+dev\b`), port: 8787},
	{name: "react-router", cmd: regexp.MustCompile(`\breact-router[ \t]+dev\b`), port: 5173},
	{name: "angular", cmd: regexp.MustCompile(`\bng[ \t]+serve\b`), port: 4200},
	{name: "gatsby", cmd: regexp.MustCompile(`\bgatsby[ \t]+develop\b`), port: 8000},
	{name: "docusaurus", cmd: regexp.MustCompile(`\bdocusaurus[ \t]+start\b`), port: 3000},
	{name: "storybook", cmd: regexp.MustCompile(`\bstorybook[ \t]+dev\b`), port: 6006},
	{name: "vue-cli", cmd: regexp.MustCompile(`\bvue-cli-service[ \t]+serve\b`), port: 8080},
	{name: "webpack", cmd: regexp.MustCompile(`\bwebpack(?:-dev-server\b|[ \t]+serve\b)`), port: 8080},
	{name: "nest", cmd: regexp.MustCompile(`\bnest[ \t]+start\b`), dotenv: true}, // the Nest CLI has no port of its own
	{name: "create-react-app", cmd: regexp.MustCompile(`\breact-scripts[ \t]+start\b`), port: 3000, dotenv: true},
	{name: "react-email", cmd: regexp.MustCompile(`\bemail[ \t]+dev\b`), port: 3000},
}

// start is a dev server's command in a line: the tool, where its command
// starts, and its options, up to &, ;, | or the line's end.
type start struct {
	tool     *tool
	from     int
	argsText string
}

// starts returns the dev servers line starts, in order. A package.json
// script ends at a double quote; a shell line doesn't, since its quotes hold
// arguments.
func starts(line string, shell bool) []start {
	ends := `&;|"`
	if shell {
		ends = `&;|`
	}
	var found []start
	for i := range tools {
		t := &tools[i]
		for _, m := range t.cmd.FindAllStringSubmatchIndex(line, -1) {
			if !t.accepts(line, m) {
				continue
			}
			if len(m) > 2 && m[2] >= 0 && !slices.Contains(t.verbs, line[m[2]:m[3]]) {
				continue // vite build
			}
			cmdEnd := m[1]
			argsEnd := len(line)
			if i := strings.IndexAny(line[cmdEnd:], ends); i >= 0 {
				argsEnd = cmdEnd + i
			}
			found = append(found, start{tool: t, from: m[0], argsText: line[cmdEnd:argsEnd]})
		}
	}
	slices.SortFunc(found, func(a, b start) int { return cmp.Compare(a.from, b.from) })
	return found
}

// accepts drops a match that is part of a longer word, such as vite-node or
// vite.config.
func (t *tool) accepts(line string, m []int) bool {
	end := m[1]
	if len(m) > 2 && m[2] >= 0 {
		end = m[2] - 1 // the word after the command was checked on its own
		for end > m[0] && (line[end] == ' ' || line[end] == '\t') {
			end--
		}
		end++
	}
	return end >= len(line) || !strings.ContainsRune("-./:@", rune(line[end]))
}

// portRE matches a port option. The group is its value.
var portRE = regexp.MustCompile(`(?:^|[ \t])(?:-p|--port)(?:=|[ \t]+)([^ \t]*)`)

// envPortRE matches PORT=3100 before a command. The group is the port.
var envPortRE = regexp.MustCompile(`(?:^|[\s"'])PORT=(\d+)\b`)

// configPortRE matches a port set in a config file. The group is the port,
// empty when the file computes it.
var configPortRE = regexp.MustCompile(`\bport\s*:\s*(\d+\b)?`)

// server is the dev server an app's dev script starts.
type server struct {
	tool *tool
	port int // 0 when lodo can't tell
}

// serverIn returns the dev server the first line of text that starts one
// starts, with its port: the port option, PORT= before the command, the port
// in the app's config file, or the tool's own.
func serverIn(text, dir string, shell bool) (server, bool) {
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if shell && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		found := starts(line, shell)
		if len(found) == 0 {
			continue
		}
		s := found[0]
		srv := server{tool: s.tool}
		before := line[:s.from]
		if i := strings.LastIndexAny(before, `&;|`); i >= 0 {
			before = before[i+1:]
		}
		switch m, e := portRE.FindStringSubmatch(s.argsText), envPortRE.FindStringSubmatch(before); {
		case m != nil:
			srv.port, _ = strconv.Atoi(m[1]) // a variable leaves 0
		case e != nil:
			srv.port, _ = strconv.Atoi(e[1])
		default:
			srv.port = configPort(dir, s.tool)
		}
		return srv, true
	}
	return server{}, false
}

// configPort returns the port the tool's config file in dir sets, else PORT
// in the app's .env for a tool that reads it, else the tool's own, and 0 when
// the file computes it.
func configPort(dir string, t *tool) int {
	for _, name := range t.configs {
		data, err := fsutil.ReadRegular(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		m := configPortRE.FindSubmatch(data)
		if m == nil {
			return t.port
		}
		port, _ := strconv.Atoi(string(m[1]))
		return port
	}
	if t.dotenv {
		if port := envPort(dir); port > 0 {
			return port
		}
	}
	return t.port
}

// packageJSON is what lodo reads from a package.json.
type packageJSON struct {
	Scripts    map[string]string `json:"scripts"`
	Workspaces json.RawMessage   `json:"workspaces"`
}

// readPackage reads the package.json in dir.
func readPackage(dir string) (packageJSON, bool) {
	data, err := fsutil.ReadRegular(filepath.Join(dir, "package.json"))
	if err != nil {
		return packageJSON{}, false
	}
	var pkg packageJSON
	if json.Unmarshal(data, &pkg) != nil {
		return packageJSON{}, false
	}
	return pkg, true
}

// shellScriptRE matches a shell script a package.json script runs, such as
// ./scripts/dev.sh or sh scripts/dev.sh. The group is its path.
var shellScriptRE = regexp.MustCompile(`(?:^|[\s"'=])(?:\./)?((?:[\w.-]+/)*[\w.-]+\.sh)\b`)

// shellScripts returns the shell scripts inside dir that scripts run, from
// dir, sorted.
func shellScripts(scripts ...string) []string {
	found := map[string]bool{}
	for _, script := range scripts {
		for _, m := range shellScriptRE.FindAllStringSubmatch(script, -1) {
			if filepath.IsLocal(m[1]) {
				found[filepath.Clean(m[1])] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}

// runScriptRE matches a script that runs another script of the same
// package.json: npm:next in concurrently, npm run next, pnpm next. The group
// is the other script's name.
var runScriptRE = regexp.MustCompile(`(?:\bnpm:|\b(?:npm|pnpm|yarn|bun)[ \t]+(?:run[ \t]+)?)([\w:-]+)`)

// devServer returns the dev server the dev script in dir starts: in the
// script, in the scripts of the same package.json it runs, or in the shell
// scripts it runs. A dev command lodo doesn't know still gives its port when
// it sets one with -p or --port, as react-email and prisma studio do, else
// PORT in the app's .env. known
// reports whether lodo knows the command; hasDev whether dir's package.json
// has a dev script at all.
func devServer(dir string) (srv server, known, hasDev bool) {
	pkg, ok := readPackage(dir)
	dev, hasDev := pkg.Scripts["dev"]
	if !ok || !hasDev {
		return server{}, false, false
	}
	texts := devScripts(pkg)
	for _, text := range texts {
		if srv, ok := serverIn(text, dir, false); ok {
			return srv, true, true
		}
	}
	for _, rel := range shellScripts(texts...) {
		data, err := fsutil.ReadRegular(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		if srv, ok := serverIn(string(data), dir, true); ok {
			return srv, true, true
		}
	}
	if m := portRE.FindStringSubmatch(dev); m != nil {
		srv.port, _ = strconv.Atoi(m[1])
	} else {
		srv.port = envPort(dir)
	}
	return srv, false, true
}

// devScripts returns the dev script and the scripts of the same package.json
// it runs.
func devScripts(pkg packageJSON) []string {
	dev, ok := pkg.Scripts["dev"]
	if !ok {
		return nil
	}
	texts := []string{dev}
	for _, m := range runScriptRE.FindAllStringSubmatch(dev, -1) {
		if script, ok := pkg.Scripts[m[1]]; ok && m[1] != "dev" {
			texts = append(texts, script)
		}
	}
	return texts
}
