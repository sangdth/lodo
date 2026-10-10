package scan

import (
	"cmp"
	"encoding/json"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/fsutil"
)

// Fix is a line that starts a dev server on every address, with the host
// option added so it listens on its name's address only. lodo shows it and
// never writes it: the file is the project's.
type Fix struct {
	File string `json:"file"` // from the project's root, as apps/web/package.json or scripts/dev.sh
	Line int    `json:"line"` // 1-based
	Old  string `json:"old"`  // the line before, without its line end
	New  string `json:"new"`  // the line after
}

// tool is a dev server lodo knows: the command that starts it, the option
// that sets the address it listens on, and the port it takes without one.
type tool struct {
	name    string
	cmd     *regexp.Regexp // the command; a group, when it has one, is the word after it
	verbs   []string       // the words cmd's group may hold: vite dev, but not vite build
	host    string         // the host option lodo adds; empty when the tool has none, such as nest
	hostRE  *regexp.Regexp // the host options it reads: group 1 the option, 2 or 3 its value; nil without host
	port    int            // the port without a port option
	configs []string       // config files that may set the port, from the app's folder
}

// hostOption builds a hostRE for the options in alt, such as -H|--hostname.
// The value follows = or blanks; a bare option has none.
func hostOption(alt string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^\w-])(` + alt + `)(?:=([^ \t]*)|[ \t]+([^ \t]*)|[ \t]*$)`)
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
// vite covers SvelteKit's vite dev and Remix's remix vite:dev. nest, create
// react app and react-email take no host option: the app's code or HOST sets
// where they listen.
var tools = []tool{
	{name: "next", cmd: regexp.MustCompile(`\bnext[ \t]+dev\b`), host: "-H", hostRE: hostOption(`-H|--hostname`), port: 3000},
	{
		name: "vite", cmd: regexp.MustCompile(`\bvite(?::dev)?\b(?:[ \t]+([a-z][\w-]*))?`), verbs: []string{"dev", "serve"},
		host: "--host", hostRE: hostOption(`--host`), port: 5173, configs: configNames("vite.config"),
	},
	{
		name: "astro", cmd: regexp.MustCompile(`\bastro[ \t]+dev\b`), host: "--host", hostRE: hostOption(`--host`),
		port: 4321, configs: configNames("astro.config"),
	},
	{name: "nuxt", cmd: regexp.MustCompile(`\bnux[it][ \t]+dev\b`), host: "--host", hostRE: hostOption(`--host`), port: 3000},
	{name: "wrangler", cmd: regexp.MustCompile(`\bwrangler[ \t]+dev\b`), host: "--ip", hostRE: hostOption(`--ip`), port: 8787},
	{name: "react-router", cmd: regexp.MustCompile(`\breact-router[ \t]+dev\b`), host: "--host", hostRE: hostOption(`--host`), port: 5173},
	{name: "angular", cmd: regexp.MustCompile(`\bng[ \t]+serve\b`), host: "--host", hostRE: hostOption(`--host`), port: 4200},
	{name: "gatsby", cmd: regexp.MustCompile(`\bgatsby[ \t]+develop\b`), host: "-H", hostRE: hostOption(`-H|--host`), port: 8000},
	{name: "docusaurus", cmd: regexp.MustCompile(`\bdocusaurus[ \t]+start\b`), host: "--host", hostRE: hostOption(`--host`), port: 3000},
	{name: "storybook", cmd: regexp.MustCompile(`\bstorybook[ \t]+dev\b`), host: "--host", hostRE: hostOption(`--host`), port: 6006},
	{name: "vue-cli", cmd: regexp.MustCompile(`\bvue-cli-service[ \t]+serve\b`), host: "--host", hostRE: hostOption(`--host`), port: 8080},
	{
		name: "webpack", cmd: regexp.MustCompile(`\bwebpack(?:-dev-server\b|[ \t]+serve\b)`), host: "--host", hostRE: hostOption(`--host`),
		port: 8080,
	},
	{name: "nest", cmd: regexp.MustCompile(`\bnest[ \t]+start\b`), port: 3000},
	{name: "create-react-app", cmd: regexp.MustCompile(`\breact-scripts[ \t]+start\b`), port: 3000},
	{name: "react-email", cmd: regexp.MustCompile(`\bemail[ \t]+dev\b`), port: 3000},
}

// HasHost reports whether the dev server lodo calls name takes a host option,
// so lodo can show the line that makes it listen on one address.
func HasHost(name string) bool {
	i := slices.IndexFunc(tools, func(t tool) bool { return t.name == name })
	return i >= 0 && tools[i].host != ""
}

// start is a dev server's command in a line: the tool, where its command ends,
// and where the rest of that command ends.
type start struct {
	tool     *tool
	from     int // where the command starts
	cmdEnd   int // where its name ends: after next dev
	argsEnd  int // where its options end: at &, ;, | or the line's end
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
			found = append(found, start{tool: t, from: m[0], cmdEnd: cmdEnd, argsEnd: argsEnd, argsText: line[cmdEnd:argsEnd]})
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

// fixLine gives each dev server line starts the host address: the host
// option after a command without one, address in place of a fixed host, or
// address after a bare option such as vite --host, which means every
// address. A port above 0 goes into its port option too, or after the host
// when it has none: every tool with a host option takes --port.
func fixLine(line, address string, port int, shell bool) string {
	var edits []edit
	for _, s := range starts(line, shell) {
		if s.tool.host == "" {
			continue // nest: only its code can choose the address
		}
		if e, ok := hostEdit(s, address); ok {
			edits = append(edits, e)
		}
		if e, ok := portEdit(s, port); ok && port > 0 {
			edits = append(edits, e)
		}
	}
	if len(edits) == 0 {
		return line
	}
	slices.SortStableFunc(edits, func(a, b edit) int { return cmp.Compare(a.from, b.from) })
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(line[last:e.from] + e.text)
		last = e.to
	}
	return b.String() + line[last:]
}

// edit replaces line[from:to] with text.
type edit struct {
	from, to int
	text     string
}

// hostEdit returns the edit that makes s listen on address. ok is false when
// it can't: its host is a variable or quoted.
func hostEdit(s start, address string) (edit, bool) {
	h := s.tool.hostRE.FindStringSubmatchIndex(s.argsText)
	if h == nil {
		return edit{s.cmdEnd, s.cmdEnd, " " + s.tool.host + " " + address}, true
	}
	valueStart, valueEnd := h[4], h[5] // after =
	if valueStart < 0 {
		valueStart, valueEnd = h[6], h[7] // after blanks
	}
	value := ""
	if valueStart >= 0 {
		value = s.argsText[valueStart:valueEnd]
	}
	switch {
	case value == "" || strings.HasPrefix(value, "-"):
		if h[4] >= 0 {
			return edit{s.cmdEnd + h[4], s.cmdEnd + h[4], address}, true
		}
		return edit{s.cmdEnd + h[3], s.cmdEnd + h[3], " " + address}, true
	case isFixedHost(value) && value != address:
		return edit{s.cmdEnd + valueStart, s.cmdEnd + valueEnd, address}, true
	}
	return edit{}, false
}

// portEdit returns the edit that starts s on port: port in place of its port
// option's number, or --port after the command without one. ok is false when
// the option holds a variable, or port already.
func portEdit(s start, port int) (edit, bool) {
	want := strconv.Itoa(port)
	m := portRE.FindStringSubmatchIndex(s.argsText)
	if m == nil {
		return edit{s.cmdEnd, s.cmdEnd, " --port " + want}, true
	}
	value := s.argsText[m[2]:m[3]]
	if _, err := strconv.Atoi(value); err != nil || value == want {
		return edit{}, false
	}
	return edit{s.cmdEnd + m[2], s.cmdEnd + m[3], want}, true
}

// isFixedHost reports whether a host option's value is a plain address that
// lodo can replace: not quoted, escaped or a variable.
func isFixedHost(value string) bool {
	return !strings.ContainsAny(value, `$'"\`)
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

