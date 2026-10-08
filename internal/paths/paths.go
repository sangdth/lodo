// Package paths holds every file, directory and tool path lcd touches, so tests
// can move all of them under one temporary directory.
package paths

import (
	"fmt"
	"os/user"
	"path/filepath"
)

// Paths lists every file, directory and tool lcd reads, writes or runs.
// Default returns the real locations; ForTest moves all of them under one
// directory.
type Paths struct {
	// User is the login name the sudoers rule names.
	User string
	// Home is the user's home directory.
	Home string

	// lcd's own files, owned by the user.
	ConfigDir   string
	DomainsJSON string // the domain list, lcd's only source of truth
	DnsmasqConf string // generated: one address= line per enabled domain
	Resolvers   string // generated: one enabled name per line, read by the root script
	Caddyfile   string // generated: one site per enabled domain with a port
	Log         string // dnsmasq's query log
	Staging     string // setup renders root-owned files here before installing them

	// Homebrew's files, owned by the user.
	SystemConf            string // the config Homebrew's dnsmasq service loads
	SystemConfBackup      string
	SystemCaddyfile       string // the config Homebrew's caddy service loads
	SystemCaddyfileBackup string

	// Root-owned files and directories.
	LaunchDaemons string // system launchd jobs
	LoopbackPlist string
	Script        string // the resolver script, run through sudo
	Sudoers       string
	ResolverDir   string

	// Hosts is the hosts file; macOS and dnsmasq answer from it before DNS.
	Hosts string

	// Tools, by absolute path so a changed PATH can't swap them.
	Brew        string
	Dnsmasq     string
	Caddy       string
	Sudo        string
	Launchctl   string
	Ifconfig    string
	Install     string
	Visudo      string
	Rm          string
	Rmdir       string
	Dscacheutil string
	Lsof        string
	Pbcopy      string

	// Leftovers are LocalDNS files that setup reports and leaves alone.
	Leftovers []string
}

// Default returns the real paths for the current user.
func Default() (Paths, error) {
	u, err := user.Current()
	if err != nil {
		return Paths{}, fmt.Errorf("look up current user: %w", err)
	}
	return build("/", u.HomeDir, u.Username), nil
}

// ForTest returns paths that all live under root, for user "tester".
func ForTest(root string) Paths {
	return build(root, filepath.Join(root, "Users", "tester"), "tester")
}

func build(root, home, user string) Paths {
	sys := func(p string) string { return filepath.Join(root, p) }
	config := filepath.Join(home, ".config", "lcd")
	etc := sys("/opt/homebrew/etc")
	daemons := sys("/Library/LaunchDaemons")
	return Paths{
		User: user,
		Home: home,

		ConfigDir:   config,
		DomainsJSON: filepath.Join(config, "domains.json"),
		DnsmasqConf: filepath.Join(config, "dnsmasq.conf"),
		Resolvers:   filepath.Join(config, "resolvers"),
		Caddyfile:   filepath.Join(config, "Caddyfile"),
		Log:         filepath.Join(config, "dnsmasq.log"),
		Staging:     filepath.Join(config, "staging"),

		SystemConf:            filepath.Join(etc, "dnsmasq.conf"),
		SystemConfBackup:      filepath.Join(etc, "dnsmasq.conf.before-lcd"),
		SystemCaddyfile:       filepath.Join(etc, "Caddyfile"),
		SystemCaddyfileBackup: filepath.Join(etc, "Caddyfile.before-lcd"),

		LaunchDaemons: daemons,
		LoopbackPlist: filepath.Join(daemons, "io.lcd.loopback.plist"),
		Script:        sys("/Library/Application Support/lcd/apply-resolvers.sh"),
		Sudoers:       sys("/etc/sudoers.d/lcd"),
		ResolverDir:   sys("/etc/resolver"),

		Hosts: sys("/etc/hosts"),

		Brew:        sys("/opt/homebrew/bin/brew"),
		Dnsmasq:     sys("/opt/homebrew/opt/dnsmasq/sbin/dnsmasq"),
		Caddy:       sys("/opt/homebrew/bin/caddy"),
		Sudo:        sys("/usr/bin/sudo"),
		Launchctl:   sys("/bin/launchctl"),
		Ifconfig:    sys("/sbin/ifconfig"),
		Install:     sys("/usr/bin/install"),
		Visudo:      sys("/usr/sbin/visudo"),
		Rm:          sys("/bin/rm"),
		Rmdir:       sys("/bin/rmdir"),
		Dscacheutil: sys("/usr/bin/dscacheutil"),
		Lsof:        sys("/usr/sbin/lsof"),
		Pbcopy:      sys("/usr/bin/pbcopy"),

		Leftovers: []string{
			filepath.Join(home, ".config", "localdns"),
			filepath.Join(home, "Library", "LaunchAgents", "com.localdns.caddy.plist"),
			filepath.Join(home, "Library", "Application Support", "LocalDNS"),
		},
	}
}
