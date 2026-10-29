# lodo reference

The full behavior behind the [README](../README.md): what each command changes, the rules and the fixes.

## Setup

Run `lodo setup` as yourself, not with `sudo`. It asks for your password once, before it changes anything. Then it:

1. creates `~/.config/lodo`, an empty `domains.json` when there is none, and the files lodo generates from it;
2. backs up Homebrew's `/opt/homebrew/etc/dnsmasq.conf` to `dnsmasq.conf.before-lodo` (first run only), drops its
   `conf-file`, `listen-address`, `port`, `bind-interfaces` and `bind-dynamic` lines, and adds lodo's block:
   include `~/.config/lodo/dnsmasq.conf` and listen on `127.0.0.1` port 53535. Every other line stays;
3. stops a root dnsmasq job, the kind `sudo brew services start dnsmasq` makes, and removes its plist from
   `/Library/LaunchDaemons`;
4. installs the resolver script, `/Library/Application Support/lodo/apply-resolvers.sh`, and the sudoers rule,
   `/etc/sudoers.d/lodo`, which lets you run that script, and nothing else, without a password;
5. installs `/Library/LaunchDaemons/io.lodo.loopback.plist`, which adds `127.0.1.1`–`127.0.1.50` to `lo0` now
   and at every boot;
6. removes `/etc/resolver/local` if it exists: that file takes every `.local` name away from Bonjour;
7. starts dnsmasq as you, with `brew services`;
8. when Caddy is installed, backs up `/opt/homebrew/etc/Caddyfile` to `Caddyfile.before-lodo` if there is one,
   makes it import `~/.config/lodo/Caddyfile`, and starts Caddy as you;
9. runs the script through `sudo -n` to prove the rule works, then prints `lodo doctor`'s checks.

It prints one line per step and stops at the first failure, with the command that fixes it. Running it again is
safe: it repairs the setup, prints the same lines and keeps the first backup. A backup that holds lodo's lines, made
after lodo had already changed the file, loses them, and goes when nothing else is left, so uninstall never puts
them back. Do so after you install Caddy, and after you upgrade lodo when `lodo doctor` reports "script out of
date". After setup, adding, editing and deleting names never asks for
a password, and neither does `lodo apply`.

## Using lodo

Run `lodo setup` once per Mac. After that, `lodo dev` is the command you use every day, in each project's folder:

```sh
cd ~/Projects/shop
lodo dev
```

The first time, lodo doesn't list the project yet, so `lodo dev` opens the setup form: you pick the project's
name and its apps' names, and `enter` saves them. Every later run starts the apps on their names right away, and
`lodo dev --edit` opens the form again. `lodo ps` opens the dashboard, the list of every name with its checks.
`lodo` alone prints the help.

`lodo dev` and `lodo ps` refuse to start while `lodo doctor` checks 1–5 fail, and print them; a dnsmasq you turned
off doesn't count.

### lodo dev

In a project, `lodo dev`:

1. opens the setup form when no name in the list belongs to the project, or with `--edit`. `esc` saves nothing
   and exits. With `-y` or `--auto` the form saves what it proposes with no key pressed, and stays open only when
   that breaks a rule;
2. warns about each app the scan found that has no name: `warn  apps/api has no name: lodo dev --edit adds
   api.shop.test`;
3. picks the package manager: `packageManager` in the project's `package.json`, else the lock file
   (`pnpm-lock.yaml`, `yarn.lock`, `bun.lock` or `bun.lockb`, `package-lock.json`), else npm. It refuses a
   project whose `package.json` has no `dev` script;
