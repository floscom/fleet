# Fleet — Research Notes

Pre-build research for a lean multi-agent **spawner/runner daemon**: runs on any
server, owns agent sessions, and lets a remote client drive them. Pluggable
adapters per agent CLI (claude, codex, …).

Design values: single static Go binary, no Electron, no runtime deps on the
server. Lean and fast — "like Zed."

**Status: research only. Nothing has been built.**

---

## Provenance

Every claim below is tagged:

- **[V]** — verified empirically on this machine, command shown
- **[D]** — documented by the vendor, not independently run here
- **[?]** — unverified assumption, flagged as such

Environment: Linux 6.8, Node v24.21.0, Python 3.12.3, Claude Code **2.1.261**,
Codex CLI present at `~/.local/bin/codex`. Researched 2026-09-21/22.

> Note: `claude` and `codex` live in `~/.local/bin`, which is not always on
> `PATH` for non-login shells. The daemon should resolve absolute binary paths
> rather than relying on `PATH`.

---

## 1. Claude Code control surface

### 1.1 One process model covers both lanes **[V]**

```bash
claude -p --input-format stream-json --output-format stream-json \
       --verbose --session-id <uuid-you-generate>
```

Write one JSON line per user message to stdin, read NDJSON events from stdout.
Tested across two turns in a single process:

```
[init]   session=6ef2c560… model=claude-sonnet-5
[turn 1] result='stored'
[turn 2] result='42'        ← same session_id, context retained
```

**Fire-and-forget is the same thing with stdin closed after turn 1.** There is
no need for two process models — interactive is a strict superset.

`--verbose` is mandatory with `--output-format stream-json`.

### 1.2 `init` and `result` fire per *turn*, not per process **[V]**

Turn 2 emitted its own `system/init`. Treat `result` as a **turn boundary**, not
as "the agent is finished." Session lifetime is a decision the daemon makes.

### 1.3 There is no way to message a `--bg` session **[V]**

Full subcommand list: `agents · attach · auth · doctor · gateway · import ·
install · logs · mcp · plugin · project · respawn · rm · setup-token · stop ·
ultrareview · update`.

No `claude send <id>`. The only input path into a background session is
`claude attach`, a TUI. Driving one from an app means puppeting a PTY.

**Consequence:** for an interactive claude agent the daemon *must* own the
process and hold the pipes. `--bg` is dispatch-and-observe only.

`claude agents --json` is still useful as a **read-only roster**, including
sessions the daemon did not start. Returns `pid`, `cwd`, `sessionId`, `name`,
`startedAt`, `status` (e.g. `idle`), `kind`. **[V]**

### 1.4 Permission routing — the hard part **[V]**

Attempted `--permission-prompts host` with no handler. The agent did **not**
prompt and did **not** hang — it silently auto-denied and continued:

```
=== result === success
permission_denials: [
  { "tool_name": "Bash",
    "tool_use_id": "toolu_011abEnsgxVdaJsLUDmqgDdm",
    "tool_input": { "command": "curl -s https://example.com/zzz" } } ]
```

Sending a bare `{"type":"control_request","request":{"subtype":"initialize"}}`
**is** answered by the CLI (it replies with the slash-command list), but is
**not** sufficient to start receiving `can_use_tool` control requests. The
capability declaration the TS SDK makes is more than this and is undocumented.

**Do not reverse-engineer the control protocol.** Use `--permission-prompt-tool`
**[D]**, which routes prompts to an **MCP tool** the daemon serves — ordinary
JSON-RPC, stable, documented. Confirmed present in `--help` **[V]**.

Useful safety property **[V]**: an unanswered prompt **denies and continues**
rather than wedging. A bug in the approval path costs a tool call, not a stuck
agent. The flip side is that an absent human silently *degrades* the agent
rather than pausing it — see §4.3.

Settings matter when testing: a first probe of `echo hello` was auto-allowed by
existing user permission rules. Isolate with `--setting-sources ""
--strict-mcp-config` to observe default behaviour. **[V]**

### 1.5 Auth **[V]**

Headless runs work on the Claude **subscription** — observed
`"apiKeySource": "none"` (OAuth, no API key).

- No browser on the box → `claude setup-token` → `CLAUDE_CODE_OAUTH_TOKEN` **[D]**
- An `ANTHROPIC_API_KEY` present in the env silently switches to **metered API
  billing**. The daemon should scrub or explicitly set it.

Policy **[D]**: OAuth/subscription is for the subscriber's own use of the
unmodified binary. Own machines, own login = fine. A multi-tenant hosted product
routing other people's work through one plan = not permitted; that path needs
API keys.

### 1.6 Rate limits are the real ceiling **[V]**

The stream emits `rate_limit_event`:

```json
{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.81,
 "unifiedWindows":{"five_hour":{"utilization":0.06,"resetsAt":1790031000},
                   "seven_day":{"utilization":0.81,"resetsAt":1790082000}}}
```

