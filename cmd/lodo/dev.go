package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/dev"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/tui"
)

// dev starts the Node apps of the project dir is in on the project's
// addresses, once ready.
func (a app) dev(ctx context.Context, dir string, edit bool) int {
	domains, ok := a.ready(ctx, "lodo dev starts")
	if !ok {
		return 1
	}
	if err := a.startDev(ctx, dir, domains, edit); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// startDev opens the setup form when no listed name has the project's folder
// as its Root, or with edit, then replaces lodo with the project's `<pm> run
// dev`, whose NODE_OPTIONS load the hook that binds each of the project's
// ports to its address. It returns on an error, after a cancel, and in tests.
func (a app) startDev(ctx context.Context, dir string, domains []store.Domain, edit bool) error {
	backend := tui.NewBackend(a.paths, a.runner)
	_, proposal, ok := backend.Scan(ctx, dir, "", domains)
	if !ok {
		return scan.ErrNoProject
	}
	listed := slices.ContainsFunc(domains, func(d store.Domain) bool { return scan.SameFolder(d.Root, proposal.Root) })
	if edit || !listed {
		saved, err := a.form(ctx, domains, dir)
		if !saved {
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "nothing saved")
			return nil
		}
		if err != nil {
			a.fail(err) // the names are saved; only linking or a .env line failed
		}
		if domains, err = store.Load(a.paths.DomainsJSON); err != nil {
			return err
		}
		_, proposal, _ = backend.Scan(ctx, dir, "", domains)
	}
	for _, d := range proposal.Add {
		if app := proposal.Dirs[d.Name]; app != "" {
			fmt.Fprintf(a.stderr, "warn  %s has no name: lodo dev --edit adds %s\n", app, d.Name)
		}
	}

	ports, err := dev.Ports(domains, proposal.Name)
	if err != nil {
		return err
	}
	pm, err := dev.PackageManager(proposal.Root)
	if err != nil {
		return err
	}
	pmPath, err := a.lookPath(pm)
	if err != nil {
		return fmt.Errorf("find %s: %w", pm, err)
	}
	hook, err := dev.WriteHooks(a.paths, proposal.Name, ports)
	if err != nil {
		return err
	}
	env, err := a.devEnv(ctx, pm, pmPath, proposal.Root, hook)
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, formatSites(domains, proposal.Name))
	fmt.Fprintf(a.stdout, "starting: %s run dev\n", pm)
	return a.runner.Replace(proposal.Root, pmPath, []string{"run", "dev"}, env)
}

// nodeConfig is, for npm and pnpm, the flag that points their config at a
// project and the variable that beats their node-options setting. They set a
// script's NODE_OPTIONS to that setting when it has one, which drops the hook.
var nodeConfig = map[string]struct{ dirFlag, envVar string }{
	"npm":  {"--prefix", "npm_config_node_options"},
	"pnpm": {"--dir", "pnpm_config_node_options"},
}

// devEnv returns lodo's environment with NODE_OPTIONS requiring hook. For npm
// and pnpm, the project's node-options setting goes first, and the same
// options go in the variable that beats the setting. Nested runs inherit it,
// except under turbo's strict env mode, which passes only NODE_OPTIONS on.
func (a app) devEnv(ctx context.Context, pm, pmPath, root, hook string) ([]string, error) {
	options := os.Getenv("NODE_OPTIONS")
	conf, ok := nodeConfig[pm]
	if ok {
		out, err := a.runner.Run(ctx, pmPath, conf.dirFlag, root, "config", "get", "node-options")
		if err != nil {
			return nil, fmt.Errorf("read %s's node-options: %w", pm, err)
		}
		if v := strings.TrimSpace(out); v != "null" && v != "undefined" { // unset, for npm and pnpm
			options = strings.TrimSpace(v + " " + options)
		}
	}
	options = dev.NodeOptions(options, hook)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "NODE_OPTIONS=") || ok && strings.HasPrefix(kv, conf.envVar+"=")
	})
	env = append(env, "NODE_OPTIONS="+options)
	if ok {
		env = append(env, conf.envVar+"="+options)
	}
	return env, nil
}

// setupForm is app.form: it runs the setup form inline on the terminal.
func (a app) setupForm(ctx context.Context, domains []store.Domain, dir string) (bool, error) {
	s := tui.NewSetup(ctx, tui.NewBackend(a.paths, a.runner), domains, tui.Start{Dir: dir, Home: a.paths.Home})
	final, err := tea.NewProgram(s, tea.WithContext(ctx)).Run()
	if err != nil {
		return false, fmt.Errorf("run the setup form: %w", err)
	}
	setup, ok := final.(tui.Setup)
	if !ok {
		return false, errors.New("run the setup form: it returned another model")
	}
	return setup.Saved(), setup.Err()
}

// formatSites renders a line per name of project that Caddy serves: its URL,
// and the address and port lodo dev binds for it.
func formatSites(domains []store.Domain, project string) string {
	var sites []store.Domain
	nameWidth, urlWidth := 0, 0
	for _, d := range domains {
		if store.Project(d.Name) == project && d.HasSite() {
			sites = append(sites, d)
			nameWidth, urlWidth = max(nameWidth, len(d.Name)), max(urlWidth, len(siteURL(d)))
		}
	}
	var b strings.Builder
	for _, d := range sites {
		fmt.Fprintf(&b, "%-*s  %-*s  -> %s:%d\n", nameWidth, d.Name, urlWidth, siteURL(d), d.Address, d.Port)
	}
	return b.String()
}

// siteURL is the address a browser opens d at: https when Caddy serves it.
func siteURL(d store.Domain) string {
	if d.HTTPS {
		return "https://" + d.Name
	}
	return "http://" + d.Name
}
