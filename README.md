# oo

oo lets several projects run on one Mac at the same time, each database and app on its usual port. Every
project gets its own loopback address, like `127.0.1.3`, and a name that ends in `.oo`, so flowy's Postgres
listens on `127.0.1.3:5432` while crm's listens on `127.0.1.1:5432`. oo is a terminal app: it keeps the list
of names and makes dnsmasq, macOS's `/etc/resolver` files and, for names with a port, Caddy follow it.

```text
flowy.oo -> 127.0.1.3 -> 127.0.1.3:5432 (Postgres), 127.0.1.3:3000 (Next)
crm.oo   -> 127.0.1.1 -> 127.0.1.1:5432 (Postgres)
http://dashboard.crm.oo -> Caddy on port 80 -> 127.0.1.1:3000
```

## Requirements

- macOS on Apple Silicon, with Homebrew in `/opt/homebrew`. Intel Macs, with Homebrew in `/usr/local`, don't work.
- Go 1.27 or newer, to build oo.
- dnsmasq: `brew install dnsmasq`.
- Caddy, only for names with a port: `brew install caddy`.
- An admin account: setup uses `sudo`.

## Install

```sh
go install github.com/sangdth/oo/cmd/oo@latest
```

That puts `oo` in `$(go env GOPATH)/bin`, usually `~/go/bin`, which needs to be on your `PATH`. From a clone,
`go build -o oo ./cmd/oo` builds it in place.

## Setup

Run `oo setup` as yourself, not with `sudo`. It asks for your password once, before it changes anything. Then it:

1. creates `~/.config/oo`, an empty `domains.json` when there is none, and the files oo generates from it;
2. backs up Homebrew's `/opt/homebrew/etc/dnsmasq.conf` to `dnsmasq.conf.before-oo` (first run only), drops its
   `conf-file`, `listen-address`, `port`, `bind-interfaces` and `bind-dynamic` lines, and adds oo's block:
   include `~/.config/oo/dnsmasq.conf` and listen on `127.0.0.1` port 53535. Every other line stays;
3. stops a root dnsmasq job, the kind `sudo brew services start dnsmasq` makes, and removes its plist from
   `/Library/LaunchDaemons`;
4. installs the resolver script, `/Library/Application Support/oo/apply-resolvers.sh`, and the sudoers rule,
   `/etc/sudoers.d/oo`, which lets you run that script, and nothing else, without a password;
5. installs `/Library/LaunchDaemons/io.oo.loopback.plist`, which adds `127.0.1.1`–`127.0.1.50` to `lo0` now
   and at every boot;
6. removes `/etc/resolver/local` if it exists: that file takes every `.local` name away from Bonjour;
7. starts dnsmasq as you, with `brew services`;
8. when Caddy is installed, backs up `/opt/homebrew/etc/Caddyfile` to `Caddyfile.before-oo` if there is one,
   makes it import `~/.config/oo/Caddyfile`, and starts Caddy as you;
9. runs the script through `sudo -n` to prove the rule works, then prints `oo doctor`'s checks.

It prints one line per step and stops at the first failure, with the command that fixes it. Running it again is
safe: it repairs the setup and keeps the first backup. Do so after you install Caddy, and when `oo doctor`
reports the script out of date after an upgrade. After setup, adding, editing and deleting names never asks for
a password, and neither does `oo apply`.

## Using oo

`oo` opens the list of names. It refuses to open while `oo doctor` checks 1–5 fail, and prints them; a dnsmasq
you turned off doesn't count.

```text
 oo   caddy ●   dnsmasq ●   loopback ●   resolvers ●
 ────────────────────────────────────────────────────────────────────────────────────────────
 name                   address          port   compose                          own  check
 ● crm.oo               127.0.1.1               ~/Projects/crm/compose.dev.yaml  own  dns ✓
   ● dashboard.crm.oo   127.0.1.1        3000                                    own  dns ✓  http ✓
 ● flowy.oo             127.0.1.3               –                                own  dns ✓
 ○ old.oo               127.0.0.1                                                     –
   Add new domain
```

