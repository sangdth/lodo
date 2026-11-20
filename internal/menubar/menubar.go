// Package menubar manages the macOS menu bar icon helper (io.lodo.menubar):
// compiling its embedded Swift source and starting, stopping, and reading its
// user LaunchAgent job.
package menubar

import (
	"context"
	_ "embed" // for swiftSource
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// Label is the launchd label of the user LaunchAgent that keeps the menu bar
// icon running.
const Label = "io.lodo.menubar"

//go:embed menubar.swift
var swiftSource []byte

// Source returns the embedded Swift source of the menu bar app.
func Source() []byte { return swiftSource }

var pidLine = regexp.MustCompile(`pid = ([0-9]+)`)

// Status is the menu bar LaunchAgent's state in the user's launchd domain.
type Status struct {
	Running    bool
	PID        int
	Registered bool
}

// Off reports whether the menu bar icon is turned off: stopped and not
// registered.
func (s Status) Off() bool { return !s.Running && !s.Registered }

// Info returns the state of the menu bar LaunchAgent.
func Info(ctx context.Context, r run.Runner, p paths.Paths) Status {
	st := Status{Registered: fsutil.Exists(p.MenubarPlist)}
	out, err := r.Run(ctx, p.Launchctl, "print", target(p))
	if err != nil {
		return st
	}
	if m := pidLine.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
		st.Running = true
	}
	return st
}

// Plist returns the LaunchAgent plist that starts the menu bar binary at login.
func Plist(p paths.Paths) string {
	esc := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>LODO_DOMAINS_JSON</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, Label, esc(p.MenubarBin), esc(p.DomainsJSON))
}

// Start compiles the menu bar helper when missing or out of date, writes its
// LaunchAgent plist, and starts or restarts the job in the user's GUI domain.
func Start(ctx context.Context, p paths.Paths, r run.Runner) error {
	if err := ensureBinary(ctx, p, r); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.MenubarPlist), 0o755); err != nil { //nolint:gosec // G301: ~/Library/LaunchAgents holds user plists
		return fmt.Errorf("create %s: %w", filepath.Dir(p.MenubarPlist), err)
	}
	if _, err := fsutil.WriteFile(p.MenubarPlist, []byte(Plist(p)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p.MenubarPlist, err)
	}
	if _, err := r.Run(ctx, p.Launchctl, "print", target(p)); err == nil {
		if _, err := r.Run(ctx, p.Launchctl, "kickstart", "-k", target(p)); err != nil {
			return fmt.Errorf("restart menubar: %w", err)
		}
		return nil
	}
	if _, err := r.Run(ctx, p.Launchctl, "bootstrap", domain(p), p.MenubarPlist); err != nil {
		return fmt.Errorf("start menubar: %w", err)
	}
	return nil
}

// Stop unloads the menu bar LaunchAgent and removes its plist so it stays
// hidden across logins. It succeeds when the job is already stopped.
func Stop(ctx context.Context, p paths.Paths, r run.Runner) error {
	registered := fsutil.Exists(p.MenubarPlist)
	if err := os.Remove(p.MenubarPlist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", p.MenubarPlist, err)
	}
	if !registered {
		return nil
	}
	if _, err := r.Run(ctx, p.Launchctl, "print", target(p)); err == nil {
		if _, err := r.Run(ctx, p.Launchctl, "bootout", target(p)); err != nil {
			return fmt.Errorf("stop menubar: %w", err)
		}
	}
	return nil
}

func ensureBinary(ctx context.Context, p paths.Paths, r run.Runner) error {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: ~/.config/lodo holds no secrets
		return fmt.Errorf("create %s: %w", p.ConfigDir, err)
	}
	srcPath := p.MenubarBin + ".swift"
	changed, err := fsutil.WriteFile(srcPath, swiftSource, 0o644)
	if err != nil {
		return fmt.Errorf("write %s: %w", srcPath, err)
	}
	if !changed && fsutil.Exists(p.MenubarBin) {
		return nil
	}
	if !fsutil.Exists(p.Swiftc) {
		return errors.New("swiftc is not installed: xcode-select --install")
	}
	if _, err := r.Run(ctx, p.Swiftc, "-O", srcPath, "-o", p.MenubarBin); err != nil {
		return fmt.Errorf("build menubar: %w", err)
	}
	return nil
}

func domain(p paths.Paths) string { return "gui/" + strconv.Itoa(p.UID) }
func target(p paths.Paths) string { return domain(p) + "/" + Label }
