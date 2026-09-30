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
    const li = h('li', admin
      ? 'relative flex cursor-pointer flex-col gap-1.5 px-4 py-3 transition-colors hover:bg-ink-850 sm:flex-row sm:gap-3'
      : 'relative flex flex-col gap-1.5 px-4 py-3 transition-colors hover:bg-ink-850 sm:flex-row sm:gap-3');
    li.dataset.id = a.id;
    li.append(h('span', 'absolute inset-y-2 left-0 w-0.5 rounded-r ' + st.edge));
    if (admin) li.addEventListener('click', (ev) => {
      if (!ev.target.closest('button') && !getSelection().toString()) openChat(host, a.id, a);
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
    return flash(li, 'agent:' + agentKey(host, a.id));
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

  // ------------------------------------------------------------- renderers

  function renderAgents() {
    const all = allAgents();
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
    $('agents-meta').textContent = `${live.length} live · ${all.length} total`;
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
    ul.replaceChildren(...S.roots.map((r) => {
      const li = h('li', 'group flex items-start gap-3 px-4 py-3');
      const main = h('div', 'min-w-0 flex-1');
      const top = h('div', 'flex flex-wrap items-center gap-2');
      top.append(h('span', 'font-mono text-[13px] font-semibold text-zinc-100', r.name));
      top.append(r.trust
        ? chip('trusted', 'inline-flex items-center rounded-full bg-emerald-400/10 px-2 py-px text-[11px] font-medium text-emerald-300 ring-1 ring-inset ring-emerald-400/30')
        : chip('untrusted', 'inline-flex items-center rounded-full bg-zinc-400/10 px-2 py-px text-[11px] font-medium text-zinc-400 ring-1 ring-inset ring-zinc-400/25'));
      const path = h('div', 'mt-0.5 truncate font-mono text-xs text-zinc-500', shortPath(r.path));
      path.title = r.path;
      main.append(top, path);
      if (r.adapters && r.adapters.length) {
        main.append(h('div', 'mt-1.5 flex flex-wrap gap-1', ...r.adapters.map((a) => chip(a))));
      }
      li.append(main);
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
    const b = h('button', 'touch:min-h-11 shrink-0 rounded-md px-2 py-1 text-xs text-zinc-600 hover:bg-ink-800 hover:text-rose-300 focus-visible:outline-2 focus-visible:outline-emerald-400 group-hover:text-zinc-400', 'remove');
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
    $('srv-version').textContent = s.version || 'dev';
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
        if (launch.open && !launch.host) renderLaunch();
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
    } else {
      closeUnlock();
    }
    renderAccess();
    invalidate('roots', 'agents');
    scheduleRemote(0);
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
    $('roots-add').hidden = !admin;
    $('agents-new').hidden = !admin;
    $('roots-hint').hidden = admin;
    $('footer-mode').textContent = admin ? 'admin' : 'read-only view';
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

  // --------------------------------------------------------------- launcher
  //
  // Start a session: machine, agent CLI, a folder inside one of its roots,
  // an optional prompt. Other machines are reached through /api/hosts/.

  const launch = {
    open: false,
    host: '', // server id; '' = this daemon
    adapters: null, // [{id, name, available}] of the machine
    roots: null, // roots of another machine (this one's are S.roots)
    root: '', // chosen root name
    sub: '', // folder inside the root, relative, '' = the root itself
    dir: null, // listing of root/sub
    adapter: '',
    seq: 0, // stale replies are dropped
    loading: false,
    busy: false, // a start is in flight
    error: '',
  };

  const launchAPI = (path) => (launch.host ? `/api/hosts/${encodeURIComponent(launch.host)}/${path}` : '/api/' + path);
  const launchRoots = () => (launch.host ? launch.roots || [] : S.roots);
  const launchRoot = () => launchRoots().find((r) => r.name === launch.root);

  function openLaunch() {
    if (!admin) return;
    launch.open = true;
    launch.error = '';
    launch.busy = false;
    if (launch.host && !pickerHosts().some((x) => x.id === launch.host)) launch.host = '';
    $('launch-prompt').value = '';
    $('launch-name').value = '';
    $('launch-branch').value = '';
    $('launch').showModal();
    renderLaunchHosts();
    checkHosts();
    loadLaunchHost();
    if (!touch()) $('launch-prompt').focus();
  }

  function closeLaunch() {
    const d = $('launch');
    if (d.open) d.close();
  }

  // loadLaunchHost loads the adapters and roots of the chosen machine.
  async function loadLaunchHost() {
    const host = launch.host;
    const seq = ++launch.seq;
    launch.adapters = null;
    launch.roots = null;
    launch.dir = null;
    launch.loading = true;
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
    if (seq !== launch.seq) return;
    if (host && ads) setHostState(host, 'joined');
    launch.adapters = ads || [];
    launch.roots = roots;
    launch.loading = false;
    launch.error = err;
    const all = launchRoots();
    if (!all.some((r) => r.name === launch.root)) {
      launch.root = all.length ? all[0].name : '';
      launch.sub = '';
    }
    renderLaunch();
    browseLaunch();
  }

  // browseLaunch lists the folder launch.sub of the chosen root.
  async function browseLaunch() {
    const root = launchRoot();
    const seq = ++launch.seq;
    if (!root) {
      launch.dir = null;
      renderLaunch();
      return;
    }
    launch.loading = true;
    renderLaunch();
    let dir = null;
    let err = '';
    try {
      dir = await api('GET', launchAPI('fs?' + new URLSearchParams({ path: launch.sub ? joinPath(root.path, launch.sub) : root.path })));
    } catch (e) {
      err = e.message;
    }
    if (seq !== launch.seq) return;
    launch.loading = false;
    launch.dir = dir;
    launch.error = err;
    renderLaunch();
  }

  function selectLaunchHost(id) {
    if (launch.host === id) return;
    launch.host = id;
    launch.root = '';
    launch.sub = '';
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
      box.replaceChildren(...allowed.map((a) => {
        const rb = h('input', 'sr-only');
        rb.type = 'radio';
        rb.name = 'launch-adapter';
        rb.value = a.id;
        rb.checked = a.id === launch.adapter;
        rb.disabled = !a.available;
        const label = h('label', a.available ? HOST_CHIP : 'touch:min-h-11 inline-flex cursor-not-allowed select-none items-center gap-1.5 rounded-md border border-ink-700 bg-ink-900 px-2 py-1 font-mono text-[12px] text-zinc-600', rb, a.name || a.id);
        if (!a.available) {
          label.append(h('span', 'font-sans text-[11px]', 'not installed'));
          label.title = `${a.name} is not installed on ${hostName(launch.host)}`;
        }
        return label;
      }));
    }

    // roots
    const sel = $('launch-root');
    const roots = launchRoots();
    const opts = roots.map((r) => {
      const o = h('option', '', `${r.name} · ${shortPath(r.path)}`);
      o.value = r.name;
      o.selected = r.name === launch.root;
      return o;
    });
    if (!roots.length) {
      const o = h('option', '', launch.adapters ? 'no roots on this machine' : 'loading…');
      o.value = '';
      opts.push(o);
    }
    sel.replaceChildren(...opts);
    sel.disabled = !roots.length;

    renderLaunchCrumbs(root);
    renderLaunchList(root, notJoined);

    const st = $('launch-status');
    if (notJoined) {
      st.className = 'min-h-5 text-xs text-amber-200/90';
      st.textContent = `${hostName(launch.host)} does not have this fleet's key: run \`fleet start --join <key>\` there (see Add folder).`;
    } else if (launch.error) {
      st.className = 'min-h-5 text-xs text-rose-300';
      st.textContent = launch.error;
    } else if (root) {
      st.className = 'min-h-5 min-w-0 break-all text-xs text-zinc-500';
      st.replaceChildren(h('span', '', 'starts ', h('span', 'font-mono text-zinc-200', launch.adapter || '…'), ' in ',
        h('span', 'font-mono text-zinc-200', shortPath(launch.sub ? joinPath(root.path, launch.sub) : root.path))));
    } else {
      st.className = 'min-h-5 text-xs text-zinc-500';
      st.textContent = launch.adapters && !roots.length ? 'Add a root folder on this machine first (Roots → Add folder).' : '';
    }
    const start = $('launch-start');
    start.disabled = !root || !launch.adapter || launch.busy || notJoined;
    start.textContent = launch.busy ? 'Starting…' : 'Start';
  }

  function renderLaunchCrumbs(root) {
    const nav = $('launch-crumbs');
    if (!root) {
      nav.replaceChildren();
      return;
    }
    const crumb = (label, sub, current) => {
      if (current) {
        const el = h('span', 'rounded px-1 py-0.5 font-semibold text-zinc-100', label);
        el.setAttribute('aria-current', 'location');
        return el;
      }
      const b = h('button', 'touch:min-h-11 touch:min-w-11 rounded px-1 py-0.5 text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', label);
      b.type = 'button';
      b.addEventListener('click', () => {
        launch.sub = sub;
        browseLaunch();
      });
      return b;
    };
    const parts = launch.sub ? launch.sub.split('/') : [];
    const items = [crumb(root.name + ':', '', !parts.length)];
    parts.forEach((p, i) => {
      if (i > 0) items.push(h('span', 'text-zinc-700', '/'));
      items.push(crumb(p, parts.slice(0, i + 1).join('/'), i === parts.length - 1));
    });
    nav.replaceChildren(...items);
  }

  const LAUNCH_LIST_MAX = 400;

  function renderLaunchList(root, notJoined) {
    const ul = $('launch-list');
    ul.classList.toggle('opacity-50', launch.loading);
    const note = (t) => h('li', 'px-2 py-5 text-center font-mono text-xs text-zinc-600', '// ' + t);
    if (notJoined) return ul.replaceChildren(note('not in this fleet yet'));
    if (!root) return ul.replaceChildren(note(launch.loading ? 'loading…' : 'no root chosen'));
    const dir = launch.dir;
    if (!dir) return ul.replaceChildren(note(launch.loading ? 'loading…' : 'cannot list this folder'));
    const items = [];
    const row = (label, onClick, extra) => {
      const b = h('button', 'touch:min-h-11 group flex w-full items-center gap-2 rounded px-2 py-1 text-left font-mono text-[12.5px] text-zinc-300 hover:bg-ink-800 hover:text-zinc-50 focus-visible:bg-ink-800 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-emerald-400/60', ...label);
      b.type = 'button';
      if (extra) b.append(extra);
      b.append(h('span', 'ml-auto shrink-0 pl-2 text-zinc-700 group-hover:text-zinc-400', '→'));
      b.addEventListener('click', onClick);
      return h('li', '', b);
    };
    if (launch.sub) {
      items.push(row([h('span', 'text-zinc-500', '../')], () => {
        launch.sub = launch.sub.split('/').slice(0, -1).join('/');
        browseLaunch();
      }));
    }
    for (const e of dir.entries.slice(0, LAUNCH_LIST_MAX)) {
      items.push(row([h('span', 'min-w-0 truncate', e.name, h('span', 'text-zinc-600', '/'))], () => {
        launch.sub = launch.sub ? launch.sub + '/' + e.name : e.name;
        browseLaunch();
      }, e.git ? chip('git', 'inline-flex shrink-0 items-center rounded border border-violet-400/25 bg-violet-400/10 px-1.5 py-px font-mono text-[10px] text-violet-300') : null));
    }
    if (!dir.entries.length) items.push(note('no subfolders: the agent starts here'));
    if (dir.entries.length > LAUNCH_LIST_MAX) items.push(note(`${dir.entries.length - LAUNCH_LIST_MAX} more not shown`));
    ul.replaceChildren(...items);
  }

  async function startSession(ev) {
    ev.preventDefault();
    const root = launchRoot();
    if (!root || !launch.adapter || launch.busy) return;
    const host = launch.host;
    launch.busy = true;
    launch.error = '';
    renderLaunch();
    try {
      const r = await api('POST', launchAPI('agents'), {
        adapter: launch.adapter,
        root: root.name,
        path: launch.sub,
        prompt: $('launch-prompt').value,
        name: $('launch-name').value.trim(),
        branch: $('launch-branch').value.trim(),
        isolation: $('launch-iso').value,
        sandbox: $('launch-sandbox').value,
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
      openChat(host, r.agent.id, r.agent);
      return;
    } catch (e) {
      launch.error = e.message;
    }
    launch.busy = false;
    renderLaunch();
  }

  function wireLaunch() {
    const dlg = $('launch');
    $('agents-new').addEventListener('click', openLaunch);
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
    $('launch-root').addEventListener('change', (ev) => {
      launch.root = ev.target.value;
      launch.sub = '';
      renderLaunch();
      browseLaunch();
    });
    // Ctrl/Cmd+Enter in the prompt starts the session.
    $('launch-prompt').addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) {
        ev.preventDefault();
        $('launch-form').requestSubmit();
      }
    });
    dlg.addEventListener('close', () => {
      launch.open = false;
      launch.seq++;
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
  // does not show, keys and messages typed into it, and stop.

  const chat = {
    open: false,
    host: '',
    id: '',
    agent: null,
    file: '', // transcript the offsets belong to; '' = none yet
    start: 0, // offset of the oldest entry shown
    end: 0, // offset after the newest
    seq: 0, // bumped on open and close: loops of an old session stop
    tools: new Map(), // tool call id -> {el, status, body, done}
    orphans: new Map(), // tool call id -> {el, output, error}: a result shown without its call
    loaded: false, // first reply in
    earlier: false, // loading older entries
    screenOpen: false,
    screenAuto: true, // open the screen by itself while the agent needs input
    screenTimer: 0,
    sending: false,
    held: false, // the last message went into a dialog, without Enter
    confirmStop: false,
    stopping: false,
    error: '',
  };

  const chatAPI = (rest) => {
    const base = chat.host ? `/api/hosts/${encodeURIComponent(chat.host)}/agents/` : '/api/agents/';
    return base + encodeURIComponent(chat.id) + '/' + rest;
  };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const chatLive = () => chat.agent && !isFinished(chat.agent);
  const chatDialog = () => chatLive() && chat.agent.state === 'needs_input';

  function openChat(host, id, agent) {
    if (!admin) return;
    closeLaunch();
    chat.seq++;
    Object.assign(chat, {
      open: true, host, id, agent: agent || null, file: '', start: 0, end: 0, loaded: false, earlier: false,
      sending: false, held: false, confirmStop: false, stopping: false, error: '', screenAuto: true,
    });
    chat.tools.clear();
    chat.orphans.clear();
    chat.screenOpen = !!agent && (agent.state === 'needs_input' || agent.adapter === 'shell');
    $('chat-log').replaceChildren(h('div', 'py-10 text-center font-mono text-xs text-zinc-600', '// loading…'));
    $('chat-input').value = '';
    autosize();
    $('chat-screen-pre').textContent = '';
    const d = $('chat');
    if (!d.open) d.showModal();
    renderChatHead();
    renderChatScreen();
    renderChatInput();
    chatLoop(chat.seq);
    if (!touch()) $('chat-input').focus();
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
    if (a.state !== was) paintPending();
    if (a.state === 'needs_input' && was !== 'needs_input' && chat.screenAuto && !chat.screenOpen) {
      chat.screenOpen = true;
      renderChatScreen();
    }
    renderChatHead();
    renderChatInput();
  }

  // chatLoop follows the conversation until the dialog closes or shows
  // another session: the latest entries first, then long polls for more.
  async function chatLoop(seq) {
    let failures = 0;
    while (chat.open && seq === chat.seq) {
      const q = new URLSearchParams();
      if (chat.loaded) {
        q.set('file', chat.file);
        q.set('after', String(chat.end));
        q.set('wait', '1');
        q.set('v', String((chat.agent && chat.agent.updatedAtMs) || 0));
      }
      let r;
      try {
        r = await api('GET', chatAPI('chat?' + q));
      } catch (e) {
        if (seq !== chat.seq) return;
        chat.error = e.message;
        renderChatStatus();
        if (e.status === 404) return; // the agent is gone
        await sleep(Math.min(10000, 1000 * 2 ** failures++));
        continue;
      }
      if (seq !== chat.seq) return;
      failures = 0;
      chat.error = '';
      const first = !chat.loaded;
      chat.loaded = true;
      if (r.agent) chatAgent(chat.host, r.agent);
      applyChat(r, first);
      // A finished session whose transcript is read to the end is done.
      if (!first && !r.more && !r.entries.length && !chatLive()) return;
    }
  }

  function applyChat(r, first) {
    const log = $('chat-log');
    const scroller = $('chat-scroll');
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
    const hadFile = !!chat.file;
    chat.file = r.file || '';
    chat.end = r.end || 0;
    if (hadFile !== !!chat.file) renderChatScreen();
    if (r.reset || first) {
      chat.start = r.start || 0;
      chat.tools.clear();
      chat.orphans.clear();
      log.replaceChildren(...chatHead(), ...entryNodes(r.entries));
      renderChatStatus();
      scroller.scrollTop = scroller.scrollHeight;
      return;
    }
    const nodes = entryNodes(r.entries);
    if (nodes.length) {
      log.querySelector('[data-empty]')?.remove();
      log.append(...nodes);
    }
    renderChatStatus();
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  }

  // chatHead is what goes above the entries: "load earlier", or a note
  // when there is nothing to show.
  function chatHead() {
    if (chat.start > 0) {
      const b = h('button', 'touch:min-h-11 mx-auto rounded-md border border-ink-600 bg-ink-850 px-3 py-1 text-xs text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Load earlier');
      b.type = 'button';
      b.dataset.earlier = '1';
      b.addEventListener('click', loadEarlier);
      return [b];
    }
    if (!chat.file) {
      const a = chat.agent || {};
      const hint = a.adapter === 'shell'
        ? 'A shell keeps no chat. Its terminal is under Screen.'
        : chatLive()
          ? 'No messages yet. The conversation shows up here once the first message is sent.'
          : 'This session left no conversation.';
      const el = h('div', 'py-10 text-center', h('div', 'font-mono text-xs text-zinc-600', '// ' + hint));
      el.dataset.empty = '1';
      return [el];
    }
    return [];
  }

  async function loadEarlier(ev) {
    if (chat.earlier || chat.start <= 0) return;
    const seq = chat.seq;
    const btn = ev.currentTarget;
    chat.earlier = true;
    btn.disabled = true;
    btn.textContent = 'Loading…';
    let r = null;
    try {
      r = await api('GET', chatAPI('chat?' + new URLSearchParams({ file: chat.file, before: String(chat.start) })));
    } catch (e) {
      toast(`could not load earlier messages: ${e.message}`, 'error');
    }
    chat.earlier = false;
    if (seq !== chat.seq) return;
    if (!r || r.reset || r.file !== chat.file) {
      btn.disabled = false;
      btn.textContent = 'Load earlier';
      return;
    }
    const scroller = $('chat-scroll');
    const fromBottom = scroller.scrollHeight - scroller.scrollTop;
    chat.start = r.start || 0;
    // Results in this page attach to the calls in it; later results whose
    // call is in it already showed on their own.
    const known = chat.tools;
    chat.tools = new Map();
    const nodes = entryNodes(r.entries);
    // Their results, if any, are already shown on their own.
    for (const t of chat.tools.values()) {
      if (!t.done) t.status.className = TOOL_STATUS.stale;
      t.done = true;
    }
    for (const [k, v] of known) chat.tools.set(k, v);
    btn.replaceWith(...chatHead(), ...nodes);
    scroller.scrollTop = scroller.scrollHeight - fromBottom;
  }

  // entryNodes renders entries; a result is attached to its tool call.
  function entryNodes(entries) {
    const out = [];
    for (const e of entries || []) {
      const n = entryNode(e);
      if (n) out.push(n);
    }
    return out;
  }

  const timeTitle = (el, e) => {
    if (e.ts) el.title = new Date(e.ts).toLocaleString();
    return el;
  };

  function entryNode(e) {
    switch (e.kind) {
      case 'user':
        return timeTitle(h('div', 'flex justify-end',
          h('div', 'max-w-[85%] whitespace-pre-wrap break-words rounded-lg rounded-br-sm bg-emerald-400/10 px-3 py-2 text-[13.5px] leading-relaxed text-zinc-100 ring-1 ring-inset ring-emerald-400/20', e.text || '')), e);
      case 'assistant':
        return timeTitle(md(e.text || ''), e);
      case 'tool':
        return toolNode(e);
      case 'result': {
        const t = e.id && chat.tools.get(e.id);
        if (t) {
          setToolOutput(t, e.output, e.error);
          return null;
        }
        // The call is before the loaded range (or, rarely, written after
        // its result): shown on its own until the call turns up.
        const el = toolNode({ kind: 'tool', name: 'result', text: oneLine(e.output), output: e.output, error: e.error });
        if (e.id) chat.orphans.set(e.id, { el, output: e.output, error: e.error });
        return el;
      }
      default:
        return timeTitle(h('div', 'whitespace-pre-wrap break-words py-0.5 text-center font-mono text-[11px] text-zinc-500', e.text || ''), e);
    }
  }

  const oneLine = (s) => ((s || '').split('\n').find((l) => l.trim()) || '').trim();

  const TOOL_STATUS = {
    pending: 'size-2 shrink-0 rounded-full border-[1.5px] border-sky-400 border-t-transparent animate-spin motion-reduce:animate-none',
    stale: 'size-1.5 shrink-0 rounded-full bg-zinc-600',
    ok: 'size-1.5 shrink-0 rounded-full bg-emerald-400',
    error: 'size-1.5 shrink-0 rounded-full bg-rose-500',
  };

  function toolNode(e) {
    const d = h('details', 'group min-w-0 rounded-md border border-ink-700 bg-ink-850/50 open:bg-ink-850');
    const status = h('span', '');
    const sum = h('summary', 'touch:min-h-11 flex cursor-pointer list-none items-center gap-2 rounded-md px-2.5 py-1.5 font-mono text-[12px] hover:bg-ink-800/60 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-emerald-400',
      status,
      h('span', 'shrink-0 font-semibold text-sky-300', e.name || 'tool'),
      h('span', 'min-w-0 truncate text-zinc-400', e.text || ''));
    const body = h('div', 'flex min-w-0 flex-col gap-2 border-t border-ink-700 px-2.5 py-2');
    if (e.detail) body.append(codeBlock(e.detail, 'input'));
    d.append(sum, body);
    timeTitle(sum, e);
    const t = { el: d, status, body, done: false };
    const early = e.id && chat.orphans.get(e.id);
    if (early) {
      chat.orphans.delete(e.id);
      early.el.remove();
      setToolOutput(t, early.output, early.error);
    } else if (e.id) {
      // Claude: the output follows in a result.
      status.className = chat.agent && chat.agent.state === 'working' ? TOOL_STATUS.pending : TOOL_STATUS.stale;
      chat.tools.set(e.id, t);
    } else {
      setToolOutput(t, e.output, e.error);
    }
    return d;
  }

  // paintPending shows calls without output as running while the agent
  // works, and as unfinished otherwise (interrupted, or waiting for a
  // permission).
  function paintPending(tools) {
    const cls = chat.agent && chat.agent.state === 'working' ? TOOL_STATUS.pending : TOOL_STATUS.stale;
    for (const t of (tools || chat.tools).values()) if (!t.done) t.status.className = cls;
  }

  function setToolOutput(t, output, error) {
    t.done = true;
    t.status.className = error ? TOOL_STATUS.error : TOOL_STATUS.ok;
    if (error) t.el.classList.add('border-rose-500/30');
    if (output) t.body.append(codeBlock(output, error ? 'error' : 'output'));
    else if (!t.body.childElementCount) t.body.append(h('div', 'font-mono text-[11px] text-zinc-600', '// no output'));
  }

  function codeBlock(text, label) {
    return h('div', 'min-w-0',
      h('div', label === 'error' ? 'mb-1 text-[10px] font-medium uppercase tracking-wider text-rose-300/80' : 'mb-1 text-[10px] font-medium uppercase tracking-wider text-zinc-600', label),
      h('pre', 'max-h-80 overflow-auto whitespace-pre-wrap break-words rounded border border-ink-700 bg-ink-950 px-2.5 py-1.5 font-mono text-[11.5px] leading-snug text-zinc-300', text));
  }

  // md renders the agent's markdown loosely: fenced code, headings, inline
  // `code` and **bold**. Everything else stays text; nothing is parsed as
  // HTML.
  function md(text) {
    const box = h('div', 'flex min-w-0 flex-col gap-2 text-[13.5px] leading-relaxed text-zinc-200');
    const lines = text.split('\n');
    let para = [];
    const flush = () => {
      if (para.length) box.append(inline(h('p', 'whitespace-pre-wrap break-words'), para.join('\n')));
      para = [];
    };
    for (let i = 0; i < lines.length; i++) {
      const l = lines[i];
      if (/^\s*```/.test(l)) {
        flush();
        const code = [];
        for (i++; i < lines.length && !/^\s*```/.test(lines[i]); i++) code.push(lines[i]);
        box.append(h('pre', 'overflow-x-auto rounded-md border border-ink-700 bg-ink-950 px-3 py-2 font-mono text-[12px] leading-snug text-zinc-300', code.join('\n')));
        continue;
      }
      const head = /^(#{1,6})\s+(.*)$/.exec(l);
      if (head) {
        flush();
        box.append(inline(h('div', 'pt-1 font-semibold text-zinc-50'), head[2]));
      } else if (!l.trim()) {
        flush();
      } else {
        para.push(l);
      }
    }
    flush();
    return box;
  }

  function inline(el, text) {
    const re = /`[^`\n]+`|\*\*[^*\n]+\*\*/g;
    let last = 0;
    let m;
    while ((m = re.exec(text))) {
      if (m.index > last) el.append(text.slice(last, m.index));
      const t = m[0];
      el.append(t[0] === '`'
        ? h('code', 'rounded bg-ink-800 px-1 py-px font-mono text-[12px] text-emerald-200/90', t.slice(1, -1))
        : h('strong', 'font-semibold text-zinc-50', t.slice(2, -2)));
      last = m.index + t.length;
    }
    if (last < text.length) el.append(text.slice(last));
    return el;
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
    if (a.state === 'needs_input') {
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

  // renderChatStop draws Stop, and its confirmation in a bar under the
  // header rather than in Stop's place, where a double tap would land on it.
  function renderChatStop() {
    const box = $('chat-stop-box');
    const bar = $('chat-confirm');
    if (!chatLive()) {
      box.replaceChildren();
      bar.hidden = true;
      return;
    }
    const b = h('button', 'touch:min-h-11 rounded-md border border-ink-600 bg-ink-850 px-2 py-1 text-xs font-medium text-zinc-300 hover:border-rose-500/40 hover:bg-rose-500/10 hover:text-rose-200 aria-expanded:border-rose-500/40 aria-expanded:bg-rose-500/10 aria-expanded:text-rose-200 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', 'Stop');
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
    const yes = h('button', 'touch:min-h-11 rounded-md border border-rose-500/40 bg-rose-500/15 px-3 py-1 text-xs font-medium text-rose-200 hover:bg-rose-500/25 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-rose-400', chat.stopping ? 'Stopping…' : 'Stop session');
    yes.type = 'button';
    yes.disabled = chat.stopping;
    yes.dataset.focus = 'chat-stop-yes';
    yes.addEventListener('click', stopChat);
    bar.replaceChildren(
      h('p', 'min-w-48 flex-1 text-xs text-rose-100/90', h('span', 'font-semibold', 'Stop this session?'), ' The agent is killed; its files are kept.'),
      h('div', 'ml-auto flex items-center gap-2', keep, yes));
  }

  async function stopChat() {
    const seq = chat.seq;
    chat.stopping = true;
    renderChatStop();
    try {
      const r = await api('POST', chatAPI('stop'), {});
      if (seq === chat.seq && r && r.agent) chatAgent(chat.host, r.agent);
      toast(`stopped ${(r && r.agent && r.agent.name) || chat.id}`);
    } catch (e) {
      toast(`could not stop: ${e.message}`, 'error');
    }
    if (seq !== chat.seq) return;
    chat.stopping = false;
    chat.confirmStop = false;
    renderChatHead();
    renderChatInput();
  }

  function renderChatStatus() {
    const st = $('chat-status');
    if (chat.error) {
      st.className = 'mt-1 min-h-4 truncate text-[11px] text-rose-300';
      st.textContent = chat.error;
      return;
    }
    if (chat.held && chatLive()) {
      st.className = 'mt-1 min-h-4 truncate text-[11px] text-amber-300';
      st.textContent = 'The agent shows a dialog: your text was typed, but Enter was not pressed. Answer with the keys.';
      return;
    }
    st.className = 'mt-1 min-h-4 truncate text-[11px] text-zinc-500';
    const a = chat.agent;
    st.textContent = !a ? '' : !chatLive() ? 'the session has ended'
      : chatDialog() ? 'in a dialog, Send types your text without pressing Enter: Enter picks the highlighted option'
      : a.state === 'working' ? 'working… new messages appear as the agent writes them' : '';
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
      : chatDialog() ? 'Type into the dialog (Enter is not pressed)'
      : touch() ? 'Message the agent' : 'Message the agent · Enter sends, Shift+Enter for a new line';
    $('chat-send').disabled = !live || chat.sending;
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
    if (!text.trim() || chat.sending || !chatLive()) return;
    const seq = chat.seq;
    chat.sending = true;
    chat.held = false;
    renderChatInput();
    let held = false;
    try {
      // The daemon holds Enter back if the agent shows a dialog, where
      // Enter picks the highlighted option whatever was typed.
      const r = await api('POST', chatAPI('input'), { text, submit: true });
      held = !!(r && r.held);
      if (seq === chat.seq) {
        ta.value = '';
        autosize();
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

  // autosize grows the message box with its text, up to its max height.
  function autosize() {
    const ta = $('chat-input');
    ta.style.height = 'auto';
    ta.style.height = ta.scrollHeight + 2 + 'px';
  }

  function renderChatScreen() {
    // Without a chat (a shell, or before the first message) the screen
    // takes the room of the empty log.
    const big = chat.screenOpen && !chat.file;
    $('chat-screen').hidden = !chat.screenOpen;
    $('chat-screen').classList.toggle('max-h-[45%]', !big);
    $('chat-screen').classList.toggle('flex-1', big);
    $('chat-scroll').classList.toggle('flex-1', !big);
    $('chat-scroll').classList.toggle('flex-none', big);
    $('chat-screen-toggle').setAttribute('aria-pressed', String(chat.screenOpen));
    if (chat.screenOpen) refreshScreen();
    else clearTimeout(chat.screenTimer);
  }

  const SCREEN_POLL_MS = 1500;

  async function refreshScreen() {
    clearTimeout(chat.screenTimer);
    if (!chat.open || !chat.screenOpen) return;
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

  function wireChat() {
    const dlg = $('chat');
    $('chat-close').addEventListener('click', closeChat);
    $('chat-form').addEventListener('submit', sendMessage);
    const ta = $('chat-input');
    ta.addEventListener('input', autosize);
    ta.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing && !touch()) {
        ev.preventDefault();
        $('chat-form').requestSubmit();
      }
    });
    $('chat-keys').addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-key]');
      if (b) sendKey(b.dataset.key);
    });
    $('chat-screen-toggle').addEventListener('click', () => {
      chat.screenOpen = !chat.screenOpen;
      chat.screenAuto = false;
      renderChatScreen();
    });
    dlg.addEventListener('close', () => {
      chat.open = false;
      chat.seq++;
      clearTimeout(chat.screenTimer);
      chat.tools.clear();
      chat.orphans.clear();
    });
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

  // ------------------------------------------------------------------- boot

  linkToken = takeLinkToken();
  wireUnlock();
  wirePicker();
  wireLaunch();
  wireChat();
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
