// Package menubar manages the macOS menu bar icon helper (io.lodo.menubar):
// compiling its embedded Swift source and starting, stopping, refreshing, and
// checking its user LaunchAgent job.
package menubar

import (
	"bytes"
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
	"time"

	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// Label is the launchd label of the user LaunchAgent that keeps the menu bar
// icon running.
const Label = "io.lodo.menubar"

// buildTimeout bounds swiftc compilation on a cold macOS SDK cache.
const buildTimeout = 2 * time.Minute

//go:embed menubar.swift
var swiftSource []byte

// Source returns the embedded Swift source of the menu bar app.
func Source() []byte { return swiftSource }

var pidLine = regexp.MustCompile(`pid = ([0-9]+)`)

// Running reports whether the menu bar LaunchAgent has a running process in
// the user's launchd domain.
func Running(ctx context.Context, r run.Runner, p paths.Paths) bool {
	out, err := r.Run(ctx, p.Launchctl, "print", target(p))
	return err == nil && pidLine.MatchString(out)
}

// Plist returns the LaunchAgent plist that starts the menu bar binary at login
// and restarts it only on an abnormal exit so clean quits stay hidden.
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
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
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

// Refresh rebuilds and restarts the menu bar helper when it is enabled and its
// binary is missing or built from older source. It is a no-op when disabled or
// already up to date.
func Refresh(ctx context.Context, p paths.Paths, r run.Runner) error {
	if !fsutil.Exists(p.MenubarPlist) || upToDate(p) {
		return nil
	}
	return Start(ctx, p, r)
}

// Stop unloads the menu bar LaunchAgent if loaded and removes its plist so it
// stays hidden across logins. It succeeds when the job is already stopped.
func Stop(ctx context.Context, p paths.Paths, r run.Runner) error {
	if _, err := r.Run(ctx, p.Launchctl, "print", target(p)); err == nil {
		if _, err := r.Run(ctx, p.Launchctl, "bootout", target(p)); err != nil {
			return fmt.Errorf("stop menubar: %w", err)
		}
	}
	if err := os.Remove(p.MenubarPlist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", p.MenubarPlist, err)
	}
	return nil
}

func upToDate(p paths.Paths) bool {
	if !fsutil.Exists(p.MenubarBin) {
		return false
	}
	got, err := os.ReadFile(p.MenubarBin + ".swift")
	return err == nil && bytes.Equal(got, swiftSource)
}

func ensureBinary(ctx context.Context, p paths.Paths, r run.Runner) error {
	if upToDate(p) {
		return nil
	}
	if !fsutil.Exists(p.Swiftc) {
		return errors.New("swiftc is not installed: xcode-select --install")
	}
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: ~/.config/lodo holds no secrets
		return fmt.Errorf("create %s: %w", p.ConfigDir, err)
	}
	tmpSrc := p.MenubarBin + ".tmp.swift"
	tmpBin := p.MenubarBin + ".tmp"
	defer func() {
		_ = os.Remove(tmpSrc)
		_ = os.Remove(tmpBin)
	}()
	if err := os.WriteFile(tmpSrc, swiftSource, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmpSrc, err)
	}
	// Pre-create tmpBin so atomic rename also succeeds under run.Fake in tests;
	// real swiftc replaces it with an executable Mach-O binary.
	if err := os.WriteFile(tmpBin, nil, 0o700); err != nil { //nolint:gosec // G302: replaced by swiftc with executable permissions
		return fmt.Errorf("create %s: %w", tmpBin, err)
	}
	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	if _, err := r.Run(buildCtx, p.Swiftc, "-O", tmpSrc, "-o", tmpBin); err != nil {
		return fmt.Errorf("build menubar: %w", err)
	}
	if err := os.Rename(tmpBin, p.MenubarBin); err != nil {
		return fmt.Errorf("install menubar: %w", err)
	}
	if _, err := fsutil.WriteFile(p.MenubarBin+".swift", swiftSource, 0o644); err != nil {
		return fmt.Errorf("record menubar source: %w", err)
	}
	return nil
}

func domain(p paths.Paths) string { return "gui/" + strconv.Itoa(p.UID) }
func target(p paths.Paths) string { return domain(p) + "/" + Label }