4. prints each name and the address its app listens on, the [variables it sets](#the-variables-lodo-dev-sets)
   and the scan's notes, then hands the terminal to `<pm> run dev`, so `ctrl+c` goes straight to the package
   manager:

```text
web.shop.test  https://web.shop.test  -> 127.0.1.1:3001
env   DOCKER_HOST_IP=127.0.1.1
env   DATABASE_URL=postgres://***@shop.test:5432/shop
starting: pnpm run dev
```

The apps get their address from a small Node hook. `lodo dev` writes `~/.config/lodo/bind.cjs` and
`~/.config/lodo/dev/shop.test.cjs`, which holds the project's ports and addresses, and runs the dev script with
`NODE_OPTIONS="--require …/shop.test.cjs"`, which every Node process under it loads, through pnpm and turbo too.
A server that listens on one of the project's ports with no host, `localhost`, `127.0.0.1`, `::1`, `0.0.0.0` or
`::` then listens on the project's address instead; other ports stay as they are. The hook also adds `.shop.test`
to `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS`, so Vite accepts the project's names with no `allowedHosts`. No project
file changes. Only Node apps get it: start any other dev server on its address yourself, and run
`docker compose up` yourself, from the dev script or with `lodo exec`. The hook tells apps apart only by port, so
`lodo dev` refuses a project with two names on one port and two addresses.

Outside a project, `lodo dev` exits 1.

### The variables lodo dev sets

lodo never edits a `.env`. `lodo dev` and `lodo exec` export the project's values instead, and an exported
variable beats a `.env` file in dotenv, Next, Node's `--env-file` and `process.loadEnvFile`, and `docker compose
--env-file`. A `.env` copied from another checkout works as it is, and is read fresh on every start:

- `DOCKER_HOST_IP` is the project's address, for the compose ports that bind `${DOCKER_HOST_IP:-127.0.0.1}`.
- In a git worktree, `COMPOSE_PROJECT_NAME` is the project's label, such as `hugger12`. It beats `name:` in the
  compose file, so each worktree gets its own containers and volumes, and starts with an empty database. A fixed
  `container_name:` still clashes between worktrees: leave it out. The main checkout keeps the file's name.
- In each app's `.env`, a host that is a name of another checkout of the same git repository becomes this
  checkout's: in a worktree named `hugger9.test` whose main checkout is `hugger.test`'s folder,
  `postgresql://…@hugger.test:5432` becomes `postgresql://…@hugger9.test:5432`, `SMTP_HOST=hugger.test`
  becomes `SMTP_HOST=hugger9.test` and `http://mail.hugger.test` becomes `http://mail.hugger9.test`. A name this
  checkout lacks stays, with a note.
- An http or ws URL on `localhost` or `127.0.0.1` on a port a name serves becomes that name, https when HTTPS is
  on: `API_URL=http://localhost:3001` becomes `API_URL=http://web.media.test`. Any URL on a port the compose file
  binds to `DOCKER_HOST_IP` becomes `<project>.test:<port>`, such as `postgres://…@media.test:5432`. Another http
  or ws port gets a note, and the rest stays: `localhost` still reaches it.

Every app of the project gets one environment, so a key two apps' `.env` files set to values that come out
different gets a note and stays as each file has it. A value over more lines, or with a `\` or a `$`, which
loaders read differently, stays too, with a note. Only each app's `.env` is read; `.env.local` and
`.env.development` keep their values, except for a key lodo sets. An exported value also reaches `${…}` in a
compose file, and a container can't resolve a `.test` name: give a container its own value in `environment`.
lodo prints a URL's user and password as `***`. Restart the dev servers after editing a `.env`, since an
exported variable keeps its value until then.

`lodo exec <command>` runs a command in the current folder with the same variables, without the listen hook, for
what runs outside the dev script: `lodo exec pnpm db:migrate`, `lodo exec docker compose up -d`. It needs the
project's names from a first `lodo dev`, and prints the variables on stderr, so the command's output stays its own.

### The setup form

```text
lodo dev · ~/Projects/flowy

Project   flowy .test   address 127.0.1.2

Apps
  apps/web  web.flowy.test   port 3000
  apps/api  api.flowy.test   port 4000

Services  compose.dev.yaml
  mail      mail.flowy.test  port 8025

[x] link compose.dev.yaml  lodo dev sets DOCKER_HOST_IP=127.0.1.2

tab/shift+tab move  space toggle  enter save + start  esc cancel
```

The form shows what [the scan](#what-the-scan-reads) found: the project's name and address, then a row per app and
per web page in its compose file, with a label and a port. The rows hold the project's listed names first, then the
ones the scan proposes. Each label gets `.<project>.test`; an empty label gives the app the project's name alone,
such as `flowy.test`, and only one row may have it: the root's app, when the project has one. No label may be
another app's folder name, since a later scan ties each app to its folder's name. The toggle links the compose
file; the scan's notes follow it. `tab` and `shift+tab` move, `space` turns the toggle on or off, `enter` saves
everything in one change, and `esc` cancels. A field that breaks a rule says why under it when you press `enter`.
From `lodo dev`, `enter` saves and then starts the apps; from the dashboard's `i`, it saves and goes back to the
list. A form taller than the terminal, or than the dashboard's box, scrolls: the
title, the status line and the keys stay put, a dim `↑ more` or `↓ more` stands for the hidden lines, and the field
with the focus stays in view. At the last stop, `tab` scrolls the lines under it into view before it goes back to
the top.

The project's name is yours: the form suggests the folder's, such as `flowy.test`, and the subdomains follow what
you type. In a git worktree whose main checkout has a name, it suggests that name with the number of the address
the worktree gets: `hugger12.test` on `127.0.1.12` for a worktree of `hugger.test`, at the lowest free address,
or at one that holds only a deleted worktree's `hugger<n>.test`, which the new worktree takes over with its
settings and its compose containers and data. Each name gets the HTTPS its main checkout's name with the same
label has. `lodo dev -y` in a new worktree saves all of this without the form. When the suggested name belongs to
another folder that is still there, such as a second clone of the repo, it suggests the first of `flowy-2.test`,
`flowy-3.test` … that no listed name uses, with an address of its own. Once
saved, the name keeps the project's folder as `root` in `domains.json`, so later scans know the project by it, even
when you reach the folder by another path, such as with other capital letters. A name already listed that links a
compose file in the project counts too, unless its project's folder is another one that is still there. A link to a
compose file that is gone, after you rename the project's folder or move the file, counts as none, and the form
offers the project's file again. Typing a listed name that has no folder, or whose folder is gone, takes that name
over: it keeps its settings and gets this folder, and the project's old name goes with its names. A listed name
whose folder is still there is refused: `flow.test belongs to ~/Projects/flow: give this project another name`. A
later scan keeps a listed name of the project on an app's port and address as that app's.

### The dashboard

`lodo ps` opens the list of names.

```text
 lodo   caddy ●   dnsmasq ●   loopback ●   resolvers ●
 ─────────────────────────────────────────────────────────────────────────────────────────────
 name                     enabled  address          port   compose                       check
 ● blog.test              ✓        127.0.1.1               ~/Code/blog/compose.dev.yaml  dns ✓
   ○ api.blog.test        ✓        127.0.1.1        3001                                 dns ✓
   ● dashboard.blog.test  ✓        127.0.1.1        3000                                 dns ✓
 ● media.test             ✓        127.0.1.3               –                             dns ✓
 ○ old.test                        127.0.0.1                                             –
   Add new domain
```

The top line shows Caddy, dnsmasq, loopback and resolvers (doctor checks 8, 1, 3 and 4): a green `●` is on and
works, a dim `○` is off (turned off, or Caddy not running while no name has a port), and a red `○` needs
`lodo doctor`. `tab` moves the keys to Caddy and dnsmasq: `←` `→` (or `h` `l`) pick one, `space` turns it on or
off, `tab` goes back to the names. A service turned off stays off through every change until you turn it on; with
dnsmasq off no `.test` name resolves, and lodo removes its `/etc/resolver` files, so no other account's process
can answer `.test` lookups on dnsmasq's port. Turning dnsmasq on writes them again.

`enabled` has a `✓` for a name that is on: lodo writes it into dnsmasq's config, the resolver files and, with a
port, Caddy's sites. The mark before a name sums up its check column with the top line's marks: a green `●` when
every check passed, a red `○` when one failed, like `api.blog.test`'s app above, and a dim `○` when nothing was
checked, because the name is off or a service you turned off explains it. The selected row draws its mark without
color, which would cut the row's highlight. Each change saves `domains.json`, applies it and checks every name.
The status line at the bottom, above the key help, says what failed, or why the selected name fails.

| Key         | What it does                                       |
| ----------- | -------------------------------------------------- |
| `a`         | add a subdomain of the selected name               |
| `A`         | add a name; also `enter` on `Add new domain`       |
| `e`         | edit the selected name                             |
| `d`         | delete it and its subdomains; `y` confirms         |
| `space`     | turn it on or off                                  |
| `s`         | turn HTTPS on or off; the name needs a port        |
| `l`         | link it to the project's compose file              |
| `p`         | preview the compose file, as lodo would change it  |
| `i`         | open the setup form for where `lodo ps` started    |
| `g`         | show dnsmasq's query log; `g` or `esc` goes back   |
| `r`         | read `domains.json` again and apply it             |
| `tab`       | move the keys to Caddy and dnsmasq, and back       |
| `q`         | quit; `ctrl+c` quits from anywhere                 |
| `up` `down` | move; `j` and `k` work too                         |

In the add or edit form, `enter` saves, `tab` goes to the next field (`shift+tab` back) and `esc` cancels. While
you type a new name, the form fills in the address: the parent's address for a subdomain of a listed name, else
the lowest free own address. Typing in the address field stops that. A port needs Caddy installed and set up;
until then the form says what to run.

### What the scan reads

`lodo dev`, the setup form and `lodo scan` all start from a scan of the project. A project is the git root above
the folder, or outside git the nearest folder above it with a lock file and its manifest (`pnpm-lock.yaml` and
`package.json`, `go.sum` and `go.mod`); your home folder never counts, and `skills-lock.json` is no lock file. The
scan reads, and never writes:

- the apps: the root's `package.json` `dev` script, or in a workspace (`pnpm-workspace.yaml` or `workspaces`)
  each package whose `dev` script starts a dev server lodo knows, and any package under `apps/` with a `dev`
  script. It knows Next, Vite (SvelteKit, Remix), Astro, Nuxt, Wrangler, React Router, Angular, Gatsby,
  Docusaurus, Storybook, Vue CLI, webpack, NestJS, Create React App and react-email. It follows `npm:next`-style
  scripts and the shell scripts `dev` runs.
- each app's port: `-p` or `--port`, `PORT=` before the command, a literal `port:` in `vite.config` or
  `astro.config`, else the framework's own: 3000 for Next, 5173 for Vite, 4200 for Angular, and so on. NestJS,
  Create React App and an unknown command first read `PORT=` from the app's `.env`, then `.env.example`; NestJS
  has no default. A port it can't read stays empty, with a note to set it in `lodo dev --edit`.
- the compose file: up to 3 folders deep, `.dev` first, never in `node_modules`, `vendor` or `testdata`.
- the web pages in that file: Mailpit (`axllent/mailpit`, from any registry, at any tag) and the port on your Mac
  its inbox, port 8025 in the container, is published on.
- the address: a listed name's in the project, else `DOCKER_HOST_IP` from the root's `.env` or `.env.example`
  when no other project has it, else the lowest free one.
- each app's `.env`, never `.env.example`: its `KEY=value` lines, for [the variables lodo dev
  sets](#the-variables-lodo-dev-sets). The root's `.env` counts only when the root is the app; in a workspace it
  feeds compose.
- the git repository's main checkout, from the root's `.git`: the root itself, or for a worktree the folder its
  `.git` file points into.

It proposes the project's name, a subdomain per app, such as `admin.media.test` with its port, `mail.media.test`
for Mailpit's inbox, the compose file to link and the variables `lodo dev` sets. All of a project's apps share its
address, and so does the inbox. Every app keeps its own port; when another name already has that port on the
address, the app gets the next free address and a note to give one of them another port, since `lodo dev` binds
one app per port. The inbox is skipped when a listed name has its port there, or when
the project runs another compose file than the one the scan read, or none.

### Compose files

The form's `compose` field sets or changes a name's compose file at any time: `~/…`, a path from where lodo
started, or `none`.

`p` shows the file as lodo would change it, and changes nothing:

- A port with no address, or with `127.0.0.1`, binds `${DOCKER_HOST_IP:-127.0.0.1}`. Compose takes only an IP
  there, not a name, and a port with no address takes the port on every address, so projects collide.
- A `localhost` URL in `environment` takes the project's name, or the subdomain Caddy serves on that port:
  `http://localhost:3000` becomes `http://dashboard.blog.test`. A user and password in the URL stay.
- Healthchecks, commands and comments keep `localhost`: inside a container it is the container itself.

`l` links the selected name: the compose file of the project lodo started in, or outside one the name's own,
becomes the name's. The setup form's link toggle and `l` in the preview do the same. The file's ports get the
project's address from the `DOCKER_HOST_IP` that `lodo dev` and `lodo exec` set.

### HTTPS

`s` turns HTTPS on for a name with a port: Caddy then also serves `https://<name>`, with a certificate from its
own local certificate authority, and `http://<name>` keeps working. The check column adds `https ✓` once a
request over HTTPS reaches the app, `https ✗` when it doesn't.

Browsers accept the certificate once macOS trusts Caddy's root. After you turn on the first HTTPS name, run
`lodo setup` again: it copies the root to `~/.config/lodo/caddy-root.crt`, adds that copy to the System keychain
and asks for your password. Until then the name's check says `https: Caddy's certificate isn't trusted: run lodo
setup`.

- Safari and Chrome use the keychain. Firefox on macOS reads its roots too, through Settings → Privacy &
  Security → Certificates, "Allow Firefox to automatically trust third-party root certificates you install".
  Firefox's HTTPS-Only Mode then reaches the name instead of showing "Secure Site Not Available".
- The root's private key lives in `/opt/homebrew/var/lib/caddy/pki/authorities/local`. Whatever can read it can
  sign a certificate this Mac trusts, for any site, which is why lodo trusts it only once a name uses HTTPS.
  `lodo uninstall` removes the trust from the copy in `~/.config/lodo`.
- lodo turns Caddy's admin API off, so no other user or web page can change Caddy's config through
  `localhost:2019`. Each site closes any connection from another device without an answer.

### Sign-in callbacks

An OAuth provider sends the browser back to a callback URL on your app. With HTTPS on, a local app gets the same
shape of URL as production, and `Secure` cookies work:

1. Turn HTTPS on for the name, say `app.blog.test`.
2. Point the app at it: `BETTER_AUTH_URL=https://app.blog.test` for Better Auth, `AUTH_URL` (or `NEXTAUTH_URL`)
   for Auth.js. A value left at `http://localhost:3000` sends the browser back to `localhost` after sign-in.
3. Register the callback with the provider: `https://app.blog.test/api/auth/callback/github` for Better Auth
   and Auth.js; other libraries use their own path.

Step 2 matters because Next builds `request.url` from its own host and port, not from the request: code that
builds an absolute URL from it, such as an OAuth callback, gets `http://localhost:3000`. The `Host` and
`X-Forwarded-Host` headers keep the `.test` name, so code and libraries that read them, such as Auth.js with
`trustHost`, get it. Redirects within the app stay on the `.test` name: Next sends them as relative URLs.

Providers check callback URLs differently. A GitHub OAuth app takes any URL, so the `.test` callback works.
**Google refuses it:** its redirect URIs must use a host whose top-level domain is on the public suffix list,
and `.test` never is. Only `localhost` is exempt. For Google sign-in, either:

- run the sign-in on `localhost`: callback `http://localhost:3000/api/auth/callback/google`, the app's URL set
  to `http://localhost:3000`, and the app opened there while you work on sign-in. Start it with `pnpm dev`, not
  `lodo dev`, for that: it then listens on `127.0.0.1` and shares port 3000 with every other project;
- or use a name under a domain you own, such as `app.dev.example.com`. Google only checks the URL's text and
  never visits it, so the name doesn't need a public DNS record. lodo serves only `.test` names, so such a name
  needs its own DNS and certificate setup.

### Rules

- **Names end in `.test`.** macOS sends a name like `media.local`, with one label before `.local`, to Bonjour
  only. Labels use `a-z`, `0-9` and inner `-`, 1–63 characters each.
- **A project is the last two labels.** `blog.test`, `api.blog.test` and `test.blog.test` are one project.
- **A subdomain shares its project's address or takes its own.** The form fills in the parent's. While `blog.test`
  is on, an unlisted `foo.blog.test` resolves to its address anyway, and so does a listed one you turn off.
- **An own address belongs to one project.** The own block is `127.0.1.1`–`127.0.1.50`. `127.0.0.1`, or any
  other address in `127.0.0.0/8`, can be shared.
- **A port makes `http://<name>` reach `<address>:<port>`** through Caddy on port 80, and `https://<name>` too,
  on port 443, once HTTPS is on. Ports 80 and 443 themselves are refused, because Caddy listens there.
- **Apps listen on their project's address.** `lodo dev` puts Node apps there; start anything else with its own
  host flag, such as `--host 127.0.1.3`.
- **Docker:** publish ports as `"${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"`, which `p` shows on the project's
  compose file, and run compose under `lodo dev` or `lodo exec`, which set `DOCKER_HOST_IP=127.0.1.3`, so each
  project's Postgres gets port 5432 on its own address.

## Commands

`lodo dev` starts the project in the current folder on its names, and opens the setup form first when the
project has no name yet; `--edit` opens it every time. See [lodo dev](#lodo-dev). `lodo exec <command>` runs a
command with the variables `lodo dev` sets; see [The variables lodo dev sets](#the-variables-lodo-dev-sets).
`lodo ps` opens the dashboard.
`lodo` alone, or `lodo help`, prints the usage.

`lodo apply` writes lodo's files from `domains.json`, restarts dnsmasq, runs the resolver script, restarts Caddy
when its sites changed or it stopped, then checks every enabled name, prints one line each and exits 1 if one
fails. The dashboard and the setup form do the same after every change. A name whose app isn't running is marked
`–` in apply's output and doesn't count, since apply can't start it; the dashboard still shows its red `○`.
`lodo apply` refuses to run before `lodo setup`. Use it after editing `domains.json` by hand:

```json
{"version": 1, "domains": [
  {"name": "blog.test", "address": "127.0.1.1", "enabled": true},
  {"name": "dashboard.blog.test", "address": "127.0.1.1", "port": 3000, "enabled": true}
]}
```

`lodo doctor` runs eight checks and prints one line each, with the fix under each failure. It exits 1 if one fails.

1. `dnsmasq`: installed, run by `brew services` as you, and no root job shadows it.
2. `dnsmasq config`: Homebrew's `dnsmasq.conf` has one `conf-file=` line, lodo's, and listens on `127.0.0.1:53535`.
3. `loopback`: the `io.lodo.loopback` job is loaded and all 50 addresses are on `lo0`.
4. `resolvers`: the script is root-owned, mode 755 and current; `sudo` runs it without a password; every
   enabled name has its `/etc/resolver` file, or, with dnsmasq turned off, none is left.
5. `resolver for .local`: there is no `/etc/resolver/local`.
6. `generated files`: lodo's generated files in `~/.config/lodo` match `domains.json`.
7. `names resolve`: each enabled name resolves through dnsmasq and macOS; with a port, Caddy reaches the app.
   A name whose app isn't running still passes and is listed after `not running:`. An app that listens on
   `127.0.0.1` or `::1` instead of its name's address fails: start it with `lodo dev` in its project. lodo can't
   tell whose app that is, so every stopped name on that port fails with it, listed once. An app on its own
   address that gives Caddy no HTTP answer fails too. The fix is `lodo apply` when a failure needs it, and "stop
   the app, then start it with lodo dev in its project" when every failure is an app on the wrong address;
   otherwise each failure names its own.
8. `caddy`, only when an enabled name has a port: Caddy is installed, imports lodo's file, runs, passes
   `caddy validate`, and holds port 80.

`lodo scan` prints what the project in the current folder lacks from the list, as the setup form shows it, and
the variables `lodo dev` sets, and changes nothing. `--json` prints it for scripts,
with the same `***` in URLs, and `--name hugger` proposes it under `hugger.test`. A listed `hugger.test` with no
folder, or a gone one, becomes this folder's (`save  this folder as hugger.test's project`); one whose folder is
still there gets only the note `hugger.test belongs to <folder>: give this project another name`. It exits 1
outside a project.

`lodo uninstall` removes what setup installed; see [Uninstall](#uninstall). `lodo version` prints the version.

## How it works

- **dnsmasq** runs as you, from Homebrew's service, on `127.0.0.1:53535`, a port that needs no root. Homebrew's
  `dnsmasq.conf` includes `~/.config/lodo/dnsmasq.conf`: one `address=/<name>/<address>` line per enabled name,
  and every query logged to `~/.config/lodo/dnsmasq.log`.
- **`/etc/resolver/<name>`** makes macOS ask dnsmasq for that name and its subdomains. lodo writes one file per
  enabled name through a root-owned script that `sudo` runs without a password. The script only writes names
  that match the name rule, and only deletes files that start with lodo's marker line, `# lodo`.
- **The loopback LaunchDaemon**, `io.lodo.loopback`, adds `127.0.1.1`–`127.0.1.50` to `lo0` at every boot. macOS
  has only `127.0.0.1` by default.
- **Caddy** runs as you on port 80, and on 443 for HTTPS names. Homebrew's `Caddyfile` imports
  `~/.config/lodo/Caddyfile`, which has one `http://<name>` site per enabled name with a port, forwarding to
  `<address>:<port>`; an HTTPS name's site also lists `https://<name>`. The app gets the `.test` name as `Host`,
  so origin checks such as Mailpit's live inbox pass. On Next's dev paths, a site turns its own `Origin` into
  `http://localhost:<port>`, which Next's dev check accepts with no `allowedDevOrigins`; any other page's `Origin`
  passes unchanged, so Next still refuses its hot reload requests.
- **`lodo dev`** runs the project's `dev` script with `NODE_OPTIONS` loading `~/.config/lodo/dev/<project>.cjs`,
  which hands the project's ports and addresses to `~/.config/lodo/bind.cjs`. That hook makes a Node server that
  listens on one of those ports listen on the project's address, and adds `.<project>` to
  `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS`, which Vite adds to its allowed hosts.
- **`~/.config/lodo/domains.json`** is the only source of truth. lodo rewrites its generated files from it on
  every change.

## Troubleshooting

Start with `lodo doctor`: every failed check prints the command that fixes it.

- **`can't assign requested address` from Docker, or `EADDRNOTAVAIL` from Node:** the loopback addresses are
  missing. Check 3; `lodo setup` adds them again.
- **`sudo: a password is required`:** the sudoers rule is missing or the script changed. Check 4, then `lodo setup`.
- **`dig media.test` finds nothing:** expected. `dig` and `nslookup` skip `/etc/resolver`; apps and browsers use
  it. Test with `dscacheutil -q host -a name media.test`, or ask dnsmasq: `dig @127.0.0.1 -p 53535 media.test`.
- **A name isn't found right after you add it, or the log view shows no queries:** the lookup never reached
  dnsmasq. Its resolver file is missing (check 4), or a VPN is capturing DNS.
- **Check 4 says a resolver file is "not written by lodo":** someone made it by hand, and lodo never replaces such
  a file. Run the `sudo rm` that doctor prints, then `lodo apply`.
- **A name resolves to the wrong address:** an `/etc/hosts` line for it wins over DNS. The name's check points
  at the line; remove it.
- **`foo.blog.test` resolves although it isn't listed:** expected. dnsmasq's `address=` lines and the resolver
  files both match subdomains. Add a row only to give it another address or a port.
- **`502 Bad Gateway` on `http://dashboard.blog.test`:** Caddy is up but nothing listens on `127.0.1.1:3000`. When
  the app isn't running, check 7 lists it as not running. When it listens on `localhost` instead, as Vite does
  when started without `lodo dev` (Node puts `localhost` on `::1`), the check says `an app listens on [::1]:3000,
  not on the name's address`: start it with `lodo dev`. An app that serves HTTPS itself gets the same 502; the
  check says it `gave Caddy no HTTP answer`. Docker ports bind `DOCKER_HOST_IP`.
- **The wrong app answers:** a Node app listening on `*:3000` answers on every loopback address, so it shadows
  other projects' port 3000. Start each project with `lodo dev`, which puts its apps on its own address.
- **`http://dashboard.blog.test` refuses the connection:** Caddy isn't running or another program holds port 80.
  Check 8 names it.
- **Names resolve for one app but not another:** a VPN or Cloudflare WARP is capturing DNS.
- **Containers can't reach `media.test`:** they don't need to. Inside Docker they use the service name.

## Uninstall

`lodo uninstall` asks for your password, then removes lodo's `/etc/resolver` files, stops dnsmasq, and restores
Homebrew's `dnsmasq.conf` from `dnsmasq.conf.before-lodo`, or removes lodo's block when there is no backup. When
Caddy was set up, it stops Caddy and restores `Caddyfile.before-lodo`, or removes lodo's import. Last, it takes
`127.0.1.1`–`127.0.1.50` off `lo0` and removes the loopback job, the sudoers rule and the script.

It keeps `~/.config/lodo`, so a later `lodo setup` brings your names back. dnsmasq stays stopped; to run it as root
on port 53 again, as before lodo: `sudo brew services start dnsmasq`.
