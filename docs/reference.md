# lodo reference

The full behavior behind the [README](../README.md): what each command changes, the rules, the macOS menu bar GUI, and
fixes.

## Setup

Needs macOS on Apple Silicon with Homebrew in `/opt/homebrew` and an admin account.

Run `lodo setup` as yourself, not with `sudo`. It asks for your password once, before it changes anything. Then it:

1. creates `~/.config/lodo`, an empty `domains.json` when there is none, and the files `lodo` generates from it;
2. backs up Homebrew's `/opt/homebrew/etc/dnsmasq.conf` to `dnsmasq.conf.before-lodo` (first run only), drops its
   `conf-file`, `listen-address`, `port`, `bind-interfaces`, and `bind-dynamic` lines, and adds `lodo`'s block:
   include `~/.config/lodo/dnsmasq.conf` and listen on `127.0.0.1` port `53535`. Every other line stays;
3. stops any root dnsmasq job (`sudo brew services start dnsmasq`) and removes its plist from `/Library/LaunchDaemons`;
4. installs `/Library/Application Support/lodo/apply-resolvers.sh` and `/etc/sudoers.d/lodo`, allowing your user to run
   that script without a password;
5. installs `/Library/LaunchDaemons/io.lodo.loopback.plist`, adding `127.0.1.1`–`127.0.1.50` to `lo0` now and at boot;
6. removes `/etc/resolver/local` if present so `.local` stays with Bonjour;
7. starts `dnsmasq` as your user via `brew services`;
8. when Caddy is installed, backs up `/opt/homebrew/etc/Caddyfile` to `Caddyfile.before-lodo` if present, disables
   `admin`, imports `~/.config/lodo/Caddyfile`, and starts Caddy as your user;
9. when an enabled name has HTTPS on, copies Caddy's local root CA to `~/.config/lodo/caddy-root.crt` and trusts it in
   `System.keychain`;
10. proves the resolver script runs passwordlessly via `sudo -n -k`, starts the menu bar icon, and prints `lodo doctor`.

Running `lodo setup` again is safe and idempotent. After setup, adding, editing, and removing names never asks for a
password.

## Using lodo

### `lodo dev`

Run `lodo dev` inside any project folder:

```sh
cd ~/Projects/shop
lodo dev
```

1. Runs `lodo setup` first when `lodo` has not been set up on this Mac yet.
2. Refuses to start while `lodo doctor` checks 1–5 fail, printing the failing checks and fixes.
3. On first run in a project (or when `--name <domain>` changes its name), scans the project and automatically saves its
   domain, app subdomains, Mailpit `mail.<project>` subdomain, and compose file link to `domains.json`, then applies
   the configuration.
4. Detects the package manager (`packageManager` in `package.json`, else `pnpm-lock.yaml`, `yarn.lock`,
   `bun.lock`/`bun.lockb`, `package-lock.json`, else `npm`), writes `~/.config/lodo/bind.cjs` and
   `~/.config/lodo/dev/<project>.cjs`, exports `NODE_OPTIONS` and the project's environment variables, ensures the macOS
   menu bar icon is running (without restarting it when already up to date), and replaces itself (`execvp`) with
   `<pm> run dev`:

```text
web.shop.lodo  https://web.shop.lodo  -> 127.0.1.1:3001
env   DOCKER_HOST_IP=127.0.1.1
env   DATABASE_URL=postgres://***@shop.lodo:5432/shop
starting: pnpm run dev
```

The Node hook rewrites `net.Server.prototype.listen` on the project's listed ports from loopback/wildcard hosts onto
the project's `127.0.1.x` address and appends `.<project>` to `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS` so Vite accepts
requests without editing `vite.config`.

### Variables exported by `lodo dev` and `lodo exec`

`lodo` never modifies project `.env` files. Instead, `lodo dev` and `lodo exec` export environment variables that
override `.env` values in dotenv, Next.js, Node, and Docker Compose:

- `DOCKER_HOST_IP`: the project's `127.0.1.x` address, for compose ports bound to `${DOCKER_HOST_IP:-127.0.0.1}`.
- `COMPOSE_PROJECT_NAME` (in git worktrees only): the worktree's label (e.g., `shop2`), giving each worktree isolated
  containers and volumes.
- Sibling checkout and localhost URLs in each app's `.env`: rewritten in-memory to point at this checkout's `.lodo`
  names (`http://localhost:3001` -> `http://web.shop.lodo`, `postgres://...@localhost:5432` ->
  `postgres://...@shop.lodo:5432`). Credentials are always masked as `***@` in terminal and `--json` output.

`lodo exec [--] <command...>` runs one-off commands (`lodo exec pnpm db:migrate`, `lodo exec docker compose up -d`)
with the same variables and without the Node listen hook, printing exported variables to `stderr` so `stdout` stays
clean.

### `lodo ls` and the macOS menu bar GUI

`lodo ls` prints a fast, pipe-friendly status summary and domain table (`--json` outputs structured JSON; `--no-color`
disables ANSI colors):

```text
services  caddy ●  dnsmasq ●  menubar ●  loopback ●  resolvers ●

NAME                    ENABLED  ADDRESS      PORT   COMPOSE                       CHECK
● blog.lodo             ✓        127.0.1.1    –      ~/Code/blog/compose.dev.yaml  dns ✓
  ○ api.blog.lodo       ✓        127.0.1.1    3001   –                             app down
  ● web.blog.lodo       ✓        127.0.1.1    3000   –                             dns ✓  https ✓
○ old.lodo              –        127.0.0.1    –      –                             off
```

