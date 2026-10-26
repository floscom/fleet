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
through adapters) in tmux sessions on a server, and lets your other
machines on the same LAN start, watch, stop and type into them.

## Features

- **One Go binary** — `fleet daemon` is the server, every other subcommand
  is the CLI; no accounts, no cloud relay.
- **Real terminals, not a chat wrapper** — each agent runs its actual CLI
  in tmux; attach for the full TUI, or watch and type from a dashboard or
  any paired machine.
- **Web dashboard** — a chat-first UI for every session, on desktop or
  phone: conversations, images, files, workflows and questions, all live.
- **LAN pairing** — mDNS discovery, a one-time code, certificate pinning;
  or join several daemons into one fleet with a shared key.
- **Git-aware isolation** — each agent gets its own worktree by default, or
  runs pinned in a shared folder; `--clone` spins up a fresh checkout from
  any repo.
- **Docker sandboxing** — run agents in a container that only sees their
  own checkout, with your CLI logins shared in if you choose.
- **GitHub integration** — list and merge the pull requests agents open,
  from the CLI or the dashboard.
- **Push notifications** — know when an agent needs input, finishes, or
  fails.
- **Usage-limit auto-resume** — Claude Code and Codex sessions that hit a
  plan limit resume themselves once it resets.
- **Agents outlive the daemon** — restarts re-adopt running agents; tmux
  sessions are always cleaned up.
