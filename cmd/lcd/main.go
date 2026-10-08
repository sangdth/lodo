// Command lcd manages .local names on macOS through dnsmasq, /etc/resolver
// files and Caddy.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

const usage = `lcd manages .local names on macOS through dnsmasq.

Usage:
  lcd setup      one-time system setup; asks for your password
  lcd apply      write the configs from domains.json, reload, and check every name
  lcd doctor     check every part and print what to fix
  lcd uninstall  remove what setup installed; keeps domains.json
  lcd version    print the version
  lcd help       print this help
`

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch runs one command and returns the exit code: 0 on success, 1 when
// the command failed, 2 for bad usage.
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "setup", "apply", "doctor", "uninstall":
		return runSystem(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "lcd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// runSystem runs a command that reads or changes the system, as the user.
func runSystem(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintf(stderr, "lcd: %s takes no arguments\n\n%s", args[0], usage)
		return 2
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "lcd: run lcd as your user, not with sudo; it asks for your password when it needs it")
		return 2
	}
	p, err := paths.Default()
	if err != nil {
		fmt.Fprintf(stderr, "lcd: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	a := newApp(p, run.Exec{}, stdout, stderr)
	switch args[0] {
	case "setup":
		return a.setup(ctx)
	case "apply":
		return a.apply(ctx)
	case "doctor":
		return a.doctor(ctx)
	default:
		return a.uninstall(ctx)
	}
}

// version prefers the -ldflags value, then the module version that
// `go install ...@v1.2.3` records.
func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}
