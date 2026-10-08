// Command lcd manages .local names on macOS through dnsmasq, /etc/resolver
// files and Caddy.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

const usage = `lcd manages .local names on macOS through dnsmasq.

Usage:
  lcd version    print the version
  lcd help       print this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one command and returns the exit code: 0 on success, 1 when
// the command failed, 2 for bad usage.
func run(args []string, stdout, stderr io.Writer) int {
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
	default:
		fmt.Fprintf(stderr, "lcd: unknown command %q\n\n%s", args[0], usage)
		return 2
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
