// Package check runs oo doctor's checks and probes each enabled domain.
package check

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sangdth/oo/internal/brew"
	"github.com/sangdth/oo/internal/caddy"
	"github.com/sangdth/oo/internal/dnsmasq"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
)

// Env holds what the checks and probes need: oo's paths, the runner for
// commands, and where dnsmasq and Caddy listen. NewEnv returns the values for
// this Mac; tests point DNS and HTTPPort at local servers and RootUID at their
// own uid.
type Env struct {
	Paths    paths.Paths
	Runner   run.Runner
	DNS      string // dnsmasq's address, host:port
	HTTPPort int    // the port Caddy listens on
	RootUID  uint32 // the uid that must own the resolver script: 0, or the test user's uid in tests
}

// NewEnv returns the Env for this Mac: dnsmasq on 127.0.0.1:53535, Caddy on
// port 80, and a resolver script that root owns.
func NewEnv(p paths.Paths, r run.Runner) Env {
	return Env{
		Paths:    p,
		Runner:   r,
		DNS:      net.JoinHostPort(dnsmasq.ListenAddress, strconv.Itoa(dnsmasq.Port)),
		HTTPPort: store.ProxyPort,
		RootUID:  0,
	}
}

// Check is the outcome of one doctor check.
type Check struct {
	ID      int
	Name    string
	OK      bool
	Skipped bool   // the check does not apply; nothing to fix
	Detail  string // one line: what was found
	Fix     string // one line: what to run; empty when OK or Skipped
}

// The fixes most checks print.
const (
	fixSetup = "oo setup"
	fixApply = "oo apply"
)

// doctorChecks are the eight checks, in ID order. Each gets the whole domain
// list and the probe results, which only "names resolve" reads.
var doctorChecks = []struct {
	name string
	run  func(Env, context.Context, []store.Domain, []Result) Check
}{
	{"dnsmasq", Env.checkDnsmasq},
	{"dnsmasq config", Env.checkConfig},
	{"loopback", Env.checkLoopback},
	{"resolvers", Env.checkResolvers},
	{"resolver for .local", Env.checkResolverLocal},
	{"generated files", Env.checkGenerated},
	{"names resolve", Env.checkNames},
	{"caddy", Env.checkCaddy},
}

// prerequisites is how many checks, from the first, need oo setup or a
// manual fix rather than oo apply.
const prerequisites = 5

// Run runs the eight checks one after another and returns them in ID order.
// domains is the whole list from domains.json, enabled or not.
func (e Env) Run(ctx context.Context, domains []store.Domain) []Check {
	checks, _ := e.Report(ctx, domains)
	return checks
}

// Report runs the eight checks like Run and also returns the probe results
// that "names resolve" is built from, so a caller showing both probes once.
func (e Env) Report(ctx context.Context, domains []store.Domain) ([]Check, []Result) {
	results := e.Probe(ctx, domains)
	return e.run(ctx, domains, results, len(doctorChecks)), results
}

// Prerequisites runs checks 1 to 5, which need oo setup or a manual fix.
// It probes nothing, so it is quick enough to run before the TUI starts.
func (e Env) Prerequisites(ctx context.Context, domains []store.Domain) []Check {
	return e.run(ctx, domains, nil, prerequisites)
}

func (e Env) run(ctx context.Context, domains []store.Domain, results []Result, n int) []Check {
	checks := make([]Check, n)
	for i, c := range doctorChecks[:n] {
		checks[i] = c.run(e, ctx, domains, results)
		checks[i].ID, checks[i].Name = i+1, c.name
	}
	return checks
}

// Failed returns the checks that are neither OK nor Skipped, in order.
func Failed(checks []Check) []Check {
	var failed []Check
	for _, c := range checks {
		if !c.OK && !c.Skipped {
			failed = append(failed, c)
		}
	}
	return failed
}

