# Plan: `oo`, a terminal UI for dnsmasq

## Goal

`oo` is a small terminal app that manages local `.oo` names on macOS through dnsmasq. Each project gets its
own loopback address (`127.0.1.x`), so many projects run at once on their standard ports, and each is
reached by name:

```text
flowy.oo -> 127.0.1.3 -> 127.0.1.3:5432 (flowy's Postgres), 127.0.1.3:3000 (its Next app)
crm.oo   -> 127.0.1.1 -> 127.0.1.1:5432 (crm's Postgres)
```

A project may have subdomains, like `test.crm.oo`. Each is a row of its own: it shares the project's address
by default, or gets its own when it needs its own ports. Subdomains that aren't listed resolve to the parent's
address anyway, because dnsmasq's `address=` lines and the `/etc/resolver` files both match by suffix.

A row may also carry a port. Then Caddy answers `http://dashboard.crm.oo` on port 80 and forwards it to
`127.0.1.1:3000`, so a monorepo's apps get port-less URLs while every service keeps its own port.

Admin rights are needed once, for `oo setup`. Adding, editing and removing names never asks for a password.

## Decisions

Settled 2026-10-08. The steps below follow them.

- **Root hop:** one root-owned script, `apply-resolvers.sh`, run through `sudo -n` under a `NOPASSWD` rule in
  `/etc/sudoers.d/oo`. It runs synchronously: oo sees its exit code and output, and the resolve check runs
  after the cache flush. No launchd job watches files; `launchd.plist(5)` calls `WatchPaths` "highly
  race-prone".
