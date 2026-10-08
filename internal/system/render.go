package system

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/sangdth/oo/internal/dnsmasq"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/store"
)

// Marker is the first line of every file and block oo writes outside its own
// directory. The resolver script only deletes /etc/resolver files that start
// with it.
const Marker = "# oo"

// LoopbackLabel is the launchd label of the job that adds the own block to lo0.
const LoopbackLabel = "io.oo.loopback"

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.New("").
	Funcs(template.FuncMap{"sh": shellQuote}).
	ParseFS(templateFS, "templates/*.tmpl"))

// userRE matches the macOS short names oo writes into the sudoers rule.
var userRE = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]*$`)

// ResolverLines are the lines of /etc/resolver/<name> after the marker.
func ResolverLines() []string {
	return []string{
		"nameserver " + dnsmasq.ListenAddress,
		fmt.Sprintf("port %d", dnsmasq.Port),
	}
}

// ResolverFile is the content of every /etc/resolver file oo writes.
func ResolverFile() string {
	return Marker + "\n" + strings.Join(ResolverLines(), "\n") + "\n"
}

// Script returns the root resolver script for p.
func Script(p paths.Paths) string {
	return mustRender("apply-resolvers.sh.tmpl", struct {
		List, ResolverDir, Pattern, Marker string
		MaxLen                             int
		Lines                              []string
	}{
		List:        p.Resolvers,
		ResolverDir: p.ResolverDir,
		Pattern:     store.NamePattern,
		Marker:      Marker,
		MaxLen:      store.MaxNameLen,
		Lines:       ResolverLines(),
	})
}

// LoopbackPlist returns the launchd job that adds the own block to lo0 at boot.
func LoopbackPlist() string {
	return mustRender("io.oo.loopback.plist.tmpl", struct {
		Label, Prefix string
		First, Last   int
	}{LoopbackLabel, store.OwnPrefix, store.OwnFirst, store.OwnLast})
}

// Sudoers returns the rule that lets p.User run the resolver script as root
// without a password, and nothing else.
func Sudoers(p paths.Paths) (string, error) {
	if !userRE.MatchString(p.User) {
		return "", fmt.Errorf("user name %q has characters oo won't write into a sudoers rule", p.User)
	}
	return mustRender("sudoers.tmpl", struct{ User, Script string }{p.User, sudoersEscape(p.Script)}), nil
}

// ConfBlock is oo's block in Homebrew's dnsmasq.conf.
func ConfBlock(p paths.Paths) string {
	return fmt.Sprintf("%s\nconf-file=%s\nlisten-address=%s\nport=%d\nbind-interfaces\n",
		Marker, p.DnsmasqConf, dnsmasq.ListenAddress, dnsmasq.Port)
}

// CaddyBlock is oo's block in Homebrew's Caddyfile.
func CaddyBlock(p paths.Paths) string {
	return Marker + "\nimport " + p.Caddyfile + "\n"
}

// mustRender executes a template. The templates are fixed and every one runs
// in the tests, so a failure here is a bug in oo, not a runtime condition.
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
