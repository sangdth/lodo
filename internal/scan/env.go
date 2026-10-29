package scan

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/store"
)

// EnvLine is a KEY=value line of an app's .env.
type EnvLine struct {
	File string // from the project's root, with slashes: apps/web/.env
	Line int    // 1-based
	Key  string
	Text string // without its line end
}

// Var is a variable lodo dev and lodo exec set for a project, in place of the
// value its .env lines give the key. An exported variable beats a .env file in
// dotenv, Next, Node's --env-file and docker compose, so the file never
// changes. Value holds it with user info masked, value the real text.
type Var struct {
	File  string `json:"file"` // the first .env line it replaces, from the project's root
	Line  int    `json:"line"` // 1-based
	Key   string `json:"key"`
	Value string `json:"value"`

	value string
}

// Env returns v as KEY=value, with the real value, for a process's
// environment.
func (v Var) Env() string {
	return v.Key + "=" + v.value
}

// keyRE matches the start of a KEY=value line. The group is the key.
var keyRE = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_.]*)[ \t]*=`)

// findEnv returns the KEY=value lines of each app's .env; .env.example is left
// out, since it holds no real values. The root's .env is read only when the
// root is an app: in a monorepo it feeds compose.
func findEnv(root string, apps []App) []EnvLine {
	var lines []EnvLine
	for _, a := range apps {
		file := path.Join(a.Dir, ".env")
		data, err := fsutil.ReadRegular(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			continue
		}
		for i, text := range strings.Split(string(data), "\n") {
			text = strings.TrimSuffix(text, "\r")
			if m := keyRE.FindStringSubmatch(text); m != nil {
				lines = append(lines, EnvLine{File: file, Line: i + 1, Key: m[1], Text: text})
			}
		}
	}
	return lines
}

// envValue returns the value a .env line sets, as dotenv and Node read it:
// quotes come off, and an unquoted value ends at a #. ok is false for a value
// that reads otherwise in one of them or that Next expands: one over more
// lines, or one with a backslash or a $.
func envValue(text string) (value string, ok bool) {
	m := keyRE.FindStringIndex(text)
	if m == nil {
		return "", false
	}
	v := strings.TrimLeft(text[m[1]:], " \t")
	if v != "" && strings.IndexByte("\"'`", v[0]) >= 0 {
		end := strings.IndexByte(v[1:], v[0])
		if end < 0 {
			return "", false // the value goes on over more lines
		}
		v = v[1 : end+1]
	} else {
		v, _, _ = strings.Cut(v, "#")
		v = strings.TrimSpace(v)
	}
	if strings.ContainsAny(v, `\$`) {
		return "", false
	}
	return v, true
}

