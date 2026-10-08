package compose

import (
	"cmp"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// EnvFile returns the .env file the compose file at path is run with: the one
// a package.json script passes through --env-file to a command that names
// path, or else the .env at the project's root. The root is the git root
// above path, or path's folder outside a repository.
func EnvFile(path string) string {
	root := projectDir(path)
	if env, ok := scriptEnvFile(root, path); ok {
		return env
	}
	return filepath.Join(root, ".env")
}

// envFileRE matches a --env-file option; the value is the first group that
// is set: double quoted, single quoted, or bare.
var envFileRE = regexp.MustCompile(`--env-file(?:=|\s+)(?:"([^"]+)"|'([^']+)'|(\S+))`)

// scriptEnvFile returns the env file a script in root's package.json passes to
// a command that names path. npm runs scripts at root, so a relative env file
// starts there.
func scriptEnvFile(root, path string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json")) //nolint:gosec // G304: the project's own package.json
	if err != nil {
		return "", false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	// The name stands alone, so compose.yml doesn't match docker-compose.yml.
	names := regexp.MustCompile(`(?:^|[\s='"/])` + regexp.QuoteMeta(filepath.ToSlash(rel)) + `(?:$|[\s'"])`)
	for _, name := range slices.Sorted(maps.Keys(pkg.Scripts)) {
		script := pkg.Scripts[name]
		m := envFileRE.FindStringSubmatch(script)
		if m == nil || !names.MatchString(script) {
			continue
		}
		env := cmp.Or(m[1], m[2], m[3])
		if !filepath.IsAbs(env) {
			env = filepath.Join(root, env)
		}
		return env, true
	}
	return "", false
}

// SetEnv returns a .env file's content with key set to value: each line that
// sets key gets value, and content without one gets a line at its end. Other
// lines, an export before the key, a comment after the value and line ends
// stay.
func SetEnv(content []byte, key, value string) []byte {
	assign := regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*`)
	lines := strings.SplitAfter(string(content), "\n")
	found := false
	for i, line := range lines {
		m := assign.FindStringIndex(line)
		if m == nil {
			continue
		}
		found = true
		body := strings.TrimRight(line[m[1]:], "\r\n")
		lines[i] = line[:m[1]] + value + afterValue(body) + line[m[1]+len(body):]
	}
	out := strings.Join(lines, "")
	if found {
		return []byte(out)
	}
	eol := "\n"
	if strings.Contains(out, "\r\n") {
		eol = "\r\n"
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += eol
	}
	return []byte(out + key + "=" + value + eol)
}

// afterValue returns what follows a .env value on its line: what comes after
// a quoted value's closing quote, or a comment after an unquoted one.
func afterValue(body string) string {
	if body != "" && (body[0] == '"' || body[0] == '\'') {
		if end := strings.IndexByte(body[1:], body[0]); end >= 0 {
			return body[end+2:]
		}
		return ""
	}
	if loc := commentRE.FindStringIndex(body); loc != nil {
		return body[loc[0]:]
	}
	return ""
}

// commentRE matches the start of a comment after an unquoted .env value.
var commentRE = regexp.MustCompile(`[ \t]+#`)
