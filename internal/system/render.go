package system

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/store"
)

// Marker is the first line of every file and block lodo writes outside its own
// directory. The resolver script only deletes /etc/resolver files that start
// with it.
const Marker = "# lodo"

// LoopbackLabel is the launchd label of the job that adds the own block to lo0.
const LoopbackLabel = "io.lodo.loopback"

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.New("").
	Funcs(template.FuncMap{"sh": shellQuote}).
	ParseFS(templateFS, "templates/*.tmpl"))

// userRE matches the macOS short names lodo writes into the sudoers rule.
var userRE = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]*$`)

// ResolverLines are the lines of /etc/resolver/<name> after the marker.
func ResolverLines() []string {
	return []string{
		"nameserver " + dnsmasq.ListenAddress,
		fmt.Sprintf("port %d", dnsmasq.Port),
	}
}

// ResolverFile is the content of every /etc/resolver file lodo writes.
func ResolverFile() string {
	return Marker + "\n" + strings.Join(ResolverLines(), "\n") + "\n"
}

// MaxScriptInput is the most the resolver script reads on standard input: room
// for over 4,000 names of store.MaxNameLen characters. It refuses a longer list.
const MaxScriptInput = 1 << 20

// Script returns the root resolver script for p. It reads the names on
// standard input, as dnsmasq.ResolverList writes them.
func Script(p paths.Paths) string {
	return mustRender("apply-resolvers.sh.tmpl", struct {
		ResolverDir, Pattern, Marker string
		MaxLen, MaxInput, ReadInput  int
		Lines                        []string
	}{
		ResolverDir: p.ResolverDir,
		Pattern:     store.NamePattern,
		Marker:      Marker,
		MaxLen:      store.MaxNameLen,
		MaxInput:    MaxScriptInput,
		ReadInput:   MaxScriptInput + 1,
		Lines:       ResolverLines(),
	})
}

// LoopbackPlist returns the launchd job that adds the own block to lo0 at boot.
func LoopbackPlist() string {
	return mustRender("io.lodo.loopback.plist.tmpl", struct {
		Label, Prefix string
		First, Last   int
	}{LoopbackLabel, store.OwnPrefix, store.OwnFirst, store.OwnLast})
}

// Sudoers returns the rule that lets p.User run the resolver script as root
// without a password, and nothing else.
func Sudoers(p paths.Paths) (string, error) {
	if !userRE.MatchString(p.User) {
		return "", fmt.Errorf("user name %q has characters lodo won't write into a sudoers rule", p.User)
	}
	return mustRender("sudoers.tmpl", struct{ User, Script string }{p.User, sudoersEscape(p.Script)}), nil
}

// ConfBlock is lodo's block in Homebrew's dnsmasq.conf.
func ConfBlock(p paths.Paths) string {
	return fmt.Sprintf("%s\nconf-file=%s\nlisten-address=%s\nport=%d\nbind-interfaces\n",
		Marker, p.DnsmasqConf, dnsmasq.ListenAddress, dnsmasq.Port)
}

// CaddyBlock is lodo's import block at the end of Homebrew's Caddyfile.
func CaddyBlock(p paths.Paths) string {
	return Marker + "\nimport " + p.Caddyfile + "\n"
}

// CaddyGlobalBlock is lodo's global options block at the top of Homebrew's
// Caddyfile. It turns Caddy's admin API off: lodo never uses it, and on
// localhost:2019 any local user or web page could change Caddy's config.
const CaddyGlobalBlock = Marker + "\n{\n\tadmin off\n}\n"

// mustRender executes a template. The templates are fixed and every one runs
// in the tests, so a failure here is a bug in lodo, not a runtime condition.
func mustRender(name string, data any) string {
	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		panic(fmt.Sprintf("render %s: %v", name, err))
	}
	return b.String()
}

// shellQuote quotes s for /bin/sh: single quotes, with each ' written as '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sudoersEscape backslash-escapes the characters sudoers treats as syntax
// inside a command path.
func sudoersEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\ ,:=()!"#`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