// checkDnsmasq: dnsmasq is installed, no root job shadows oo's, and
// Homebrew's job runs it as the user.
func (e Env) checkDnsmasq(ctx context.Context, _ []store.Domain, _ []Result) Check {
	p := e.Paths
	if !installed(p.Dnsmasq) {
		return fail("not installed", "brew install dnsmasq")
	}
	for _, label := range system.DnsmasqSystemLabels {
		if _, err := e.Runner.Run(ctx, p.Launchctl, "print", "system/"+label); err == nil {
			return fail("a root job, system/"+label+", runs dnsmasq and shadows oo's", fixSetup)
		}
	}
	st, err := brew.Info(ctx, e.Runner, p.Brew, "dnsmasq")
	switch {
	case err != nil:
		return fail(oneLine(err.Error()), fixSetup)
	case !st.Running:
		return fail("not running", fixSetup)
	case st.User != "" && st.User != p.User:
		return fail("runs as "+st.User, fixSetup)
	}
	return pass(fmt.Sprintf("running as %s, pid %d", p.User, st.PID))
}

// checkConfig: Homebrew's dnsmasq.conf includes oo's file and makes dnsmasq
// listen where the resolver files point.
func (e Env) checkConfig(_ context.Context, _ []store.Domain, _ []Result) Check {
	p := e.Paths
	content, err := readFile(p.SystemConf)
	if errors.Is(err, fs.ErrNotExist) {
		return fail("missing", fixSetup)
	}
	if err != nil {
		return fail(oneLine(err.Error()), fixSetup)
	}
	if problem := confProblem(confValues(content), p.DnsmasqConf); problem != "" {
		return fail(problem, fixSetup)
	}
	return pass("includes " + p.DnsmasqConf)
}

// checkLoopback: the loopback job is loaded and every own address is on lo0.
func (e Env) checkLoopback(ctx context.Context, _ []store.Domain, _ []Result) Check {
	p := e.Paths
	_, err := e.Runner.Run(ctx, p.Launchctl, "print", "system/"+system.LoopbackLabel)
	loaded := err == nil
	out, _ := e.Runner.Run(ctx, p.Ifconfig, "lo0") // a failed ifconfig shows no addresses
	found, total := ownAliases(out), store.OwnLast-store.OwnFirst+1
	detail := fmt.Sprintf("%d of %d addresses on lo0", found, total)
	switch {
	case !loaded:
		return fail("job not loaded; "+detail, fixSetup)
	case found < total:
		return fail(detail, fixSetup)
	}
	return pass(detail)
}

// checkResolvers: the root script is the one setup installs, sudo runs it
// without a password, and every enabled domain has its resolver file.
func (e Env) checkResolvers(ctx context.Context, domains []store.Domain, _ []Result) Check {
	p := e.Paths
	if problem := e.scriptProblem(); problem != "" {
		return fail(problem, fixSetup)
	}
	if _, err := e.Runner.Run(ctx, p.Sudo, "-n", "-k", "-l", p.Script); err != nil {
		return fail("sudo asks for a password: the sudoers rule is missing", fixSetup)
	}
	on := enabled(domains)
	var missing, foreign, foreignNames []string
	for _, d := range on {
		path := filepath.Join(p.ResolverDir, d.Name)
		got, err := readFile(path)
		switch {
		case err == nil && got == system.ResolverFile():
		case errors.Is(err, fs.ErrNotExist), err == nil && strings.HasPrefix(got, system.Marker+"\n"):
			missing = append(missing, d.Name)
		default:
			// The script leaves files without oo's marker alone, so apply
			// can never fix this one.
			foreign = append(foreign, path)
			foreignNames = append(foreignNames, d.Name)
		}
	}
	if len(foreign) > 0 {
		return fail("not written by oo, so oo apply won't replace: "+firstFew(foreignNames, ", "),
			"sudo rm "+strings.Join(foreign, " ")+", then oo apply")
	}
	if len(missing) > 0 {
		return fail("missing for "+firstFew(missing, ", "), fixApply)
	}
	return pass(plural(len(on), "file"))
}

