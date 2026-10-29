package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/tui"
)

// scan prints what the project dir is in lacks from the list, under the name
// o gives when it gives one: as text, or as JSON for scripts and agents. It
// changes nothing; lodo dev's setup form applies it, and lodo dev and lodo
// exec set its variables.
func (a app) scan(ctx context.Context, dir string, o options) int {
	name := ""
	if o.name != "" {
		name = strings.TrimSuffix(o.name, store.Suffix) + store.Suffix
		if err := store.ValidateProject(name); err != nil {
			a.fail(fmt.Errorf("--name %s: %w", o.name, err))
			return 2
		}
	}
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		return 1
	}
	_, proposal, ok := tui.NewBackend(a.paths, a.runner).Scan(ctx, dir, name, domains)
	if !ok {
		a.fail(scan.ErrNoProject)
		return 1
	}
	if o.json {
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
// link, each variable lodo dev sets, masked, and the notes.
func formatProposal(p scan.Proposal, home string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "scan of %s\n", tui.ShortPath(p.Root, home))
	switch {
	case p.Name == "":
		b.WriteString("its folder's name makes no " + store.Suffix + " name\n")
		return b.String()
	case !p.CanAccept() && len(p.Notes) == 0 && !p.Listed:
		b.WriteString("found no dev server or compose file\n")
		return b.String()
	case !p.CanAccept():
		b.WriteString(p.Name + " is set up: nothing to add or link\n")
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
		fmt.Fprintf(&b, "link  ./%s\n", filepath.ToSlash(rel))
	}
	b.WriteString(formatVars(p)) // the notes come last
	if p.CanAccept() {
		b.WriteString("\nrun lodo dev --edit here to add and link these\n")
	}
	if p.Address != "" {
		b.WriteString("\nlodo dev and lodo exec set the env lines; the .env files stay as they are\n")
	}
	return b.String()
}
