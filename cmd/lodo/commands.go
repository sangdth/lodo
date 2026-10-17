package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
	"github.com/sangdth/lodo/internal/tui"
)

// app runs the commands that read or change the system.
type app struct {
	paths  paths.Paths
	runner run.Runner
	env    check.Env
	stdout io.Writer
	stderr io.Writer

	// form runs the setup form for the project dir is in on the terminal;
	// saved reports whether domains.json holds the project's names. Tests
	// replace it, and lookPath, which finds a command on PATH.
	form     func(ctx context.Context, domains []store.Domain, dir string) (saved bool, err error)
	lookPath func(file string) (string, error)
}

func newApp(p paths.Paths, r run.Runner, stdout, stderr io.Writer) app {
	a := app{paths: p, runner: r, env: check.NewEnv(p, r), stdout: stdout, stderr: stderr, lookPath: exec.LookPath}
	a.form = a.setupForm
	return a
}

// command runs one of dispatch's commands that read or change the system.
func (a app) command(ctx context.Context, command string, o options) int {
	switch command {
	case "ps":
		return a.tui(ctx)
	case "setup":
		return a.setup(ctx)
	case "apply":
		return a.apply(ctx)
	case "doctor":
		return a.doctor(ctx)
	case "uninstall":
		return a.uninstall(ctx)
	}
	dir, err := os.Getwd()
	if err != nil {
		a.fail(fmt.Errorf("find the current folder: %w", err))
		return 1
	}
	if command == "scan" {
		return a.scan(ctx, dir, o)
	}
	return a.dev(ctx, dir, o.edit)
}

// ready loads domains.json and refuses while checks 1 to 5 fail: those need
// lodo setup or a manual fix, which neither the dashboard nor lodo dev can do.
// what names the command that waits, such as "lodo ps opens"; ok is false once
// ready printed why.
func (a app) ready(ctx context.Context, what string) (domains []store.Domain, ok bool) {
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		return nil, false
	}
	if failed := check.Failed(a.env.Prerequisites(ctx, domains)); len(failed) > 0 {
		fmt.Fprint(a.stderr, formatChecks(failed))
		fmt.Fprintf(a.stderr, "%s once these pass; lodo doctor shows every check.\n", what)
		return nil, false
	}
	return domains, true
}

// tui opens the dashboard, once ready.
func (a app) tui(ctx context.Context) int {
	domains, ok := a.ready(ctx, "lodo ps opens")
	if !ok {
		return 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dir, _ := os.Getwd() // an unknown folder only skips the compose question
	start := tui.Start{Dir: dir, Home: a.paths.Home}
	program := tea.NewProgram(tui.New(ctx, tui.NewBackend(a.paths, a.runner), domains, start), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// setup installs lodo's system parts, then prints the doctor table.
func (a app) setup(ctx context.Context) int {
	if err := system.Setup(ctx, a.paths, a.runner, a.stdout); err != nil {
		a.fail(err)
		return 1
	}
	fmt.Fprintln(a.stdout)
	return a.doctor(ctx)
}

// apply makes the system match domains.json and probes every enabled name.
// A name whose app isn't running doesn't make it fail.
func (a app) apply(ctx context.Context) int {
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		return 1
	}
	applyErr := system.Apply(ctx, a.paths, a.runner, domains)
	if errors.Is(applyErr, system.ErrNotSetUp) {
		a.fail(applyErr)
		return 1
	}
	results := a.env.Probe(ctx, domains)
	fmt.Fprint(a.stdout, formatResults(results))
	if applyErr != nil {
		a.fail(applyErr)
		return 1
	}
	return probeCode(results)
}

// probeCode is apply's exit code after the probes: 1 when a name fails for
// more than its app not running, which apply can't fix.
func probeCode(results []check.Result) int {
	for _, r := range results {
		if !r.OK() && !r.Down {
			return 1
		}
	}
	return 0
}

// doctor prints every check and exits 1 when one failed.
func (a app) doctor(ctx context.Context) int {
	code := 0
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		code = 1
	}
	checks := a.env.Run(ctx, domains)
	fmt.Fprint(a.stdout, formatChecks(checks))
	if len(check.Failed(checks)) > 0 {
		code = 1
	}
	return code
}

func (a app) uninstall(ctx context.Context) int {
	if err := system.Uninstall(ctx, a.paths, a.runner, a.stdout); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// fail prints an error, with its fix when it carries one.
func (a app) fail(err error) {
	if se, ok := errors.AsType[*system.StepError](err); ok {
		fmt.Fprintf(a.stderr, "✗ %s\n  %v\n  fix: %s\n", se.Step, se.Err, se.Fix)
		return
	}
	fmt.Fprintf(a.stderr, "✗ %v\n", err)
}

// formatChecks renders the doctor table: one line per check, plus a fix line
// under each failure.
func formatChecks(checks []check.Check) string {
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	var b strings.Builder
	for _, c := range checks {
		mark := "✗"
		switch {
		case c.OK:
			mark = "✓"
		case c.Skipped:
			mark = "–"
		}
		fmt.Fprintf(&b, "%s %d %-*s  %s\n", mark, c.ID, width, c.Name, c.Detail)
		if !c.OK && !c.Skipped && c.Fix != "" {
			fmt.Fprintf(&b, "    %*s  fix: %s\n", width, "", c.Fix)
		}
	}
	return b.String()
}

// formatResults renders one line per probed name, marked – when only its app
// isn't running. A name with HTTPS on says so after its port, since its line
// covers the HTTPS probe too.
func formatResults(results []check.Result) string {
	if len(results) == 0 {
		return "no enabled domains\n"
	}
	nameWidth, targetWidth := 0, 0
	targets := make([]string, len(results))
	for i, r := range results {
		targets[i] = r.Address
		if r.Port > 0 {
			targets[i] += fmt.Sprintf(" port %d", r.Port)
		}
		if r.Secure {
			targets[i] += ", https"
		}
		nameWidth = max(nameWidth, len(r.Name))
		targetWidth = max(targetWidth, len(targets[i]))
	}
	var b strings.Builder
	for i, r := range results {
		mark := "✓"
		switch {
		case r.Down:
			mark = "–"
		case !r.OK():
			mark = "✗"
		}
		line := fmt.Sprintf("%s %-*s  %-*s  %s", mark, nameWidth, r.Name, targetWidth, targets[i], r.Detail)
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}
