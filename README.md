# fleet

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

Requirements on the server: Linux (macOS builds too, but is less tested),
tmux (tested with 3.4), git (for worktrees), and the agent CLIs you want to
run. Building needs Go 1.22.

```sh
make build                      # -> bin/fleet (static, CGO_ENABLED=0)
install -m 755 bin/fleet ~/.local/bin/fleet
```

Other targets: `make test`, `make vet`, `make lint` (vet, `buf lint`,
gofmt), `make proto` (regenerate `gen/fleetv1` with buf), and `make cross`
(`bin/fleet-{darwin,linux}-{arm64,amd64}`).

Install the same binary on every machine you want to control the server
from.

## Quick start

On the server:

```sh
fleet start                     # daemon in the background, log in ~/.fleet/daemon.log
fleet roots add ~/code          # allow agents in ~/code (root name "code")
fleet adapters                  # which agent CLIs were found
fleet run claude code:api --attach   # try it locally; detach with Ctrl-\
fleet pair                      # one-time code + server fingerprint for a new device
```

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
| `fleet daemon [--listen ADDR\|off] [--no-mdns]` | Run the daemon in the foreground (logs to stderr). |
| `fleet start [--listen ...] [--no-mdns]` | Start the daemon in the background. |
| `fleet stop [--force]` | Stop the daemon. Agents keep running in tmux. |
| `fleet status` | Daemon version, uptime, server id, agent, root and device counts. |
| `fleet version` | Print the version. |
| `fleet roots` / `roots add <path> [--name N] [--adapters a,b]` / `roots rm <name>` | Manage the folders agents may run in. |
| `fleet browse [root[/path]] [-a]` | List folders inside a root, marking git repos and running agents. |
| `fleet adapters` | Adapters, whether their CLI is installed, version and features. |
| `fleet run <adapter> [path] [flags] [-- agent args]` | Start an agent. `path` is `root:rel/path`, an absolute path, or (local only) a relative path; default `.`. Flags: `--name`, `--pinned`, `--worktree`, `--branch`, `--prompt`, `--attach`. |
| `fleet ls [-a] [--json]` | List agents (`-a` includes exited and failed ones). |
| `fleet attach <agent> [-r]` | Attach to an agent's terminal. Detach with Ctrl-\\. |
| `fleet send <agent> <text...> [--no-enter]` | Type text into an agent's terminal and press Enter. |
| `fleet kill <agent>... [--rm-worktree] [--force] [--forget]` | Stop agents; optionally remove their worktree and history entry. |
| `fleet watch` | Print agent state changes as they happen. |
| `fleet pair [--ttl 5m]` | Create a one-time pairing code (at most 5 minutes). |
| `fleet connect [addr\|name] --code CODE [--fingerprint HEX] [--name N]` | Pair this machine with a remote daemon. |
| `fleet discover [--timeout 2s]` | List daemons on the LAN. |
| `fleet servers` / `servers rm <name\|id>` | Daemons this machine is paired with. |
| `fleet devices` / `devices revoke <id\|name>` | Devices paired with the daemon. |
| `fleet hook ...` | Internal: called by agent hooks. |

Agents are referred to by id (`a1b2c3`) or name (`claude-api-1`, or the
`--name` you gave).

### Isolation

Inside a git repository an agent gets its own **worktree** by default: a
new branch `fleet/<name>` checked out under
`~/.fleet/worktrees/<repo>-<hash>/<name>`, so several agents can work on
the same repo without stepping on each other. `--pinned` runs the agent
directly in the folder instead; only one pinned agent may run per folder.
Outside git, agents are always pinned. `fleet kill --rm-worktree` removes
the worktree (the branch is kept) unless it has uncommitted changes.

## Configuration

Everything lives in `FLEET_HOME` (default `~/.fleet`, mode 0700):

| Path | Content |
|------|---------|
| `config.toml` | configuration (below) |
| `fleet.sock` | local control socket (0600) |
| `daemon.pid`, `daemon.log` | pid of the running daemon; log when started with `fleet start` |
| `identity/` | server TLS key and certificate, this machine's device key (0600) |
| `devices.json` | devices paired with this daemon |
| `servers.json` | daemons this machine has paired with |
| `agents.json`, `agents/<id>/` | agent registry and per-agent adapter files |
| `worktrees/` | git worktrees created for agents |

`config.toml`, with every key:

```toml
# Name shown to clients and in mDNS. Default: the hostname.
name = "studio"

# TCP address for paired devices (TLS). Default "0.0.0.0:7420".
# "off" or "" disables remote access; the CLI on the server still works.
listen = "0.0.0.0:7420"

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

# Per-adapter overrides (claude, codex, shell).
[adapter.claude]
binary = "/home/flo/.local/bin/claude"   # default: PATH, then well-known dirs such as ~/.local/bin
args = ["--model", "opus"]               # added to every launch, before `fleet run ... -- args`
```

The daemon reads `config.toml` at startup. Changes made through the CLI
(`fleet roots add/rm`) apply immediately and rewrite the file (comments are
not kept). Restart the daemon after editing the file by hand. Command-line
`--listen` and `--no-mdns` override the file.

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
  `CLAUDECODE`). It can read and write anything that user can, anywhere.
  Sandboxing is up to the agent CLI's own permission system.
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

- **Folder trust dialogs.** Claude Code and Codex ask whether to trust a
  folder the first time they run in it. No hook fires until someone answers
  in the terminal, so the agent shows RUNNING instead of IDLE. Claude's
  default answer is "No, exit". Options not built yet: pre-trusting
  configured roots (for Claude, in `~/.claude.json`; for Codex, passing a
  per-project trust override on opt-in), or making the app show this state
  clearly.
- **Codex "Hooks need review".** If you have untrusted hooks of your own in
  `~/.codex/hooks.json`, Codex shows this dialog at startup. Not exercised.
- **Pairing uses a short code with a fingerprint check, not a PAKE.** See
  the security section. A PAKE needs protocol v2.
- **Shared tmux socket.** Two `FLEET_HOME`s that set the *same*
  `tmux_socket` in `config.toml` can still kill each other's agents as
  orphans. There is no lock on the socket name.
- **Stale socket file.** tmux does not delete `/tmp/tmux-<uid>/fleet` when
  it exits because the last session ended. Harmless.
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
