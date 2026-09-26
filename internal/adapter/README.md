# Adapters

An adapter tells fleet how to launch one agent CLI and how to read its hook
events. The agent always runs its own interactive TUI in a tmux session. The
daemon streams that terminal to paired apps. The adapter never starts a
process itself.

| package  | agent       | activity state source                              |
|----------|-------------|----------------------------------------------------|
| `claude` | Claude Code | hooks in a generated `--settings` file             |
| `codex`  | Codex CLI   | hooks passed as `-c hooks.<Event>=...` overrides   |
| `shell`  | `$SHELL -l` | none (the simplest example)                        |

Shared helpers:

- `detect`: finds the binary (override, then `PATH`, then well-known dirs
  such as `~/.local/bin`, which is often missing from a service's `PATH`).
  It runs `<bin> --version` with a 3s timeout and caches the result.
- `hookcmd`: builds the `fleet hook` command line and summarizes tool calls
  for `StateUpdate.Detail`.

## Writing an adapter

1. Create `internal/adapter/<id>/` with `New(...) adapter.Adapter`.
2. `ID`, `DisplayName`, `Capabilities`: `ID` is the stable protocol id. Only
   advertise capabilities you implement.
3. `Detect`: embed a `*detect.Binary{Name: "<exe>", Override: override}`
   and forward to it.
4. `Launch`: return the argv, with an absolute `Argv[0]` from
   `Binary.Find()`. Guidelines:
   - Put config `extraArgs` first, then `req.ExtraArgs`, then `--` and the
     prompt. The `--` keeps a prompt that starts with `-` from being read
     as a flag.
   - Write generated files only into `req.StateDir`.
   - Use `UnsetEnv` for variables that change the agent's behaviour behind
     the user's back, such as `ANTHROPIC_API_KEY`, which switches Claude
     Code to metered billing.
   - Set `SessionID` if you can choose it up front, as `claude --session-id`
     does.
5. Hooks (optional): configure the agent to run
   `hookcmd.Command(req.FleetBinary, req.AgentID, "<id>", "<Event>")`.
   `fleet hook` is expected to pass the agent's JSON payload through as
   `HookEvent.Payload`. The payload normally arrives on stdin. Codex's
   legacy `notify` sends it as an extra trailing argument instead.
   The hook must print nothing and exit 0. Claude Code and Codex add a
   `UserPromptSubmit`/`SessionStart` hook's stdout to the model context, and
   exit code 2 blocks the prompt.
6. `HandleHook`: map events to `WORKING` / `IDLE` / `NEEDS_INPUT`. Put
   something short in `Detail`. Copy the agent's session id into
   `SessionID`. Return `ok=false` for anything you don't recognize.
7. Register it in `cmd/fleet`. Test argv construction, generated files
   (parse them back), and `HandleHook` with payloads captured from the
   real CLI.

## Verified agent behaviour (claude 2.1.280, codex 0.154.0)

### Claude Code

- `--settings <file>` merges with the user's settings. Hook lists from
  every source run; this was checked with a project hook and a `--settings`
  hook on the same event, and both fired.
- `SessionStart` fires at startup with `session_id` equal to our
  `--session-id`.
- `Notification` has `notification_type`. `idle_prompt` means "Claude is
  waiting for your input" and arrives about 60s after a turn.
  `permission_prompt` means Claude is asking for permission.
- On first launch in a directory where neither the directory nor any
  parent is trusted, Claude shows a trust dialog whose default is
  "No, exit". No hook fires until someone answers it in the terminal.

### Codex

- Hooks set with `-c` belong to the "session flags" layer. Codex runs
  only trusted hooks. The adapter adds matching
  `hooks.state.<key>.trusted_hash` entries, so only fleet's hooks are
  trusted and `~/.codex` is never written.
- If the user has untrusted hooks of their own, Codex still shows its
  "Hooks need review" dialog.
- `SessionStart` fires lazily, together with the first turn.
- In new directories Codex asks whether to trust the directory. The default
  answer is yes.
- `check_for_update_on_startup=false` suppresses the blocking update
  prompt.