The top line shows Caddy, dnsmasq, loopback and resolvers (doctor checks 8, 1, 3 and 4): a green `●` is on and
works, a dim `○` is off (turned off, or Caddy not running while no name has a port), and a red `○` needs
`oo doctor`. `tab` moves the keys to Caddy and dnsmasq: `←` `→` (or `h` `l`) pick one, `space` turns it on or
off, `tab` goes back to the names. A service turned off stays off through every change until you turn it on; with
dnsmasq off no `.oo` name resolves.

`●` marks a name that is on, `○` one that is off. Each change saves `domains.json`, applies it and checks every
name. The status line at the bottom, above the key help, says what failed, or why the selected name fails.

| Key         | What it does                                     |
| ----------- | ------------------------------------------------ |
| `a`         | add a subdomain of the selected name             |
| `A`         | add a name; also `enter` on `Add new domain`     |
| `e`         | edit the selected name                           |
| `d`         | delete it; `y` confirms, any other key keeps it  |
| `space`     | turn it on or off                                |
| `c`         | link it to the project's compose file and `.env` |
| `p`         | preview the compose file, as oo would change it  |
| `g`         | show dnsmasq's query log; `g` or `esc` goes back |
| `r`         | read `domains.json` again and apply it           |
| `tab`       | move the keys to Caddy and dnsmasq, and back     |
| `q`         | quit; `ctrl+c` quits from anywhere               |
| `up` `down` | move; `j` and `k` work too                       |

In the add or edit form, `enter` saves, `tab` goes to the next field (`shift+tab` back) and `esc` cancels. While
you type a new name, the form fills in the address: the parent's address for a subdomain of a listed name, else
the lowest free own address. Typing in the address field stops that. A port needs Caddy installed and set up;
until then the form says what to run.

### Compose files

When `oo` starts inside a project, a git repository with a lock file, it looks up to 3 folders deep for
`compose.yml`, `docker-compose.yaml` and their variants, `.dev` first. Unless a listed name, whatever it is
called, already links a compose file in the project, it asks once: `use ./compose.dev.yaml for flowy.oo? Y/n`.
The project's folder suggests the name; to link a name called something else, answer `n` and press `c` on it. `y`
or `enter` links it (see `c` below), `e` lets you fix the path first, and `n` or `esc` saves `none`, so oo stops
asking; any other key leaves the question open. A name that isn't listed gets the add form, filled in. The form's
`compose` field sets or changes the path at any time: `~/…`, a path from where oo started, or `none`.

`p` shows the file as oo would change it, and changes nothing:

- A port with no address, or with `127.0.0.1`, binds `${DOCKER_HOST_IP:-127.0.0.1}`. Compose takes only an IP
  there, not a name, and a port with no address takes the port on every address, so projects collide.
- A `localhost` URL in `environment` takes the project's name, or the subdomain Caddy serves on that port:
  `http://localhost:3000` becomes `http://dashboard.crm.oo`.
- Healthchecks, commands and comments keep `localhost`: inside a container it is the container itself.

`c` links the selected name: the compose file of the project oo started in, or outside one the name's own,
becomes the name's, and `DOCKER_HOST_IP=<address>` goes into the `.env` that file runs with. That is the file a
`package.json` script passes with `--env-file`, else the project root's `.env`. oo leaves a `.env` that git
tracks alone: this Mac's address doesn't belong in a shared file. The question's `y`, `c` in the preview, and a
form save that changes a linked name's compose file or address do the same.

### Rules

- **Names end in `.oo`.** macOS sends a name like `flowy.local`, with one label before `.local`, to Bonjour
  only. Labels use `a-z`, `0-9` and inner `-`, 1–63 characters each.
- **A project is the last two labels.** `crm.oo`, `api.crm.oo` and `test.crm.oo` are one project.
- **A subdomain shares its project's address or takes its own.** The form fills in the parent's. While `crm.oo`
  is on, an unlisted `foo.crm.oo` resolves to its address anyway, and so does a listed one you turn off.
