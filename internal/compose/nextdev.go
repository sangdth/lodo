package compose

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Fix is a line that starts next dev on every address, with -H added so it
// listens on the project's address only.
type Fix struct {
	File     string // from the project's root, as package.json or scripts/dev.sh
	Line     int    // 1-based
	Old, New string // the line before and after, without its line end
}

// nextDevRE matches the command that starts Next's dev server.
var nextDevRE = regexp.MustCompile(`\bnext[ \t]+dev\b`)

// hostRE matches a host option after next dev. The group is its value.
var hostRE = regexp.MustCompile(`(?:^|[ \t])(?:-H|--hostname)(?:=|[ \t]+)([^ \t]*)`)

// shellScriptRE matches a shell script a package.json script runs, such as
// ./scripts/dev.sh or sh scripts/dev.sh. The group is its path.
var shellScriptRE = regexp.MustCompile(`(?:^|[\s"'=])(?:\./)?((?:[\w.-]+/)*[\w.-]+\.sh)\b`)

// NextDev returns the lines that run next dev without -H address, in the
// package.json at the root of the project the compose file at path belongs
// to and in the shell scripts its scripts run. Without a host, next dev
// listens on every address, so two projects can't share a port. A host that
// is a variable stays: the project sets it. NextDev only reads; a file it
// can't read has no fixes.
func NextDev(path, address string) []Fix {
	root := projectDir(path)
	data, err := os.ReadFile(filepath.Join(root, "package.json")) //nolint:gosec // G304: the project's own package.json
	if err != nil {
		return nil
	}
	fixes := fixLines("package.json", string(data), address, false)
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return fixes
	}
	scripts := map[string]bool{}
	for _, script := range pkg.Scripts {
		for _, m := range shellScriptRE.FindAllStringSubmatch(script, -1) {
			if filepath.IsLocal(m[1]) {
				scripts[filepath.Clean(m[1])] = true
			}
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(scripts)) {
		data, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // G304: a script the project's package.json runs
		if err == nil {
			fixes = append(fixes, fixLines(filepath.ToSlash(rel), string(data), address, true)...)
		}
	}
	return fixes
}

// fixLines returns the fixes in file's content. In a shell script a comment
// line stays.
func fixLines(file, content, address string, shell bool) []Fix {
	var fixes []Fix
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if shell && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if fixed := fixLine(line, address); fixed != line {
			fixes = append(fixes, Fix{File: file, Line: i + 1, Old: line, New: fixed})
		}
	}
	return fixes
}

// fixLine gives each next dev in line the host address: -H address after a
// next dev without a host, or address in place of another fixed host. The
// command ends at &, ;, | or a double quote, which ends a package.json
// script.
func fixLine(line, address string) string {
	var b strings.Builder
	last := 0
	for _, m := range nextDevRE.FindAllStringIndex(line, -1) {
		end := len(line)
		if i := strings.IndexAny(line[m[1]:], `&;|"`); i >= 0 {
			end = m[1] + i
		}
		host := hostRE.FindStringSubmatchIndex(line[m[1]:end])
		switch {
		case host == nil:
			b.WriteString(line[last:m[1]] + " -H " + address)
			last = m[1]
		case isFixedHost(line[m[1]+host[2] : m[1]+host[3]]):
			b.WriteString(line[last:m[1]+host[2]] + address)
			last = m[1] + host[3]
		}
	}
	if last == 0 {
		return line
	}
	return b.String() + line[last:]
}

// isFixedHost reports whether a host option's value is a plain address that
// oo can replace: not empty, quoted or a variable.
func isFixedHost(value string) bool {
	return value != "" && !strings.ContainsAny(value, `$'"`)
}

// projectDir returns the git root above the file at path, or its folder
// outside a repository.
func projectDir(path string) string {
	if root, ok := gitRoot(filepath.Dir(path)); ok {
		return root
	}
	return filepath.Dir(path)
}