// configPort returns the port the tool's config file in dir sets, the tool's
// own when none does, and 0 when the file computes it.
func configPort(dir string, t *tool) int {
	for _, name := range t.configs {
		data, err := fsutil.ReadRegular(filepath.Join(dir, name), fsutil.MaxProjectFile)
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
	return t.port
}

// packageJSON is what lodo reads from a package.json.
type packageJSON struct {
	Scripts    map[string]string `json:"scripts"`
	Workspaces json.RawMessage   `json:"workspaces"`
}

// readPackage reads the package.json in dir: its scripts and their text.
func readPackage(dir string) (packageJSON, string, bool) {
	data, err := fsutil.ReadRegular(filepath.Join(dir, "package.json"), fsutil.MaxProjectFile)
	if err != nil {
		return packageJSON{}, "", false
	}
	var pkg packageJSON
	if json.Unmarshal(data, &pkg) != nil {
		return packageJSON{}, string(data), false
	}
	return pkg, string(data), true
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
// it sets one with -p or --port, as react-email and prisma studio do. known
// reports whether lodo knows the command; hasDev whether dir's package.json
// has a dev script at all.
func devServer(dir string) (srv server, known, hasDev bool) {
	pkg, _, ok := readPackage(dir)
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
		data, err := fsutil.ReadRegular(filepath.Join(dir, rel), fsutil.MaxProjectFile)
		if err != nil {
			continue
		}
		if srv, ok := serverIn(string(data), dir, true); ok {
			return srv, true, true
		}
	}
	if m := portRE.FindStringSubmatch(dev); m != nil {
		srv.port, _ = strconv.Atoi(m[1])
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

// fixesIn returns the fixes for the dev servers in the scripts of dir's
// package.json, and in the shell scripts those run, with address. A port
// above 0 goes only to the dev script, the scripts it runs and their shell
// scripts: another script, such as next dev -p 3100, keeps its own. dir is
// from root, with slashes. A file it can't read has no fixes.
func fixesIn(root, dir, address string, port int) []Fix {
	full := filepath.Join(root, filepath.FromSlash(dir))
	pkg, text, ok := readPackage(full)
	if !ok {
		return nil
	}
	values := slices.Collect(maps.Values(pkg.Scripts))
	dev := devScripts(pkg)
	var fixes []Fix
	if script := scriptLineRE(values); script != nil {
		devLine := scriptLineRE(dev)
		file := path.Join(dir, "package.json")
		for i, line := range strings.Split(text, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if !script.MatchString(line) {
				continue // not "vite": "^6.0.0" among the dependencies
			}
			p := 0
			if devLine != nil && devLine.MatchString(line) {
				p = port
			}
			if fixed := fixLine(line, address, p, false); fixed != line {
				fixes = append(fixes, Fix{File: file, Line: i + 1, Old: line, New: fixed})
			}
		}
	}
	devShell := shellScripts(dev...)
	for _, rel := range shellScripts(values...) {
		data, err := fsutil.ReadRegular(filepath.Join(full, rel), fsutil.MaxProjectFile)
		if err != nil {
			continue
		}
		p := 0
		if slices.Contains(devShell, rel) {
			p = port
		}
		fixes = append(fixes, fixLines(path.Join(dir, filepath.ToSlash(rel)), string(data), address, p)...)
	}
	return fixes
}

// scriptLineRE matches a package.json line that holds one of the script
// values, as a value: after a colon, in quotes. It is nil without values.
func scriptLineRE(values []string) *regexp.Regexp {
	var quoted []string
	for _, v := range values {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(v) == nil {
			quoted = append(quoted, regexp.QuoteMeta(strings.TrimSpace(b.String())))
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	return regexp.MustCompile(`:\s*(?:` + strings.Join(quoted, "|") + `)`)
}

// fixLines returns the fixes in a shell script's content. A comment line
// stays.
func fixLines(file, content, address string, port int) []Fix {
	var fixes []Fix
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if fixed := fixLine(line, address, port, true); fixed != line {
			fixes = append(fixes, Fix{File: file, Line: i + 1, Old: line, New: fixed})
		}
	}
	return fixes
}
