# AGENTS.md

`lodo` is a Rust CLI and native macOS menu bar app that manages `.lodo` and custom local domain names on macOS through
dnsmasq, `/etc/resolver` files, and Caddy. The README stays short; user-facing detail goes in `docs/reference.md`, and
contributor/build notes go in `docs/development.md`.

## Commands

```bash
cargo test --locked                                   # all unit + hermetic E2E tests
cargo test --locked store                             # tests matching "store"
cargo fmt --all -- --check                            # formatting check
cargo clippy --all-targets --locked -- -D warnings    # lint gate
cargo run -- doctor                                   # run the CLI from source; read-only
cargo run -- scan --json                              # inspect current project; read-only
```

Every finished change ends with the steps that make it take effect: `cargo install --path . --locked` from the repo
root (or `brew upgrade lodo` for released versions), then nothing, `lodo menubar`, `lodo apply`, or `lodo setup`, per
the table in `docs/development.md` ("Change it and use it right away").

A commit requires `cargo fmt --all -- --check`, `cargo clippy --all-targets --locked -- -D warnings`, and
`cargo test --locked` clean; chain them with `set -e` so a failure stops the commit.

## Layout

- `src/main.rs`, `src/cli.rs` (`src/cli/`): subcommand dispatch, flags, and output. The only code that constructs
  `Paths::default_paths()` and `ExecRunner`. Commands: `dev`, `exec`, `ls`, `add`, `set`, `rm`, `service`, `menubar`,
  `setup`, `apply`, `doctor`, `scan`, `prune`, `uninstall`, `version`, `help`.
- `src/ls.rs`: `lodo ls` service + domain status report (`LsReport`), ANSI/plain table formatter, and `--json` schema
  consumed by the macOS menu bar GUI.
- `src/menubar.rs`, `src/menubar.swift`: native AppKit + SwiftUI macOS menu bar GUI and `io.lodo.menubar` user
  LaunchAgent (`start`, `refresh`, `stop`, `running`). Uses prebuilt `/opt/homebrew/opt/lodo/libexec/lodo-menubar` when
  installed via Homebrew, or compiles `menubar.swift` on demand to `~/.config/lodo/lodo-menubar` in local `cargo`
  builds. The Swift GUI never writes `domains.json` directly; it invokes `lodo` CLI commands asynchronously.
- `src/paths.rs`: every file, directory, and tool path. `Paths::for_test(root)` relocates all of them under `root`.
- `src/run.rs`: the only code that spawns external processes (`Runner`, `ExecRunner` with deadlines and `execvp`
  `replace`, `FakeRunner` for tests).
- `src/fsutil.rs`: atomic temp+sync+rename writes (`write_file`) and bounded regular-file reads (`read_regular`).
- `src/store.rs`: `domains.json` persistence, `flock` locking (`with_lock`), and all domain validation/sorting rules
  (`Domain`, `FieldError`, `validate_name`, `validate_project`, `resolve_name`, `cut_project`, `next_free`, `sort`).
- `src/brew.rs`, `src/dnsmasq.rs`, `src/caddy.rs`: `launchd` service control (`launchctl print` and `kickstart -k`),
  generated `dnsmasq.conf` + resolver list + log tail, and generated `Caddyfile` + `caddy validate`.
- `src/system/`: `setup`, `apply`, `set_service`, `uninstall`; rendered `apply-resolvers.sh`, `sudoers`, and
  `io.lodo.loopback.plist`; `dnsmasq.conf` and `Caddyfile` marker blocks and backup restoration.
- `src/compose/`: project root discovery (`project_root`), compose file discovery (`find`), `saphyr` service/port
  parsing (`services`), and localhost URL mapping (`map_urls`, `Values::point`). Reads files and never writes them.
- `src/scan/`: discovers workspace apps, 15 dev-server frameworks, ports, Mailpit web pages, git worktree slots, and
  `.env` variables (`Proposal`, `Var`). `Var` masks URL credentials as `***@` in `value` and redacts `Debug`/`Serialize`
  output, exposing unmasked `KEY=value` only through `Var::env_pair()`.
- `src/dev.rs`, `src/bind.cjs`: package manager detection, port-to-address map (`BTreeMap`), embedded Node listen hook,
  and `NODE_OPTIONS` builder.
- `src/check/`: the 8 `lodo doctor` checks (`ID_DNSMASQ` … `ID_CADDY`) and concurrent UDP DNS, `dscacheutil`, and
  `rustls` + `rustls-platform-verifier` HTTP/HTTPS probes.
- `tests/`: hermetic E2E (`e2e_dataplane.rs`), destructive real-Mac E2E (`e2e_system.rs`, `#[ignore]`), and fixtures.

## Rules

- No unit or default test changes the system: nothing writes to `/etc`, `/Library`, `/opt/homebrew`, or the real
  `~/.config/lodo`, and nothing runs `sudo` or starts a LaunchAgent. Tests use `Paths::for_test` and `FakeRunner`.
- `lodo apply`, `lodo setup`, `lodo uninstall`, `lodo prune`, `lodo add`, `lodo set`, `lodo rm`, `lodo service`,
  `lodo menubar`, and `lodo dev` change this Mac's DNS or services, `lodo dev` starts dev servers, and `lodo exec`
  runs arbitrary commands: run them only when the task asks for it. `lodo doctor`, `lodo scan`, `lodo version`, and
  `cargo test --locked` are safe.
- `#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]` is enforced across the crate. Never use
  `.unwrap()` or `.expect()` in production code; use `#[expect(clippy::expect_used)]` only on compile-time static
  `LazyLock<Regex>` items.
- Every external command goes through `run::Runner` with absolute tool paths from `paths::Paths` and separate
  arguments — never through a shell command string built from data.
- `apply-resolvers.sh` reads domain names on standard input (`run_input`), caps input at `MAX_SCRIPT_INPUT` bytes in
  its own `mktemp -d`, creates `/etc/resolver` files only for lines matching `store::NAME_PATTERN`, and deletes only
  files starting with `# lodo`.
- Setup trusts Caddy's local CA (`~/.config/lodo/caddy-root.crt` in `System.keychain`) only when an enabled domain has
  HTTPS on, and ensures `admin off` in Homebrew's `Caddyfile`. Every generated Caddy site aborts connections outside
  `127.0.0.0/8` and `::1`, passes `Host` through unchanged, and rewrites `Origin` to `http://localhost:<port>` only on
  Next.js dev paths (`*/_next/*`, `*/__nextjs*`) when matching the site's own origin.
- `lodo` never modifies project files on disk; per-checkout values are exported as environment variables by `lodo dev`
  and `lodo exec`, and project files are read only through `fsutil::read_regular`.

## Rust development

Before any Rust coding, review, refactoring, or testing task, load the `rust-best-practices` and `rust-testing` skills;
load `rust-refactor-helper` as well when refactoring.