// scriptProblem says what is wrong with the installed resolver script, or
// returns "" when it is what setup installs.
func (e Env) scriptProblem() string {
	p := e.Paths
	fi, err := os.Lstat(p.Script)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "script missing"
	case err != nil:
		return oneLine(err.Error())
	case fi.Mode()&fs.ModeSymlink != 0:
		return "script is a symlink"
	case !fi.Mode().IsRegular():
		return "script is not a regular file"
	case !ownedBy(fi, e.RootUID):
		return "script not owned by root"
	case fi.Mode().Perm() != 0o755:
		return fmt.Sprintf("script mode %04o, want 0755", fi.Mode().Perm())
	}
	content, err := readFile(p.Script)
	switch {
	case err != nil:
		return oneLine(err.Error())
	case content != system.Script(p):
		return "script out of date"
	}
	return ""
}

// checkResolverLocal: no /etc/resolver/local takes every .local name away
// from Bonjour. Like setup, it counts the file as present only when lstat
// finds it.
func (e Env) checkResolverLocal(_ context.Context, _ []store.Domain, _ []Result) Check {
	local := filepath.Join(e.Paths.ResolverDir, "local")
	if _, err := os.Lstat(local); err != nil {
		return pass("absent")
	}
	return fail(local+" sends every .local name to one server, away from Bonjour", "sudo rm "+local)
}

// checkGenerated: oo's three generated files hold what domains generates.
func (e Env) checkGenerated(_ context.Context, domains []store.Domain, _ []Result) Check {
	p := e.Paths
	var stale []string
	for _, f := range []struct{ path, want string }{
		{p.DnsmasqConf, dnsmasq.Config(domains, p.Log)},
		{p.Resolvers, dnsmasq.ResolverList(domains)},
		{p.Caddyfile, caddy.Config(domains)},
	} {
		if got, err := readFile(f.path); err != nil || got != f.want {
			stale = append(stale, filepath.Base(f.path))
		}
	}
	if len(stale) > 0 {
		return fail("out of date: "+strings.Join(stale, ", "), fixApply)
	}
	return pass("match domains.json")
}

// checkNames: every enabled domain passed its probes.
func (e Env) checkNames(_ context.Context, _ []store.Domain, results []Result) Check {
	if len(results) == 0 {
		return pass("no enabled domains")
	}
	var failed []string
	for _, r := range results {
		if !r.OK() {
			failed = append(failed, r.Name+": "+r.Detail)
		}
	}
	if len(failed) > 0 {
		return fail(firstFew(failed, "; "), fixApply)
	}
	return pass(fmt.Sprintf("%d of %d resolve", len(results), len(results)))
}

// checkCaddy: when an enabled domain has a port, Caddy is installed, set up,
// running and valid, and it owns the HTTP port.
func (e Env) checkCaddy(ctx context.Context, domains []store.Domain, _ []Result) Check {
	sites := 0
	for _, d := range enabled(domains) {
		if d.Port > 0 {
			sites++
		}
	}
	if sites == 0 {
		return skip("no enabled domain has a port")
	}
	p := e.Paths
	if !installed(p.Caddy) {
		return fail("not installed", "brew install caddy, then oo setup")
	}
	if !system.CaddySetUp(p) {
		return fail("Homebrew's Caddyfile does not import oo's", fixSetup)
	}
	st, err := brew.Info(ctx, e.Runner, p.Brew, "caddy")
	switch {
	case err != nil:
		return fail(oneLine(err.Error()), fixSetup)
	case !st.Running:
		return fail("not running", fixSetup)
	}
	if err := caddy.Validate(ctx, e.Runner, p.Caddy, p.SystemCaddyfile); err != nil {
		return fail(oneLine(err.Error()), fixApply)
	}
	return e.portOwner(ctx, sites)
}

// portOwner checks that Caddy is what listens on the HTTP port.
func (e Env) portOwner(ctx context.Context, sites int) Check {
	// lsof exits 1 with no output when nothing listens.
	out, _ := e.Runner.Run(ctx, e.Paths.Lsof, "-nP", fmt.Sprintf("-iTCP:%d", e.HTTPPort), "-sTCP:LISTEN")
	commands := listeners(out)
	switch {
	case slices.ContainsFunc(commands, func(c string) bool { return strings.HasPrefix(c, "caddy") }):
		return pass("running, serves " + plural(sites, "site"))
	case len(commands) > 0:
		return fail(fmt.Sprintf("port %d is taken by %s", e.HTTPPort, commands[0]), "stop "+commands[0]+", then oo apply")
	}
	return fail(fmt.Sprintf("nothing listens on port %d", e.HTTPPort), fixSetup)
}

