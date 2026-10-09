package run_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sangdth/lodo/internal/run"
)

func TestExec_Run(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cmd        string
		args       []string
		timeout    time.Duration
		wantOut    string
		wantErr    string
		wantStderr string
	}{
		{name: "stdout only", cmd: "/bin/sh", args: []string{"-c", "echo out; echo noise >&2"}, wantOut: "out\n"},
		{
			name: "failure carries stderr", cmd: "/bin/sh", args: []string{"-c", "echo partial; echo boom >&2; exit 3"},
			wantOut: "partial\n", wantStderr: "boom", wantErr: "/bin/sh -c echo partial; echo boom >&2; exit 3: boom",
		},
		{
			name: "missing tool", cmd: "/nonexistent/tool",
			wantErr: "/nonexistent/tool: fork/exec /nonexistent/tool: no such file or directory",
		},
		{
			name: "timeout", cmd: "/bin/sleep", args: []string{"5"}, timeout: 50 * time.Millisecond,
			wantErr: "/bin/sleep 5: context deadline exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := run.Exec{Timeout: tt.timeout}.Run(context.Background(), tt.cmd, tt.args...)
			if out != tt.wantOut {
				t.Errorf("out = %q, want %q", out, tt.wantOut)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			re, ok := errors.AsType[*run.Error](err)
			if !ok {
				t.Fatalf("err = %v (%T), want *run.Error", err, err)
			}
			if re.Error() != tt.wantErr {
				t.Errorf("err = %q, want %q", re.Error(), tt.wantErr)
			}
			if re.Stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", re.Stderr, tt.wantStderr)
			}
		})
	}
}

func TestExec_RunTimeoutUnwraps(t *testing.T) {
	t.Parallel()

	_, err := run.Exec{Timeout: 20 * time.Millisecond}.Run(context.Background(), "/bin/sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded in the chain", err)
	}
}

func TestExec_RunInput(t *testing.T) {
	t.Parallel()

	out, err := run.Exec{}.RunInput(context.Background(), "a\nb\n", "/usr/bin/wc", "-l")
	if err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("out = %q, want 2 lines counted", out)
	}

	_, err = run.Exec{Timeout: 50 * time.Millisecond}.RunInput(context.Background(), "", "/bin/sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded in the chain", err)
	}
}

func TestLine(t *testing.T) {
	t.Parallel()

	if got := run.Line("/usr/bin/sudo", "-n", "/a b/c.sh"); got != "/usr/bin/sudo -n /a b/c.sh" {
		t.Errorf("Line = %q", got)
	}
}

func TestFake(t *testing.T) {
	t.Parallel()

	f := run.NewFake()
	f.Set("brew services info dnsmasq --json", `[{"running":true}]`)
	f.Fail("sudo -n script", "sudo: a password is required")
	ctx := context.Background()

	if out, err := f.Run(ctx, "brew", "services", "info", "dnsmasq", "--json"); err != nil || out != `[{"running":true}]` {
		t.Errorf("Set answer: out %q, err %v", out, err)
	}
	_, err := f.Run(ctx, "sudo", "-n", "script")
	re, ok := errors.AsType[*run.Error](err)
	if !ok || re.Stderr != "sudo: a password is required" {
		t.Errorf("Fail answer: err %v", err)
	}
	if out, err := f.Run(ctx, "unknown"); err != nil || out != "" {
		t.Errorf("unknown command: out %q, err %v, want success with no output", out, err)
	}
	if err := f.RunTTY(ctx, "sudo", "-v"); err != nil {
		t.Errorf("RunTTY: %v", err)
	}
	if _, err := f.RunInput(ctx, "a.test\n", "sudo", "-n", "script"); err == nil {
		t.Error("RunInput: err nil, want the Fail answer")
	}
	_, _ = f.RunInput(ctx, "", "sudo", "-n", "script")
	if got := f.Inputs("sudo -n script"); !slices.Equal(got, []string{"a.test\n", ""}) {
		t.Errorf("Inputs = %q, want both inputs in order", got)
	}
	if got := f.Inputs("sudo -v"); got != nil {
		t.Errorf("Inputs of a Run-only line = %q, want none", got)
	}
	want := []string{
		"brew services info dnsmasq --json", "sudo -n script", "unknown", "sudo -v",
		"sudo -n script", "sudo -n script",
	}
	got := f.Calls()
	if len(got) != len(want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}
