package scan

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/store"
)

// envFiles are the files an app's URLs come from. .env.example is left out:
// git tracks it.
var envFiles = []string{".env"}

// EnvLine is a line of an app's .env that mentions this Mac's own host.
type EnvLine struct {
	File string // from the project's root, with slashes: apps/web/.env
	Line int    // 1-based
	Key  string
	Text string // without its line end
}

// keyRE matches the start of a KEY=value line. The group is the key.
var keyRE = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_.]*)[ \t]*=`)

// findEnv returns the lines of each app's envFiles that set a key to
// something on localhost or 127.0.0.1. The root's .env is read only when the
// root is an app: in a monorepo it feeds compose. A file tracked says git
// tracks goes into shared instead, without its lines, since this Mac's names
// don't belong in a shared file; tracked is asked only about a file with such
// a line, and nil means nothing is tracked.
func findEnv(root string, apps []App, tracked func(string) bool) (lines []EnvLine, shared []string) {
	for _, a := range apps {
		for _, name := range envFiles {
			file := path.Join(a.Dir, name)
			full := filepath.Join(root, filepath.FromSlash(file))
			data, err := fsutil.ReadRegular(full, fsutil.MaxProjectFile)
			if err != nil {
				continue
			}
			var found []EnvLine
			for i, text := range strings.Split(string(data), "\n") {
				text = strings.TrimSuffix(text, "\r")
				m := keyRE.FindStringSubmatch(text)
				low := strings.ToLower(text)
				if m != nil && (strings.Contains(low, "localhost") || strings.Contains(low, "127.0.0.1")) {
					found = append(found, EnvLine{File: file, Line: i + 1, Key: m[1], Text: text})
				}
			}
			if len(found) == 0 {
				continue
			}
			if real, err := filepath.EvalSymlinks(full); err == nil {
				full = real // git knows the file a symlink points at
			}
			if tracked != nil && tracked(full) {
				shared = append(shared, file)
				continue
			}
			lines = append(lines, found...)
		}
	}
	return lines, shared
}

// envFixes returns the .env lines of p pointed at the project, and notes for
// what lodo leaves. In each URL on this Mac's own host, first match wins: a
// port a name serves gets that name (Values.Point); a port the compose file
// binds to DOCKER_HOST_IP, in bound, gets the project's name and keeps its
// port, whatever the scheme; another http or ws port gets a note, without the
// value, which may hold a password. Anything else stays, such as Homebrew's
// postgres on 5432: localhost still reaches it.
func envFixes(p Project, v compose.Values, bound map[int]bool) ([]Fix, []string) {
	var fixes []Fix
	var notes []string
	for _, l := range p.Env {
		out, changed := compose.MapURLs(l.Text, func(u compose.LocalURL) (string, bool) {
			url, named := v.Point(u)
			port, err := strconv.Atoi(u.Port)
			switch {
			case named || (err == nil && bound[port]):
				return url, true
			case u.Web() && err == nil && v.Names[port] == "": // https on a named port stays: Caddy serves the name on 443
				notes = append(notes, fmt.Sprintf("%s:%d %s: no name of %s has port %d", l.File, l.Line, l.Key, v.Domain, port))
			}
			return "", false
		})
		if changed {
			fixes = append(fixes, Fix{File: l.File, Line: l.Line, Old: mask(l.Text), New: mask(out), before: l.Text, after: out})
		}
	}
	for _, file := range p.Shared {
		notes = append(notes, file+": git tracks it, so lodo leaves its localhost URLs")
	}
	return fixes, notes
}

// ValuesFor returns what owner's project points localhost at: owner's name,
// and the enabled names with a port on owner's address in owner's project, by
// port, with their HTTPS.
func ValuesFor(domains []store.Domain, owner store.Domain) compose.Values {
	names, secure := map[int]string{}, map[int]bool{}
	for _, d := range domains {
		if d.Enabled && d.Port > 0 && d.Address == owner.Address && store.Project(d.Name) == store.Project(owner.Name) {
			names[d.Port] = d.Name
			secure[d.Port] = d.HTTPS
		}
	}
	return compose.Values{Domain: owner.Name, Names: names, Secure: secure}
}

// userInfoRE matches a URL's scheme and its user info. The group is the
// scheme with its ://. Unlike compose's URL matcher it lets the user info hold
// ? # and \, so a raw password still masks; a raw / ends it, as in every URL
// parser.
var userInfoRE = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/]*@`)

// mask returns s with each URL's user info shown as ***@, so a password in a
// .env never reaches the screen.
func mask(s string) string {
	return userInfoRE.ReplaceAllString(s, "${1}***@")
}

// ApplyEnv returns content with each fix's line replaced, keeping its line
// end. A line that already holds the new text counts as done. A line that
// changed since the scan, a line past the end, or a Fix that isn't a .env
// line is an error, and nothing changes.
func ApplyEnv(content []byte, fixes []Fix) ([]byte, error) {
	lines := strings.SplitAfter(string(content), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // after the last line end
	}
	for _, f := range fixes {
		switch {
		case f.after == "":
			return nil, fmt.Errorf("%s:%d is no .env line lodo sets", f.File, f.Line)
		case f.Line < 1 || f.Line > len(lines):
			return nil, fmt.Errorf("%s:%d is past the file's end: i scans again", f.File, f.Line)
		}
		line := lines[f.Line-1]
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch text {
		case f.after:
		case f.before:
			lines[f.Line-1] = f.after + line[len(text):]
		default:
			return nil, fmt.Errorf("%s:%d changed since the scan: i scans again", f.File, f.Line)
		}
	}
	return []byte(strings.Join(lines, "")), nil
}