On a subscription this — not cost — is what stops a fleet. `total_cost_usd` in
the `result` event is notional. **Gate fleet concurrency on `rate_limit_event`.**

### 1.7 Latency **[V]**

Measured `ttft_ms` ≈ 1621, `duration_api_ms` ≈ 1524 on a trivial turn. Model
TTFT dominates end-to-end latency; daemon language is irrelevant to throughput.
Go's win here is **footprint and deploy**, not speed. Optimize accordingly:
stream from the first token, never block the UI on a turn completing.

### 1.8 Other flags worth knowing **[D]**

`--include-partial-messages` (token-level streaming) · `--include-hook-events` ·
`--forward-subagent-text` (otherwise subagent output is invisible) ·
`--json-schema` (structured final output) · `--max-budget-usd` · `-n <name>` ·
`--fork-session` · `--permission-mode {acceptEdits,plan,manual,…}`

State on disk: transcripts `~/.claude/projects/*/*.jsonl`, background jobs
`~/.claude/jobs/<short-id>/`.

### 1.9 SDKs **[D]**

Official Agent SDKs are **TypeScript and Python only** — no Go. A third-party
.NET SDK (`0xeb/claude-agent-sdk-dotnet`) does implement the control protocol
and bidirectional client, and is a better porting reference than the TS source
if the control protocol is ever needed.

---

## 2. Codex control surface

Codex is architecturally **ahead** of Claude Code for this use case. It is
already most of a remote spawner.

- `codex exec --json` — JSONL events; `--output-schema <FILE>` for structured
  final output; `-o/--output-last-message <FILE>` **[V, from --help]**
- **`codex queue --thread <uuid> --message <text>`** — inject a message into a
  **live** session from outside the process. Claude has no equivalent. **[V]**
- `codex app-server daemon` — managed daemon. Subcommands `bootstrap · start ·
  restart · stop · enable-remote-control · disable-remote-control · version`.
  `bootstrap` is described as *"Install durable local app-server management for
  **SSH-driven use**."* **[V]**
- `codex agents` — browse sessions on the shared local app-server daemon **[V]**
- **`--remote <ADDR>`** accepting `ws://`, `wss://`, `unix://`, plus
  `--remote-auth-token-env <ENV_VAR>` for a bearer token. Remote transport and
  auth are built in. `codex remote-control pair` issues short-lived pairing
  codes. **[V]**
- `codex app-server generate-json-schema --out <DIR>` — emits the **full
  protocol as JSON Schema**: ~689KB, with typed approval params
  (`ExecCommandApprovalParams`, `ApplyPatchApprovalParams`,
  `FileChangeRequestApprovalParams`, `CommandExecutionRequestApprovalParams`).
  **Codegen Go structs from this instead of hand-writing them.** **[V]**
  Also `generate-ts` for TypeScript bindings.
- Built-in managed **git worktree** per session, and a no-persist mode **[V]**
- `resume` / `fork` / `archive` / `delete` for session lifecycle **[V]**

**Strategic consequence:** do not rebuild for codex what codex already ships.
The value of this project is being the **one control plane across heterogeneous
agents**, not being a better codex daemon.

---

## 3. Capability matrix

| | **claude** 2.1.261 | **codex** |
|---|---|---|
| JSONL event stream | `-p --output-format stream-json` | `exec --json` |
| multi-turn, one process | ✅ `--input-format stream-json` | via app-server |
| **inject into live session externally** | ❌ none | ✅ `queue --thread` |
| session registry | `agents --json` (read-only) | `agents` / app-server |
| **remote transport built in** | ❌ | ✅ `--remote ws://…` + token env |
| **typed protocol spec** | ❌ undocumented control protocol | ✅ `generate-json-schema` |
| managed git worktree | roll your own | ✅ built-in |
| structured final output | `--json-schema` | `--output-schema` |
| approvals | MCP tool via `--permission-prompt-tool` | typed protocol requests |

### 3.1 Adapters must be capability-based, not uniform

The instinct — one uniform interface every agent implements — is wrong here. The
lowest common denominator is "spawn a process and scrape stdout," which discards
Codex's entire protocol and forces reimplementing remoting it already has.

Adapters should **advertise** capabilities; the core degrades gracefully:

```go
type Caps struct {
    Interactive  bool          // can accept a second message at all
    ExternalSend bool          // ...without the daemon owning the process
    Registry     bool          // can enumerate sessions it didn't spawn
    NativeRemote bool          // brings its own transport + auth
    Worktree     bool          // manages isolation itself
    Approvals    ApprovalKind  // none | mcpTool | protocol
}
```

`ExternalSend` is the load-bearing bit: `false` (claude) forces the daemon to
hold a process and its pipes; `true` (codex) lets the adapter be near-stateless.

### 3.2 Normalized event model (draft)