`lodo menubar` shows the native macOS menu bar icon (`--stop` hides it):

- **Services controls:** Toggle `DNS (dnsmasq)` and `Reverse Proxy (Caddy)` on or off directly from the menu.
- **Project groups:** Click any running service to open its URL in your browser (`⌥`-click copies the URL).
- **Per-domain submenu (`▸`):** Open in browser, copy URL or `DOCKER_HOST_IP`, toggle `Enabled` or `HTTPS`, open the
  native `Add Subdomain…` or `Edit…` form window (with `NSOpenPanel` compose picker), or `Delete…`.
- **Footer actions:** `Add Domain…`, `Prune Missing Projects`, `Open DNS Query Log` (opens `dnsmasq.log` in macOS
  `Console.app`), `Refresh Status`, and `Hide Menu Bar Icon`.

### Rules

- **Names default to `.lodo` and allow custom TLDs.** Typing a bare label (`shop`) resolves to `shop.lodo`; include a
  dot (`shop.custom`) to use a custom TLD. `.local` and `.localhost` are refused.
- **A project is the last two labels** (`blog.lodo`, `api.blog.lodo`).
- **Own-block addresses (`127.0.1.1`–`127.0.1.50`) belong to one project each.** `127.0.0.1` and other `127.0.0.0/8`
  addresses may be shared across projects.
- **Git worktrees** of a listed project automatically receive `<main><n>.<tld>` on `127.0.1.<n>` at the lowest free
  slot, inheriting each subdomain's HTTPS setting from the main checkout. `lodo prune` (including `--worktree` and the
  menu bar's `Prune Missing Projects`) and worktree slot reclaim in `lodo dev` stop dev servers listening on the freed
  `127.0.1.<n>` IP and remove the worktree's Compose containers, networks, and volumes by
  `com.docker.compose.project=<label>` (without touching non-worktree Docker resources or Compose setups with
  `--compose none`).
- **Ports 1–65535 except 80 and 443** are served through Caddy on port 80 (and 443 when HTTPS is enabled).

## Commands

| Command               | What it does                                                        |
| --------------------- | ------------------------------------------------------------------- |
| `lodo dev`            | Register project (`--name`), open menu bar icon, run `<pm> run dev` |
| `lodo exec CMD...`    | Run `CMD` with the project's exported variables                     |
| `lodo ls`             | Print service status and domain table (`--json`, `--no-color`)      |
| `lodo add NAME`       | Add a domain (`--address`, `--port`, `--https`, `--compose`)        |
| `lodo set NAME`       | Edit a domain (`--name`, `--address`, `--port`, `--on`/`--off`, …)  |
| `lodo rm NAME`        | Remove a domain and its subdomains                                  |
| `lodo service SVC ST` | Turn `dnsmasq`, `caddy`, or `menubar` `on` or `off`                 |
| `lodo menubar`        | Start or refresh the macOS menu bar app (`--stop` hides it)         |
| `lodo setup`          | Install or repair `lodo`'s macOS system configuration               |
| `lodo apply`          | Regenerate configs from `domains.json`, reload services, and probe  |
| `lodo doctor`         | Run all 8 health checks and print fix commands                      |
| `lodo scan`           | Inspect what the current project needs (`--json`, `--name`)         |
| `lodo prune`          | Remove domains whose project folder is gone (`--worktree`)          |
| `lodo uninstall`      | Undo `lodo setup`; keep `~/.config/lodo`                            |
| `lodo version`        | Print the version                                                   |

## HTTPS

Enable HTTPS on any domain with a port from the menu bar (`HTTPS` toggle) or CLI (`lodo set web.shop.lodo --https`).
Caddy serves both `http://<name>` and `https://<name>` using its local CA. Run `lodo setup` once after enabling your
first HTTPS domain so macOS trusts `~/.config/lodo/caddy-root.crt` in `System.keychain`.

For OAuth providers like Google that reject non-public-suffix TLDs (`.lodo`), either test OAuth on `localhost` or give
the project a custom public-suffix domain (`lodo dev --name shop.org`).

## Troubleshooting

Run `lodo doctor` first — every failing check prints the exact command to fix it.

- **`can't assign requested address` / `EADDRNOTAVAIL`:** Loopback aliases are missing (check 3); run `lodo setup`.
- **`sudo: a password is required`:** Sudoers rule missing or script updated (check 4); run `lodo setup`.
- **`dig shop.lodo` returns nothing:** Expected — `dig` bypasses `/etc/resolver`. Test with
  `dscacheutil -q host -a name shop.lodo` or `dig @127.0.0.1 -p 53535 shop.lodo`.
- **`502 Bad Gateway`:** Caddy is running, but the app isn't listening on its `127.0.1.x:<port>` address. Start Node
  apps with `lodo dev` and bind Docker ports to `${DOCKER_HOST_IP:-127.0.0.1}`.
- **VPN / Cloudflare WARP:** Split-tunnel or DNS-capturing VPNs can bypass `/etc/resolver`.

## Uninstall

`lodo uninstall` removes `lodo`'s `/etc/resolver` files, stops `dnsmasq`, `caddy`, and `io.lodo.menubar`, restores
`dnsmasq.conf.before-lodo` and `Caddyfile.before-lodo`, untrusts `~/.config/lodo/caddy-root.crt`, removes `127.0.1.1`–
`127.0.1.50` from `lo0`, and deletes the loopback plist, sudoers rule, and resolver script. `~/.config/lodo` is kept.