- **An own address belongs to one project.** The own block is `127.0.1.1`–`127.0.1.50`. `127.0.0.1`, or any
  other address in `127.0.0.0/8`, can be shared.
- **A port makes `http://<name>` reach `<address>:<port>`** through Caddy on port 80. HTTP only. Port 80 itself
  is refused, because Caddy listens there.
- **Apps listen on their project's address**, for example `next dev -H 127.0.1.3` or `vite --host 127.0.1.3`.
- **Docker:** publish ports as `"${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"`, which `p` shows on the project's
  compose file, and `c` writes `DOCKER_HOST_IP=127.0.1.3` into the project's `.env`, so each project's Postgres
  gets port 5432 on its own address.

## Commands

`oo apply` writes oo's files from `domains.json`, restarts dnsmasq, runs the resolver script, restarts Caddy
when its sites changed or it stopped, then checks every enabled name, prints one line each and exits 1 if one
fails. The list does the same after every change. It refuses to run before `oo setup`. Use it after editing
`domains.json` by hand:

```json
{"version": 1, "domains": [
  {"name": "crm.oo", "address": "127.0.1.1", "enabled": true},
  {"name": "dashboard.crm.oo", "address": "127.0.1.1", "port": 3000, "enabled": true}
]}
```

`oo doctor` runs eight checks and prints one line each, with the fix under each failure. It exits 1 if one fails.

1. `dnsmasq`: installed, run by `brew services` as you, and no root job shadows it.
2. `dnsmasq config`: Homebrew's `dnsmasq.conf` has one `conf-file=` line, oo's, and listens on `127.0.0.1:53535`.
3. `loopback`: the `io.oo.loopback` job is loaded and all 50 addresses are on `lo0`.
4. `resolvers`: the script is root-owned, mode 755 and current; `sudo` runs it without a password; every
   enabled name has its `/etc/resolver` file.
5. `resolver for .local`: there is no `/etc/resolver/local`.
6. `generated files`: oo's generated files in `~/.config/oo` match `domains.json`.
7. `names resolve`: each enabled name resolves through dnsmasq and macOS; with a port, Caddy reaches the app.
8. `caddy`, only when an enabled name has a port: Caddy is installed, imports oo's file, runs, passes
   `caddy validate`, and holds port 80.

