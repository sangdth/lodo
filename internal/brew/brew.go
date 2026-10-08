// Package brew runs brew services for dnsmasq and caddy: it reads a service's
// state, restarts it and stops it, always as the current user.
package brew

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sangdth/oo/internal/run"
)

// Status is one service as brew services info --json reports it. JSON nulls,
// such as the pid of a stopped service, decode to zero values, and fields oo
// does not read are ignored.
type Status struct {
	Name       string `json:"name"`       // the formula, such as "dnsmasq"
	Running    bool   `json:"running"`    // the job has a process
	Loaded     bool   `json:"loaded"`     // launchd has the job loaded
	User       string `json:"user"`       // who the job runs as; empty when brew reports none
	PID        int    `json:"pid"`        // 0 when not running
	Status     string `json:"status"`     // brew's summary, such as "started", "none" or "error"
	File       string `json:"file"`       // the launchd plist brew uses for the job
	Registered bool   `json:"registered"` // the job starts at login; Stop clears it, a crash does not
}

// Off reports whether the service was turned off: stopped and no longer
// registered, as Stop leaves it. A service that crashed is still registered.
func (s Status) Off() bool { return !s.Running && !s.Registered }

// Info returns the state of service from brew services info <service> --json.
func Info(ctx context.Context, r run.Runner, brew, service string) (Status, error) {
	out, err := r.Run(ctx, brew, "services", "info", service, "--json")
	if err != nil {
		return Status{}, fmt.Errorf("read %s status: %w", service, err)
	}
	var list []Status
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return Status{}, fmt.Errorf("parse %s status: %w", service, err)
	}
	if len(list) == 0 {
		return Status{}, fmt.Errorf("parse %s status: brew listed no service", service)
	}
	return list[0], nil
}

// Restart stops service if it runs, starts it, and registers it to start at
// login.
func Restart(ctx context.Context, r run.Runner, brew, service string) error {
	if _, err := r.Run(ctx, brew, "services", "restart", service); err != nil {
		return fmt.Errorf("restart %s: %w", service, err)
	}
	return nil
}

// Stop stops service and unregisters it, so it does not start again at login.
func Stop(ctx context.Context, r run.Runner, brew, service string) error {
	if _, err := r.Run(ctx, brew, "services", "stop", service); err != nil {
		return fmt.Errorf("stop %s: %w", service, err)
	}
	return nil
}
