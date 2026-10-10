package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
)

// scan prints what the project dir is in lacks from the list, under the name
// sf gives when it gives one: as text, or as JSON for scripts and agents. It
// changes nothing; the TUI applies it.
func (a app) scan(dir string, sf scanFlags) int {
	name := ""
	if sf.name != "" {
		name = strings.TrimSuffix(sf.name, "."+store.TLD) + "." + store.TLD
		if err := scan.ValidName(name); err != nil {
			a.fail(fmt.Errorf("--name %s: %w", sf.name, err))
			return 2
		}
	}
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		return 1
	}
	p, ok := scan.Find(dir)
	if !ok {
		fmt.Fprintln(a.stderr, "✗ not in a project: lodo scans the git root, or a folder with a lock file, it starts in")
		return 1
	}
	proposal := scan.Propose(p, domains, name)
	if sf.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(proposal); err != nil {
			a.fail(fmt.Errorf("write the scan: %w", err))
			return 1
		}
		return 0
	}
	fmt.Fprint(a.stdout, formatProposal(proposal, a.paths.Home))
	return 0
}

// formatProposal renders a scan: a line per name to add, the compose file to
// link, each line to change by hand with its old and new text, and the notes.
func formatProposal(p scan.Proposal, home string) string {
	root := p.Root
	if home != "" && strings.HasPrefix(root, home+string(filepath.Separator)) {
		root = "~" + root[len(home):]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "scan of %s\n", root)
	switch {
	case p.Name == "":
		b.WriteString("its folder's name makes no ." + store.TLD + " name\n")
		return b.String()
	case p.Empty() && len(p.Notes) == 0 && !p.Listed:
		b.WriteString("found no dev server or compose file\n")
		return b.String()
	case p.Empty() && len(p.Notes) == 0:
		b.WriteString(p.Name + " is set up: nothing to add, link or change\n")
		return b.String()
	}
	width := 0
	for _, d := range p.Add {
		width = max(width, len(d.Name))
	}
	for _, d := range p.Add {
		line := fmt.Sprintf("add   %-*s  %s", width, d.Name, d.Address)
		if d.Port > 0 {
			line += "  port " + strconv.Itoa(d.Port)
		}
		b.WriteString(line + "\n")
	}
	if p.Claim {
		fmt.Fprintf(&b, "save  this folder as %s's project\n", p.Name)
	}
	if p.Compose != "" {
		rel, err := filepath.Rel(p.Root, p.Compose)
		if err != nil {
			rel = p.Compose
		}
		fmt.Fprintf(&b, "link  ./%s · %s=%s in its .env\n", filepath.ToSlash(rel), compose.EnvVar, p.Address)
	}
	for _, f := range p.Fixes {
		fmt.Fprintf(&b, "fix   %s:%d\n      - %s\n      + %s\n", f.File, f.Line, strings.TrimSpace(f.Old), strings.TrimSpace(f.New))
	}
	for _, n := range p.Notes {
		b.WriteString("note  " + n + "\n")
	}
	if len(p.Add) > 0 || p.Compose != "" || p.Claim {
		b.WriteString("\nrun lodo here and press y to add and link these; e names the project\n")
	}
	return b.String()
}
