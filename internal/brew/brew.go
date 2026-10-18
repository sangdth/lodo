// Package brew runs Homebrew's services for dnsmasq and caddy: it reads a
// service's state, restarts it and stops it, always as the current user. It
// reads state and restarts a loaded job through launchctl, which answers in
// milliseconds, where brew takes most of a second just to start.
package brew

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// Status is one service as brew services info reports it.
type Status struct {
	Running    bool   // the job has a process
	User       string // who the job runs as; empty when no plist registers it
	PID        int    // 0 when not running
	Registered bool   // a plist starts the job at login; Stop removes it, a crash does not
}

// Off reports whether the service was turned off: stopped and no longer
// registered, as Stop leaves it. A service that crashed is still registered.
func (s Status) Off() bool { return !s.Running && !s.Registered }

// labels are the launchd labels Homebrew gives service's job, newest first.
func labels(service string) []string {
	return []string{"sh.brew." + service, "homebrew.mxcl." + service}
}

// pidLine is the line of launchctl print that brew services reads the pid from.
var pidLine = regexp.MustCompile(`pid = ([0-9]+)`)

// job is service's launchd job: its label, its state, and whether launchd has
// it loaded in the user's domain.
type job struct {
	label  string
	status Status
	loaded bool
}

// find reads service's job as brew services info does: a plist in the user's
// LaunchAgents, or a root one in LaunchDaemons, registers it, and launchctl
// print in the user's domain says whether it is loaded and its pid.
func find(ctx context.Context, r run.Runner, p paths.Paths, service string) job {
	j := job{label: labels(service)[0]}
	for _, label := range labels(service) {
		if exists(filepath.Join(p.LaunchAgents, label+".plist")) {
			j.label, j.status.Registered, j.status.User = label, true, p.User
			break
		}
		if exists(filepath.Join(p.LaunchDaemons, label+".plist")) {
			j.label, j.status.Registered, j.status.User = label, true, "root"
			break
		}
	}
	out, err := r.Run(ctx, p.Launchctl, "print", target(p, j.label))
	if err != nil {
		return j // launchctl print fails for a job that is not loaded
	}
	j.loaded = true
	if m := pidLine.FindStringSubmatch(out); m != nil {
		j.status.PID, _ = strconv.Atoi(m[1]) // the pattern only matches digits
		j.status.Running = true
	}
	return j
}

// Info returns the state of service.
func Info(ctx context.Context, r run.Runner, p paths.Paths, service string) Status {
	return find(ctx, r, p, service).status
}

// Restart stops service if it runs, starts it, and registers it to start at
// login. A job that is registered and loaded restarts through launchctl
// kickstart; any other goes through brew services restart, which writes and
// loads the plist first.
func Restart(ctx context.Context, r run.Runner, p paths.Paths, service string) error {
	if j := find(ctx, r, p, service); j.loaded && j.status.User == p.User {
		if _, err := r.Run(ctx, p.Launchctl, "kickstart", "-k", target(p, j.label)); err != nil {
			return fmt.Errorf("restart %s: %w", service, err)
		}
		return nil
	}
	if _, err := r.Run(ctx, p.Brew, "services", "restart", service); err != nil {
		return fmt.Errorf("restart %s: %w", service, err)
	}
	return nil
}

// Stop stops service and unregisters it, so it does not start again at login.
func Stop(ctx context.Context, r run.Runner, p paths.Paths, service string) error {
	if _, err := r.Run(ctx, p.Brew, "services", "stop", service); err != nil {
		return fmt.Errorf("stop %s: %w", service, err)
	}
	return nil
}

// target is label's job in the user's launchd domain.
func target(p paths.Paths, label string) string {
	return "gui/" + strconv.Itoa(p.UID) + "/" + label
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
