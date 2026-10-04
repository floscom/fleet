// fleet dashboard: live view fed by the daemon's /ws endpoint, plus root
// management (a folder picker over /api/) and sessions (start an agent,
// follow its chat, answer it, stop it) once this browser holds the admin
// token that `fleet web` prints (or was unlocked with the fleet key). With
// the token, agents of the other fleets that share this fleet's key are
// listed and driven too (/api/hosts/).
//
// Vanilla ES2020, no framework, no build step. The server only ever sends
// on the socket; we never write to it. DOM is built with
// createElement/textContent exclusively (never innerHTML with server data).
//
// Every Tailwind class used here must appear as a complete literal string
// so the Tailwind scanner (see input.css @source) picks it up.
'use strict';

(() => {
  // ---------------------------------------------------------------- state

  const S = {
    server: null,
    devices: [],
    peers: [],
    roots: [],
    agents: new Map(), // id -> agent
    remote: new Map(), // server id of another fleet -> {agents: Map(id -> agent), fails}
    workflows: new Map(), // agentKey -> workflow runs going on (see pollWorkflows)
    usage: new Map(), // server id -> agent CLIs and their plan usage (see pollUsage)
    snapshotDone: false,
    peersSeen: false, // first complete (post-browse) peers list of this connection received
  };
  const changedAt = new Map(); // "kind:id" -> ms when it last changed (for flash)
  const FLASH_MS = 1600;

  // --------------------------------------------------------------- helpers

  const $ = (id) => document.getElementById(id);

  // h('div', 'classes', child, ...) — children are nodes or strings (text).
  function h(tag, cls, ...children) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    for (const c of children) {
      if (c == null || c === false) continue;
      el.append(typeof c === 'string' || typeof c === 'number' ? String(c) : c);
    }
    return el;
  }

  function markChanged(key) {
    changedAt.set(key, Date.now());
  }

  // Apply the flash animation to el if key changed recently. A negative
  // animation-delay (set via CSSOM, which CSP allows) keeps the animation
  // continuous across re-renders instead of restarting it.
  function flash(el, key) {
    const t = changedAt.get(key);
    if (!t) return el;
    const age = Date.now() - t;
    if (age >= FLASH_MS) {
      changedAt.delete(key);
      return el;
    }
    el.classList.add('animate-flash', 'motion-reduce:animate-none');
    el.style.animationDelay = `-${age}ms`;
    return el;
  }

  function rel(ms) {
    if (!ms) return 'never';
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 5) return 'just now';
    if (s < 60) return `${s}s ago`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m ago`;
    const hr = Math.floor(m / 60);
    if (hr < 48) return `${hr}h ago`;
    return `${Math.floor(hr / 24)}d ago`;
  }

  function dur(ms) {
    let s = Math.max(0, Math.floor(ms / 1000));
    const d = Math.floor(s / 86400); s -= d * 86400;
    const hr = Math.floor(s / 3600); s -= hr * 3600;
    const m = Math.floor(s / 60); s -= m * 60;
    if (d) return `${d}d ${hr}h ${m}m`;
    if (hr) return `${hr}h ${m}m ${s}s`;
    if (m) return `${m}m ${s}s`;
    return `${s}s`;
  }

  // Element whose text is a relative time; refreshed by the 1s ticker.
  function relTime(ms, prefix, cls) {
    const el = h('time', cls || 'font-mono tabular-nums text-zinc-500');
    el.dataset.ts = String(ms || 0);
    el.dataset.prefix = prefix || '';
    if (ms) {
      el.dateTime = new Date(ms).toISOString();
      el.title = new Date(ms).toLocaleString();
    }
    el.textContent = (prefix || '') + rel(ms);
    return el;
  }

  // Empty state: a comment-style title plus an optional hint and command.
  function emptyRow(title, hint, cmd) {
    return h('li', 'px-4 py-8 text-center',
      h('div', 'font-mono text-xs text-zinc-600', '// ' + title),
      hint ? h('div', 'mt-1 text-xs text-zinc-500', hint) : null,
      cmd ? h('code', 'mt-2 inline-block rounded-md border border-ink-600 bg-ink-850 px-2 py-1 font-mono text-[11px] text-zinc-300', '$ ' + cmd) : null);
  }

  function chip(text, cls) {
    return h('span', cls || 'inline-flex items-center rounded border border-ink-600 bg-ink-800 px-1.5 py-px font-mono text-[11px] text-zinc-400', text);
  }

  // pathBreaks lets a long path wrap before a slash rather than inside a
  // folder name.
  function pathBreaks(p) {
    return p.split('/').flatMap((part, i) => (i ? [h('wbr'), '/' + part] : [part]));
  }

  // fitHeight grows a text box with its text, up to its CSS max height.
  function fitHeight(ta) {
    ta.style.height = 'auto';
    ta.style.height = ta.scrollHeight + 2 + 'px';
  }

  function shortPath(p) {
    if (!p) return '';
    return p.replace(/^\/(home|Users)\/[^/]+/, '~');
  }

  function hostPort(addr, port) {
    const a = addr.includes(':') ? `[${addr}]` : addr;
    return port ? `${a}:${port}` : a;
  }

  // ---------------------------------------------------------- agent states

  const STATES = {
    needs_input: {
      label: 'needs input', rank: 0,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-amber-400/10 px-2 py-0.5 text-[11px] font-semibold text-amber-300 ring-1 ring-inset ring-amber-400/40',
      dot: 'size-1.5 rounded-full bg-amber-400 animate-breathe motion-reduce:animate-none',
      edge: 'bg-amber-400',
    },
    working: {
      label: 'working', rank: 1,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-sky-400/10 px-2 py-0.5 text-[11px] font-semibold text-sky-300 ring-1 ring-inset ring-sky-400/30',
      dot: 'size-2.5 rounded-full border-[1.5px] border-sky-400 border-t-transparent animate-spin motion-reduce:animate-none',
      edge: 'bg-sky-400',
    },
    running: {
      label: 'running', rank: 2,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-indigo-400/10 px-2 py-0.5 text-[11px] font-semibold text-indigo-300 ring-1 ring-inset ring-indigo-400/30',
      dot: 'size-1.5 rounded-full bg-indigo-400',
      edge: 'bg-indigo-400/70',
    },
    starting: {
      label: 'starting', rank: 2,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-indigo-400/10 px-2 py-0.5 text-[11px] font-semibold text-indigo-300 ring-1 ring-inset ring-indigo-400/30',
      dot: 'size-1.5 rounded-full bg-indigo-400 animate-pulse motion-reduce:animate-none',
      edge: 'bg-indigo-400/70',
    },
    idle: {
      label: 'idle', rank: 3,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-emerald-400/10 px-2 py-0.5 text-[11px] font-semibold text-emerald-300/90 ring-1 ring-inset ring-emerald-400/25',
      dot: 'size-1.5 rounded-full bg-emerald-400/80',
      edge: 'bg-emerald-400/40',
    },
    unspecified: {
      label: 'unknown', rank: 4,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-zinc-400/10 px-2 py-0.5 text-[11px] font-semibold text-zinc-400 ring-1 ring-inset ring-zinc-400/25',
      dot: 'size-1.5 rounded-full bg-zinc-500',
      edge: 'bg-zinc-600',
    },
    failed: {
      label: 'failed', rank: 9,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-rose-500/10 px-2 py-0.5 text-[11px] font-semibold text-rose-300 ring-1 ring-inset ring-rose-500/40',
      dot: 'size-1.5 rounded-full bg-rose-500',
      edge: 'bg-rose-500/70',
    },
    exited: {
      label: 'exited', rank: 9,
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-zinc-400/5 px-2 py-0.5 text-[11px] font-semibold text-zinc-500 ring-1 ring-inset ring-zinc-500/25',
      dot: 'size-1.5 rounded-full bg-zinc-600',
      edge: 'bg-zinc-700',
    },
  };
  const stateOf = (a) => STATES[a.state] || STATES.unspecified;
  const isFinished = (a) => a.state === 'exited' || a.state === 'failed';

  function stateBadge(a) {
    const st = stateOf(a);
    const b = h('span', st.badge, h('span', st.dot), st.label);
    if (a.stateDetail) b.title = a.stateDetail;
    return b;
  }

  // agentRow is the row of agent a, running on machine host ('' = here).
  function agentRow({ a, host }) {
    const st = stateOf(a);
    const done = isFinished(a);
    const key = agentKey(host, a.id);
    const li = h('li', admin
      ? 'relative flex cursor-pointer touch-pan-y flex-col gap-1.5 px-4 py-3 transition-colors hover:bg-ink-850 sm:flex-row sm:gap-3'
      : 'relative flex flex-col gap-1.5 px-4 py-3 transition-colors hover:bg-ink-850 sm:flex-row sm:gap-3');
    li.dataset.id = a.id;
    li.dataset.key = key;
    li.append(h('span', 'absolute inset-y-2 left-0 w-0.5 rounded-r ' + st.edge));
    // By the event's path: a button's handler may have replaced the
    // clicked node (its iOS haptic label) before the click gets here.
    if (admin) li.addEventListener('click', (ev) => {
      const onButton = ev.composedPath().some((n) => n instanceof HTMLButtonElement);
      if (!onButton && !getSelection().toString()) openChat(host, a.id, a);
    });

    const body = h('div', 'min-w-0 flex-1');

    // line 1: name, adapter, badges ... updated
    const top = h('div', 'flex flex-wrap items-center gap-x-2 gap-y-1');
    const name = h('span', done ? 'truncate font-mono text-[13px] font-semibold text-zinc-400' : 'truncate font-mono text-[13px] font-semibold text-zinc-100', a.name || a.id);
    name.title = a.id;
    top.append(name, stateBadge(a));
    if (S.remote.size) top.append(hostChip(host));
    if (a.adapter) top.append(chip(a.adapter));
    if (a.sandbox === 'docker') {
      top.append(chip('docker', 'inline-flex items-center rounded border border-cyan-500/30 bg-cyan-500/10 px-1.5 py-px font-mono text-[11px] text-cyan-300'));
    }
    if (a.isolation && a.isolation !== 'pinned' && a.isolation !== 'unspecified') {
      top.append(chip(a.isolation));
    }
    if (done && a.exitCode != null) {
      top.append(chip('exit ' + a.exitCode, a.exitCode === 0
        ? 'inline-flex items-center rounded border border-ink-600 bg-ink-800 px-1.5 py-px font-mono text-[11px] text-zinc-500'
        : 'inline-flex items-center rounded border border-rose-500/30 bg-rose-500/10 px-1.5 py-px font-mono text-[11px] text-rose-300'));
    }
    body.append(top);

    // line 2: root/path, branch
    const meta = h('div', 'mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-zinc-500');
    const where = h('span', 'min-w-0 truncate font-mono');
    if (a.root) where.append(h('span', 'text-zinc-400', a.root), h('span', 'text-zinc-600', ' : '));
    where.append(shortPath(a.path || a.cwd || ''));
    where.title = a.cwd || a.path || '';
    meta.append(where);
    if (a.branch) {
      meta.append(h('span', 'inline-flex min-w-0 items-center gap-1 font-mono text-violet-300/80',
        h('span', 'text-zinc-600', '⎇'), h('span', 'truncate', a.branch)));
    }
    if (a.cloneUrl) {
      const c = h('span', 'min-w-0 truncate font-mono text-zinc-500', a.cloneUrl);
      c.title = a.cloneUrl;
      meta.append(c);
    }
    if (a.stateDetail && !done) meta.append(h('span', 'truncate text-amber-200/70', a.stateDetail));
    body.append(meta);

    // line 3: its workflow runs going on, the latest first
    const runs = (admin && S.workflows.get(agentKey(host, a.id))) || [];
    for (const run of runs.slice(-3).reverse()) body.append(workflowLine(host, a, run));

    // working: a thin shimmer under the row
    if (a.state === 'working') {
      li.append(h('span', 'shimmer-bar pointer-events-none absolute inset-x-0 bottom-0 h-px animate-shimmer motion-reduce:animate-none'));
    }

    // right column: attached, updated
    const side = h('div', 'flex shrink-0 flex-row-reverse items-center justify-end gap-3 text-xs sm:flex-col sm:items-end sm:justify-start sm:gap-1');
    const att = a.attachedClients || 0;
    const attEl = h('span', att > 0
      ? 'inline-flex items-center gap-1 rounded-full bg-emerald-400/10 px-1.5 font-mono tabular-nums text-emerald-300'
      : 'inline-flex items-center gap-1 px-1.5 font-mono tabular-nums text-zinc-600', (att > 0 ? '◉ ' : '○ ') + att);
    attEl.title = att === 1 ? '1 client attached' : `${att} clients attached`;
    side.append(attEl, relTime(a.updatedAtMs, 'updated ', 'whitespace-nowrap font-mono text-[11px] tabular-nums text-zinc-500'));
    if (admin) {
      const open = h('button', a.state === 'needs_input'
        ? 'touch:min-h-11 rounded-md bg-amber-400/15 px-2 py-0.5 text-xs font-medium text-amber-200 ring-1 ring-inset ring-amber-400/40 hover:bg-amber-400/25 focus-visible:outline-2 focus-visible:outline-amber-400'
        : 'touch:min-h-11 rounded-md px-2 py-0.5 text-xs text-zinc-400 hover:bg-ink-800 hover:text-emerald-300 focus-visible:outline-2 focus-visible:outline-emerald-400',
      a.state === 'needs_input' ? 'answer ›' : 'open ›');
      open.type = 'button';
      open.setAttribute('aria-label', `Open the session of ${a.name || a.id}`);
      open.addEventListener('click', () => openChat(host, a.id, a));
      side.append(open);
    }

    li.append(body, side);
    if (admin) {
      li.append(swipeAction(host, a));
      if (swipe.open === key) li.style.translate = `${-SWIPE_W}px 0`;
    }
    return flash(li, 'agent:' + key);
  }

  // endAction is what ending agent a does: a live session is archived
  // (killed, kept under History with its chat and files), a finished one
  // deleted (forgotten, and its worktree or clone removed unless it holds
  // work that would be lost; a worktree's branch stays).
  function endAction(a) {
    const iso = a.isolation === 'worktree' || a.isolation === 'clone' ? a.isolation : '';
    return isFinished(a)
      ? { label: 'Delete', busy: 'Deleting…', forget: true,
        note: 'It leaves History' + (iso ? `; its ${iso} is removed unless it holds unsaved work.` : '.') }
      : { label: 'Archive', busy: 'Archiving…', forget: false,
        note: 'The agent is ended and moves to History; its chat and files are kept.' };
  }

  // endAgent archives or deletes agent id on machine host (see endAction).
  // It returns {agent, forgot}, or null when it failed.
  async function endAgent(host, id, a) {
    const act = endAction(a || {});
    const name = (a && a.name) || id;
    const base = host ? `/api/hosts/${encodeURIComponent(host)}/agents/` : '/api/agents/';
    try {
      const r = await api('POST', base + encodeURIComponent(id) + '/stop', act.forget ? { forget: true, removeWorktree: true } : {});
      const rem = host && S.remote.get(host);
      if (act.forget) {
        saveDraft(host, id, '');
        if (rem) rem.agents.delete(id);
        else if (!host) S.agents.delete(id);
        if (r && r.worktreeKept) toast(`deleted ${name}; kept ${(a && a.cwd) || 'its worktree'}: ${r.reason}`, 'warn');
        else toast(`deleted ${name}`);
      } else {
        if (rem && r && r.agent) rem.agents.set(id, r.agent);
        toast(`archived ${name}`);
      }
      invalidate('agents');
      return { agent: r && r.agent, forgot: act.forget };
    } catch (e) {
      toast(`could not ${act.label.toLowerCase()} ${name}: ${e.message}`, 'error');
      return null;
    }
  }

  // ------------------------------------------------------------ row swipes
  //
  // On a touch screen an agent row swiped to the left uncovers its end
  // action (Archive or Delete, see endAction), done by a tap on it. One
  // row is open at a time; a tap elsewhere or a scroll closes it. The
  // agent lists are not re-rendered while a finger drags a row.

  const SWIPE_W = 96; // px, the width of the action (w-24)
  const swipe = {
    open: '', // agent key of the open row
    g: null, // the gesture: {li, key, id, x0, y0, base, x, drag}
    tapClose: false, // the finger came down while a row was open
    quiet: 0, // clicks are dropped until then: they end a gesture
    stale: false, // a render was skipped during a drag
  };

  function swipeAction(host, a) {
    const act = endAction(a);
    const b = h('button', act.forget
      ? 'absolute inset-y-0 left-full w-24 bg-rose-600 text-xs font-semibold text-white disabled:opacity-60'
      : 'absolute inset-y-0 left-full w-24 bg-amber-500 text-xs font-semibold text-ink-950 disabled:opacity-60',
    act.label);
    b.type = 'button';
    b.tabIndex = -1; // Archive and Delete are in the session for keyboards
    b.dataset.swipeAction = '';
    b.setAttribute('aria-label', `${act.label} ${a.name || a.id}`);
    b.addEventListener('click', async () => {
      b.disabled = true;
      b.textContent = act.busy;
      await endAgent(host, a.id, a);
      if (swipe.open === agentKey(host, a.id)) closeSwipe();
    });
    return b;
  }

  const swipeRow = (key) => document.querySelector(`li[data-key="${CSS.escape(key)}"]`);
  const swipeEase = () => (matchMedia('(prefers-reduced-motion: reduce)').matches ? 'none' : 'translate 200ms ease-out');

  function setSwipe(li, x, animate) {
    li.style.transition = animate ? swipeEase() : 'none';
    li.style.translate = x ? `${x}px 0` : '';
  }

  function closeSwipe() {
    const li = swipe.open && swipeRow(swipe.open);
    swipe.open = '';
    if (li) setSwipe(li, 0, true);
  }

  function wireSwipe() {
    // Capture: before the lists' handlers and the row's click.
    document.addEventListener('pointerdown', (ev) => {
      swipe.tapClose = !!swipe.open && !ev.target.closest(`li[data-key="${CSS.escape(swipe.open)}"] [data-swipe-action]`);
    }, true);
    document.addEventListener('click', (ev) => {
      if (Date.now() >= swipe.quiet) return;
      ev.preventDefault();
      ev.stopPropagation();
    }, true);

    const down = (ev) => {
      if (ev.pointerType !== 'touch' || !ev.isPrimary || !admin) return;
      const li = ev.target.closest('li[data-key]');
      if (!li || ev.target.closest('[data-swipe-action]')) return;
      swipe.g = { li, key: li.dataset.key, id: ev.pointerId, x0: ev.clientX, y0: ev.clientY,
        base: swipe.open === li.dataset.key ? -SWIPE_W : 0, x: 0, drag: false };
    };
    const move = (ev) => {
      const g = swipe.g;
      if (!g || ev.pointerId !== g.id) return;
      const dx = ev.clientX - g.x0, dy = ev.clientY - g.y0;
      if (!g.drag) {
        if (Math.abs(dy) > 10 && Math.abs(dy) > Math.abs(dx)) return end(ev); // a scroll
        if (Math.abs(dx) < 10) return;
        g.drag = true;
        g.li.setPointerCapture(g.id);
        if (swipe.open && swipe.open !== g.key) closeSwipe();
      }
      // Past the action the row follows the finger at half speed.
      let x = Math.min(0, g.base + dx);
      if (x < -SWIPE_W) x = -SWIPE_W + (x + SWIPE_W) / 2;
      g.x = x;
      setSwipe(g.li, x, false);
    };
    const end = (ev) => {
      const g = swipe.g;
      if (!g || ev.pointerId !== g.id) return;
      swipe.g = null;
      if (g.drag) {
        swipe.quiet = Date.now() + 250;
        const open = ev.type === 'pointerup' && g.x < -SWIPE_W / 2;
        swipe.open = open ? g.key : '';
        setSwipe(g.li, open ? -SWIPE_W : 0, true);
      } else if (swipe.tapClose) {
        // A tap or scroll beside the open row only closes it.
        if (ev.type === 'pointerup') swipe.quiet = Date.now() + 250;
        closeSwipe();
      }
      swipe.tapClose = false;
      if (swipe.stale) {
        swipe.stale = false;
        invalidate('agents');
      }
    };
    for (const ul of [$('agents-live'), $('agents-history')]) {
      ul.addEventListener('pointerdown', down);
      ul.addEventListener('pointermove', move);
      ul.addEventListener('pointerup', end);
      ul.addEventListener('pointercancel', end);
    }
    // A tap or scroll anywhere else closes the open row too.
    const outside = (ev) => {
      if (swipe.g || !swipe.tapClose) return;
      swipe.tapClose = false;
      if (ev.type === 'pointerup') swipe.quiet = Date.now() + 250;
      closeSwipe();
    };
    document.addEventListener('pointerup', outside);
    document.addEventListener('pointercancel', outside);
  }

  // agentKey identifies an agent across machines.
  const agentKey = (host, id) => (host ? host + '/' + id : id);

  // hostChip names the machine an agent runs on.
  function hostChip(host) {
    const c = chip(hostName(host), host
      ? 'inline-flex items-center rounded border border-sky-400/25 bg-sky-400/10 px-1.5 py-px font-mono text-[11px] text-sky-300'
      : 'inline-flex items-center rounded border border-emerald-400/25 bg-emerald-400/10 px-1.5 py-px font-mono text-[11px] text-emerald-300');
    c.title = host ? 'runs on another fleet' : 'runs on this machine';
    return c;
  }

  // allAgents lists this daemon's agents and those of other fleets.
  function allAgents() {
    const out = [...S.agents.values()].map((a) => ({ a, host: '' }));
    for (const [host, r] of S.remote) for (const a of r.agents.values()) out.push({ a, host });
    return out;
  }

  // folderOf is the folder agent a was started in, the key of the folder filter.
  const folderOf = (a) => a.path || a.cwd || '';

  // agentFolder is the folder the agents list is narrowed to ('' = all),
  // kept across reloads.
  const AGENT_FOLDER = 'fleet.agentFolder';
  let agentFolder = '';
  try {
    agentFolder = localStorage.getItem(AGENT_FOLDER) || '';
  } catch {}

  function setAgentFolder(f) {
    agentFolder = f;
    try {
      if (f) localStorage.setItem(AGENT_FOLDER, f);
      else localStorage.removeItem(AGENT_FOLDER);
    } catch {}
    invalidate('agents');
  }

  // renderFolderFilter fills the folder buttons under the agents heading:
  // "All", then one per folder with live agents and their count, the
  // busiest first. It stays hidden while they all work in the same folder.
  function renderFolderFilter(all) {
    const box = $('agents-folders');
    const folders = new Map(); // folder -> live agents in it
    for (const { a } of all) {
      const f = folderOf(a);
      if (f && !isFinished(a)) folders.set(f, (folders.get(f) || 0) + 1);
    }
    // The picked folder keeps its button while it has no agents (or those of
    // another fleet are still loading), so the filter can always be cleared.
    if (agentFolder && !folders.has(agentFolder)) folders.set(agentFolder, 0);
    box.hidden = folders.size < 2 && !agentFolder;
    if (box.hidden) return box.replaceChildren();

    const tail = (f) => f.split('/').filter(Boolean).pop() || f;
    const names = new Map(); // tail -> how many folders end in it
    for (const f of folders.keys()) names.set(tail(f), (names.get(tail(f)) || 0) + 1);
    const button = (f, label, live) => {
      const on = f === agentFolder;
      const b = h('button', on
        ? 'touch:min-h-11 inline-flex shrink-0 items-center gap-1.5 rounded-md border border-emerald-400/40 bg-emerald-400/15 px-2 py-0.5 font-mono text-xs text-emerald-200 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400'
        : 'touch:min-h-11 inline-flex shrink-0 items-center gap-1.5 rounded-md border border-ink-600 bg-ink-850 px-2 py-0.5 font-mono text-xs text-zinc-400 hover:bg-ink-800 hover:text-zinc-200 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400',
      label);
      b.type = 'button';
      b.setAttribute('aria-pressed', String(on));
      if (f) b.title = shortPath(f);
      if (live) b.append(h('span', on ? 'tabular-nums text-emerald-300/70' : 'tabular-nums text-zinc-600', String(live)));
      b.addEventListener('click', () => setAgentFolder(on ? '' : f));
      return b;
    };
    const sorted = [...folders].sort(([x, nx], [y, ny]) => ny - nx || tail(x).localeCompare(tail(y)));
    box.replaceChildren(button('', 'All', 0),
      ...sorted.map(([f, live]) => button(f, names.get(tail(f)) > 1 ? shortPath(f) : tail(f), live)));
  }

  // ------------------------------------------------------------- renderers

  function renderAgents() {
    if (swipe.g && swipe.g.drag) {
      swipe.stale = true;
      return;
    }
    const every = allAgents();
    if (swipe.open && !every.some(({ a, host }) => agentKey(host, a.id) === swipe.open)) swipe.open = '';
    renderFolderFilter(every);
    const all = agentFolder ? every.filter((x) => folderOf(x.a) === agentFolder) : every;
    const live = all.filter((x) => !isFinished(x.a)).sort(({ a: x }, { a: y }) =>
      stateOf(x).rank - stateOf(y).rank || (y.updatedAtMs || 0) - (x.updatedAtMs || 0) || x.name.localeCompare(y.name));
    const hist = all.filter((x) => isFinished(x.a)).sort(({ a: x }, { a: y }) => (y.updatedAtMs || 0) - (x.updatedAtMs || 0));

    const ul = $('agents-live');
    if (live.length) ul.replaceChildren(...live.map(agentRow));
    else if (!S.snapshotDone) ul.replaceChildren(emptyRow('loading…'));
    else if (admin) {
      const li = emptyRow('no live agents', 'start Claude Code or Codex on any machine of the fleet');
      const start = h('button', 'touch:min-h-11 mt-3 inline-flex items-center gap-1 rounded-md bg-emerald-400 px-3 py-1.5 text-xs font-semibold text-ink-950 hover:bg-emerald-300 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', '+ New session');
      start.type = 'button';
      start.addEventListener('click', () => openLaunch());
      li.append(h('div', '', start));
      ul.replaceChildren(li);
    } else ul.replaceChildren(emptyRow('no live agents', 'start one with', 'fleet run claude code:api'));

    const hl = $('agents-history');
    const HIST_MAX = 100;
    if (hist.length) {
      const rows = hist.slice(0, HIST_MAX).map(agentRow);
      if (hist.length > HIST_MAX) rows.push(h('li', 'px-4 py-2 text-center text-xs text-zinc-600', `${hist.length - HIST_MAX} older not shown`));
      hl.replaceChildren(...rows);
    } else {
      hl.replaceChildren(emptyRow('no finished agents'));
    }
    $('history-count').textContent = String(hist.length);
    $('agents-meta').textContent = `${live.length} live · ${all.length} ${agentFolder ? 'here' : 'total'}`;
  }

  function renderPeers() {
    const ul = $('peers');
    const peers = [...S.peers].sort((x, y) => (y.self ? 1 : 0) - (x.self ? 1 : 0) || x.name.localeCompare(y.name));
    if (!peers.length) {
      ul.replaceChildren(emptyRow('no fleets seen', 'other fleet daemons on this LAN show up here via mDNS'));
      return;
    }
    ul.replaceChildren(...peers.map((p) => {
      const key = 'peer:' + (p.id || p.name + '@' + p.host);
      const url = !p.self && peerURL(p);
      const li = h('li', '');
      const row = url
        ? h('a', 'group flex items-start gap-3 px-4 py-3 transition-colors hover:bg-ink-850 focus-visible:bg-ink-850 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400')
        : h('div', 'flex items-start gap-3 px-4 py-3');
      if (url) {
        row.href = url;
        row.rel = 'noreferrer';
        row.title = `Open the dashboard of ${p.name || p.host} (${url})`;
      }
      li.append(row);
      row.append(h('span', p.self
        ? 'mt-1.5 size-2 shrink-0 rounded-full bg-emerald-400 ring-4 ring-emerald-400/15'
        : 'mt-1.5 size-2 shrink-0 rounded-full bg-sky-400 ring-4 ring-sky-400/15'));
      const body = h('div', 'min-w-0 flex-1');
      const top = h('div', 'flex flex-wrap items-center gap-2');
      top.append(h('span', 'truncate font-mono text-[13px] font-semibold text-zinc-100', p.name || '(unnamed)'));
      if (p.self) top.append(chip('this daemon', 'inline-flex items-center rounded-full bg-emerald-400/10 px-2 py-px text-[11px] font-medium text-emerald-300 ring-1 ring-inset ring-emerald-400/30'));
      body.append(top);
      const addrs = (p.addrs || []).map((a) => hostPort(a, p.port));
      const sub = h('div', 'mt-0.5 flex flex-wrap gap-x-3 font-mono text-xs text-zinc-500');
      if (p.host) sub.append(h('span', 'text-zinc-400', p.host));
      // One unbreakable span per address so IPv6 never splits mid-address.
      if (addrs.length) addrs.forEach((a) => sub.append(h('span', 'whitespace-nowrap tabular-nums', a)));
      else if (p.port) sub.append(h('span', 'tabular-nums', ':' + p.port));
      body.append(sub);
      if (p.id) {
        const idEl = h('div', 'mt-0.5 truncate font-mono text-[11px] text-zinc-600', p.id.slice(0, 16));
        idEl.title = p.id;
        body.append(idEl);
      }
      const u = usageNode(p.id);
      if (u) body.append(u);
      row.append(body);
      if (url) row.append(h('span', 'mt-0.5 shrink-0 whitespace-nowrap text-xs text-zinc-500 group-hover:text-emerald-300', 'open ↗'));
      else if (!p.self) {
        const na = h('span', 'mt-0.5 shrink-0 text-[11px] text-zinc-600', 'no dashboard');
        na.title = 'This fleet does not advertise a web dashboard on the LAN (older version, or web is off or loopback-only).';
        row.append(na);
      }
      return flash(li, key);
    }));
  }

  // peerURL is the dashboard of peer p, at the address that looks most
  // reachable from here (longest shared IPv4 prefix with this page's host).
  function peerURL(p) {
    const addrs = p.addrs || [];
    if (!p.webPort || !addrs.length) return '';
    const here = location.hostname.split('.');
    const shared = (a) => {
      const parts = a.split('.');
      let n = 0;
      while (n < 4 && parts[n] === here[n]) n++;
      return n;
    };
    let best = addrs[0];
    for (const a of addrs) if (shared(a) > shared(best)) best = a;
    return `http://${hostPort(best, p.webPort)}/`;
  }

  const PLATFORM = { macos: 'macOS', ios: 'iOS', ipados: 'iPadOS', linux: 'Linux', windows: 'Windows', android: 'Android' };

  function renderDevices() {
    const ul = $('devices');
    const devs = [...S.devices].sort((x, y) => (y.connected ? 1 : 0) - (x.connected ? 1 : 0) || x.name.localeCompare(y.name));
    const online = devs.filter((d) => d.connected).length;
    $('devices-meta').textContent = devs.length ? `${online}/${devs.length} online` : '';
    if (!devs.length) {
      ul.replaceChildren(emptyRow('no paired devices', 'only for the fleet CLI or a native app on another machine. This page needs no pairing.', 'fleet pair'));
      return;
    }
    ul.replaceChildren(...devs.map((d) => {
      const li = h('li', 'flex items-center gap-3 px-4 py-3');
      const dot = h('span', d.connected
        ? 'size-2 shrink-0 rounded-full bg-emerald-400 shadow-[0_0_8px] shadow-emerald-400/60'
        : 'size-2 shrink-0 rounded-full bg-zinc-600');
      dot.title = d.connected ? 'connected' : 'offline';
      const body = h('div', 'min-w-0 flex-1',
        h('div', d.connected ? 'truncate text-[13px] font-medium text-zinc-100' : 'truncate text-[13px] font-medium text-zinc-400', d.name || d.id),
        h('div', 'text-xs text-zinc-500', PLATFORM[d.platform] || d.platform || 'unknown'));
      const right = d.connected
        ? h('span', 'text-xs font-medium text-emerald-300', 'online')
        : relTime(d.lastSeenAtMs, 'seen ', 'whitespace-nowrap font-mono text-[11px] tabular-nums text-zinc-500');
      li.append(dot, body, right);
      return flash(li, 'device:' + d.id);
    }));
  }

  function renderRoots() {
    const ul = $('roots');
    const multi = pickerHosts().length > 1 && S.server;
    $('roots-meta').textContent = (S.roots.length ? String(S.roots.length) : '') +
      (multi ? `${S.roots.length ? ' · ' : ''}on ${S.server.name || S.server.hostname}` : '');
    if (confirmRemove && !S.roots.some((r) => r.name === confirmRemove)) confirmRemove = '';
    if (!S.roots.length) {
      $('roots-find').hidden = true;
      $('roots-more').hidden = true;
      if (admin) {
        const li = emptyRow('no roots yet', 'agents can only start inside a root folder');
        const add = h('button', 'touch:min-h-11 mt-3 inline-flex items-center gap-1 rounded-md bg-emerald-400 px-3 py-1.5 text-xs font-semibold text-ink-950 hover:bg-emerald-300 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', '+ Add a folder');
        add.type = 'button';
        add.addEventListener('click', openPicker);
        li.append(h('div', '', add));
        ul.replaceChildren(li);
      } else {
        ul.replaceChildren(emptyRow('no roots configured', 'allow agents in a folder with', 'fleet roots add ~/code'));
      }
      return;
    }
    // Many roots: a filter, and the first few until "show all".
    const many = S.roots.length > ROOTS_FEW;
    $('roots-find').hidden = !many;
    const q = many ? $('roots-filter').value.trim().toLowerCase() : '';
    const words = q.split(/\s+/).filter(Boolean);
    const found = S.roots.filter((r) => words.every((w) => (r.name + ' ' + shortPath(r.path)).toLowerCase().includes(w)));
    const shown = q || rootsAll || !many ? found : found.slice(0, ROOTS_FEW);
    // Keep the one awaiting removal in view.
    if (confirmRemove && !shown.some((r) => r.name === confirmRemove)) {
      const r = found.find((x) => x.name === confirmRemove);
      if (r) shown.push(r);
    }
    const more = $('roots-more');
    more.hidden = !many || !!q;
    more.textContent = rootsAll ? 'Show fewer' : `Show all ${S.roots.length} roots`;
    more.setAttribute('aria-expanded', String(rootsAll));
    if (!shown.length) {
      ul.replaceChildren(emptyRow(`no root matches "${q}"`));
      return;
    }
    ul.replaceChildren(...shown.map((r) => {
      const li = h('li', 'group flex items-center gap-2 py-2 pl-4 pr-2');
      const main = h('div', 'min-w-0 flex-1');
      const top = h('div', 'flex min-w-0 items-center gap-2');
      const dot = h('span', r.trust ? 'size-1.5 shrink-0 rounded-full bg-emerald-400' : 'size-1.5 shrink-0 rounded-full bg-zinc-600');
      dot.title = r.trust ? 'trusted: agents skip the "trust this folder?" prompt' : 'untrusted';
      dot.setAttribute('role', 'img');
      dot.setAttribute('aria-label', r.trust ? 'trusted' : 'untrusted');
      top.append(dot, h('span', 'min-w-0 truncate font-mono text-[13px] font-semibold text-zinc-100', r.name));
      if (r.adapters && r.adapters.length) {
        top.append(h('span', 'shrink-0 truncate font-mono text-[11px] text-zinc-500', r.adapters.join(' · ')));
      }
      const path = h('div', 'truncate pl-3.5 font-mono text-xs text-zinc-500', shortPath(r.path));
      path.title = r.path;
      main.append(top, path);
      li.append(main);
      if (admin && confirmRemove !== r.name) {
        const go = h('button', 'touch:min-h-11 touch:min-w-11 touch:text-base shrink-0 rounded-md px-2 py-1 text-xs font-medium text-emerald-300/80 hover:bg-emerald-400/10 hover:text-emerald-200 focus-visible:outline-2 focus-visible:outline-emerald-400', '▸', h('span', 'touch:hidden', ' start'));
        go.type = 'button';
        go.title = `Start a session in ${r.name}`;
        go.setAttribute('aria-label', `Start a session in ${r.name}`);
        go.addEventListener('click', () => openLaunch({ root: r.name }));
        li.append(go);
      }
      if (admin) li.append(removeControl(r));
      return flash(li, 'root:' + r.name);
    }));
  }

  // The remove button of a root row, or its inline confirmation.
  function removeControl(r) {
    if (confirmRemove === r.name) {
      const yes = h('button', 'touch:min-h-11 rounded-md bg-rose-500/15 px-2 py-1 text-xs font-medium text-rose-200 ring-1 ring-inset ring-rose-500/40 hover:bg-rose-500/25 focus-visible:outline-2 focus-visible:outline-rose-400', 'Remove');
      yes.type = 'button';
      yes.dataset.focus = 'confirm-remove';
      yes.addEventListener('click', () => removeRoot(r.name));
      const no = h('button', 'touch:min-h-11 rounded-md px-2 py-1 text-xs text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Keep');
      no.type = 'button';
      no.addEventListener('click', () => {
        confirmRemove = '';
        focusAfterRender('[data-remove="' + CSS.escape(r.name) + '"]');
        invalidate('roots');
      });
      return h('div', 'flex shrink-0 flex-col items-end gap-1',
        h('span', 'text-[11px] text-zinc-500', 'running agents keep going'),
        h('div', 'flex items-center gap-1 touch:gap-2', no, yes));
    }
    const b = h('button', 'touch:min-h-11 touch:min-w-11 touch:text-sm shrink-0 rounded-md px-2 py-1 text-xs text-zinc-600 hover:bg-ink-800 hover:text-rose-300 focus-visible:outline-2 focus-visible:outline-emerald-400 group-hover:text-zinc-400',
      h('span', 'touch:hidden', 'remove'), h('span', 'hidden touch:inline', '✕'));
    b.type = 'button';
    b.dataset.remove = r.name;
    b.setAttribute('aria-label', `Remove root ${r.name}`);
    b.addEventListener('click', () => {
      confirmRemove = r.name;
      focusAfterRender('[data-focus="confirm-remove"]');
      invalidate('roots');
    });
    return b;
  }

  function renderServer() {
    const s = S.server;
    if (!s) return;
    $('srv-name').textContent = s.name || s.hostname || 'fleet';
    $('srv-version').textContent = $('srv-version').title = s.version || 'dev';
    $('srv-host').textContent = s.hostname || '–';
    $('srv-platform').textContent = `${s.os || '?'}/${s.arch || '?'}`;
    $('srv-listen').textContent = (s.listen || '–') + (s.mdns ? ' · mdns' : '');
    $('srv-id').textContent = s.id ? 'server ' + s.id : '';
    $('srv-id').title = s.id || '';
    $('roots-hint-host').textContent = s.hostname || s.name || 'the server';
    renderAccess();
    tickUptime();
  }

  function renderCounters() {
    let live = 0, input = 0, working = 0;
    for (const { a } of allAgents()) {
      if (isFinished(a)) continue;
      live++;
      if (a.state === 'needs_input') input++;
      if (a.state === 'working') working++;
    }
    $('c-live').textContent = String(live);
    const ci = $('c-input');
    ci.textContent = String(input);
    ci.className = input > 0
      ? 'mt-0.5 font-mono text-xl font-semibold tabular-nums text-amber-300 sm:mt-1 sm:text-2xl'
      : 'mt-0.5 font-mono text-xl font-semibold tabular-nums text-zinc-100 sm:mt-1 sm:text-2xl';
    const cw = $('c-working');
    cw.textContent = String(working);
    cw.className = working > 0
      ? 'mt-0.5 font-mono text-xl font-semibold tabular-nums text-sky-300 sm:mt-1 sm:text-2xl'
      : 'mt-0.5 font-mono text-xl font-semibold tabular-nums text-zinc-100 sm:mt-1 sm:text-2xl';
    $('c-peers').textContent = String(S.peers.length);
    const online = S.devices.filter((d) => d.connected).length;
    const cd = $('c-devices');
    cd.replaceChildren(String(online), h('span', 'text-base font-normal text-zinc-600', ' / ' + S.devices.length));
  }

  // The title is set outside requestAnimationFrame, which browsers pause in
  // background tabs: that is exactly when the needs-input count matters.
  function updateTitle() {
    let input = 0;
    for (const { a } of allAgents()) if (a.state === 'needs_input') input++;
    const base = S.server && S.server.name ? `fleet · ${S.server.name}` : 'fleet';
    const t = input > 0 ? `(${input}) ${base}` : base;
    if (document.title !== t) document.title = t;
  }

  // Batch renders per animation frame.
  const dirty = new Set();
  let rafPending = false;
  const RENDER = { server: renderServer, agents: renderAgents, peers: renderPeers, devices: renderDevices, roots: renderRoots };
  function invalidate(...parts) {
    updateTitle();
    for (const p of parts) dirty.add(p);
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(() => {
      rafPending = false;
      for (const p of dirty) RENDER[p]();
      dirty.clear();
      renderCounters();
      if (pendingFocus) {
        const el = document.querySelector(pendingFocus);
        pendingFocus = '';
        if (el) el.focus();
      }
    });
  }

  // Re-rendered lists lose focus; this restores it to selector's match.
  let pendingFocus = '';
  function focusAfterRender(selector) {
    pendingFocus = selector;
  }

  // ---------------------------------------------------------------- tickers

  function tickUptime() {
    const s = S.server;
    $('srv-uptime').textContent = s && s.startedAtMs ? dur(Date.now() - s.startedAtMs) : '–';
  }

  setInterval(() => {
    tickUptime();
    for (const el of document.querySelectorAll('time[data-ts]')) {
      const ts = Number(el.dataset.ts);
      const t = (el.dataset.prefix || '') + rel(ts);
      if (el.textContent !== t) el.textContent = t;
    }
    for (const el of document.querySelectorAll('[data-since]')) {
      const t = dur(Date.now() - Number(el.dataset.since));
      if (el.textContent !== t) el.textContent = t;
    }
  }, 1000);

  // ----------------------------------------------------------------- toasts

  const TOAST = {
    ok: ['pointer-events-auto flex max-w-md items-center gap-2 rounded-lg border border-emerald-400/30 bg-ink-850/95 px-3 py-2 text-xs text-zinc-200 shadow-lg shadow-black/40 transition-all duration-300 motion-reduce:transition-none', 'size-1.5 shrink-0 rounded-full bg-emerald-400'],
    warn: ['pointer-events-auto flex max-w-md items-center gap-2 rounded-lg border border-amber-400/30 bg-ink-850/95 px-3 py-2 text-xs text-zinc-200 shadow-lg shadow-black/40 transition-all duration-300 motion-reduce:transition-none', 'size-1.5 shrink-0 rounded-full bg-amber-400'],
    error: ['pointer-events-auto flex max-w-md items-center gap-2 rounded-lg border border-rose-500/40 bg-ink-850/95 px-3 py-2 text-xs text-zinc-200 shadow-lg shadow-black/40 transition-all duration-300 motion-reduce:transition-none', 'size-1.5 shrink-0 rounded-full bg-rose-500'],
  };

  function toast(text, kind) {
    const [box, dot] = TOAST[kind] || TOAST.ok;
    const t = h('div', box, h('span', dot), text);
    $('toasts').append(t);
    setTimeout(() => {
      t.classList.add('opacity-0', 'translate-y-1');
      setTimeout(() => t.remove(), 350);
    }, kind === 'error' || kind === 'warn' ? 7000 : 4000);
  }

  // ---------------------------------------------------------------- messages

  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);

  // Mark entries of a keyed list that are new or changed vs. the previous list.
  function diffMark(prev, next, keyFn, prefix, onNew) {
    const old = new Map(prev.map((x) => [keyFn(x), x]));
    for (const x of next) {
      const k = keyFn(x);
      const o = old.get(k);
      if (!o) {
        markChanged(prefix + k);
        if (onNew) onNew(x);
      } else if (!same(o, x)) {
        markChanged(prefix + k);
      }
    }
  }

  const peerKey = (p) => p.id || p.name + '@' + p.host;

  function resetState() {
    S.server = null;
    S.devices = [];
    S.peers = [];
    S.roots = [];
    S.agents.clear();
    S.snapshotDone = false;
    S.peersSeen = false;
    changedAt.clear();
  }

  function handle(msg) {
    switch (msg.type) {
      case 'hello':
        resetState();
        S.server = msg.server || null;
        setMainDim(false);
        invalidate('server', 'agents', 'peers', 'devices', 'roots');
        // The daemon may have restarted, or the token been rotated.
        checkSession(linkToken);
        linkToken = false;
        break;
      case 'devices': {
        const next = msg.devices || [];
        if (S.snapshotDone) diffMark(S.devices, next, (d) => d.id, 'device:');
        S.devices = next;
        invalidate('devices');
        break;
      }
      case 'peers': {
        const next = msg.peers || [];
        // Lists before the server's first mDNS browse (complete=false) hold
        // only this daemon; diff only against a complete list, so fleets
        // that were already running are not announced as new.
        if (S.peersSeen) {
          diffMark(S.peers, next, peerKey, 'peer:', (p) => {
            if (!p.self) toast(`new fleet on the network: ${p.name || p.host}`);
          });
        }
        if (msg.complete) S.peersSeen = true;
        S.peers = next;
        invalidate('peers', 'roots');
        if (next.some((p) => p.id && !S.usage.has(p.id))) scheduleUsage(0);
        if (picker.open) {
          renderHosts();
          checkHosts();
        }
        if (launch.open) renderLaunchHosts();
        break;
      }
      case 'roots': {
        const next = msg.roots || [];
        if (S.snapshotDone) diffMark(S.roots, next, (r) => r.name, 'root:');
        S.roots = next;
        invalidate('roots');
        if (picker.open) renderPicker();
        if (launch.open && !launch.host) launchRootsChanged();
        break;
      }
      case 'agent': {
        const a = msg.agent;
        if (!a || !a.id) break;
        const prev = S.agents.get(a.id);
        if (S.snapshotDone && (!prev || !same(prev, a))) markChanged('agent:' + a.id);
        S.agents.set(a.id, a);
        invalidate('agents');
        chatAgent('', a);
        break;
      }
      case 'agentRemoved':
        S.agents.delete(msg.id);
        changedAt.delete('agent:' + msg.id);
        invalidate('agents');
        break;
      case 'snapshotDone':
        S.snapshotDone = true;
        invalidate('agents');
        tryOpenPending();
        break;
      default:
        // unknown message types are ignored for forward compatibility
    }
  }

  // ------------------------------------------------------------------ admin
  //
  // Changing the fleet needs the admin token that `fleet web` prints as a
  // link (http://host:7421/#token=...), or that the unlock dialog gets for
  // the fleet key. It is kept in localStorage, which is per origin (port
  // included, unlike cookies), and sent as a Bearer header. The fragment
  // never reaches the server and is removed from the address bar right away.

  const TOKEN_KEY = 'fleet.adminToken';
  let memToken = ''; // fallback when storage is unavailable
  let admin = false;
  let linkToken = false; // a token just came from a link; the next hello checks it
  let confirmRemove = ''; // root whose removal awaits confirmation
  let rootsAll = false; // every root shown, not just the first few
  const ROOTS_FEW = 6;

  function getToken() {
    try {
      return localStorage.getItem(TOKEN_KEY) || memToken;
    } catch (e) {
      return memToken;
    }
  }

  function setToken(t) {
    memToken = t;
    try {
      if (t) localStorage.setItem(TOKEN_KEY, t);
      else localStorage.removeItem(TOKEN_KEY);
    } catch (e) {
      // storage disabled: memToken lasts until the tab closes
    }
  }

  // takeLinkToken stores a token from the URL fragment and strips it.
  function takeLinkToken() {
    const m = /(?:^#|&)token=([0-9A-Za-z]+)/.exec(location.hash);
    if (!m) return false;
    setToken(m[1]);
    history.replaceState(null, '', location.pathname + location.search);
    return true;
  }

  class APIError extends Error {
    constructor(status, message, notJoined) {
      super(message);
      this.status = status;
      this.notJoined = !!notJoined; // another machine without this fleet's key
    }
  }

  // api calls the admin API and returns the decoded JSON reply (null for
  // 204). Failures throw an APIError carrying the server's message.
  async function api(method, path, body) {
    const headers = {};
    const token = getToken();
    if (token) headers.Authorization = 'Bearer ' + token;
    const init = { method, headers, cache: 'no-store', credentials: 'omit' };
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(path, init);
    } catch (e) {
      throw new APIError(0, 'cannot reach the daemon');
    }
    let data = null;
    if (res.status !== 204) {
      try {
        data = await res.json();
      } catch (e) {
        // not JSON
      }
    }
    if (res.status === 401) lostAdmin();
    if (!res.ok) throw new APIError(res.status, (data && data.error) || `request failed (${res.status})`, data && data.notJoined);
    return data;
  }

  function setAdmin(on) {
    if (admin === on) return;
    admin = on;
    confirmRemove = '';
    if (!on) {
      closePicker();
      closeLaunch();
      closeChat();
      S.remote.clear();
      S.workflows.clear();
      S.usage.clear();
    } else {
      closeUnlock();
    }
    renderAccess();
    invalidate('roots', 'agents', 'peers');
    syncPush();
    tryOpenPending();
    scheduleRemote(0);
    scheduleWorkflows(0);
    scheduleUsage(0);
  }

  // lostAdmin handles a rejected token (rotated, or from another server).
  function lostAdmin() {
    const had = admin;
    setToken('');
    setAdmin(false);
    if (had) toast('admin access ended: unlock again with the fleet key, or run `fleet web` on the server', 'warn');
  }

  // checkSession asks whether our token is (still) valid. fromLink is set
  // when it just came from a `fleet web` link.
  async function checkSession(fromLink) {
    if (!getToken()) {
      setAdmin(false);
      return;
    }
    let s;
    try {
      s = await api('GET', '/api/session');
    } catch (e) {
      return; // daemon unreachable: keep the token, the next hello retries
    }
    if (s && s.admin) {
      setAdmin(true);
      if (fromLink) toast('this browser can now manage the fleet');
      return;
    }
    setToken('');
    setAdmin(false);
    toast(fromLink
      ? 'this admin link is no longer valid: run `fleet web` again'
      : 'the saved admin token is no longer valid: unlock again with the fleet key', 'warn');
  }

  function signOut() {
    // The api call takes the token before it goes.
    if (push.on) stopPush();
    setToken('');
    setAdmin(false);
    toast('signed out: this browser is read-only again');
  }

  function renderAccess() {
    const pill = $('access');
    const where = (S.server && (S.server.hostname || S.server.name)) || 'the server';
    if (admin) {
      pill.className = 'inline-flex items-center gap-1.5 rounded-full border border-violet-400/30 bg-violet-400/10 px-2.5 py-1 text-xs font-medium text-violet-200';
      pill.textContent = 'admin';
      pill.title = 'This browser can add and remove roots.';
    } else {
      pill.className = 'inline-flex items-center gap-1.5 rounded-full border border-ink-600 bg-ink-850 px-2.5 py-1 text-xs font-medium text-zinc-500';
      pill.textContent = 'read-only';
      pill.title = `To manage this fleet here, unlock it with the fleet key, or run "fleet web" on ${where} and open the link it prints.`;
    }
    $('unlock-host').textContent = where;
    $('unlock-open').hidden = admin;
    $('signout').hidden = !admin;
    renderNotify();
    $('roots-add').hidden = !admin;
    $('agents-new').hidden = !admin;
    $('roots-hint').hidden = admin;
    $('footer-mode').textContent = admin ? 'admin' : 'read-only view';
  }

  // ------------------------------------------------------------------- push
  //
  // Notifications when an agent needs input, finishes or fails (push.go).
  // Browsers offer push to secure contexts only, and iOS only to the web
  // app added to the Home Screen, so the button says what is missing
  // instead of failing. Subscribing needs the admin token; the service
  // worker (sw.js) shows the notifications.

  const PUSH_OK = window.isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  const push = { key: '', on: false, busy: false, reg: null, endpoint: '' };
  // openPending is an agent to show once it is known: a tapped notification.
  let openPending = '';

  function pushHint() {
    if (!window.isSecureContext) return `Notifications need HTTPS: open this dashboard over https (e.g. tailscale serve), not http://${location.host}.`;
    if (IOS && !navigator.standalone) return 'On iPhone, add the dashboard to the Home Screen (Share › Add to Home Screen) and open it from there to get notifications.';
    return 'This browser does not support push notifications.';
  }

  function renderNotify() {
    const b = $('notify');
    b.hidden = !admin;
    b.disabled = push.busy;
    b.setAttribute('aria-pressed', String(push.on));
    b.setAttribute('aria-label', push.on ? 'notifications on' : 'notifications off');
    b.querySelector('svg').setAttribute('fill', push.on ? 'currentColor' : 'none');
    // Pill-sized like its neighbours; ::after widens the tap area on touch.
    b.className = 'relative inline-flex items-center rounded-full border px-2 py-1 pointer-coarse:after:absolute pointer-coarse:after:-inset-2 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400 ' +
      (push.on ? 'border-emerald-400/30 bg-emerald-400/10 text-emerald-300 hover:bg-emerald-400/20' : 'border-ink-600 bg-ink-850 text-zinc-500 hover:bg-ink-800 hover:text-zinc-200');
    b.title = !PUSH_OK ? pushHint()
      : push.on ? 'Notifications on: this device hears when an agent needs you, finishes or fails. Click to turn them off.'
      : 'Notifications off: click to hear on this device when an agent needs you, finishes or fails.';
  }

  // b64url decodes base64url into bytes.
  function b64url(str) {
    const bin = atob(str.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((str.length + 3) % 4));
    return Uint8Array.from(bin, (c) => c.charCodeAt(0));
  }

  // sameKey reports whether sub was made for the daemon's VAPID key.
  function sameKey(sub, key) {
    const k = sub.options && sub.options.applicationServerKey;
    if (!k) return true; // not exposed: assume it is ours
    const a = new Uint8Array(k);
    const b = b64url(key);
    return a.length === b.length && a.every((x, i) => x === b[i]);
  }

  // syncPush registers the service worker and checks this browser's
  // subscription with the daemon: one the daemon lost is sent again, one
  // made for another key is dropped.
  async function syncPush() {
    if (!PUSH_OK || !admin) return renderNotify();
    try {
      await navigator.serviceWorker.register('/sw.js');
      push.reg = await navigator.serviceWorker.ready;
      const sub = await push.reg.pushManager.getSubscription();
      const r = await api('GET', '/api/push' + (sub ? '?endpoint=' + encodeURIComponent(sub.endpoint) : ''));
      push.key = r.key;
      push.on = false;
      if (sub && sameKey(sub, r.key) && Notification.permission === 'granted') {
        if (!r.subscribed) await api('POST', '/api/push/subscribe', sub.toJSON());
        Object.assign(push, { on: true, endpoint: sub.endpoint });
      } else if (sub) {
        await sub.unsubscribe();
      }
    } catch (e) {
      // e.g. 404: notifications are off on this daemon
    }
    renderNotify();
  }

  async function toggleNotify() {
    if (!PUSH_OK) return toast(pushHint(), 'warn');
    if (push.busy) return;
    push.busy = true;
    renderNotify();
    try {
      if (push.on) {
        await stopPush();
        toast('notifications off on this device');
      } else {
        if (!push.reg || !push.key) throw new Error('not ready yet, try again in a moment');
        // Straight from the click: Safari asks for permission only from
        // a user gesture.
        const sub = await push.reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64url(push.key) });
        await api('POST', '/api/push/subscribe', sub.toJSON());
        Object.assign(push, { on: true, endpoint: sub.endpoint });
        try {
          await api('POST', '/api/push/test', { endpoint: sub.endpoint });
        } catch (e) {
          // The push service refused this subscription: the daemon dropped it.
          if (e.status === 410) await stopPush();
          throw e;
        }
        toast('notifications on: a test notification is on its way');
      }
    } catch (e) {
      if (window.Notification && Notification.permission === 'denied') toast('notifications are blocked for this site in the browser or system settings', 'warn');
      else toast(`notifications: ${e.message}`, 'error');
    }
    push.busy = false;
    renderNotify();
  }

  // stopPush ends this browser's subscription here and at the push service.
  async function stopPush() {
    const endpoint = push.endpoint;
    push.on = false;
    push.endpoint = '';
    if (endpoint) await api('POST', '/api/push/unsubscribe', { endpoint }).catch(() => {});
    const sub = push.reg && (await push.reg.pushManager.getSubscription());
    if (sub) await sub.unsubscribe();
  }

  // openFromPush shows the agent of a tapped notification once the agent
  // list and admin rights are in.
  function openFromPush(id) {
    openPending = id || '';
    tryOpenPending();
  }

  function tryOpenPending() {
    if (!openPending || !admin || !S.snapshotDone) return;
    const a = S.agents.get(openPending);
    openPending = '';
    if (a) openChat('', a.id, a);
  }

  function wirePush() {
    $('notify').addEventListener('click', toggleNotify);
    const id = new URLSearchParams(location.search).get('agent');
    if (id) {
      history.replaceState(null, '', location.pathname + location.hash);
      openFromPush(id);
    }
    if (!PUSH_OK) return;
    navigator.serviceWorker.addEventListener('message', (ev) => {
      if (ev.data && ev.data.type === 'openAgent') openFromPush(ev.data.agent);
    });
  }

  // ----------------------------------------------------------------- unlock
  //
  // Someone who knows the fleet key (or this machine's admin token) types
  // it in instead of opening a `fleet web` link: POST /api/unlock trades
  // either for the admin token, which is then kept like one from a link.

  let unlockBusy = false;

  function openUnlock() {
    if (admin) return;
    unlockBusy = false;
    $('unlock-key').value = '';
    renderUnlock('');
    $('unlock').showModal();
    if (!touch()) $('unlock-key').focus();
  }

  function closeUnlock() {
    const d = $('unlock');
    if (d.open) d.close();
  }

  function renderUnlock(error) {
    const st = $('unlock-status');
    st.className = error ? 'min-h-5 text-xs text-rose-300' : 'min-h-5 text-xs text-zinc-500';
    st.textContent = error || (unlockBusy ? 'checking…' : '');
    $('unlock-submit').disabled = unlockBusy;
  }

  // unlockKey is the key in what was typed or pasted: bare, or out of a
  // `fleet web` link (#token=...) or join command (--join ...). Any length:
  // an admin token set by hand may be short.
  function unlockKey(text) {
    const m = /(?:#token=|--join\s+)([0-9A-Za-z]+)/.exec(text);
    return m ? m[1] : text.trim();
  }

  async function submitUnlock(ev) {
    ev.preventDefault();
    if (unlockBusy) return;
    const key = unlockKey($('unlock-key').value);
    if (!key) {
      renderUnlock('enter the fleet key or the admin token');
      return;
    }
    unlockBusy = true;
    renderUnlock('');
    let r;
    try {
      r = await api('POST', '/api/unlock', { key });
    } catch (e) {
      unlockBusy = false;
      renderUnlock(e.message);
      return;
    }
    unlockBusy = false;
    if (!r || !r.token) {
      renderUnlock('the daemon sent no token');
      return;
    }
    setToken(r.token);
    setAdmin(true);
    toast('this browser can now manage the fleet');
  }

  function wireUnlock() {
    const dlg = $('unlock');
    $('unlock-open').addEventListener('click', openUnlock);
    $('roots-unlock').addEventListener('click', openUnlock);
    $('unlock-form').addEventListener('submit', submitUnlock);
    $('unlock-close').addEventListener('click', closeUnlock);
    $('unlock-cancel').addEventListener('click', closeUnlock);
    // The field holds a secret: gone once the dialog closes.
    dlg.addEventListener('close', () => {
      $('unlock-key').value = '';
    });
    let downOutside = false;
    dlg.addEventListener('mousedown', (ev) => {
      downOutside = ev.target === dlg;
    });
    dlg.addEventListener('click', (ev) => {
      if (downOutside && ev.target === dlg && !unlockBusy) closeUnlock();
    });
  }

  async function removeRoot(name) {
    confirmRemove = '';
    try {
      await api('DELETE', '/api/roots/' + encodeURIComponent(name));
      toast(`removed root ${name}`);
    } catch (e) {
      toast(`could not remove ${name}: ${e.message}`, 'error');
    }
    invalidate('roots');
  }

  // ----------------------------------------------------------------- picker
  //
  // A folder browser over GET /api/fs: walk a machine's folders and add
  // the current one as a root. The machine is this daemon or another fleet
  // on the LAN, reached through /api/hosts/<id>/ (this daemon signs the
  // request with the fleet key; machines without it are "not joined").

  const picker = {
    open: false,
    host: '', // server id of the machine being browsed; '' = this daemon
    hosts: new Map(), // server id -> 'checking' | 'joined' | 'not_joined' | 'unreachable'
    dir: null, // last listing: {path, parent, home, entries, truncated}
    seq: 0, // request counter; stale replies are dropped
    loading: false,
    error: '',
    busy: false, // an add is in flight
    adapters: null, // [{id, name, available}] once loaded
    roots: null, // roots of another machine, once loaded
    joinCmd: '', // `fleet start --join <key>`, once loaded
  };

  // Phones and tablets: no keyboard to keep focus for, and focusing a text
  // field opens the on-screen keyboard.
  const touch = () => matchMedia('(pointer: coarse)').matches;

  // pickerHosts: this daemon, then the fleets on the LAN with a dashboard.
  function pickerHosts() {
    const self = S.server || {};
    const out = [{ id: '', name: self.name || self.hostname || 'this machine' }];
    for (const p of S.peers) {
      if (!p.self && p.id && p.webPort && (p.addrs || []).length) out.push({ id: p.id, name: p.name || p.host || p.id.slice(0, 8) });
    }
    return out;
  }

  const hostName = (id) => (pickerHosts().find((x) => x.id === id) || { name: 'that machine' }).name;
  const hostAPI = (path) => (picker.host ? `/api/hosts/${encodeURIComponent(picker.host)}/${path}` : '/api/' + path);
  const hostRoots = () => (picker.host ? picker.roots || [] : S.roots);

  const joinPath = (dir, name) => (dir.endsWith('/') ? dir : dir + '/') + name;
  const baseName = (p) => p.split('/').filter(Boolean).pop() || '';
  const rootAt = (path) => hostRoots().find((r) => r.path === path);

  // rootAround is the most specific root containing path.
  function rootAround(path) {
    let best = null;
    for (const r of hostRoots()) {
      const inside = path === r.path || path.startsWith(r.path.endsWith('/') ? r.path : r.path + '/');
      if (inside && (!best || r.path.length > best.path.length)) best = r;
    }
    return best;
  }

  function openPicker() {
    if (!admin) return;
    $('picker-name').value = '';
    $('picker-trust').checked = false;
    $('picker-filter').value = '';
    picker.error = '';
    picker.open = true;
    if (picker.host && !pickerHosts().some((x) => x.id === picker.host)) resetHost('');
    $('picker').showModal();
    renderHosts();
    renderAdapters();
    renderPicker();
    browse(picker.dir ? picker.dir.path : '');
    loadAdapters();
    loadHostRoots();
    checkHosts();
    if (!touch()) $('picker-filter').focus();
  }

  function closePicker() {
    const d = $('picker');
    if (d.open) d.close();
  }

  // browse lists path ('' = home). typed is set when path came from the
  // path field; otherwise the field is not overwritten while being edited.
  async function browse(path, typed) {
    const seq = ++picker.seq;
    picker.loading = true;
    renderPicker();
    const q = new URLSearchParams();
    if (path) q.set('path', path);
    if ($('picker-hidden').checked) q.set('hidden', '1');
    let dir = null;
    let err = '';
    try {
      dir = await api('GET', hostAPI('fs?' + q));
    } catch (e) {
      err = e.message;
      if (picker.host && seq === picker.seq) {
        if (e.notJoined) setHostState(picker.host, 'not_joined');
        else if (e.status === 502 || e.status === 404) setHostState(picker.host, 'unreachable');
      }
    }
    if (seq !== picker.seq) return;
    if (dir && picker.host) setHostState(picker.host, 'joined');
    picker.loading = false;
    picker.error = err;
    if (dir) {
      const moved = !picker.dir || picker.dir.path !== dir.path;
      picker.dir = dir;
      const field = $('picker-path');
      if (typed || document.activeElement !== field) field.value = dir.path;
      if (moved) {
        $('picker-filter').value = '';
        $('picker-name').value = '';
        $('picker-list').scrollTop = 0;
      }
    }
    renderPicker();
  }

  function goToTyped() {
    let p = $('picker-path').value.trim();
    if (!p) return;
    if (p === '~' || p.startsWith('~/')) p = ((picker.dir && picker.dir.home) || '') + p.slice(1);
    browse(p, true);
  }

  async function loadAdapters() {
    if (picker.adapters) return;
    const host = picker.host;
    let list = [];
    try {
      const r = await api('GET', hostAPI('adapters'));
      list = (r && r.adapters) || [];
    } catch (e) {
      // not joined or unreachable: the folder list says so
    }
    if (host !== picker.host) return;
    picker.adapters = list;
    renderAdapters();
  }

  // loadHostRoots fetches another machine's roots, to mark them in the list.
  async function loadHostRoots() {
    const host = picker.host;
    if (!host) return;
    try {
      const r = await api('GET', hostAPI('roots'));
      if (host !== picker.host) return;
      picker.roots = (r && r.roots) || [];
      renderPicker();
    } catch (e) {
      // browse reports it
    }
  }

  // ---------------------------------------------------------- machines

  function setHostState(id, state) {
    if (!id) return;
    const changed = picker.hosts.get(id) !== state;
    picker.hosts.set(id, state);
    if (changed) renderHosts();
    if (changed && launch.open) renderLaunchHosts();
    if (id !== picker.host) return;
    if (changed) renderPicker();
    if (state === 'not_joined') waitForJoin();
  }

  // checkHosts asks every other fleet whether it holds this fleet's key.
  function checkHosts() {
    for (const x of pickerHosts()) {
      if (!x.id || picker.hosts.get(x.id) === 'joined') continue;
      if (!picker.hosts.has(x.id)) picker.hosts.set(x.id, 'checking');
      checkHost(x.id);
    }
    renderHosts();
  }

  async function checkHost(id) {
    let state;
    try {
      const r = await api('GET', `/api/hosts/${encodeURIComponent(id)}/session`);
      state = r && r.admin ? 'joined' : 'not_joined';
    } catch (e) {
      state = e.notJoined ? 'not_joined' : 'unreachable';
    }
    setHostState(id, state);
    return state;
  }

  const HOST_CHIP = 'touch:min-h-11 inline-flex cursor-pointer select-none items-center gap-1.5 rounded-md border border-ink-600 bg-ink-900 px-2 py-1 font-mono text-[12px] text-zinc-300 hover:border-zinc-500 has-checked:border-emerald-400/50 has-checked:bg-emerald-400/10 has-checked:text-emerald-200 has-focus-visible:outline-2 has-focus-visible:outline-offset-2 has-focus-visible:outline-emerald-400';
  const HOST_STATE = {
    self: ['size-1.5 shrink-0 rounded-full bg-emerald-400', 'this machine'],
    joined: ['size-1.5 shrink-0 rounded-full bg-sky-400', ''],
    checking: ['size-1.5 shrink-0 rounded-full bg-zinc-500 animate-pulse motion-reduce:animate-none', ''],
    not_joined: ['size-1.5 shrink-0 rounded-full bg-amber-400', 'not joined'],
    unreachable: ['size-1.5 shrink-0 rounded-full bg-rose-500', 'unreachable'],
  };

  function renderHosts() {
    const box = $('picker-hosts');
    box.replaceChildren(...pickerHosts().map((x) => {
      const rb = h('input', 'sr-only');
      rb.type = 'radio';
      rb.name = 'picker-host';
      rb.value = x.id;
      rb.checked = x.id === picker.host;
      const [dot, note] = HOST_STATE[x.id ? picker.hosts.get(x.id) || 'checking' : 'self'];
      return h('label', HOST_CHIP, rb, h('span', dot), x.name,
        note ? h('span', 'font-sans text-[11px] text-zinc-500', note) : null);
    }));
    $('picker-host').textContent = hostName(picker.host);
  }

  // resetHost switches the picker to machine id without loading anything.
  function resetHost(id) {
    picker.host = id;
    picker.dir = null;
    picker.roots = null;
    picker.adapters = null;
    picker.error = '';
    picker.seq++;
  }

  function selectHost(id) {
    if (picker.host === id) return;
    resetHost(id);
    $('picker-path').value = '';
    $('picker-filter').value = '';
    $('picker-name').value = '';
    renderHosts();
    renderAdapters();
    browse('');
    loadAdapters();
    loadHostRoots();
  }

  // waitForJoin re-checks a not-joined machine every few seconds while it is
  // picked, and loads it once `fleet start --join` ran there.
  let joinTimer = 0;
  function waitForJoin() {
    clearTimeout(joinTimer);
    if (!picker.open) return;
    const host = picker.host;
    joinTimer = setTimeout(async () => {
      if (!picker.open || picker.host !== host) return;
      // checkHost keeps waiting (setHostState) while it is not joined.
      const state = await checkHost(host);
      if (state === 'joined' && picker.open && picker.host === host) {
        toast(`${hostName(host)} joined the fleet`);
        browse('');
        loadAdapters();
        loadHostRoots();
      }
    }, 3000);
  }

  async function loadJoin() {
    if (picker.joinCmd) return;
    try {
      const r = await api('GET', '/api/join');
      picker.joinCmd = (r && r.command) || '';
    } catch (e) {
      return;
    }
    renderPickerList();
  }

  // copyText copies el's text. Plain HTTP has no Clipboard API, so this
  // falls back to selecting the text and the old copy command.
  async function copyText(el, btn) {
    let ok = false;
    try {
      await navigator.clipboard.writeText(el.textContent);
      ok = true;
    } catch (e) {
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      try {
        ok = document.execCommand('copy');
      } catch (e2) {
        ok = false;
      }
    }
    btn.textContent = ok ? 'Copied' : 'Selected: copy it';
  }

  // joinPanel explains how to add a machine that lacks this fleet's key.
  function joinPanel() {
    loadJoin();
    const name = hostName(picker.host);
    const cmd = h('code', 'block min-w-0 flex-1 select-all break-all rounded-md border border-ink-600 bg-ink-950 px-2.5 py-2 text-left font-mono text-[12px] leading-relaxed text-zinc-200', picker.joinCmd || 'loading…');
    const copy = h('button', 'touch:min-h-11 shrink-0 self-start rounded-md border border-ink-600 bg-ink-850 px-2.5 py-1.5 text-xs font-medium text-zinc-300 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', 'Copy');
    copy.type = 'button';
    copy.disabled = !picker.joinCmd;
    copy.addEventListener('click', () => copyText(cmd, copy));
    return h('li', 'px-2 py-6',
      h('div', 'text-center font-mono text-xs text-zinc-600', `// ${name} is not in this fleet yet`),
      h('p', 'mt-3 text-xs text-zinc-400', 'Run this on ', h('span', 'font-mono text-zinc-200', name), ' once (it starts fleet there if needed):'),
      h('div', 'mt-2 flex items-start gap-2', cmd, copy),
      h('p', 'mt-2 text-[11px] leading-relaxed text-zinc-600', 'This picker notices by itself. Machines with the same key manage each other from their dashboards, so share it like a password.'));
  }

  function renderAdapters() {
    const box = $('picker-adapters');
    if (!picker.adapters) {
      box.replaceChildren(h('span', 'text-xs text-zinc-600', 'loading…'));
      return;
    }
    if (!picker.adapters.length) {
      box.replaceChildren(h('span', 'text-xs text-zinc-600', 'all adapters'));
      return;
    }
    box.replaceChildren(...picker.adapters.map((a) => {
      const cb = h('input', 'accent-emerald-400');
      cb.type = 'checkbox';
      cb.value = a.id;
      const label = h('label', 'touch:min-h-11 inline-flex cursor-pointer select-none items-center gap-1.5 rounded-md border border-ink-600 bg-ink-900 px-2 py-1 font-mono text-[12px] text-zinc-300 hover:border-zinc-500 has-checked:border-emerald-400/50 has-checked:bg-emerald-400/10 has-checked:text-emerald-200', cb, a.id);
      label.title = a.available ? a.name : `${a.name}: not installed on this machine`;
      if (!a.available) label.append(h('span', 'text-zinc-600', 'n/a'));
      return label;
    }));
  }

  function renderPicker() {
    if (!picker.open) return;
    const dir = picker.dir;
    renderCrumbs(dir ? dir.path : '');
    renderPickerList();
    const existing = dir && rootAt(dir.path);
    const st = $('picker-status');
    const notJoined = picker.host && picker.hosts.get(picker.host) === 'not_joined';
    if (notJoined) {
      st.className = 'min-h-5 text-xs text-amber-200/90';
      st.textContent = `${hostName(picker.host)} does not have this fleet's key yet.`;
    } else if (picker.error) {
      st.className = 'min-h-5 text-xs text-rose-300';
      st.replaceChildren(h('span', '', picker.error, dir ? h('span', 'text-zinc-500', ` · still in ${dir.path}`) : null));
    } else if (!dir) {
      st.className = 'min-h-5 text-xs text-zinc-500';
      st.textContent = 'loading…';
    } else if (existing) {
      st.className = 'min-h-5 text-xs text-amber-200/90';
      st.textContent = `This folder is already the root "${existing.name}".`;
    } else {
      const around = rootAround(dir.path);
      st.className = 'min-h-5 min-w-0 break-all text-xs text-zinc-500';
      st.replaceChildren(h('span', '', 'adds ', h('span', 'font-mono text-zinc-100', dir.path),
        around ? ` · inside root "${around.name}", where agents can already start` : null));
    }
    $('picker-name').placeholder = dir ? baseName(dir.path) || 'root' : '';
    const add = $('picker-add');
    add.disabled = !dir || picker.loading || picker.busy || !!existing || notJoined;
    add.textContent = picker.busy ? 'Adding…' : 'Add as root';
    $('picker-list').classList.toggle('opacity-50', picker.loading);
  }

  function renderCrumbs(path) {
    const nav = $('picker-crumbs');
    if (!path) {
      nav.replaceChildren();
      return;
    }
    const crumb = (label, target, current) => {
      if (current) {
        const el = h('span', 'rounded px-1 py-0.5 font-semibold text-zinc-100', label);
        el.setAttribute('aria-current', 'location');
        return el;
      }
      const b = h('button', 'touch:min-h-11 touch:min-w-11 rounded px-1 py-0.5 text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', label);
      b.type = 'button';
      b.addEventListener('click', () => browse(target));
      return b;
    };
    const parts = path.split('/').filter(Boolean);
    const items = [crumb('/', '/', !parts.length)];
    let acc = '';
    parts.forEach((p, i) => {
      acc += '/' + p;
      if (i > 0) items.push(h('span', 'text-zinc-700', '/'));
      items.push(crumb(p, acc, i === parts.length - 1));
    });
    nav.replaceChildren(...items);
  }

  const pickerNote = (text) => h('li', 'px-2 py-8 text-center font-mono text-xs text-zinc-600', '// ' + text);

  function renderPickerList() {
    const ul = $('picker-list');
    const dir = picker.dir;
    if (picker.host && picker.hosts.get(picker.host) === 'not_joined') {
      ul.replaceChildren(joinPanel());
      return;
    }
    if (!dir) {
      if (picker.loading || !picker.host || !picker.error) {
        ul.replaceChildren(pickerNote(picker.loading ? 'loading…' : 'nothing to show'));
        return;
      }
      const retry = h('button', 'touch:min-h-11 mt-3 rounded-md border border-ink-600 bg-ink-850 px-2.5 py-1 font-sans text-xs text-zinc-300 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Try again');
      retry.type = 'button';
      retry.addEventListener('click', () => {
        browse('');
        loadAdapters();
        loadHostRoots();
      });
      const li = pickerNote(picker.error);
      li.append(h('div', '', retry));
      ul.replaceChildren(li);
      return;
    }
    const f = $('picker-filter').value.trim().toLowerCase();
    const shown = f ? dir.entries.filter((e) => e.name.toLowerCase().includes(f)) : dir.entries;
    const items = [];
    if (dir.parent && !f) items.push(folderItem(dir.parent, null));
    for (const e of shown) items.push(folderItem(joinPath(dir.path, e.name), e));
    if (!shown.length) items.push(pickerNote(f ? `no folder matches "${f}"` : 'no subfolders here'));
    if (dir.truncated) items.push(pickerNote(`only the first ${dir.entries.length} folders are listed: filter or type a path`));
    ul.replaceChildren(...items);
  }

  // folderItem is a row that opens path; e is null for the parent folder.
  function folderItem(path, e) {
    const b = h('button', 'touch:min-h-11 group flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-[13px] text-zinc-300 hover:bg-ink-800 hover:text-zinc-50 focus-visible:bg-ink-800 focus-visible:text-zinc-50 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-emerald-400/60');
    b.type = 'button';
    if (!e) {
      b.append(h('span', 'text-zinc-500', '../'), h('span', 'font-sans text-xs text-zinc-600', 'parent folder'));
    } else {
      b.append(h('span', 'min-w-0 truncate', e.name, h('span', 'text-zinc-600', '/')));
      if (e.git) b.append(chip('git', 'inline-flex shrink-0 items-center rounded border border-violet-400/25 bg-violet-400/10 px-1.5 py-px font-mono text-[10px] text-violet-300'));
      const r = rootAt(path);
      if (r) b.append(chip('root: ' + r.name, 'inline-flex shrink-0 items-center rounded-full bg-emerald-400/10 px-2 py-px font-sans text-[10px] font-medium text-emerald-300 ring-1 ring-inset ring-emerald-400/30'));
    }
    b.append(h('span', 'ml-auto shrink-0 pl-2 text-zinc-700 group-hover:text-zinc-400 group-focus-visible:text-zinc-400', '→'));
    b.addEventListener('click', () => {
      browse(path);
      if (!touch()) $('picker-filter').focus();
    });
    return h('li', '', b);
  }

  async function addRoot(ev) {
    ev.preventDefault();
    const dir = picker.dir;
    if (!dir || picker.loading || picker.busy || rootAt(dir.path)) return;
    const name = $('picker-name').value.trim();
    if (/[/\\]/.test(name)) {
      picker.error = 'a root name cannot contain / or \\';
      renderPicker();
      return;
    }
    const adapters = [...$('picker-adapters').querySelectorAll('input:checked')].map((i) => i.value);
    const host = picker.host;
    picker.busy = true;
    picker.error = '';
    renderPicker();
    try {
      const r = await api('POST', hostAPI('roots'), { path: dir.path, name, adapters, trust: $('picker-trust').checked });
      if (host) {
        toast(`added root ${r.root.name} on ${hostName(host)} → ${shortPath(r.root.path)}`);
      } else {
        markChanged('root:' + r.root.name);
        toast(`added root ${r.root.name} → ${shortPath(r.root.path)}`);
      }
      closePicker();
    } catch (e) {
      picker.error = e.message;
    }
    picker.busy = false;
    renderPicker();
  }

  function wirePicker() {
    const dlg = $('picker');
    const filter = $('picker-filter');
    const list = $('picker-list');
    $('roots-add').addEventListener('click', openPicker);
    $('signout').addEventListener('click', signOut);
    $('picker-form').addEventListener('submit', addRoot);
    $('picker-close').addEventListener('click', closePicker);
    $('picker-cancel').addEventListener('click', closePicker);
    $('picker-home').addEventListener('click', () => browse(''));
    $('picker-go').addEventListener('click', goToTyped);
    $('picker-hidden').addEventListener('change', () => browse(picker.dir ? picker.dir.path : ''));
    $('picker-hosts').addEventListener('change', (ev) => {
      if (ev.target.name === 'picker-host') selectHost(ev.target.value);
    });
    $('picker-path').addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault();
        goToTyped();
      }
    });
    filter.addEventListener('input', renderPickerList);
    filter.addEventListener('keydown', (ev) => {
      const first = list.querySelector('button');
      if (ev.key === 'ArrowDown' && first) {
        ev.preventDefault();
        first.focus();
      } else if (ev.key === 'Enter') {
        ev.preventDefault(); // never add by accident; with a filter, open the first match
        if (filter.value.trim() && first) first.click();
      } else if (ev.key === 'Backspace' && !filter.value && picker.dir && picker.dir.parent) {
        ev.preventDefault();
        browse(picker.dir.parent);
      } else if (ev.key === 'Escape' && filter.value) {
        ev.preventDefault(); // clear the filter instead of closing the dialog
        filter.value = '';
        renderPickerList();
      }
    });
    list.addEventListener('keydown', (ev) => {
      const buttons = [...list.querySelectorAll('button')];
      const i = buttons.indexOf(document.activeElement);
      if (ev.key === 'ArrowDown') {
        ev.preventDefault();
        if (i + 1 < buttons.length) buttons[i + 1].focus();
      } else if (ev.key === 'ArrowUp') {
        ev.preventDefault();
        if (i > 0) buttons[i - 1].focus();
        else filter.focus();
      } else if (ev.key === 'Backspace' && picker.dir && picker.dir.parent) {
        ev.preventDefault();
        browse(picker.dir.parent);
        filter.focus();
      }
    });
    dlg.addEventListener('close', () => {
      picker.open = false;
      picker.seq++; // drop replies still in flight
      picker.loading = false;
      clearTimeout(joinTimer);
    });
    // Close on a click on the backdrop, but not when a text selection
    // started inside the dialog ends outside it.
    let downOutside = false;
    dlg.addEventListener('mousedown', (ev) => {
      downOutside = ev.target === dlg;
    });
    dlg.addEventListener('click', (ev) => {
      if (downOutside && ev.target === dlg) closePicker();
    });
  }

  // ----------------------------------------------------------- other fleets
  //
  // With the admin token, the agents of every other fleet that holds this
  // fleet's key are listed too, polled through /api/hosts/<id>/agents while
  // the page is visible. Fleets without the key are asked again now and then.

  const REMOTE_POLL_MS = 4000;
  const REMOTE_RETRY_ROUNDS = 5; // not joined or unreachable: ask every 5th round
  const REMOTE_MAX_FAILS = 3; // rounds a failing fleet keeps its agents listed
  let remoteTimer = 0;
  let remoteBusy = false;
  let remoteRound = 0;

  function scheduleRemote(delay) {
    clearTimeout(remoteTimer);
    if (admin) remoteTimer = setTimeout(pollRemote, delay);
  }

  async function pollRemote() {
    if (!admin || remoteBusy) return;
    if (document.hidden) {
      scheduleRemote(REMOTE_POLL_MS);
      return;
    }
    remoteBusy = true;
    const round = remoteRound++;
    const hosts = pickerHosts().filter((x) => x.id);
    let changed = false;
    for (const id of [...S.remote.keys()]) {
      if (!hosts.some((x) => x.id === id)) changed = S.remote.delete(id) || changed;
    }
    await Promise.all(hosts.map(async ({ id }) => {
      const st = picker.hosts.get(id);
      if ((st === 'not_joined' || st === 'unreachable') && round % REMOTE_RETRY_ROUNDS) return;
      try {
        const r = await api('GET', `/api/hosts/${encodeURIComponent(id)}/agents`);
        setHostState(id, 'joined');
        changed = applyRemote(id, (r && r.agents) || []) || changed;
      } catch (e) {
        if (e.notJoined) setHostState(id, 'not_joined');
        else if (e.status === 502 || e.status === 404) setHostState(id, 'unreachable');
        const r = S.remote.get(id);
        if (r && ++r.fails >= REMOTE_MAX_FAILS) changed = S.remote.delete(id) || changed;
      }
    }));
    remoteBusy = false;
    if (changed) invalidate('agents');
    scheduleRemote(REMOTE_POLL_MS);
  }

  // applyRemote stores the agent list of fleet host and reports whether it
  // changed.
  function applyRemote(host, list) {
    const prev = S.remote.get(host);
    const next = new Map(list.map((a) => [a.id, a]));
    S.remote.set(host, { agents: next, fails: 0 });
    let changed = !prev || prev.agents.size !== next.size;
    for (const a of next.values()) {
      const old = prev && prev.agents.get(a.id);
      if (!old || !same(old, a)) {
        if (prev) markChanged('agent:' + agentKey(host, a.id));
        changed = true;
        chatAgent(host, a);
      }
    }
    return changed;
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) scheduleRemote(0);
  });

  // ------------------------------------------------------------------ usage
  //
  // Each fleet's agent CLIs, installed or not, and how much of their
  // account's plan limits (5h session, week, ...) is used, shown under the
  // fleet in "Fleet on the network". Asked once a minute while the page is
  // visible; the daemons cache the providers' answers.

  const USAGE_POLL_MS = 60000;
  let usageTimer = 0;
  let usageBusy = false;
  let usageAt = 0;

  function scheduleUsage(delay) {
    clearTimeout(usageTimer);
    if (admin) usageTimer = setTimeout(pollUsage, delay);
  }

  async function pollUsage() {
    if (!admin || usageBusy || document.hidden) return;
    usageBusy = true;
    usageAt = Date.now();
    const self = S.server && S.server.id;
    const ids = S.peers.filter((p) => p.id).map((p) => p.id);
    for (const id of [...S.usage.keys()]) if (!ids.includes(id)) S.usage.delete(id);
    await Promise.all(ids.map(async (id) => {
      try {
        const r = await api('GET', id === self ? '/api/usage' : `/api/hosts/${encodeURIComponent(id)}/usage`);
        S.usage.set(id, (r && r.agents) || []);
      } catch (e) {
        // Not joined: nothing to show. Unreachable: keep the last answer.
        if (e.notJoined || !S.usage.has(id)) S.usage.set(id, null);
      }
    }));
    usageBusy = false;
    invalidate('peers');
    scheduleUsage(USAGE_POLL_MS);
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && Date.now() - usageAt >= USAGE_POLL_MS) scheduleUsage(0);
  });

  function usageNode(id) {
    const list = admin && id && S.usage.get(id);
    if (!list || !list.length) return null;
    return h('div', 'mt-2 flex flex-col gap-1.5', ...list.map(agentUsageRow));
  }

  function agentUsageRow(a) {
    const name = h('span', 'inline-flex w-16 shrink-0 items-center gap-1.5 font-mono ' + (a.available ? 'text-zinc-200' : 'text-zinc-600'),
      h('span', 'size-1.5 shrink-0 rounded-full ' + (a.available ? 'bg-emerald-400' : 'bg-zinc-600')), a.id);
    name.title = a.available ? `${a.name} ${a.version || ''}`.trim() : `${a.name}: ${a.reason || 'not installed'}`;
    // Meters wrap under each other, not under the name.
    const meters = h('div', 'flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1');
    if (!a.available) meters.append(h('span', 'text-zinc-600', 'not installed'));
    else {
      for (const l of a.limits) meters.append(limitMeter(l));
      if (a.plan) meters.append(h('span', 'font-mono text-[11px] text-zinc-500', a.plan));
      if (a.error) {
        const e = h('span', 'min-w-0 truncate text-zinc-500', 'usage unknown');
        e.title = a.error;
        meters.append(e);
      }
    }
    return h('div', 'flex items-start gap-3 text-xs leading-4', name, meters);
  }

  // limitMeter is one usage window: label, a bar, percent used; the reset
  // time is in its tooltip.
  function limitMeter(l) {
    const pct = Math.max(0, Math.min(100, l.percent));
    const tone = pct >= 90 ? 'bg-rose-400' : pct >= 70 ? 'bg-amber-400' : 'bg-emerald-400';
    const fill = h('span', 'absolute inset-y-0 left-0 rounded-full ' + tone);
    fill.style.width = pct + '%';
    const el = h('span', 'inline-flex items-center gap-1.5 whitespace-nowrap',
      h('span', 'text-zinc-500', l.label),
      h('span', 'relative h-1.5 w-10 overflow-hidden rounded-full bg-ink-700', fill),
      h('span', 'font-mono tabular-nums ' + (pct >= 90 ? 'text-rose-300' : 'text-zinc-300'), Math.round(pct) + '%'));
    el.title = `${l.label}: ${Math.round(pct)}% used` +
      (l.resetsAtMs ? `, resets in ${dur(l.resetsAtMs - Date.now()).replace(/ \d+s$/, '')} (${new Date(l.resetsAtMs).toLocaleString()})` : '');
    return el;
  }

  // --------------------------------------------------------------- launcher
  //
  // Start a session: machine, agent CLI, a folder inside one of its roots,
  // an optional prompt, with images. Other machines are reached through
  // /api/hosts/.

  const launch = {
    open: false,
    host: '', // server id; '' = this daemon
    adapters: null, // [{id, name, available, images, models, efforts}] of the machine
    roots: null, // roots of another machine (this one's are S.roots)
    root: '', // chosen folder: a root name
    sub: '', // and a folder inside it, relative, '' = the root itself
    look: null, // {root, sub} the folder list looks into; null = recent folders and roots
    dirs: new Map(), // dirKey -> {root, sub, entries, truncated, error}, entries null while listed
    adapter: '',
    gen: 0, // bumped for another machine or on close: stale replies are dropped
    busy: false, // a start is in flight
    error: '',
    listSig: '', // what the folder list shows (see renderLaunchList)
    listView: '', // and which list: a new one scrolls back to the top
    listShown: false, // the chosen folder was scrolled into view in it
    modelSig: '', // what the model pickers show (see renderLaunchModel)
    then: '', // what to show once started, if a shortcut says so
    images: [], // {data, url, name} to send with the prompt (see readImages)
    seq: 0, // bumped on each opening: images read for an earlier one are dropped
  };

  const launchAPI = (path) => (launch.host ? `/api/hosts/${encodeURIComponent(launch.host)}/${path}` : '/api/' + path);
  const launchRoots = () => (launch.host ? launch.roots || [] : S.roots);
  const launchRoot = () => launchRoots().find((r) => r.name === launch.root);
  const dirKey = (root, sub) => JSON.stringify([root, sub]);
  const inRoot = (r, sub) => (sub ? joinPath(r.path, sub) : r.path);

  // openLaunch opens the dialog; opts.root (with opts.host, opts.sub)
  // chooses the folder to start in.
  function openLaunch(opts) {
    if (!admin) return;
    launch.open = true;
    launch.error = '';
    launch.busy = false;
    if (opts && opts.root) {
      launch.host = opts.host || '';
      launch.root = opts.root;
      launch.sub = opts.sub || '';
    }
    if (launch.host && !pickerHosts().some((x) => x.id === launch.host)) launch.host = '';
    launch.seq++;
    launch.images = [];
    renderThumbs($('launch-images'), launch.images);
    $('launch-prompt').value = '';
    $('launch-search').value = '';
    // Key hints only where there are keys.
    $('launch-prompt').placeholder = touch() ? 'What should the agent do?' : `What should the agent do? · ${MOD}+Enter starts`;
    $('launch-search').placeholder = touch() ? 'Search folders' : 'Search folders · ↓ to move · → to look inside';
    $('launch-name').value = '';
    $('launch-branch').value = '';
    $('launch-more').open = false;
    renderLaunchMore();
    renderLaunchOptions();
    renderLaunchKeys();
    $('launch').showModal();
    $('launch-body').scrollTop = 0;
    fitHeight($('launch-prompt'));
    renderLaunchHosts();
    checkHosts();
    loadLaunchHost();
    if (!touch()) $('launch-prompt').focus();
  }

  // Checkout, sandbox and what to show once started: click chips, kept
  // for the next session.
  const LAUNCH_OPTS = 'fleet.launch.options';
  const LAUNCH_CHOICES = {
    iso: [['', 'Auto', 'an own worktree in git repos, else the folder itself'], ['worktree', 'Own worktree', 'a git worktree on a branch of its own'], ['pinned', 'The folder itself', 'works in the folder, next to you and other agents']],
    sandbox: [['', 'Server default', "the server's [sandbox] setting"], ['docker', 'Docker', 'in a Docker container'], ['none', 'None', 'right on the server']],
    then: [['chat', 'Show chat', 'open the conversation'], ['screen', 'Show screen', 'open the conversation with the terminal screen'], ['stay', 'Stay here', 'keep this dialog closed and the list in view']],
  };
  const MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  const MOD = MAC ? '⌘' : 'Ctrl';

  const checkedValue = (id) => ($(id).querySelector('input:checked') || { value: '' }).value;

  function launchOpts() {
    try {
      return JSON.parse(localStorage.getItem(LAUNCH_OPTS)) || {};
    } catch {
      return {};
    }
  }

  function renderLaunchOptions() {
    const saved = launchOpts();
    for (const [key, choices] of Object.entries(LAUNCH_CHOICES)) {
      const pick = choices.some(([v]) => v === saved[key]) ? saved[key] : choices[0][0];
      refill($('launch-' + key), ...choices.map(([v, label, title]) => radioChip('launch-' + key, v, label, v === pick, title)));
    }
  }

  function saveLaunchOptions() {
    const o = {};
    for (const key of Object.keys(LAUNCH_CHOICES)) o[key] = checkedValue('launch-' + key);
    try {
      localStorage.setItem(LAUNCH_OPTS, JSON.stringify(o));
    } catch {
      // private mode: kept until the dialog opens again
    }
  }

  // renderLaunchMore sums up the name and branch while they are folded.
  function renderLaunchMore() {
    const set = [$('launch-name').value.trim(), $('launch-branch').value.trim()].filter(Boolean);
    $('launch-more-note').textContent = '· ' + (set.length ? set.join(' · ') : 'generated');
  }

  function renderLaunchKeys() {
    const box = $('launch-keys');
    box.hidden = touch();
    if (box.hidden) return;
    const key = (k, what) => h('span', '', h('kbd', 'rounded border border-ink-600 bg-ink-850 px-1 font-mono text-[10px] text-zinc-400', k), ' ' + what);
    box.replaceChildren(key(MOD + '+Enter', 'start'), key(MOD + '+Shift+Enter', 'start, show screen'),
      key((MAC ? '⌥' : 'Alt+') + '1–9', 'agent'), key('Esc', 'close'));
  }

  function closeLaunch() {
    const d = $('launch');
    if (d.open) d.close();
  }

  // loadLaunchHost loads the adapters and roots of the chosen machine.
  async function loadLaunchHost() {
    const host = launch.host;
    const gen = ++launch.gen;
    launch.adapters = null;
    launch.roots = null;
    launch.dirs = new Map();
    launch.look = null;
    launch.error = '';
    renderLaunch();
    let err = '';
    const fail = (e) => {
      err = err || e.message;
      if (host && e.notJoined) setHostState(host, 'not_joined');
      return null;
    };
    const [ads, roots] = await Promise.all([
      api('GET', launchAPI('adapters')).then((r) => (r && r.adapters) || []).catch(fail),
      host ? api('GET', launchAPI('roots')).then((r) => (r && r.roots) || []).catch(fail) : null,
    ]);
    if (gen !== launch.gen) return;
    if (host && ads) setHostState(host, 'joined');
    launch.adapters = ads || [];
    launch.roots = roots;
    launch.error = err;
    launchRootsChanged();
  }

  // launchRootsChanged lists new roots for the search, and chooses the
  // last folder a session started in if the chosen root is gone.
  function launchRootsChanged() {
    if (!launchRoot()) {
      const r = launchRoots()[0];
      const pick = launchRecent(1)[0] || (r ? { root: r.name, sub: '' } : { root: '', sub: '' });
      launch.root = pick.root;
      launch.sub = pick.sub;
    }
    if (launch.look && !launchRoots().some((r) => r.name === launch.look.root)) launch.look = null;
    for (const r of launchRoots()) listDir(r.name, '');
    renderLaunch();
  }

  // listDir lists the subfolders of a folder into launch.dirs, once per
  // opening of the dialog: the search looks through every listing.
  async function listDir(root, sub) {
    const key = dirKey(root, sub);
    const r = launchRoots().find((x) => x.name === root);
    if (!r || launch.dirs.has(key)) return;
    const gen = launch.gen;
    const d = { root, sub, entries: null, truncated: false, error: '' };
    launch.dirs.set(key, d);
    try {
      const dir = await api('GET', launchAPI('fs?' + new URLSearchParams({ path: inRoot(r, sub) })));
      d.entries = dir.entries || [];
      d.truncated = !!dir.truncated;
    } catch (e) {
      d.entries = [];
      d.error = e.message;
    }
    if (gen === launch.gen) renderLaunch();
  }

  // launchRecent is the folders sessions started in on the chosen machine,
  // newest first. It keeps its last 200 sessions, so every browser and
  // phone sees the same ones. The list shows five, the search finds all.
  const LAUNCH_RECENT = 5;

  function launchRecent(max) {
    const agents = launch.host ? (S.remote.get(launch.host) || { agents: new Map() }).agents : S.agents;
    const out = [];
    const seen = new Set();
    for (const a of [...agents.values()].sort((x, y) => y.createdAtMs - x.createdAtMs)) {
      const r = launchRoots().find((x) => x.name === a.root);
      if (!r || a.isolation === 'clone' || !a.path) continue; // a clone runs outside the root
      const base = joinPath(r.path, '');
      const sub = a.path === r.path ? '' : a.path.startsWith(base) ? a.path.slice(base.length) : null;
      const key = sub == null ? '' : dirKey(r.name, sub);
      if (!key || seen.has(key)) continue;
      seen.add(key);
      out.push({ root: r.name, sub });
      if (out.length === max) break;
    }
    return out;
  }

  function selectLaunchHost(id) {
    if (launch.host === id) return;
    launch.host = id;
    launch.root = '';
    launch.sub = '';
    $('launch-search').value = '';
    renderLaunchHosts();
    loadLaunchHost();
  }

  function renderLaunchHosts() {
    const box = $('launch-hosts');
    box.replaceChildren(...pickerHosts().map((x) => {
      const rb = h('input', 'sr-only');
      rb.type = 'radio';
      rb.name = 'launch-host';
      rb.value = x.id;
      rb.checked = x.id === launch.host;
      const [dot, note] = HOST_STATE[x.id ? picker.hosts.get(x.id) || 'checking' : 'self'];
      return h('label', HOST_CHIP, rb, h('span', dot), x.name,
        note ? h('span', 'font-sans text-[11px] text-zinc-500', note) : null);
    }));
    $('launch-host').textContent = hostName(launch.host);
  }

  // refill replaces the chips of a radio group, and keeps the focus on
  // the checked one if it was in the group (arrow keys move through it).
  function refill(box, ...chips) {
    const had = box.contains(document.activeElement);
    box.replaceChildren(...chips);
    const checked = had && box.querySelector('input:checked');
    if (checked) checked.focus();
  }

  function radioChip(name, value, label, checked, title) {
    const rb = h('input', 'sr-only');
    rb.type = 'radio';
    rb.name = name;
    rb.value = value;
    rb.checked = checked;
    const chipEl = h('label', HOST_CHIP, rb, label);
    if (title) chipEl.title = title;
    return chipEl;
  }

  // Model and effort picks, per agent CLI, kept for the next session.
  const LAUNCH_PICKS = 'fleet.launch.models';

  function launchPicks() {
    try {
      return JSON.parse(localStorage.getItem(LAUNCH_PICKS)) || {};
    } catch {
      return {};
    }
  }

  // launchPick returns the model and effort chosen for the chosen agent
  // CLI, as far as it still offers them: {ad, model, effort, efforts}.
  function launchPick() {
    const ad = (launch.adapters || []).find((a) => a.id === launch.adapter);
    // Daemons older than model choices send none.
    if (!ad || (!(ad.models || []).length && !(ad.efforts || []).length)) return null;
    const pick = launchPicks()[ad.id] || {};
    const model = (ad.models || []).find((m) => m.id === pick.model);
    const efforts = model ? (ad.efforts || []).filter((e) => model.efforts.includes(e.id)) : ad.efforts || [];
    const effort = efforts.find((e) => e.id === pick.effort);
    return { ad, model, effort, efforts };
  }

  // renderLaunchModel offers the chosen agent's models, and the efforts of
  // the chosen model. "Default" leaves either to the CLI's own settings.
  function renderLaunchModel() {
    const p = launchPick();
    $('launch-model-row').hidden = !p;
    if (!p) return;
    // Rebuilt only when they change: the dialog renders again as folders
    // are listed, and would swallow a click on a chip.
    const sig = JSON.stringify([p.ad.id, p.ad.models, p.ad.efforts, p.model && p.model.id, p.effort && p.effort.id]);
    if (sig === launch.modelSig) return;
    launch.modelSig = sig;
    const dflt = "the CLI's own setting";
    refill($('launch-model'), radioChip('launch-model', '', 'Default', !p.model, dflt),
      ...(p.ad.models || []).map((m) => radioChip('launch-model', m.id, m.label, p.model === m)));
    if (!p.efforts.length) {
      refill($('launch-effort'), h('span', 'text-xs text-zinc-600', p.model ? `${p.model.label} has no effort levels` : 'none'));
    } else {
      refill($('launch-effort'), radioChip('launch-effort', '', 'Default', !p.effort, dflt),
        ...p.efforts.map((e) => radioChip('launch-effort', e.id, e.label, p.effort === e)));
    }
  }

  function saveLaunchPick() {
    const picks = launchPicks();
    picks[launch.adapter] = { model: checkedValue('launch-model'), effort: checkedValue('launch-effort') };
    try {
      localStorage.setItem(LAUNCH_PICKS, JSON.stringify(picks));
    } catch {
      // private mode: the pick lasts until the dialog renders again
    }
    renderLaunch();
  }

  // launchImages reports whether the chosen agent CLI takes images with
  // its first prompt (daemons older than that say nothing).
  function launchImages() {
    const ad = (launch.adapters || []).find((a) => a.id === launch.adapter);
    return !!(ad && ad.images);
  }

  // launchImagesProblem says why the images cannot go with the session.
  function launchImagesProblem() {
    if (!launch.images.length) return '';
    if (!launchImages()) return `${launch.adapter || 'this agent'} takes no images at start: remove them, or choose another agent.`;
    if (!$('launch-prompt').value.trim()) return 'Images go with a prompt: write one.';
    return '';
  }

  async function addLaunchImages(files) {
    if (!launch.open || launch.busy || !launchImages()) return;
    const seq = launch.seq;
    if (await readImages(files, launch.images, () => launch.open && seq === launch.seq)) {
      renderThumbs($('launch-images'), launch.images);
      renderLaunch();
    }
  }

  // pickAdapter keeps the chosen adapter if the root allows it and it is
  // installed, else takes Claude Code, Codex or the first one that is.
  function pickAdapter(list) {
    const ok = (id) => list.some((a) => a.id === id && a.available);
    if (ok(launch.adapter)) return;
    launch.adapter = ['claude', 'codex'].find(ok) || (list.find((a) => a.available) || {}).id || '';
  }

  function renderLaunch() {
    if (!launch.open) return;
    const root = launchRoot();
    const notJoined = launch.host && picker.hosts.get(launch.host) === 'not_joined';

    // agents: those the root allows
    const box = $('launch-adapters');
    const allowed = (launch.adapters || []).filter((a) => !root || !root.adapters.length || root.adapters.includes(a.id));
    pickAdapter(allowed);
    if (!launch.adapters) box.replaceChildren(h('span', 'text-xs text-zinc-600', 'loading…'));
    else if (!allowed.length) box.replaceChildren(h('span', 'text-xs text-zinc-600', 'no agent CLI allowed here'));
    else {
      const sig = JSON.stringify([allowed, launch.adapter, launch.host]);
      if (box.dataset.sig !== sig) {
        box.dataset.sig = sig;
        refill(box, ...allowed.map((a) => {
          const label = radioChip('launch-adapter', a.id, a.name || a.id, a.id === launch.adapter);
          if (!a.available) {
            label.querySelector('input').disabled = true;
            label.className = 'touch:min-h-11 inline-flex cursor-not-allowed select-none items-center gap-1.5 rounded-md border border-ink-700 bg-ink-900 px-2 py-1 font-mono text-[12px] text-zinc-600';
            label.append(h('span', 'font-sans text-[11px]', 'not installed'));
            label.title = `${a.name} is not installed on ${hostName(launch.host)}`;
          }
          return label;
        }));
      }
    }
    if (!launch.adapters || !allowed.length) delete box.dataset.sig;

    renderLaunchModel();

    const folder = $('launch-folder');
    folder.textContent = root ? `${root.name}:${launch.sub}` : '';
    folder.title = root ? shortPath(inRoot(root, launch.sub)) : '';
    renderLaunchCrumbs();
    renderLaunchList(notJoined);

    $('launch-attach').disabled = !launchImages() || launch.busy;
    $('launch-attach').title = launchImages() || !launch.adapter
      ? 'Attach images to the prompt (or paste / drop them)'
      : `${launch.adapter} takes no images at start`;
    const problem = launchImagesProblem();

    const st = $('launch-status');
    const roots = launchRoots();
    if (notJoined) {
      st.className = 'min-h-5 min-w-0 basis-full text-xs text-amber-200/90 sm:basis-0 sm:flex-1';
      st.textContent = `${hostName(launch.host)} does not have this fleet's key: run \`fleet start --join <key>\` there (see Add folder).`;
    } else if (launch.error) {
      st.className = 'min-h-5 min-w-0 basis-full text-xs text-rose-300 sm:basis-0 sm:flex-1';
      st.textContent = launch.error;
    } else if (problem) {
      st.className = 'min-h-5 min-w-0 basis-full text-xs text-amber-200/90 sm:basis-0 sm:flex-1';
      st.textContent = problem;
    } else if (root) {
      st.className = 'min-h-5 min-w-0 basis-full break-words text-xs text-zinc-500 sm:basis-0 sm:flex-1';
      const p = launchPick();
      const runs = p && [p.model && p.model.label, p.effort && p.effort.label + ' effort'].filter(Boolean).join(' · ');
      const n = launch.images.length;
      st.replaceChildren(h('span', '', 'starts ', h('span', 'font-mono text-zinc-200', launch.adapter || '…'),
        runs ? h('span', 'text-zinc-400', ` (${runs})`) : null, n ? ` with ${n} image${n > 1 ? 's' : ''}` : '', ' in ',
        h('span', 'font-mono text-zinc-200', ...pathBreaks(shortPath(inRoot(root, launch.sub))))));
    } else {
      st.className = 'min-h-5 min-w-0 basis-full text-xs text-zinc-500 sm:basis-0 sm:flex-1';
      st.textContent = launch.adapters && !roots.length ? 'Add a root folder on this machine first (Roots → Add folder).' : '';
    }
    const start = $('launch-start');
    start.disabled = !root || !launch.adapter || launch.busy || notJoined || !!problem;
    start.textContent = launch.busy ? 'Starting…' : 'Start';
  }

  // The folder list: recent folders and roots, the search results, or the
  // subfolders of one folder. A click on a row chooses it, › looks inside.

  function chooseFolder(root, sub) {
    launch.root = root;
    launch.sub = sub;
    renderLaunch();
  }

  // lookInto shows the subfolders of root/sub; a null root goes back to
  // recent folders and roots.
  function lookInto(root, sub) {
    launch.look = root == null ? null : { root, sub };
    $('launch-search').value = '';
    if (launch.look) listDir(root, sub);
    renderLaunch();
    if (!touch()) $('launch-search').focus();
  }

  function lookUp() {
    const l = launch.look;
    if (!l) return;
    if (!l.sub) lookInto(null);
    else lookInto(l.root, l.sub.split('/').slice(0, -1).join('/'));
  }

  // folderRow describes the row of root/sub: its name, where it is, and
  // whether it is a git repo (if not given, as its parent's listing says).
  function folderRow(root, sub, git) {
    const r = launchRoots().find((x) => x.name === root);
    const parts = sub ? sub.split('/') : [];
    const name = parts.length ? parts[parts.length - 1] : root;
    if (git == null && parts.length) {
      const d = launch.dirs.get(dirKey(root, parts.slice(0, -1).join('/')));
      git = d && d.entries && (d.entries.find((e) => e.name === name) || {}).git;
    }
    return {
      root, sub, name, git: !!git,
      where: parts.length ? [root, ...parts.slice(0, -1)].join('/') : shortPath(r ? r.path : ''),
    };
  }

  const LAUNCH_FOUND_MAX = 50;

  // launchRows is what the folder list shows, as plain data (compared
  // before the list is rebuilt).
  function launchRows(notJoined) {
    const note = (text) => ({ note: text });
    if (notJoined) return [note('not in this fleet yet')];
    if (!launch.adapters) return [note('loading…')];
    const roots = launchRoots();
    if (!roots.length) return [note('no roots on this machine')];
    const q = $('launch-search').value.trim().toLowerCase();
    if (q) return searchRows(q);

    const l = launch.look;
    if (l) {
      const d = launch.dirs.get(dirKey(l.root, l.sub));
      const rows = [{ ...folderRow(l.root, l.sub), where: 'this folder', here: true }];
      if (!d || !d.entries) rows.push(note('loading…'));
      else if (d.error) rows.push(note(d.error));
      else {
        for (const e of d.entries) rows.push({ ...folderRow(l.root, l.sub ? l.sub + '/' + e.name : e.name, e.git), where: '' });
        if (!d.entries.length) rows.push(note('no subfolders'));
        if (d.truncated) rows.push(note(`only the first ${d.entries.length} folders are listed: search to find others`));
      }
      return rows;
    }

    const rows = [];
    const recent = launchRecent(LAUNCH_RECENT);
    if (recent.length) rows.push({ head: 'Recent' }, ...recent.map((f) => folderRow(f.root, f.sub)));
    rows.push({ head: 'Roots' }, ...roots.map((r) => folderRow(r.name, '')));
    return rows;
  }

  // searchRows finds folders whose root:path has every word of q: folders
  // of past sessions, roots, the folders in them and in every folder
  // looked into.
  function searchRows(q) {
    const words = q.split(/\s+/);
    const last = words[words.length - 1];
    const found = new Map();
    const add = (root, sub, git, rank) => {
      const key = dirKey(root, sub);
      if (found.has(key)) return;
      const hay = (root + ':' + sub).toLowerCase();
      if (!words.every((w) => hay.includes(w))) return;
      const row = folderRow(root, sub, git);
      const name = row.name.toLowerCase();
      const score = name === last ? 0 : name.startsWith(last) ? 1 : name.includes(last) ? 2 : 3;
      found.set(key, { row, score, rank, depth: sub ? sub.split('/').length : 0 });
    };
    const recent = launchRecent();
    recent.forEach((f, i) => add(f.root, f.sub, null, i));
    for (const r of launchRoots()) add(r.name, '', null, recent.length);
    let listing = false;
    for (const d of launch.dirs.values()) {
      if (!d.entries) listing = true;
      else for (const e of d.entries) add(d.root, d.sub ? d.sub + '/' + e.name : e.name, e.git, recent.length);
    }
    const hits = [...found.values()].sort((a, b) => a.score - b.score || a.rank - b.rank || a.depth - b.depth ||
      a.row.name.localeCompare(b.row.name));
    const rows = hits.slice(0, LAUNCH_FOUND_MAX).map((x) => x.row);
    if (hits.length > LAUNCH_FOUND_MAX) rows.push({ note: `${hits.length - LAUNCH_FOUND_MAX} more: type more to narrow them down` });
    if (listing) rows.push({ note: 'still listing folders…' });
    else if (!hits.length) rows.push({ note: `no folder matches "${q}": look inside one (›) to search its folders too` });
    return rows;
  }

  const LAUNCH_ROW = 'touch:min-h-11 flex min-w-0 flex-1 items-center gap-2 rounded px-2 py-1 text-left font-mono text-[12.5px] text-zinc-300 hover:bg-ink-800 hover:text-zinc-50 focus-visible:bg-ink-800 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-emerald-400/60 aria-pressed:bg-emerald-400/10 aria-pressed:text-emerald-100 aria-pressed:hover:bg-emerald-400/15';
  const LAUNCH_LOOK = 'touch:min-h-11 touch:min-w-11 shrink-0 rounded px-2.5 font-mono text-sm text-zinc-600 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-emerald-400/60';
  const GIT_CHIP = 'inline-flex shrink-0 items-center rounded border border-violet-400/25 bg-violet-400/10 px-1.5 py-px font-mono text-[10px] text-violet-300';

  function renderLaunchList(notJoined) {
    const ul = $('launch-list');
    const rows = launchRows(notJoined);
    const view = JSON.stringify([launch.host, $('launch-search').value.trim(), launch.look]);
    const sig = JSON.stringify([view, rows, launch.root, launch.sub]);
    if (sig === launch.listSig) return;
    const sameView = view === launch.listView;
    launch.listSig = sig;
    launch.listView = view;
    const focused = ul.contains(document.activeElement) ? document.activeElement.dataset.key : '';
    ul.replaceChildren(...rows.map((row) => {
      if (row.head) return h('li', 'px-2 pb-1 pt-2.5 text-[10px] font-medium uppercase tracking-wider text-zinc-600 first:pt-1', row.head);
      if (row.note) return h('li', 'px-2 py-4 text-center font-mono text-xs text-zinc-600', '// ' + row.note);
      const chosen = row.root === launch.root && row.sub === launch.sub;
      const b = h('button', LAUNCH_ROW,
        h('span', 'w-3 shrink-0 text-emerald-400', chosen ? '✓' : ''),
        h('span', 'min-w-0 max-w-[70%] shrink-0 truncate', row.name, row.here ? '' : h('span', 'text-zinc-600', '/')),
        row.git ? chip('git', GIT_CHIP) : null,
        h('span', 'ml-auto min-w-0 truncate pl-2 font-sans text-[11px] text-zinc-500', row.where));
      b.type = 'button';
      b.dataset.key = dirKey(row.root, row.sub);
      b.setAttribute('aria-pressed', String(chosen));
      const r = launchRoots().find((x) => x.name === row.root);
      if (r) b.title = shortPath(inRoot(r, row.sub));
      b.addEventListener('click', () => chooseFolder(row.root, row.sub));
      if (row.here) return h('li', 'flex', b);
      const look = h('button', LAUNCH_LOOK, '›');
      look.type = 'button';
      look.tabIndex = -1; // → does it from the keyboard
      look.setAttribute('aria-label', `Look inside ${row.name}`);
      look.title = 'look inside';
      look.addEventListener('click', () => lookInto(row.root, row.sub));
      return h('li', 'flex items-stretch gap-0.5', b, look);
    }));
    ul.classList.toggle('opacity-60', rows.length === 1 && rows[0].note === 'loading…');
    // A new list starts at the top; recent folders and roots at the chosen
    // folder once it is in them.
    if (!sameView) {
      ul.scrollTop = 0;
      launch.listShown = false;
    }
    const c = !launch.listShown && !launch.look && !$('launch-search').value.trim() && ul.querySelector('button[aria-pressed="true"]');
    if (c) {
      launch.listShown = true;
      ul.scrollTop = c.getBoundingClientRect().top - ul.getBoundingClientRect().top - ul.clientHeight / 3;
    }
    if (focused) {
      const again = [...ul.querySelectorAll('button[data-key]')].find((b) => b.dataset.key === focused);
      if (again) again.focus();
      else if (!touch()) $('launch-search').focus();
    }
  }

  function renderLaunchCrumbs() {
    const nav = $('launch-crumbs');
    const l = launch.look;
    nav.hidden = !l || !!$('launch-search').value.trim();
    if (nav.hidden) return nav.replaceChildren();
    const up = h('button', 'touch:min-h-11 touch:min-w-11 shrink-0 rounded px-2 py-0.5 text-sm text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', '‹');
    up.type = 'button';
    up.setAttribute('aria-label', l.sub ? 'Parent folder' : 'Back to recent folders and roots');
    up.title = l.sub ? 'parent folder' : 'recent folders and roots';
    up.addEventListener('click', lookUp);
    const crumb = (label, sub, current) => {
      if (current) {
        const el = h('span', 'min-w-0 truncate rounded px-1 py-0.5 font-semibold text-zinc-100', label);
        el.setAttribute('aria-current', 'location');
        return el;
      }
      const b = h('button', 'touch:min-h-11 shrink-0 rounded px-1 py-0.5 text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', label);
      b.type = 'button';
      b.addEventListener('click', () => lookInto(l.root, sub));
      return b;
    };
    const parts = l.sub ? l.sub.split('/') : [];
    const items = [up, crumb(l.root + ':', '', !parts.length)];
    parts.forEach((p, i) => {
      if (i > 0) items.push(h('span', 'shrink-0 text-zinc-700', '/'));
      items.push(crumb(p, parts.slice(0, i + 1).join('/'), i === parts.length - 1));
    });
    nav.replaceChildren(...items);
  }

  async function startSession(ev) {
    ev.preventDefault();
    const root = launchRoot();
    if (!root || !launch.adapter || launch.busy || launchImagesProblem()) return;
    const host = launch.host;
    const then = launch.then || checkedValue('launch-then') || 'chat';
    launch.then = '';
    launch.busy = true;
    launch.error = '';
    renderLaunch();
    const pick = launchPick();
    try {
      const r = await api('POST', launchAPI('agents'), {
        model: (pick && pick.model && pick.model.id) || '',
        effort: (pick && pick.effort && pick.effort.id) || '',
        adapter: launch.adapter,
        root: root.name,
        path: launch.sub,
        prompt: $('launch-prompt').value,
        name: $('launch-name').value.trim(),
        branch: $('launch-branch').value.trim(),
        isolation: checkedValue('launch-iso'),
        sandbox: checkedValue('launch-sandbox'),
        images: launch.images.map((im) => im.data),
      });
      launch.busy = false;
      closeLaunch();
      toast(`started ${r.agent.name}${host ? ' on ' + hostName(host) : ''}`);
      if (host) {
        const rem = S.remote.get(host) || { agents: new Map(), fails: 0 };
        rem.agents.set(r.agent.id, r.agent);
        S.remote.set(host, rem);
        invalidate('agents');
      }
      if (then !== 'stay') openChat(host, r.agent.id, r.agent, { screen: then === 'screen' });
      return;
    } catch (e) {
      launch.error = e.message;
    }
    launch.busy = false;
    renderLaunch();
  }

  function wireLaunch() {
    const dlg = $('launch');
    const search = $('launch-search');
    const list = $('launch-list');
    const prompt = $('launch-prompt');
    $('agents-new').addEventListener('click', () => openLaunch());
    $('launch-form').addEventListener('submit', startSession);
    $('launch-close').addEventListener('click', closeLaunch);
    $('launch-cancel').addEventListener('click', closeLaunch);
    $('launch-hosts').addEventListener('change', (ev) => {
      if (ev.target.name === 'launch-host') selectLaunchHost(ev.target.value);
    });
    $('launch-adapters').addEventListener('change', (ev) => {
      if (ev.target.name !== 'launch-adapter') return;
      launch.adapter = ev.target.value;
      renderLaunch();
    });
    $('launch-model').addEventListener('change', saveLaunchPick);
    $('launch-effort').addEventListener('change', saveLaunchPick);
    for (const key of Object.keys(LAUNCH_CHOICES)) $('launch-' + key).addEventListener('change', saveLaunchOptions);
    $('launch-name').addEventListener('input', renderLaunchMore);
    $('launch-branch').addEventListener('input', renderLaunchMore);
    search.addEventListener('input', renderLaunch);
    search.addEventListener('keydown', (ev) => {
      const first = list.querySelector('button[data-key]');
      if (ev.key === 'ArrowDown' && first) {
        ev.preventDefault();
        first.focus();
      } else if (ev.key === 'Enter') {
        ev.preventDefault(); // never start by accident; with a search, choose the first match
        if (search.value.trim() && first) first.click();
      } else if (ev.key === 'Backspace' && !search.value && launch.look) {
        ev.preventDefault();
        lookUp();
      } else if (ev.key === 'Escape' && search.value) {
        ev.preventDefault(); // clear the search instead of closing the dialog
        search.value = '';
        renderLaunch();
      }
    });
    list.addEventListener('keydown', (ev) => {
      const rows = [...list.querySelectorAll('button[data-key]')];
      const i = rows.indexOf(document.activeElement);
      if (i < 0) return;
      if (ev.key === 'ArrowDown') {
        ev.preventDefault();
        if (i + 1 < rows.length) rows[i + 1].focus();
      } else if (ev.key === 'ArrowUp') {
        ev.preventDefault();
        (i > 0 ? rows[i - 1] : search).focus();
      } else if (ev.key === 'ArrowRight') {
        const look = rows[i].nextElementSibling;
        if (!look) return;
        ev.preventDefault();
        look.click();
      } else if ((ev.key === 'ArrowLeft' || ev.key === 'Backspace') && launch.look) {
        ev.preventDefault();
        lookUp();
      }
    });
    // The prompt grows as it is typed into; keep its end in view.
    prompt.addEventListener('input', () => {
      fitHeight(prompt);
      if (launch.images.length) renderLaunch(); // images need a prompt
      const body = $('launch-body');
      const below = prompt.getBoundingClientRect().bottom + 12 - body.getBoundingClientRect().bottom;
      if (below > 0) body.scrollTop += below;
    });
    // Anywhere in the dialog: Ctrl/Cmd+Enter starts the session, with
    // Shift it shows the screen too; Alt+1–9 chooses the agent CLI.
    dlg.addEventListener('keydown', (ev) => {
      if (ev.isComposing) return;
      if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) {
        ev.preventDefault();
        launch.then = ev.shiftKey ? 'screen' : '';
        $('launch-form').requestSubmit();
      } else if (ev.altKey && !ev.ctrlKey && !ev.metaKey && /^Digit[1-9]$/.test(ev.code)) {
        const rb = $('launch-adapters').querySelectorAll('input[name="launch-adapter"]')[Number(ev.code.slice(5)) - 1];
        if (!rb || rb.disabled) return;
        ev.preventDefault();
        rb.checked = true;
        launch.adapter = rb.value;
        renderLaunch();
      }
    });
    prompt.addEventListener('paste', (ev) => {
      const files = imageFiles(ev.clipboardData);
      if (!files.length || !launchImages()) return;
      ev.preventDefault();
      addLaunchImages(files);
    });
    $('launch-attach').addEventListener('click', () => $('launch-file').click());
    $('launch-file').addEventListener('change', (ev) => {
      addLaunchImages(ev.target.files);
      ev.target.value = '';
    });
    $('launch-images').addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-image]');
      if (!b || launch.busy) return;
      launch.images.splice(Number(b.dataset.image), 1);
      renderThumbs($('launch-images'), launch.images);
      renderLaunch();
      if (!touch()) prompt.focus();
    });
    // A file dropped anywhere on the dialog must not open in the tab.
    dlg.addEventListener('dragover', (ev) => {
      if (ev.dataTransfer && [...ev.dataTransfer.types].includes('Files')) ev.preventDefault();
    });
    dlg.addEventListener('drop', (ev) => {
      if (!ev.dataTransfer || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      if (launchImages()) addLaunchImages(ev.dataTransfer.files);
      else if (imageFiles(ev.dataTransfer).length) toast(`${launch.adapter || 'this agent'} takes no images at start`, 'warn');
    });
    dlg.addEventListener('close', () => {
      launch.open = false;
      launch.images = [];
      launch.gen++;
      launch.listSig = '';
      launch.listView = '';
    });
    let downOutside = false;
    dlg.addEventListener('mousedown', (ev) => {
      downOutside = ev.target === dlg;
    });
    dlg.addEventListener('click', (ev) => {
      if (downOutside && ev.target === dlg && !launch.busy) closeLaunch();
    });
  }

  // ------------------------------------------------------------------- chat
  //
  // One session: the conversation parsed from the agent's transcript
  // (GET .../chat, long-polled), its terminal screen for dialogs the chat
  // does not show, keys and messages typed into it, and stop. A session
  // that runs workflows gets a Workflows tab (see workflows below).

  const chat = {
    open: false,
    host: '',
    id: '',
    agent: null,
    seq: 0, // bumped on open and close: requests of an old session are dropped
    view: 'chat', // 'chat'; 'wf': its workflow runs; 'sub': one workflow agent
    feed: null, // the conversation (see feeds)
    screenOpen: false,
    screenAuto: true, // open the screen by itself while the agent needs input
    screenTimer: 0,
    sending: false,
    held: false, // the last message went into a dialog, without Enter
    model: null, // {model, name, effort, switch, models, efforts}; null: the agent offers no choice
    switching: false, // a model switch is in flight
    switchedAt: 0, // when the last one ended: replies read before it are stale
    confirmStop: false,
    stopping: false,
    screenAutoOpened: false, // the screen opened by itself for a dialog
    asks: [], // questions the agent waits on, first shown first (see questions)
    answered: new Set(), // ids of questions answered here: late replies may still list them
    askForm: null, // the form of asks[0]
    images: [], // {data (base64), url (data: URL), name} to send with the next message
  };

  const chatAPI = (rest) => {
    const base = chat.host ? `/api/hosts/${encodeURIComponent(chat.host)}/agents/` : '/api/agents/';
    return base + encodeURIComponent(chat.id) + '/' + rest;
  };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const chatLive = () => chat.agent && !isFinished(chat.agent);
  const chatDialog = () => chatLive() && chat.agent.state === 'needs_input';

  // A session's draft: what is typed into its message box stays in
  // localStorage until it is sent, so notes outlive closing the session
  // or reloading the page.
  const draftKey = (host, id) => 'fleet.draft.' + agentKey(host, id);
  const loadDraft = (host, id) => localStorage.getItem(draftKey(host, id)) || '';

  function saveDraft(host, id, text) {
    try {
      if (text) localStorage.setItem(draftKey(host, id), text);
      else localStorage.removeItem(draftKey(host, id));
    } catch (e) {
      // Storage full or blocked: the draft lives only in the box.
    }
  }

  // openChat shows a session. opts.view 'wf' opens its Workflows tab, with
  // run opts.run unfolded.
  function openChat(host, id, agent, opts) {
    if (!admin) return;
    closeLaunch();
    chat.seq++;
    const view = (opts && opts.view) || 'chat';
    Object.assign(chat, {
      open: true, host, id, agent: agent || null, view,
      sending: false, held: false, confirmStop: false, stopping: false, screenAuto: true,
      model: null, switching: false, switchedAt: 0, screenAutoOpened: false, asks: [], askForm: null, images: [],
    });
    chat.answered.clear();
    Object.assign(wf, { runs: [], v: '', loaded: false, sub: null, hint: (S.workflows.get(agentKey(host, id)) || []).length });
    wf.shown.clear();
    wf.known.clear();
    wf.cards.clear();
    if (opts && opts.run) wf.shown.add(opts.run);
    chat.screenOpen = !!agent && (agent.state === 'needs_input' || agent.adapter === 'shell');
    chat.screenAutoOpened = chat.screenOpen && agent.adapter !== 'shell';
    if (opts && opts.screen) Object.assign(chat, { screenOpen: true, screenAuto: false, screenAutoOpened: false });
    $('chat-input').value = loadDraft(host, id);
    renderChatImages();
    $('chat-screen-pre').textContent = '';
    $('chat-ask').replaceChildren();
    $('chat-ask').hidden = true;
    $('wf-list').replaceChildren();
    const d = $('chat');
    if (!d.open) d.showModal();
    autosize();
    renderChatHead();
    renderChatView();
    renderChatInput();
    renderWorkflows();
    stopFeed(wf.feed);
    startFeed(chat.feed);
    wfLoop(chat.seq);
    if (!touch() && view === 'chat') $('chat-input').focus();
  }

  function closeChat() {
    const d = $('chat');
    if (d.open) d.close();
  }

  // chatAgent takes a newer state of an agent from the socket or a poll.
  function chatAgent(host, a) {
    if (!chat.open || chat.host !== host || chat.id !== a.id) return;
    if (chat.agent && (a.updatedAtMs || 0) < (chat.agent.updatedAtMs || 0)) return;
    const was = chat.agent && chat.agent.state;
    chat.agent = a;
    if (a.state !== was) paintPending(chat.feed);
    if (a.state === 'needs_input' && was !== 'needs_input' && chat.screenAuto && !chat.screenOpen && !chat.asks.length) {
      chat.screenOpen = true;
      chat.screenAutoOpened = true;
      renderChatScreen();
    }
    renderChatHead();
    renderChatAsk();
    renderChatInput();
  }

  // chatEmpty is the note for a session without a conversation.
  function chatEmpty() {
    const a = chat.agent || {};
    return a.adapter === 'shell' ? 'A shell keeps no chat. Its terminal is under Screen.'
      : chatLive() ? 'No messages yet. The conversation shows up here once the first message is sent.'
      : 'This session left no conversation.';
  }

  // ------------------------------------------------------------------ feeds
  //
  // A feed shows one transcript in a log: its latest entries first, then
  // new ones as they are written (long polls), older ones on request. The
  // session's conversation is one, a workflow agent's another.

  // newFeed makes a feed. o holds its scroller and log elements; url(query)
  // of its chat requests; live() while more may be written; working()
  // while its agent works (a call without output yet is then running);
  // empty(), the note for no transcript; imageURL(query) of its images; and
  // optionally query(q) to add to its long polls, onReply(reply),
  // onStatus() when its error or entries changed, onFile() when a
  // transcript appeared or went.
  function newFeed(o) {
    return Object.assign({
      seq: 0, // bumped on start and stop: loops of an old start stop
      file: '', // transcript the offsets belong to; '' = none yet
      start: 0, // offset of the oldest entry shown
      end: 0, // offset after the newest
      loaded: false, // first reply in
      earlier: false, // loading older entries
      error: '',
      tools: new Map(), // tool call id -> {el, status, body, name, done}
      orphans: new Map(), // tool call id -> {el, output, error}: a result shown without its call
      watch: new Set(), // repaints of the nodes that show workflow runs (see applyWorkflows)
    }, o);
  }

  function startFeed(f) {
    stopFeed(f);
    Object.assign(f, { file: '', start: 0, end: 0, loaded: false, earlier: false, error: '' });
    f.log.replaceChildren(h('div', 'py-10 text-center font-mono text-xs text-zinc-600', '// loading…'));
    feedLoop(f, f.seq);
  }

  function stopFeed(f) {
    f.seq++;
    f.tools.clear();
    f.orphans.clear();
    f.watch.clear();
  }

  // feedLoop follows a transcript until the feed stops or the dialog
  // closes: the latest entries first, then long polls for more.
  async function feedLoop(f, seq) {
    let failures = 0;
    while (chat.open && seq === f.seq) {
      const q = new URLSearchParams();
      if (f.loaded) {
        q.set('file', f.file);
        q.set('after', String(f.end));
        q.set('wait', '1');
        q.set('v', String((chat.agent && chat.agent.updatedAtMs) || 0));
        if (f.query) f.query(q);
      }
      let r;
      try {
        r = await api('GET', f.url(q));
      } catch (e) {
        if (seq !== f.seq) return;
        f.error = e.message;
        if (f.onStatus) f.onStatus();
        if (e.status === 404) return; // gone
        await sleep(Math.min(10000, 1000 * 2 ** failures++));
        continue;
      }
      if (seq !== f.seq) return;
      failures = 0;
      f.error = '';
      const first = !f.loaded;
      f.loaded = true;
      if (f.onReply) f.onReply(r);
      feedApply(f, r, first);
      // A finished transcript read to the end is done.
      if (!first && !r.more && !r.entries.length && !f.live()) return;
    }
  }

  function feedApply(f, r, first) {
    const { log, scroller } = f;
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
    const hadFile = !!f.file;
    f.file = r.file || '';
    f.end = r.end || 0;
    if (hadFile !== !!f.file && f.onFile) f.onFile();
    if (r.reset || first) {
      f.start = r.start || 0;
      f.tools.clear();
      f.orphans.clear();
      f.watch.clear();
      log.replaceChildren(...feedHead(f), ...entryNodes(f, r.entries));
      if (f.onStatus) f.onStatus();
      scroller.scrollTop = scroller.scrollHeight;
      return;
    }
    const nodes = entryNodes(f, r.entries);
    if (nodes.length) {
      log.querySelector('[data-empty]')?.remove();
      log.append(...nodes);
    }
    if (f.onStatus) f.onStatus();
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  }

  // feedHead is what goes above the entries: "load earlier", or a note
  // when there is nothing to show.
  function feedHead(f) {
    if (f.start > 0) {
      const b = h('button', 'touch:min-h-11 mx-auto rounded-md border border-ink-600 bg-ink-850 px-3 py-1 text-xs text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Load earlier');
      b.type = 'button';
      b.addEventListener('click', (ev) => loadEarlier(f, ev));
      return [b];
    }
    if (!f.file) {
      const el = h('div', 'py-10 text-center', h('div', 'font-mono text-xs text-zinc-600', '// ' + f.empty()));
      el.dataset.empty = '1';
      return [el];
    }
    return [];
  }

  async function loadEarlier(f, ev) {
    if (f.earlier || f.start <= 0) return;
    const seq = f.seq;
    const btn = ev.currentTarget;
    f.earlier = true;
    btn.disabled = true;
    btn.textContent = 'Loading…';
    let r = null;
    try {
      r = await api('GET', f.url(new URLSearchParams({ file: f.file, before: String(f.start) })));
    } catch (e) {
      toast(`could not load earlier messages: ${e.message}`, 'error');
    }
    f.earlier = false;
    if (seq !== f.seq) return;
    if (!r || r.reset || r.file !== f.file) {
      btn.disabled = false;
      btn.textContent = 'Load earlier';
      return;
    }
    const scroller = f.scroller;
    const fromBottom = scroller.scrollHeight - scroller.scrollTop;
    f.start = r.start || 0;
    // Results in this page attach to the calls in it; later results whose
    // call is in it already showed on their own.
    const known = f.tools;
    f.tools = new Map();
    const nodes = entryNodes(f, r.entries);
    // Their results, if any, are already shown on their own.
    for (const t of f.tools.values()) {
      if (!t.done) t.status.className = TOOL_STATUS.stale;
      t.done = true;
    }
    for (const [k, v] of known) f.tools.set(k, v);
    btn.replaceWith(...feedHead(f), ...nodes);
    scroller.scrollTop = scroller.scrollHeight - fromBottom;
  }

  // entryNodes renders entries; a result is attached to its tool call.
  function entryNodes(f, entries) {
    const out = [];
    for (const e of entries || []) {
      const n = entryNode(f, e);
      if (n) out.push(n);
    }
    return out;
  }

  const timeTitle = (el, e) => {
    if (e.ts) el.title = new Date(e.ts).toLocaleString();
    return el;
  };

  function entryNode(f, e) {
    switch (e.kind) {
      case 'user': {
        if (f !== chat.feed) return taskNode(e);
        const imgs = e.images && e.images.length ? imagesNode(f, e.images, 'max-w-[85%] justify-end') : null;
        return timeTitle(h('div', 'flex flex-col items-end gap-1.5',
          e.text ? h('div', 'max-w-[85%] whitespace-pre-wrap break-words rounded-lg rounded-br-sm bg-emerald-400/10 px-3 py-2 text-[13.5px] leading-relaxed text-zinc-100 ring-1 ring-inset ring-emerald-400/20', e.text) : null,
          imgs), e);
      }
      case 'assistant':
        return timeTitle(md(e.text || ''), e);
      case 'tool':
        return toolNode(f, e);
      case 'task':
        return taskEndNode(f, e);
      case 'result': {
        const t = e.id && f.tools.get(e.id);
        if (t) {
          setToolOutput(t, e.output, e.error, e.images);
          return null;
        }
        // The call is before the loaded range (or, rarely, written after
        // its result): shown on its own until the call turns up.
        const el = toolNode(f, { kind: 'tool', name: 'result', text: oneLine(e.output) || (e.images ? 'image' : ''), output: e.output, error: e.error, images: e.images });
        if (e.id) f.orphans.set(e.id, { el, output: e.output, error: e.error, images: e.images });
        return el;
      }
      default:
        return timeTitle(h('div', 'whitespace-pre-wrap break-words py-0.5 text-center font-mono text-[11px] text-zinc-500', e.text || ''), e);
    }
  }

  const oneLine = (s) => ((s || '').split('\n').find((l) => l.trim()) || '').trim();

  // taskNode shows what a workflow agent was told: written by the script,
  // often pages long, so folded to its start.
  function taskNode(e) {
    const text = e.text || '';
    const body = h('div', 'whitespace-pre-wrap break-words text-[13px] leading-relaxed text-zinc-300', text);
    const box = h('div', 'min-w-0 rounded-lg border border-fuchsia-400/20 bg-fuchsia-400/5 px-3 py-2',
      h('div', 'mb-1 text-[10px] font-semibold uppercase tracking-wider text-fuchsia-300/80', 'Task'), body);
    foldLong(box, body, text, 'max-h-36', 'touch:min-h-11 mt-1.5 rounded px-1.5 py-0.5 text-xs font-medium text-fuchsia-300 hover:bg-fuchsia-400/10 hover:text-fuchsia-200 focus-visible:outline-2 focus-visible:outline-fuchsia-400');
    return timeTitle(box, e);
  }

  const TOOL_STATUS = {
    pending: 'size-2 shrink-0 rounded-full border-[1.5px] border-sky-400 border-t-transparent animate-spin motion-reduce:animate-none',
    stale: 'size-1.5 shrink-0 rounded-full bg-zinc-600',
    ok: 'size-1.5 shrink-0 rounded-full bg-emerald-400',
    error: 'size-1.5 shrink-0 rounded-full bg-rose-500',
  };

  // toolNode shows a call folded to one line. Its input and output are
  // drawn when it is first opened: most never are.
  function toolNode(f, e) {
    if (e.name === 'Workflow' && f === chat.feed) return workflowNode(f, e);
    if ((e.name === 'Agent' || e.name === 'Task') && f === chat.feed) return agentCallNode(f, e);
    return plainToolNode(f, e);
  }

  function plainToolNode(f, e) {
    const d = h('details', 'group min-w-0 rounded-md border border-ink-700 bg-ink-850/50 open:bg-ink-850');
    const status = h('span', '');
    const sum = h('summary', 'touch:min-h-11 flex cursor-pointer list-none items-center gap-2 rounded-md px-2.5 py-1.5 font-mono text-[12px] hover:bg-ink-800/60 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400',
      status,
      h('span', 'shrink-0 font-semibold text-sky-300', e.name || 'tool'),
      h('span', 'min-w-0 truncate text-zinc-400', FILE_TOOLS.has(e.name) ? relPath(e.text) : e.text || ''));
    const body = h('div', 'flex min-w-0 flex-col gap-2 border-t border-ink-700 px-2.5 py-2');
    d.append(sum, body);
    timeTitle(sum, e);
    const t = { f, el: d, status, body, name: e.name, call: e, done: false, output: null, error: false, images: null, built: false };
    d.addEventListener('toggle', () => {
      if (d.open) buildTool(t);
    });
    trackTool(f, t, e);
    return d;
  }

  // agentCallNode is an Agent call with the subagent it started, live from
  // the Subagents tab's data: what it does now, and a way to its
  // conversation. A subagent knows its call, or (a teammate) only its
  // description.
  function agentCallNode(f, e) {
    const d = plainToolNode(f, e);
    const sum = d.firstChild;
    const live = h('span', 'ml-auto flex min-w-0 flex-1 items-center justify-end gap-2 font-sans');
    sum.lastChild.classList.add('shrink-0', 'max-w-[50%]'); // its description goes first
    sum.append(live);
    const find = () => {
      let byLabel = null;
      for (const run of wf.runs) {
        if (!run.id.startsWith('agents-')) continue;
        for (const a of run.agents) {
          if (a.call && a.call === e.id) return { run, a };
          if (!a.call && !byLabel && a.label && a.label === e.text) byLabel = { run, a };
        }
      }
      return byLabel;
    };
    let shown = '';
    const paint = () => {
      const m = find();
      const sig = m ? JSON.stringify([m.run.id, m.a.status, m.a.tool, m.a.activity]) : '';
      if (sig === shown) return;
      shown = sig;
      if (!m) {
        live.replaceChildren();
        return;
      }
      const st = wfStatus(m.a.status);
      const open = h('button', 'touch:min-h-11 shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium text-fuchsia-300 hover:bg-fuchsia-400/10 hover:text-fuchsia-200 focus-visible:outline-2 focus-visible:outline-fuchsia-400', 'Open ›');
      open.type = 'button';
      open.setAttribute('aria-label', `Open the conversation of ${m.a.label || m.a.id}`);
      open.addEventListener('click', (ev) => {
        ev.preventDefault(); // not the details' toggle
        ev.stopPropagation();
        openSub(m.run.id, m.a.id);
      });
      const act = m.a.status === 'running'
        ? h('span', 'min-w-0 truncate text-[11px] text-zinc-500 max-sm:hidden',
          m.a.tool ? h('span', 'font-mono text-sky-300/80', m.a.tool + ' ') : null, m.a.activity || 'starting…')
        : null;
      live.replaceChildren(act || '', h('span', st.badge, h('span', st.dot), st.label), open);
    };
    paint();
    f.watch.add(paint);
    return d;
  }

  // Tools whose summary is a file's path.
  const FILE_TOOLS = new Set(['Read', 'Write', 'Edit', 'MultiEdit', 'NotebookEdit']);

  // relPath shortens a path in the session's folder to the part in it, so
  // the file's name shows.
  function relPath(p) {
    const cwd = chat.agent && chat.agent.cwd;
    return cwd && p && p.startsWith(cwd + '/') ? p.slice(cwd.length + 1) : shortPath(p);
  }

  // trackTool gives a call its output: its own, a result shown before the
  // call turned up, or (Claude) the result that follows.
  function trackTool(f, t, e) {
    const early = e.id && f.orphans.get(e.id);
    if (early) {
      f.orphans.delete(e.id);
      early.el.remove();
      f.tools.set(e.id, t);
      setToolOutput(t, early.output, early.error, early.images);
    } else if (e.id) {
      t.status.className = f.working() ? TOOL_STATUS.pending : TOOL_STATUS.stale;
      f.tools.set(e.id, t);
    } else {
      setToolOutput(t, e.output, e.error, e.images);
    }
  }

  // paintPending shows calls without output as running while the feed's
  // agent works, and as unfinished otherwise (interrupted, or waiting for
  // a permission).
  function paintPending(f, tools) {
    const cls = f.working() ? TOOL_STATUS.pending : TOOL_STATUS.stale;
    for (const t of (tools || f.tools).values()) if (!t.done) t.status.className = cls;
  }

  // setToolOutput gives a call what it returned. A call that returned
  // images opens to show them.
  function setToolOutput(t, output, error, images) {
    t.done = true;
    t.output = output || '';
    t.error = !!error;
    t.images = images && images.length ? images : null;
    t.status.className = error ? TOOL_STATUS.error : TOOL_STATUS.ok;
    if (t.onOutput) {
      t.onOutput();
      return;
    }
    if (error) t.el.classList.add('border-rose-500/30');
    if (t.built) addOutput(t);
    else if (t.images) t.el.open = true; // builds it (toggle)
  }

  function buildTool(t) {
    if (t.built) return;
    t.built = true;
    // A Read of an image says all in its summary: the image is what counts.
    if (t.call.detail && !(t.images && t.name === 'Read')) t.body.append(inputView(t.call));
    if (t.done) addOutput(t);
  }

  function addOutput(t) {
    if (t.output == null) return; // shown on its own (see loadEarlier)
    if (t.images) t.body.append(imagesNode(t.f, t.images));
    if (t.output) t.body.append(outputView(t));
    else if (!t.body.childElementCount) t.body.append(h('div', 'font-mono text-[11px] text-zinc-600', '// no output'));
  }

  // ---- images: pasted into a message, or returned by a tool

  // Images stay in the transcript: the page asks for each as it scrolls
  // into view (GET .../image, its data as JSON) and keeps the latest few.
  const IMG_KEEP = 48;
  const imgCache = new Map(); // url -> Promise of a data: URL, oldest first

  function loadImage(url) {
    let p = imgCache.get(url);
    if (p) imgCache.delete(url); // most recent again
    else {
      p = api('GET', url).then((r) => `data:${r.type};base64,${r.data}`);
      p.catch(() => imgCache.delete(url));
    }
    imgCache.set(url, p);
    while (imgCache.size > IMG_KEEP) imgCache.delete(imgCache.keys().next().value);
    return p;
  }

  const imgSeen = 'IntersectionObserver' in window ? new IntersectionObserver((list) => {
    for (const en of list) {
      if (!en.isIntersecting) continue;
      imgSeen.unobserve(en.target);
      en.target.loadImage();
    }
  }, { rootMargin: '600px' }) : null;

  // imagesNode shows images of feed f's entries as thumbnails, which open
  // full size.
  function imagesNode(f, images, cls) {
    const row = h('div', 'flex min-w-0 flex-wrap gap-2 ' + (cls || ''));
    for (const im of images) {
      const url = f.imageURL(new URLSearchParams({ file: f.file, line: String(im.line), n: String(im.n) }));
      const img = h('img', 'block max-h-64 max-w-full object-contain');
      img.alt = 'image';
      const b = h('button', 'flex min-h-20 min-w-28 max-w-full items-center justify-center overflow-hidden rounded-md border border-ink-600 bg-ink-950 hover:border-zinc-500 focus-visible:outline-2 focus-visible:outline-emerald-400',
        h('span', 'font-mono text-[11px] text-zinc-600', 'image…'));
      b.type = 'button';
      b.title = 'Open full size';
      b.loadImage = () => loadImage(url).then((src) => {
        img.src = src;
        b.replaceChildren(img);
      }, (e) => {
        b.replaceChildren(h('span', 'px-2 font-mono text-[11px] text-zinc-500', '// image: ' + e.message));
        b.disabled = true;
      });
      b.addEventListener('click', () => {
        if (img.src) openImage(img.src);
      });
      if (imgSeen) imgSeen.observe(b);
      else b.loadImage();
      row.append(b);
    }
    return row;
  }

  function openImage(src) {
    $('img-view-img').src = src;
    const d = $('img-view');
    if (!d.open) d.showModal();
  }

  // Tools whose output is the agent's or a page's text, in markdown.
  const MD_OUTPUT = new Set(['Agent', 'Task', 'WebFetch', 'WebSearch']);

  // inputView shows a call's input: a command, a diff, a file's content,
  // a script, a plan, or JSON.
  function inputView(e) {
    const n = e.name;
    if (n === 'ExitPlanMode') return mdView(e.detail, 'plan');
    const o = { label: 'input', text: e.detail, wrap: true, max: true };
    if (n === 'Bash' || n === 'shell') o.lang = 'sh';
    else if (n === 'Edit' || n === 'MultiEdit' || n === 'edit') {
      o.lang = 'diff';
      o.inner = extLang((e.text || '').split(', ')[0]);
    } else if (n === 'Write') o.lang = extLang(e.text);
    else if (n === 'Workflow') o.lang = 'js';
    else if (looksJSON(e.detail)) o.lang = 'json';
    return codeView(o);
  }

  // outputView shows what a call returned: a file with its line numbers
  // (Read), markdown, JSON, a diff, or text.
  function outputView(t) {
    const out = t.output;
    const o = { label: t.error ? 'error' : 'output', text: out, error: t.error, wrap: true, max: true };
    if (t.error) return codeView(o);
    // MCP tools (Codex names them server.tool) often answer in markdown.
    if (MD_OUTPUT.has(t.name) || (/^mcp__|\./.test(t.name || '') && looksMarkdown(out))) return mdView(out, o.label);
    if (t.name === 'Read' && READ_LINE.test(out)) {
      o.lang = extLang(t.call.text);
      o.read = true;
    } else if (looksJSON(out)) o.lang = 'json';
    else if (looksDiff(out)) o.lang = 'diff';
    return codeView(o);
  }

  const looksJSON = (s) => /^\s*[[{]\s*["[{\]}]/.test(s || '');
  const looksDiff = (s) => /^diff --git /m.test(s) || (/^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/m.test(s) && /^[+-]/m.test(s));
  const looksMarkdown = (s) => /^#{1,6} \S/m.test(s) || /^\s*(?:```|~~~)/m.test(s) || /^\s*\|.*\|\s*\n\s*\|?[\s:|-]*-[\s:|-]*$/m.test(s) ||
    (/\*\*[^*\n]+\*\*/.test(s) && /^\s*(?:[-*]|\d+\.) \S/m.test(s));

  // ---- the workflow a call started, and background tasks that ended

  // workflowNode shows a Workflow call as the run it started, live from
  // the Workflows tab's data: its phases, the agents at work, its totals,
  // and the script. Its output names the run.
  function workflowNode(f, e) {
    const [name, ...desc] = (e.text || '').split(': ');
    const status = h('span', '');
    const title = h('span', 'min-w-0 truncate font-mono text-[13px] font-semibold text-fuchsia-50', name || 'workflow');
    const badge = h('span', 'flex shrink-0 items-center');
    const time = h('span', 'flex items-center');
    const about = h('p', 'mt-1 text-xs leading-relaxed text-zinc-400', desc.join(': '));
    const live = h('div', 'min-w-0');
    const open = h('button', 'touch:min-h-11 shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium text-fuchsia-300 hover:bg-fuchsia-400/10 hover:text-fuchsia-200 focus-visible:outline-2 focus-visible:outline-fuchsia-400', 'Open run ›');
    open.type = 'button';
    open.hidden = true;
    const head = h('div', 'px-3.5 pb-2.5 pt-3',
      h('div', 'flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1',
        status, h('span', 'font-mono text-fuchsia-400/70', '⧉'), title, badge,
        h('span', 'ml-auto flex shrink-0 items-center gap-2', time, open)),
      about);
    const card = h('div', 'min-w-0 overflow-hidden rounded-lg border border-fuchsia-400/20 bg-fuchsia-400/[0.03]', head, live);
    timeTitle(head, e);
    const t = { el: card, status, name: e.name, call: e, done: false, output: null, error: false, run: '' };
    open.addEventListener('click', () => setView('wf', t.run));

    const paint = () => {
      const run = t.run && wf.runs.find((r) => r.id === t.run);
      open.hidden = !t.run;
      if (!run) {
        badge.replaceChildren(h('span', 'text-[11px] text-zinc-500', t.error ? '' : !t.done ? 'launching…' : t.run && wf.loaded ? 'starting…' : ''));
        return;
      }
      status.hidden = true; // the badge says it now
      title.textContent = run.name;
      if (run.description) about.textContent = run.description;
      badge.replaceChildren(wfBadge(run.status));
      time.replaceChildren(elapsed(run.startedMs, run.status === 'running' ? 0 : run.endedMs || run.updatedMs, 'font-mono text-xs tabular-nums text-zinc-400'));
      live.replaceChildren(...runLive(run));
    };
    t.onOutput = () => {
      const m = /\bRun ID: (wf_[\w-]+)/.exec(t.output);
      if (m) t.run = m[1];
      // A launch that failed, or said something else than expected.
      if (t.error || !m) head.append(h('div', 'mt-2', codeView({ label: t.error ? 'error' : 'output', text: t.output || '(no output)', error: t.error, wrap: true, max: true })));
      paint();
    };

    if (e.detail) {
      const script = h('details', 'group min-w-0 border-t border-fuchsia-400/10');
      script.append(h('summary', 'touch:min-h-11 flex cursor-pointer list-none items-center gap-2 px-3.5 py-1.5 text-[11px] text-zinc-500 hover:bg-fuchsia-400/5 hover:text-zinc-300 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-fuchsia-400',
        h('span', 'inline-block transition-transform group-open:rotate-90 motion-reduce:transition-none', '▸'),
        'Script',
        h('span', 'font-mono text-zinc-600', `${e.detail.split('\n').length} lines`)));
      script.addEventListener('toggle', () => {
        if (script.open && script.childElementCount === 1) script.append(h('div', 'px-3.5 pb-3', codeView({ text: e.detail, lang: 'js', tag: 'js', wrap: true, max: true })));
      });
      card.append(script);
    }
    trackTool(f, t, e);
    paint();
    f.watch.add(paint);
    return card;
  }

  // runLive is a run in the chat: its phases at a glance, the agents at
  // work (each opens its conversation), and its totals.
  function runLive(run) {
    const out = [phaseStrip(run)];
    const busy = run.agents.filter((a) => a.status === 'running');
    if (busy.length) {
      const list = h('ul', 'flex min-w-0 flex-col px-2 pb-2');
      for (const a of busy.slice(-3)) {
        const b = h('button', 'touch:min-h-11 group flex w-full min-w-0 items-center gap-2 rounded px-1.5 py-1 text-left text-xs hover:bg-fuchsia-400/5 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-fuchsia-400',
          h('span', 'flex w-3 shrink-0 justify-center', h('span', WF.running.dot)),
          h('span', 'shrink-0 font-mono text-[11.5px] text-zinc-200', a.label || a.id),
          h('span', 'min-w-0 flex-1 truncate text-zinc-500',
            a.tool ? h('span', 'font-mono text-sky-300/80', a.tool + ' ') : null, a.activity || 'starting…'),
          h('span', 'shrink-0 text-zinc-600 group-hover:text-zinc-300', '›'));
        b.type = 'button';
        b.addEventListener('click', () => openSub(run.id, a.id));
        b.setAttribute('aria-label', `${a.label || a.id}, at work. Open its conversation`);
        list.append(h('li', '', b));
      }
      if (busy.length > 3) list.append(h('li', 'py-0.5 pl-7 pr-1.5 text-[11px] text-zinc-600', `+${busy.length - 3} more at work`));
      out.push(list);
    }
    const done = run.counts.done + run.counts.failed + run.counts.stopped;
    out.push(h('div', 'flex flex-wrap gap-x-3 gap-y-0.5 border-t border-fuchsia-400/10 px-3.5 py-1.5 font-mono text-[11px] tabular-nums text-zinc-500',
      h('span', '', `${done}/${run.agents.length} agents`),
      run.counts.failed ? h('span', 'text-rose-300/80', `${run.counts.failed} failed`) : null,
      h('span', '', `${run.toolUses} tool calls`),
      h('span', '', `${fmtTokens(run.tokens)} tokens`),
      h('span', 'ml-auto text-zinc-600', run.id)));
    return out;
  }

  const TASK_END = {
    completed: ['min-w-0 overflow-hidden rounded-lg border border-emerald-400/20 bg-emerald-400/[0.03]', 'shrink-0 text-[11px] font-semibold uppercase tracking-wider text-emerald-300/90', 'min-w-0 border-t border-emerald-400/15 px-3.5 py-2.5'],
    failed: ['min-w-0 overflow-hidden rounded-lg border border-rose-500/30 bg-rose-500/[0.04]', 'shrink-0 text-[11px] font-semibold uppercase tracking-wider text-rose-300', 'min-w-0 border-t border-rose-500/20 px-3.5 py-2.5'],
    stopped: ['min-w-0 overflow-hidden rounded-lg border border-ink-600 bg-ink-850/40', 'shrink-0 text-[11px] font-semibold uppercase tracking-wider text-zinc-400', 'min-w-0 border-t border-ink-700 px-3.5 py-2.5'],
  };

  // taskEndNode shows that a background task ended: a workflow run, with
  // what it returned, or a background command or agent.
  function taskEndNode(f, e) {
    const st = e.error ? 'failed' : e.name === 'completed' ? 'completed' : 'stopped';
    const call = e.id ? f.tools.get(e.id) : null;
    const runOf = () => (call && call.run && wf.runs.find((r) => r.id === call.run)) ||
      wf.runs.find((r) => r.summary && r.summary === e.text);
    if (!(call && call.name === 'Workflow') && !runOf() && !/^Dynamic workflow\b/.test(e.text || '')) return bgTaskNode(e, st);
    const [boxCls, labelCls, bodyCls] = TASK_END[st];
    const title = h('span', 'min-w-0 truncate font-mono text-[12.5px] font-semibold text-zinc-100');
    const stats = h('span', 'flex items-center gap-1 font-mono text-[11px] tabular-nums text-zinc-500');
    const open = h('button', 'touch:min-h-11 shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium text-fuchsia-300 hover:bg-fuchsia-400/10 hover:text-fuchsia-200 focus-visible:outline-2 focus-visible:outline-fuchsia-400', 'Open run ›');
    open.type = 'button';
    open.addEventListener('click', () => {
      const run = runOf();
      setView('wf', run ? run.id : '');
    });
    const head = h('div', 'flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 px-3.5 py-2',
      h('span', wfStatus(st).dot), h('span', 'font-mono text-fuchsia-400/70', '⧉'),
      h('span', labelCls, `workflow ${wfStatus(st).label}`), title,
      h('span', 'ml-auto flex shrink-0 items-center gap-2', stats, open));
    const part = h('div', bodyCls);
    const box = timeTitle(h('div', boxCls, head, part), e);
    let shown = null;
    const paint = () => {
      const run = runOf();
      title.textContent = run ? run.name : call ? (call.call.text || '').split(': ')[0] : (/"(.*)"/.exec(e.text || '') || [])[1] || '';
      title.title = e.text || '';
      open.hidden = !run;
      if (run) stats.replaceChildren(`${run.agents.length} agents ·`, elapsed(run.startedMs, run.endedMs || run.updatedMs));
      // The notification cuts the result at 8000 characters; the run has
      // all of it.
      const result = (run && run.result) || e.output || '';
      if (result === shown) return;
      shown = result;
      part.hidden = !result && st !== 'failed';
      if (!result) {
        part.replaceChildren(h('pre', 'whitespace-pre-wrap break-words font-mono text-[11.5px] leading-snug text-rose-100/90', e.text || ''));
        return;
      }
      const body = h('div', 'min-w-0', resultNode(result));
      part.replaceChildren(body);
      foldLong(part, body, result, 'max-h-60', 'touch:min-h-11 mt-1.5 rounded px-1.5 py-0.5 text-xs font-medium text-emerald-300 hover:bg-emerald-400/10 hover:text-emerald-200 focus-visible:outline-2 focus-visible:outline-emerald-400');
    };
    paint();
    f.watch.add(paint);
    return box;
  }

  // bgTaskNode shows a background command or agent that ended, and what it
  // returned (an agent's report) when opened.
  function bgTaskNode(e, st) {
    const text = h('span', 'min-w-0 flex-1 break-words', e.text || '');
    if (!e.output) {
      return timeTitle(h('div', 'flex min-w-0 items-center gap-2 rounded-md border border-ink-700/70 bg-ink-850/30 px-2.5 py-1.5 text-xs text-zinc-400', h('span', wfStatus(st).dot), text), e);
    }
    const d = h('details', 'group min-w-0 rounded-md border border-ink-700/70 bg-ink-850/30 open:bg-ink-850');
    d.append(timeTitle(h('summary', 'touch:min-h-11 flex cursor-pointer list-none items-center gap-2 rounded-md px-2.5 py-1.5 text-xs text-zinc-400 hover:bg-ink-800/60 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400',
      h('span', wfStatus(st).dot), text,
      h('span', 'shrink-0 text-[10px] font-medium uppercase tracking-wider text-zinc-600 group-open:hidden', 'result')), e));
    d.addEventListener('toggle', () => {
      if (d.open && d.childElementCount === 1) d.append(h('div', 'border-t border-ink-700 px-2.5 py-2', mdView(e.output, 'result')));
    });
    return d;
  }

  // foldLong caps body, a part of box, at cls (a max height) when text is
  // long, with a button (of classes btn) to show it all.
  function foldLong(box, body, text, cls, btn) {
    if (text.length <= 600 && text.split('\n').length <= 8) return;
    body.classList.add(cls, 'overflow-hidden');
    const more = h('button', btn, 'Show all');
    more.type = 'button';
    more.setAttribute('aria-expanded', 'false');
    more.addEventListener('click', () => {
      const folded = body.classList.toggle(cls);
      body.classList.toggle('overflow-hidden', folded);
      more.textContent = folded ? 'Show all' : 'Show less';
      more.setAttribute('aria-expanded', String(!folded));
    });
    box.append(more);
  }

  // ---- code

  // A clipped text ends with this note (transcript.Clip).
  const CLIP = /\n… \((\d+) more bytes\)$/;
  // A line of a Read output: its number, then a tab.
  const READ_LINE = /^ *(\d+)(?:\t|→)/;

  const fmtBytes = (n) => (n < 1024 ? `${n} bytes` : `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`);

  // codeBar is the bar over code: a label (input, output, error) or a tag
  // (the language a fence names), and a button to copy raw.
  function codeBar(o, raw) {
    const copy = h('button', 'touch:min-h-11 ml-auto shrink-0 rounded px-1.5 py-0.5 text-[10.5px] font-medium text-zinc-500 hover:bg-ink-800 hover:text-zinc-200 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Copy');
    copy.type = 'button';
    copy.addEventListener('click', () => copyRaw(raw, copy));
    return h('div', 'flex min-w-0 items-center gap-2 border-b border-ink-700/70 bg-ink-900/70 py-0.5 pl-2.5 pr-1',
      o.label ? h('span', o.error ? 'text-[10px] font-medium uppercase tracking-wider text-rose-300/80' : 'text-[10px] font-medium uppercase tracking-wider text-zinc-500', o.label) : null,
      o.tag ? h('span', 'font-mono text-[10.5px] text-zinc-500', o.tag) : null,
      copy);
  }

  // codeView frames o.text as code, highlighted as o.lang ('diff': a
  // diff, its code highlighted as o.inner). o.read shows the line numbers
  // of a Read output in a gutter; o.wrap wraps long lines instead of
  // scrolling them; o.max caps the height; o.error tints it.
  function codeView(o) {
    let text = o.text || '';
    const clip = CLIP.exec(text);
    if (clip) text = text.slice(0, clip.index);
    let raw = text;
    let body;
    const rows = o.lang === 'diff' || o.read;
    if (o.lang === 'diff') body = diffRows(text, o.inner || '');
    else if (o.read) [body, raw] = readRows(text, o.lang);
    else body = hlInto(h('code', ''), text, o.lang);
    const cls = ['font-mono leading-snug text-zinc-300', o.max ? 'max-h-80 overflow-auto text-[11.5px]' : 'overflow-x-auto text-[12px]'];
    if (rows) cls.push('py-1.5');
    else cls.push(o.wrap ? 'whitespace-pre-wrap break-words px-2.5 py-1.5' : 'px-3 py-2');
    return h('div', o.error ? 'min-w-0 overflow-hidden rounded-md border border-rose-500/30 bg-ink-950' : 'min-w-0 overflow-hidden rounded-md border border-ink-700 bg-ink-950',
      codeBar(o, raw),
      h('pre', cls.join(' '), body),
      clip ? h('div', 'border-t border-ink-700/70 px-2.5 py-1 font-mono text-[10.5px] text-zinc-600', `… ${fmtBytes(Number(clip[1]))} more not shown`) : null);
  }

  // mdView frames markdown text, rendered.
  function mdView(text, label) {
    return h('div', 'min-w-0 overflow-hidden rounded-md border border-ink-700 bg-ink-900/40',
      codeBar({ label }, text),
      h('div', 'max-h-96 overflow-y-auto px-3 py-2', md(text)));
  }

  const GUTTER = ['w-7', 'w-7', 'w-7', 'w-9', 'w-11', 'w-13'];

  // readRows shows a Read output ("N\tline" a line) with the numbers in a
  // gutter. It returns the rows and the file's text without them.
  function readRows(text, lang) {
    const nums = [];
    const code = text.split('\n').map((l) => {
      const m = READ_LINE.exec(l);
      nums.push(m ? m[1] : '');
      return m ? l.slice(m[0].length) : l;
    });
    const raw = code.join('\n');
    const w = GUTTER[Math.min(5, nums.reduce((n, s) => Math.max(n, s.length), 0))];
    const box = h('div', '');
    hlLines(raw, lang).forEach((nodes, i) => box.append(h('div', 'flex min-w-0 px-1.5',
      h('span', 'shrink-0 select-none pr-3 text-right tabular-nums text-zinc-600 ' + w, nums[i]),
      h('span', 'min-w-0 flex-1 whitespace-pre-wrap break-words', ...(nodes.length ? nodes : [' '])))));
    return [box, raw];
  }

  const DIFF_ROW = {
    '+': ['flex min-w-0 bg-emerald-400/10 px-1.5', 'w-4 shrink-0 select-none text-emerald-400'],
    '-': ['flex min-w-0 bg-rose-500/10 px-1.5', 'w-4 shrink-0 select-none text-rose-400'],
    ' ': ['flex min-w-0 px-1.5', 'w-4 shrink-0 select-none'],
  };
  // A diff's file header: git's, or Codex's "M path" (A, M, D).
  const DIFF_FILE = /^(?:diff --git |index [\da-f]+\.\.|--- (?:a\/|\/dev\/null)|\+\+\+ (?:b\/|\/dev\/null)|[^+\- @\\])/;

  // diffRows shows a diff: added and removed lines marked and tinted,
  // their code highlighted as lang, or as the file a header names.
  function diffRows(text, lang) {
    const box = h('div', '');
    let cur = lang;
    let code = []; // [mark, line] of the current hunk
    const flush = () => {
      if (!code.length) return;
      const lines = hlLines(code.map((c) => c[1]).join('\n'), cur);
      code.forEach(([mark], i) => {
        const [row, sign] = DIFF_ROW[mark];
        box.append(h('div', row, h('span', sign, mark.trim()),
          h('span', 'min-w-0 flex-1 whitespace-pre-wrap break-words', ...(lines[i].length ? lines[i] : [' ']))));
      });
      code = [];
    };
    for (const l of text.split('\n')) {
      if (l && DIFF_FILE.test(l)) {
        flush();
        const p = /^diff --git a\/.* b\/(.*)$/.exec(l) || /^\+\+\+ b\/(.*)$/.exec(l) || /^[AMD] (?:.* -> )?(\S.*)$/.exec(l);
        if (p) cur = extLang(p[1]) || lang;
        // A file starts at git's "diff --git" or Codex's "M path"; git's
        // index and ---/+++ lines follow.
        box.append(h('div', /^(?:diff --git |[AMD] )/.test(l) ? 'px-1.5 pt-1.5 font-semibold text-zinc-100 first:pt-0' : 'px-1.5 text-zinc-500', l));
      } else if (l.startsWith('@@') || l.startsWith('\\')) {
        flush();
        box.append(h('div', 'px-1.5 text-cyan-300/80', l));
      } else {
        const mark = l[0] === '+' || l[0] === '-' ? l[0] : ' ';
        code.push([mark, l[0] === mark ? l.slice(1) : l]);
      }
    }
    flush();
    return box;
  }

  // copyRaw copies text. Plain HTTP has no Clipboard API: then a hidden
  // textarea and the old copy command (in the dialog, which makes the rest
  // of the page inert).
  async function copyRaw(text, btn) {
    let ok = false;
    try {
      await navigator.clipboard.writeText(text);
      ok = true;
    } catch (e) {
      const ta = h('textarea', 'fixed left-0 top-0 size-px opacity-0');
      ta.value = text;
      ta.readOnly = true;
      (btn.closest('dialog') || document.body).append(ta);
      ta.select();
      try {
        ok = document.execCommand('copy');
      } catch (e2) {
        ok = false;
      }
      ta.remove();
      btn.focus();
    }
    btn.textContent = ok ? 'Copied' : 'Copy failed';
    setTimeout(() => {
      btn.textContent = 'Copy';
    }, 1500);
  }

  // ---- highlighting
  //
  // A tokenizer per language family (comments, strings, numbers, keywords,
  // a few more), not a parser: enough to read code by.

  const TOK = {
    c: 'italic text-zinc-500', // comment
    s: 'text-emerald-300', // string
    n: 'text-amber-300', // number, constant
    k: 'text-violet-300', // keyword
    t: 'text-yellow-200', // type
    f: 'text-sky-300', // function, command
    p: 'text-rose-300', // key, property, tag, variable
    m: 'text-cyan-300', // meta: decorator, flag, section, heading
  };

  const words = (s) => new Set(s.split(' '));

  // Parts of the patterns. Every group is (?:…): hl numbers the parts.
  const P = {
    slash: String.raw`\/\/[^\n]*|\/\*[\s\S]*?(?:\*\/|(?![\s\S]))`,
    hash: String.raw`(?:^|[ \t])#[^\n]*`,
    dq: String.raw`"(?:[^"\\\n]|\\.)*"?`,
    sq: String.raw`'(?:[^'\\\n]|\\.)*'?`,
    bq: String.raw`\x60(?:[^\x60\\]|\\[\s\S])*\x60?`,
    num: String.raw`\b(?:0[xX][\da-fA-F_]+|0[bBoO][\d_]+|\d[\d_]*(?:\.\d[\d_]*)?(?:[eE][+-]?\d+)?)[a-zA-Z]*\b`,
    word: String.raw`[A-Za-z_$][\w$]*`,
  };

  const KW = {
    js: words('as async await break case catch class const continue debugger default delete do else enum export extends finally for from function if implements import in instanceof interface let new of private protected public readonly return satisfies static super switch this throw try type typeof var void while with yield declare namespace abstract keyof infer'),
    go: words('break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var'),
    py: words('and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case'),
    rust: words('as async await break const continue crate dyn else enum extern fn for if impl in let loop match mod move mut pub ref return static struct super trait type unsafe use where while'),
    c: words('abstract break case catch class const continue default delete do else enum export extends extern final finally for fun func goto if implements import in inline interface internal let namespace new operator override package private protected public return sealed static struct super switch template throw throws try typedef typename union using val var virtual volatile when where while yield guard defer'),
    sh: words('if then else elif fi for while until do done case esac function in select return export local readonly declare unset shift source exit break continue trap eval exec time'),
    sql: words('select from where insert into values update set delete create table index view drop alter add column primary key foreign references join left right inner outer full cross on as and or not is in exists between like ilike group by order asc desc having limit offset distinct union all case when then else end begin commit rollback transaction returning with default unique check constraint if replace grant revoke cascade'),
  };
  const LIT = {
    js: words('true false null undefined NaN Infinity'),
    go: words('true false nil iota'),
    py: words('True False None self cls'),
    rust: words('true false self None Some Ok Err'),
    c: words('true false null nullptr nil NULL None this self'),
    json: words('true false null'),
    yaml: words('true false null yes no on off True False Null TRUE FALSE NULL'),
  };
  const TYPES = {
    js: words('string number boolean any unknown never object symbol bigint'),
    go: words('bool byte complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr any comparable'),
    py: words('int float str bool list dict set tuple bytes object type frozenset complex'),
    rust: words('i8 i16 i32 i64 i128 isize u8 u16 u32 u64 u128 usize f32 f64 bool char str Self String Vec Option Result Box'),
    c: words('void int char float double long short unsigned signed bool boolean byte string auto size_t int8_t int16_t int32_t int64_t uint8_t uint16_t uint32_t uint64_t'),
    sql: words('int integer bigint smallint serial bigserial text varchar char boolean bool date time timestamp timestamptz uuid json jsonb real numeric decimal float double'),
  };

  // tokenizer builds one from [kind, pattern] parts, tried in order at
  // each position. Kind w is a word (see wordKind), x a word after a lead
  // (o.lead; the word is o.xkind, or a command), g an HTML tag.
  function tokenizer(parts, o) {
    return Object.assign({
      re: new RegExp(parts.map((p) => '(' + p[1] + ')').join('|'), o && o.i ? 'gmi' : 'gm'),
      kinds: parts.map((p) => p[0]),
    }, o);
  }

  const PY_STR = String.raw`[rRbBfFuU]{0,2}(?:"""[\s\S]*?(?:"""|(?![\s\S]))|'''[\s\S]*?(?:'''|(?![\s\S]))|${P.dq}|${P.sq})`;

  const LANGS = {
    js: tokenizer([['c', P.slash], ['s', P.bq], ['s', P.dq], ['s', P.sq], ['m', '@[A-Za-z_][\\w.]*'], ['n', P.num], ['w', P.word]],
      { kw: KW.js, lit: LIT.js, ty: TYPES.js, caps: true }),
    go: tokenizer([['c', P.slash], ['s', P.bq], ['s', P.dq], ['s', P.sq], ['n', P.num], ['w', P.word]],
      { kw: KW.go, lit: LIT.go, ty: TYPES.go, caps: true }),
    rust: tokenizer([['c', P.slash], ['s', P.dq], ['s', String.raw`'(?:\\.|[^'\\\n])'`], ['m', String.raw`'[A-Za-z_]\w*|#!?\[[^\]\n]*\]`],
      ['f', String.raw`[A-Za-z_]\w*!(?=[(\[{])`], ['n', P.num], ['w', P.word]],
    { kw: KW.rust, lit: LIT.rust, ty: TYPES.rust, caps: true }),
    c: tokenizer([['c', P.slash], ['s', P.dq], ['s', P.sq], ['m', String.raw`^[ \t]*#[ \t]*[a-z]+|@[A-Za-z_]\w*`], ['n', P.num], ['w', P.word]],
      { kw: KW.c, lit: LIT.c, ty: TYPES.c, caps: true }),
    py: tokenizer([['c', P.hash], ['s', PY_STR], ['m', String.raw`@[A-Za-z_][\w.]*`], ['n', P.num], ['w', P.word]],
      { kw: KW.py, lit: LIT.py, ty: TYPES.py, caps: true }),
    sh: tokenizer([['c', P.hash], ['s', String.raw`"(?:[^"\\]|\\[\s\S])*"?`], ['s', String.raw`'[^']*'?`],
      ['p', String.raw`\$\{[^}\n]*\}|\$[A-Za-z_]\w*|\$[\d@#?$!*-]`],
      ['x', String.raw`(?:^|&&|\|\||\$\(|[|;&({\x60])[ \t]*[A-Za-z_./~][\w./+~-]*(?![\w./+~=-])`],
      ['m', String.raw`(?:^|[ \t])--?[A-Za-z][\w-]*`], ['n', P.num], ['w', String.raw`[A-Za-z_][\w-]*`]],
    { kw: KW.sh, lead: /^(?:&&|\|\||\$\(|[|;&({`])?[ \t]*/ }),
    json: tokenizer([['p', String.raw`"(?:[^"\\\n]|\\.)*"(?=\s*:)`], ['s', P.dq], ['n', String.raw`-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`], ['w', P.word]],
      { lit: LIT.json }),
    yaml: tokenizer([['c', P.hash], ['m', String.raw`^(?:---|\.\.\.)[ \t]*$|[&*][\w-]+`],
      ['x', String.raw`^[ \t]*(?:-[ \t]+)*[^\s#'"{}\[\],:&*!|>%@\x60-][^\n:#]*(?=:(?:[ \t]|$))`],
      ['s', P.dq], ['s', P.sq], ['n', P.num], ['w', P.word]],
    { lit: LIT.yaml, lead: /^[ \t]*(?:-[ \t]+)*/, xkind: 'p' }),
    toml: tokenizer([['c', String.raw`(?:^|[ \t])[#;][^\n]*`], ['m', String.raw`^[ \t]*\[[^\n]*\]`], ['p', String.raw`[\w.-]+(?=[ \t]*=)`],
      ['s', String.raw`"""[\s\S]*?(?:"""|(?![\s\S]))|'''[\s\S]*?(?:'''|(?![\s\S]))`], ['s', P.dq], ['s', P.sq], ['n', P.num], ['w', P.word]],
    { lit: LIT.json }),
    css: tokenizer([['c', String.raw`\/\*[\s\S]*?(?:\*\/|(?![\s\S]))`], ['s', P.dq], ['s', P.sq], ['k', String.raw`@[\w-]+|!important`],
      ['n', String.raw`#[\da-fA-F]{3,8}(?![\w-])`], ['p', String.raw`-{0,2}[A-Za-z][\w-]*(?=[ \t]*:(?:[ \t]|[^\n{]*;))`],
      ['n', String.raw`-?(?:\d+\.?\d*|\.\d+)(?:%|[A-Za-z]+)?`], ['w', String.raw`[A-Za-z_-][\w-]*`]], {}),
    html: tokenizer([['c', String.raw`<!--[\s\S]*?(?:-->|(?![\s\S]))`], ['m', String.raw`<![A-Za-z][^>]*>?|<\?[\s\S]*?(?:\?>|(?![\s\S]))`],
      ['g', String.raw`<\/?[A-Za-z][\w:.-]*(?:[^<>"']|"[^"]*"|'[^']*')*>?`], ['n', String.raw`&#?\w+;`]], {}),
    sql: tokenizer([['c', String.raw`--[^\n]*|\/\*[\s\S]*?(?:\*\/|(?![\s\S]))`], ['s', P.sq], ['p', P.dq], ['n', P.num], ['w', P.word]],
      { i: true, kw: KW.sql, lit: LIT.json, ty: TYPES.sql }),
    md: tokenizer([['m', String.raw`^#{1,6}[ \t][^\n]*`], ['c', String.raw`^[ \t]*(?:\x60{3,}|~{3,})[^\n]*|^[ \t]*>[^\n]*`],
      ['s', String.raw`\x60[^\x60\n]+\x60`], ['k', String.raw`\*\*[^*\n]+\*\*|__[^_\n]+__`], ['f', String.raw`!?\[[^\]\n]*\]\([^)\n]*\)`],
      ['p', String.raw`^[ \t]*(?:[-*+]|\d+[.)])(?=[ \t])`]], {}),
  };

  // Names of languages (in fences, file extensions) for the tokenizers.
  const LANG_NAMES = {
    javascript: 'js', mjs: 'js', cjs: 'js', jsx: 'js', ts: 'js', tsx: 'js', mts: 'js', cts: 'js', typescript: 'js', json5: 'js',
    golang: 'go',
    python: 'py', python3: 'py', pyi: 'py', rb: 'py', ruby: 'py',
    bash: 'sh', shell: 'sh', zsh: 'sh', ksh: 'sh', fish: 'sh', console: 'sh', shellscript: 'sh', dockerfile: 'sh', makefile: 'sh', make: 'sh', mk: 'sh', env: 'sh',
    jsonl: 'json', jsonc: 'json', geojson: 'json',
    yml: 'yaml',
    ini: 'toml', cfg: 'toml', conf: 'toml', properties: 'toml',
    rs: 'rust',
    h: 'c', cc: 'c', cpp: 'c', cxx: 'c', hpp: 'c', 'c++': 'c', java: 'c', kt: 'c', kts: 'c', kotlin: 'c', cs: 'c', csharp: 'c', swift: 'c', scala: 'c',
    dart: 'c', php: 'c', proto: 'c', groovy: 'c', gradle: 'c', m: 'c', mm: 'c', objc: 'c', zig: 'c', sol: 'c',
    scss: 'css', less: 'css',
    htm: 'html', xml: 'html', svg: 'html', xhtml: 'html', vue: 'html', svelte: 'html', plist: 'html',
    psql: 'sql', pgsql: 'sql', mysql: 'sql', sqlite: 'sql', postgres: 'sql',
    markdown: 'md', mdx: 'md',
    patch: 'diff', diff: 'diff',
  };

  const langKey = (name) => {
    const k = (name || '').toLowerCase();
    return LANGS[k] ? k : LANG_NAMES[k] || '';
  };

  // extLang is the language of a file, by its name.
  function extLang(path) {
    const base = (path || '').split(/[\\/]/).pop().toLowerCase();
    if (/^(?:makefile|gnumakefile|dockerfile|containerfile|\.(?:bash|zsh)rc|\.profile)$/.test(base)) return 'sh';
    const dot = base.lastIndexOf('.');
    return dot < 0 ? '' : langKey(base.slice(dot + 1));
  }

  // wordKind colours a word: a keyword, a constant, a call, a type.
  function wordKind(L, w, text, at, end) {
    let j = end;
    while (text[j] === ' ') j++;
    const call = text[j] === '(';
    if (text[at - 1] === '.') return call ? 'f' : L.caps && /^[A-Z][a-z]/.test(w) ? 't' : '';
    const k = L.i ? w.toLowerCase() : w;
    if (L.kw && L.kw.has(k)) return 'k';
    if (L.lit && L.lit.has(k)) return 'n';
    if (call) return 'f';
    if (L.ty && L.ty.has(k)) return 't';
    if (L.caps && /^[A-Z]/.test(w)) return /[a-z]/.test(w) || w.length === 1 ? 't' : 'n';
    return '';
  }

  // tokens splits text into [text, kind] for lang; kind '' is plain.
  function tokens(text, name) {
    const L = LANGS[name];
    if (!L || text.length > 200000) return [[text, '']];
    const out = [];
    const re = L.re;
    re.lastIndex = 0;
    let last = 0;
    let m;
    while ((m = re.exec(text))) {
      const s = m[0];
      if (!s) {
        re.lastIndex++;
        continue;
      }
      let g = 1;
      while (m[g] === undefined) g++;
      const kind = L.kinds[g - 1];
      if (m.index > last) out.push([text.slice(last, m.index), '']);
      last = m.index + s.length;
      if (kind === 'x') {
        const lead = L.lead.exec(s)[0];
        const w = s.slice(lead.length);
        if (lead) out.push([lead, '']);
        out.push([w, L.xkind || (L.kw.has(w) ? 'k' : 'f')]);
      } else if (kind === 'g') {
        tagTokens(s, out);
      } else {
        out.push([s, kind === 'w' ? wordKind(L, s, text, m.index, last) : kind]);
      }
    }
    if (last < text.length) out.push([text.slice(last), '']);
    return out;
  }

  // tagTokens splits an HTML tag: its name, attributes and their values.
  function tagTokens(s, out) {
    const name = /^<\/?[\w:.-]*/.exec(s)[0];
    out.push([name.slice(0, name[1] === '/' ? 2 : 1), ''], [name.slice(name[1] === '/' ? 2 : 1), 'p']);
    const rest = s.slice(name.length);
    const re = /"[^"]*"?|'[^']*'?|[^\s=>"'/]+/g;
    let last = 0;
    let m;
    while ((m = re.exec(rest))) {
      if (m.index > last) out.push([rest.slice(last, m.index), '']);
      out.push([m[0], m[0][0] === '"' || m[0][0] === "'" ? 's' : 't']);
      last = re.lastIndex;
    }
    if (last < rest.length) out.push([rest.slice(last), '']);
  }

  // hlInto appends text to el, highlighted as lang.
  function hlInto(el, text, name) {
    for (const [s, k] of tokens(text, name)) el.append(k ? h('span', TOK[k], s) : s);
    return el;
  }

  // hlLines highlights text as lang, a list of nodes per line.
  function hlLines(text, name) {
    const lines = [[]];
    for (const [s, k] of tokens(text, name)) {
      s.split('\n').forEach((part, i) => {
        if (i) lines.push([]);
        if (part) lines[lines.length - 1].push(k ? h('span', TOK[k], part) : part);
      });
    }
    return lines;
  }

  // ---- markdown

  const MD_ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const MD_TABLE_SEP = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;
  const MD_INDENT = ['', 'pl-5', 'pl-10', 'pl-14'];

  // md renders the agent's markdown: headings, paragraphs, lists, quotes,
  // tables, rules and fenced code (highlighted), with inline code, bold,
  // italics and links. Nothing is parsed as HTML.
  function md(text) {
    const box = h('div', 'flex min-w-0 flex-col gap-2 text-[13.5px] leading-relaxed text-zinc-200');
    const lines = text.split('\n');
    let para = [];
    let list = null;
    const flush = () => {
      if (para.length) box.append(inline(h('p', 'whitespace-pre-wrap break-words'), para.join('\n')));
      para = [];
    };
    const end = () => {
      flush();
      list = null;
    };
    for (let i = 0; i < lines.length; i++) {
      const l = lines[i];
      const fence = /^(\s*)(`{3,}|~{3,})\s*([^\s`]*)/.exec(l);
      if (fence) {
        end();
        const close = new RegExp(`^\\s*${fence[2][0] === '`' ? '`' : '~'}{${fence[2].length},}\\s*$`);
        const indent = new RegExp(`^ {0,${fence[1].length}}`);
        const code = [];
        for (i++; i < lines.length && !close.test(lines[i]); i++) code.push(lines[i].replace(indent, ''));
        const src = code.join('\n');
        const k = langKey(fence[3]) || (looksJSON(src) ? 'json' : '');
        box.append(codeView({ text: src, lang: k, tag: fence[3] || 'text' }));
        continue;
      }
      if (!l.trim()) {
        end();
        continue;
      }
      const head = /^\s{0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/.exec(l);
      if (head) {
        end();
        box.append(inline(h('div', head[1].length <= 2 ? 'pt-1 text-[15px] font-semibold text-zinc-50' : 'pt-1 font-semibold text-zinc-50'), head[2]));
        continue;
      }
      if (/^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/.test(l)) {
        end();
        box.append(h('hr', 'my-1 border-ink-700'));
        continue;
      }
      if (l.includes('|') && i + 1 < lines.length && lines[i + 1].includes('|') && MD_TABLE_SEP.test(lines[i + 1])) {
        end();
        const sep = lines[i + 1];
        const rows = [];
        for (i += 2; i < lines.length && lines[i].includes('|') && lines[i].trim(); i++) rows.push(lines[i]);
        i--;
        box.append(mdTable(l, sep, rows));
        continue;
      }
      if (/^\s{0,3}>/.test(l)) {
        end();
        const quote = [];
        for (; i < lines.length && /^\s{0,3}>/.test(lines[i]); i++) quote.push(lines[i].replace(/^\s{0,3}>\s?/, ''));
        i--;
        box.append(h('blockquote', 'border-l-2 border-ink-600 pl-3 text-zinc-400', md(quote.join('\n'))));
        continue;
      }
      const item = MD_ITEM.exec(l);
      if (item) {
        flush();
        if (!list) {
          list = h('ul', 'flex min-w-0 flex-col gap-1');
          box.append(list);
        }
        list.append(mdItem(item));
        continue;
      }
      if (list && /^\s{2,}\S/.test(l)) {
        // More of the item above.
        inline(list.lastElementChild.lastElementChild, '\n' + l.trim());
        continue;
      }
      list = null;
      para.push(l);
    }
    end();
    return box;
  }

  function mdItem(m) {
    const depth = Math.min(3, Math.floor(m[1].replace(/\t/g, '    ').length / 2));
    const num = /\d/.test(m[2]);
    let marker = num ? m[2] : depth ? '◦' : '•';
    let text = m[3];
    const task = /^\[([ xX])\]\s+/.exec(text);
    if (task) {
      marker = task[1] === ' ' ? '☐' : '☑';
      text = text.slice(task[0].length);
    }
    return h('li', 'flex min-w-0 gap-2 ' + MD_INDENT[depth],
      h('span', num && !task ? 'min-w-5 shrink-0 select-none text-right tabular-nums text-zinc-500' : 'shrink-0 select-none text-zinc-500', marker),
      inline(h('span', 'min-w-0 flex-1 whitespace-pre-wrap break-words'), text));
  }

  // mdTable renders a table: its head row, the separator (which aligns
  // the columns) and the body rows.
  function mdTable(head, sep, rows) {
    const align = splitRow(sep).map((c) => {
      const s = c.trim();
      return !s.endsWith(':') ? '' : s.startsWith(':') ? 'text-center' : 'text-right';
    });
    const th = splitRow(head).map((c, j) => inline(h('th', 'border-b border-ink-600 px-2.5 py-1.5 font-semibold text-zinc-100 ' + (align[j] || 'text-left')), c.trim()));
    const trs = rows.map((r) => h('tr', 'border-t border-ink-700/70 first:border-t-0',
      ...splitRow(r).map((c, j) => inline(h('td', 'px-2.5 py-1.5 align-top text-zinc-300 ' + (align[j] || '')), c.trim()))));
    return h('div', 'min-w-0 overflow-x-auto rounded-md border border-ink-700',
      h('table', 'w-full border-collapse text-[12.5px] leading-snug',
        h('thead', 'bg-ink-850', h('tr', '', ...th)),
        h('tbody', '', ...trs)));
  }

  // splitRow splits a table row into cells: on | outside `code`, not \|.
  function splitRow(s) {
    s = s.trim();
    if (s.startsWith('|')) s = s.slice(1);
    if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
    const cells = [];
    let cell = '';
    let code = false;
    for (let i = 0; i < s.length; i++) {
      const c = s[i];
      if (c === '\\' && s[i + 1] === '|') {
        cell += '|';
        i++;
        continue;
      }
      if (c === '`') code = !code;
      if (c === '|' && !code) {
        cells.push(cell);
        cell = '';
        continue;
      }
      cell += c;
    }
    cells.push(cell);
    return cells;
  }

  const INLINE = /`[^`\n]+`|\*\*(?=\S)[^*\n]*?\S\*\*|__(?=\S)[^_\n]*?\S__|~~(?=\S)[^~\n]*?\S~~|\[([^\]\n]+)\]\(((?:https?:\/\/|mailto:)[^\s)]+)\)|\*(?=[^\s*])[^*\n]*?[^\s*]\*|_(?=[^\s_])[^_\n]*?[^\s_]_|https?:\/\/[^\s<>()`]*[^\s<>()`.,;:!?'"*_\]]/g;

  // inline renders inline markdown into el: `code`, **bold**, *italics*,
  // ~~struck~~, [links](https://…) and bare links, which open in a new
  // tab. Other link targets stay text.
  function inline(el, text) {
    let last = 0;
    let m;
    INLINE.lastIndex = 0;
    while ((m = INLINE.exec(text))) {
      const t = m[0];
      // _ and * inside a word (snake_case, a*b) are no emphasis.
      if ((t[0] === '_' || (t[0] === '*' && t[1] !== '*')) &&
        (/\w/.test(text[m.index - 1] || '') || /\w/.test(text[m.index + t.length] || ''))) {
        INLINE.lastIndex = m.index + 1;
        continue;
      }
      if (m.index > last) el.append(text.slice(last, m.index));
      last = m.index + t.length;
      el.append(inlineNode(m));
      INLINE.lastIndex = last; // inlineNode used the pattern too
    }
    if (last < text.length) el.append(text.slice(last));
    return el;
  }

  function inlineNode(m) {
    const t = m[0];
    if (t[0] === '`') return h('code', 'rounded bg-ink-800 px-1 py-px font-mono text-[12px] text-emerald-200/90', t.slice(1, -1));
    if (m[1] !== undefined) return link(m[2], m[1]);
    if (t.startsWith('http')) return link(t, t);
    if (t.startsWith('**') || t.startsWith('__')) return inline(h('strong', 'font-semibold text-zinc-50'), t.slice(2, -2));
    if (t.startsWith('~~')) return inline(h('del', 'text-zinc-500'), t.slice(2, -2));
    return inline(h('em', 'italic'), t.slice(1, -1));
  }

  function link(href, text) {
    const a = h('a', 'break-all text-sky-300 underline decoration-sky-300/30 underline-offset-2 hover:decoration-sky-300', text);
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    return a;
  }

  function renderChatHead() {
    if (!chat.open) return;
    const a = chat.agent || { id: chat.id, name: chat.id, state: 'unspecified' };
    $('chat-h').textContent = a.name || a.id;
    const badges = [stateBadge(a)];
    if (chat.host || S.remote.size) badges.push(hostChip(chat.host));
    if (a.adapter) badges.push(chip(a.adapter));
    if (a.sandbox === 'docker') badges.push(chip('docker', 'inline-flex items-center rounded border border-cyan-500/30 bg-cyan-500/10 px-1.5 py-px font-mono text-[11px] text-cyan-300'));
    $('chat-badges').replaceChildren(...badges);
    const sub = $('chat-sub');
    sub.replaceChildren();
    if (a.root) sub.append(h('span', 'text-zinc-400', a.root), h('span', 'text-zinc-600', ' : '));
    sub.append(shortPath(a.path || a.cwd || ''));
    if (a.branch) sub.append(h('span', 'text-zinc-600', '  ⎇ '), h('span', 'text-violet-300/80', a.branch));
    sub.title = a.cwd || a.path || '';

    const det = $('chat-detail');
    if (askShown()) {
      det.hidden = true; // the form says it
    } else if (a.state === 'needs_input') {
      det.hidden = false;
      det.replaceChildren(h('span', 'font-semibold', 'Needs input'), a.stateDetail ? ': ' + a.stateDetail : '',
        h('span', 'text-amber-200/60', ' · answer on the screen below with the keys'));
    } else if (isFinished(a) && a.stateDetail) {
      det.hidden = false;
      det.replaceChildren(h('span', 'text-zinc-400', 'Session ended: ' + a.stateDetail));
    } else {
      det.hidden = true;
    }
    renderChatStop();
  }

  // renderChatStop draws Archive (a live session) or Delete (a finished
  // one), and its confirmation in a bar under the header rather than in
  // the button's place, where a double tap would land on it.
  function renderChatStop() {
    const box = $('chat-stop-box');
    const bar = $('chat-confirm');
    if (!chat.agent) {
      box.replaceChildren();
      bar.hidden = true;
      return;
    }
    const act = endAction(chat.agent);
    const b = h('button', 'touch:min-h-11 rounded-md border border-ink-600 bg-ink-850 px-2 py-1 text-xs font-medium text-zinc-300 hover:border-rose-500/40 hover:bg-rose-500/10 hover:text-rose-200 aria-expanded:border-rose-500/40 aria-expanded:bg-rose-500/10 aria-expanded:text-rose-200 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', act.label);
    b.type = 'button';
    b.setAttribute('aria-expanded', String(chat.confirmStop));
    b.setAttribute('aria-controls', 'chat-confirm');
    b.addEventListener('click', () => {
      chat.confirmStop = !chat.confirmStop;
      renderChatStop();
      if (chat.confirmStop) bar.querySelector('[data-focus="chat-stop-yes"]').focus();
    });
    box.replaceChildren(b);
    bar.hidden = !chat.confirmStop;
    if (!chat.confirmStop) return;
    const keep = h('button', 'touch:min-h-11 rounded-md border border-ink-600 bg-ink-850 px-3 py-1 text-xs font-medium text-zinc-300 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Keep');
    keep.type = 'button';
    keep.addEventListener('click', () => {
      chat.confirmStop = false;
      renderChatStop();
    });
    const yes = h('button', 'touch:min-h-11 rounded-md border border-rose-500/40 bg-rose-500/15 px-3 py-1 text-xs font-medium text-rose-200 hover:bg-rose-500/25 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-rose-400', chat.stopping ? act.busy : act.label + ' session');
    yes.type = 'button';
    yes.disabled = chat.stopping;
    yes.dataset.focus = 'chat-stop-yes';
    yes.addEventListener('click', stopChat);
    bar.replaceChildren(
      h('p', 'min-w-48 flex-1 text-xs text-rose-100/90', h('span', 'font-semibold', act.label + ' this session?'), ' ' + act.note),
      h('div', 'ml-auto flex items-center gap-2', keep, yes));
  }

  async function stopChat() {
    const seq = chat.seq;
    const { host, id } = chat;
    chat.stopping = true;
    renderChatStop();
    const done = await endAgent(host, id, chat.agent);
    if (seq !== chat.seq) return;
    chat.stopping = false;
    chat.confirmStop = false;
    if (done) return closeChat();
    renderChatHead();
    renderChatInput();
  }

  function renderChatStatus() {
    const st = $('chat-status');
    if (chat.feed.error) {
      st.className = 'min-h-4 min-w-0 flex-1 truncate text-[11px] text-rose-300';
      st.textContent = chat.feed.error;
      return;
    }
    if (chat.held && chatLive()) {
      st.className = 'min-h-4 min-w-0 flex-1 truncate text-[11px] text-amber-300';
      st.textContent = 'The agent shows a dialog: your text was typed, but Enter was not pressed. Answer with the keys.';
      return;
    }
    st.className = 'min-h-4 min-w-0 flex-1 truncate text-[11px] text-zinc-500';
    const a = chat.agent;
    st.textContent = !a ? '' : !chatLive() ? 'the session has ended'
      : chat.switching ? 'switching the model…'
      : askShown() ? 'a message sent now answers none of the questions: the agent reads it instead'
      : chatDialog() ? 'in a dialog, Send types your text without pressing Enter: Enter picks the highlighted option'
      : a.state === 'working' ? 'working… new messages appear as the agent writes them' : '';
  }

  // ---------------------------------------------------------------- model
  //
  // The model and effort the session runs at (each chat reply has them),
  // and a switch to others, for this session only (POST .../model).

  function opt(value, label, disabled) {
    const o = h('option', '', label);
    o.value = value;
    o.disabled = !!disabled;
    return o;
  }

  // chatModel takes the model of a chat reply. Replies read while a
  // switch went on, or just before it ended, would undo it on the page.
  function chatModel(m) {
    if (chat.switching || Date.now() - chat.switchedAt < 2000) return;
    chat.model = m || null;
    renderChatModel();
  }

  // renderChatModel shows the model and effort in two pickers; the efforts
  // are those of the session's model. A value the page cannot pick (none
  // known yet, a model not on the list) shows as a disabled option.
  function renderChatModel() {
    const m = chat.model;
    $('chat-model').hidden = !m;
    if (!m) return;
    const msel = $('chat-model-sel');
    const esel = $('chat-effort-sel');
    const cur = m.models.find((x) => x.id === m.model);
    const mopts = m.models.map((x) => opt(x.id, x.label));
    if (!cur) mopts.unshift(opt('', m.name || 'default model', true));
    msel.replaceChildren(...mopts);
    msel.value = cur ? cur.id : '';
    const efforts = cur ? m.efforts.filter((e) => cur.efforts.includes(e.id)) : m.efforts;
    const ecur = efforts.find((e) => e.id === m.effort);
    const eopts = efforts.map((e) => opt(e.id, e.label));
    if (!ecur) eopts.unshift(opt('', cur && !efforts.length ? 'no effort' : m.effort || 'default effort', true));
    esel.replaceChildren(...eopts);
    esel.value = ecur ? ecur.id : '';
    const live = chatLive();
    const can = m.switch && live && !chat.switching;
    msel.disabled = !can;
    esel.disabled = !can || !efforts.length;
    const why = !live ? 'the session has ended'
      : !m.switch ? 'this agent cannot switch a running session: choose when starting one'
      : 'a switch holds for this session only';
    msel.title = `Model${m.name ? ': ' + m.name : ''} · ${why}`;
    esel.title = `Effort${m.effort ? ': ' + m.effort : ''} · ${why}`;
  }

  // switchChatModel switches to what the pickers show. A new model keeps
  // the effort shown if it runs at it.
  async function switchChatModel() {
    const m = chat.model;
    if (!m || chat.switching || !chatLive()) return;
    const model = $('chat-model-sel').value;
    const effort = $('chat-effort-sel').value;
    const body = { model: model && model !== m.model ? model : '', effort: '' };
    const to = m.models.find((x) => x.id === (body.model || m.model));
    if (body.model) body.effort = to && to.efforts.includes(effort) ? effort : '';
    else if (effort && effort !== m.effort) body.effort = effort;
    if (!body.model && !body.effort) return;
    const label = [to ? to.label : m.name, (m.efforts.find((e) => e.id === (body.effort || m.effort)) || {}).label]
      .filter(Boolean).join(' · ');
    const seq = chat.seq;
    chat.switching = true;
    renderChatModel();
    renderChatStatus();
    try {
      const r = await api('POST', chatAPI('model'), body);
      if (seq !== chat.seq) return;
      chat.model = (r && r.model) || m;
      toast(`${label || 'switched'} for this session`);
    } catch (e) {
      if (seq !== chat.seq) return;
      toast(`could not switch: ${e.message}`, 'error');
    }
    chat.switching = false;
    chat.switchedAt = Date.now();
    renderChatModel();
    renderChatStatus();
    if (chat.screenOpen) setTimeout(refreshScreen, 150);
  }

  // -------------------------------------------------------------- questions
  //
  // Questions the agent asks in a dialog of choices (Claude's
  // AskUserQuestion), as a form above the message box: each chat reply
  // lists those it waits on (asks), POST .../answer answers one. The dialog
  // stays open in the terminal too; whichever answers first wins. A message
  // sent while the form shows replies in place of answers ("Chat about
  // this" in the terminal).

  // askShown is the question the form shows: the agent shows its dialogs
  // one at a time, in order.
  const askShown = () => (chatLive() && chat.view === 'chat' && chat.asks[0]) || null;

  // chatAsks takes the questions of a chat reply.
  function chatAsks(asks) {
    asks = (asks || []).filter((a) => !chat.answered.has(a.id));
    if (asks.map((a) => a.id).join(',') === chat.asks.map((a) => a.id).join(',')) return;
    const had = chat.asks.length > 0;
    chat.asks = asks;
    // The form answers the dialog: no need for the screen it opened for it.
    if (asks.length && !had && chat.screenAutoOpened && chat.screenOpen) {
      chat.screenOpen = false;
      chat.screenAutoOpened = false;
      renderChatScreen();
    }
    renderChatHead();
    renderChatAsk();
    renderChatInput();
  }

  function renderChatAsk() {
    const box = $('chat-ask');
    const a = askShown();
    box.hidden = !a;
    if (!a) {
      if (chat.askForm) box.replaceChildren();
      chat.askForm = null;
      return;
    }
    // Built once per question: a reply must not undo the picks.
    if (!chat.askForm || chat.askForm.id !== a.id) {
      chat.askForm = askForm(a);
      box.replaceChildren(chat.askForm.el);
    }
    paintAsk();
  }

  const ASK_OPTION = 'touch:min-h-11 flex min-w-0 cursor-pointer items-start gap-2.5 rounded-md border border-ink-600 bg-ink-900 px-2.5 py-2 hover:border-zinc-500 has-checked:border-amber-400/50 has-checked:bg-amber-400/10 focus-within:outline-2 focus-within:outline-offset-1 focus-within:outline-amber-400';
  const ASK_INPUT = 'mt-0.5 size-4 shrink-0 accent-amber-400 focus:outline-none';

  // askForm builds the form of a question: per question its options, one
  // to type, a preview of the option looked at, and a note.
  function askForm(a) {
    const form = { id: a.id, sending: false, qs: [] };
    const fields = h('div', 'flex flex-col gap-4');
    a.questions.forEach((q, i) => {
      const name = `ask-${a.id}-${i}`;
      const type = q.multiSelect ? 'checkbox' : 'radio';
      const fq = { q, opts: [], other: null, note: null };
      const opts = h('div', 'grid gap-1.5 sm:grid-cols-2');
      // The preview of the option looked at, else of the one picked. Its
      // box keeps its size: the form grows upwards, and options moving
      // under the pointer would take the click meant for another.
      const hasPreview = (q.options || []).some((o) => o.preview);
      const preview = h('pre', 'mt-1.5 h-44 overflow-auto rounded-md border border-ink-700 bg-ink-950 px-3 py-2 font-mono text-[11.5px] leading-snug text-zinc-300');
      preview.hidden = !hasPreview;
      const showPreview = (o) => {
        if (!hasPreview) return;
        const p = (o && o.preview) || (fq.opts.find((x) => x.input.checked && x.preview) || {}).preview;
        preview.textContent = p || 'Point at an option to see its preview.';
        preview.classList.toggle('text-zinc-600', !p);
      };
      for (const o of q.options || []) {
        const input = h('input', ASK_INPUT);
        input.type = type;
        input.name = name;
        input.value = o.label;
        const label = h('label', ASK_OPTION, input,
          h('span', 'min-w-0',
            h('span', 'block break-words text-[13px] font-medium text-zinc-100', o.label),
            o.description ? h('span', 'mt-0.5 block break-words text-xs leading-snug text-zinc-400', o.description) : null));
        if (o.preview) {
          label.addEventListener('mouseenter', () => showPreview(o));
          label.addEventListener('mouseleave', () => showPreview(null));
          label.addEventListener('focusin', () => showPreview(o));
        }
        input.addEventListener('change', () => {
          showPreview(o.preview ? o : null);
          paintAsk();
        });
        fq.opts.push({ label: o.label, input, preview: o.preview || '' });
        opts.append(label);
      }
      // One of their own, as in the terminal's "Type something".
      const oin = h('input', ASK_INPUT);
      oin.type = type;
      oin.name = name;
      oin.setAttribute('aria-label', 'Your own answer');
      const otext = h('input', 'touch:min-h-9 min-w-0 flex-1 rounded border border-ink-600 bg-ink-950 px-2 py-1 text-[13px] max-sm:text-base pointer-coarse:text-base text-zinc-100 placeholder:text-zinc-600 focus:border-amber-400/60 focus:outline-none');
      otext.type = 'text';
      otext.placeholder = 'Something else…';
      otext.maxLength = 8000;
      otext.setAttribute('aria-label', `Your own answer to: ${q.question}`);
      otext.addEventListener('input', () => {
        oin.checked = otext.value.trim() !== '' || (!q.multiSelect && oin.checked);
        paintAsk();
      });
      otext.addEventListener('focus', () => {
        if (!q.multiSelect && otext.value.trim()) oin.checked = true;
        paintAsk();
      });
      otext.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter' && !ev.isComposing) {
          ev.preventDefault();
          submitAsk();
        }
      });
      oin.addEventListener('change', () => {
        if (oin.checked && !otext.value.trim()) otext.focus();
        paintAsk();
      });
      fq.other = { input: oin, text: otext };
      opts.append(h('label', ASK_OPTION + ' items-center sm:col-span-2', oin, otext));
      // A note on the answer.
      const note = h('textarea', 'touch:min-h-11 mt-1.5 min-h-9 w-full resize-y rounded-md border border-ink-600 bg-ink-950 px-2.5 py-1.5 text-[13px] max-sm:text-base pointer-coarse:text-base text-zinc-100 placeholder:text-zinc-600 focus:border-amber-400/60 focus:outline-none');
      note.rows = 2;
      note.maxLength = 8000;
      note.placeholder = 'A note for the agent on this answer';
      note.hidden = true;
      note.setAttribute('aria-label', `Note on: ${q.question}`);
      const addNote = h('button', 'touch:min-h-11 mt-1 rounded px-1.5 py-0.5 text-[11px] font-medium text-zinc-500 hover:bg-ink-800 hover:text-zinc-200 focus-visible:outline-2 focus-visible:outline-amber-400', '+ note');
      addNote.type = 'button';
      addNote.addEventListener('click', () => {
        note.hidden = false;
        addNote.hidden = true;
        note.focus();
      });
      fq.note = note;
      const legend = h('legend', 'mb-2 flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1',
        q.header ? h('span', 'rounded bg-amber-400/15 px-1.5 py-px text-[10px] font-semibold uppercase tracking-wider text-amber-200', q.header) : null,
        h('span', 'break-words text-[14px] font-medium leading-snug text-zinc-100', q.question),
        q.multiSelect ? h('span', 'text-[11px] text-zinc-500', 'pick any') : null);
      fields.append(h('fieldset', 'min-w-0', legend, opts, preview, addNote, note));
      showPreview(null);
      form.qs.push(fq);
    });
    form.progress = h('span', 'text-[11px] text-zinc-500');
    form.send = h('button', 'touch:h-11 touch:px-4 h-8 shrink-0 rounded-md bg-amber-400 px-3 text-xs font-semibold text-ink-950 hover:bg-amber-300 disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-amber-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-amber-400', 'Answer');
    form.send.type = 'button';
    form.send.addEventListener('click', submitAsk);
    form.more = h('span', 'text-[11px] text-zinc-500');
    const who = chat.agent && chat.agent.adapter === 'claude' ? 'Claude asks' : 'The agent asks';
    form.el = h('div', 'mx-auto flex max-w-3xl flex-col gap-3',
      h('div', 'flex items-center gap-2',
        h('span', STATES.needs_input.dot),
        h('span', 'text-[11px] font-semibold uppercase tracking-wider text-amber-300', who),
        form.more),
      fields,
      // Stays in sight while the questions scroll (phones).
      h('div', 'sticky -bottom-3 z-10 -mx-3 -mb-3 flex flex-wrap items-center justify-end gap-x-3 gap-y-1.5 border-t border-amber-400/15 bg-ink-900 px-3 py-2',
        h('span', 'mr-auto text-[11px] text-zinc-500', 'Answer here or in the terminal · or reply below instead'),
        form.progress, form.send));
    return form;
  }

  // askAnswer is what the form says: {answers, notes}, and how many
  // questions are still unanswered.
  function askAnswer(form) {
    const answers = {};
    const notes = {};
    let open = 0;
    for (const fq of form.qs) {
      const picks = fq.opts.filter((o) => o.input.checked).map((o) => o.label);
      const own = fq.other.text.value.trim();
      if (fq.other.input.checked && own) picks.push(own);
      if (!picks.length) open++;
      else answers[fq.q.question] = picks;
      const note = fq.note.value.trim();
      if (note) notes[fq.q.question] = note;
    }
    return { answers, notes, open };
  }

  function paintAsk() {
    const form = chat.askForm;
    if (!form) return;
    const { open } = askAnswer(form);
    const n = form.qs.length;
    form.progress.textContent = n > 1 ? `${n - open} of ${n} answered` : '';
    form.send.disabled = open > 0 || form.sending || !chatLive();
    form.send.textContent = form.sending ? 'Sending…' : 'Answer';
    form.more.textContent = chat.asks.length > 1 ? `· ${chat.asks.length - 1} more after this` : '';
  }

  async function submitAsk() {
    const form = chat.askForm;
    if (!form) return;
    const { answers, notes, open } = askAnswer(form);
    if (open) return;
    await sendAnswer(form, Object.keys(notes).length ? { answers, notes } : { answers });
  }

  // sendAnswer answers the form's question; it reports whether that worked.
  async function sendAnswer(form, body) {
    if (form.sending || !chatLive()) return false;
    const seq = chat.seq;
    form.sending = true;
    paintAsk();
    let ok = false;
    try {
      await api('POST', chatAPI('answer'), Object.assign({ ask: form.id }, body));
      ok = true;
    } catch (e) {
      toast(e.status === 404 ? 'that question was answered already, or is gone' : `could not answer: ${e.message}`, 'error');
      if (e.status !== 404) {
        form.sending = false;
        if (seq === chat.seq) paintAsk();
        return false;
      }
    }
    if (seq !== chat.seq) return ok;
    chat.answered.add(form.id);
    chat.asks = chat.asks.filter((x) => x.id !== form.id);
    renderChatAsk();
    renderChatInput();
    return ok;
  }

  // Keys for the agent's own dialogs (permissions, trust, menus).
  const CHAT_KEYS = [
    ['escape', 'Esc'], ['up', '↑'], ['down', '↓'], ['left', '←'], ['right', '→'], ['tab', 'Tab'],
    ['enter', 'Enter'], ['1', '1'], ['2', '2'], ['3', '3'], ['4', '4'], ['5', '5'], ['6', '6'], ['ctrl-c', 'Ctrl-C'],
  ];

  function renderChatInput() {
    const live = chatLive();
    const ta = $('chat-input');
    ta.disabled = !live;
    ta.placeholder = !live ? 'the session has ended'
      : askShown() ? 'Or reply in your own words instead of answering'
      : chatDialog() ? 'Type into the dialog (Enter is not pressed)'
      : touch() ? 'Message the agent' : 'Message the agent · Enter sends, Shift+Enter for a new line';
    $('chat-send').disabled = !live || chat.sending;
    $('chat-attach').disabled = !live || chat.sending || askShown();
    renderChatModel();
    const keys = $('chat-keys');
    keys.hidden = !live;
    if (live && !keys.childElementCount) {
      keys.append(h('span', 'mr-1 shrink-0 text-[10px] font-medium uppercase tracking-wider text-zinc-600', 'keys'));
      for (const [k, label] of CHAT_KEYS) {
        const b = h('button', 'touch:min-h-11 touch:min-w-11 touch:rounded-md touch:px-2.5 touch:text-[13px] shrink-0 rounded border border-ink-600 bg-ink-900 px-1.5 py-0.5 font-mono text-[11px] text-zinc-300 hover:border-zinc-500 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', label);
        b.type = 'button';
        b.dataset.key = k;
        b.title = `Press ${label} in the agent's terminal`;
        keys.append(b);
      }
    }
    renderChatStatus();
  }

  async function sendKey(key) {
    if (!chatLive()) return;
    chat.held = false;
    renderChatStatus();
    try {
      await api('POST', chatAPI('input'), { keys: [key] });
    } catch (e) {
      toast(`could not send the key: ${e.message}`, 'error');
      return;
    }
    if (chat.screenOpen) setTimeout(refreshScreen, 150);
  }

  async function sendMessage(ev) {
    ev.preventDefault();
    const ta = $('chat-input');
    const text = ta.value;
    const images = chat.images;
    const { host, id } = chat;
    if ((!text.trim() && !images.length) || chat.sending || !chatLive()) return;
    if (askShown() && chat.askForm) {
      if (images.length) {
        toast('answer the questions first, then send the images', 'warn');
        return;
      }
      // In place of answers: the agent gets the text instead.
      if (await sendAnswer(chat.askForm, { decline: text }) && ta.value === text) {
        ta.value = '';
        autosize();
        saveDraft(host, id, '');
      }
      return;
    }
    const seq = chat.seq;
    chat.sending = true;
    chat.held = false;
    renderChatInput();
    let held = false;
    try {
      // The daemon holds Enter back if the agent shows a dialog, where
      // Enter picks the highlighted option whatever was typed.
      const body = { text, submit: true };
      if (images.length) body.images = images.map((im) => im.data);
      const r = await api('POST', chatAPI('input'), body);
      held = !!(r && r.held);
      if (seq === chat.seq || loadDraft(host, id) === text) saveDraft(host, id, '');
      if (seq === chat.seq) {
        ta.value = '';
        autosize();
        chat.images = chat.images.filter((im) => !images.includes(im));
        renderChatImages();
      }
    } catch (e) {
      toast(`could not send: ${e.message}`, 'error');
    }
    if (seq !== chat.seq) return;
    chat.sending = false;
    chat.held = held;
    renderChatInput();
    if (held) {
      if (!chat.screenOpen) {
        chat.screenOpen = true;
        renderChatScreen();
      } else setTimeout(refreshScreen, 150);
    }
    if (!touch()) ta.focus();
  }

  // Images sent with a message: the daemon pastes each into the agent's
  // prompt, where Claude and Codex show it as [Image #n]. Big photos are
  // scaled down here (the models shrink them anyway) to keep requests small;
  // formats the agents cannot read (HEIC, …) are re-encoded as JPEG.
  const MAX_IMAGES = 10;
  const IMAGE_SIDE = 2000; // longest side, px
  const IMAGE_BYTES = 3.5 * 1024 * 1024; // the daemon's limit per image
  const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];

  async function addChatImages(files) {
    if (!chatLive() || chat.sending || askShown()) return;
    const seq = chat.seq;
    if (await readImages(files, chat.images, () => seq === chat.seq)) renderChatImages();
  }

  // readImages adds the image files among files to images, ready to send,
  // up to MAX_IMAGES in all. It stops, answering false, once alive() is
  // false: the message they were for is gone.
  async function readImages(files, images, alive) {
    for (const f of [...files].filter((f) => f.type.startsWith('image/'))) {
      if (images.length >= MAX_IMAGES) {
        toast(`at most ${MAX_IMAGES} images per message`, 'warn');
        break;
      }
      try {
        const blob = await fitImage(f);
        const url = await blobURL(blob);
        if (!alive()) return false;
        if (images.length < MAX_IMAGES) images.push({ data: url.slice(url.indexOf(',') + 1), url, name: f.name || 'pasted image' });
      } catch (e) {
        if (!alive()) return false;
        toast(`${f.name || 'image'}: ${e.message}`, 'error');
      }
    }
    return alive();
  }

  // imageFiles is the image files pasted or dropped, if any.
  const imageFiles = (dt) => [...(dt ? dt.files : [])].filter((f) => f.type.startsWith('image/'));

  // fitImage returns f as is if the agents can read it and it is small
  // enough, else scaled down and re-encoded (GIFs lose their animation).
  async function fitImage(f) {
    let bmp;
    try {
      bmp = await createImageBitmap(f);
    } catch {
      throw new Error('this browser cannot read the image');
    }
    try {
      const scale = Math.min(1, IMAGE_SIDE / Math.max(bmp.width, bmp.height));
      if (IMAGE_TYPES.includes(f.type) && scale === 1 && f.size <= IMAGE_BYTES) return f;
      const c = document.createElement('canvas');
      c.width = Math.max(1, Math.round(bmp.width * scale));
      c.height = Math.max(1, Math.round(bmp.height * scale));
      c.getContext('2d').drawImage(bmp, 0, 0, c.width, c.height);
      // PNG keeps screenshots sharp; photos (and PNGs too big) go JPEG.
      for (const [type, q] of [['image/png'], ['image/jpeg', 0.9], ['image/jpeg', 0.75]]) {
        if (type === 'image/png' && f.type !== 'image/png') continue;
        const b = await new Promise((res) => c.toBlob(res, type, q));
        if (b && b.size <= IMAGE_BYTES) return b;
      }
      throw new Error('too large, even scaled down');
    } finally {
      bmp.close();
    }
  }

  function blobURL(blob) {
    return new Promise((res, rej) => {
      const r = new FileReader();
      r.onload = () => res(r.result);
      r.onerror = () => rej(new Error('cannot read the image'));
      r.readAsDataURL(blob);
    });
  }

  function renderChatImages() {
    renderThumbs($('chat-images'), chat.images);
  }

  // renderThumbs shows images to send in list ul, each with a button to
  // remove it (data-image is its index).
  function renderThumbs(ul, images) {
    ul.hidden = !images.length;
    ul.replaceChildren(...images.map((im, i) => {
      const img = h('img', 'h-16 w-16 rounded-md border border-ink-600 bg-ink-950 object-cover touch:h-20 touch:w-20');
      img.src = im.url;
      img.alt = im.name;
      img.title = im.name;
      const x = h('button', 'touch:h-8 touch:w-8 absolute -right-1.5 -top-1.5 grid h-5 w-5 place-items-center rounded-full border border-ink-600 bg-ink-900 text-[11px] leading-none text-zinc-300 hover:border-rose-400/60 hover:text-rose-300 focus-visible:outline-2 focus-visible:outline-emerald-400', '✕');
      x.type = 'button';
      x.dataset.image = String(i);
      x.setAttribute('aria-label', `Remove ${im.name}`);
      return h('li', 'relative', img, x);
    }));
  }

  // autosize grows the message box with its text, up to its max height.
  function autosize() {
    fitHeight($('chat-input'));
  }

  function renderChatScreen() {
    // Without a chat (a shell, or before the first message) the screen
    // takes the room of the empty log. It shows with the chat only.
    const shown = chat.screenOpen && chat.view === 'chat';
    const big = chat.screenOpen && !chat.feed.file;
    $('chat-screen').hidden = !shown;
    $('chat-screen').classList.toggle('max-h-[45%]', !big);
    $('chat-screen').classList.toggle('flex-1', big);
    $('chat-scroll').classList.toggle('flex-1', !big);
    $('chat-scroll').classList.toggle('flex-none', big);
    $('chat-screen-toggle').setAttribute('aria-pressed', String(shown));
    if (shown) refreshScreen();
    else clearTimeout(chat.screenTimer);
  }

  const SCREEN_POLL_MS = 1500;

  async function refreshScreen() {
    clearTimeout(chat.screenTimer);
    if (!chat.open || !chat.screenOpen || chat.view !== 'chat') return;
    const seq = chat.seq;
    const pre = $('chat-screen-pre');
    if (chatLive()) {
      try {
        const r = await api('GET', chatAPI('screen'));
        if (seq === chat.seq) {
          // Dialogs and the prompt are at the bottom: stay there, unless
          // scrolled up to read.
          const box = $('chat-screen');
          const atBottom = !pre.textContent || box.scrollHeight - box.scrollTop - box.clientHeight < 24;
          pre.textContent = (r && r.screen) || '';
          if (atBottom) box.scrollTop = box.scrollHeight;
        }
      } catch (e) {
        if (seq === chat.seq) pre.textContent = '// ' + e.message;
      }
    } else if (chat.agent) {
      pre.textContent = '// the session has ended';
    }
    if (seq === chat.seq && chat.screenOpen) chat.screenTimer = setTimeout(refreshScreen, SCREEN_POLL_MS);
  }

  // setView switches the dialog to the chat, the workflow runs ('wf';
  // unfolding and showing run, if given) or a workflow agent ('sub', see
  // openSub). Leaving 'sub' stops following that agent.
  function setView(view, run) {
    if (view === 'sub' && !wf.sub) view = 'wf';
    if (view !== 'sub' && wf.sub) {
      stopFeed(wf.feed);
      wf.sub = null;
    }
    chat.view = view;
    if (run) wf.shown.add(run);
    renderChatView();
    if (view === 'wf') {
      renderWorkflows();
      const card = run && $('wf-list').querySelector(`[data-run="${CSS.escape(run)}"]`);
      if (card) card.scrollIntoView({ block: 'start' });
    } else if (view === 'chat') {
      // Hidden, the log kept growing but lost its scroll position.
      const sc = $('chat-scroll');
      sc.scrollTop = sc.scrollHeight;
    }
  }

  // renderChatView shows the parts of the dialog the view has.
  function renderChatView() {
    const v = chat.view;
    renderChatAsk();
    $('chat-scroll').hidden = v !== 'chat';
    $('chat-foot').hidden = v !== 'chat';
    $('wf-view').hidden = v !== 'wf';
    $('sub-view').hidden = v !== 'sub';
    renderChatScreen();
    renderTabs();
  }

  function wireChat() {
    const dlg = $('chat');
    $('img-view').addEventListener('click', () => $('img-view').close());
    $('chat-close').addEventListener('click', closeChat);
    $('chat-form').addEventListener('submit', sendMessage);
    const ta = $('chat-input');
    ta.addEventListener('input', () => {
      autosize();
      if (chat.open) saveDraft(chat.host, chat.id, ta.value);
    });
    ta.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing && !touch()) {
        ev.preventDefault();
        $('chat-form').requestSubmit();
      }
    });
    ta.addEventListener('paste', (ev) => {
      const files = imageFiles(ev.clipboardData);
      if (!files.length) return;
      ev.preventDefault();
      addChatImages(files);
    });
    $('chat-attach').addEventListener('click', () => $('chat-file').click());
    $('chat-file').addEventListener('change', (ev) => {
      addChatImages(ev.target.files);
      ev.target.value = '';
    });
    $('chat-images').addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-image]');
      if (!b) return;
      chat.images.splice(Number(b.dataset.image), 1);
      renderChatImages();
      if (!touch()) ta.focus();
    });
    dlg.addEventListener('dragover', (ev) => {
      if (ev.dataTransfer && [...ev.dataTransfer.types].includes('Files') && chatLive()) ev.preventDefault();
    });
    dlg.addEventListener('drop', (ev) => {
      if (!ev.dataTransfer || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      addChatImages(ev.dataTransfer.files);
    });
    $('chat-keys').addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-key]');
      if (b) sendKey(b.dataset.key);
    });
    $('chat-model-sel').addEventListener('change', switchChatModel);
    $('chat-effort-sel').addEventListener('change', switchChatModel);
    $('chat-screen-toggle').addEventListener('click', () => {
      chat.screenAuto = false;
      chat.screenAutoOpened = false;
      if (chat.view !== 'chat') {
        chat.screenOpen = true;
        setView('chat');
        return;
      }
      chat.screenOpen = !chat.screenOpen;
      renderChatScreen();
    });
    $('chat-tab-chat').addEventListener('click', () => setView('chat'));
    $('chat-tab-wf').addEventListener('click', () => setView('wf'));
    $('chat-tabs').addEventListener('keydown', (ev) => {
      if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
      const to = chat.view === 'chat' ? 'wf' : 'chat';
      setView(to);
      $(to === 'chat' ? 'chat-tab-chat' : 'chat-tab-wf').focus();
    });
    $('sub-back').addEventListener('click', () => setView('wf'));
    // Esc steps back from a workflow agent before it closes the dialog.
    dlg.addEventListener('cancel', (ev) => {
      if (chat.view === 'sub') {
        ev.preventDefault();
        setView('wf');
      }
    });
    dlg.addEventListener('close', () => {
      chat.open = false;
      chat.seq++;
      clearTimeout(chat.screenTimer);
      stopFeed(chat.feed);
      stopFeed(wf.feed);
      wf.sub = null;
      wf.cards.clear();
    });
    chat.feed = newFeed({
      scroller: $('chat-scroll'),
      log: $('chat-log'),
      url: (q) => chatAPI('chat?' + q),
      imageURL: (q) => chatAPI('image?' + q),
      live: chatLive,
      working: () => !!chat.agent && chat.agent.state === 'working',
      empty: chatEmpty,
      query: (q) => q.set('asks', chat.asks.map((a) => a.id).join(',')),
      onReply: (r) => {
        if (r.agent) chatAgent(chat.host, r.agent);
        chatModel(r.model);
        chatAsks(r.asks);
      },
      onStatus: renderChatStatus,
      onFile: renderChatScreen,
    });
    wf.feed = newFeed({
      scroller: $('sub-scroll'),
      log: $('sub-log'),
      url: (q) => chatAPI(`workflows/${encodeURIComponent(wf.sub.run)}/agents/${encodeURIComponent(wf.sub.id)}/chat?` + q),
      imageURL: (q) => chatAPI(`workflows/${encodeURIComponent(wf.sub.run)}/agents/${encodeURIComponent(wf.sub.id)}/image?` + q),
      live: subLive,
      working: subLive,
      empty: () => 'This agent has written nothing yet.',
      onReply: (r) => {
        if (r.agent) chatAgent(chat.host, r.agent);
      },
      onStatus: renderSubHead,
    });
  }

  // -------------------------------------------------------------- workflows
  //
  // A workflow is a script a Claude Code session runs in the background: it
  // starts many subagents, in phases, and returns a result. The agent list
  // shows the runs going on (GET /api/workflows, here and on every joined
  // fleet); a session's Workflows tab shows all of its runs, long-polled,
  // and each of their agents' conversations.

  const wf = {
    runs: [], // the open session's runs, oldest first
    v: '', // their version, for the next long poll
    loaded: false,
    hint: 0, // runs the agent list knew of when the dialog opened
    shown: new Set(), // run ids unfolded
    known: new Set(), // run ids seen: a new one unfolds by itself
    cards: new Map(), // run id -> {sig, el}: cards only change with their run
    sub: null, // {run, id}: the workflow agent in the 'sub' view
    feed: null, // its conversation
  };

  const WF = {
    running: {
      label: 'running',
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-sky-400/10 px-2 py-0.5 text-[11px] font-semibold text-sky-300 ring-1 ring-inset ring-sky-400/30',
      dot: 'size-2.5 shrink-0 rounded-full border-[1.5px] border-sky-400 border-t-transparent animate-spin motion-reduce:animate-none',
      bar: 'bg-sky-400 animate-pulse motion-reduce:animate-none',
    },
    completed: {
      label: 'completed',
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-emerald-400/10 px-2 py-0.5 text-[11px] font-semibold text-emerald-300/90 ring-1 ring-inset ring-emerald-400/25',
      dot: 'size-1.5 shrink-0 rounded-full bg-emerald-400',
      bar: 'bg-emerald-400/80',
    },
    done: {
      label: 'done',
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-emerald-400/10 px-2 py-0.5 text-[11px] font-semibold text-emerald-300/90 ring-1 ring-inset ring-emerald-400/25',
      dot: 'size-1.5 shrink-0 rounded-full bg-emerald-400',
      bar: 'bg-emerald-400/80',
    },
    failed: {
      label: 'failed',
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-rose-500/10 px-2 py-0.5 text-[11px] font-semibold text-rose-300 ring-1 ring-inset ring-rose-500/40',
      dot: 'size-1.5 shrink-0 rounded-full bg-rose-500',
      bar: 'bg-rose-500',
    },
    stopped: {
      label: 'stopped',
      badge: 'inline-flex items-center gap-1.5 rounded-full bg-zinc-400/5 px-2 py-0.5 text-[11px] font-semibold text-zinc-400 ring-1 ring-inset ring-zinc-500/25',
      dot: 'size-1.5 shrink-0 rounded-full bg-zinc-500',
      bar: 'bg-zinc-600',
    },
  };
  const wfStatus = (s) => WF[s] || WF.stopped;

  function wfBadge(status) {
    const st = wfStatus(status);
    return h('span', st.badge, h('span', st.dot), st.label);
  }

  // fmtTokens shortens a token count: 812, 9.4k, 748k, 1.2M.
  function fmtTokens(n) {
    n = n || 0;
    if (n < 1000) return String(n);
    if (n < 1e6) return (n / 1000).toFixed(n < 1e4 ? 1 : 0) + 'k';
    return (n / 1e6).toFixed(1) + 'M';
  }

  // elapsed shows how long from start to end; without end it keeps
  // counting (the 1s ticker updates [data-since]).
  function elapsed(start, end, cls) {
    const el = h('span', cls || '', start ? dur((end || Date.now()) - start) : '–');
    if (start && !end) el.dataset.since = String(start);
    return el;
  }

  // Up to this many agents get a bar each; more would not fit the row.
  const BARS_MAX = 16;

  // agentBars draws one bar per agent status. Many agents become one bar
  // per stretch of agents with the same status, as wide as their share.
  function agentBars(statuses, cls) {
    const row = h('span', 'flex h-1.5 min-w-0 gap-0.5 overflow-hidden ' + (cls || ''));
    if (!statuses.length) row.append(h('span', 'flex-1 rounded-full bg-ink-700'));
    const runs = [];
    for (const s of statuses) {
      const last = runs[runs.length - 1];
      if (statuses.length > BARS_MAX && last && last.s === s) last.n++;
      else runs.push({ s, n: 1 });
    }
    for (const { s, n } of runs) {
      const bar = h('span', 'min-w-px flex-1 rounded-full ' + wfStatus(s).bar);
      bar.style.flexGrow = String(n);
      row.append(bar);
    }
    return row;
  }

  // phaseGroups puts a run's agents under its phases, in order; agents of
  // no phase go last.
  function phaseGroups(run) {
    const groups = run.phases.map((p) => ({ title: p.title, detail: p.detail || '', agents: [] }));
    for (const a of run.agents) {
      let g = groups.find((x) => x.title === (a.phase || ''));
      if (!g) {
        g = { title: a.phase || '', detail: '', agents: [] };
        groups.push(g);
      }
      g.agents.push(a);
    }
    return groups;
  }

  // ---- the agent list

  const WF_POLL_MS = 3000;
  let wfTimer = 0;
  let wfBusy = false;

  function scheduleWorkflows(delay) {
    clearTimeout(wfTimer);
    if (admin) wfTimer = setTimeout(pollWorkflows, delay);
  }

  // pollWorkflows asks this fleet and every joined one for the runs going
  // on. The list keeps what it needs to draw them, so runs only redraw it
  // when that changed.
  async function pollWorkflows() {
    if (!admin || wfBusy) return;
    if (document.hidden) {
      scheduleWorkflows(WF_POLL_MS);
      return;
    }
    wfBusy = true;
    const next = new Map();
    const keep = (host) => {
      for (const [k, v] of S.workflows) if (k.startsWith(host + '/') || (!host && !k.includes('/'))) next.set(k, v);
    };
    const hosts = ['', ...pickerHosts().filter((x) => x.id && picker.hosts.get(x.id) === 'joined').map((x) => x.id)];
    await Promise.all(hosts.map(async (host) => {
      let r;
      try {
        r = await api('GET', host ? `/api/hosts/${encodeURIComponent(host)}/workflows` : '/api/workflows');
      } catch (e) {
        if (e.status !== 404) keep(host); // 404: a fleet without workflows
        return;
      }
      for (const { agent, run } of (r && r.workflows) || []) {
        const k = agentKey(host, agent);
        if (!next.has(k)) next.set(k, []);
        next.get(k).push({
          id: run.id, name: run.name, status: run.status, phase: run.phase,
          phases: run.phases.map((p) => p.title), counts: run.counts,
          bars: run.agents.map((a) => a.status), startedMs: run.startedMs, endedMs: run.endedMs,
        });
      }
    }));
    wfBusy = false;
    if (!admin) return;
    if (!same([...S.workflows], [...next])) {
      S.workflows = next;
      invalidate('agents');
    }
    scheduleWorkflows(WF_POLL_MS);
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) scheduleWorkflows(0);
  });

  // workflowLine sums up a run in its agent's row; it opens the run in the
  // session's Workflows tab.
  function workflowLine(host, a, run) {
    const st = wfStatus(run.status);
    const n = run.bars.length;
    const done = run.counts.done + run.counts.failed + run.counts.stopped;
    const b = h('button', 'touch:min-h-11 mt-2 flex w-full min-w-0 flex-wrap items-center gap-x-3 gap-y-1.5 rounded-md border border-fuchsia-400/15 bg-fuchsia-400/5 px-2.5 py-1.5 text-left text-xs hover:border-fuchsia-400/30 hover:bg-fuchsia-400/10 focus-visible:outline-2 focus-visible:outline-fuchsia-400');
    b.type = 'button';
    b.addEventListener('click', () => openChat(host, a.id, a, { view: 'wf', run: run.id }));
    const at = run.phases.indexOf(run.phase);
    b.append(
      h('span', 'flex min-w-0 items-center gap-2',
        h('span', st.dot),
        h('span', 'font-mono text-fuchsia-400/70', '⧉'),
        h('span', 'truncate font-mono font-medium text-fuchsia-100', run.name)),
      run.phase ? h('span', 'shrink-0 text-zinc-400', run.phase,
        at >= 0 && run.phases.length > 1 ? h('span', 'text-zinc-600', ` ${at + 1}/${run.phases.length}`) : null) : '',
      h('span', 'ml-auto flex min-w-0 items-center gap-3',
        agentBars(run.bars, 'w-20 sm:w-32'),
        h('span', 'shrink-0 font-mono tabular-nums text-zinc-500',
          `${done}/${n} agents · `, elapsed(run.startedMs, run.status === 'running' ? 0 : run.endedMs))));
    b.setAttribute('aria-label', `Workflow ${run.name}, ${st.label}${run.phase ? ', phase ' + run.phase : ''}, ${done} of ${n} agents finished. Open it`);
    return b;
  }

  // ---- the Workflows tab

  // wfLoop follows the session's runs while the dialog shows it; a
  // finished session's runs no longer change.
  async function wfLoop(seq) {
    let failures = 0;
    while (chat.open && seq === chat.seq) {
      const q = new URLSearchParams();
      if (wf.loaded) {
        q.set('wait', '1');
        q.set('v', wf.v);
      }
      let r;
      try {
        r = await api('GET', chatAPI('workflows?' + q));
      } catch (e) {
        if (seq !== chat.seq) return;
        if (e.status === 404) return; // gone, or a fleet without workflows
        await sleep(Math.min(15000, 1000 * 2 ** failures++));
        continue;
      }
      if (seq !== chat.seq) return;
      failures = 0;
      const first = !wf.loaded;
      wf.loaded = true;
      wf.v = r.v || '';
      if (r.agent) chatAgent(chat.host, r.agent);
      applyWorkflows(r.runs || [], first);
      if (!first && !chatLive()) return;
    }
  }

  function applyWorkflows(runs, first) {
    const was = subAgent();
    wf.runs = runs;
    for (const r of runs) {
      // At first the runs going on and the latest unfold; later every new one.
      if (!wf.known.has(r.id) && (!first || r.status === 'running')) wf.shown.add(r.id);
      wf.known.add(r.id);
    }
    if (first && runs.length) wf.shown.add(runs[runs.length - 1].id);
    renderTabs();
    for (const f of [chat.feed, wf.feed]) for (const paint of f.watch) paint();
    if (chat.view === 'wf') renderWorkflows();
    if (chat.view === 'sub') {
      const now = subAgent();
      if (was && now && was.status !== now.status) paintPending(wf.feed);
      renderSubHead();
    }
  }

  function renderTabs() {
    const n = wf.runs.length || wf.hint;
    $('chat-tabs').hidden = !n && chat.view === 'chat';
    $('chat-tab-chat').setAttribute('aria-selected', String(chat.view === 'chat'));
    $('chat-tab-wf').setAttribute('aria-selected', String(chat.view !== 'chat'));
    $('chat-tab-chat').tabIndex = chat.view === 'chat' ? 0 : -1;
    $('chat-tab-wf').tabIndex = chat.view === 'chat' ? -1 : 0;
    const count = $('chat-tab-wf-n');
    count.replaceChildren(String(wf.runs.length));
    if (wf.runs.some((r) => r.status === 'running')) count.prepend(h('span', WF.running.dot));
    count.hidden = !wf.runs.length;
  }

  // renderWorkflows draws the run cards, newest first. A card is drawn
  // anew only when its run changed, so reading a finished run's result is
  // not disturbed by a run going on.
  function renderWorkflows() {
    const box = $('wf-list');
    if (!chat.open) return;
    if (!wf.loaded || !wf.runs.length) {
      wf.cards.clear();
      box.replaceChildren(h('div', 'py-10 text-center font-mono text-xs text-zinc-600',
        !wf.loaded ? '// loading…' : '// This session has started no subagents.'));
      return;
    }
    const focus = box.contains(document.activeElement) ? document.activeElement.dataset.key : '';
    const cards = [];
    for (const run of [...wf.runs].reverse()) {
      const sig = JSON.stringify(run) + wf.shown.has(run.id);
      let c = wf.cards.get(run.id);
      if (!c || c.sig !== sig) {
        c = { sig, el: runCard(run) };
        wf.cards.set(run.id, c);
      }
      cards.push(c.el);
    }
    for (const id of wf.cards.keys()) if (!wf.runs.some((r) => r.id === id)) wf.cards.delete(id);
    // Drop what is not shown anymore, then put new cards in place: the
    // others stay where they are (a moved element loses its scroll
    // position).
    for (const el of [...box.children]) if (!cards.includes(el)) el.remove();
    cards.forEach((el, i) => {
      if (box.children[i] !== el) box.insertBefore(el, box.children[i] || null);
    });
    if (focus && !box.contains(document.activeElement)) box.querySelector(`[data-key="${CSS.escape(focus)}"]`)?.focus();
  }

  function runCard(run) {
    const open = wf.shown.has(run.id);
    const card = h('section', 'min-w-0 rounded-lg border border-ink-700 bg-ink-850/40');
    card.dataset.run = run.id;
    const head = h('button', 'touch:min-h-11 flex w-full min-w-0 items-start gap-2.5 rounded-t-lg px-3.5 pb-2.5 pt-3 text-left hover:bg-ink-800/40 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-fuchsia-400');
    head.type = 'button';
    head.dataset.key = 'run:' + run.id;
    head.setAttribute('aria-expanded', String(open));
    head.addEventListener('click', () => {
      if (open) wf.shown.delete(run.id);
      else wf.shown.add(run.id);
      renderWorkflows();
    });
    const n = run.agents.length;
    const stats = [`${n} agent${n === 1 ? '' : 's'}`, `${run.toolUses} tool calls`, `${fmtTokens(run.tokens)} tokens`];
    if (run.startedMs) stats.push('started ' + new Date(run.startedMs).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }));
    head.append(
      h('span', open ? 'mt-0.5 inline-block rotate-90 text-zinc-500 transition-transform motion-reduce:transition-none' : 'mt-0.5 inline-block text-zinc-500 transition-transform motion-reduce:transition-none', '▸'),
      h('div', 'min-w-0 flex-1',
        h('div', 'flex flex-wrap items-center gap-x-2 gap-y-1',
          h('span', 'font-mono text-fuchsia-400/70', '⧉'),
          h('span', 'min-w-0 truncate font-mono text-[13px] font-semibold text-zinc-100', run.name),
          wfBadge(run.status)),
        run.description ? h('p', open ? 'mt-1 text-xs leading-relaxed text-zinc-400' : 'mt-1 truncate text-xs text-zinc-500', run.description) : null,
        h('div', 'mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 font-mono text-[11px] tabular-nums text-zinc-500', ...stats.map((s) => h('span', '', s)))),
      elapsed(run.startedMs, run.status === 'running' ? 0 : run.endedMs || run.updatedMs, 'shrink-0 pt-0.5 font-mono text-xs tabular-nums text-zinc-300'));
    card.append(head, phaseStrip(run));
    if (open) card.append(runBody(run));
    return card;
  }

  // phaseStrip is a run at a glance: its phases side by side, an agent a
  // bar.
  function phaseStrip(run) {
    const strip = h('div', 'flex min-w-0 gap-3 px-3.5 pb-3');
    for (const g of phaseGroups(run)) {
      const done = g.agents.filter((a) => a.status !== 'running').length;
      const now = run.status === 'running' && g.title === run.phase;
      strip.append(h('div', 'min-w-0 flex-1',
        h('div', now
          ? 'mb-1 flex items-baseline justify-between gap-2 text-[10px] font-semibold uppercase tracking-wider text-sky-200'
          : 'mb-1 flex items-baseline justify-between gap-2 text-[10px] font-medium uppercase tracking-wider text-zinc-500',
        h('span', 'truncate', g.title || 'agents'),
        g.agents.length ? h('span', 'shrink-0 font-mono normal-case tabular-nums tracking-normal text-zinc-500', `${done}/${g.agents.length}`) : null),
        agentBars(g.agents.map((a) => a.status))));
    }
    return strip;
  }

  function runBody(run) {
    const body = h('div', 'flex min-w-0 flex-col gap-4 border-t border-ink-700 px-3.5 py-3');
    if (run.status === 'failed' || (run.status === 'stopped' && run.summary)) {
      body.append(h('div', 'rounded-md border border-rose-500/30 bg-rose-500/5 px-3 py-2',
        h('div', 'mb-1 text-[10px] font-semibold uppercase tracking-wider text-rose-300/80', run.status === 'failed' ? 'Failed' : 'Stopped'),
        h('pre', 'max-h-60 overflow-auto whitespace-pre-wrap break-words font-mono text-[11.5px] leading-snug text-rose-100/90', run.summary || 'no reason given')));
    } else if (run.status === 'stopped') {
      body.append(h('div', 'font-mono text-[11px] text-zinc-500', '// stopped: the session ended before the run did'));
    }
    for (const g of phaseGroups(run)) body.append(phaseSection(run, g));
    if (run.logs && run.logs.length) {
      body.append(h('div', 'min-w-0',
        h('div', 'mb-1 text-[10px] font-semibold uppercase tracking-wider text-zinc-500', 'Log'),
        h('pre', 'max-h-48 overflow-auto whitespace-pre-wrap break-words rounded border border-ink-700 bg-ink-950 px-2.5 py-1.5 font-mono text-[11.5px] leading-snug text-zinc-400', run.logs.join('\n'))));
    }
    if (run.result) body.append(resultBox(run.result));
    return body;
  }

  function phaseSection(run, g) {
    const c = { running: 0, done: 0, failed: 0, stopped: 0 };
    let from = 0;
    let to = 0;
    for (const a of g.agents) {
      c[a.status in c ? a.status : 'stopped']++;
      if (a.startedMs && (!from || a.startedMs < from)) from = a.startedMs;
      to = Math.max(to, a.updatedMs || 0);
    }
    const status = !g.agents.length ? '' : c.running ? 'running' : c.failed ? 'failed' : c.stopped ? 'stopped' : 'done';
    const icon = status ? h('span', wfStatus(status).dot) : h('span', 'size-1.5 shrink-0 rounded-full ring-1 ring-inset ring-zinc-600');
    const counts = [];
    if (g.agents.length) counts.push(`${c.done}/${g.agents.length} done`);
    if (c.failed) counts.push(`${c.failed} failed`);
    const head = h('div', 'flex min-w-0 items-center gap-2',
      h('span', 'flex w-4 shrink-0 justify-center', icon),
      h('span', status === 'running'
        ? 'shrink-0 text-[11px] font-semibold uppercase tracking-wider text-sky-200'
        : 'shrink-0 text-[11px] font-semibold uppercase tracking-wider text-zinc-300', g.title || 'Agents'),
      g.detail ? h('span', 'min-w-0 truncate text-xs text-zinc-500', g.detail) : null,
      h('span', 'ml-auto flex shrink-0 items-center gap-2 font-mono text-[11px] tabular-nums text-zinc-500',
        counts.join(' · '), from ? elapsed(from, status === 'running' ? 0 : to, 'text-zinc-400') : null));
    if (g.detail) head.title = g.detail;
    const list = h('ul', 'mt-1 flex flex-col');
    for (const a of g.agents) list.append(h('li', '', wfAgentRow(run, a)));
    if (!g.agents.length) {
      list.append(h('li', 'py-1 pl-6 font-mono text-[11px] text-zinc-600',
        run.status === 'running' ? '// not started yet' : '// not reached'));
    }
    return h('div', 'min-w-0', head, list);
  }

  function wfAgentRow(run, a) {
    const st = wfStatus(a.status);
    const b = h('button', 'touch:min-h-11 group flex w-full min-w-0 items-start gap-2 rounded-md px-1 py-1.5 text-left hover:bg-ink-800/70 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-fuchsia-400');
    b.type = 'button';
    b.dataset.key = `agent:${run.id}/${a.id}`;
    b.addEventListener('click', () => openSub(run.id, a.id));
    const act = h('div', 'mt-0.5 truncate text-xs text-zinc-500');
    if (a.tool) act.append(h('span', 'font-mono font-medium text-sky-300/80', a.tool), ' ');
    act.append(a.activity || (a.status === 'running' ? 'starting…' : ''));
    if (a.activity) act.title = a.activity;
    b.append(
      h('span', 'flex h-5 w-4 shrink-0 items-center justify-center', h('span', st.dot)),
      h('div', 'min-w-0 flex-1',
        h('div', 'flex min-w-0 items-baseline gap-2',
          h('span', 'min-w-0 truncate font-mono text-[12.5px] text-zinc-100', a.label || a.id),
          h('span', 'ml-auto flex shrink-0 items-baseline gap-2.5 font-mono text-[11px] tabular-nums text-zinc-500',
            h('span', 'max-sm:hidden', `${a.toolUses} tool${a.toolUses === 1 ? '' : 's'}`),
            h('span', 'max-sm:hidden', fmtTokens(a.tokens) + ' tok'),
            elapsed(a.startedMs, a.status === 'running' ? 0 : a.updatedMs, 'text-zinc-400'))),
        act),
      h('span', 'self-center pl-1 text-zinc-600 group-hover:text-zinc-300', '›'));
    b.setAttribute('aria-label', `${a.label || a.id}, ${st.label}. Open its conversation`);
    return b;
  }

  // resultBox shows what a run returned.
  function resultBox(result) {
    return h('div', 'min-w-0 rounded-md border border-emerald-400/20 bg-emerald-400/[0.03]',
      h('div', 'border-b border-emerald-400/15 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-emerald-300/80', 'Result'),
      h('div', 'max-h-[32rem] min-w-0 overflow-y-auto px-3 py-2.5', resultNode(result)));
  }

  // resultNode draws a run's result: JSON (mostly objects and lists of
  // them, with markdown text) as labelled parts, text as markdown, and
  // JSON cut short as JSON.
  function resultNode(text) {
    try {
      return resultValue(JSON.parse(text), 0);
    } catch {
      return looksJSON(text) ? codeView({ text, lang: 'json', tag: 'json', wrap: true }) : md(text);
    }
  }

  // resultValue draws a JSON value: text as markdown, objects and lists as
  // labelled parts (a list item by its title, name, id or number), deeper
  // ones as JSON.
  function resultValue(x, depth) {
    if (typeof x === 'string') return md(x);
    if (x === null || typeof x !== 'object') return h('div', 'font-mono text-[12px] text-zinc-300', String(x));
    if (depth > 2) return codeView({ text: JSON.stringify(x, null, 2), lang: 'json', tag: 'json', wrap: true });
    const box = h('div', depth ? 'flex min-w-0 flex-col gap-2.5 border-l border-ink-700 pl-3' : 'flex min-w-0 flex-col gap-3');
    const label = (v, i) => {
      const t = v && typeof v === 'object' && !Array.isArray(v) && [v.title, v.name, v.id, v.page, v.key].find((s) => typeof s === 'string' && s);
      return t || String(i + 1);
    };
    const parts = Array.isArray(x) ? x.map((v, i) => [label(v, i), v]) : Object.entries(x);
    if (!parts.length) box.append(h('div', 'font-mono text-[11px] text-zinc-600', Array.isArray(x) ? '// empty list' : '// empty'));
    for (const [k, v] of parts) {
      box.append(h('div', 'min-w-0',
        h('div', 'mb-1 font-mono text-[11px] font-semibold text-emerald-300/70', k),
        resultValue(v, depth + 1)));
    }
    return box;
  }

  // ---- one workflow agent

  function subRun() {
    return wf.sub && wf.runs.find((r) => r.id === wf.sub.run);
  }

  function subAgent() {
    const run = subRun();
    return run && run.agents.find((a) => a.id === wf.sub.id);
  }

  // subLive: the agent may write more (also while not known yet).
  function subLive() {
    const a = subAgent();
    return !a || a.status === 'running';
  }

  function openSub(run, id) {
    wf.sub = { run, id };
    chat.view = 'sub';
    renderChatView();
    renderSubHead();
    startFeed(wf.feed);
    $('sub-back').focus();
  }

  function renderSubHead() {
    if (!wf.sub) return;
    const run = subRun();
    const a = subAgent() || { id: wf.sub.id, status: 'running' };
    $('sub-back-name').textContent = run ? run.name : 'workflow';
    $('sub-title').textContent = a.label || a.id;
    const badges = [wfBadge(a.status)];
    if (a.phase) badges.push(chip(a.phase));
    if (a.model) badges.push(chip(a.model.replace(/^claude-/, '')));
    $('sub-badges').replaceChildren(...badges);
    const meta = $('sub-meta');
    if (wf.feed.error) {
      meta.className = 'mt-0.5 truncate text-[11px] text-rose-300';
      meta.textContent = wf.feed.error;
      return;
    }
    meta.className = 'mt-0.5 flex flex-wrap gap-x-3 font-mono text-[11px] tabular-nums text-zinc-500';
    meta.replaceChildren(
      h('span', '', `${a.toolUses || 0} tool calls`),
      h('span', '', `${fmtTokens(a.tokens)} tokens`),
      elapsed(a.startedMs, a.status === 'running' ? 0 : a.updatedMs),
      h('span', 'text-zinc-600', 'read-only: subagents take no input here'));
  }

  // -------------------------------------------------------------- websocket

  function setConn(state) {
    const dot = $('conn-dot');
    const text = $('conn-text');
    if (state === 'live') {
      dot.className = 'size-2 rounded-full bg-emerald-400 shadow-[0_0_8px] shadow-emerald-400/70';
      text.className = 'text-emerald-300';
      text.textContent = 'live';
    } else if (state === 'reconnecting') {
      dot.className = 'size-2 rounded-full bg-amber-400 animate-pulse motion-reduce:animate-none';
      text.className = 'text-amber-300';
      text.textContent = 'reconnecting';
    } else {
      dot.className = 'size-2 rounded-full bg-zinc-500 animate-pulse motion-reduce:animate-none';
      text.className = 'text-zinc-400';
      text.textContent = 'connecting';
    }
  }

  function setMainDim(dim) {
    $('main').classList.toggle('opacity-50', dim);
  }

  let backoff = 500;
  const BACKOFF_MAX = 10000;

  function connect() {
    const url = (location.protocol === 'https:' ? 'wss' : 'ws') + '://' + location.host + '/ws';
    let ws;
    try {
      ws = new WebSocket(url);
    } catch (e) {
      scheduleReconnect();
      return;
    }
    ws.onopen = () => {
      backoff = 500;
      setConn('live');
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data !== 'string') return;
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      if (msg && typeof msg.type === 'string') handle(msg);
    };
    ws.onclose = () => {
      ws.onclose = null;
      scheduleReconnect();
    };
    ws.onerror = () => {
      // onclose follows and handles the reconnect
    };
  }

  function scheduleReconnect() {
    setConn('reconnecting');
    setMainDim(true);
    const wait = backoff;
    backoff = Math.min(BACKOFF_MAX, backoff * 2);
    setTimeout(connect, wait);
  }

  function wireRoots() {
    const filter = $('roots-filter');
    filter.addEventListener('input', () => invalidate('roots'));
    filter.addEventListener('keydown', (ev) => {
      if (ev.key === 'Escape' && filter.value) {
        filter.value = '';
        invalidate('roots');
      }
    });
    $('roots-more').addEventListener('click', () => {
      rootsAll = !rootsAll;
      invalidate('roots');
    });
  }

  // N starts a session from anywhere on the page, but in a text field or
  // an open dialog.
  function wireKeys() {
    document.addEventListener('keydown', (ev) => {
      if (ev.key !== 'n' && ev.key !== 'N') return;
      if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.isComposing || ev.repeat || !admin) return;
      const t = ev.target;
      if (t.closest('input, textarea, select, [contenteditable]') || document.querySelector('dialog[open]')) return;
      ev.preventDefault();
      openLaunch();
    });
  }

  // Swiping the session to the right on a touch screen does what Esc
  // does: steps back from a workflow agent, or closes it. The swipe starts
  // anywhere but in a text field or a box scrolled sideways, which the
  // finger scrolls back instead.
  function wireChatSwipe() {
    const dlg = $('chat');
    let g = null; // {el, x0, y0, dx, on, t, v}
    const scrolledLeft = (el) => {
      for (; el && el !== dlg; el = el.parentElement) if (el.scrollLeft > 0) return true;
      return false;
    };
    dlg.addEventListener('touchstart', (ev) => {
      g = null;
      if (ev.touches.length !== 1 || !touch()) return;
      const t = ev.touches[0];
      if (ev.target.closest('input, textarea, select, [contenteditable]') || scrolledLeft(ev.target)) return;
      g = { el: chat.view === 'sub' ? $('sub-view') : dlg, x0: t.clientX, y0: t.clientY, dx: 0, on: false, t: ev.timeStamp, v: 0 };
    }, { passive: true });
    dlg.addEventListener('touchmove', (ev) => {
      if (!g) return;
      const t = ev.touches[0];
      const dx = t.clientX - g.x0, dy = t.clientY - g.y0;
      if (!g.on) {
        // Up, down or left: not this gesture. The page may scroll already.
        if (Math.abs(dy) > 10 || dx < -10 || !ev.cancelable) return (g = null);
        if (dx < 12 || dx < 2 * Math.abs(dy)) return;
        g.on = true;
        g.el.style.transition = 'none';
      }
      ev.preventDefault();
      const x = Math.max(0, dx);
      g.v = (x - g.dx) / Math.max(1, ev.timeStamp - g.t); // px/ms
      g.t = ev.timeStamp;
      g.dx = x;
      g.el.style.translate = `${x}px 0`;
    }, { passive: false });
    const end = (ev) => {
      const s = g;
      g = null;
      if (!s || !s.on) return;
      const w = s.el.getBoundingClientRect().width;
      const go = ev.type === 'touchend' && (s.dx > w / 3 || (s.dx > 40 && s.v > 0.5));
      const done = () => {
        s.el.style.transition = '';
        s.el.style.translate = '';
        if (!go) return;
        if (s.el === dlg) closeChat();
        else setView('wf');
      };
      if (matchMedia('(prefers-reduced-motion: reduce)').matches) return done();
      s.el.style.transition = 'translate 180ms ease-out';
      s.el.style.translate = go ? `${w}px 0` : '';
      setTimeout(done, 190);
    };
    dlg.addEventListener('touchend', end);
    dlg.addEventListener('touchcancel', end);
  }

  // Touch feedback. Safari has no navigator.vibrate, but it ticks the
  // Taptic Engine when the user toggles a switch checkbox (<input switch>),
  // also through its label. Since iOS 26.5 a label.click() from script no
  // longer does, only a real tap: so on iOS each control gets a clear label
  // over it with a hidden switch inside (see input.css), the way
  // github.com/tijnjh/ios-haptics does. The tap lands on the label and
  // bubbles to the control's handlers as before, but the label's default
  // action (toggling the switch) displaces the control's own: links,
  // summaries and submit buttons get theirs back below. Elsewhere
  // navigator.vibrate stands in.
  const TAPPABLE = 'button, a[href], summary, select, input[type="checkbox"], input[type="radio"], .cursor-pointer';
  const TYPING = 'textarea, input:not([type="checkbox"]):not([type="radio"])';
  const HAPTIC_HOST = 'button, a[href], summary, .cursor-pointer:not(label, input, select)';
  const IOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

  function addHaptic(el) {
    for (const c of el.children) if (c.hasAttribute('data-haptic')) return;
    const sw = h('input');
    sw.type = 'checkbox';
    sw.setAttribute('switch', '');
    sw.tabIndex = -1;
    // The label passes its click on to the switch: keep that copy from
    // reaching the control's handlers a second time.
    sw.addEventListener('click', (ev) => ev.stopPropagation());
    const lab = h('label', '', sw);
    lab.setAttribute('data-haptic', '');
    lab.setAttribute('aria-hidden', 'true');
    el.setAttribute('data-haptic-host', '');
    // First, so that controls nested in el paint over its label.
    el.prepend(lab);
  }

  function wireHaptics() {
    const scan = (root) => {
      if (root.matches(HAPTIC_HOST)) addHaptic(root);
      for (const el of root.querySelectorAll(HAPTIC_HOST)) addHaptic(el);
    };
    scan(document.body);
    // New controls, and controls whose content (label too) was replaced.
    new MutationObserver((recs) => {
      for (const r of recs) {
        if (r.target.matches(HAPTIC_HOST)) addHaptic(r.target);
        for (const n of r.addedNodes) if (n.nodeType === Node.ELEMENT_NODE) scan(n);
      }
    }).observe(document.body, { childList: true, subtree: true });
    // Whether the control's own default still runs differs between
    // engines, so see after the click what it did and do the rest. A link
    // cannot tell, so it loses its href for the click and is followed here.
    let submits = 0;
    document.addEventListener('submit', () => submits++, true);
    let link = null;
    window.addEventListener('click', (ev) => {
      const lab = ev.target;
      link = null;
      if (!(lab instanceof HTMLLabelElement) || !lab.hasAttribute('data-haptic')) return;
      const el = lab.parentElement;
      if (el.matches('a[href]')) {
        link = el.getAttribute('href');
        el.removeAttribute('href');
        const href = link;
        setTimeout(() => el.setAttribute('href', href));
        return;
      }
      const details = el.matches('summary') && el.parentElement.matches('details') && el.parentElement;
      const open = details && details.open;
      const n = submits;
      setTimeout(() => {
        if (ev.defaultPrevented) return;
        if (details) {
          if (details.open === open) details.open = !open;
        } else if (el.matches('button') && el.type === 'submit' && el.form && submits === n) {
          el.form.requestSubmit(el);
        }
      });
    }, true);
    // On window, so after the page's handlers: a prevented default stays
    // so. Still in the tap, so a new tab is no popup.
    window.addEventListener('click', (ev) => {
      const href = link;
      link = null;
      if (href == null || ev.defaultPrevented) return;
      if (ev.target.parentElement.target === '_blank') window.open(href, '_blank', 'noopener,noreferrer');
      else location.assign(href);
    });
  }

  function wireTouch() {
    // Without a touch listener iOS Safari skips :active (see input.css).
    document.addEventListener('touchstart', () => {}, { passive: true });
    if (IOS) return wireHaptics();
    if (!navigator.vibrate) return;
    let last = 0;
    document.addEventListener('click', (ev) => {
      if (ev.target.closest(TYPING)) return;
      const el = ev.target.closest(TAPPABLE);
      if (!el || el.matches(':disabled') || matchMedia('(pointer: fine)').matches) return;
      // A label click comes back as a click on its input: one tick for both.
      const now = Date.now();
      if (now - last < 80) return;
      last = now;
      navigator.vibrate(8);
    }, true);
  }

  // ------------------------------------------------------------------- boot

  linkToken = takeLinkToken();
  wireUnlock();
  wirePush();
  wirePicker();
  wireLaunch();
  wireChat();
  wireRoots();
  wireKeys();
  wireTouch();
  wireSwipe();
  wireChatSwipe();
  // A `fleet web` link opened in a tab that already shows the dashboard.
  window.addEventListener('hashchange', () => {
    if (takeLinkToken()) checkSession(true);
  });
  // Another tab signed in or out.
  window.addEventListener('storage', (ev) => {
    if (ev.key === TOKEN_KEY || ev.key === null) checkSession(false);
  });
  setConn('connecting');
  renderAccess();
  invalidate('agents', 'peers', 'devices', 'roots');
  connect();
})();
