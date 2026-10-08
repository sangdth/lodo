package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/oo/internal/check"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
	"github.com/sangdth/oo/internal/tui"
)

// app runs the commands that read or change the system.
type app struct {
	paths  paths.Paths
	runner run.Runner
	env    check.Env
	stdout io.Writer
	stderr io.Writer
}

func newApp(p paths.Paths, r run.Runner, stdout, stderr io.Writer) app {
	return app{paths: p, runner: r, env: check.NewEnv(p, r), stdout: stdout, stderr: stderr}
}

// tui opens the terminal UI. It refuses while checks 1 to 5 fail: those need
// oo setup or a manual fix, which the TUI can't do.
func (a app) tui(ctx context.Context) int {
	domains, err := store.Load(a.paths.DomainsJSON)
	if err != nil {
		a.fail(err)
		return 1
	}
	if failed := check.Failed(a.env.Prerequisites(ctx, domains)); len(failed) > 0 {
		fmt.Fprint(a.stderr, formatChecks(failed))
		fmt.Fprintln(a.stderr, "oo opens once these pass; oo doctor shows every check.")
		return 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	program := tea.NewProgram(tui.New(ctx, tui.NewBackend(a.paths, a.runner), domains), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

// setup installs oo's system parts, then prints the doctor table.
func (a app) setup(ctx context.Context) int {
	if err := system.Setup(ctx, a.paths, a.runner, a.stdout); err != nil {
		a.fail(err)
		return 1
	}
	fmt.Fprintln(a.stdout)
	return a.doctor(ctx)
}

// apply makes the system match domains.json and probes every enabled name.
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
	for _, r := range results {
		if !r.OK() {
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

// formatResults renders one line per probed name.
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
		nameWidth = max(nameWidth, len(r.Name))
		targetWidth = max(targetWidth, len(targets[i]))
	}
	var b strings.Builder
	for i, r := range results {
		mark := "✓"
		if !r.OK() {
			mark = "✗"
		}
		line := fmt.Sprintf("%s %-*s  %-*s  %s", mark, nameWidth, r.Name, targetWidth, targets[i], r.Detail)
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}
