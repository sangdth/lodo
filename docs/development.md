# Development

Contributor guide for building, testing, and reinstalling `lodo` from source. See the [README](../README.md) for quick
start and [docs/reference.md](reference.md) for user-facing behavior.

## Run tests and checks

```sh
cargo test --locked                                   # all unit + hermetic E2E tests (temp dir only)
cargo fmt --all -- --check                            # verify formatting
cargo clippy --all-targets --locked -- -D warnings    # strict lint gate
cargo run -- doctor                                   # run lodo from source (read-only)
cargo run -- scan                                     # inspect the current project (read-only)
```

`tests/e2e_dataplane.rs` runs automatically as part of `cargo test --locked` when `dnsmasq`, `caddy`, `node`, and
`swiftc` are installed, starting real daemons on free ports inside temporary directories without `sudo`.

`tests/e2e_system.rs` (`#[ignore]`) exercises the full `lodo setup` -> `lodo menubar` -> `lodo dev` -> `lodo ls` ->
`lodo doctor` -> `lodo apply` -> `lodo exec` -> `lodo prune` -> `lodo uninstall` lifecycle against the real macOS host.
Because it overwrites `~/.config/lodo`, `/etc/resolver`, and `lo0` before uninstalling, run it only on a disposable
macOS VM or in CI:

```sh
LODO_E2E_DESTRUCTIVE=1 cargo test --locked --test e2e_system -- --ignored --nocapture
```

Override the version printed by `lodo version` at build time:

```sh
LODO_VERSION=v0.1.0 cargo build --release --locked
```

Further project docs:

- [docs/reference.md](reference.md) — full command and GUI behavior
- [AGENTS.md](../AGENTS.md) — code layout and development rules

## Change it and use it right away

```sh
cargo install --path . --locked   # installs ~/.cargo/bin/lodo
```

Ensure `~/.cargo/bin` comes before any older `lodo` binary on your `PATH` (`whence -a lodo`).

| What you changed                                    | After `cargo install --path . --locked`     |
| --------------------------------------------------- | ------------------------------------------- |
| CLI commands, `lodo ls`, checks, messages           | nothing, just run `lodo ls` or `lodo dev`   |
| The listen hook (`src/bind.cjs`)                    | nothing: the next `lodo dev` writes it      |
| The menu bar app (`src/menubar.swift`)              | `lodo menubar` (or `lodo apply`)            |
| What goes into the generated dnsmasq or Caddy files | `lodo apply`                                |
| The root script, sudoers rule, or loopback job      | `lodo setup`, which asks for your password  |

`lodo doctor` tells you which applies: "script out of date" means `lodo setup`, and "generated files out of date"
means `lodo apply`.