- **Address:** one form field, prefilled with the lowest free `127.0.1.x`. You may overwrite it (`127.0.1.3`
  to match flowy's `.env.example`, or `127.0.0.1` to share). "Own" is derived: the address lies in
  `127.0.1.1`–`127.0.1.50`. An own-block address belongs to one project; other addresses may be shared.
- **Subdomains:** a subdomain is a normal row; the hierarchy is derived from the name, nothing is nested in
  `domains.json`. The form prefills a subdomain with its parent's address. A project is the last two labels
  (`crm.oo`), and any name in a project may share its own-block address. Editing or deleting a parent leaves
  its subdomains as they are.
- **Ports:** a row has an optional port. With one, oo writes a Caddy site
  `http://<name> { reverse_proxy <address>:<port> }` and restarts Caddy as the user. HTTP only; HTTPS stays out.
  Caddy is needed only when a row has a port. Apps listen on their project's address.
- **Stack:** Charm v2 modules under their `charm.land` import paths.
- **Repo:** module `github.com/sangdth/oo`; `master` holds the initial commit, work happens on `sang-dev`.
- **localdns leftovers:** `setup` replaces the old `conf-file` line, starts with an empty `domains.json`, and
  prints the other leftovers for manual cleanup. No migration code.
- **Hand test:** Phase 2 runs on this Mac right after Phase 1, with Sang at the keyboard for the password.
- **Uninstall:** restores the conf backup and leaves dnsmasq stopped; prints the command that brings the old
  root job back.
- **flowy README:** the pointer edit is the last step, its own commit on `sang-dev` in `opscom/flowy`.

### Settled while building

- **Branch:** `master` already held Sang's first commit, so Phase 0 is committed on `sang-dev`.
- **Order:** rows sort by their labels read right to left: `crm.oo`, `api.crm.oo`, `a.api.crm.oo`,
  `test.crm.oo`, `flowy.oo`. A parent comes right before its subdomains, at any depth.
- **Port 80 is refused:** Caddy listens there, so a route to it would loop back into Caddy.
- **Probing is separate from applying:** `system.Apply` changes the system; `check.Env.Probe` checks the names.
  `oo apply` and the TUI call both. `check` reads `system`'s templates, so `system` can't import `check`.
- **Setup asks for the password first,** before it changes anything, so a cancelled prompt leaves no trace.
- **A root dnsmasq job is stopped with `launchctl bootout` and its plist removed,** not with
  `sudo brew services stop`, so Homebrew never runs as root.
- **`bind-dynamic` is dropped from Homebrew's dnsmasq.conf too:** it conflicts with oo's `bind-interfaces`.
- **The sudoers rule allows the script with no arguments only** (`""` after the path), and the script refuses a
  symlinked list and never prints a line it read from the list.
- **The HTTP probe reads Caddy's headers** (checked against Caddy 2.11): `Via: 1.0 Caddy` means the app answered;
  `502` with `Server: Caddy` means the app is down; any other `Server: Caddy` answer means Caddy has no site for
  the name.
- **Charm modules are added in Phase 3,** where the TUI first imports them; `go mod tidy` drops them earlier.
- **Names end in `.oo`** (`store.TLD`). The hand test showed macOS 27 sends a name with one label before
  `.local`, like `flowy.local`, to Bonjour only and ignores its resolver file (`docs/setup-log.md`). `.dev` was
  ruled out: browsers force HTTPS on all of it (HSTS preload) and `flowy.dev` is a registered domain. `.oo` is in
  no public zone today, and two-letter top-level domains are kept for country codes. The tool and its
  names were renamed from `lcd` to `oo` on 2026-10-08.
- **The TUI has one `Backend` interface** (load, save, apply, report) instead of separate applier and checker
  interfaces; the real one wraps `store`, `system` and `check`.
- **`oo` refuses to open while checks 1–5 fail,** using `check.Env.Prerequisites`, which probes nothing. The
  TUI then runs `check.Env.Report` in the background: one probe feeds the status bar and every row.
- **The table has its own key map:** the default binds `space` and `d` to paging.
- **TUI snapshots golden `View().Content`,** not teatest's byte stream, which holds spinner frames and timing;
  one teatest test drives the real program loop.
- **The form refuses a port until Caddy is ready** (installed, and Homebrew's Caddyfile imports oo's), checked
  only when the port is new or changed. The form's cursor doesn't blink, so it starts no timers.
- **Golden files** use `github.com/charmbracelet/x/exp/golden`: `testdata/<TestName>.golden`, `-update` per package.
- **`apply` refuses before setup:** until the script is installed and Homebrew's dnsmasq.conf includes oo's,
  `system.Apply` returns `ErrNotSetUp` and changes nothing. Without it, `oo apply` before setup started a user
  dnsmasq against the old config.
- **Doctor names hand-made resolver files:** an `/etc/resolver/<name>` without oo's marker gets `sudo rm` as its
  fix, because the script never replaces it. The old flowy README recipe made exactly such files.
- **Probes name `/etc/hosts` conflicts:** macOS and dnsmasq answer from `/etc/hosts` first, so an entry with another
  address is reported as the cause of a failed lookup.

## Stack

- Go 1.27, one binary, module `github.com/sangdth/oo`.
- `charm.land/bubbletea/v2` v2.0.10 (app loop), `charm.land/bubbles/v2` v2.2.1 (`table`, `textinput`, `spinner`,
  `viewport`, `key`), `charm.land/lipgloss/v2` v2.0.6 (styling).
- Tests only: `github.com/charmbracelet/x/exp/teatest/v2` with its `x/exp/golden` helper.
- `go.yaml.in/yaml/v3` v3.0.5 reads compose files. No other dependencies. Direct DNS checks use `net.Resolver`
  dialing `127.0.0.1:53535`. `brew`, `sudo`, `launchctl`, `dscacheutil` and `git` run through `os/exec` with
  absolute paths.
- dnsmasq 2.93 from Homebrew (`/opt/homebrew`, Apple Silicon).
- Caddy 2.11 from Homebrew, optional: only rows with a port need it. It binds port 80 as the user; macOS allows
  that without root, and localdns's Caddy did it on this Mac.

Bubble Tea v2 facts the TUI code relies on (checked against the module docs): `Model` is `Init() Cmd`,
`Update(Msg) (Model, Cmd)`, `View() View`; `tea.NewView(string)` builds the view; keys arrive as
`tea.KeyPressMsg` with `.String()` giving `"a"`, `"ctrl+c"`, `"enter"`; `tea.WindowSizeMsg`, `tea.Tick`.

## Commands

| Command         | What it does                                                                                  |
| --------------- | --------------------------------------------------------------------------------------------- |
| `oo`           | opens the TUI; refuses when doctor checks 1–5 fail, and prints them                           |
| `oo setup`     | one-time system setup; asks for the admin password in the terminal; safe to run again         |
| `oo apply`     | regenerates the files from `domains.json`, restarts dnsmasq (and Caddy when needed), writes   |
|                 | resolver files, checks                                                                        |
| `oo doctor`    | runs the eight checks, prints what's wrong and the fix; exit 1 if any fail                    |
| `oo uninstall` | removes everything `setup` installed; keeps `domains.json`                                    |
| `oo version`   | prints the version set at build time (`-ldflags "-X main.Version=..."`)                       |

`apply` is the code path the TUI runs after every change and on `r`. It exists as a command so the Phase 2 hand
test and scripts can use it.

## How it works

### Files

| Path                                                  | Owner | Written by                               |
| ----------------------------------------------------- | ----- | ---------------------------------------- |
| `~/.config/oo/domains.json`                          | user  | the TUI (only source of truth)           |
| `~/.config/oo/dnsmasq.conf`                          | user  | `apply`, on every change                 |
| `~/.config/oo/resolvers`                             | user  | `apply`, on every change                 |
| `~/.config/oo/Caddyfile`                             | user  | `apply`, on every change                 |
| `~/.config/oo/dnsmasq.log`                           | user  | dnsmasq                                  |
| `/opt/homebrew/etc/dnsmasq.conf`                      | user  | `oo setup` (user-owned, no sudo)        |
| `/opt/homebrew/etc/dnsmasq.conf.before-oo`           | user  | `oo setup`, first run only              |
| `/opt/homebrew/etc/Caddyfile`                         | user  | `oo setup` (user-owned dir, no sudo)    |
| `/opt/homebrew/etc/Caddyfile.before-oo`              | user  | `oo setup`, first run, if one existed   |
| `/Library/LaunchDaemons/io.oo.loopback.plist`        | root  | `oo setup`                              |
| `/Library/Application Support/oo/apply-resolvers.sh` | root  | `oo setup`                              |
| `/etc/sudoers.d/oo`                                  | root  | `oo setup`                              |
| `/etc/resolver/<domain>`                              | root  | `apply-resolvers.sh`                     |

### dnsmasq runs as the user on port 53535

```conf
# /opt/homebrew/etc/dnsmasq.conf (oo's block; everything else in the file stays)
# oo
conf-file=/Users/<user>/.config/oo/dnsmasq.conf
listen-address=127.0.0.1
port=53535
bind-interfaces
```

A port above 1024 needs no root, so `brew services restart dnsmasq` runs as the user on every change. The job
must live in the user's launchd domain (`~/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist`). A job started
with `sudo brew services start` lives in the system domain, runs as `nobody`, and is invisible to the user's
`brew services`; `setup` stops it first.

The generated file holds `log-queries`, `log-facility=/Users/<user>/.config/oo/dnsmasq.log`, and one
`address=/<name>/<ip>` line per enabled domain, in the list's order. Only names with a resolver file
reach it.

### One resolver file per domain, written by one root script

macOS sends a name to dnsmasq only when a file in `/etc/resolver/` matches it:

```text
# /etc/resolver/flowy.oo
# oo
nameserver 127.0.0.1
port 53535
```

```text
# /etc/sudoers.d/oo
<user> ALL=(root) NOPASSWD: /Library/Application\ Support/oo/apply-resolvers.sh
```

`apply` runs `sudo -n "/Library/Application Support/oo/apply-resolvers.sh"`. The script (`/bin/sh`, `set -eu`,
fixed `PATH`, absolute tool paths, user paths baked in at setup):

1. reads `~/.config/oo/resolvers`, one name per line;
2. keeps only lines matching the name rule below. The regex is one Go constant, rendered into the script
   template, so the two can't drift;
3. writes `/etc/resolver/<name>` (temp file, then `mv`) with the three fixed lines above;
4. removes `/etc/resolver/*` files whose first line is `# oo` and whose name is no longer listed. It never
   touches a file it didn't write;
5. flushes the cache (`dscacheutil -flushcache; killall -HUP mDNSResponder`) when running as root, and skips
   the flush otherwise, so tests run it unprivileged.

It never runs user-writable code and never writes user content, only file names that pass the check.

### The loopback address block

`io.oo.loopback` runs `ifconfig lo0 alias 127.0.1.$i up` for `i` = 1–50 at every boot (`RunAtLoad`). macOS
only has `127.0.0.1` by default.

### Ports go through Caddy

A row with a port gets a Caddy site. Caddy listens on port 80 as the user and forwards by name:

```text
# ~/.config/oo/Caddyfile
# Generated by oo from domains.json. Edits are overwritten.
http://dashboard.crm.oo {
	reverse_proxy 127.0.1.1:3000
}
```

```text
# /opt/homebrew/etc/Caddyfile, the file Homebrew's service loads
# oo
import /Users/<user>/.config/oo/Caddyfile
```

The `http://` prefix keeps Caddy on port 80 with no automatic HTTPS (`caddy adapt` shows one server on `:80`
and no TLS app). The upstream is the row's address and port, so apps listen on their project's address:
`next dev -H 127.0.1.1`, Vite `--host 127.0.1.1`, Docker ports on `DOCKER_HOST_IP`. An app on `*:3000` answers
on every loopback address and shadows other projects' port 3000. A file with no sites is a valid config, so
Caddy idles when no row has a port, and rows without a port never need it.

### What happens when a domain changes (`apply`)

1. Save `domains.json` (written to a temp file, then renamed into place).
2. Regenerate `dnsmasq.conf`, `resolvers` and `Caddyfile`.
3. `/opt/homebrew/bin/brew services restart dnsmasq`.
4. `sudo -n` the resolver script. It writes `/etc/resolver/` and flushes the cache.
5. When the `Caddyfile` changed and Caddy is set up: `caddy validate`, then `brew services restart caddy`.
6. Per enabled domain: query dnsmasq on `127.0.0.1:53535`, then resolve through macOS
   (`dscacheutil -q host -a name <name>`); with a port, `GET http://<address>/` with `Host: <name>`. Any HTTP
   status means Caddy routes the name; 502 means nothing listens on the upstream. Each row shows ✓ or ✗ with
   the reason.

A failure at any step shows in the status line. The saved file stays as written; `r` runs `apply` again.

## The TUI

```text
 oo   caddy ●   dnsmasq ●   loopback ●   resolvers ●
 ─────────────────────────────────────────────────────────────────────────────
  ● crm.oo                127.0.1.1          own    dns ✓
    ● dashboard.crm.oo    127.0.1.1  :3000   own    dns ✓  http ✓
    ● service.crm.oo      127.0.1.1  :3002   own    dns ✓  http ✗ 502, app down
  ● flowy.oo              127.0.1.3          own    dns ✓
  ○ old.oo                127.0.0.1                 –
    Add new domain
 ─────────────────────────────────────────────────────────────────────────────
 a sub  e edit  d del  space on/off  c env  g log  r apply  tab top  q quit
```

```text
 Add domain
 Name     dashboard.crm .oo
 Address  127.0.1.1          crm.oo's address; next free: 127.0.1.4
 Port     3000               optional; http://dashboard.crm.oo then reaches 127.0.1.1:3000
 enter save   esc cancel
```

- **List:** a `bubbles/table` with name, address, port, own, on/off and the checks (dns, http). Up to 50
  rows, no filtering. A subdomain's mark is indented with its name. The last row, `Add new domain`, is dim and
  has no mark; `enter`, `space` or `a` on it opens the add form, and the keys line there lists only what works.
  `A` opens the add form from anywhere; `a` on a name opens it for a subdomain of that name.
- **Form:** four `textinput`s: name, address, port and compose. The name field takes the labels only; a dimmed
  suffix that can't be edited follows it: `.oo`, or `.demo.oo` for a subdomain of `demo.oo`. A suffix typed
  anyway is not doubled. Edit locks a subdomain's parent the same way; moving it to another parent means delete
  and add. Add prefills the address while you type the name: the parent's address when the name is a subdomain of
  a listed name, else the lowest free own address (`127.0.0.1`, with a note, when all 50 are taken). The hint
  names the next free own address so a subdomain can get its own. Typing in the address field stops the prefill.
  Port is optional; a port when `caddy` isn't installed says `brew install caddy`, then `oo setup`. Edit prefills
  the stored values. Validation errors show under the field.
- **Spinner:** while a change runs, the changed row's mark spins, whether the name is on or off; the first
  check and `r` spin every enabled row. A name being added is listed at once with the spinner, and goes away
  if the save fails.
- **Delete:** `d` asks `delete flowy.oo? y/N` in the status line; only `y` deletes.
- **Link** (`c`): the name gets the compose file of the project oo started in, or keeps its own outside one, and
  `DOCKER_HOST_IP=<address>` goes into the `.env` that file runs with: the one a `package.json` script passes
  with `--env-file`, else the project root's (`compose.EnvFile`, `compose.SetEnv`). A `.env` git tracks is
  refused; the compose path is still saved. The question's `y`, `c` in the preview, and a form save that changes
  a linked name's compose file or address link too. Linking saves without an apply.
- **Log:** a `viewport` tailing `dnsmasq.log`, polled every 500 ms; `g` or `esc` returns.
- **Compose:** started in a project, oo asks once, after the first check, about the best compose file for the
  name the project's folder suggests: `y` links it, `e` edits it first, anything else saves `none`; an unlisted
  name gets the add form. A compose path is saved without an apply: it changes no generated file. `p` previews
  the file rewritten by `compose.Rewrite` in a `viewport`, the changed lines marked, with the `.env` line; `c`
  links. oo writes one project file: the linked `.env`, and only its `DOCKER_HOST_IP` lines.
- **Status bar:** checks 8, 1, 3 and 4, in that order. A green `●` is on and works, a dim `○` is off: turned off,
  or Caddy not running while no row has a port. A red `○` fails. Any red one says "run `oo doctor`".
- **Services:** `tab` moves the keys to the status bar; `←` `→` (or `h` `l`) pick Caddy or dnsmasq, `space` turns
  it on or off, with the spinner on its mark. Loopback and resolvers need root, so they only show their state.
  Off is brew's own state: `brew services stop` unregisters the job, so `Apply` leaves an unregistered service
  stopped, while one that crashed stays registered and is restarted. A dnsmasq turned off is check 1 skipped, so
  the TUI still opens to turn it back on. The status line leaves out failures a service turned off explains:
  every name with dnsmasq off, the http probe with Caddy off. Only one place is highlighted at a time: the picked
  service, or the selected row.
- **Layout:** everything sits in one bordered box at the middle of the terminal: 70% of the width, at least
  84 columns (the whole width on a narrower terminal). Its height fits the names, up to 80% of the
  terminal; the log takes the full 80%.
- **Cursor during a change:** a change runs in the background and the cursor moves freely. When it lands,
  the cursor goes to what the change is about (the new name, a deleted name's neighbor) only if it did not
  move; otherwise it stays on the name it moved to.
- Every change runs `apply` as a `tea.Cmd`, with a spinner while dnsmasq restarts. Errors show in the status
  line and never exit the app.

## Rules

- Names: lowercase labels of `[a-z0-9-]`, 1–63 chars, no leading or trailing `-`, ending in `.oo`, and not
  `oo` itself. Regex:
  `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*\.oo$`
- Names are unique.
- A project is the last two labels of a name: `crm.oo`, `test.crm.oo` and `api.crm.oo` are one
  project. Rows sort by their labels read right to left, so a parent comes right before its subdomains.
- Addresses: IPv4 inside `127.0.0.0/8`. `127.0.1.1`–`127.0.1.50` is the own block. An own-block address belongs
  to one project; any name in that project may share it. Every other address may be shared by anyone.
- Subdomains that aren't listed resolve to the parent's address anyway. A row for a subdomain gives it another
  address, or a check mark and log lines of its own.
- Port: optional, 1–65535 except 80, which is Caddy's own. Rows may share a port; Caddy routes by name. A port
  needs Caddy installed and set up.
- Turning a subdomain off removes its own lines only. While its parent is on, the parent's lines still answer
  for it.
- Editing or deleting a row never changes other rows: moving `crm.oo` leaves `test.crm.oo` where it is.
- Deleting a domain frees its address.
- `oo` refuses to start the TUI when checks 1–5 fail, and prints them.

## `oo doctor` checks

| #   | Check                                                                              | Fix it prints        |
| --- | ---------------------------------------------------------------------------------- | -------------------- |
| 1   | dnsmasq installed; `brew services info dnsmasq --json` says running, as this user; | `oo setup`          |
|     | no system job under `homebrew.mxcl.dnsmasq` or `sh.brew.dnsmasq`                   |                      |
| 2   | system conf has oo's block, exactly one `conf-file=` line, pointing at oo's file | `oo setup`          |
| 3   | `system/io.oo.loopback` is loaded and `127.0.1.1` is on `lo0` (count reported)    | `oo setup`          |
| 4   | script is root-owned, mode 755, content current; `sudo -n -l <script>` succeeds;   | `oo setup`, then    |
|     | every enabled domain has its `/etc/resolver` file with the marker and port         | `oo apply`          |
| 5   | no `/etc/resolver/local` (it takes every `.local` name away from Bonjour)          | `sudo rm` it         |
| 6   | `dnsmasq.conf`, `resolvers` and `Caddyfile` equal what `domains.json` generates    | `oo apply`          |
| 7   | each enabled domain resolves, from dnsmasq directly and through macOS; with a      | `oo apply`, `g`     |
|     | port, Caddy answers for it                                                         |                      |
| 8   | only when an enabled row has a port: Caddy installed; `brew services info caddy    | `brew install        |
|     | --json` running; Homebrew's `Caddyfile` holds oo's import line;                   | caddy`, `oo setup`  |
|     | `caddy validate` passes on oo's file; port 80 belongs to Caddy, else `lsof -nP    |                      |
|     | -iTCP:80 -sTCP:LISTEN` names the listener                                          |                      |

With dnsmasq turned off, check 7 prints "turn dnsmasq on" instead of `oo apply`, which leaves it off.

"Content current" compares the installed script with the one this build of oo renders, so an upgraded oo
asks for `oo setup` again. `oo` starts the TUI when checks 1–5 pass; a failing check 8 only turns the status
bar red.

## This Mac today, and what `setup` changes

| Found                                                                    | `setup` does                 |
| ------------------------------------------------------------------------ | ---------------------------- |
| dnsmasq runs as `nobody` from a root job in `/Library/LaunchDaemons`     | bootout, remove its plist    |
| system conf: `conf-file=~/.config/localdns/dnsmasq.conf`                 | backup, rewrite              |
| `/etc/resolver/` empty; `lo0` has only `127.0.0.1`                       | install script, plist, rule  |
| Caddy 2.11.6 installed, not running; no `/opt/homebrew/etc/Caddyfile`    | write it, start Caddy        |
| `~/.config/localdns/`, the caddy LaunchAgent, LocalDNS `domains.json`    | print, leave alone           |
| `/etc/sudoers.d/` exists and is empty                                    | add `oo`                    |
| Cloudflare WARP and NordVPN helpers installed                            | nothing (see README)         |

`setup` is safe to run twice: the backup is written only when missing, every install overwrites, and
`launchctl bootout` before `bootstrap` ignores "not loaded".

## Layout

```text
cmd/oo/main.go         subcommand dispatch, exit codes, Version
internal/paths/         Paths struct: every file and tool path; Default() and ForTest(root)
internal/run/           Runner interface over os/exec; Exec (real) and Fake (records calls, canned output)
internal/fsutil/        atomic writes (temp file, then rename) that skip unchanged content
internal/store/         domains.json load/save, name, address and port rules, own-block allocation
internal/brew/          brew services restart and status (--json) for dnsmasq and caddy
internal/dnsmasq/       dnsmasq.conf and resolver-list generation, log tail
internal/caddy/         Caddyfile generation and caddy validate
internal/system/        setup, apply, uninstall; templates for the script, plist, sudoers, conf blocks
internal/check/         the eight doctor checks and the per-domain dns, macOS and http probes
internal/compose/       find a project's compose file; rewrite its ports and localhost URLs for the preview
internal/tui/           Bubble Tea model, views, key map
```

Every package takes a `paths.Paths` and a `run.Runner`, so tests point all paths at a temp directory and all
commands at the fake. Only `cmd/oo` builds the real ones.

## Steps

Each step ends with `go test -race ./...`, `go vet ./...` and `gofmt -l .` clean, then one conventional commit.

### Phase 0: repo

| Step | Work                                                                                             |
| ---- | ------------------------------------------------------------------------------------------------ |
| 0.1  | `git init`; `.gitignore` (`/oo`, `.DS_Store`); `go mod init github.com/sangdth/oo`; `go 1.27`   |
| 0.2  | the three `charm.land` modules and `teatest/v2` come in Phase 3, where they are first imported     |
| 0.3  | `CLAUDE.md` (short): commands, layout, "no test touches the system", `charm.land` import paths     |
| 0.4  | `git switch -c sang-dev`; commit `chore: init module and plan` there                              |

### Phase 1: core, no system changes

| Step | Package            | Work                                                                                   |
| ---- | ------------------ | -------------------------------------------------------------------------------------- |
| 1.1  | `internal/paths`   | `Paths{Home, User, ConfigDir, DomainsJSON, DnsmasqConf, Resolvers, Caddyfile, Log,     |
|      |                    | SystemConf, SystemConfBackup, SystemCaddyfile, SystemCaddyfileBackup, LoopbackPlist,   |
|      |                    | Script, Sudoers, ResolverDir, Brew, Dnsmasq, Caddy, Sudo, Launchctl, Dscacheutil,      |
|      |                    | Lsof, Pbcopy}`; `Default()`; `ForTest(root)` puts every path under `root` and every    |
|      |                    | tool at `root/bin/<name>`                                                              |
| 1.2  | `internal/run`     | `Runner` with `Run(ctx, name, args...) (string, error)`; `Exec` (10 s timeout, stderr  |
|      |                    | folded into the error); `Fake` (map of `name args` to output or error, records calls)  |
| 1.3  | `internal/store`   | `Domain{Name, Address, Port, Enabled}` (`port` left out of the JSON when 0),           |
|      |                    | `File{Version, Domains}`; `Load` (missing file is empty, bad JSON is an error); `Save` |
|      |                    | (mkdir, temp + rename, 0644, sorted); `Sort` (project, label count, name);             |
|      |                    | `ValidateName`, `ValidateAddress`, `ValidatePort`, `IsOwn`, `Project`, `Parent`        |
|      |                    | (longest listed suffix), `NextFree` (`ErrBlockFull`); pure `Add`, `Update`, `Remove`,  |
|      |                    | `Toggle` that return a new slice and enforce the uniqueness rules                      |
| 1.4  | `internal/brew`    | `Restart` and `Stop(ctx, r, brew, service)`; `Info(ctx, r, brew, service)` returns     |
|      |                    | `Status{Running, Loaded, User, PID}` from `brew services info <service> --json`        |
| 1.5  | `internal/dnsmasq` | `Config(domains, logPath)`, `ResolverList(domains)`; `Tail(path, offset)` that         |
|      |                    | restarts from 0 when the file shrank                                                   |
| 1.6  | `internal/caddy`   | `Config(domains)`: one `http://<name> { reverse_proxy <address>:<port> }` site per     |
|      |                    | enabled row with a port, sorted, or the header alone; `Validate(ctx, r, caddyBin,`     |
|      |                    | `path)` runs `caddy validate --config <path> --adapter caddyfile` and returns the      |
|      |                    | `msg` of caddy's last JSON error line                                                  |
| 1.7  | `internal/system`  | templates (`embed`): `apply-resolvers.sh`, `io.oo.loopback.plist`, `sudoers`, the     |
|      |                    | dnsmasq conf block, the Caddyfile import block; `Render(paths)` for each;              |
|      |                    | `RewriteSystemConf(old string, paths) string` that drops old `conf-file=`,             |
|      |                    | `listen-address=`, `port=`, `bind-interfaces` and oo blocks, keeps the rest, appends  |
|      |                    | oo's block; `WriteFiles(paths, domains)` writes the three generated files (temp +     |
|      |                    | rename)                                                                                |
| 1.8  | `cmd/oo`          | dispatch with `os.Args`; `version`; unknown command prints usage, exit 2               |

Tests for Phase 1:

- `store`: names accept `flowy.oo`, `a-b.dev.oo`; reject `oo`, `.oo`, `X.OO`, `x.com`, `flowy.local`,
  `../x.oo`, `x.oo\nfoo`, `-x.oo`, a 64-char label. Addresses accept `127.0.0.1`, `127.0.1.3`;
  reject `10.0.0.1`, `::1`, `127.0.1`, `abc`. Ports accept empty (none), `1`, `3000`, `65535`; reject `0`,
  `70000`, `abc`. `NextFree`: empty gives `.1`; `.1,.2` gives `.3`; a gap gives the
  gap; 50 taken gives `ErrBlockFull`. `Add` rejects a duplicate name and an own address held by another
  project; it allows `test.crm.oo` at `crm.oo`'s address, at its own free address, and two `127.0.0.1`.
  `Parent` of `a.test.crm.oo` is `test.crm.oo` when it and `crm.oo` are listed, and none when neither
  is. `Sort` puts `crm.oo` before `test.crm.oo` before `flowy.oo`. `Remove` frees the address. `Save`
  then `Load` round-trips and leaves no temp file.
- `dnsmasq`: golden files `testdata/dnsmasq.conf.golden` and `testdata/resolvers.golden` (disabled domains
  left out, sorted, one subdomain sharing its parent's address and one with its own; dnsmasq answers from the
  longest matching `address=` line, so a subdomain's own line wins). `Tail` handles append and truncation.
- `brew`: `Status` parses the dnsmasq and caddy JSON captured from this Mac; `Restart` runs
  `brew services restart <service>` and returns stderr on failure.
- `caddy`: golden `testdata/Caddyfile.golden` with two sites, and the comment-only file when no row has a
  port; a disabled row with a port is left out; `Validate` passes the oo file path and the adapter flag.
- `system`: golden files for the rendered script, plist, sudoers and conf block with fixed fake paths.
  `RewriteSystemConf` with this Mac's current conf, with Homebrew's all-comment default, and with oo's own
  block (unchanged). The script test renders it with a temp resolver dir and input file, runs `/bin/sh`, and
  asserts: files written for good names with the marker; bad names skipped; a marker file no longer listed is
  removed; a foreign file is untouched; exit 0; missing input exits non-zero.

Commit: `feat: domain store, generated configs, caddyfile and resolver script`.

### Phase 2: setup, apply, doctor, uninstall

- **2.1 `internal/check`:** `Env{Paths, Runner, DNS, HTTPPort, RootUID}`; `Env.Run(ctx, domains) []Check`
  runs the eight checks; `Env.Probe(ctx, domains) []Result` probes every enabled domain concurrently. The
  direct query uses `net.Resolver{PreferGo: true}` with a `Dial` to `127.0.0.1:53535`; the http probe dials
  `<address>:80` for `GET http://<name>/` and reads Caddy's `Via` and `Server` headers.
- **2.2 `internal/system.Apply(ctx, paths, r, domains) error`:** write the three files, restart dnsmasq, run
  the script through `sudo -n`, and when oo's import line is in place and the Caddyfile changed or Caddy
  stopped: validate, then restart Caddy. `oo apply` and the TUI probe afterward with `check.Env.Probe`.
- **2.3 `internal/system.Setup(ctx, paths, r, out)`**, one `✓`/`✗` line per step, stop at the first failure
  with the fix:
  1. preflight: dnsmasq binary, system conf file, `/opt/homebrew` prefix;
  2. `sudo -v`, the one password prompt, before anything changes;
  3. user files: `mkdir`, empty `domains.json` when missing, the generated files;
  4. system conf: backup once, rewrite with `RewriteSystemConf`;
  5. stop a root dnsmasq job (`homebrew.mxcl.dnsmasq` or `sh.brew.dnsmasq`) with `launchctl bootout` and
     remove its plist from `/Library/LaunchDaemons`;
  6. script: `sudo install -o root -g wheel -m 755` from a temp file;
  7. sudoers: `sudo visudo -cf` on the temp file, then `install -m 440`;
  8. loopback plist: `install -m 644`, `launchctl bootout` (ignore "not loaded"), then `bootstrap`;
  9. remove `/etc/resolver/local` when present;
  10. `brew services restart dnsmasq` as the user;
  11. Caddy, when installed: back up `/opt/homebrew/etc/Caddyfile` once if present, write oo's import block,
      `brew services restart caddy`; when not installed, print the optional `brew install caddy` hint;
  12. `sudo -n` the script, which proves the rule works without a password;
  13. print the localdns leftovers it saw, then the doctor table.
- **2.4 `internal/system.Uninstall(ctx, paths, r, out)`:** write an empty resolver list and `sudo -n` the
  script (removes oo's resolver files, flushes); `brew services stop dnsmasq`; restore the backup conf, or
  strip oo's block when there is none; `brew services stop caddy`; restore `Caddyfile.before-oo`, or remove
  oo's `/opt/homebrew/etc/Caddyfile` when none existed; `sudo`: `bootout` the loopback job, remove its plist,
  `ifconfig lo0 -alias 127.0.1.$i` for 1–50, remove sudoers and the script. Keep `~/.config/oo`. Print the
  `sudo brew services start dnsmasq` hint.
- **2.5 `cmd/oo`:** wire `setup`, `apply`, `doctor` (table, exit 1 on any failure) and `uninstall`.
- **2.6 Hand test on this Mac, Sang present:** `go run ./cmd/oo setup`; `doctor`; add `flowy.oo` and
  `test.flowy.oo`, both `127.0.1.3`, to `domains.json` by hand; `apply`; `time dscacheutil -q host -a name`
  for `flowy.oo`, `test.flowy.oo` and the unlisted `foo.flowy.oo` (all `127.0.1.3`, which proves suffix
  matching on both sides); `ifconfig lo0 | grep 127.0.1`. Then ports: add `dashboard.flowy.oo`,
  `127.0.1.3`, port 3000; `apply`; in another terminal `python3 -m http.server 3000 --bind 127.0.1.3`;
  `curl -s http://dashboard.flowy.oo/ | head -3` shows the listing; stop the server, `apply` reports
  `http ✗ 502`. Then `uninstall`; `doctor` (expect failures); `setup` again. Record the output in
  `docs/setup-log.md`.

Tests for Phase 2 use the fake runner and temp paths only:

- `check`: each check both ways with canned command output; the direct resolve against an in-test UDP server
  that answers one A record (packet built by hand, no DNS library).
- `check`: the http check against an in-test `httptest` server: 200 passes, 502 reports "app down", a closed
  port reports "caddy not answering".
- `system`: the exact command sequence of `Setup` on a fresh Mac, with a system job present, with and without
  Caddy installed, and on a second run (no second backup); `Uninstall`'s sequence and that `domains.json`
  survives; `Apply` writes the three files before any command, restarts Caddy only when the Caddyfile changed,
  and stops at the first failing command.

Commit: `feat: setup, apply, doctor and uninstall commands`.

### Phase 3: TUI list and status

| Step | Work                                                                                                 |
| ---- | ---------------------------------------------------------------------------------------------------- |
| 3.1  | `tui.Model` with modes `list`, `form`, `confirm`, `log`; fields: domains, checks, results, table,     |
|      | spinner, status text, busy flag, size                                                                |
| 3.2  | `Applier` interface (`Apply(ctx, domains)`) and `Checker` (`Run(ctx)`); real ones wrap `system` and  |
|      | `check`; fakes in tests                                                                              |
| 3.3  | `Init`: load domains, run checks and resolve as one `tea.Cmd`; `Update`: `q`/`ctrl+c`, `r` (apply), |
|      | `space` (toggle then apply), `j`/`k`/arrows via the table; messages `appliedMsg`, `checksMsg`,      |
|      | `errMsg`; spinner while busy, other keys ignored while busy                                          |
| 3.4  | `View`: status bar (checks 1, 3, 4 and 8), table with subdomains indented by label count and the dns |
|      | and http marks, key help; colors through one `styles` struct                                         |
| 3.5  | `cmd/oo`: no args runs checks 1–5 first; failures print and exit 1; else `tea.NewProgram` with the |
|      | alt screen                                                                                           |

Tests: `Update` tests with the fakes (toggle calls `Apply` with the flipped domain; an apply error lands in the
status text and keeps the list; keys are ignored while busy). `teatest` golden of the list view at 80×24 with
three domains.

Commit: `feat: tui list and status bar`.

### Phase 4: add, edit, delete, copy env

| Step | Work                                                                                                 |
| ---- | ---------------------------------------------------------------------------------------------------- |
| 4.1  | form: name, address and port `textinput`s, `tab`/`shift+tab`, `enter` validates with `store` rules   |
|      | and uniqueness then applies, `esc` cancels; while the name is typed, add prefills the address from   |
|      | `Parent`, else `NextFree` (with the "all 50 taken" note); `addressTouched` stops the prefill; the    |
|      | hint names the next free own address; a port when the `caddy` binary is missing is refused with      |
|      | `brew install caddy`, then `oo setup`                                                               |
| 4.2  | edit (`e`) prefills the selected row; a changed name keeps the address                               |
| 4.3  | delete (`d`): `confirm` mode, `y` applies the removal, anything else cancels                         |
| 4.4  | copy env (`c`): `pbcopy` through the runner with `DOCKER_HOST_IP=<address>`; status "copied"         |

Tests: form validation paths (bad name, duplicate name, own address held by another project, a subdomain
sharing its parent's address allowed, shared `127.0.0.1` allowed, bad port, port without Caddy refused with
the hint); prefill from the parent, from `NextFree`, and frozen after typing in the address field; the hint's
next free value; delete confirm and cancel; `pbcopy` input seen by the fake. `teatest` golden of the form.

Commit: `feat: add, edit, delete and copy env`.

### Phase 5: log view

| Step | Work                                                                                              |
| ---- | ------------------------------------------------------------------------------------------------- |
| 5.1  | `l` opens a `viewport` over `dnsmasq.log`; `tea.Tick` every 500 ms calls `Tail`, appends, goes to |
|      | the bottom; `l`/`esc` returns; missing log file shows "no log yet"                                |

Tests: `Update` toggles the mode and stops the tick when leaving; `Tail` growth and truncation already covered.

Commit: `feat: dnsmasq log view`.

### Phase 6: docs and repo

| Step | Work                                                                                                   |
| ---- | ------------------------------------------------------------------------------------------------------ |
| 6.1  | `README.md`: install (`go install github.com/sangdth/oo/cmd/oo@latest`), `setup`, keys, `apply`,     |
|      | `doctor`, `uninstall`, how it works (short: project, subdomain and port rules), troubleshooting        |
| 6.2  | `CLAUDE.md` final pass, under 200 lines                                                                |
| 6.3  | `.github/workflows/ci.yml` like randomport's, plus `go test -race ./...`, on `macos-latest`            |
| 6.4  | `opscom/flowy`: branch `sang-dev`; shrink "Local services on their own address" to the address table, |
|      | `oo` install and `oo setup`, "add `flowy.oo` with `127.0.1.3`", and the `.env` note; move          |
|      | `.env.example` hosts from `flowy.local` to `flowy.oo`. Own commit                                    |
|      | `docs: point local network setup at oo`. Search the flowy Linear project for a matching ticket first |

### Phase 7: compose files

| Step | Work                                                                                                       |
| ---- | ---------------------------------------------------------------------------------------------------------- |
| 7.1  | `store`: `Domain.Compose`: an absolute `.yml`/`.yaml` path, `none` after a no, or empty before oo asked    |
| 7.2  | `compose.ProjectRoot`: walk up to the git root, which needs a lock file: `*.lock`, `*.lockb`,              |
|      | `*-lock.json`, `*-lock.yaml` or `go.sum`                                                                   |
| 7.3  | `compose.Find`: `(docker[-.])compose[.variant].y(a)ml` up to 3 folders deep, skipping hidden folders,      |
|      | `node_modules` and `vendor`; dev variants first, then the plain file, the rest, prod last; shallower first |
| 7.4  | `compose.Rewrite` reads the YAML with `go.yaml.in/yaml/v3` and edits the original text: a port with no     |
|      | address or `127.0.0.1` binds `${DOCKER_HOST_IP:-127.0.0.1}`; a `localhost` URL in `environment` takes the  |
|      | project's name, or the name Caddy serves on that port; healthchecks, commands and comments stay            |
| 7.5  | columns: name fits its longest row, then address, port, compose, own, check; the compose path is cut       |
|      | from the left so the file name stays                                                                       |
| 7.6  | form: a `compose` field; `~/` and relative paths resolve from where oo started; the file must exist        |
| 7.7  | startup in a project: the git root's folder names the domain; `use <file> for <name>? y/N`, `e` edits;     |
|      | no saves `none`; a name that isn't listed gets the add form, filled in                                     |
| 7.8  | `p` previews the rewritten file: changed lines marked, the `.env` line for `DOCKER_HOST_IP`                |
| 7.9  | keys on two lines; README, CLAUDE.md layout                                                                |

Tests: project root with and without a lock file; ranking and depth; every rewrite rule on its own, and a
snapshot of a file built from the shapes in `~/Projects` (ports with and without an address, variables,
healthchecks, `${VAR:-http://localhost:3000}` defaults, comments); the prompt's yes, no and edit; the
preview; the form's compose field. oo never writes a project file: the preview only shows the result.

Commits: one per step group.
## Tests

- `go test -race ./...`, table-driven, `t.TempDir()` for every path, `run.Fake` for every command.
- No test changes the system: nothing writes to `/etc`, `/Library`, `/opt/homebrew` or the real `~/.config/oo`,
  and nothing runs `sudo` or starts a service. A few tests run `dnsmasq --test`, `caddy validate`, `visudo -c`
  and `plutil -lint` read-only on generated files, and skip when the tool is missing.
- Golden files live in `testdata/`; `teatest` goldens update with `go test ./internal/tui -update`.
- The resolver script is tested for real with `/bin/sh` on temp paths (its flush step skips when not root).
- `caddy validate` and every `brew` call go through the runner, so tests never need Caddy or Homebrew.

## Risks

- **`.oo` could become a country code.** Two-letter top-level domains are kept for ISO 3166 country codes, so
  a new-domain round can't claim `.oo`; only an ISO assignment of `OO` could. Only the names oo lists would be
  shadowed, because each has its own resolver file.
- **Someone runs `sudo brew services start dnsmasq` again.** The root job comes back and shadows oo's.
  Doctor check 1 names it; `setup` fixes it.
- **A VPN that captures DNS.** Cloudflare WARP and NordVPN helpers are installed here. When one is on, `.oo`
  lookups may skip `/etc/resolver`. Troubleshooting line; nothing oo can do.
- **Bad sudoers file locks `sudo`.** `visudo -cf` runs on the temp file and install happens only when it passes.
- **Docker Desktop binding to `127.0.1.x`.** The flowy compose file proves it after the hand test; oo only
  provides the aliases.
- **Port 80 taken.** Docker publishing `:80` or another proxy keeps Caddy from starting. Doctor check 8 names
  the listener; nothing else oo can do.
- **Dev servers bound to localhost only.** Vite and some tools listen on `127.0.0.1`, so Caddy's upstream
  `127.0.1.1:5173` gets a 502 until `--host 127.0.1.1`. The row shows "app down" and the README says why.

## Troubleshooting (goes in the README)

- `can't assign requested address` from Docker: the loopback job didn't run. Run `oo doctor`.
- `sudo: a password is required` in the TUI: the sudoers rule is missing or the script changed. `oo doctor`
  check 4, then `oo setup`.
- `dig flowy.oo` finds nothing: expected. `dig` and `nslookup` skip `/etc/resolver`; Node, `psql` and
  browsers use it. Test with `dscacheutil -q host -a name flowy.oo`.
- A name isn't found right after adding it: the resolver file is missing, so macOS asked the public DNS servers.
  `oo doctor` check 4.
- `foo.crm.oo` resolves although it isn't listed: expected. dnsmasq's `address=` and the resolver file both
  match subdomains. Add a row only to give it another address or a port.
- `502 Bad Gateway` on `http://dashboard.crm.oo`: Caddy is up but nothing listens on `127.0.1.1:3000`. Start
  the app on its address: `next dev -H 127.0.1.1`, Vite `--host 127.0.1.1`; Docker ports bind `DOCKER_HOST_IP`.
- `http://dashboard.crm.oo` refuses the connection: Caddy isn't running or port 80 is taken. `oo doctor`
  check 8.
- Names resolve for one app but not another: a VPN or WARP is capturing DNS.
- Containers can't reach `flowy.oo`: they don't need to. Inside Docker they use the service name.
- The log view shows no queries: the lookup never reached dnsmasq. Check 4 (resolver file) or the VPN line.

## Out of scope for v1

- HTTPS. Sites are `http://` on port 80; `https://` with mkcert certificates is v2. Rows without a port are
  reached at `http://<name>:<port>`.
- Migrating or cleaning up localdns.
- Intel Homebrew (`/usr/local`) and Linux.
- Non-interactive `oo add` / `oo rm`. `oo apply` on a hand-edited `domains.json` covers scripts for now.
- A Homebrew tap and a release workflow. `go install` until there's a second user.
