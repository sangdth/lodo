package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
	"github.com/sangdth/lodo/internal/tui"
)

// scan prints what the project dir is in lacks from the list, under the name
// o gives when it gives one: as text, or as JSON for scripts and agents. It
// changes nothing; lodo dev's setup form applies it. Only git runs, to tell
// which .env files it tracks.
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
	p, ok := scan.Find(dir, func(path string) bool { return system.Tracked(ctx, a.paths, a.runner, path) })
	if !ok {
		a.fail(scan.ErrNoProject)
		return 1
	}
	proposal := scan.Propose(p, domains, name, nil)
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
// link, each .env line lodo sets with its old and new text, and the notes. The
// .env lines come masked.
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
	case !p.CanAccept() && len(p.Notes) == 0:
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
	for _, f := range p.Env {
		fmt.Fprintf(&b, "set   %s:%d\n      - %s\n      + %s\n", f.File, f.Line, strings.TrimSpace(f.Old), strings.TrimSpace(f.New))
	}
	for _, n := range p.Notes {
		b.WriteString("note  " + n + "\n")
	}
	if p.CanAccept() {
		b.WriteString("\nrun lodo dev --edit here to add, link and set these\n")
	}
	return b.String()
}
