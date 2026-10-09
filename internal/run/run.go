// Package run runs external commands. Exec runs them for real; Fake records
// them for tests.
package run

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs external commands.
type Runner interface {
	// Run runs name with args and returns its standard output. A failed
	// command returns an *Error carrying its standard error.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// RunInput runs name with args like Run, with stdin as its standard
	// input.
	RunInput(ctx context.Context, stdin, name string, args ...string) (string, error)
	// RunTTY runs name with args attached to the terminal, so sudo can ask
	// for a password.
	RunTTY(ctx context.Context, name string, args ...string) error
}

// Error is a command that failed to start, exited non-zero or ran out of time.
type Error struct {
	Cmd    string // the command line, for messages
	Stderr string // trimmed standard error
	Err    error
}

func (e *Error) Error() string {
	if e.Stderr != "" {
		return e.Cmd + ": " + e.Stderr
	}
	return e.Cmd + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Line joins a command and its arguments for messages and Fake keys.
func Line(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

const defaultTimeout = 30 * time.Second

// Exec runs commands for real.
type Exec struct {
	// Timeout bounds every Run whose context has no deadline. Zero means 30s.
	Timeout time.Duration
}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, name string, args ...string) (string, error) {
	return e.run(ctx, nil, name, args...)
}

// RunInput implements Runner.
func (e Exec) RunInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	return e.run(ctx, strings.NewReader(stdin), name, args...)
}

// run runs name with stdin as its standard input, none when nil, under e's
// timeout.
func (e Exec) run(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		timeout := e.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: tools come from paths.Paths and arguments are passed separately, never through a shell
	cmd.WaitDelay = time.Second
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return stdout.String(), &Error{Cmd: Line(name, args...), Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.String(), nil
}

// RunTTY implements Runner. It has no timeout: a password prompt waits for
// the user.
func (e Exec) RunTTY(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: same as Run
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return &Error{Cmd: Line(name, args...), Err: err}
	}
	return nil
}
