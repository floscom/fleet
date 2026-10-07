# fleet

```
███████╗██╗     ███████╗███████╗████████╗
██╔════╝██║     ██╔════╝██╔════╝╚══██╔══╝
█████╗  ██║     █████╗  █████╗     ██║
██╔══╝  ██║     ██╔══╝  ██╔══╝     ██║
██║     ███████╗███████╗███████╗   ██║
╚═╝     ╚══════╝╚══════╝╚══════╝   ╚═╝
agents in formation // lan // tmux
```

fleet runs coding agents (Claude Code, Codex, a plain shell, and more
through adapters) in tmux sessions on a server, and lets your other machines
on the same LAN start, watch, stop and type into them.

- **One Go binary.** `fleet daemon` is the headless server; every other
  subcommand is the CLI.
- **Terminal-first.** Each agent runs its own interactive TUI in tmux.
  Clients stream the real terminal. Hooks installed into the agent CLI
  report finer status (working, idle, needs input).
- **LAN pairing, no accounts.** Daemons advertise themselves with mDNS.
  A device pairs once with a short code and is pinned to the daemon's
  certificate. There is no cloud relay.
- **Roots.** Agents can only be started inside folders you allow.
- **Agents outlive the daemon.** Restarting the daemon re-adopts running
  agents. tmux sessions are always cleaned up.

The protocol is protobuf over TLS, documented for native clients (a macOS
app is next) in [docs/PROTOCOL.md](docs/PROTOCOL.md).

## Install

Requirements on the server: Linux (macOS works too, but is less tested),
tmux (tested with 3.4), git (for worktrees), and the agent CLIs you want to
run.

