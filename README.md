# lodo

Run multiple projects on one Mac at once, each database and app on its usual port. Every project gets its own
loopback address (`127.0.1.x`) and a `.lodo` (or custom) domain name, backed by dnsmasq, `/etc/resolver`, Caddy, and a
native macOS menu bar app.

```text
media.lodo -> 127.0.1.3 -> 127.0.1.3:5432 (Postgres), 127.0.1.3:3000 (Next)
blog.lodo  -> 127.0.1.1 -> 127.0.1.1:5432 (Postgres)
http://dashboard.blog.lodo -> Caddy on port 80 -> 127.0.1.1:3000
```

## Quick start

Requires macOS on Apple Silicon with Homebrew in `/opt/homebrew` and an admin account.

```sh
# 1. Install lodo; Homebrew also installs its dnsmasq and caddy
brew install sangdth/tap/lodo

# 2. Start a project (runs one-time Mac setup, registers names, and opens the menu bar icon)
cd ~/Projects/shop
lodo dev

# 3. Inspect domains in the terminal or toggle the macOS menu bar icon anytime
lodo ls
lodo menubar
```

Upgrade `lodo` anytime with `brew upgrade lodo`.

For Docker Compose, bind ports with `"${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"`. `lodo dev` and `lodo exec` export
`DOCKER_HOST_IP` and your project's `.env` URLs automatically without editing any project file.

Something broken? Run `lodo doctor` first.

## Documentation

- [Setup & how it works](docs/reference.md#setup) — what one-time setup changes, rules, and uninstall
- [Using lodo & the menu bar](docs/reference.md#using-lodo) — `lodo dev`, `lodo exec`, `lodo ls`, worktrees, and GUI
- [Commands](docs/reference.md#commands) — full CLI reference (`dev`, `exec`, `ls`, `add`, `set`, `rm`, `menubar`, …)
- [HTTPS & sign-in callbacks](docs/reference.md#https) — local TLS certificates and OAuth (including Google sign-in)
- [Troubleshooting](docs/reference.md#troubleshooting) — common errors and fixes
- [Development](docs/development.md) — running tests, linting, building from source, and reinstalling locally
