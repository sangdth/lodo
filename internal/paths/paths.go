// Package paths holds every file, directory and tool path lodo touches, so tests
// can move all of them under one temporary directory.
package paths

import (
	"fmt"
	"os/user"
	"path/filepath"
)

// Paths lists every file, directory and tool lodo reads, writes or runs.
// Default returns the real locations; ForTest moves all of them under one
// directory.
type Paths struct {
	// User is the login name the sudoers rule names.
	User string
	// Home is the user's home directory.
	Home string

	// lodo's own files, owned by the user.
	ConfigDir   string
	DomainsJSON string // the domain list, lodo's only source of truth
	DnsmasqConf string // generated: one address= line per enabled domain
	Resolvers   string // generated: one enabled name per line, read by the root script
	Caddyfile   string // generated: one site per enabled domain with a port
	Log         string // dnsmasq's query log
	Staging     string // setup renders root-owned files here before installing them
	TrustedCA   string // the copy of Caddy's root that setup trusted in the System keychain

	// Homebrew's files, owned by the user.
	SystemConf            string // the config Homebrew's dnsmasq service loads
	SystemConfBackup      string
	SystemCaddyfile       string // the config Homebrew's caddy service loads
	SystemCaddyfileBackup string
	CaddyRoot             string // the root certificate of the local CA Homebrew's caddy service makes

	// Root-owned files and directories.
	LaunchDaemons string // system launchd jobs
	LoopbackPlist string
	Script        string // the resolver script, run through sudo
	Sudoers       string
	ResolverDir   string

	// SystemKeychain holds the roots every user's browsers trust.
	SystemKeychain string

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
	Git         string
	Security    string

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
	config := filepath.Join(home, ".config", "lodo")
	brew := sys("/opt/homebrew")
	etc := filepath.Join(brew, "etc")
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
		TrustedCA:   filepath.Join(config, "caddy-root.crt"),

		SystemConf:            filepath.Join(etc, "dnsmasq.conf"),
		SystemConfBackup:      filepath.Join(etc, "dnsmasq.conf.before-lodo"),
		SystemCaddyfile:       filepath.Join(etc, "Caddyfile"),
		SystemCaddyfileBackup: filepath.Join(etc, "Caddyfile.before-lodo"),
		CaddyRoot:             filepath.Join(brew, "var/lib/caddy/pki/authorities/local/root.crt"),

		LaunchDaemons: daemons,
		LoopbackPlist: filepath.Join(daemons, "io.lodo.loopback.plist"),
		Script:        sys("/Library/Application Support/lodo/apply-resolvers.sh"),
		Sudoers:       sys("/etc/sudoers.d/lodo"),
		ResolverDir:   sys("/etc/resolver"),

		SystemKeychain: sys("/Library/Keychains/System.keychain"),

		Hosts: sys("/etc/hosts"),

		Brew:        filepath.Join(brew, "bin/brew"),
		Dnsmasq:     filepath.Join(brew, "opt/dnsmasq/sbin/dnsmasq"),
		Caddy:       filepath.Join(brew, "bin/caddy"),
		Sudo:        sys("/usr/bin/sudo"),
		Launchctl:   sys("/bin/launchctl"),
		Ifconfig:    sys("/sbin/ifconfig"),
		Install:     sys("/usr/bin/install"),
		Visudo:      sys("/usr/sbin/visudo"),
		Rm:          sys("/bin/rm"),
		Rmdir:       sys("/bin/rmdir"),
		Dscacheutil: sys("/usr/bin/dscacheutil"),
		Lsof:        sys("/usr/sbin/lsof"),
		Git:         sys("/usr/bin/git"),
		Security:    sys("/usr/bin/security"),

		Leftovers: []string{
			filepath.Join(home, ".config", "localdns"),
			filepath.Join(home, "Library", "LaunchAgents", "com.localdns.caddy.plist"),
			filepath.Join(home, "Library", "Application Support", "LocalDNS"),
		},
	}
}