Install the latest release (macOS and Linux, amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/floscom/fleet/main/install.sh | sh
```

The script verifies the download against the release's `checksums.txt` and
installs to `/usr/local/bin` if writable, else `~/.local/bin`. Set
`FLEET_VERSION=v0.1.0` for a specific release or `FLEET_INSTALL_DIR` for
another directory (both go before `sh`, e.g. `... | FLEET_VERSION=v0.1.0 sh`).
Run it again to update, then restart the daemon (`fleet stop && fleet
start`; running agents are re-adopted). The archives are also on the
[releases page](https://github.com/floscom/fleet/releases).

From source (needs Go 1.22):

```sh
make build                      # -> bin/fleet (static, CGO_ENABLED=0)
install -m 755 bin/fleet ~/.local/bin/fleet
```

Releases are built by GitHub Actions (`.github/workflows/release.yml`)
when a `v*` tag is pushed: `git tag v0.1.0 && git push origin v0.1.0`.

Other targets: `make test`, `make vet`, `make lint` (vet, `buf lint`,
gofmt), `make proto` (regenerate `gen/fleetv1` with buf), and `make cross`
(`bin/fleet-{darwin,linux}-{arm64,amd64}`).

Install the same binary on every machine you want to control the server
from.

## Quick start

On the server:

```sh
fleet start                     # daemon in the background, log in ~/.fleet/daemon.log
fleet roots add ~/code --trust  # allow agents in ~/code (root name "code"), skip trust prompts
fleet adapters                  # which agent CLIs were found
fleet run claude code:api --attach   # try it locally; detach with Ctrl-\
fleet pair                      # one-time code + server fingerprint for a new device
fleet web                       # admin link for the web dashboard: sessions, folders, roots
```

Or, from inside a project folder, start the daemon and make that folder a
root in one go: `fleet start --root .` (see
[Starting in a folder](#starting-in-a-folder)).

On the laptop, with the code and fingerprint that `fleet pair` printed:

```sh
fleet discover                  # daemons on the LAN
fleet connect studio --code ABCD-EFGH --fingerprint 3b4405b1be5c9a1f
fleet -H studio ls
fleet -H studio run claude code:api --attach
fleet -H studio run codex code:web --prompt "fix the flaky test"
fleet -H studio attach codex-web-1 --read-only
```

`-H` (or `FLEET_HOST`) takes a paired server's name, id prefix or
address. Without it, commands talk to the local daemon.

## Commands

| Command | What it does |
|---------|--------------|
| `fleet daemon [--listen ADDR\|off] [--web ADDR\|off] [--no-mdns] [--root DIR]... [--trust] [--join KEY]` | Run the daemon in the foreground (logs to stderr). `--root`: see [Starting in a folder](#starting-in-a-folder). `--join`: see [Several machines](#several-machines-one-dashboard). |
| `fleet start [--listen ...] [--web ...] [--no-mdns] [--root DIR]... [--trust] [--join KEY]` | Start the daemon in the background. If it already runs, only adds the `--root` folders and stores the `--join` key. |
| `fleet stop [--force]` | Stop the daemon. Agents keep running in tmux. |
| `fleet status` | Daemon version, uptime, server id, agent, root and device counts. |
| `fleet version` | Print the version. |
| `fleet roots` / `roots add <path> [--name N] [--adapters a,b] [--trust]` / `roots rm <name>` | Manage the folders agents may run in. `--trust`: see [Folder trust](#folder-trust). |
| `fleet browse [root[/path]] [-a]` | List folders inside a root, marking git repos and running agents. |
| `fleet adapters` | Adapters, whether their CLI is installed, version and features, and the models and efforts `fleet run --model/--effort` take. |
| `fleet run <adapter> [path] [flags] [-- agent args]` | Start an agent. `path` is `root:rel/path`, an absolute path, or (local only) a relative path; default `.`. Flags: `--name`, `--pinned`, `--worktree`, `--branch`, `--prompt`, `--model`, `--effort`, `--attach`, `--clone <repo>` (instead of a path), `--docker` / `--sandbox docker\|none`. |
| `fleet ls [-a] [--json]` | List agents (`-a` includes exited and failed ones). |
| `fleet attach <agent> [-r]` | Attach to an agent's terminal. Detach with Ctrl-\\. |
| `fleet send <agent> [text...] [--image FILE]... [--no-enter]` | Type text into an agent's terminal and press Enter; `--image` attaches image files first. |
| `fleet kill <agent>... [--rm-worktree] [--force] [--forget]` | Stop agents; optionally remove their worktree or clone and history entry. |
| `fleet sandbox build [--pull] [--tag T]` / `sandbox dockerfile` | Build the Docker image sandboxed agents run in (on the server) / print its Dockerfile. |
| `fleet watch` | Print agent state changes as they happen. |
| `fleet pair [--ttl 5m]` | Create a one-time pairing code (at most 5 minutes). |
| `fleet connect [addr\|name] --code CODE [--fingerprint HEX] [--name N]` | Pair this machine with a remote daemon. |
| `fleet discover [--timeout 2s]` | List daemons on the LAN. |
| `fleet servers` / `servers rm <name\|id>` | Daemons this machine is paired with. |
| `fleet devices` / `devices revoke <id\|name>` | Devices paired with the daemon. |
| `fleet web [--rotate]` | Print admin links for this machine's [web dashboard](#web-dashboard) and the `fleet start --join` command for other machines; `--rotate` signs every browser out. Run it on the server. |
| `fleet hook ...` | Internal: called by agent hooks. |

Agents are referred to by id (`a1b2c3`) or name (`claude-api-1`, or the
`--name` you gave).

### Starting in a folder

`--root` makes folders roots as the daemon starts, so a machine joins the
fleet with one command from wherever you are:

```sh
cd ~/code/api
fleet start --root .            # start the daemon if needed, allow agents in ~/code/api
```

- If the daemon already runs, `fleet start --root` only adds the folder.
  A folder that already is a root is left as it is, so running it again is
  harmless. `fleet daemon --root .` does the same in the foreground.
- The root is named after the folder (`api`, or `api-2` if that name is
  taken). Like `fleet roots add`, it is saved in `config.toml` and stays
  after the daemon stops; `fleet roots rm api` removes it.
- `--root` can be repeated. `--trust` trusts the roots it adds (see
  [Folder trust](#folder-trust)), not roots that already existed.
- A relative path is taken from the current folder, and the folder must
  exist. If it does not, `fleet start` fails before starting the daemon.

Once the daemon runs it is on the LAN (`fleet discover`, the dashboards of
other fleets), and every paired device can start agents in the new root:
`fleet -H studio run claude api: --prompt "..."`. A device that never
paired with this machine still pairs once (`fleet pair`, `fleet connect`).

### Isolation

Inside a git repository an agent gets its own **worktree** by default: a
new branch `fleet/<name>` checked out under
`~/.fleet/worktrees/<repo>-<hash>/<name>`, so several agents can work on
the same repo without stepping on each other. `--pinned` runs the agent
directly in the folder instead; several pinned agents may share a folder
(and its files).
Outside git, agents are always pinned. `fleet kill --rm-worktree` removes
the worktree (the branch is kept) unless it has uncommitted changes.

`--clone <repo>` starts from a fresh clone instead of a folder: the daemon
clones `owner/repo` (GitHub), an `https://` or `ssh://` URL, or
`git@host:org/repo` with its own git credentials into
`~/.fleet/clones/<name>`, and checks out `--branch` (the remote branch if it
exists, else a new branch, default `fleet/<name>`). `fleet kill
--rm-worktree` deletes the clone unless it has uncommitted changes or
commits that were never pushed.

### Docker sandbox

`--docker` (or `--sandbox docker`, or `default = "docker"` under
`[sandbox]`) runs the agent in a container instead of on the server. It
works with every mode above:

```sh
fleet sandbox build                                  # once, on the server: the fleet-agent image
fleet run claude code:api --docker                   # worktree of a local repo
fleet run claude code:notes --docker --pinned        # the folder itself, mounted read-write
fleet run claude --clone owner/repo --docker --prompt "fix issue 42"
```

Nothing else changes: the tmux pane runs `docker run -it`, so attach,
send, `needs input` and exit codes work as usual, and the container is
removed when the agent exits or is killed (the daemon also removes
leftover containers at startup).

What the container sees, each at the same path as on the host:

- the agent's checkout: its worktree or clone, or for `--pinned` the
  folder (the whole repository when the folder is inside one), read-write;
- for a worktree, the repository's `.git` directory, read-write so the
  agent can commit. The rest of your checkout is not visible;
- `.git/config`, `.git/hooks` and a worktree's `.git` file read-only,
  because git on the host runs what they name;
- a home directory, `/home/fleet`, kept across containers in
  `[sandbox] dir`/home: log in once (`/login` in Claude Code) and it sticks,
  or share the server's login (below);
- a socket that only accepts this agent's hook events, and a copy of the
  fleet binary for the hooks. The daemon's own socket is not visible.

**Using the server's login.** With `auth = ["claude", "codex"]` under
`[sandbox]`, sandboxed agents use the Claude Code and Codex logins of the
user running the daemon (`~/.claude/.credentials.json`,
`~/.codex/auth.json`, or under `CLAUDE_CONFIG_DIR` / `CODEX_HOME`). They are
not bind-mounted: the CLIs replace these files atomically, which a file
mount does not survive, and snap Docker cannot see them. The daemon copies
them into the sandbox home instead, and keeps both copies in sync while
sandboxed agents run: whichever side refreshed its token last wins, and the
host file is written under Claude's own write lock. That way neither side
logs the other out when OAuth tokens rotate. A missing host login is never
recreated from a sandbox. On macOS, Claude Code keeps its login in the
Keychain, so there is no file to share.

The container runs as your uid:gid with all capabilities dropped. Your
git name and email, and the daemon environment variables listed in
`[sandbox] env` (default `CLAUDE_CODE_OAUTH_TOKEN`, `GH_TOKEN`,
`GITHUB_TOKEN`), are passed in; `claude setup-token` gives you a
`CLAUDE_CODE_OAUTH_TOKEN` for the daemon's environment. Folder trust
prompts are skipped in a sandbox, since what a folder's settings run stays
in the container. The image ships Claude Code, Codex, git, gh, ripgrep,
Node and Python; `fleet sandbox dockerfile` prints its Dockerfile to build
your own `FROM fleet-agent` with more tools. Update the agent CLIs with
`fleet sandbox build --pull`.

**Docker installed as a snap** (Ubuntu's default) cannot see `/tmp` or
hidden folders such as `~/.fleet`. fleet checks this and tells you; set
`dir` under `[sandbox]` to a visible folder, such as `~/fleet-sandbox`.
Worktrees and clones of sandboxed agents, their state, the sandbox home and
the fleet binary copy all live there. Repositories you mount (`--pinned`,
and a worktree's `.git`) must be visible to Docker too.

The sandbox protects the server from what the agent runs. It does not make
the agent's output trustworthy: review what it commits before building or
running it on the host.

## Configuration

Everything lives in `FLEET_HOME` (default `~/.fleet`, mode 0700):

| Path | Content |
|------|---------|
| `config.toml` | configuration (below) |
| `fleet.sock` | local control socket (0600) |
| `daemon.pid`, `daemon.log` | pid of the running daemon; log when started with `fleet start` |
| `identity/` | server TLS key and certificate, this machine's device key (0600) |
| `devices.json` | devices paired with this daemon |
| `web-token` | the web dashboard's admin token (0600), created by `fleet web` |
| `fleet-key` | the key shared by machines that manage each other (0600), from `fleet web` or `--join` |
| `servers.json` | daemons this machine has paired with |
| `agents.json`, `agents/<id>/` | agent registry and per-agent adapter files |
| `worktrees/` | git worktrees created for agents |
| `clones/` | repositories cloned for agents (`--clone`) |
| `sandbox/` | default `[sandbox] dir`: worktrees, clones, state and home of sandboxed agents |

`config.toml`, with every key:

```toml
# Name shown to clients and in mDNS. Default: the hostname.
name = "studio"

# TCP address for paired devices (TLS). Default "0.0.0.0:7420".
# "off" or "" disables remote access; the CLI on the server still works.
listen = "0.0.0.0:7420"

# Web dashboard, plain HTTP (see "Web dashboard"). Default "0.0.0.0:7421".
# "127.0.0.1:7421" keeps it on this machine; "off" disables it.
web = "0.0.0.0:7421"

# Advertise the daemon on the LAN via mDNS (_fleet._tcp). Default true.
mdns = true

# tmux -L socket name. Default "fleet" for ~/.fleet, and
# "fleet-<hash of FLEET_HOME>" for any other FLEET_HOME.
tmux_socket = "fleet"

# "worktree" (default: a worktree when inside a git repo, else pinned) or "pinned".
default_isolation = "worktree"

# Folders agents may start in. Managed with `fleet roots add/rm`.
[[root]]
name = "code"
path = "/home/flo/code"          # stored symlink-resolved
adapters = ["claude", "codex"]   # optional; empty or missing = all adapters
trust = true                     # optional; pre-answer folder trust prompts (see below)

# Per-adapter overrides (claude, codex, shell).
[adapter.claude]
binary = "/home/flo/.local/bin/claude"   # default: PATH, then well-known dirs such as ~/.local/bin
args = ["--model", "opus"]               # added to every launch, before `fleet run ... -- args`

# Docker sandboxes (see "Docker sandbox"). Every key is optional.
[sandbox]
default = "none"                 # "docker" sandboxes agents that do not ask for either
image = "fleet-agent:latest"     # built by `fleet sandbox build`
dir = "~/fleet-sandbox"          # default ~/.fleet/sandbox; must be visible to Docker
env = ["CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"]   # passed in when set
auth = ["claude", "codex"]       # share the server's logins with sandboxes (default: none)
network = ""                     # docker run --network; default: Docker's bridge
args = ["--memory=8g", "--cpus=4"]   # extra docker run flags, e.g. limits
docker = "docker"                # the docker CLI

# Agents stopped by a usage limit (see "Usage limits"). Every key is optional.
[limits]
auto_resume = true               # resume them once the limit resets (default true)
message = "continue"             # what is typed into the session to resume it
```

The daemon reads `config.toml` at startup. Changes made through the CLI
(`fleet roots add/rm`, `--root`) apply immediately and rewrite the file
(comments are not kept). Restart the daemon after editing the file by hand. Command-line
`--listen`, `--web` and `--no-mdns` override the file.

## Web dashboard

The daemon serves a live dashboard at <http://SERVER:7421/>: agents and
their states, the fleet daemons it sees on the LAN, paired devices and
roots. It updates over a WebSocket (`/ws`) as agents start, change state or
exit.

**Viewing needs nothing; managing needs the admin link.** Run `fleet web`
on the server:

```
$ fleet web
Admin links for the fleet dashboard on studio (open one once per browser):

    http://192.168.1.20:7421/#token=3f9c...
    http://localhost:7421/#token=3f9c...
```

Open one in the browser you manage the fleet from. Or click **unlock** in
the dashboard's header and type (or paste) the fleet key from
`fleet start --join KEY`, or the admin token from the link: the same key
unlocks the dashboard of every machine in the fleet, no `fleet web` run
there needed. That browser then shows **admin** in the header and can:

- **Add folders as roots**: *Roots → Add folder* opens a folder picker
  over the server's file system. Walk into folders (click, or type to
  filter and press Enter; Backspace goes up), paste a path, show hidden
  folders; git repositories and existing roots are marked. Then pick a
  name, optionally limit it to some agents, optionally trust it (see
  [Folder trust](#folder-trust)) and add the current folder.
- **Edit roots**: *Roots → edit* opens the folder picker with the current
  name, path, agent restrictions and trust setting. Save changes directly;
  running agents are not affected. An existing root in the picker also has
  an *Edit root* button, including on other machines.
- **Remove roots** (running agents are not affected).
- **Run sessions**: start Claude Code, Codex or a shell, follow the
  conversation, answer the agent, type into it and stop it (see
  [Sessions in the browser](#sessions-in-the-browser)), on this machine or
  on other fleets.
- **See agent CLIs and plan usage per machine**: under *Fleet on the
  network*, this daemon and every fleet holding the fleet key list Claude
  Code and Codex: installed or not, and how much of the logged-in account's
  plan limits (5-hour session, week, ...) is used, with the reset time below
  each meter in your local time (full date and time on hover). Each daemon
  asks the provider at most every 45 s with the token its CLI stored
  (`~/.claude/.credentials.json`, `~/.codex/auth.json`) and never
  refreshes it; a Claude login in the macOS Keychain shows as unknown.
- **Add roots on other machines**: the folder picker starts with a
  *Machine* row listing this daemon and every fleet on the LAN with a
  dashboard. Pick one to browse its folders and add a root there (see
  [Several machines](#several-machines-one-dashboard)).
- **Jump to other fleets**: every daemon under *Fleet on the network*
  links to its own dashboard. Each fleet has its own admin token: unlock
  it there with the fleet key, or run `fleet web` there once as well.

Changes apply at once, rewrite `config.toml` and show up in the CLI
(`fleet roots`) and in every open dashboard.

- **The token.** `fleet web` creates `~/.fleet/web-token` (0600) and
  prints it in the link's `#fragment`, which browsers never send to the
  server. The page moves it to `localStorage` (per origin) and sends it as
  an `Authorization: Bearer` header on admin requests; there is no cookie,
  so other sites cannot make your browser act for them.
  `fleet web --rotate` replaces it and every browser loses admin rights on
  its next request (no restart needed). *sign out* forgets it in one
  browser. *unlock* sends the key typed into it to `POST /api/unlock`,
  which answers with the admin token (creating it if `fleet web` never
  ran) when the key is this machine's fleet key or admin token.
- **Plain HTTP.** The token crosses the network in the clear on each admin
  request. Use it on a network you trust, over Tailscale/WireGuard, or keep
  the dashboard on the server (`web = "127.0.0.1:7421"`) and forward it:
  `ssh -L 7421:127.0.0.1:7421 studio`.
- Requests whose `Host` does not name this machine (an IP, `localhost` or
  its hostname) get 403 (DNS rebinding), the WebSocket only accepts
  same-origin pages, and a strict Content-Security-Policy is sent.
- `--web ADDR` or `web` in `config.toml` sets the address, `--web off`
  disables it. If the port is busy the daemon logs a warning and runs
  without it. The daemon advertises the dashboard's port in mDNS (TXT
  `web`) unless it listens on loopback only.
- On desktop, clicking outside a dialog closes it. **Ctrl+1** through
  **Ctrl+9** opens the first nine live agents in the displayed order,
  respecting the folder filter. The shortcuts also switch sessions from
  an open chat; each agent row shows its shortcut.
- Text fields are 16px on phones, so iOS does not zoom in, and dialogs
  do not pop up the keyboard when they open. Buttons, keys and list rows
  are at least 44px tall on touch screens and phone-width windows.
- Everything is embedded in the binary. The compiled Tailwind CSS
  (`internal/web/static/app.css`) is committed, so `make build` needs no
  Tailwind; `make web` rebuilds it after UI changes.
- The favicon and the home-screen / install icons (with a web app
  manifest) are drawn from one pixel map in `internal/web/gen_icons.go`;
  `make icons` redraws them, and the results are committed too. Added to
  an iPhone's or iPad's home screen, the dashboard opens full screen with
  a launch screen (the icon over the header's FLEET logo): iOS needs one
  image per screen size and orientation, so `make icons` draws 44 of them
  into `static/splash/` and writes their links into `index.html`.

### Notifications

*notify* (next to *sign out*) subscribes the browser to Web Push: the
daemon then sends a notification when one of its agents needs input, is
done or fails, and tapping it opens that session. *Done* means the agent
stayed idle for 20 seconds and its session runs no background subagents or
workflows: those start another turn when they end, so the turns in between
send nothing. It is the
standard Push API, so it works with Safari (macOS 13+, and iOS/iPadOS
16.4+ once the dashboard is added to the Home Screen and opened from
there), Chrome, Edge and Firefox. No Apple developer account or relay is
needed: the daemon signs with its own VAPID key (`~/.fleet/push-key`) and
posts the encrypted message straight to the browser's push service
(`*.push.apple.com`, FCM, Mozilla), so the server needs outbound HTTPS.
Subscriptions live in `~/.fleet/push.json`; ones the push service reports
gone are dropped.

Browsers only offer push to **HTTPS** pages (or `localhost`), so the plain
HTTP port is not enough. With Tailscale, enable HTTPS certificates for the
tailnet and run `tailscale serve --bg 7421` on the server, then open
`https://SERVER.TAILNET.ts.net/`. On iPhone: open that URL in Safari,
Share › Add to Home Screen, open the app, unlock it (a Home Screen app has
its own storage), and tap *notify*.

### Usage limits

When a Claude Code or Codex session stops because the account ran out of
a plan limit (the 5-hour window, the week, ...), the daemon notices, finds
out when the limit resets, and types `continue` into the session a minute
after that. The agent shows *idle* with a **limit** chip (`limit 5h ·
resumes 22:58`), its session a bar with **Resume now** and **Turn
auto-resume off** (per agent; `[limits] auto_resume = false` turns it off
for all), and a notification says the agent hit the limit.

How it knows: Claude Code writes the refused turn to the transcript with
the limit and its reset time (`"error":"rate_limit"`, `quotaLimits.resetsAt`).
Codex writes the window that ran out with its `resets_at` (`token_count`),
and its refusal (`error`, or the message on the screen). When neither says
when it resets, the account's usage (the meters under *Fleet on the
network*) does. Before typing, the daemon asks the account's usage again
and waits longer if the limit is still used up; it does nothing while the
agent is working (Claude Code can wait out a limit by itself), closes a
dialog first (Esc), and gives up, with a notification, after 3 resumes the
session did not react to, or 4 limits within an hour.

### Sessions in the browser

With the admin link, every agent row has an **open** button (**answer** when
it needs input), and *Agents → New session* (or the **N** key, or **▸ start**
on a root, which picks that folder) starts one:

- **Start**: pick the machine, the agent (Claude Code, Codex, shell), a
  root and a folder inside it, and optionally a prompt.
  Agent rows show a small provider logo on the right. The launcher remembers
  the last successfully used provider for each root on each machine, using
  recent sessions on other browsers and local storage after sessions are
  removed. If that provider is unavailable or disallowed, it picks an
  installed provider the root allows.
  *Model* and *Effort* choose what the agent runs at (Claude Code: Opus,
  Fable, Sonnet, Haiku at Low to Max or Ultracode; Codex: the models it
  lists), *Default* leaves it to the CLI's own settings; the page remembers
  the pick per agent. *Checkout* (worktree or the folder itself) and
  *Sandbox* (Docker or not) are click choices like `fleet run`'s flags, *Then*
  opens the chat, the chat with the terminal screen, or stays on the list;
  the page remembers them. *Name & branch* are optional. Keys: Ctrl/⌘+Enter
  starts, Ctrl/⌘+Shift+Enter starts and shows the screen, Alt+1–9 picks the
  agent.
- **Roots**: with more than six, the list shows the first six, a filter and
  *Show all*.
- **Model and effort.** Under the message box, *model* and *effort* show
  what the session runs at (read from the transcript: the model of the
  last reply, or a switch since). In a Claude Code session they switch it,
  for this session only: the daemon opens `/model` in the terminal, picks
  the model and effort, and presses `s`, which, unlike Enter or
  `/model <name>`, does not make them your default for new sessions
  (`POST /api/agents/{id}/model`). Codex saves any `/model` pick as its
  default, so a Codex session shows its model and effort without switching.
- **Chat.** The conversation is read from the transcript the CLI writes
  (`~/.claude/projects/…/<session>.jsonl` for Claude Code,
  `~/.codex/sessions/…/rollout-….jsonl` for Codex; in a sandbox, those in
  the sandbox home). Your messages, the agent's replies and every tool call
  are shown; click a tool call for its input and output. Messages appear
  when the CLI has written them, not token by token. The page long-polls
  (`GET /api/agents/{id}/chat?wait=1`, answered as soon as something
  changes, else after 8 seconds); *Load earlier* pages back through long
  sessions. After `/clear` the chat follows the new session.
- **Images.** Images pasted into a message, and those a tool returned (a
  `Read` of a PNG, a browser screenshot), show in the chat as thumbnails;
  click one for full size. They stay in the transcript: the page fetches
  each as it scrolls into view (`GET /api/agents/{id}/image`, the data as
  JSON). The *Media* tab, shown once a session holds an image, has all of
  them in a grid, newest first, labelled with the file a tool read (or "sent
  by you"); in the full-size view the arrow keys, a sideways swipe or ‹ ›
  step through them. It lists them with `GET /api/agents/{id}/media` (the
  whole transcript, then what follows an offset) and asks again whenever the
  chat shows a new image. Claude Code only for now.
- **Questions.** When Claude asks you something with choices (its
  AskUserQuestion tool), the questions show as a form above the message box:
  pick one option, or several where it allows, type your own answer, add a
  note, and *Answer*. A message sent while the form shows answers none of
  them: Claude reads it instead, as with *Chat about this* in the terminal.
  The dialog stays open in the terminal meanwhile, and whichever you answer
  first wins. This works through Claude's PermissionRequest hook, which
  `fleet hook --wait` holds open until the question is answered or gone
  (`POST /api/agents/{id}/answer`); sessions started before this version,
  or a question asked while the daemon restarted, fall back to the keys.
- **Send.** The message box types into the agent's terminal and presses
  Enter (Enter sends, Shift+Enter is a new line; text with several lines
  goes in as one paste). While the agent works, Claude Code and Codex queue
  it.
- **Send images.** Attach images with the picture button, by pasting them
  into the message box or by dropping them on the chat (up to 10 per
  message). They go with the next message: the daemon saves each in the
  agent's state dir and pastes its path, which Claude Code and Codex turn
  into `[Image #n]`. Photos over 2000 px are scaled down in the browser
  first; formats the agents cannot read (HEIC) become JPEG. From the CLI:
  `fleet send <agent> --image a.png --image b.png "what changed?"`.
- **Screen and keys.** Permission prompts, folder trust and menus are not
  part of the transcript. *Screen* shows the agent's terminal as text
  (refreshed every 1.5 seconds; it opens by itself when the agent needs
  input), and the key row sends Esc, arrows, Tab, Enter, 1-3 and Ctrl-C to
  answer them. For the full terminal, `fleet attach` still works.
- **Stop** asks in a bar under the header, away from Stop itself, so a
  double tap cannot stop a session; *Stop session* there kills it like
  `fleet kill` (the worktree is kept). On a phone the session fills the
  screen and closes with the back arrow at the top left.
- **Workflows.** When Claude Code runs a workflow (its Workflow tool: a
  script that starts many subagents in phases, in the background), the
  agent's row shows each run going on: its phase, a bar per subagent and
  how long it has run (a finished run stays for 10 minutes). The session
  gets a *Workflows* tab, and the Workflow call in the chat a *progress*
  link. Each run is a card: its phases side by side, then every subagent
  by phase with what
  it does right now (its latest tool call), its tool calls, tokens and
  time; once the run ended, the script's log and its result (JSON shown
  as sections, text as markdown), or the error it failed with. Click a
  subagent for its whole conversation, read-only (Esc goes back). The tab
  long-polls (`GET /api/agents/{id}/workflows?wait=1`); the list asks
  `GET /api/workflows` every 3 seconds, on every joined machine.

The agent's hooks tell the daemon where its transcript is; fleet only
reads transcripts inside the CLI's own transcript folder (and, for
sandboxed agents, inside the sandbox home), so a sandboxed agent cannot
point the dashboard at other files on the server. Agents started before
this version are found by their session id.

Claude Code keeps a workflow run next to the session's transcript
(`<session>/subagents/workflows/<run>/`: a journal of the agents started
and finished, and each agent's transcript; the script in
`<session>/workflows/scripts/`). How a run ended is in the session's
transcript; the whole result and the script's log are in the task's output
file under `/tmp/claude-<uid>/`, which fleet reads only at the exact path
of that session and task. Symlinks there are not followed. Fleet reads
each of these files once and then only what is appended, so following a
run with dozens of agents costs a few hundred microseconds a poll.

### Several machines, one dashboard

Machines that hold the same **fleet key** manage each other's roots, and
run each other's agents, from their dashboards. `fleet web` prints the command for the other machines:

```
To manage other machines from this dashboard, start fleet on them with:

    fleet start --join 5b1e...
```

Run it once on each machine (it starts fleet there if needed, and only
stores the key if fleet already runs). From then on, the dashboard of any
of them lists the agents of all of them (tagged with their machine, polled
every 4 seconds), starts sessions on any of them, and its folder picker
can browse and add roots on all of them. A machine without the
key shows up as *not joined*, with the command to copy; the picker notices
when it has been run. No pairing is needed for this: pairing (`fleet pair`)
is only for the fleet CLI and native apps talking to a daemon over TLS.

How it works: the browser only talks to the dashboard it opened, with that
machine's admin token. That daemon asks the other one for a nonce and sends
the request signed with the key (HMAC-SHA256 over the target's server id,
the nonce, the method, the path and the body). The key never crosses the
network, a signature is good for one request to one daemon, and a machine
that only pretends to be a fleet learns nothing it can use. Only folder
listings, adding or removing roots, and the session routes (list, start,
chat, images, screen, input, answers, model, stop, workflows) are
forwarded.

- The key lives in `~/.fleet/fleet-key` (0600). `fleet web --rotate` does
  not change it. For a new key, delete the file and run `fleet web`
  again, then join the other machines again.
- The key is a password for all machines at once: whoever has it, or the
  admin token of any member, can add roots, list folders, read agent
  conversations and start agents (a shell included) on every member. Treat
  it like an SSH key that opens all of them.
- The other machines' dashboards must be reachable from this one (the
  `web` port on their LAN address).

## How tmux is used, and how cleanup works

- The daemon uses its own tmux server, `tmux -L fleet`, never your default
  one. Each agent is the only pane of a session named `fleet-<agent id>`.
  On the server you can look at them directly:
  `tmux -L fleet ls`, `tmux -L fleet attach -t fleet-a1b2c3`.
- Sessions are created with `remain-on-exit on`, so when an agent exits,
  its pane stays around long enough for the daemon to read the exit status
  (or the killing signal) and the last lines of output.
- A reconcile loop checks tmux every second. It records dead panes as
  EXITED (or FAILED, if the agent died with an error within about 3 seconds
  of starting) and then **kills the session**. Sessions that vanished are
  marked EXITED. Killing an agent kills its session right away.
- `exit-empty on`: when the last fleet session is gone, the tmux server
  exits.
- When the daemon starts, it re-adopts every agent whose session still
  exists and **kills every `fleet-*` session it has no record of**. Agents
  keep running while the daemon is stopped or restarted.
- Attaching (from the CLI or an app) starts a tmux client in a PTY on the
  server and streams it; detaching kills only that client.
- Two daemons with different `FLEET_HOME`s get different sockets by
  default, so they do not reap each other's sessions.

## Folder trust

Claude Code and Codex ask "Do you trust this folder?" the first time they
start in a project. Nothing reports that prompt through hooks, so fleet
watches the screen of each starting agent: while the prompt is up the agent
is `needs input` with a note saying so, until someone answers it in the
agent's terminal (`fleet attach`). Codex's "Hooks need review" prompt is
reported the same way.

A root added with `--trust` (`trust = true` in `config.toml`) skips the
prompt. Before starting an agent there, fleet records the trust the way the
CLI would if you had answered yes. The trusted folder is the agent's git
repository (the main checkout, which also covers its worktrees) or, outside
git, the agent's folder:

- **Claude Code**: fleet sets `hasTrustDialogAccepted` for that folder in
  `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/.claude.json`). It takes the
  lock Claude uses for that file, so running sessions keep their changes.
  The entry stays, as it would after answering yes yourself.
- **Codex**: fleet passes `-c projects={"<folder>"={trust_level="trusted"}}`
  for that launch only. `~/.codex/config.toml` is not changed.

Trusting a parent folder is not enough for either CLI: both look only at
the repository (or folder) itself. That is why fleet trusts each
repository as it is used, not the root. A trusted project may run its own
hooks, MCP servers and settings, so only use `--trust` for roots that hold
  code you trust. Existing roots can be edited in the dashboard, or in
  `config.toml` (then restart the daemon).

## Security model

What protects the daemon:

- **Transport.** Remote clients talk TLS 1.3 to a self-signed certificate.
  Clients pin the SHA-256 of that certificate (the server id), learned
  during pairing.
- **Pairing.** `fleet pair` creates a code of 8 Crockford-base32 characters
  (40 bits). It is single use, lives at most 5 minutes, dies after 5 wrong
  attempts, and is replaced by the next `fleet pair`. The client proves it
  knows the code with an HMAC bound to the certificate it saw and to its
  device key; the daemon proves it knows the code back.
- **Authentication.** Each device has an Ed25519 key and signs a fresh
  per-connection nonce plus the certificate hash. Revoking a device
  (`fleet devices revoke`) closes its connections and it must pair again.
- **Local socket.** `~/.fleet/fleet.sock` is mode 0600: any process of the
  same Unix user has full access without pairing.
- **Web dashboard.** Anyone who can reach its port sees agents, paths,
  roots and devices. Everything else needs the admin token from `fleet
  web`, which *unlock* also hands out for the fleet key (see
  [Web dashboard](#web-dashboard)): changing roots, listing
  folders, reading conversations and terminal screens, and starting,
  typing into and stopping agents. Like a paired device, the admin token
  is a shell on the server. Machines holding the same fleet key accept
  each other's signed requests for the same actions.

Honest limits:

- **A paired device has full control, which means a shell on the server.**
  There are no per-device permissions in v1. Any paired device can run the
  `shell` adapter, pass arbitrary extra arguments to agent CLIs (including
  flags that disable their permission prompts), type into any agent, add
  roots, pair more devices and revoke others. Pair only devices you would
  give an SSH login to.
- **Roots restrict where agents start, not what they can touch.** An agent
  runs as the daemon's user with that user's full permissions and
  environment (the claude adapter only removes `ANTHROPIC_API_KEY` and
  `CLAUDECODE`). It can read and write anything that user can, anywhere,
  unless it runs in a Docker sandbox (`--docker`), which confines it to
  its checkout and a home directory of its own.
- **A sandboxed agent can still reach the network,** and it can write the
  repository's git objects and refs (it commits). It cannot write
  `.git/config` or `.git/hooks`, but it could, for example, add a
  `.git/commondir` file to a pinned repository that points git on the host
  at a config it wrote. Treat a sandboxed agent's repository like one you
  cloned from a stranger until you have looked at it.
- **`[sandbox] auth` hands your login to the agent.** A sandboxed agent
  with the server's Claude or Codex credentials can use them like you, also
  from outside the container. Leave it off when you run code you do not
  trust.
- **`--clone` uses the daemon's git credentials.** Any paired device can
  make the daemon clone anything those credentials can read.
- **The pairing code is short.** The proof is an HMAC keyed with a 40-bit
  code. An active man in the middle on the LAN who captures a pairing
  exchange during the code's 5-minute window can brute-force the code
  offline and then pair with the real daemon. The defence is the
  fingerprint check: `fleet connect` refuses to send the proof until the
  server fingerprint matches `--fingerprint` or the user confirmed it, and
  native apps must do the same. That check only helps if people actually
  compare the fingerprint. **Pair on a trusted network**, and compare the
  fingerprint. A PAKE bound to both certificates would remove this attack;
  it needs protocol v2.
- **mDNS is unauthenticated.** Anyone on the LAN can advertise a fake
  daemon. Pinning makes this harmless for paired servers; during pairing
  the fingerprint check is what stops it.
- **The web admin token travels over plain HTTP.** Someone who can sniff
  the LAN can capture it from a browser's admin request and then do
  anything the daemon's user can: start a shell agent, read conversations,
  add roots (with trust) and list every folder.
  Rotate it with `fleet web --rotate`, and bind the dashboard to
  `127.0.0.1` on networks you do not trust. Unlocking a dashboard sends
  the fleet key over the wire in the clear, once; unlike the token, a
  captured fleet key can only be replaced by joining every machine again
  (delete `~/.fleet/fleet-key`, `fleet web`, `fleet start --join` on the
  others). With a fleet key, that token
  reaches every machine in the fleet through this dashboard. The requests
  between daemons are signed but also plain HTTP, so folder listings,
  conversations and what you type can be read on the wire.
- **The device key is the credential.** Whoever copies
  `~/.fleet/identity/device.key` (or an app's key) has that device's access
  until it is revoked.
- **LAN only.** Do not expose the listen port to the internet. Use a VPN
  such as WireGuard or Tailscale if you need remote access.

## Adapters

An adapter tells fleet how to launch one agent CLI and how to turn its hook
events into agent states. v1 ships `claude` (Claude Code), `codex` (Codex
CLI) and `shell` (your login shell, no status). See
[internal/adapter/README.md](internal/adapter/README.md) for how to write
one, and for what was verified about the Claude Code and Codex hook
behaviour.

## Status: v0

It works end to end on Linux: an end-to-end test drives the real binary
through run, send, attach, kill, worktrees, pairing, revocation and daemon
restart with re-adoption. Known gaps:

- **Codex "Hooks need review".** If you have untrusted hooks of your own,
  Codex shows this dialog at startup, also in trusted roots. The agent shows
  `needs input` until it is answered in the terminal.
- **Prompt detection reads the screen.** The trust and hook-review dialogs
  are recognized by their text (Claude Code 2.1.280, Codex 0.154.0). If a
  CLI rewords them, the agent shows `running` while it waits, as before.
- **Pairing uses a short code with a fingerprint check, not a PAKE.** See
  the security section. A PAKE needs protocol v2.
- **Shared tmux socket.** Two `FLEET_HOME`s that set the *same*
  `tmux_socket` in `config.toml` can still kill each other's agents as
  orphans. There is no lock on the socket name.
- **Stale socket file.** tmux does not delete `/tmp/tmux-<uid>/fleet` when
  it exits because the last session ended. Harmless.
- **`no-new-privileges` is not set on sandboxes.** Snap-packaged Docker's
  AppArmor profile makes every exec fail with it. Add
  `"--security-opt=no-new-privileges"` to `[sandbox] args` where it works.
- **Some Claude hooks not verified live.** The `PostToolUseFailure` and
  `PermissionDenied` handling was written from the CLI's schema. Whether
  answering "No" in a permission dialog fires one of them has not been
  checked.
- **Failure output from the wrong window.** For an agent that failed right
  after starting, the saved output comes from the session's active pane. If
  someone opened an extra window in that session, it may be the wrong one.
- **No resume.** No adapter advertises the `resume` capability yet.
  `fleet run claude -- --continue` works as a manual workaround.
- **No service files.** There is no systemd unit or launchd plist; use
  `fleet start` or write your own unit around `fleet daemon`.
- **Config edits by hand** need a daemon restart.
- **IPv4 only by default.** The default listen address is `0.0.0.0:7420`.
- **No native app yet.** The macOS app is the next step; the protocol guide
  for it is [docs/PROTOCOL.md](docs/PROTOCOL.md).

## License

MIT, see [LICENSE](LICENSE). The provider logos in
`internal/web/static/providers` come from
[lobe-icons](https://github.com/lobehub/lobe-icons) (MIT).
