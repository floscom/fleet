# Design direction

The web dashboard is a chat app for coding agents, no longer a
terminal-styled overview page. It lives in `internal/web/static/`:
`index.html` (the shell and the icon sprite), `app.css` (tokens and every
component), `theme.js` (applies the theme before the first paint) and
`app.js` (the app).

## Context

- Product and audience: fleet's web app, used by a developer who runs Claude
  Code, Codex and shells on several machines and follows them from a desktop
  browser and from a phone (home-screen app, push notifications).
- Main task: see which sessions need you, talk to them, and start new ones.
  Machines, folders and paired apps are background facts.
- Character: calm, polished chat; native-app feel; fast. It must not feel like
  a terminal toy (no ASCII art, no monospace everywhere, no neon on black).
- Foundation: the FLEET wordmark in block letters (figlet "ANSI Shadow", as
  in the old header), the app-icon glyph (three blocks in a V formation,
  emerald lead) and emerald as the brand color; vanilla JS and plain CSS,
  no framework, no build step; CSP `script-src 'self'; style-src 'self'`
  (no inline scripts, no `style=""` attributes).
- References (behavior, not looks): desktop coding-agent chat apps put
  sessions in a left sidebar, the conversation on the right and the prompt
  box at the bottom, with agent, model and effort inside the prompt box.
- Fidelity: the working app (a click dummy with sample data came first).
- Dials: ENERGY 2 / RHYTHM 1 / MOTION 2.
- Design read: developer following many coding agents on desktop and phone;
  see who needs you and answer; calm, polished chat; 2 / 1 / 2.

## Composition

- Opening: desktop shows the session list and, with nothing open, the new
  session composer (prompt first). Phones open on the session list.
- Reading flow: sessions that need you (amber card on top of the list) →
  the open conversation → its workflow, files, media and terminal on demand.
- Organizing idea: a session is a chat with an agent. Its avatar is the agent
  CLI's logo; its state is a badge on the avatar, like presence in a
  messenger. Notes are sticky notes on the row and pinned in the chat.
- Views: Sessions (sidebar) · Chat · New session · Devices · Settings.
  Devices holds machines (health, plan usage, folders) and paired apps;
  everywhere else a machine is a dot with its name.

## Visual system

| Role | Project choice |
| --- | --- |
| Color | Tokens defined once with `light-dark()`; `<html data-theme="system\|light\|dark">` sets `color-scheme`. Light: canvas `#ffffff`, sidebar `#f4f6f5`, surface `#eef1f0`, text `#151918`, secondary `#454d4b`, muted `#5f6866`, decorative line `#e3e7e5`, control border `#858f8c`. Dark: canvas `#18191a`, sidebar `#111213`, surface `#232426`, raised `#2a2b2d`, text `#ececec`, secondary `#c4c7c9`, muted `#9a9fa3`, line `#2b2d2f`, control border `#6f7479`. Action: `#0a7f5a` with white text (light), `#3ecf98` with `#04261a` (dark). Focus ring = action color. Status: needs input amber, working blue, failed red, usage limit orange, workflow violet, note yellow, online green. |
| Typography | System UI stack (no web font: the binary stays small and it looks native on macOS and iOS); system mono for code, paths and branches only. Scale: page title 24/30 semibold, chat title 15/20 semibold, message 15/1.65 (16 on phones), UI 13.5–14/20, meta 12/16, code 13/20. Labels in sentence case. |
| Layout | Sidebar 288 px; conversation column max 760 px; inspector 440 px beside the chat from 1200 px, as an overlay below. Below 1024 px the sidebar is a drawer; below 640 px screens stack (list → chat → detail). |
| Spacing | 4 px steps; 8 inside a group, 16–24 between groups, 32+ between page sections. |
| Surfaces | Radius by role: 6 kbd and small chips, 10 buttons, inputs, rows and tool calls, 14 cards and menus, 22 the prompt box, round for status pills and composer pickers. Elevation only for the prompt box, menus, sheets and dialogs. |
| Media | One line-icon family (1.75 stroke, 16/18/20 px) in an inline sprite; provider logos as avatars. |
| Shell | The FLEET wordmark top left of the sidebar, 20 px tall (22 on phones): the old header's block letters redrawn as vectors, letters in the text color, the box-drawing shadow as a thin muted line. The V glyph stays the favicon and app icon. Device dots and settings at the sidebar's foot. |
| Themes | System (default), Light, Dark; chosen in Settings (the gear at the foot of the sidebar, top right on phones), kept in localStorage and applied before the first paint. |

## Components and states

- Session row: avatar + state badge, title, one line of what is going on
  (question, current step, workflow progress, limit, last reply), optional
  note line, time, pull request chips. Unread (a turn ended while you were
  elsewhere) = bold title + dot. Grouped by folder by default; status,
  machine or none from the list's group menu.
- Composer: one component for chat and new session. New session adds a
  context row (machine, folder, checkout, options). Pickers are menus on
  desktop and bottom sheets on phones.
- Agent activity: tool calls between two replies fold into one line
  ("Read 4 files, searched twice, ran 2 commands · 38s"), open while the
  agent works.
- Workflow: a live run card in the chat; the full run (phases, agents,
  their read-only chats, result) in the inspector.
- States: loading, empty, read-only (locked), ended session, usage limit,
  needs input (question form or terminal keys), working, interrupted.

## Responsive and accessible behavior

- Phones: list → chat → inspector as stacked screens with a back button and
  swipe back; swipe a row left to archive; bottom sheets for pickers; prompt
  box above the safe area, and on the keyboard while it shows (the screens
  fill the visual viewport); 16 px inputs (no iOS zoom); toasts at the top,
  like system banners; no page bounce, no text selection or link callouts
  on the app's chrome, hover styles only where there is hover.
- Opens like an app: the last lists and conversations show at once from
  the cache and catch up as the daemon answers; "Connecting…" only shows
  after 1.5 s without it.
- Access: muted text ≥ 4.5:1 and control borders ≥ 3:1 in both themes
  (checked with antislop's contrast-check); focus ring 2 px in the action
  color; native `<dialog>`; menus with arrow keys and Esc; live region for
  toasts; touch targets ≥ 40 px.

## Motion

- 120–160 ms fades for hovers and menus; 240 ms slides for phone screens and
  sheets; spinner only for working states. `prefers-reduced-motion` turns
  slides into cuts and spinners into still rings.

## Data the design relies on

- Session titles: the daemon reads the title Claude Code writes into its
  transcript (`"type":"ai-title"`) and falls back to the start of the first
  message, for Codex too (`Agent.title`).
- Unread: the page keeps a last-seen time per session in localStorage
  (`fleet.seen`); a turn that ended after it marks the row.

## Open items

- Not verified on a real iPhone (safe areas, keyboard, haptics, swipes).
