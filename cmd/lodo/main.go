// Command lodo manages .test names on macOS through dnsmasq, /etc/resolver
// files and Caddy.
package main

import (
	"context"
	"errors"
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
  lodo dev        start the project in this folder on its names; the first run sets
                  them up, --edit changes them, -y or --auto saves what lodo proposes
                  without the form: a new worktree gets <main><n>.test on 127.0.1.<n>
  lodo exec CMD   run CMD here with the variables lodo dev sets, such as
                  lodo exec pnpm db:migrate
  lodo ps         open the dashboard: the list of names, their checks, and keys to
                  change them
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
// the command failed, 2 for bad usage. No command prints the help.
func dispatch(args []string, stdout, stderr io.Writer) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "version", "--version":
		fmt.Fprintln(stdout, version())
		return 0
	case "", "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "ps", "setup", "apply", "doctor", "uninstall":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "lodo: %s takes no arguments\n\n%s", command, usage)
			return 2
		}
		return runApp(command, options{}, stdout, stderr)
	case "scan", "dev":
		o, err := parseFlags(command, args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "lodo: %s: %v\n\n%s", command, err, usage)
			return 2
		}
		return runApp(command, o, stdout, stderr)
	case "exec":
		args = args[1:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			fmt.Fprintf(stderr, "lodo: exec needs a command to run\n\n%s", usage)
			return 2
		}
		return runApp(command, options{args: args}, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "lodo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// options are the commands' flags: scan's JSON output and the project's name
// the user chose, with or without .test, dev's --edit and -y or --auto, and
// the command exec runs.
type options struct {
	json bool
	name string
	edit bool
	auto bool
	args []string
}

// parseFlags reads the flags of command, scan or dev.
func parseFlags(command string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if command == "scan" {
		fs.BoolVar(&o.json, "json", false, "")
		fs.StringVar(&o.name, "name", "", "")
	} else {
		fs.BoolVar(&o.edit, "edit", false, "")
		fs.BoolVar(&o.auto, "y", false, "")
		fs.BoolVar(&o.auto, "auto", false, "")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.edit && o.auto {
		return o, errors.New("--edit opens the form and -y skips it: pick one")
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

// runApp runs a command that reads or changes the system, as the user. o
// holds scan's and dev's flags.
func runApp(command string, o options, stdout, stderr io.Writer) int {
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

	return newApp(p, run.Exec{}, stdout, stderr).command(ctx, command, o)
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
