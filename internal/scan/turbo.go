package scan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
)

// isTurbo reports whether the project at root runs its apps through turbo: it
// has a turbo.json. Its dev scripts then read their host from DOCKER_HOST_IP
// (compose.HostIP), which the root's turbo line sets (turboFix): turbo starts
// them without the root's .env.
func isTurbo(root string) bool {
	info, err := os.Stat(filepath.Join(root, "turbo.json"))
	return err == nil && info.Mode().IsRegular()
}

// hostIPFromEnv sets DOCKER_HOST_IP for the command after it to the value of
// the first DOCKER_HOST_IP line of the .env in the folder the command runs
// in, with or without export and quotes: the line hostIPRE reads, one whose
// value starts with a digit. A value outside 127.0.0.0/8 counts as none, so a
// LAN address or 0.0.0.0 never reaches -H. Without one it is empty, and
// compose.HostIP falls back to 127.0.0.1.
//
// shortcut: turbo 2.x passes DOCKER_* to its tasks by default (DOCKER_* is in
// turbo 2.11.6's binary, not in its docs), so turbo.json gets no fix; add
// DOCKER_HOST_IP to its globalPassThroughEnv if a turbo version drops it.
//
// shortcut: it reads .env from the folder the line runs in, the root for a
// package.json script; a shell script that cds away from the root before turbo
// reads none, and gets 127.0.0.1. Read it from the script's path if one does.
const hostIPFromEnv = compose.EnvVar + `="$(sed -E -n '/^[[:space:]]*(export[[:space:]]+)?` + compose.EnvVar +
	`[[:space:]]*=[[:space:]]*[^0-9]?[0-9]/{s/^[^=]*=[[:space:]]*[^0-9]?(127\.[0-9.]*).*/\1/p;q;}' .env 2>/dev/null)"`

// turboRE matches a turbo command that runs the dev task: turbo dev, turbo run
// dev --ui=stream, turbo run lint dev. Group 1 is turbo.
var turboRE = regexp.MustCompile(`(?:^|[^\w.@-])(turbo)(?:[ \t]+[^\s&;|"']+)*?[ \t]+dev\b`)

// leadRE matches what may come before a command's first word on its line:
// blanks, and the shell words that open a block or a condition.
var leadRE = regexp.MustCompile(`^[ \t]*(?:(?:then|do|else|elif|if|while|until|\{|!)[ \t]+)*`)

// runnerRE matches the words that may sit between a command's start and
// turbo, so turbo is the command and not an argument, as in echo turbo dev.
var runnerRE = regexp.MustCompile(`^(?:(?:exec|npx|pnpm|yarn|bunx)[ \t]+)*$`)

// jsonText escapes text for a package.json string.
var jsonText = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// turboFix returns the fix that sets DOCKER_HOST_IP from the root's .env
// (hostIPFromEnv) on the first line that runs turbo's dev task: in the root's
// dev script, the scripts it runs, or their shell scripts. A line lodo leaves
// (turboLine) is passed over. ok is false when there is none, or a line that
// mentions turbo's dev task names DOCKER_HOST_IP already.
func turboFix(root string) (Fix, bool) {
	pkg, text, ok := readPackage(root)
	dev := devScripts(pkg)
	if !ok || len(dev) == 0 {
		return Fix{}, false
	}
	devLine := scriptLineRE(dev)
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if devLine != nil && devLine.MatchString(line) && turboRE.MatchString(line) {
			if f, ok := turboLine("package.json", i+1, line, false); ok || strings.Contains(line, compose.EnvVar) {
				return f, ok
			}
		}
	}
	for _, rel := range shellScripts(dev...) {
		data, err := fsutil.ReadRegular(filepath.Join(root, rel), fsutil.MaxProjectFile)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && turboRE.MatchString(line) {
				if f, ok := turboLine(filepath.ToSlash(rel), i+1, line, true); ok || strings.Contains(line, compose.EnvVar) {
					return f, ok
				}
			}
		}
	}
	return Fix{}, false
}

// turboLine returns the fix for line n of file, which runs turbo: hostIPFromEnv
// at the start of its command, before exec, npx or pnpm, escaped for JSON
// unless shell. ok is false when the line names DOCKER_HOST_IP already, turbo
// is an argument of another command, or the command sits in quotes, as in
// sh -c 'cd apps && turbo dev'.
func turboLine(file string, n int, line string, shell bool) (Fix, bool) {
	m := turboRE.FindStringSubmatchIndex(line)
	if m == nil || strings.Contains(line, compose.EnvVar) {
		return Fix{}, false
	}
	at := strings.LastIndexAny(line[:m[2]], `&;|("'`) + 1 // a " also opens a package.json script
	dquote := `\"`                                        // a shell's double quote in a package.json string
	if shell {
		dquote = `"`
	}
	// shortcut: an odd count of quotes before turbo means it sits in quotes; a
	// quote escaped inside quotes miscounts. Walk the quotes if a script has one.
	if strings.Count(line[:at], "'")%2 == 1 || strings.Count(line[:at], dquote)%2 == 1 {
		return Fix{}, false
	}
	at += len(leadRE.FindString(line[at:m[2]]))
	if !runnerRE.MatchString(line[at:m[2]]) {
		return Fix{}, false
	}
	text := hostIPFromEnv + " "
	if !shell {
		text = jsonText.Replace(text)
	}
	return Fix{File: file, Line: n, Old: line, New: line[:at] + text + line[at:]}, true
}
