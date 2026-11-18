// Package system installs, updates and removes the parts of lodo that live
// outside ~/.config/lodo: the root resolver script and its sudoers rule, the
// loopback job, and lodo's blocks in Homebrew's dnsmasq and Caddy configs.
package system

import (
	"fmt"
	"os"
	"slices"
	"strconv"
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
// one global block, and only first, so a file with its own keeps it, gains
// `admin off` if missing or not off, and gets the import. It is idempotent.
func RewriteSystemCaddyfile(old string, p paths.Paths) string {
	rest := StripSystemCaddyfile(old, p)
	if hasGlobalBlock(rest) {
		return appendBlock(ensureAdminOff(rest), CaddyBlock(p))
	}
	return CaddyGlobalBlock + "\n" + appendBlock(rest, CaddyBlock(p))
}

// StripSystemCaddyfile returns Homebrew's Caddyfile without lodo's marker,
// global block, injected admin off line, and import line.
func StripSystemCaddyfile(old string, p paths.Paths) string {
	importPlain := "import " + p.Caddyfile
	importQuoted := "import " + strconv.Quote(p.Caddyfile)
	adminLine := strings.TrimSpace(adminOffMarker)
	return strip(old, func(line string) bool {
		return line == importPlain || line == importQuoted || line == adminLine
	})
}

const adminOffMarker = "\tadmin off " + Marker

func globalBlockBounds(content string) (lines []string, start, end int) {
	for line := range strings.Lines(content) {
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
	start = -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if t == "{" {
			start = i
		}
		break
	}
	if start < 0 {
		return lines, -1, -1
	}
	return lines, start, blockEnd(lines, start)
}

func isAdminOffLine(line string) bool {
	f := strings.Fields(line)
	return len(f) >= 2 && f[0] == "admin" && f[1] == "off" && (len(f) == 2 || strings.HasPrefix(f[2], "#"))
}

// ensureAdminOff inserts `admin off` inside an existing global options block
// unless it already disables the admin API, and replaces any other `admin`
// directive in that block so Caddy's unauthenticated control port stays off.
func ensureAdminOff(content string) string {
	lines, start, end := globalBlockBounds(content)
	if start < 0 || end < 0 {
		return content
	}
	for i := start + 1; i < end; i++ {
		f := strings.Fields(lines[i])
		if len(f) == 0 || f[0] != "admin" {
			continue
		}
		if isAdminOffLine(lines[i]) {
			return content
		}
		lines[i] = adminOffMarker
		return strings.Join(lines, "\n") + "\n"
	}
	lines = slices.Insert(lines, start+1, adminOffMarker)
	return strings.Join(lines, "\n") + "\n"
}

// CaddyAdminDisabled reports whether Homebrew's Caddyfile has a global block
// at the top that sets `admin off`.
func CaddyAdminDisabled(p paths.Paths) bool {
	data, err := os.ReadFile(p.SystemCaddyfile)
	if err != nil {
		return false
	}
	lines, start, end := globalBlockBounds(string(data))
	if start < 0 || end < 0 {
		return false
	}
	return slices.ContainsFunc(lines[start+1:end], isAdminOffLine)
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
	if start < 0 || start >= len(lines) || strings.TrimSpace(lines[start]) != "{" {
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

// GeneratedFile is a file lodo generates and what domains make of it.
type GeneratedFile struct {
	Path    string
	Content string
}

// Generated returns lodo's three generated files for domains: dnsmasq's
// config, the resolver list and the Caddyfile. WriteFiles writes them, and
// lodo doctor compares them.
func Generated(p paths.Paths, domains []store.Domain) []GeneratedFile {
	return []GeneratedFile{
		{p.DnsmasqConf, dnsmasq.Config(domains, p.Log)},
		{p.Resolvers, dnsmasq.ResolverList(domains)},
		{p.Caddyfile, caddy.Config(domains)},
	}
}

// WriteFiles writes lodo's generated files for domains and returns the paths
// of the ones it changed.
func WriteFiles(p paths.Paths, domains []store.Domain) ([]string, error) {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: ~/.config/lodo holds no secrets
		return nil, fmt.Errorf("create %s: %w", p.ConfigDir, err)
	}
	var changed []string
	for _, f := range Generated(p, domains) {
		ok, err := fsutil.WriteFile(f.Path, []byte(f.Content), 0o644)
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, f.Path)
		}
	}
	return changed, nil
}
