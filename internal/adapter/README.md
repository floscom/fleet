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
   - Write generated files only into `req.StateDir`. The one exception is
     `req.TrustDir`, set in roots the user marked as trusted: record that
     folder as trusted wherever the CLI keeps trust, preferably per launch
     (Codex: a `-c` override), else in its own config under its own lock
     (Claude: `~/.claude.json`).
   - When `req.Sandbox` is set, the command runs in a container whose image
     provides the CLI: use the bare command name as `Argv[0]` and do not
     require the host binary. All paths in the request are the same inside
     the container. Per-user config the adapter writes (trust) goes into
     `req.Home`, the container's home directory, not the daemon user's.
   - If the CLI keeps its login in a file, implement `adapter.AuthProvider`
     (`AuthFile`, and `LockAuth` if the CLI locks it while writing) so
     `[sandbox] auth` can share the host login with sandboxes.
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
7. Startup dialogs (optional): if the CLI can block on a prompt before any
   hook runs, implement `adapter.PromptDetector`. `DetectPrompt` gets the
   visible screen of a starting agent and returns a short detail while the
   prompt is up. Match text that only the dialog shows.
8. Chat view (optional): if the CLI writes its conversation to a JSONL
   transcript, implement `adapter.Transcripter`: `TranscriptDir` (where
   the CLI keeps transcripts for a home directory; fleet reads nothing
   outside it), `FindTranscript` (by session id, for agents whose hooks
   never said where it is) and `ParseTranscript` (one line to
   `transcript.Entry` values: user, assistant, tool, result, note). Set
   `StateUpdate.Transcript` from the hook payload's `transcript_path`.
   Give a tool entry an `ID` only when its output follows in a separate
   result entry.
9. Dialogs (optional): implement `adapter.DialogDetector`. `DialogOpen`
   gets the visible screen and reports whether the CLI shows a dialog,
   where Enter picks the highlighted option. The daemon then does not
   press Enter after text typed from the dashboard, and drops dialogs
   hooks reported once the screen no longer shows one. Set
   `StateUpdate.Subagent` and `Call` so dialogs close on the right hooks.
10. Register it in `cmd/fleet`. Test argv construction, generated files
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
  `permission_prompt` means Claude is asking for permission. It follows
  the `PermissionRequest` for the same dialog and does not say who asks,
  so fleet ignores it.
- `AskUserQuestion` (Claude's multiple-choice questions) is a permission
  dialog: `PermissionRequest` fires with the questions in `tool_input`.
  Enter in it picks the highlighted option; typed text is dropped.
- Hooks fired inside a subagent carry `agent_id`. Background subagents keep
  calling tools while a dialog waits for the user, so their `PostToolUse`
  says nothing about the dialog. Their own permission dialogs show in the
  main terminal. Refusing a permission, or dismissing a question with
  Esc, fires no hook at all (no `PostToolUseFailure`, no `Stop`, and no
  `idle_prompt` later), so the daemon checks the screen for the dialog
  (`DialogDetector`). `SubagentStop` tells when a subagent is gone (it
  also fires for Claude's internal helper agents).
- `PermissionRequest` has no `tool_use_id`; its `tool_name` and
  `tool_input` match the call's `PreToolUse` / `PostToolUse`.
- On first launch in an untrusted directory, Claude shows a trust dialog
  whose default is "No, exit". No hook fires until someone answers it in
  the terminal. Trust lives in the global config file (`~/.claude.json`,
  or `$CLAUDE_CONFIG_DIR/.claude.json`) as
  `projects["<dir>"].hasTrustDialogAccepted`. Answering yes records the
  git root, or the directory outside git. A trusted directory covers the
  directories below it only down to the enclosing git root, so a trusted
  `~/code` does not cover the repository `~/code/api`. A trusted main
  checkout covers its linked worktrees.
- Claude rewrites that file with a locked read-modify-write; the lock is
  the directory `<file>.lock` (proper-lockfile, stale after 10 s).
- The internal variable `CLAUDE_CODE_SANDBOXED` also skips the dialog, but
  it trusts every folder and changes other behaviour; fleet does not use it.

### Codex

- Hooks set with `-c` belong to the "session flags" layer. Codex runs
  only trusted hooks. The adapter adds matching
  `hooks.state.<key>.trusted_hash` entries, so only fleet's hooks are
  trusted and `~/.codex` is never written.
- If the user has untrusted hooks of their own, Codex still shows its
  "Hooks need review" dialog.
- `SessionStart` fires lazily, together with the first turn.
- In new directories Codex asks whether to trust the directory. The default
  answer is yes. Trust is keyed on the git root (a trusted main checkout
  covers its worktrees) or the directory outside git; parent directories
  do not count.
- `-c 'projects={"<dir>"={trust_level="trusted"}}'` trusts a project for
  one launch, merged with the user's own `[projects]`. The dotted form
  `-c 'projects."<dir>".trust_level="trusted"'` has no effect.
- `check_for_update_on_startup=false` suppresses the blocking update
  prompt.