// envVars returns the variables lodo dev sets for p, from its apps' .env
// lines, and notes for what it leaves. In a value, each name of another
// checkout of the project's repository becomes the project's (siblings); then
// in each URL on this Mac's own host, first match wins: a port a name serves
// gets that name (Values.Point); a port the compose file binds to
// DOCKER_HOST_IP, in bound, gets the project's name and keeps its port,
// whatever the scheme; another http or ws port gets a note, without the value,
// which may hold a password. Anything else stays, such as Homebrew's postgres
// on 5432: localhost still reaches it. The apps share one environment, so a
// key two .env files set to values that come out different gets a note and no
// variable.
func envVars(p Project, v compose.Values, bound map[int]bool, siblings map[string]string) ([]Var, []string) {
	var notes []string
	type value struct {
		line    EnvLine
		out     string
		known   bool // lodo could read the value
		changed bool
	}
	var keys []string
	byKey := map[string][]value{}
	for _, l := range p.Env {
		val, ok := envValue(l.Text)
		got := value{line: l, known: ok}
		if ok {
			var missing []string
			got.out, missing = renameHosts(val, siblings)
			for _, name := range missing {
				notes = append(notes, fmt.Sprintf("%s:%d %s: %s has no name for %s, so lodo dev leaves it", l.File, l.Line, l.Key, v.Domain, name))
			}
			got.out, _ = compose.MapURLs(got.out, func(u compose.LocalURL) (string, bool) {
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
			got.changed = got.out != val
		} else if local(l.Text, siblings) {
			notes = append(notes, fmt.Sprintf("%s:%d %s: lodo can't read a value over more lines or with a \\ or a $, so lodo dev leaves it", l.File, l.Line, l.Key))
		}
		if _, seen := byKey[l.Key]; !seen {
			keys = append(keys, l.Key)
		}
		byKey[l.Key] = append(byKey[l.Key], got)
	}

	var vars []Var
	for _, key := range keys {
		values := byKey[key]
		i := slices.IndexFunc(values, func(x value) bool { return x.changed })
		if i < 0 {
			continue
		}
		if slices.ContainsFunc(values, func(x value) bool { return !x.known || x.out != values[i].out }) {
			files := make([]string, 0, len(values))
			for _, x := range values {
				files = append(files, x.line.File)
			}
			notes = append(notes, fmt.Sprintf("%s: %s set it to different values, and lodo dev gives every app one environment, so it leaves %s as each file has it",
				key, strings.Join(slices.Compact(files), " and "), key))
			continue
		}
		l, out := values[i].line, values[i].out
		vars = append(vars, Var{File: l.File, Line: l.Line, Key: key, Value: mask(out), value: out})
	}
	return vars, notes
}

// local reports whether a .env line names this Mac's own host or a sibling
// checkout's name, so a value lodo can't read there earns a note.
func local(text string, siblings map[string]string) bool {
	low := strings.ToLower(text)
	if strings.Contains(low, "localhost") || strings.Contains(low, "127.0.0.1") {
		return true
	}
	for name := range siblings {
		if strings.Contains(low, name) {
			return true
		}
	}
	return false
}

// renameHosts returns s with each whole host that is a key of names, in any
// case, replaced by its value: mail.hugger.test in http://mail.hugger.test,
// but not inside x-hugger.test or hugger.test.example.com. A host whose value
// is empty stays, and comes back in missing.
func renameHosts(s string, names map[string]string) (out string, missing []string) {
	if len(names) == 0 {
		return s, nil
	}
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, regexp.QuoteMeta(name))
	}
	// Longest first: the regexp takes the first alternative that matches.
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	re := regexp.MustCompile(`(?i)` + strings.Join(keys, "|"))
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		if (m[0] > 0 && isHostByte(s[m[0]-1])) || (m[1] < len(s) && isHostByte(s[m[1]])) {
			continue
		}
		host := strings.ToLower(s[m[0]:m[1]])
		if names[host] == "" {
			missing = append(missing, host)
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(names[host])
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), missing
}

// isHostByte reports whether c can be part of a host name.
func isHostByte(c byte) bool {
	return c == '.' || c == '-' || c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// siblingNames maps each name of another checkout of the project's git
// repository to the same name under project: hugger.test to hugger9.test and
// mail.hugger.test to mail.hugger9.test, for the worktree named hugger9.test
// whose repository's main checkout is hugger.test's Root. A .env copied from
// one checkout then works in every other. A name with no match in list maps
// to "", which renameHosts leaves.
func siblingNames(p Project, list []store.Domain, project string) map[string]string {
	if p.Repo == "" {
		return nil
	}
	names := map[string]string{}
	for _, owner := range list {
		sibling := owner.Name
		if owner.Root == "" || store.Project(sibling) != sibling || sibling == project || SameFolder(owner.Root, p.Root) ||
			!SameFolder(repoRoot(owner.Root), p.Repo) {
			continue
		}
		for _, d := range list {
			if store.Project(d.Name) != sibling {
				continue
			}
			target := strings.TrimSuffix(d.Name, sibling) + project
			if _, ok := store.Find(list, target); !ok {
				target = ""
			}
			names[strings.ToLower(d.Name)] = target
		}
	}
	return names
}

// repoRoot returns the main checkout of the git repository whose root is dir:
// dir itself, or for a worktree the folder its .git file points into,
// <main>/.git/worktrees/<name>. It is empty when dir holds no .git, or one
// lodo can't read, such as a submodule's.
func repoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	switch {
	case err != nil:
		return ""
	case info.IsDir():
		return dir
	}
	data, err := fsutil.ReadRegular(gitPath)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	main, _, ok := strings.Cut(filepath.ToSlash(gitdir), "/.git/worktrees/")
	if !ok {
		return ""
	}
	return filepath.FromSlash(main)
}

// ValuesFor returns what owner's project points localhost at: owner's name,
// and the enabled names with a port on owner's address in owner's project, by
// port, with their HTTPS.
func ValuesFor(domains []store.Domain, owner store.Domain) compose.Values {
	names, secure := map[int]string{}, map[int]bool{}
	for _, d := range domains {
		if d.HasSite() && d.Address == owner.Address && store.Project(d.Name) == store.Project(owner.Name) {
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