Thin common core, plus vendor passthrough so a client can render native detail
when it knows how:

```
session.started | session.ended
message.delta   | message.complete
tool.call       | tool.result
approval.requested | approval.resolved
usage | limit | error
raw{vendor, payload}
```

### 3.3 Scope trap: agent CLIs vs bare model APIs **[?]**

claude and codex ship their own **agent loop**. A bare model API (e.g. grok/xAI)
does not — adapting one means writing the loop, tool dispatch and approvals
yourself. That is a second product, not an adapter. **Confirm before promising a
grok adapter: does it ship an agent loop, or just model access?** Not installed
on this machine, so nothing here is verified about it.

---

## 4. Architecture conclusions

### 4.1 The daemon is an *owner*, not a communication layer **[V]**

Spawned a claude session, then `SIGKILL`ed its parent:

```
RESULT: child SURVIVED (orphaned, reparented)
    PID    PPID STAT   ELAPSED
1082117 1082106 Rl     00:00
```

The agent does **not** die with the process that started it. It is reparented
and keeps running — but its pipes died with the parent, so it is **unreachable
while still burning quota and still editing files**. Worse than a clean death.

Requirements that follow:

- persist child PIDs to disk; **reap orphans on daemon startup**
- the daemon generates `--session-id` itself, so deliberate resume is always
  possible (`-r <uuid>`); transcripts outlive the daemon on disk
- treat process lifetime as owned state, never as an implicit consequence of a
  connection

### 4.2 The client is usually disconnected — that is the whole point

If this were a pure relay, SSH would do. The value is holding the session while
the coder is away. So the daemon **persists** events rather than forwarding them:

- append-only event log per session (SQLite; `modernc.org/sqlite` is pure Go, no
  cgo, keeps the single-static-binary property)
- clients reconnect with a **cursor** and replay from it
- multi-client, reconnect, and history all fall out of this one decision

### 4.3 Approvals need a policy for an absent human **[V]**

Because unanswered prompts auto-deny and continue (§1.4), an offline coder does
not pause the agent — it silently makes the agent worse, and the reason is
invisible. Every session needs an explicit stance:

- auto-approve within its own worktree
- park-and-idle until a human returns
- deny

### 4.4 Isolation: configurable per agent

One field on the session record:

- `worktree` — daemon runs `git worktree add`, sets cwd there (codex can do this
  natively; claude needs it done for it)
- `pinned` — cwd is the live working dir; daemon must refuse a second agent there

### 4.5 Auth has two layers — only one is solved

**Agent auth: solved.** claude and codex are logged in locally on the box.

**Daemon auth: open, and the real exposure.** A port that spawns coding agents
with filesystem access is remote-code-execution-as-a-service. Cheapest solid
answer that costs nothing now: bind `127.0.0.1` and reach it over an **SSH
tunnel** — transport encryption and identity for free, on infrastructure already
in place. A bearer token is a reasonable second step. Do not let "auth isn't a
problem" carry over from the agent layer to the transport layer; retrofitting is
much more painful.

### 4.6 Sketch

```
cmd/fleetd/
internal/
  protocol/   normalized events; tagged union on "type"
  adapter/    Caps + Adapter/Session interfaces
    claude/   owns exec.Cmd; goroutine reads stdout, chan writes stdin
    codex/    app-server client; structs codegen'd from generate-json-schema
  approval/   MCP server for --permission-prompt-tool; pending-approval queue
  store/      SQLite event log (modernc.org/sqlite, pure Go)
  worktree/   git worktree add/remove; nil for pinned sessions
  web/        net/http + embed.FS; WS fan-out to connected clients
```

Goroutine per agent; one channel out, fanned to all connected clients.

---

## 5. Open questions

1. **Resume-after-daemon-restart has not been tested.** The whole ownership
   model rests on it. Sharpest next experiment: start a session, kill the
   daemon, restart, recover the agent via `--session-id` + `-r`. ~40 lines.
2. `--permission-prompt-tool` has not been exercised end-to-end — only confirmed
   to exist. Needs a real MCP server round-trip before it's load-bearing.
3. Does grok (or any third adapter) ship an agent loop? (§3.3)
4. Codex app-server protocol is marked **experimental** — how stable is the
   schema across releases? Codegen makes churn cheap, but versioning needs a
   stance.
5. Worktree cleanup policy: when is it safe to remove one with uncommitted work?

## 6. Reproducing

```bash
CLAUDE=~/.local/bin/claude
CODEX=~/.local/bin/codex

$CLAUDE --version && $CLAUDE --help          # 2.1.261; subcommands, flags
$CLAUDE agents --json                        # live roster
$CLAUDE -p "hi" --output-format stream-json --verbose   # event shapes

$CODEX --help && $CODEX exec --help && $CODEX queue --help
$CODEX app-server generate-json-schema --out /tmp/codex-schema
```
