// Package system installs, updates and removes the parts of lodo that live
// outside ~/.config/lodo: the root resolver script and its sudoers rule, the
// loopback job, and lodo's blocks in Homebrew's dnsmasq and Caddy configs.
package system

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sangdth/lodo/internal/caddy"
	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/store"
)

// confKeys are the dnsmasq options lodo owns in Homebrew's dnsmasq.conf. Setup
// replaces every line that sets one of them; every other line stays.
// bind-dynamic is listed because it conflicts with lodo's bind-interfaces.
var confKeys = []string{"conf-file", "listen-address", "port", "bind-interfaces", "bind-dynamic"}

// RewriteSystemConf returns Homebrew's dnsmasq.conf with every line setting
// one of lodo's options removed and lodo's block appended. It is idempotent.
func RewriteSystemConf(old string, p paths.Paths) string {
	return appendBlock(StripSystemConf(old), ConfBlock(p))
}

// StripSystemConf returns Homebrew's dnsmasq.conf without lodo's marker and
// without any line setting one of lodo's options.
func StripSystemConf(old string) string {
	return strip(old, func(line string) bool {
		key, _, _ := strings.Cut(line, "=")
		return slices.Contains(confKeys, strings.TrimSpace(key))
	})
}

// RewriteSystemCaddyfile returns Homebrew's Caddyfile with lodo's global block
// at the top and lodo's import line moved to a block at the end. Caddy allows
// one global block, and only first, so a file with its own keeps it and gets
// just the import. It is idempotent.
func RewriteSystemCaddyfile(old string, p paths.Paths) string {
	rest := StripSystemCaddyfile(old, p)
	if hasGlobalBlock(rest) {
		return appendBlock(rest, CaddyBlock(p))
	}
	return CaddyGlobalBlock + "\n" + appendBlock(rest, CaddyBlock(p))
}

// StripSystemCaddyfile returns Homebrew's Caddyfile without lodo's marker,
// global block and import line.
func StripSystemCaddyfile(old string, p paths.Paths) string {
	importLine := "import " + p.Caddyfile
	return strip(old, func(line string) bool { return line == importLine })
}

// hasGlobalBlock reports whether a Caddyfile starts with a global options
// block: its first line that is not blank or a comment opens with {.
func hasGlobalBlock(content string) bool {
	for line := range strings.Lines(content) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return strings.HasPrefix(trimmed, "{")
	}
	return false
}

// strip drops lodo's marker lines and the lines drop selects (drop sees each
// line trimmed of surrounding space), then blank lines at either end. A
// marker line right before a { line starts lodo's block, which strip drops
// through the next } line.
func strip(old string, drop func(trimmed string) bool) string {
	var lines []string
	for line := range strings.Lines(old) {
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
	var kept []string
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == Marker {
			if end := blockEnd(lines, i+1); end >= 0 {
				i = end
			}
			continue
		}
		if drop(trimmed) {
			continue
		}
		kept = append(kept, lines[i])
	}
	for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
		kept = kept[1:]
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// blockEnd returns the index of the } line that closes a block whose { line is
// lines[start], or -1 when lines[start] is not { or nothing closes it.
func blockEnd(lines []string, start int) int {
	if start >= len(lines) || strings.TrimSpace(lines[start]) != "{" {
		return -1
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "}" {
			return i
		}
	}
	return -1
}

func appendBlock(content, block string) string {
	if content == "" {
		return block
	}
	return content + "\n" + block
}

// Changes says which generated files WriteFiles changed.
type Changes struct {
	Dnsmasq   bool
	Resolvers bool
	Caddy     bool
}

// WriteFiles writes lodo's three generated files for domains and reports
// which ones changed.
func WriteFiles(p paths.Paths, domains []store.Domain) (Changes, error) {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: ~/.config/lodo holds no secrets
		return Changes{}, fmt.Errorf("create %s: %w", p.ConfigDir, err)
	}
	var c Changes
	var err error
	if c.Dnsmasq, err = writeConfig(p.DnsmasqConf, dnsmasq.Config(domains, p.Log)); err != nil {
		return c, err
	}
	if c.Resolvers, err = writeConfig(p.Resolvers, dnsmasq.ResolverList(domains)); err != nil {
		return c, err
	}
	if c.Caddy, err = writeConfig(p.Caddyfile, caddy.Config(domains)); err != nil {
		return c, err
	}
	return c, nil
}

func writeConfig(path, content string) (bool, error) {
	return fsutil.WriteFile(path, []byte(content), 0o644)
}
