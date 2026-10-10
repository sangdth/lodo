// Command lodo manages .test names on macOS through dnsmasq, /etc/resolver
// files and Caddy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

const usage = `lodo manages .test names on macOS through dnsmasq.

Usage:
  lodo            open the TUI: the list of names, their checks, and keys to change them
  lodo setup      one-time system setup; asks for your password
  lodo apply      write the configs from domains.json, reload, and check every name
  lodo doctor     check every part and print what to fix
  lodo scan       print what the project in this folder needs; --json for scripts,
                  --name hugger to name the project
  lodo uninstall  remove what setup installed; keeps domains.json
  lodo version    print the version
  lodo help       print this help
`

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch runs one command and returns the exit code: 0 on success, 1 when
// the command failed, 2 for bad usage. No command opens the TUI.
func dispatch(args []string, stdout, stderr io.Writer) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "version", "--version":
		fmt.Fprintln(stdout, version())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "", "setup", "apply", "doctor", "uninstall":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "lodo: %s takes no arguments\n\n%s", command, usage)
			return 2
		}
		return runApp(command, scanFlags{}, stdout, stderr)
	case "scan":
		sf, err := parseScanFlags(args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "lodo: scan: %v\n\n%s", err, usage)
			return 2
		}
		return runApp(command, sf, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "lodo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// scanFlags are lodo scan's options: JSON output, and the project's name the
// user chose, with or without .test.
type scanFlags struct {
	json bool
	name string
}

func parseScanFlags(args []string) (scanFlags, error) {
	var sf scanFlags
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&sf.json, "json", false, "")
	fs.StringVar(&sf.name, "name", "", "")
	if err := fs.Parse(args); err != nil {
		return sf, err
	}
	if fs.NArg() > 0 {
		return sf, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return sf, nil
}

// runApp runs the TUI or a command that reads or changes the system, as the
// user. sf holds scan's options.
func runApp(command string, sf scanFlags, stdout, stderr io.Writer) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "lodo: run lodo as your user, not with sudo; it asks for your password when it needs it")
		return 2
	}
	p, err := paths.Default()
	if err != nil {
		fmt.Fprintf(stderr, "lodo: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	a := newApp(p, run.Exec{}, stdout, stderr)
	switch command {
	case "":
		return a.tui(ctx)
	case "setup":
		return a.setup(ctx)
	case "apply":
		return a.apply(ctx)
	case "doctor":
		return a.doctor(ctx)
	case "scan":
		dir, err := os.Getwd()
		if err != nil {
			a.fail(fmt.Errorf("find the current folder: %w", err))
			return 1
		}
		return a.scan(dir, sf)
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