`oo uninstall` removes what setup installed; see [Uninstall](#uninstall). `oo version` prints the version, and
`oo help` the usage.

## How it works

- **dnsmasq** runs as you, from Homebrew's service, on `127.0.0.1:53535`, a port that needs no root. Homebrew's
  `dnsmasq.conf` includes `~/.config/oo/dnsmasq.conf`: one `address=/<name>/<address>` line per enabled name,
  and every query logged to `~/.config/oo/dnsmasq.log`.
- **`/etc/resolver/<name>`** makes macOS ask dnsmasq for that name and its subdomains. oo writes one file per
  enabled name through a root-owned script that `sudo` runs without a password. The script only writes names
  that match the name rule, and only deletes files that start with oo's marker line, `# oo`.
- **The loopback LaunchDaemon**, `io.oo.loopback`, adds `127.0.1.1`–`127.0.1.50` to `lo0` at every boot. macOS
  has only `127.0.0.1` by default.
- **Caddy** runs as you on port 80. Homebrew's `Caddyfile` imports `~/.config/oo/Caddyfile`, which has one
  `http://<name>` site per enabled name with a port, forwarding to `<address>:<port>`.
- **`~/.config/oo/domains.json`** is the only source of truth. oo rewrites its generated files from it on
  every change.

## Troubleshooting

Start with `oo doctor`: every failed check prints the command that fixes it.

- **`can't assign requested address` from Docker, or `EADDRNOTAVAIL` from Node:** the loopback addresses are
  missing. Check 3; `oo setup` adds them again.
- **`sudo: a password is required`:** the sudoers rule is missing or the script changed. Check 4, then `oo setup`.
- **`dig flowy.oo` finds nothing:** expected. `dig` and `nslookup` skip `/etc/resolver`; apps and browsers use
  it. Test with `dscacheutil -q host -a name flowy.oo`, or ask dnsmasq: `dig @127.0.0.1 -p 53535 flowy.oo`.
- **A name isn't found right after you add it, or the log view shows no queries:** the lookup never reached
  dnsmasq. Its resolver file is missing (check 4), or a VPN is capturing DNS.
- **Check 4 says a resolver file is "not written by oo":** someone made it by hand, and oo never replaces such
  a file. Run the `sudo rm` that doctor prints, then `oo apply`.
- **A name resolves to the wrong address:** an `/etc/hosts` line for it wins over DNS. The name's check points
  at the line; remove it.
- **`foo.crm.oo` resolves although it isn't listed:** expected. dnsmasq's `address=` lines and the resolver
  files both match subdomains. Add a row only to give it another address or a port.
- **`502 Bad Gateway` on `http://dashboard.crm.oo`:** Caddy is up but nothing listens on `127.0.1.1:3000`; the
  check says `app down`. Vite needs `--host 127.0.1.1`, and Docker ports bind `DOCKER_HOST_IP`.
- **The wrong app answers:** a Node app listening on `*:3000` answers on every loopback address, so it shadows
  other projects' port 3000. Start each app on its own address: `next dev -H 127.0.1.3`.
- **`http://dashboard.crm.oo` refuses the connection:** Caddy isn't running or another program holds port 80.
  Check 8 names it.
- **Names resolve for one app but not another:** a VPN or Cloudflare WARP is capturing DNS.
- **Containers can't reach `flowy.oo`:** they don't need to. Inside Docker they use the service name.

## Uninstall

`oo uninstall` asks for your password, then removes oo's `/etc/resolver` files, stops dnsmasq, and restores
Homebrew's `dnsmasq.conf` from `dnsmasq.conf.before-oo`, or removes oo's block when there is no backup. When
Caddy was set up, it stops Caddy and restores `Caddyfile.before-oo`, or removes oo's import. Last, it takes
`127.0.1.1`–`127.0.1.50` off `lo0` and removes the loopback job, the sudoers rule and the script.

It keeps `~/.config/oo`, so a later `oo setup` brings your names back. dnsmasq stays stopped; to run it as root
on port 53 again, as before oo: `sudo brew services start dnsmasq`.

## Development

```sh
go test -race ./...                       # all tests
go vet ./... && golangci-lint run ./...   # lint, config in .golangci.yml
go test ./internal/tui -update            # rewrite that package's golden files after an intended change
go run ./cmd/oo doctor                    # run oo from source
```

### Change it and use it right away

Edit the code, then install the new build over the old one. It runs at once, as long as `~/go/bin` is on your
`PATH` (`echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc`):

```sh
go vet ./... && go test -race ./... && golangci-lint run ./...   # the checks CI runs
go install ./cmd/oo                                              # replaces ~/go/bin/oo
oo
```

| What you changed                                      | After `go install`                       |
| ----------------------------------------------------- | ---------------------------------------- |
| TUI, messages, checks, the CLI                        | nothing, just run `oo`                   |
| What goes into the generated dnsmasq or Caddy files   | `oo apply`                               |
| The root script, the sudoers rule or the loopback job | `oo setup`, which asks for your password |

`oo doctor` tells you which applies: "script out of date" means `oo setup`, and "generated files out of date"
means `oo apply`.

Golden files live in each package's `testdata/`; only packages that have them take `-update`. No test changes
the system, and the few that run `dnsmasq --test`, `caddy validate`, `visudo -c` or `plutil -lint` skip when the
tool is missing. `go build -ldflags "-X main.Version=v0.1.0" -o oo ./cmd/oo` sets the version `oo version`
prints. `docs/plan.md` holds the design and its decisions, `docs/setup-log.md` the hand test on a real Mac, and
`CLAUDE.md` the layout and rules for changing the code.
