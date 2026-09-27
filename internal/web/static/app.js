// fleet dashboard: live view fed by the daemon's /ws endpoint, plus root
// management (a folder picker over /api/) once this browser holds the
// admin token that `fleet web` prints.
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

  function agentRow(a) {
    const st = stateOf(a);
    const done = isFinished(a);
    const li = h('li', 'relative flex flex-col gap-1.5 px-4 py-3 transition-colors hover:bg-ink-850 sm:flex-row sm:gap-3');
    li.dataset.id = a.id;
    li.append(h('span', 'absolute inset-y-2 left-0 w-0.5 rounded-r ' + st.edge));

    const body = h('div', 'min-w-0 flex-1');

    // line 1: name, adapter, badges ... updated
    const top = h('div', 'flex flex-wrap items-center gap-x-2 gap-y-1');
    const name = h('span', done ? 'truncate font-mono text-[13px] font-semibold text-zinc-400' : 'truncate font-mono text-[13px] font-semibold text-zinc-100', a.name || a.id);
    name.title = a.id;
    top.append(name, stateBadge(a));
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

    li.append(body, side);
    return flash(li, 'agent:' + a.id);
  }

  // ------------------------------------------------------------- renderers

  function renderAgents() {
    const all = [...S.agents.values()];
    const live = all.filter((a) => !isFinished(a)).sort((x, y) =>
      stateOf(x).rank - stateOf(y).rank || (y.updatedAtMs || 0) - (x.updatedAtMs || 0) || x.name.localeCompare(y.name));
    const hist = all.filter(isFinished).sort((x, y) => (y.updatedAtMs || 0) - (x.updatedAtMs || 0));

    const ul = $('agents-live');
    if (live.length) ul.replaceChildren(...live.map(agentRow));
    else ul.replaceChildren((S.snapshotDone ? emptyRow('no live agents', 'start one with', 'fleet run claude code:api') : emptyRow('loading…')));

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
      ul.replaceChildren(emptyRow('no paired devices', 'pair a laptop or phone with', 'fleet pair'));
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
    $('roots-meta').textContent = S.roots.length ? String(S.roots.length) : '';
    if (confirmRemove && !S.roots.some((r) => r.name === confirmRemove)) confirmRemove = '';
    if (!S.roots.length) {
      if (admin) {
        const li = emptyRow('no roots yet', 'agents can only start inside a root folder');
        const add = h('button', 'mt-3 inline-flex items-center gap-1 rounded-md bg-emerald-400 px-3 py-1.5 text-xs font-semibold text-ink-950 hover:bg-emerald-300 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-400', '+ Add a folder');
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
      const yes = h('button', 'rounded-md bg-rose-500/15 px-2 py-1 text-xs font-medium text-rose-200 ring-1 ring-inset ring-rose-500/40 hover:bg-rose-500/25 focus-visible:outline-2 focus-visible:outline-rose-400', 'Remove');
      yes.type = 'button';
      yes.dataset.focus = 'confirm-remove';
      yes.addEventListener('click', () => removeRoot(r.name));
      const no = h('button', 'rounded-md px-2 py-1 text-xs text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', 'Keep');
      no.type = 'button';
      no.addEventListener('click', () => {
        confirmRemove = '';
        focusAfterRender('[data-remove="' + CSS.escape(r.name) + '"]');
        invalidate('roots');
      });
      return h('div', 'flex shrink-0 flex-col items-end gap-1',
        h('span', 'text-[11px] text-zinc-500', 'running agents keep going'),
        h('div', 'flex items-center gap-1', no, yes));
    }
    const b = h('button', 'shrink-0 rounded-md px-2 py-1 text-xs text-zinc-600 hover:bg-ink-800 hover:text-rose-300 focus-visible:outline-2 focus-visible:outline-emerald-400 group-hover:text-zinc-400', 'remove');
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
    for (const a of S.agents.values()) {
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
    for (const a of S.agents.values()) if (a.state === 'needs_input') input++;
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
        invalidate('peers');
        break;
      }
      case 'roots': {
        const next = msg.roots || [];
        if (S.snapshotDone) diffMark(S.roots, next, (r) => r.name, 'root:');
        S.roots = next;
        invalidate('roots');
        if (picker.open) renderPicker();
        break;
      }
      case 'agent': {
        const a = msg.agent;
        if (!a || !a.id) break;
        const prev = S.agents.get(a.id);
        if (S.snapshotDone && (!prev || !same(prev, a))) markChanged('agent:' + a.id);
        S.agents.set(a.id, a);
        invalidate('agents');
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
  // link (http://host:7421/#token=...). It is kept in localStorage, which is
  // per origin (port included, unlike cookies), and sent as a Bearer header.
  // The fragment never reaches the server and is removed from the address
  // bar right away.

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
    constructor(status, message) {
      super(message);
      this.status = status;
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
    if (!res.ok) throw new APIError(res.status, (data && data.error) || `request failed (${res.status})`);
    return data;
  }

  function setAdmin(on) {
    if (admin === on) return;
    admin = on;
    confirmRemove = '';
    if (!on) closePicker();
    renderAccess();
    invalidate('roots');
  }

  // lostAdmin handles a rejected token (rotated, or from another server).
  function lostAdmin() {
    const had = admin;
    setToken('');
    setAdmin(false);
    if (had) toast('admin access ended: run `fleet web` on the server for a new link', 'warn');
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
      : 'the saved admin token is no longer valid: run `fleet web` for a new link', 'warn');
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
      pill.title = `To manage this fleet here, run "fleet web" on ${where} and open the link it prints.`;
    }
    $('signout').hidden = !admin;
    $('roots-add').hidden = !admin;
    $('roots-hint').hidden = admin;
    $('footer-mode').textContent = admin ? 'admin' : 'read-only view';
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
  // A folder browser over GET /api/fs: walk the server's folders and add
  // the current one as a root.

  const picker = {
    open: false,
    dir: null, // last listing: {path, parent, home, entries, truncated}
    seq: 0, // request counter; stale replies are dropped
    loading: false,
    error: '',
    busy: false, // an add is in flight
    adapters: null, // [{id, name, available}] once loaded
  };

  const joinPath = (dir, name) => (dir.endsWith('/') ? dir : dir + '/') + name;
  const baseName = (p) => p.split('/').filter(Boolean).pop() || '';
  const rootAt = (path) => S.roots.find((r) => r.path === path);

  // rootAround is the most specific root containing path.
  function rootAround(path) {
    let best = null;
    for (const r of S.roots) {
      const inside = path === r.path || path.startsWith(r.path.endsWith('/') ? r.path : r.path + '/');
      if (inside && (!best || r.path.length > best.path.length)) best = r;
    }
    return best;
  }

  function openPicker() {
    if (!admin) return;
    $('picker-host').textContent = (S.server && (S.server.hostname || S.server.name)) || 'the server';
    $('picker-name').value = '';
    $('picker-trust').checked = false;
    $('picker-filter').value = '';
    picker.error = '';
    picker.open = true;
    $('picker').showModal();
    renderAdapters();
    renderPicker();
    browse(picker.dir ? picker.dir.path : '');
    loadAdapters();
    $('picker-filter').focus();
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
      dir = await api('GET', '/api/fs?' + q);
    } catch (e) {
      err = e.message;
    }
    if (seq !== picker.seq) return;
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
    try {
      const r = await api('GET', '/api/adapters');
      picker.adapters = (r && r.adapters) || [];
    } catch (e) {
      picker.adapters = null;
    }
    renderAdapters();
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
      const label = h('label', 'inline-flex cursor-pointer select-none items-center gap-1.5 rounded-md border border-ink-600 bg-ink-900 px-2 py-1 font-mono text-[12px] text-zinc-300 hover:border-zinc-500 has-checked:border-emerald-400/50 has-checked:bg-emerald-400/10 has-checked:text-emerald-200', cb, a.id);
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
    if (picker.error) {
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
    add.disabled = !dir || picker.loading || picker.busy || !!existing;
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
      const b = h('button', 'rounded px-1 py-0.5 text-zinc-400 hover:bg-ink-800 hover:text-zinc-100 focus-visible:outline-2 focus-visible:outline-emerald-400', label);
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
    if (!dir) {
      ul.replaceChildren(pickerNote(picker.loading ? 'loading…' : 'nothing to show'));
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
    const b = h('button', 'group flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-[13px] text-zinc-300 hover:bg-ink-800 hover:text-zinc-50 focus-visible:bg-ink-800 focus-visible:text-zinc-50 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-emerald-400/60');
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
      $('picker-filter').focus();
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
    picker.busy = true;
    picker.error = '';
    renderPicker();
    try {
      const r = await api('POST', '/api/roots', { path: dir.path, name, adapters, trust: $('picker-trust').checked });
      markChanged('root:' + r.root.name);
      toast(`added root ${r.root.name} → ${shortPath(r.root.path)}`);
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
  wirePicker();
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