- **Updates itself** — the daemon installs new releases and restarts on
  them once no agent is working; `fleet update --auto off` turns that off.

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
The archives are also on the
[releases page](https://github.com/floscom/fleet/releases).

**Updates are automatic.** A few minutes after it starts, and every 6
hours after that, the daemon checks for a newer release. When it finds one,
it downloads the archive and verifies it against `checksums.txt`, like
install.sh. Then it replaces its own binary and restarts once no agent is
working, no question is waiting for an answer and no terminal is attached
through it. Running agents carry on in tmux and are re-adopted, like after
`fleet stop && fleet start`. The restart keeps the pid, arguments and log,
so it also works under systemd or launchd. `fleet update` updates right
away, `fleet update --check` only checks, and `fleet update --auto off`
(or `auto_update = false` in `config.toml`) turns automatic updates off.
Builds from source never update themselves. Neither does a binary in a
folder the daemon cannot write to (e.g. `/usr/local/bin` after a `sudo`
install); `fleet status` says so.

From source (needs Go 1.26):

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
fleet host                      # the machine: CPU, load, memory, disks
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
| `fleet status` | Daemon version, uptime, a line on the machine's load, memory and disks, server id, agent, root and device counts. |
| `fleet host [--json]` | The daemon's machine: OS, kernel, CPU, uptime, load, memory, swap and the disks of `/`, the home folder, the roots and `~/.fleet`. A line ends in `!` when the load passes one per CPU or memory, swap or a disk is 90% full. With `-H`, of a paired server. |
| `fleet version` | Print the version. |
| `fleet update [--check] [--auto on\|off]` | Install the newest release now (the daemon restarts on it once no agent works), only check for one, or turn automatic updates on or off. With `-H`, a paired server's daemon; without a running daemon, this binary. |
| `fleet roots` / `roots add <path> [--name N] [--adapters a,b] [--trust]` / `roots rm <name>` | Manage the folders agents may run in. `--trust`: see [Folder trust](#folder-trust). |
| `fleet browse [root[/path]] [-a]` | List folders inside a root, marking git repos and running agents. |
| `fleet adapters` | Adapters, whether their CLI is installed, version and features, and the models and efforts `fleet run --model/--effort` take. |
| `fleet run <adapter> [path] [flags] [-- agent args]` | Start an agent. `path` is `root:rel/path`, an absolute path, or (local only) a relative path; default `.`. Flags: `--name`, `--pinned`, `--worktree`, `--branch`, `--prompt`, `--model`, `--effort`, `--attach`, `--clone <repo>` (instead of a path), `--docker` / `--sandbox docker\|none`. |
| `fleet ls [-a] [--json]` | List agents (`-a` includes exited and failed ones). |
| `fleet attach <agent> [-r]` | Attach to an agent's terminal. Detach with Ctrl-\\. |
| `fleet send <agent> [text...] [--image FILE]... [--no-enter]` | Type text into an agent's terminal and press Enter; `--image` attaches image files first. |
| `fleet interrupt <agent>...` | Interrupt the turn agents are working on, as Esc does; they keep running. |
| `fleet kill <agent>... [--rm-worktree] [--force] [--forget]` | Stop agents; optionally remove their worktree or clone and history entry. |
| `fleet queue add\|ls\|rm\|pause\|resume <agent> ...` | Queue messages for an agent; the daemon sends the next one each time it finishes a turn. |
| `fleet note <agent> [text...] [-d]` | Leave a note on an agent (e.g. why it must stay open), shown on its dashboard row and in `fleet ls`; without text prints it, `-d` removes it. |
| `fleet sandbox build [--pull] [--tag T]` / `sandbox dockerfile` | Build the Docker image sandboxed agents run in (on the server) / print its Dockerfile. |
| `fleet watch` | Print agent state changes as they happen. |
| `fleet prs [agent] [-a] [--json]` | List the GitHub pull requests agents created, open ones unless `-a`. |
| `fleet merge <agent> [pr] [--squash\|--merge\|--rebase] [-d]` | Merge a pull request an agent created (its only open one unless `pr`, a number or URL) with the daemon host's `gh`; `-d` deletes the branch on GitHub. |
| `fleet git <agent> [status\|fetch\|pull\|push] [--rebase\|--merge] [--base] [-f]` | Show the git state of an agent's checkout, or fetch, pull (fast-forward unless `--rebase`/`--merge`; `--base` from the remote's default branch) or push it (`-f`: with lease) with git on the daemon host. |
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

`--root` joins the fleet with one command from inside a project:

```sh
cd ~/code/api
fleet start --root .            # start the daemon if needed, allow agents in ~/code/api
```

- If the daemon already runs, this only adds the folder; a folder that's
  already a root is left alone, so running it again is harmless.
- The root is named after the folder (`api`, or `api-2` if that's taken),
  saved in `config.toml`, and removed with `fleet roots rm api`.
- `--root` can repeat. `--trust` trusts the roots it adds, not ones that
  already existed.
- A relative path is resolved from the current folder, which must exist.

Once the daemon runs it's on the LAN, and every paired device can start
agents in the new root: `fleet -H studio run claude api: --prompt "..."`.
A device that never paired with this machine still only pairs once.

### Isolation

Inside a git repository, an agent gets its own **worktree** by default: a
new branch `fleet/<name>` under `~/.fleet/worktrees/<repo>-<hash>/<name>`,
so several agents can work on the same repo without stepping on each
other. `--pinned` runs the agent directly in the folder instead; several
pinned agents may share a folder. Outside git, agents are always pinned.
`fleet kill --rm-worktree` removes the worktree (keeping the branch)
unless it has uncommitted changes.

`--clone <repo>` starts from a fresh clone instead of a folder: GitHub
`owner/repo`, an `https://`/`ssh://` URL, or `git@host:org/repo`, cloned
with the daemon's own git credentials into `~/.fleet/clones/<name>`, then
checking out `--branch` (the remote branch if it exists, else a new one,
default `fleet/<name>`). `fleet kill --rm-worktree` deletes the clone too,
unless it has uncommitted or unpushed work.

### Docker sandbox

`--docker` (or `--sandbox docker`, or `default = "docker"` under
`[sandbox]`) runs the agent in a container instead of on the server, in
every mode above:

```sh
fleet sandbox build                                  # once, on the server: the fleet-agent image
fleet run claude code:api --docker                   # worktree of a local repo
fleet run claude code:notes --docker --pinned        # the folder itself, mounted read-write
fleet run claude --clone owner/repo --docker --prompt "fix issue 42"
```

Attach, send, `needs input` and exit codes all work as usual; the
container is removed when the agent exits or is killed (leftover
containers are also cleaned up at daemon startup).

The container sees, each at the same path as on the host:

- the agent's checkout — its worktree, clone, or (for `--pinned`) the
  folder — read-write;
- for a worktree, the repository's `.git` directory, read-write so the
  agent can commit; the rest of your checkout isn't visible;
- `.git/config`, `.git/hooks` and a worktree's `.git` file, read-only;
- a home directory, `/home/fleet`, kept across containers (log in once
  and it sticks), or the server's own login shared in (below);
- a socket that only accepts this agent's hook events.

**Sharing the server's login.** `auth = ["claude", "codex"]` under
`[sandbox]` gives sandboxed agents the Claude Code / Codex logins of the
daemon's user, copied into the sandbox home rather than bind-mounted
(the CLIs replace credential files atomically, which a mount doesn't
survive) and kept in sync as tokens rotate. A missing host login is never
recreated from a sandbox. macOS keeps Claude Code's login in the Keychain,
so there's nothing to share there.

The container runs as your uid:gid with all capabilities dropped. Your
git name and email, and the env vars in `[sandbox] env` (default
`CLAUDE_CODE_OAUTH_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`), are passed in.
Folder trust prompts are skipped, since a sandbox's own settings stay in
the container. The image ships Claude Code, Codex, git, gh, ripgrep, Node
and Python; `fleet sandbox dockerfile` prints its Dockerfile to build your
own `FROM fleet-agent`. Update agent CLIs with `fleet sandbox build --pull`.

**Docker installed as a snap** (Ubuntu's default) can't see `/tmp` or
hidden folders such as `~/.fleet`; fleet checks and tells you — set
`dir` under `[sandbox]` to a visible folder such as `~/fleet-sandbox`.
Anything you mount (`--pinned`, a worktree's `.git`) must be visible to
Docker too.

The sandbox protects the server from what the agent runs. It does not
make the agent's output trustworthy: review what it commits before
building or running it on the host.

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

# macOS: keep the machine from sleeping (caffeinate -i -s) while an agent
# works, runs workflows in the background, or waits up to 6h for its usage
# limit to reset to be resumed. Closing a laptop's lid still sleeps it on
# battery. Default true; no effect on other systems.
stay_awake = true

# Install new fleet releases and restart on them once no agent works (see
# "Install"). Default true. `fleet update --auto off|on` sets it at runtime.
auto_update = true

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

A live dashboard at `http://SERVER:7421/` — a chat app for your sessions.
On desktop the sessions sit in a sidebar, grouped by folder (or machine,
or not at all), and the open one fills the rest; on a phone the list is
the home screen and a session fills the screen. **Devices** shows the
machines of the fleet: health, agent CLIs, plan usage and roots; elsewhere
a dot per machine shows whether it's online. The page updates over a
WebSocket as agents start, change state or exit, and follows the device's
light/dark mode (or the one picked in Settings).

**Viewing needs nothing; managing needs the admin link.** Run `fleet web`
on the server:

```
$ fleet web
Admin links for the fleet dashboard on studio (open one once per browser):

    http://192.168.1.20:7421/#token=3f9c...
    http://localhost:7421/#token=3f9c...
```

Open one in the browser you manage the fleet from — or click **Unlock**
and paste the fleet key (from `fleet start --join KEY`) or the admin token
from the link; the same key unlocks every machine in the fleet, no
`fleet web` run there needed. That browser can then:

- **Manage folders** — *Add folder* on Devices opens a file picker over a
  machine's filesystem (walk in, type to filter, paste a path, show hidden
  folders); existing roots and git repos are marked. *New folder* creates
  and trusts a folder in one step. A folder's ✎ edits its name, agent
  restrictions and trust; roots can also be removed. None of this affects
  agents already running.
- **Run sessions** — start Claude Code, Codex or a shell, chat with it,
  answer it, type into it and archive it, on this machine or any other in
  the fleet (see [Sessions in the browser](#sessions-in-the-browser)).
- **See plan usage and host health** — Devices lists, per machine, whether
  Claude Code and Codex are installed and how much of the account's plan
  limits are used (polled from the provider at most every 45s), plus OS,
  CPU, memory, swap, disk and load, with a meter that turns amber/red near
  capacity.
- **Jump to other fleets** — every other machine on Devices links to its
  own dashboard, which has its own admin token (unlock it there too).

Changes apply at once, rewrite `config.toml`, and show up in the CLI
(`fleet roots`) and every open dashboard.

- **The token.** `fleet web` creates `~/.fleet/web-token` (0600) and
  prints it in the link's fragment, which browsers never send to the
  server; there's no cookie, so other sites can't act as you. `fleet web
  --rotate` revokes it everywhere at once (no restart needed); *Sign out*
  in Settings forgets it in one browser only.
- **Plain HTTP.** The token crosses the network in the clear on each admin
  request. Use a trusted network, Tailscale/WireGuard, or an SSH tunnel
  (`ssh -L 7421:127.0.0.1:7421 studio`) and keep `web = "127.0.0.1:7421"`.
- Requests whose `Host` doesn't name this machine get 403 (DNS rebinding
  protection), the WebSocket only accepts same-origin pages, and a strict
  Content-Security-Policy is sent.
- `--web ADDR` / `web` in `config.toml` sets the address, `off` disables
  it; a busy port just logs a warning and runs without it.
- **Keys.** **N** starts a session, **/** searches, **Ctrl+1–9** jump to
  the first nine sessions. In a session, **Alt+A** archives or deletes,
  **Ctrl+C** (no text selected) interrupts the turn, **Esc** closes a
  dialog or goes back. Settings lists them all.
- Installable to a phone's home screen (manifest, launch screens) and
  cached by a service worker, so the dashboard opens instantly — with the
  sessions and the conversations it showed last, even while the daemon is
  out of reach — and refreshes itself after a daemon upgrade. The app's
  badge counts the sessions that need you, and on Android images, videos,
  PDFs and text can be shared into it as a new session. Everything is embedded
  in the binary — no build step (`internal/web/static`; see `DESIGN.md`).

### Notifications

*Push notifications on this device* (Settings) subscribes the browser to
Web Push: the daemon sends one when an agent needs input, is done (idle
20s, no background runs) or fails, and tapping it opens that session.
Works in Safari (macOS 13+, iOS/iPadOS 16.4+ once added to Home Screen),
Chrome, Edge and Firefox — no Apple developer account or relay, the daemon
signs with its own VAPID key (`~/.fleet/push-key`) and posts straight to
each browser's push service.

Browsers only offer push to **HTTPS** pages (or localhost). With
Tailscale: enable HTTPS certs for the tailnet, `tailscale serve --bg 7421`
on the server, then open `https://SERVER.TAILNET.ts.net/` — on iPhone, add
that to the Home Screen first.

### Usage limits

When a Claude Code or Codex session stops on a plan limit (5-hour window,
week, ...), the daemon reads the reset time from the transcript (or, if
that's missing, the account's usage) and types `continue` a minute after
it resets. The row shows *Usage limit · resumes 22:58* with **Resume now**
and **Turn auto-resume off** (or `[limits] auto_resume = false` globally),
and a notification fires. It does nothing while the agent is still
working, and gives up — with a notification — after 3 resumes the session
never reacted to, or 4 limits within an hour.

### Sessions in the browser

Click a session in the sidebar to open its chat; ones waiting on you are
listed on top under *Needs you*. *New session* (**N**, Ctrl/⌘+K, or a
folder/machine's **+**) opens the starter: machine (if more than one),
folder, checkout mode, agent (Claude Code, Codex or shell) with its model
and effort, then **Start**; *Options* folds sandbox, post-start action,
name and branch. *New folder* in the folder picker creates and trusts a
folder anywhere on the machine. Ctrl/⌘+Enter starts; Ctrl/⌘+Shift+Enter
starts and opens the terminal too.

Once a session is open:

- **Chat** — messages are read straight from the transcript the CLI
  itself writes, so nothing here is reinterpreted; tool calls between two
  messages fold into one line (*Read 3 files, ran a command · 42s*) you
  can expand for inputs and outputs (edits as a diff).
- **Images** — pasted into a message, or returned by a tool (a file read,
  a screenshot), show as thumbnails; the panel's *Media* tab collects them
  all (Claude Code only, for now).
- **Videos and PDFs** — attach, paste or drop into a message or a new
  session's prompt (any agent, up to 5, 4 GB each); they upload in the
  background with progress shown, and the agent gets a `Video: <path>` or
  `PDF: <path>` to open with its own tools.
- **Files** — the panel's *Files* tab lists what the agent wrote and what
  else changed in its folder, newest first, with a search box; click one
  to preview (images/video/audio play, markdown renders, code is
  highlighted), save it, or send it to another machine in the fleet.
- **Questions** — Claude's AskUserQuestion shows as a form above the
  message box: pick an option (or several, where allowed), type your own,
  add a note. Sending a chat message instead has Claude read it, same as
  *Chat about this* in the terminal.
- **Send** — types into the terminal and presses Enter (Shift+Enter for a
  new line); Claude Code and Codex queue it while the agent is working.
- **Send images** — paperclip, paste or drop (up to 10 per message); large
  photos are scaled down and HEIC becomes JPEG automatically. From the
  CLI: `fleet send <agent> --image a.png "what changed?"`.
- **Stop a turn** — *Stop* (or Ctrl+C with no text selected) interrupts
  like Esc in the terminal; the agent keeps running. CLI: `fleet interrupt
  <agent>`.
- **Queue** — the queue button beside *Send* (or Alt/⌥+Enter) lines a
  message up instead; the daemon sends the next one each time the agent
  finishes a turn with no subagent or workflow left running, even with
  the dashboard closed. × removes one; *Stop* pauses the queue, *Resume*
  continues. Images can't be queued. CLI: `fleet queue add <agent> <text>`.
- **Terminal** — permission prompts, trust dialogs and menus aren't part
  of the transcript, so a card above the message box shows the terminal's
  tail with answer keys (Esc, arrows, Tab, Enter, 1-6, Ctrl-C) whenever one
  is up; the panel's *Terminal* tab shows the whole screen. `fleet attach`
  still works for the full thing.
- **Pull requests** — when an agent opens one (`gh pr create`), a chip
  (*#12 open*) appears on its row and header, a notification fires, and
  the git button lists state, checks, review, size and **Merge** (pick
  squash/merge/rebase, optionally delete the branch). CLI: `fleet prs`,
  `fleet merge`.
- **Git** — the header shows the checkout's branch, ahead/behind counts
  and uncommitted files; click it for *Fetch*, *Pull* (fast-forward,
  rebase or merge) and *Push* (force-with-lease if it diverged). Runs on
  the daemon host with that user's git credentials. CLI: `fleet git
  <agent> [fetch|pull|push]`.
- **Notes** — leave a note on a session (e.g. "customer is testing the
  preview, keep it open") from its row or header; survives archiving and
  restarts. CLI: `fleet note <agent> [text]`.
- **Archive** (header menu, Alt+A, or a left swipe on a phone) ends the
  agent like `fleet kill` (keeping the worktree) and moves it to
  *Archived*, chat still readable; *Delete* removes a finished one.
- **Workflows** — a Claude Code Workflow run (many subagents in phases, in
  the background) shows live on the agent's row and as a card in the chat;
  *Open run* shows every subagent's phase, current tool call, tokens and
  time in the panel's *Workflow* tab, and the script's result once it ends.

fleet only ever reads transcripts inside the CLI's own transcript folder
(or, for sandboxed agents, inside the sandbox home), so an agent can't
point the dashboard at other files on the server.

## Several machines, one dashboard

Machines holding the same **fleet key** manage and run each other's
agents from any of their dashboards. `fleet web` prints the join command
for the others:

```
To manage other machines from this dashboard, start fleet on them with:

    fleet start --join 5b1e...
```

Run it once per machine (starts fleet there if needed; only stores the key
if fleet already runs). From then on, any of their dashboards lists all of
their sessions, starts sessions on any of them, and browses/adds roots on
all of them from Devices — found via mDNS on the LAN and, with
[Tailscale](https://tailscale.com) running, across the tailnet too (their
dashboard must be reachable on its LAN or tailnet address). No pairing is
needed for this; `fleet pair`/`connect` is only for the CLI and native
apps talking to a daemon over TLS.

How it works: the browser only ever talks to the dashboard it opened. That
daemon asks another one for a nonce and signs the request with the fleet
key (HMAC over the target's server id, the nonce, method, path and body) —
the key itself never crosses the network, and a signature is only good for
one request to one daemon. Only folder listings, adding/removing roots and
the session routes are forwarded.

- The key lives in `~/.fleet/fleet-key` (0600); `fleet web --rotate`
  doesn't change it. For a new key, delete the file, run `fleet web`
  again, and rejoin every machine.
- The key is a password for the whole fleet: whoever has it, or any
  member's admin token, can add roots, read conversations and start a
  shell on every machine holding it. Treat it like an SSH key that opens
  all of them.
- Inter-daemon requests are signed but sent over plain HTTP, so folder
  listings, conversations and what you type can be read on the wire.

## How tmux is used, and how cleanup works

- The daemon runs its own tmux server, `tmux -L fleet`, never your default
  one. Each agent is the only pane of a session named `fleet-<agent id>`;
  inspect directly with `tmux -L fleet ls` / `attach -t fleet-a1b2c3`.
- Sessions are created with `remain-on-exit on`, so a dead agent's pane
  stays around long enough for the daemon to read its exit status and last
  output.
- A reconcile loop checks tmux every second: it records dead panes as
  EXITED (or FAILED, if the agent died with an error within ~3s of
  starting) and kills the session. A vanished session is marked EXITED.
  Killing an agent kills its session right away.
- `exit-empty on`: the tmux server itself exits once the last fleet
  session is gone.
- On startup, the daemon re-adopts every agent whose session still exists
  and **kills every `fleet-*` session it has no record of**. Agents keep
  running while the daemon is stopped or restarted.
- Attaching starts a tmux client in a PTY and streams it; detaching kills
  only that client.
- Different `FLEET_HOME`s get different tmux sockets by default, so they
  don't reap each other's sessions.

## Folder trust

Claude Code and Codex ask "Do you trust this folder?" the first time they
start in a project. Nothing reports that prompt through hooks, so fleet
watches the screen of a starting agent: while the prompt is up the agent
is `needs input` with a note saying so, until it's answered in the
terminal (`fleet attach`). Codex's "Hooks need review" prompt is handled
the same way.

A root added with `--trust` (`trust = true` in `config.toml`) skips the
prompt — before starting an agent there, fleet records the trust the way
the CLI would if you'd answered yes yourself:

- **Claude Code**: fleet sets `hasTrustDialogAccepted` for that folder in
  `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/.claude.json`), taking the same
  lock Claude uses, so running sessions keep their changes.
- **Codex**: fleet passes `-c projects={"<folder>"={trust_level="trusted"}}`
  for that launch only; `~/.codex/config.toml` is never changed.

Trust follows the git repository (the main checkout, covering its
worktrees) or, outside git, the agent's folder — not the root above it,
since neither CLI looks further up. A trusted project can run its own
hooks, MCP servers and settings, so only use `--trust` for roots you
actually trust. Existing roots can be edited in the dashboard or in
`config.toml` (then restart the daemon).

## Security model

What protects the daemon:

- **Transport** — TLS 1.3 to a self-signed certificate, pinned by its
  SHA-256 (the server id) during pairing.
- **Pairing** — `fleet pair` makes an 8-character, single-use code (40
  bits, 5-minute lifetime, dies after 5 wrong attempts, replaced by the
  next `fleet pair`). The client proves it knows the code with an HMAC
  bound to the certificate and its device key; the daemon proves the same
  back.
- **Authentication** — each device has an Ed25519 key and signs a fresh
  per-connection nonce plus the certificate hash. `fleet devices revoke`
  closes its connections right away and it must pair again.
- **Local socket** — `~/.fleet/fleet.sock` is mode 0600: any process of
  the same Unix user has full access, no pairing needed.
- **Web dashboard** — read-only to anyone who can reach the port; the
  admin token (or the fleet key, via *Unlock*) is needed to change roots,
  read conversations/terminals, or start and control agents. Machines
  holding the same fleet key accept each other's signed requests for the
  same actions.

Honest limits:

- **A paired device, or the admin token, is a shell on the server.** No
  per-device permissions in v1 — any of them can run the `shell` adapter,
  pass arbitrary flags to agent CLIs (including ones that disable their
  own permission prompts), add roots, pair more devices and revoke others.
  Only pair devices, or hand out the admin token, as you would an SSH login.
- **Roots restrict where agents start, not what they can touch.** Outside
  Docker, an agent runs as the daemon's user with that user's full
  permissions and environment (the claude adapter only strips
  `ANTHROPIC_API_KEY` and `CLAUDECODE`).
- **A sandboxed agent can still reach the network,** and can write the
  repository's git objects and refs (it commits). It can't write
  `.git/config` or `.git/hooks` directly, but could, say, add a
  `.git/commondir` pointing git on the host at a config it wrote. Treat a
  sandboxed agent's repository like one cloned from a stranger.
- **`[sandbox] auth` hands your login to the agent** — including from
  outside the container. Leave it off for code you don't trust.
- **`--clone` uses the daemon's git credentials** — any paired device can
  make the daemon clone anything those credentials can read.
- **The pairing code is short.** An active man-in-the-middle who captures
  a pairing exchange within its 5-minute window could brute-force it
  offline and then pair for real. The actual defence is the fingerprint
  check: `fleet connect` refuses to send its proof until the server
  fingerprint matches `--fingerprint` or is confirmed, and native apps must
  do the same — but only if people actually compare it. **Pair on a
  trusted network**, and compare the fingerprint. A PAKE bound to both
  certificates would remove this attack; it needs protocol v2.
- **mDNS and `/api/hello` are unauthenticated.** Anyone on the LAN or
  tailnet can claim to be a daemon; pinning makes this harmless once
  paired — the fingerprint check is what matters during pairing.
- **The web admin token travels over plain HTTP,** sniffable on the LAN,
  and gives anyone who captures it the same access as the daemon's user.
  Rotate it with `fleet web --rotate`, and bind to `127.0.0.1` on networks
  you don't trust. Unlocking a dashboard sends the fleet key over the wire
  once, in the clear; a captured fleet key can only be replaced by
  rejoining every machine (delete `~/.fleet/fleet-key`, `fleet web`,
  `fleet start --join` on the others). Inter-daemon requests are signed
  but also plain HTTP.
- **The device key is the credential.** Whoever copies
  `~/.fleet/identity/device.key` (or an app's key) has that device's
  access until it's revoked.
- **LAN only.** Don't expose the listen port to the internet; use a VPN
  such as WireGuard or Tailscale for remote access.

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
