package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/dev"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/tui"
)

// dev starts the Node apps of the project dir is in on the project's
// addresses, once ready.
func (a app) dev(ctx context.Context, dir string, o options) int {
	domains, ok := a.ready(ctx, "lodo dev starts")
	if !ok {
		return 1
	}
	if err := a.startDev(ctx, dir, domains, o); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// startDev opens the setup form when no listed name has the project's folder
// as its Root, or with o.edit, then replaces lodo with the project's `<pm> run
// dev`, whose NODE_OPTIONS load the hook that binds each of the project's
// ports to its address, with the project's variables (projectEnv). It returns
// on an error, after a cancel, and in tests.
// With o.auto the form saves what the scan proposes with no key pressed: a
// new worktree of a listed project gets <main><n>.test on 127.0.1.<n>.
func (a app) startDev(ctx context.Context, dir string, domains []store.Domain, o options) error {
	backend := tui.NewBackend(a.paths, a.runner)
	_, proposal, ok := backend.Scan(ctx, dir, "", domains)
	if !ok {
		return scan.ErrNoProject
	}
	listed := slices.ContainsFunc(domains, func(d store.Domain) bool { return scan.SameFolder(d.Root, proposal.Root) })
	if o.edit || !listed {
		saved, err := a.form(ctx, domains, dir, o.auto)
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
	fmt.Fprint(a.stdout, formatVars(proposal))
	fmt.Fprintf(a.stdout, "starting: %s run dev\n", pm)
	return a.runner.Replace(proposal.Root, pmPath, []string{"run", "dev"}, projectEnv(env, proposal))
}

// exec runs args in dir with the variables lodo dev sets for the project dir
// is in, once ready (startExec).
func (a app) exec(ctx context.Context, dir string, args []string) int {
	domains, ok := a.ready(ctx, "lodo exec runs")
	if !ok {
		return 1
	}
	if err := a.startExec(ctx, dir, domains, args); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// startExec replaces lodo with args, run in dir with the variables lodo dev
// sets for the project dir is in (projectEnv): a migration, a compose command
// or a test run then reaches the project's containers outside lodo dev. The
// project needs its names, from a first lodo dev. It returns on an error, and
// in tests.
func (a app) startExec(ctx context.Context, dir string, domains []store.Domain, args []string) error {
	_, proposal, ok := tui.NewBackend(a.paths, a.runner).Scan(ctx, dir, "", domains)
	switch {
	case !ok:
		return scan.ErrNoProject
	case !proposal.Listed:
		return fmt.Errorf("%s has no names yet: run lodo dev there first", proposal.Root)
	}
	path, err := a.lookPath(args[0])
	if err != nil {
		return fmt.Errorf("find %s: %w", args[0], err)
	}
	fmt.Fprint(a.stderr, formatVars(proposal)) // stdout stays the command's
	return a.runner.Replace(dir, path, args[1:], projectEnv(os.Environ(), proposal))
}

// projectEnv returns env with the project's variables in place of any it
// held: DOCKER_HOST_IP set to the project's address, for its compose file's
// ports; in a git worktree, COMPOSE_PROJECT_NAME set to the project's label,
// which beats the compose file's name:, so each worktree gets its own
// containers and volumes; and the .env values the scan points at the
// project's names (scan.Var). An exported variable beats a .env file, so the files stay as
// they are, and a .env copied from another checkout works as it is.
func projectEnv(env []string, proposal scan.Proposal) []string {
	vars := composeVars(proposal)
	for _, v := range proposal.Env {
		vars = append(vars, v.Env())
	}
	keys := map[string]bool{}
	for _, kv := range vars {
		key, _, _ := strings.Cut(kv, "=")
		keys[key] = true
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return keys[key]
	})
	return append(env, vars...)
}

// composeVars returns the variables projectEnv sets for compose, as
// KEY=value.
func composeVars(proposal scan.Proposal) []string {
	var vars []string
	if proposal.Address != "" {
		vars = append(vars, compose.EnvVar+"="+proposal.Address)
	}
	if proposal.Worktree && proposal.Name != "" {
		vars = append(vars, compose.ProjectVar+"="+strings.TrimSuffix(proposal.Name, store.Suffix))
	}
	return vars
}

// formatVars renders a line per variable projectEnv sets, user info masked,
// and the scan's notes, which say what it leaves.
func formatVars(proposal scan.Proposal) string {
	var b strings.Builder
	for _, kv := range composeVars(proposal) {
		b.WriteString("env   " + kv + "\n")
	}
	for _, v := range proposal.Env {
		fmt.Fprintf(&b, "env   %s=%s\n", v.Key, v.Value)
	}
	for _, n := range proposal.Notes {
		b.WriteString("note  " + n + "\n")
	}
	return b.String()
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
func (a app) setupForm(ctx context.Context, domains []store.Domain, dir string, auto bool) (bool, error) {
	s := tui.NewSetup(ctx, tui.NewBackend(a.paths, a.runner), domains, tui.Start{Dir: dir, Home: a.paths.Home, Auto: auto})
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