func pass(detail string) Check      { return Check{OK: true, Detail: detail} }
func fail(detail, fix string) Check { return Check{Detail: detail, Fix: fix} }
func skip(detail string) Check      { return Check{Skipped: true, Detail: detail} }

// enabled returns the enabled domains in store.Sort order.
func enabled(domains []store.Domain) []store.Domain {
	var on []store.Domain
	for _, d := range store.Sort(domains) {
		if d.Enabled {
			on = append(on, d)
		}
	}
	return on
}

// installed reports whether a tool is at path. It follows symlinks, such as
// Homebrew's links into its Cellar, so a dangling link is not installed.
func installed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readFile returns the content of a file at a path from paths.Paths.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: every path comes from paths.Paths
	return string(data), err
}

// ownedBy reports whether uid owns the file fi describes.
func ownedBy(fi fs.FileInfo, uid uint32) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

// confValues returns the values a dnsmasq config sets, by option. An active
// line is non-empty once trimmed and does not start with '#'; the option is
// the text before '=' and the value the text after it, both trimmed.
func confValues(content string) map[string][]string {
	values := map[string][]string{}
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		values[key] = append(values[key], strings.TrimSpace(value))
	}
	return values
}

// confProblem returns the first way values differ from oo's block in
// Homebrew's dnsmasq.conf, or "" when they match it.
func confProblem(values map[string][]string, ooConf string) string {
	if problem := lineCount(values, "conf-file"); problem != "" {
		return problem
	}
	if got := values["conf-file"][0]; got != ooConf {
		return "conf-file points at " + got
	}
	for _, want := range []struct{ key, value string }{
		{"port", strconv.Itoa(dnsmasq.Port)},
		{"listen-address", dnsmasq.ListenAddress},
	} {
		if problem := lineCount(values, want.key); problem != "" {
			return problem
		}
		if got := values[want.key][0]; got != want.value {
			return fmt.Sprintf("%s is %s, want %s", want.key, got, want.value)
		}
	}
	if _, ok := values["bind-interfaces"]; !ok {
		return "no bind-interfaces line"
	}
	return ""
}

// lineCount returns what is wrong unless exactly one active line sets key.
func lineCount(values map[string][]string, key string) string {
	switch n := len(values[key]); n {
	case 0:
		return "no " + key + " line"
	case 1:
		return ""
	default:
		return fmt.Sprintf("%d %s lines", n, key)
	}
}

// ownAliases counts the own-block addresses in the output of ifconfig lo0.
func ownAliases(out string) int {
	on := map[string]bool{}
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "inet" {
			on[f[1]] = true
		}
	}
	n := 0
	for i := store.OwnFirst; i <= store.OwnLast; i++ {
		if on[store.OwnAddress(i)] {
			n++
		}
	}
	return n
}

// listeners returns the COMMAND column of lsof output, below its header.
func listeners(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var commands []string
	for _, line := range lines[1:] {
		if f := strings.Fields(line); len(f) > 0 {
			commands = append(commands, f[0])
		}
	}
	return commands
}

// firstFew joins the first three items with sep and counts the rest, as in
// "a, b, c, and 2 more".
func firstFew(items []string, sep string) string {
	const shown = 3
	if len(items) <= shown {
		return strings.Join(items, sep)
	}
	return strings.Join(items[:shown], sep) + sep + fmt.Sprintf("and %d more", len(items)-shown)
}

// plural returns n and noun, adding an s unless n is 1: "1 file", "2 files".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// oneLine joins the non-empty lines of s with "; ", so a multi-line command
// error fits in a Detail.
func oneLine(s string) string {
	var lines []string
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "; ")
}
