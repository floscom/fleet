// fleet web app: sessions with coding agents on every machine of the fleet,
// as chats. The daemon's /ws endpoint feeds it live (agents, machines,
// roots, paired devices); the admin API (/api/, with the token `fleet web`
// prints, or the one the unlock dialog trades the fleet key for) starts,
// follows, answers and ends sessions, here and on the fleet's other
// machines (/api/hosts/<id>/...).
//
// Vanilla ES2020, no framework, no build step. The server only ever sends
// on the socket; we never write to it. The DOM is built with createElement
// and textContent only (never innerHTML with server data). Look and layout:
// app.css and DESIGN.md.
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
    host: new Map(), // server id -> how its machine is doing (see pollUsage)
    update: new Map(), // server id -> where its daemon stands with updates (see pollUsage)
    peerRoots: new Map(), // server id of another fleet -> its roots, for Devices
    snapshotDone: false,
    unseen: null, // ids of the agents shown that the coming snapshot has not named yet (see resetState)
    peersSeen: false, // first complete (post-browse) peers list of this connection received
    online: false, // the socket is up
    connShown: false, // the banner says it is not
  };

  // What the page shows, and choices kept in this browser.
  const U = {
    view: 'home', // 'home' (phones: the list), 'new', 'chat', 'devices'
    key: '', // agent key of the session shown
    groupBy: load('fleet.groupBy', 'folder'),
    collapsed: new Set(load('fleet.collapsed', [])),
    q: '', // the session search
    archivedOpen: false,
    sideOff: false, // desktops: the sidebar is hidden
    drawer: false, // tablets: the sidebar drawer is open
  };

  // --------------------------------------------------------------- helpers

  const $ = (id) => document.getElementById(id);
  const SVGNS = 'http://www.w3.org/2000/svg';
  const DAY = 86400000;

  // h('div', 'classes' | {attributes}, ...children). Children are nodes or
  // text; arrays are flattened, null, false and '' skipped. Attributes
  // named on... are listeners.
  function h(tag, a, ...kids) {
    const el = document.createElement(tag);
    if (typeof a === 'string') el.className = a;
    else if (a) {
      for (const [k, v] of Object.entries(a)) {
        if (v == null || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'value') el.value = v;
        else if (k === 'checked') el.checked = !!v;
        else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v === true ? '' : String(v));
      }
    }
    return add(el, kids);
  }

  function add(el, kids) {
    for (const c of kids.flat(Infinity)) {
      if (c == null || c === false || c === '') continue;
      el.append(c instanceof Node ? c : String(c));
    }
    return el;
  }

  // fill replaces el's children like h() adds them.
  function fill(el, ...kids) {
    el.replaceChildren();
    return add(el, kids);
  }

  function svgUse(href, cls) {
    const s = document.createElementNS(SVGNS, 'svg');
    s.setAttribute('class', cls);
    s.setAttribute('aria-hidden', 'true');
    const u = document.createElementNS(SVGNS, 'use');
    u.setAttribute('href', href);
    s.append(u);
    return s;
  }
  const icon = (name, cls) => svgUse('#i-' + name, 'i' + (cls ? ' ' + cls : ''));
  const LOGOS = { claude: 'claude', codex: 'codex', gemini: 'gemini', shell: 'shell' };
  // AGENT_NAMES are the agent CLIs' short names, for sentences.
  const AGENT_NAMES = { claude: 'Claude', codex: 'Codex', gemini: 'Gemini' };
  const logo = (adapter, cls) => svgUse('#logo-' + (LOGOS[adapter] || 'shell'), 'logo logo-' + (LOGOS[adapter] || 'shell') + (cls ? ' ' + cls : ''));

  function iconBtn(name, label, onclick, o = {}) {
    return h('button', { type: 'button', class: 'icon-btn ' + (o.cls || ''), 'aria-label': label, title: o.title || label, onclick, ...o.attrs }, icon(name));
  }

  // The wordmark: FLEET in figlet's "ANSI Shadow" letters, drawn as
  // vectors so it stays crisp when small. Full blocks are the letters; the
  // box-drawing characters become a thin line shadow.
  const WORDMARK = [
    '███████╗██╗     ███████╗███████╗████████╗',
    '██╔════╝██║     ██╔════╝██╔════╝╚══██╔══╝',
    '█████╗  ██║     █████╗  █████╗     ██║',
    '██╔══╝  ██║     ██╔══╝  ██╔══╝     ██║',
    '██║     ███████╗███████╗███████╗   ██║',
    '╚═╝     ╚══════╝╚══════╝╚══════╝   ╚═╝',
  ];
  function wordmark() {
    const CW = 6;
    const RH = 10;
    let ink = '';
    let shade = '';
    WORDMARK.forEach((line, r) => {
      const ch = [...line];
      for (let c = 0; c < ch.length; c++) {
        const x = c * CW;
        const y = r * RH;
        const cx = x + CW / 2;
        const cy = y + RH / 2;
        if (ch[c] === '█') {
          let e = c;
          while (ch[e + 1] === '█') e++;
          ink += `M${x} ${y}h${(e - c + 1) * CW}v${RH}h${-(e - c + 1) * CW}z`;
          c = e;
        } else {
          shade += {
            '═': `M${x} ${cy}H${x + CW}`, '║': `M${cx} ${y}V${y + RH}`,
            '╗': `M${x} ${cy}H${cx}V${y + RH}`, '╔': `M${x + CW} ${cy}H${cx}V${y + RH}`,
            '╝': `M${x} ${cy}H${cx}V${y}`, '╚': `M${x + CW} ${cy}H${cx}V${y}`,
          }[ch[c]] || '';
        }
      }
    });
    const s = document.createElementNS(SVGNS, 'svg');
    s.setAttribute('viewBox', '0 0 246 60');
    s.setAttribute('aria-hidden', 'true');
    for (const [cls, d] of [['wm-shade', shade], ['wm-ink', ink]]) {
      const p = document.createElementNS(SVGNS, 'path');
      p.setAttribute('class', cls);
      p.setAttribute('d', d);
      s.append(p);
    }
    return s;
  }

  function load(key, dflt) {
    try {
      const v = JSON.parse(localStorage.getItem(key));
      return v == null ? dflt : v;
    } catch {
      return dflt;
    }
  }

  function save(key, v) {
    try {
      if (v == null) localStorage.removeItem(key);
      else localStorage.setItem(key, JSON.stringify(v));
    } catch {
      // storage full or blocked: the choice lasts this page
    }
  }

  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const plural = (n, one, many) => `${n} ${n === 1 ? one : many || one + 's'}`;

  // Phones and tablets: no keyboard to keep focus for, and focusing a text
  // field opens the on-screen keyboard.
  const touch = () => matchMedia('(pointer: coarse)').matches;
  // The layouts: phones stack the screens, tablets have a drawer.
  const small = () => matchMedia('(max-width: 639px)').matches;
  const narrow = () => matchMedia('(max-width: 1023px)').matches;
  // Keyboard hints only where there is a keyboard.
  const keyboard = () => !touch() && !small();
  const MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  const MOD = MAC ? '⌘' : 'Ctrl';
  // viewHeight is the height the on-screen keyboard leaves.
  const viewHeight = () => (window.visualViewport ? visualViewport.height : innerHeight);
  const IOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

  // rel is how long ago ms was, short: now, 4m, 3h, Tue, Oct 12.
  function rel(ms) {
    if (!ms) return '';
    const s = Math.round((Date.now() - ms) / 1000);
    if (s < 45) return 'now';
    const m = Math.round(s / 60);
    if (m < 60) return m + 'm';
    const hr = Math.round(m / 60);
    if (hr < 24) return hr + 'h';
    const d = new Date(ms);
    if (Date.now() - ms < 6 * DAY) return d.toLocaleDateString([], { weekday: 'short' });
    return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  }

  // ago is rel in words: just now, 4 min ago, 3 h ago, Tue, Oct 12.
  function ago(ms) {
    const r = rel(ms);
    if (r === 'now') return 'just now';
    if (/^\d+m$/.test(r)) return r.slice(0, -1) + ' min ago';
    if (/^\d+h$/.test(r)) return r.slice(0, -1) + ' h ago';
    return r;
  }

  // relTime is an element whose text (rel or ago) the ticker keeps fresh.
  function relTime(ms, cls, long) {
    const el = h('time', cls || '');
    el.dataset.ts = String(ms || 0);
    if (long) el.dataset.long = '1';
    if (ms) {
      el.dateTime = new Date(ms).toISOString();
      el.title = new Date(ms).toLocaleString();
    }
    el.textContent = long ? ago(ms) : rel(ms);
    return el;
  }

  // dur is a duration: 42s, 3m 12s, 2h 5m.
  function dur(ms) {
    let s = Math.max(0, Math.floor(ms / 1000));
    const d = Math.floor(s / 86400);
    s -= d * 86400;
    const hr = Math.floor(s / 3600);
    s -= hr * 3600;
    const m = Math.floor(s / 60);
    s -= m * 60;
    if (d) return `${d}d ${hr}h`;
    if (hr) return `${hr}h ${m}m`;
    if (m) return `${m}m ${s}s`;
    return `${s}s`;
  }

  // elapsed shows how long from start to end; without end it keeps
  // counting (the ticker updates [data-since]).
  function elapsed(start, end, cls) {
    const el = h('span', cls || '', start ? dur((end || Date.now()) - start) : '–');
    if (start && !end) el.dataset.since = String(start);
    return el;
  }

  // clock is ms as a time of day, with the weekday when it is not today.
  function clock(ms) {
    const d = new Date(ms);
    const t = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return d.toDateString() === new Date().toDateString() ? t : d.toLocaleDateString([], { weekday: 'short' }) + ' ' + t;
  }

  // upFor is a long time, roughly: 17d, 5h, 12m.
  function upFor(ms) {
    const m = Math.max(0, Math.floor(ms / 60000));
    if (m >= 2880) return Math.floor(m / 1440) + 'd';
    if (m >= 60) return Math.floor(m / 60) + 'h';
    return m + 'm';
  }

  // fmtIEC is a size in binary units: 512 B, 3.4 KiB, 31 GiB.
  function fmtIEC(n) {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) {
      n /= 1024;
      i++;
    }
    return i ? `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}` : `${n} B`;
  }

  const fmtSize = (n) => (n < 1024 ? `${n} B`
    : n < 1 << 20 ? `${(n / 1024).toFixed(n < 10 << 10 ? 1 : 0)} KB`
      : `${(n / (1 << 20)).toFixed(n < 10 << 20 ? 1 : 0)} MB`);
  const fmtBytes = (n) => (n < 1024 ? `${n} bytes` : `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`);

  // fmtTokens shortens a token count: 812, 9.4k, 748k, 1.2M.
  function fmtTokens(n) {
    n = n || 0;
    if (n < 1000) return String(n);
    if (n < 1e6) return (n / 1000).toFixed(n < 1e4 ? 1 : 0) + 'k';
    return (n / 1e6).toFixed(1) + 'M';
  }

  function shortPath(p) {
    if (!p) return '';
    return p.replace(/^\/(home|Users)\/[^/]+/, '~');
  }

  const joinPath = (dir, name) => (dir.endsWith('/') ? dir : dir + '/') + name;
  const baseName = (p) => p.split('/').filter(Boolean).pop() || '';

  function hostPort(addr, port) {
    const a = addr.includes(':') ? `[${addr}]` : addr;
    return port ? `${a}:${port}` : a;
  }

  // ---------------------------------------------------------- agent states

  const STATES = {
    needs_input: { label: 'Needs input', rank: 0 },
    working: { label: 'Working', rank: 1 },
    running: { label: 'Running', rank: 2 },
    starting: { label: 'Starting', rank: 2 },
    idle: { label: 'Idle', rank: 3 },
    unspecified: { label: 'Unknown', rank: 4 },
    failed: { label: 'Failed', rank: 9 },
    exited: { label: 'Ended', rank: 9 },
  };
  const isFinished = (a) => a.state === 'exited' || a.state === 'failed';
  // stateKey is a's state for its badge and pill, a usage limit counting
  // as one.
  const stateKey = (a) => (a.usageLimit && !isFinished(a) ? 'limit' : STATES[a.state] ? a.state : 'unspecified');
  const KEY_LABEL = { ...Object.fromEntries(Object.entries(STATES).map(([k, v]) => [k, v.label])), limit: 'Usage limit' };

  // label is what a session is called: the conversation's title, else the
  // agent's name.
  const label = (a) => a.title || a.name || a.id;

  // agentKey identifies an agent across machines.
  const agentKey = (host, id) => (host ? host + '/' + id : id);
  const splitKey = (key) => {
    const i = key.lastIndexOf('/');
    return i < 0 ? { host: '', id: key } : { host: key.slice(0, i), id: key.slice(i + 1) };
  };

  // allAgents lists this daemon's agents and those of other fleets.
  function allAgents() {
    const out = [...S.agents.values()].map((a) => ({ a, host: '' }));
    for (const [host, r] of S.remote) for (const a of r.agents.values()) out.push({ a, host });
    return out;
  }

  const agentsOf = (host) => (host ? (S.remote.get(host) || { agents: new Map() }).agents : S.agents);
  const findAgent = (host, id) => agentsOf(host).get(id) || null;

  // putAgent stores an agent the API returned, here or on another fleet.
  function putAgent(host, a) {
    if (!a || !a.id) return;
    if (host) {
      const rem = S.remote.get(host) || { agents: new Map(), fails: 0 };
      rem.agents.set(a.id, a);
      S.remote.set(host, rem);
    } else S.agents.set(a.id, a);
    invalidate('agents');
    chatAgent(host, a);
  }

  // limitNext says what happens next to an agent a usage limit l stopped.
  function limitNext(l) {
    if (l.detail) return l.detail;
    if (l.resumeAtMs) return 'resumes ' + clock(l.resumeAtMs);
    if (l.resetsAtMs) return 'resets ' + clock(l.resetsAtMs) + ', auto-resume off';
    return 'auto-resume off';
  }

  // Pull requests agents created (Agent.pullRequests).
  const prOpen = (p) => p.state !== 'merged' && p.state !== 'closed';

  // prStatus is a pull request's state in a word, and its chip's class.
  function prStatus(p) {
    if (p.state === 'merged') return ['merged', 'merged'];
    if (p.state === 'closed') return ['closed', 'bad'];
    if (p.state !== 'open') return [p.detail ? 'PR' : 'checking', ''];
    if (p.draft) return ['draft', ''];
    if (p.mergeState === 'dirty') return ['conflicts', 'bad'];
    if (p.checks === 'failing') return ['checks fail', 'bad'];
    if (p.checks === 'pending') return ['checks running', 'warn'];
    if (p.mergeState === 'blocked') return ['blocked', 'warn'];
    if (p.mergeState === 'behind') return ['behind', 'warn'];
    return ['ready', 'ok'];
  }

  function prTitle(p) {
    const parts = [`${p.repo}#${p.number}`];
    if (p.title) parts.push(p.title);
    if (p.headBranch) parts.push(`${p.headBranch} → ${p.baseBranch}`);
    if (p.changedFiles || p.additions || p.deletions) parts.push(`+${p.additions} −${p.deletions} · ${plural(p.changedFiles, 'file')}`);
    if (p.checks && p.checks !== 'unspecified') parts.push('checks ' + p.checks);
    if (p.reviewDecision) parts.push(p.reviewDecision.replace('_', ' '));
    if (p.detail) parts.push(p.detail);
    return parts.join('\n');
  }

  function prChip(p) {
    const [word, cls] = prStatus(p);
    return h('span', { class: 'chip ' + cls, title: prTitle(p) }, icon(p.state === 'merged' ? 'merge' : 'pr'), `#${p.number} ${word}`);
  }

  // gitUnpushed counts the commits the remote lacks: ahead of the upstream,
  // or of the base for a branch never pushed.
  const gitUnpushed = (g) => (g.upstream ? g.ahead : g.baseAhead);

  function gitTitle(g) {
    const parts = [(g.branch || 'detached HEAD') + (g.upstream ? ' → ' + g.upstream : ' (not pushed)')];
    if (g.upstream && (g.ahead || g.behind)) parts.push(`${g.ahead} to push, ${g.behind} to pull`);
    if (g.base && (g.baseAhead || g.baseBehind)) parts.push(`${g.base}: ${g.baseAhead} ahead, ${g.baseBehind} behind`);
    if (g.changed) parts.push(`${g.changed} uncommitted`);
    if (g.operation) parts.push(g.operation + ' in progress');
    if (g.detail) parts.push(g.detail);
    return parts.join('\n');
  }

  // -------------------------------------------------------------- unread
  //
  // A session whose turn ended while it was not shown here is unread until
  // it is opened. The page keeps the update time it last showed of each
  // session (fleet.seen); sessions it never met count as seen, unless they
  // started after the page loaded.

  const LOADED = Date.now();
  let seen = load('fleet.seen', null);
  let seenTimer = 0;

  function saveSeen() {
    clearTimeout(seenTimer);
    seenTimer = setTimeout(() => save('fleet.seen', seen), 500);
  }

  function markSeen(host, a) {
    if (!seen || !a) return;
    const k = agentKey(host, a.id);
    if (seen[k] !== a.updatedAtMs) {
      seen[k] = a.updatedAtMs || Date.now();
      saveSeen();
    }
  }

  // meet records the agents the page has not met yet, and drops the ones
  // gone (keeping the latest 500).
  function meet() {
    const all = allAgents();
    if (!seen) seen = {};
    const known = new Set();
    for (const { a, host } of all) {
      const k = agentKey(host, a.id);
      known.add(k);
      if (!(k in seen)) seen[k] = (a.createdAtMs || 0) > LOADED - 5000 ? a.createdAtMs : a.updatedAtMs || 0;
    }
    const keys = Object.keys(seen);
    if (keys.length > 500) for (const k of keys) if (!known.has(k)) delete seen[k];
    saveSeen();
  }

  function isUnread(host, a) {
    if (!seen || isFinished(a) || a.state !== 'idle') return false;
    const k = agentKey(host, a.id);
    if (U.view === 'chat' && U.key === k) return false;
    const s = seen[k];
    return s !== undefined && (a.updatedAtMs || 0) > s + 1500;
  }

  // ---------------------------------------------------------------- theme

  const THEME_COLORS = { light: '#f4f6f5', dark: '#111213' }; // --bg-side
  const SCREEN_COLORS = { light: '#ffffff', dark: '#18191a' }; // --bg
  const theme = () => document.documentElement.dataset.theme || 'system';

  function setTheme(t) {
    document.documentElement.dataset.theme = t;
    try {
      localStorage.setItem('fleet.theme', t);
    } catch {}
    paintThemeColor();
  }

  // paintThemeColor colours the browser's bars as the page under them: the
  // sidebar, or on phones the screen over it.
  function paintThemeColor() {
    const t = theme();
    const c = small() && $('app').dataset.screen === 'main' ? SCREEN_COLORS : THEME_COLORS;
    document.querySelectorAll('meta[name="theme-color"]').forEach((m, i) => {
      const want = t === 'system' ? (i === 0 ? c.light : c.dark) : c[t];
      if (m.content !== want) m.content = want;
    });
  }

  function themeSeg() {
    const seg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Theme' });
    const opts = [['system', 'System', 'monitor'], ['light', 'Light', 'sun'], ['dark', 'Dark', 'moon']];
    for (const [v, text, ic] of opts) {
      seg.append(h('button', {
        type: 'button', 'aria-pressed': String(theme() === v),
        onclick: () => {
          setTheme(v);
          seg.querySelectorAll('button').forEach((b, i) => b.setAttribute('aria-pressed', String(opts[i][0] === v)));
        },
      }, icon(ic), text));
    }
    return seg;
  }

  // --------------------------------------------------------------- routes
  //
  // The address names the view: #/ (phones: the session list; desktops: a
  // new session), #/new, #/devices and #/s/<agent key>. A `fleet web`
  // link's #token=... is taken before (takeLinkToken).

  function parseRoute() {
    const m = /^#\/(s|new|devices)(?:\/(.+))?$/.exec(location.hash);
    if (!m) return { view: 'home', key: '' };
    if (m[1] === 's') {
      let key = '';
      try {
        key = decodeURIComponent(m[2] || '');
      } catch {}
      return key ? { view: 'chat', key } : { view: 'home', key: '' };
    }
    return { view: m[1], key: '' };
  }

  function go(hash) {
    if (location.hash === hash) route();
    else location.hash = hash;
  }

  const chatHash = (host, id) => '#/s/' + encodeURIComponent(agentKey(host, id));

  function route() {
    const r = parseRoute();
    const changed = r.view !== U.view || r.key !== U.key;
    const wasChat = U.view === 'chat';
    U.view = r.view;
    U.key = r.key;
    if (changed) {
      closeMenu(false);
      U.drawer = false;
      if (wasChat && r.view !== 'chat') leaveChat();
    }
    $('app').dataset.screen = U.view === 'home' ? 'list' : 'main';
    paintThemeColor();
    applyShell();
    renderSideList();
    renderSideFoot();
    renderMain();
    updateTitle();
    if (changed && U.view !== 'home' && small()) $('main').focus({ preventScroll: true });
  }

  // The title counts the sessions that wait for an answer. It is set
  // outside requestAnimationFrame, which browsers pause in background tabs:
  // exactly when the count matters.
  function updateTitle() {
    let n = 0;
    for (const { a } of allAgents()) if (a.state === 'needs_input') n++;
    const base = S.server && S.server.name ? `fleet · ${S.server.name}` : 'fleet';
    const t = n ? `(${n}) ${base}` : base;
    if (document.title !== t) document.title = t;
    setBadge(n);
  }

  function renderMain() {
    const main = $('main');
    if (U.view === 'chat') {
      if (!admin) return lockedView(main);
      const { host, id } = splitKey(U.key);
      return showChat(host, id);
    }
    if (U.view === 'devices') return renderDevices();
    // Phones: the list is the screen; whatever main showed slides away.
    if (U.view === 'home' && small()) return;
    if (!admin) return lockedView(main);
    renderNew();
  }

  // applyShell sets the sidebar's state: hidden (desktops) or a drawer
  // (tablets).
  function applyShell() {
    const app = $('app');
    app.classList.toggle('side-off', U.sideOff && !narrow());
    app.classList.toggle('drawer', U.drawer && narrow() && !small());
    app.classList.toggle('offline', !S.online && !!S.connShown);
    $('side-scrim').hidden = !(U.drawer && narrow() && !small());
  }

  function toggleSide(open) {
    if (narrow()) U.drawer = open;
    else U.sideOff = !open;
    applyShell();
    // The views put a button to bring it back in their bars.
    if (U.view === 'chat') renderChatHead();
    else if (U.view !== 'home' || !small()) renderMain();
    if (open && narrow() && !touch()) setTimeout(() => $('side-q')?.focus(), 60);
  }

  // leadBtn brings the sessions back: a back button on phones, the drawer
  // on tablets, the hidden sidebar on desktops; null when it shows.
  function leadBtn() {
    if (small()) return iconBtn('chevron-left', 'Back to the sessions', () => go('#/'), { cls: 'back-btn' });
    if (narrow()) return iconBtn('panel-left', 'Show the sessions', () => toggleSide(true));
    if (U.sideOff) return iconBtn('panel-left', 'Show the sessions', () => toggleSide(true));
    return null;
  }

  function lockedView(main) {
    const lead = leadBtn();
    const where = (S.server && (S.server.hostname || S.server.name)) || 'the server';
    fill(main, h('div', 'view',
      lead ? h('div', 'topbar', lead, h('h1', null, 'fleet')) : null,
      h('div', 'no-chat',
        h('span', 'big-ic', icon('lock', 'i-lg')),
        h('h2', null, 'This browser can only look'),
        h('p', null, 'Unlock it with the fleet key to open chats, answer agents and start sessions, or open the link ', h('code', 'mono', 'fleet web'), ` prints on ${where}.`),
        h('button', { type: 'button', class: 'btn primary', onclick: openUnlock }, icon('key'), 'Unlock'))));
  }

  // ------------------------------------------------------------- machines
  //
  // The fleet's machines: this daemon, then the others on the network.
  // Those with a dashboard are reached through /api/hosts/<id>/, signed
  // with the fleet key; a machine without it is "not joined".

  const hostStates = new Map(); // server id -> 'checking' | 'joined' | 'not_joined' | 'unreachable'
  const hostState = (id) => (id ? hostStates.get(id) || 'checking' : 'self');
  const hostUp = (st) => st === 'self' || st === 'joined';

  // pickerHosts: this daemon, then the fleets on the network with a dashboard.
  function pickerHosts() {
    const self = S.server || {};
    const out = [{ id: '', name: self.name || self.hostname || 'this machine' }];
    for (const p of S.peers) {
      if (!p.self && p.id && p.webPort && (p.addrs || []).length) out.push({ id: p.id, name: p.name || p.host || p.id.slice(0, 8) });
    }
    return out;
  }

  const hostName = (id) => (pickerHosts().find((x) => x.id === id) || { name: 'that machine' }).name;

  // machines lists every machine with its state: self, joined,
  // not_joined, unreachable, checking, or nodash (seen, but without a
  // dashboard to manage it through).
  function machines() {
    const out = [];
    for (const p of [...S.peers].sort((x, y) => (y.self ? 1 : 0) - (x.self ? 1 : 0) || (x.name || '').localeCompare(y.name || ''))) {
      const reach = p.id && p.webPort && (p.addrs || []).length;
      out.push({ id: p.self ? '' : p.id || '', peer: p, name: p.name || p.host || (p.id || '').slice(0, 8), state: p.self ? 'self' : reach ? hostState(p.id) : 'nodash' });
    }
    if (!out.some((m) => m.state === 'self')) {
      const s = S.server || {};
      out.unshift({ id: '', peer: null, name: s.name || s.hostname || 'this machine', state: 'self' });
    }
    return out;
  }

  const MACHINE_STATE = {
    self: ['dot', 'online, this machine'],
    joined: ['dot', 'online'],
    checking: ['dot off', 'checking…'],
    not_joined: ['dot warn', 'not in this fleet yet'],
    unreachable: ['dot bad', 'unreachable'],
    nodash: ['dot off', 'on the network, without a dashboard'],
  };

  function setHostState(id, state) {
    if (!id) return;
    const changed = hostStates.get(id) !== state;
    hostStates.set(id, state);
    if (!changed) return;
    renderSideFoot();
    if (U.view === 'devices') renderDevices();
    if (U.view === 'new' || U.view === 'home') renderLaunch();
  }

  // checkHosts asks every other fleet whether it holds this fleet's key.
  function checkHosts() {
    for (const x of pickerHosts()) {
      if (!x.id || hostStates.get(x.id) === 'joined') continue;
      if (!hostStates.has(x.id)) hostStates.set(x.id, 'checking');
      checkHost(x.id);
    }
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

  // -------------------------------------------------------------- sidebar

  let sideBuilt = false;

  function buildSide() {
    if (sideBuilt) return;
    sideBuilt = true;
    $('side').append(sideHead(), sideTools(),
      h('div', { class: 'side-scroll', id: 'side-list' }),
      h('div', { class: 'side-foot not-phone', id: 'side-foot' }),
      newBar());
  }

  function sideHead() {
    return h('div', 'side-head',
      h('a', { class: 'brand', href: '#/', 'aria-label': 'fleet, home' }, wordmark()),
      h('span', 'spacer'),
      h('span', { class: 'on-phone', id: 'side-dots' }),
      iconBtn('settings', 'Settings', openSettings, { cls: 'on-phone' }),
      iconBtn('panel-left', 'Hide the sessions', () => toggleSide(false), { cls: 'not-phone' }));
  }

  function sideTools() {
    const setQ = (v) => {
      q.value = v;
      U.q = v;
      clear.hidden = !v;
      renderSideList();
    };
    const q = h('input', {
      id: 'side-q', type: 'search', placeholder: 'Search sessions', 'aria-label': 'Search sessions', autocomplete: 'off', spellcheck: 'false',
      oninput: (ev) => setQ(ev.target.value),
      onkeydown: (ev) => {
        if (ev.key === 'Escape' && U.q) {
          ev.stopPropagation();
          setQ('');
        }
      },
    });
    const clear = h('button', { type: 'button', class: 'icon-btn q-clear', 'aria-label': 'Clear the search', title: 'Clear the search', hidden: true, onclick: () => {
      setQ('');
      q.focus();
    } }, icon('x', 'i-sm'));
    return h('div', 'side-tools',
      h('a', { id: 'new-btn', class: 'new-btn not-phone', href: '#/new', title: 'Start a session (N)' }, icon('plus'), 'New session', h('kbd', null, 'N')),
      h('div', 'search', icon('search', 'i-sm'), q, clear,
        h('kbd', { class: 'not-phone', 'aria-hidden': 'true' }, '/'),
        h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Group sessions', title: 'Group sessions', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => groupMenu(ev.currentTarget) }, icon('sliders', 'i-sm'))));
  }

  function groupMenu(anchor) {
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Group sessions by'));
      for (const [v, text, ic] of [['folder', 'Folder', 'folder'], ['status', 'Status', 'alert'], ['machine', 'Machine', 'server'], ['none', 'Nothing, newest first', 'clock']]) {
        el.append(mi({ icon: ic, label: text, checked: U.groupBy === v, onSelect: () => {
          U.groupBy = v;
          save('fleet.groupBy', v);
          close();
          renderSideList();
        } }));
      }
    }, { label: 'Group sessions' });
  }

  function newBar() {
    return h('div', 'newbar on-phone',
      h('button', { type: 'button', onclick: () => go('#/new') }, icon('plus'), 'New session', h('span', { class: 'send', 'aria-hidden': 'true' }, icon('arrow-up'))));
  }

  const folderOf = (a) => a.path || a.cwd || '';

  function matches({ a, host }, words) {
    if (!words.length) return true;
    const hay = [a.title, a.name, a.id, folderOf(a), a.root, a.note, a.branch, a.adapter, host ? hostName(host) : ''].join(' ').toLowerCase();
    return words.every((w) => hay.includes(w));
  }

  // groupsOf puts the sessions into the sidebar's groups, the groups and
  // their sessions the most recent first (by status: in the order below).
  function groupsOf(list) {
    const recent = (x, y) => (y.a.updatedAtMs || 0) - (x.a.updatedAtMs || 0);
    if (U.groupBy === 'none') return [{ key: 'all', label: 'Recent', items: list.sort(recent) }];
    const map = new Map();
    const put = (key, text, x, extra) => {
      if (!map.has(key)) map.set(key, { key, label: text, items: [], ...extra });
      map.get(key).items.push(x);
    };
    for (const x of list) {
      if (U.groupBy === 'status') {
        const k = stateKey(x.a);
        const g = k === 'needs_input' ? ['1', 'Needs input'] : k === 'working' || k === 'starting' ? ['2', 'Working'] : k === 'limit' ? ['3', 'Usage limit'] : ['4', 'Ready'];
        put('st:' + g[0], g[1], x);
      } else if (U.groupBy === 'machine') {
        put('m:' + x.host, x.host ? hostName(x.host) : (S.server && S.server.name) || 'this machine', x, { machine: x.host });
      } else {
        const f = folderOf(x.a);
        put('f:' + x.host + ':' + f, baseName(f) || f || 'no folder', x, { folder: f, host: x.host });
      }
    }
    const groups = [...map.values()];
    for (const g of groups) g.items.sort(recent);
    if (U.groupBy === 'status') groups.sort((x, y) => x.key.localeCompare(y.key));
    else groups.sort((x, y) => (y.items[0].a.updatedAtMs || 0) - (x.items[0].a.updatedAtMs || 0));
    return groups;
  }

  const HISTORY_MAX = 100;

  function renderSideList() {
    const box = $('side-list');
    if (!box) return;
    if (swipe.drag) {
      swipe.stale = true;
      return;
    }
    const top = box.scrollTop;
    const words = U.q.toLowerCase().split(/\s+/).filter(Boolean);
    const every = allAgents().filter((x) => matches(x, words));
    const live = every.filter((x) => !isFinished(x.a));
    const done = every.filter((x) => isFinished(x.a)).sort((x, y) => (y.a.updatedAtMs || 0) - (x.a.updatedAtMs || 0));
    const kids = [];

    const waiting = live.filter((x) => x.a.state === 'needs_input');
    if (waiting.length && !words.length) {
      kids.push(h('section', { class: 'attn', 'aria-label': 'Sessions that need you' },
        h('div', 'attn-h', icon('alert'), waiting.length === 1 ? 'Needs you' : `${waiting.length} sessions need you`),
        waiting.map(({ a, host }) => h('a', { class: 'attn-item', href: chatHash(host, a.id) },
          avatar(a),
          h('div', 'row-main', h('div', 'row-name', label(a)), h('div', 'attn-q', a.stateDetail || 'Waiting for you in its terminal')),
          h('span', 'attn-go', 'Answer')))));
    }

    if (!S.snapshotDone && !S.agents.size) kids.push(h('p', 'side-empty', 'Loading…'));
    else if (!live.length && !done.length) {
      kids.push(h('p', 'side-empty', words.length ? `No session matches “${U.q}”.` : admin ? 'No sessions yet. Start one with New session.' : 'No sessions yet.'));
    }

    for (const g of groupsOf(live)) {
      const open = !U.collapsed.has(g.key) || !!words.length;
      const toggle = h('button', { type: 'button', class: 'grp-toggle', 'aria-expanded': String(open), onclick: () => {
        if (open) U.collapsed.add(g.key);
        else U.collapsed.delete(g.key);
        save('fleet.collapsed', [...U.collapsed]);
        renderSideList();
      } }, icon('chevron-down'), h('span', 'trunc', g.label), g.items.length > 1 ? h('span', 'grp-count', String(g.items.length)) : null);
      toggle.title = g.folder ? shortPath(g.folder) : '';
      const where = g.host && U.groupBy === 'folder' ? h('span', 'grp-where', hostName(g.host)) : null;
      const addBtn = admin && g.folder ? h('button', { type: 'button', class: 'icon-btn grp-add', 'aria-label': `New session in ${g.label}`, title: `New session in ${shortPath(g.folder)}`, onclick: () => newIn(g.host, g.folder) }, icon('plus')) : null;
      kids.push(h('section', { class: 'grp', 'aria-label': g.label },
        h('div', 'grp-h', toggle, where, addBtn),
        open ? h('ul', 'rows', g.items.map(sessionRow)) : null));
    }

    if (done.length) {
      const open = U.archivedOpen || !!words.length;
      const toggle = h('button', { type: 'button', class: 'grp-toggle', 'aria-expanded': String(open), onclick: () => {
        U.archivedOpen = !U.archivedOpen;
        renderSideList();
      } }, icon('chevron-down'), 'Archived', h('span', 'grp-count', String(done.length)));
      kids.push(h('section', { class: 'grp archived', 'aria-label': 'Archived' }, h('div', 'grp-h', toggle),
        open ? [h('ul', 'rows', done.slice(0, HISTORY_MAX).map(sessionRow)),
          done.length > HISTORY_MAX ? h('p', 'more-row', `${done.length - HISTORY_MAX} older ones: search finds them`) : null] : null));
    }

    fill(box, kids);
    box.scrollTop = top;
    $('new-btn')?.setAttribute('aria-current', U.view === 'new' || (U.view === 'home' && !small()) ? 'page' : 'false');
  }

  function avatar(a) {
    const k = stateKey(a);
    const badge = ['working', 'starting', 'needs_input', 'failed', 'limit'].includes(k) ? h('span', 'badge ' + k) : null;
    return h('span', { class: 'av', 'aria-hidden': 'true' }, logo(a.adapter), badge);
  }

  // subLine says in a few words what is going on in session a of host.
  function subLine(a, host) {
    const k = stateKey(a);
    const out = [];
    if (host && U.groupBy !== 'machine') out.push(h('span', 'host', hostName(host) + ' ·'));
    const runs = (admin && S.workflows.get(agentKey(host, a.id))) || [];
    const run = runs[runs.length - 1];
    if (k === 'needs_input') out.push(h('span', 't-needs_input', 'Needs input'), a.stateDetail ? h('span', null, '· ' + a.stateDetail) : null);
    else if (k === 'working') out.push(h('span', 't-working', a.stateDetail || 'Working'));
    else if (k === 'starting') out.push(h('span', 't-starting', 'Starting…'));
    else if (k === 'limit') out.push(h('span', 't-limit', 'Usage limit'), h('span', null, '· ' + limitNext(a.usageLimit)));
    else if (k === 'failed') out.push(h('span', 't-failed', 'Failed'), h('span', null, a.exitCode != null ? `· exit ${a.exitCode}` : a.stateDetail ? '· ' + a.stateDetail : ''));
    else if (run) {
      const n = run.bars.length;
      const fin = run.counts.done + run.counts.failed + run.counts.stopped;
      out.push(h('span', 't-wf', run.name), h('span', null, `· ${run.phase ? run.phase + ' ' : ''}${fin}/${n}`), miniBar(run.bars));
    } else {
      const bits = [a.title ? a.name : '', a.branch ? '⎇ ' + a.branch : '', U.groupBy !== 'folder' ? baseName(folderOf(a)) : ''].filter(Boolean);
      if (k === 'exited') bits.unshift(a.exitCode ? `Ended · exit ${a.exitCode}` : 'Ended');
      out.push(h('span', null, bits.join(' · ') || shortPath(folderOf(a))));
    }
    if (!isFinished(a) && a.queue && a.queue.length) out.push(h('span', 't-queue', `· ${a.queue.length} queued${a.queuePaused ? ', paused' : ''}`));
    return out;
  }

  function miniBar(bars) {
    return h('span', { class: 'mini-bar', 'aria-hidden': 'true' }, bars.slice(0, 12).map((s) => h('span', s === 'running' ? 'run' : 'done')));
  }

  function rowChips(a) {
    const prs = a.pullRequests || [];
    const open = prs.filter(prOpen);
    const shown = (open.length ? open : prs.slice(-1)).slice(-2);
    return shown.length ? h('div', 'row-chips', shown.map(prChip)) : null;
  }

  function sessionRow({ a, host }) {
    const key = agentKey(host, a.id);
    const cur = U.view === 'chat' && U.key === key;
    const done = isFinished(a);
    const unread = isUnread(host, a);
    const k = stateKey(a);
    const row = h('a', {
      class: 'row' + (unread ? ' unread' : '') + (done ? ' done' : ''),
      href: chatHash(host, a.id),
      'aria-current': cur ? 'page' : null,
      'data-key': key,
      title: a.title ? a.name : null,
    },
    avatar(a),
    h('div', 'row-main',
      h('div', 'row-top', h('span', 'row-name', label(a)), relTime(a.updatedAtMs, 'row-time')),
      h('div', 'row-sub', subLine(a, host), unread ? h('span', 'unread-dot') : null),
      a.note ? h('div', 'row-note', icon('note'), h('span', null, a.note)) : null,
      rowChips(a),
      h('span', 'sr-only', [k === 'working' ? 'working' : '', unread ? 'new reply' : ''].filter(Boolean).join(', '))));
    const li = h('li', 'row-wrap', row);
    if (admin) {
      const act = endAction(a);
      li.prepend(h('button', { type: 'button', class: 'swipe-act ' + (done ? 'delete' : 'archive'), tabindex: '-1', 'aria-hidden': 'true', onclick: () => endSwiped(host, a) },
        h('span', 'swipe-in', icon(done ? 'trash' : 'archive'), act.label)));
      li._end = () => endSwiped(host, a);
      li.append(h('div', 'row-acts',
        iconBtn('note', a.note ? 'Edit the note' : 'Add a note', (ev) => noteEditor(ev.currentTarget, host, a)),
        iconBtn(done ? 'trash' : 'archive', `${act.label} ${label(a)}`, () => confirmEnd(host, a))));
    }
    return li;
  }

  function renderSideFoot() {
    const ms = machines();
    const up = ms.filter((m) => hostUp(m.state)).length;
    const text = `Devices: ${up} of ${ms.length} machine${ms.length === 1 ? '' : 's'} online`;
    const foot = $('side-foot');
    if (foot) {
      fill(foot,
        h('a', { class: 'devstrip', href: '#/devices', 'aria-current': U.view === 'devices' ? 'page' : null, 'aria-label': text, title: 'Devices' },
          ms.map((m) => h('span', { class: 'dev' + (hostUp(m.state) ? '' : ' off'), title: `${m.name}: ${MACHINE_STATE[m.state][1]}` }, h('span', MACHINE_STATE[m.state][0]), m.name))),
        iconBtn('settings', 'Settings', openSettings));
    }
    const dots = $('side-dots');
    if (dots) {
      fill(dots, h('a', { class: 'devdots', href: '#/devices', 'aria-label': text },
        ms.slice(0, 5).map((m) => h('span', MACHINE_STATE[m.state][0])), h('span', 'n', `${up}/${ms.length}`)));
    }
  }

  // newIn starts a new session in folder of machine host.
  function newIn(host, folder) {
    launchFor(host, folder);
    go('#/new');
  }

  // ------------------------------------------------------------ row swipes
  //
  // On phones and tablets a session row swiped to the left uncovers its end action
  // (Archive, or Delete for a finished one), done by a tap. Swiped on past
  // FULL of its width, letting go asks right away. A quick flick opens or
  // closes it whatever the distance. One row is open at a time; a tap
  // elsewhere or a scroll closes it. The list is not rendered again while a
  // finger drags a row.

  const SWIPE_W = 84;
  const FULL = 0.55;
  const swipe = { open: null, drag: false, stale: false };

  // setSwipe moves row to dx (<= 0), its action label along with it.
  function setSwipe(row, dx, full) {
    row.style.translate = dx ? dx + 'px 0' : '';
    const wrap = row.parentElement;
    wrap?.style.setProperty('--reveal', Math.max(0, -dx) + 'px');
    wrap?.querySelector('.swipe-act')?.classList.toggle('full', !!full);
  }

  function closeSwipe(row) {
    if (!row) return;
    setSwipe(row, 0);
    setTimeout(() => {
      if (!row.style.translate) row.parentElement?.classList.remove('swiping');
    }, 340);
  }

  // endSwiped closes the open row and asks to end session a of host.
  function endSwiped(host, a) {
    if (swipe.open) closeSwipe(swipe.open);
    swipe.open = null;
    confirmEnd(host, a);
  }

  // A link to a session pressed, or pointed at for a moment, fetches its
  // conversation (warmFeed): it shows at once when it opens.
  function wireWarm() {
    const keyOf = (ev) => {
      const a = ev.target.closest && ev.target.closest('a[href^="#/s/"]');
      if (!a) return '';
      try {
        return decodeURIComponent(a.getAttribute('href').slice(4));
      } catch {
        return '';
      }
    };
    let timer = 0;
    document.addEventListener('pointerdown', (ev) => {
      const key = keyOf(ev);
      if (key) warmFeed(key);
    }, { passive: true });
    document.addEventListener('pointerover', (ev) => {
      clearTimeout(timer);
      const key = ev.pointerType === 'mouse' && keyOf(ev);
      if (key) timer = setTimeout(() => warmFeed(key), 120);
    }, { passive: true });
  }

  function wireSwipe() {
    const box = $('side-list');
    let g = null; // {row, id, x0, y0, base, dx, on, w, full, t, v}
    box.addEventListener('touchstart', (ev) => {
      if (g && g.on) return; // a second finger: the first one leads
      g = null;
      const row = ev.target.closest('.row');
      if (!admin || !touch() || !row || ev.touches.length !== 1) return;
      if (swipe.open && swipe.open !== row) {
        closeSwipe(swipe.open);
        swipe.open = null;
      }
      const t = ev.touches[0];
      g = { row, id: t.identifier, x0: t.clientX, y0: t.clientY, base: swipe.open === row ? -SWIPE_W : 0, dx: 0, on: false, w: row.offsetWidth, full: false, t: ev.timeStamp, v: 0 };
    }, { passive: true });
    box.addEventListener('touchmove', (ev) => {
      const t = g && [...ev.touches].find((x) => x.identifier === g.id);
      if (!t) return;
      const mx = t.clientX - g.x0;
      const my = t.clientY - g.y0;
      if (!g.on) {
        // The list scrolls already, or the finger goes up or down.
        if (!ev.cancelable || (Math.abs(my) > 10 && Math.abs(my) > Math.abs(mx))) return (g = null);
        if (Math.abs(mx) < 10 || Math.abs(mx) < 1.5 * Math.abs(my)) return;
        g.on = true;
        swipe.drag = true;
        g.row.parentElement.classList.add('swiping');
        g.row.style.transition = 'none';
        g.x0 = t.clientX - (mx > 0 ? 10 : -10); // no jump by the slop
      }
      ev.preventDefault(); // the list holds still while a row moves
      let dx = g.base + t.clientX - g.x0;
      // To the right of closed it gives a little, then stops.
      if (dx > 0) dx = Math.min(dx * 0.25, 16);
      dx = Math.max(dx, -g.w);
      const dt = ev.timeStamp - g.t;
      if (dt > 0) g.v = 0.6 * g.v + 0.4 * ((dx - g.dx) / dt);
      g.t = ev.timeStamp;
      g.dx = dx;
      const full = -dx > g.w * FULL;
      if (full !== g.full && navigator.vibrate) navigator.vibrate(8);
      g.full = full;
      setSwipe(g.row, dx, full);
    }, { passive: false });
    const end = (ev) => {
      if (ev.touches.length) return; // fingers still down
      const s = g;
      g = null;
      if (!s || !s.on) return;
      swipe.drag = false;
      s.row.style.transition = '';
      // A click follows the touch: it must not open the session.
      s.row.dataset.swiped = String(Date.now());
      let open = false;
      if (s.full && ev.type === 'touchend') {
        swipe.open = s.row;
        s.row.parentElement._end();
      } else {
        const flick = Math.abs(s.v) > 0.45;
        open = ev.type === 'touchend' && (flick ? s.v < 0 : s.dx < -SWIPE_W / 2);
        if (open) setSwipe(s.row, -SWIPE_W);
        else closeSwipe(s.row);
        swipe.open = open ? s.row : null;
      }
      // Changes that came in meanwhile, once the row is back.
      if (swipe.stale && !open) {
        swipe.stale = false;
        setTimeout(() => swipe.drag || swipe.open ? (swipe.stale = true) : renderSideList(), 340);
      }
    };
    box.addEventListener('touchend', end);
    box.addEventListener('touchcancel', end);
    box.addEventListener('click', (ev) => {
      const row = ev.target.closest('.row');
      if (!row) return;
      if (Date.now() - Number(row.dataset.swiped || 0) < 400 || swipe.open === row) {
        ev.preventDefault();
        if (swipe.open === row && Date.now() - Number(row.dataset.swiped || 0) >= 400) {
          closeSwipe(row);
          swipe.open = null;
        }
      }
    });
    box.addEventListener('scroll', () => {
      if (swipe.open && !swipe.drag) {
        closeSwipe(swipe.open);
        swipe.open = null;
      }
    }, { passive: true });
  }

  // ------------------------------------------------------- menus, sheets
  //
  // One menu at a time, placed by its anchor on desktops and a bottom sheet
  // on phones. Arrow keys move through its items, Esc closes it and puts
  // the focus back. Opened with the keyboard, the first item takes the
  // focus; opened with a pointer, the menu holds it until the arrows move.

  let MENU = null; // {el, anchor, scrim, repaint}
  let lastPointer = 0;

  function openMenu(anchor, build, o = {}) {
    if (MENU && MENU.anchor === anchor) {
      closeMenu();
      return;
    }
    closeMenu(false);
    const el = h('div', { class: 'menu' + (o.wide ? ' wide' : ''), role: o.role || 'menu', 'aria-label': o.label || null, tabindex: '-1' });
    const close = (restore = true) => closeMenu(restore);
    MENU = { el, anchor, scrim: null, repaint: null, o };
    build(el, close, MENU);
    const layer = $('layer');
    if (small()) {
      MENU.scrim = h('div', { class: 'scrim', onclick: () => close() });
      layer.append(MENU.scrim);
    }
    layer.append(el);
    anchor.setAttribute('aria-expanded', 'true');
    if (!small()) placeMenu();
    const byPointer = Date.now() - lastPointer < 600;
    const first = o.focus ? el.querySelector(o.focus)
      : byPointer ? null : el.querySelector('.mi:not([aria-disabled="true"])') || el.querySelector('input, textarea');
    (first || el).focus({ preventScroll: true });
  }

  // placeMenu puts the menu under its anchor, or over it when there is
  // more room above, inside the window.
  function placeMenu() {
    if (!MENU || small()) return;
    const { el, anchor, o } = MENU;
    const r = anchor.getBoundingClientRect();
    const m = el.getBoundingClientRect();
    let left = o.alignRight ? r.right - m.width : r.left;
    left = Math.max(8, Math.min(left, innerWidth - m.width - 8));
    const below = innerHeight - r.bottom;
    let top = below >= m.height + 12 || below >= r.top ? r.bottom + 6 : r.top - m.height - 6;
    top = Math.max(8, Math.min(top, innerHeight - m.height - 8));
    el.style.left = left + 'px';
    el.style.top = top + 'px';
  }

  function closeMenu(restore = true) {
    if (!MENU) return;
    const { el, anchor, scrim } = MENU;
    MENU = null;
    el.remove();
    if (scrim) scrim.remove();
    anchor.setAttribute('aria-expanded', 'false');
    if (restore && anchor.isConnected) anchor.focus({ preventScroll: true });
  }

  function wireMenus() {
    document.addEventListener('pointerdown', (ev) => {
      lastPointer = Date.now();
      if (MENU && !MENU.el.contains(ev.target) && !MENU.anchor.contains(ev.target) && ev.target !== MENU.scrim) closeMenu(false);
    }, true);
    document.addEventListener('keydown', (ev) => {
      if (!MENU) return;
      if (ev.key === 'Escape') {
        ev.preventDefault();
        ev.stopPropagation();
        closeMenu();
        return;
      }
      if (ev.key === 'Tab' && MENU.el.getAttribute('role') === 'menu') {
        closeMenu(false);
        return;
      }
      if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
      if (ev.target.matches('textarea')) return;
      const items = [...MENU.el.querySelectorAll('.mi:not([aria-disabled="true"])')];
      if (!items.length) return;
      ev.preventDefault();
      const i = items.indexOf(document.activeElement);
      (ev.key === 'ArrowDown' ? items[(i + 1) % items.length] : items[(i - 1 + items.length) % items.length]).focus();
    }, true);
    addEventListener('resize', () => {
      if (MENU && !small()) placeMenu();
    });
  }

  // mi is a menu item: o.icon, o.logo or o.custom first, o.label with
  // o.sub under it, o.right or o.kbd at the end; o.checked makes it one of
  // a choice.
  function mi(o) {
    const radio = o.checked !== undefined;
    const b = h('button', {
      type: 'button', class: 'mi' + (o.danger ? ' danger' : '') + (o.cls ? ' ' + o.cls : ''),
      role: radio ? 'menuitemradio' : 'menuitem', 'aria-checked': radio ? String(!!o.checked) : null,
      'aria-disabled': o.disabled ? 'true' : null, title: o.title || null,
    },
    o.icon ? icon(o.icon) : o.logo ? logo(o.logo) : o.custom || null,
    h('span', 'grow', h('span', null, o.label), o.sub ? h('span', 'sub', o.sub) : null),
    o.right ? h('span', 'right', o.right) : null,
    o.kbd ? h('kbd', null, o.kbd) : null,
    o.checked ? icon('check', 'check') : null);
    b.addEventListener('click', () => {
      if (!o.disabled) o.onSelect?.();
    });
    return b;
  }

  // ---------------------------------------------------------------- dialogs

  // Require both ends of the click to be outside the dialog's box, so
  // selecting text or clicking empty space inside never dismisses it.
  function wireBackdrop(dlg, close, canClose = () => true) {
    let downOutside = false;
    const outside = (ev) => {
      const r = dlg.getBoundingClientRect();
      return ev.target === dlg && (ev.clientX < r.left || ev.clientX > r.right || ev.clientY < r.top || ev.clientY > r.bottom);
    };
    dlg.addEventListener('pointerdown', (ev) => {
      downOutside = ev.button === 0 && outside(ev);
    });
    dlg.addEventListener('pointercancel', () => {
      downOutside = false;
    });
    dlg.addEventListener('close', () => {
      downOutside = false;
    });
    dlg.addEventListener('click', (ev) => {
      const dismiss = downOutside && outside(ev);
      downOutside = false;
      if (dismiss && canClose()) close();
    });
  }

  const dlgState = { onClose: null, canClose: () => true, back: null, id: '' };

  // openDialog shows the one dialog: a title, a line under it, a body and
  // the buttons at its foot. o.id names it (for repaints), o.focus is the
  // element to focus, o.canClose may keep it open (a request in flight).
  function openDialog(o) {
    const dlg = $('dlg');
    if (dlg.open) dlg.close();
    dlg.className = 'dlg' + (o.wide ? ' wide' : '');
    fill(dlg,
      h('div', 'dlg-h', h('div', 'grow', h('h2', { id: 'dlg-h' }, o.title), o.sub ? h('p', 'dlg-sub', o.sub) : null),
        iconBtn('x', 'Close', () => closeDialog())),
      o.body ? h('div', 'dlg-b', o.body) : null,
      o.foot ? h('div', 'dlg-f', o.foot) : null);
    Object.assign(dlgState, { onClose: o.onClose || null, canClose: o.canClose || (() => true), back: document.activeElement, id: o.id || '' });
    dlg.setAttribute('tabindex', '-1');
    dlg.showModal();
    if (o.focus) o.focus.focus();
    else if (Date.now() - lastPointer < 600 || touch()) dlg.focus();
  }

  function closeDialog() {
    const dlg = $('dlg');
    if (dlg.open && dlgState.canClose()) dlg.close();
  }

  function wireDialog() {
    const dlg = $('dlg');
    dlg.addEventListener('cancel', (ev) => {
      if (!dlgState.canClose()) ev.preventDefault();
    });
    dlg.addEventListener('close', () => {
      const { onClose, back } = dlgState;
      Object.assign(dlgState, { onClose: null, canClose: () => true, back: null, id: '' });
      if (onClose) onClose();
      if (back && back.isConnected) back.focus({ preventScroll: true });
    });
    wireBackdrop(dlg, closeDialog, () => dlgState.canClose());
  }

  const dialogShows = (id) => $('dlg').open && dlgState.id === id;

  // ----------------------------------------------------------------- toasts

  function toast(text, kind) {
    const t = h('div', 'toast' + (kind && kind !== 'ok' ? ' ' + kind : ''), text);
    const box = $('toasts');
    box.append(t);
    while (box.children.length > 3) box.firstChild.remove();
    // The toasts are a popover, kept open: in the top layer, but under a
    // dialog opened later, unless shown again above it.
    if (box.popover && document.querySelector('dialog[open]')) {
      box.hidePopover();
      box.showPopover();
    }
    setTimeout(() => t.remove(), kind === 'error' || kind === 'warn' ? 7000 : 4000);
  }

  // copyText copies text. Plain HTTP has no Clipboard API: then a hidden
  // text box and the old copy command (inside an open dialog, which makes
  // the rest of the page inert).
  async function copyText(text, btn) {
    let ok = false;
    try {
      await navigator.clipboard.writeText(text);
      ok = true;
    } catch {
      const ta = h('textarea', { class: 'sr-only', readonly: true });
      ta.value = text;
      (document.querySelector('dialog[open]') || document.body).append(ta);
      ta.select();
      try {
        ok = document.execCommand('copy');
      } catch {
        ok = false;
      }
      ta.remove();
      if (btn) btn.focus();
    }
    if (btn && btn.classList.contains('btn')) {
      const was = btn.textContent;
      btn.textContent = ok ? 'Copied' : 'Copy failed';
      setTimeout(() => {
        btn.textContent = was;
      }, 1500);
    } else toast(ok ? 'Copied' : 'Could not copy: select the text and copy it', ok ? 'ok' : 'warn');
  }

  // --------------------------------------------------------------- settings

  function openSettings() {
    openDialog({ id: 'settings', title: 'Settings', sub: 'Kept in this browser.', body: settingsBody() });
  }

  function repaintSettings() {
    if (!dialogShows('settings')) return;
    const b = $('dlg').querySelector('.dlg-b');
    if (b) fill(b, settingsBody());
  }

  function settingsBody() {
    const s = S.server || {};
    const notify = h('input', {
      type: 'checkbox', class: 'switch', checked: push.on, disabled: !PUSH_OK || !admin || push.busy,
      'aria-label': 'Push notifications', onchange: () => toggleNotify(),
    });
    const shortcuts = [
      ['New session', ['N']], ['Search sessions', ['/']], ['Open the 1st–9th live session', ['Ctrl', '1–9']],
      ['Folder, in a new session', [MOD, 'K']], ['Start the session', [MOD, '↵']], ['Start it and show its terminal', [MOD, '⇧', '↵']],
      ['Interrupt the agent', ['Ctrl', 'C']], ['Archive the open session', [MAC ? '⌥' : 'Alt', 'A']], ['Close a menu or panel, go back', ['Esc']],
    ];
    const where = s.hostname || s.name || 'the server';
    return [
      h('section', 'set-sec', h('h3', null, 'Appearance'),
        h('div', 'set-row', h('div', 'grow', h('div', null, 'Theme'), h('div', 'sub', 'System follows your device’s light or dark mode.')), themeSeg())),
      h('section', 'set-sec', h('h3', null, 'Notifications'),
        h('label', 'set-row', h('div', 'grow', h('div', null, 'Push notifications on this device'),
          h('div', 'sub', !PUSH_OK ? pushHint() : !admin ? 'Unlock this browser first.' : 'When an agent needs you, finishes its work, hits a usage limit or opens a pull request.')), notify),
        push.on ? h('div', null, h('button', { type: 'button', class: 'btn sm', onclick: testPush }, icon('bell'), 'Send a test')) : null),
      h('section', 'set-sec', h('h3', null, 'Access'),
        admin
          ? h('div', 'set-row', h('div', 'grow', h('div', null, 'This browser can manage the fleet'),
            h('div', 'sub', 'It starts, answers and ends sessions on every machine that shares the fleet key.')),
          h('button', { type: 'button', class: 'btn sm', onclick: () => { closeDialog(); signOut(); } }, icon('logout'), 'Sign out'))
          : h('div', 'set-row', h('div', 'grow', h('div', null, 'This browser can only look'),
            h('div', 'sub', ['Unlock it with the fleet key, or open the link ', h('code', 'mono', 'fleet web'), ` prints on ${where}.`])),
          h('button', { type: 'button', class: 'btn sm primary', onclick: openUnlock }, icon('key'), 'Unlock'))),
      keyboard() ? h('section', 'set-sec', h('h3', null, 'Keyboard'),
        h('dl', 'shortcuts', shortcuts.map(([what, keys]) => [h('dt', null, what), h('dd', null, keys.map((k) => h('kbd', null, k)))]))) : null,
      h('section', 'set-sec', h('h3', null, 'About'),
        h('dl', 'about',
          h('dt', null, 'Version'), h('dd', null, 'fleet ' + (s.version || 'dev')),
          h('dt', null, 'Machine'), h('dd', null, [s.name, s.hostname && s.hostname !== s.name ? s.hostname : '', `${s.os || '?'}/${s.arch || '?'}`].filter(Boolean).join(' · ')),
          h('dt', null, 'Listening'), h('dd', null, (s.listen || '–') + (s.mdns ? ' · mDNS' : '')),
          h('dt', null, 'Up'), h('dd', null, s.startedAtMs ? elapsed(s.startedAtMs) : '–'),
          s.id ? [h('dt', null, 'Server'), h('dd', null, s.id)] : null)),
    ];
  }

  // ----------------------------------------------------------------- unlock
  //
  // Someone who knows the fleet key (or this machine's admin token) types
  // it in instead of opening a `fleet web` link: POST /api/unlock trades
  // either for the admin token, which is then kept like one from a link.

  function openUnlock() {
    if (admin) return;
    let busy = false;
    const key = h('input', { class: 'input mono', type: 'password', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: 'the key, or a pasted link or join command', 'aria-label': 'Fleet key or admin token' });
    const status = h('p', { class: 'dlg-status', role: 'status', 'aria-live': 'polite' });
    const btn = h('button', { type: 'submit', class: 'btn primary' }, 'Unlock');
    const say = (text, err) => {
      status.className = 'dlg-status' + (err ? ' err' : '');
      status.textContent = text;
    };
    const submit = async (ev) => {
      ev.preventDefault();
      if (busy) return;
      const k = unlockKey(key.value);
      if (!k) return say('Enter the fleet key or the admin token.', true);
      busy = true;
      btn.disabled = true;
      say('Checking…');
      let r;
      try {
        r = await api('POST', '/api/unlock', { key: k });
      } catch (e) {
        busy = false;
        btn.disabled = false;
        return say(e.message, true);
      }
      busy = false;
      if (!r || !r.token) {
        btn.disabled = false;
        return say('The daemon sent no token.', true);
      }
      setToken(r.token);
      closeDialog();
      setAdmin(true);
      toast('This browser can now manage the fleet');
    };
    const where = (S.server && (S.server.hostname || S.server.name)) || 'the server';
    const form = h('form', { novalidate: true, onsubmit: submit }, h('label', 'field', h('span', 'field-l', 'Key'), key), status);
    openDialog({
      id: 'unlock',
      title: 'Unlock this browser',
      sub: `With the fleet key, or the admin token of ${where}: fleet web prints both. The browser keeps admin rights until you sign out.`,
      body: form,
      foot: [h('button', { type: 'button', class: 'btn', onclick: closeDialog }, 'Cancel'), h('button', { type: 'button', class: 'btn primary', onclick: (ev) => submit(ev) }, 'Unlock')],
      focus: touch() ? null : key,
      canClose: () => !busy,
      onClose: () => {
        key.value = ''; // a secret: gone once the dialog closes
      },
    });
  }

  // unlockKey is the key in what was typed or pasted: bare, or out of a
  // `fleet web` link (#token=...) or join command (--join ...).
  function unlockKey(text) {
    const m = /(?:#token=|--join\s+)([0-9A-Za-z]+)/.exec(text);
    return m ? m[1] : text.trim();
  }

  // --------------------------------------------------------- end a session

  // endAction is what ending agent a does: a live session is archived
  // (killed, kept under Archived with its chat and files), a finished one
  // deleted (forgotten). Either can remove the worktree or clone the daemon
  // made for it (wt names it), unless it holds work that would be lost; a
  // worktree's branch stays. Deleting removes it by default, archiving
  // keeps it.
  function endAction(a) {
    const iso = a.isolation === 'clone' ? 'clone' : 'worktree';
    const wt = a.worktree || '';
    return isFinished(a)
      ? { label: 'Delete', busy: 'Deleting…', forget: true, wt, iso, dropWt: true, note: 'It leaves Archived, with its chat.' }
      : { label: 'Archive', busy: 'Archiving…', forget: false, wt, iso, dropWt: false, note: 'The agent is ended and the session moves to Archived. Its chat stays readable.' };
  }

  // endAgent archives or deletes agent id of host (see endAction),
  // removing its worktree or clone if dropWt. It reports whether it worked.
  async function endAgent(host, id, a, dropWt) {
    const act = endAction(a || {});
    if (dropWt === undefined) dropWt = act.dropWt;
    const name = label(a || { id });
    const base = host ? `/api/hosts/${encodeURIComponent(host)}/agents/` : '/api/agents/';
    const body = {};
    if (act.forget) body.forget = true;
    if (dropWt) body.removeWorktree = true;
    try {
      const r = await api('POST', base + encodeURIComponent(id) + '/stop', body);
      const what = `${act.forget ? 'Deleted' : 'Archived'} ${name}`;
      if (act.forget) {
        saveDraft(host, id, '');
        agentsOf(host).delete(id);
      } else if (host && r && r.agent) {
        putAgent(host, r.agent);
      }
      if (r && r.worktreeKept) toast(`${what}; kept ${shortPath(act.wt || (a && a.cwd)) || 'its ' + act.iso}: ${r.reason}`, 'warn');
      else if (dropWt && act.wt) toast(`${what} and removed its ${act.iso}`);
      else toast(what);
      invalidate('agents');
      return true;
    } catch (e) {
      toast(`Could not ${act.label.toLowerCase()} ${name}: ${e.message}`, 'error');
      return false;
    }
  }

  // confirmEnd asks before archiving or deleting session a of host.
  function confirmEnd(host, a) {
    if (!admin || !a) return;
    const act = endAction(a);
    let busy = false;
    const cb = act.wt ? h('input', { type: 'checkbox', checked: act.dropWt }) : null;
    const yes = h('button', { type: 'button', class: 'btn danger', onclick: async () => {
      if (busy) return;
      busy = true;
      yes.disabled = true;
      yes.textContent = act.busy;
      const ok = await endAgent(host, a.id, a, cb ? cb.checked : undefined);
      busy = false;
      closeDialog();
      if (ok && U.view === 'chat' && U.key === agentKey(host, a.id)) go(small() ? '#/' : '#/new');
    } }, act.label);
    openDialog({
      id: 'end',
      title: `${act.label} “${label(a)}”?`,
      sub: act.note,
      body: cb ? h('label', 'check-row', cb, h('span', null, h('strong', null, `Also delete its ${act.iso} `), h('span', 'mono', shortPath(act.wt)),
        act.iso === 'worktree' && a.branch ? ` (branch ${a.branch} stays)` : '', '. Kept anyway if it holds unsaved work.')) : null,
      foot: [h('button', { type: 'button', class: 'btn', onclick: closeDialog }, 'Keep'), yes],
      focus: yes,
      canClose: () => !busy,
    });
  }

  // ------------------------------------------------------------------- note
  //
  // A note left on a session ("customer is testing, keep it open"), shown
  // on its row and pinned in its chat, for everyone with this dashboard.

  function noteEditor(anchor, host, a) {
    if (!admin) return;
    openMenu(anchor, (el, close) => {
      let busy = false;
      const ta = h('textarea', { class: 'input', rows: 3, maxlength: 500, placeholder: 'e.g. customer is testing the preview, keep it open', 'aria-label': 'Note' });
      ta.value = a.note || '';
      const count = h('span', 'muted num');
      const status = h('span', { class: 'muted', role: 'status' });
      const paint = () => {
        count.textContent = ta.value.length > 400 ? `${ta.value.length}/500` : '';
      };
      const saveIt = async (text) => {
        if (busy) return;
        busy = true;
        status.textContent = 'Saving…';
        const base = host ? `/api/hosts/${encodeURIComponent(host)}/agents/` : '/api/agents/';
        try {
          const r = await api('POST', base + encodeURIComponent(a.id) + '/note', { note: text.trim() });
          if (r && r.agent) putAgent(host, r.agent);
          close();
        } catch (e) {
          busy = false;
          status.textContent = e.message;
        }
      };
      ta.addEventListener('input', paint);
      // Enter saves (Shift+Enter: a new line); on a touch keyboard Enter
      // is a new line and Save saves. Ctrl/Cmd+Enter saves everywhere.
      ta.addEventListener('keydown', (ev) => {
        if (ev.key !== 'Enter' || ev.isComposing) return;
        if (ev.ctrlKey || ev.metaKey || (!ev.shiftKey && !touch())) {
          ev.preventDefault();
          saveIt(ta.value);
        }
      });
      el.append(h('div', 'pop-pad',
        h('div', null, h('div', 'pop-title', `Note on “${label(a)}”`), h('p', 'field-hint', 'Shown on its row and in its chat, to everyone with this dashboard.')),
        ta,
        h('div', 'pop-row', status, count, h('span', 'grow'),
          a.note ? h('button', { type: 'button', class: 'btn ghost sm', onclick: () => saveIt('') }, 'Remove') : null,
          h('button', { type: 'button', class: 'btn sm', onclick: () => close() }, 'Cancel'),
          h('button', { type: 'button', class: 'btn primary sm', onclick: () => saveIt(ta.value) }, 'Save'))));
      paint();
      setTimeout(() => ta.setSelectionRange(ta.value.length, ta.value.length));
    }, { label: 'Note', role: 'dialog', wide: true, focus: touch() ? null : 'textarea', alignRight: true });
  }

  // ------------------------------------------------------ git, pull requests
  //
  // The open session's checkout and pull requests, in a popover from the
  // branch in its header: fetch, pull (fast-forward, rebase or merge,
  // remembered) and push; a branch that diverged from its upstream is a
  // force push, and merging a pull request, which a first click arms and a
  // second within ARM_MS runs.

  const ARM_MS = 5000;
  const PULL_MODE = 'fleet.pullMode';
  const PULL_MODES = [['ff_only', 'Fast-forward'], ['rebase', 'Rebase'], ['merge', 'Merge']];
  const pullMode = () => {
    const m = load(PULL_MODE, 'ff_only');
    return PULL_MODES.some(([v]) => v === m) ? m : 'ff_only';
  };

  function gitPop(anchor) {
    openMenu(anchor, (el, close, menu) => {
      menu.repaint = () => fill(el, gitBody());
      menu.repaint();
    }, { label: 'Git and pull requests', role: 'dialog', wide: true, focus: '.btn:not(:disabled)' });
  }

  function repaintGit() {
    if (MENU && MENU.repaint && MENU.o.label === 'Git and pull requests') MENU.repaint();
  }

  function gitBody() {
    const a = chat.agent || {};
    const g = a.git;
    const ui = chat.gitUI;
    const out = [];
    if (g) {
      const shortRef = (ref) => (g.remote && ref.startsWith(g.remote + '/') ? ref.slice(g.remote.length + 1) : ref);
      const busy = !!ui.busy;
      const btn = (text, ic, title, disabled, onclick, cls = '') => h('button', { type: 'button', class: 'btn sm ' + cls, title, disabled: busy || disabled, onclick }, icon(ic), text);
      const noRemote = !g.remote;
      const working = a.state === 'working';
      const why = working ? 'Waits until the agent is idle: a pull changes its files'
        : !g.branch ? 'Check out a branch to pull into' : g.operation ? `A ${g.operation} is in progress` : '';
      const from = g.upstream || g.base;
      const behind = g.upstream ? g.behind : g.baseBehind;
      const up = gitUnpushed(g);
      const force = !!(g.upstream && g.ahead && g.behind);
      const armed = force && ui.armed && Date.now() - ui.armed < ARM_MS;
      const pushWhy = noRemote ? 'The repository has no remote' : !g.branch ? 'Check out a branch to push'
        : g.upstream && !g.ahead ? `${g.upstream} has every commit` : '';
      const mode = h('select', { class: 'select', 'aria-label': 'Pull mode', disabled: busy, onchange: (ev) => save(PULL_MODE, ev.target.value) },
        PULL_MODES.map(([v, text]) => h('option', { value: v, selected: v === pullMode() }, text)));
      const facts = [];
      if (g.base && (g.baseAhead || g.baseBehind)) facts.push(h('span', 'mono', `${g.base} `, g.baseAhead ? h('span', 'add', '↑' + g.baseAhead + ' ') : '', g.baseBehind ? h('span', 'del', '↓' + g.baseBehind) : ''));
      if (g.changed) facts.push(h('span', 'chip warn', `${g.changed} uncommitted`));
      if (g.conflicts) facts.push(h('span', 'chip bad', `${g.conflicts} conflicted`));
      if (g.operation) facts.push(h('span', 'chip bad', g.operation + ' in progress'));
      out.push(
        h('div', 'git-line', icon('branch'), h('span', 'mono trunc', g.branch || 'detached HEAD'),
          g.upstream ? h('span', 'muted mono trunc', '→ ' + g.upstream) : g.branch ? h('span', 'chip', 'not pushed') : null,
          g.upstream && g.ahead ? h('span', 'add mono', '↑' + g.ahead) : null, g.upstream && g.behind ? h('span', 'del mono', '↓' + g.behind) : null),
        facts.length ? h('div', 'git-facts', facts) : null,
        g.head ? h('div', 'git-commit', h('span', 'mono', g.head), ' ', g.headSubject || '', g.headTimeMs ? [' · ', relTime(g.headTimeMs, '', true)] : null,
          g.fetchedAtMs ? [' · fetched ', relTime(g.fetchedAtMs, '', true)] : null) : null,
        g.detail ? h('div', 'git-note err', g.detail) : null,
        h('div', 'pop-acts',
          btn(ui.busy === 'fetch' ? 'Fetching…' : 'Fetch', 'refresh', noRemote ? 'The repository has no remote' : `git fetch ${g.remote}`, noRemote, () => runGit('fetch')),
          from ? [mode, btn(ui.busy === 'pull' ? 'Pulling…' : 'Pull' + (g.upstream ? '' : ' ' + shortRef(g.base)) + (behind ? ' ↓' + behind : ''), 'arrow-down',
            why || `Fetch, then bring ${from} into ${g.branch}`, !!why, () => runGit('pull', { mode: mode.value }))] : null,
          g.upstream && g.base && g.baseBehind ? btn(ui.busy === 'base' ? 'Pulling…' : `Pull ${shortRef(g.base)} ↓${g.baseBehind}`, 'arrow-down',
            why || `Fetch, then bring ${g.base} into ${g.branch}`, !!why, () => runGit('pull', { mode: mode.value, fromBase: true }, 'base')) : null,
          btn(ui.busy === 'push' ? 'Pushing…' : force ? (armed ? 'Confirm force push' : `Force push ↑${g.ahead}`) : 'Push' + (up ? ' ↑' + up : ''), 'arrow-up',
            pushWhy || (force ? `${g.upstream} has ${plural(g.behind, 'commit')} ${g.branch} lacks: overwrite them (--force-with-lease)` : `Push ${g.branch} to ${g.remote}` + (g.upstream ? '' : ' and track it')),
            !!pushWhy, () => {
              if (force && !(chat.gitUI.armed && Date.now() - chat.gitUI.armed < ARM_MS)) {
                chat.gitUI.armed = Date.now();
                repaintGit();
                setTimeout(repaintGit, ARM_MS + 50);
                return;
              }
              runGit('push', { force });
            }, force ? 'force' + (armed ? ' armed' : '') : up ? 'primary' : '')),
        ui.note ? h('p', { class: 'git-note' + (ui.note.error ? ' err' : ''), role: 'status' }, ui.note.text) : null);
    }
    const prs = (a.pullRequests || []).slice().reverse();
    if (prs.length) {
      if (g) out.push(h('div', 'pop-sep'));
      out.push(h('div', 'pop-title', prs.length === 1 ? 'Pull request' : 'Pull requests'));
      for (const p of prs) out.push(prRow(p));
    }
    return h('div', 'pop-pad', out);
  }

  function prRow(p) {
    const ui = chat.prUI.get(p.url) || {};
    const [word, cls] = prStatus(p);
    const facts = [h('span', 'mono', `${p.repo}#${p.number}`)];
    if (p.headBranch) facts.push(h('span', null, `${p.headBranch} → ${p.baseBranch}`));
    if (p.changedFiles || p.additions || p.deletions) facts.push(h('span', null, h('span', 'add', '+' + p.additions), ' ', h('span', 'del', '−' + p.deletions), ` · ${plural(p.changedFiles, 'file')}`));
    if (p.reviewDecision === 'approved') facts.push(h('span', 'add', 'approved'));
    if (p.reviewDecision === 'changes_requested') facts.push(h('span', 'del', 'changes requested'));
    if (p.state === 'merged' && p.mergedAtMs) facts.push(relTime(p.mergedAtMs, '', true));
    if (p.detail) facts.push(h('span', 'del', p.detail));
    const link = h('a', { href: p.url, target: '_blank', rel: 'noopener noreferrer', title: prTitle(p) }, p.title || p.url);
    const acts = h('div', 'pr-acts', h('span', 'chip ' + cls, word));
    if (p.state === 'open') {
      const methods = p.mergeMethods && p.mergeMethods.length ? p.mergeMethods : ['squash', 'merge', 'rebase'];
      const method = methods.includes(ui.method) ? ui.method : methods[0];
      if (methods.length > 1) {
        acts.append(h('select', { class: 'select', 'aria-label': 'Merge method', disabled: !!ui.busy, onchange: (ev) => chat.prUI.set(p.url, { ...ui, method: ev.target.value, armed: 0 }) },
          methods.map((m) => h('option', { value: m, selected: m === method }, m))));
      }
      acts.append(h('label', null, h('input', { type: 'checkbox', checked: !!ui.deleteBranch, disabled: !!ui.busy, onchange: (ev) => chat.prUI.set(p.url, { ...chat.prUI.get(p.url), deleteBranch: ev.target.checked }) }), 'delete branch'));
      const armed = ui.armed && Date.now() - ui.armed < ARM_MS;
      acts.append(h('button', {
        type: 'button', class: 'btn sm' + (armed ? ' armed' : ''), disabled: !!ui.busy,
        title: p.draft ? 'Marks the draft ready for review, then merges it on GitHub' : `Merge #${p.number} on GitHub (${method})`,
        onclick: () => {
          const cur = chat.prUI.get(p.url) || {};
          if (!(cur.armed && Date.now() - cur.armed < ARM_MS)) {
            chat.prUI.set(p.url, { ...cur, armed: Date.now() });
            repaintGit();
            setTimeout(repaintGit, ARM_MS + 50);
            return;
          }
          mergePull(p, method, !!cur.deleteBranch);
        },
      }, icon('merge'), ui.busy ? 'Merging…' : armed ? `Confirm ${method} merge` : 'Merge'));
    }
    return h('div', 'pr-row', icon(p.state === 'merged' ? 'merge' : 'pr'),
      h('div', 'grow', h('div', 'pr-title', link), h('div', 'pr-sub', facts), acts));
  }

  // runGit runs a git action on the open session's checkout; key names it
  // in chat.gitUI.busy (default: the action).
  async function runGit(action, body, key) {
    const { host, id, seq } = chat;
    chat.gitUI = { busy: key || action };
    repaintGit();
    let note;
    try {
      const r = await api('POST', chatAPI('git'), { action, ...body });
      setChatGit(host, id, seq, r && r.git);
      const g = (r && r.git) || {};
      const said = r && r.output ? r.output.trim().split('\n').pop().trim() : '';
      if (action === 'push') note = `Pushed ${g.branch} to ${g.upstream || g.remote}`;
      else if (action === 'pull') note = `Pulled into ${g.branch}` + (said ? ': ' + said : '');
      else note = `Fetched ${g.remote}`;
      note = { text: note };
    } catch (e) {
      note = { text: `git ${action}: ${e.message}`, error: true };
    }
    if (chat.seq === seq) {
      chat.gitUI = { note };
      repaintGit();
      renderChatHead();
    }
  }

  // setChatGit stores the git state an action returned on the agent.
  function setChatGit(host, id, seq, git) {
    const a = findAgent(host, id);
    if (!a || !git) return;
    const next = { ...a, git };
    agentsOf(host).set(id, next);
    if (chat.seq === seq) chat.agent = next;
    invalidate('agents');
  }

  // refreshGit reads the open session's checkout when it opens, fetching
  // first if the last fetch is older than GIT_FETCH_AGE, so ahead and behind
  // are current. Failures stay quiet: the header shows what was last read.
  const GIT_FETCH_AGE = 5 * 60 * 1000;
  async function refreshGit(seq) {
    const a = chat.agent;
    if (!a || !a.git) return;
    const g = a.git;
    const action = g.remote && Date.now() - (g.fetchedAtMs || 0) > GIT_FETCH_AGE ? 'fetch' : 'status';
    const { host, id } = chat;
    try {
      const r = await api('POST', chatAPI('git'), { action });
      setChatGit(host, id, seq, r && r.git);
      if (chat.seq === seq) {
        renderChatHead();
        if (!chat.gitUI.busy) repaintGit();
      }
    } catch {
      // not a repository, gone, or offline
    }
  }

  // mergePull merges pull request p of the open session.
  async function mergePull(p, method, deleteBranch) {
    const { host, id, seq } = chat;
    chat.prUI.set(p.url, { ...chat.prUI.get(p.url), busy: true, armed: 0 });
    repaintGit();
    try {
      const r = await api('POST', chatAPI('merge'), { pullRequest: p.url, method, deleteBranch });
      const merged = r && r.pullRequest;
      const a = findAgent(host, id);
      if (a && merged) {
        const next = { ...a, pullRequests: (a.pullRequests || []).map((q) => (q.url === merged.url ? merged : q)) };
        agentsOf(host).set(id, next);
        if (chat.seq === seq) chat.agent = next;
        invalidate('agents');
      }
      toast(`Merged ${p.repo}#${p.number}`);
    } catch (e) {
      toast(`Could not merge #${p.number}: ${e.message}`, 'error');
    }
    if (chat.seq === seq) {
      chat.prUI.set(p.url, { ...chat.prUI.get(p.url), busy: false, armed: 0 });
      repaintGit();
      renderChatHead();
    }
  }

  // limitAction posts body to the open session's resume route: resume now,
  // or switch auto-resume after a usage limit.
  async function limitAction(btn, body, done) {
    const { host, id } = chat;
    btn.disabled = true;
    try {
      const r = await api('POST', chatAPI('resume'), body);
      if (r && r.agent) putAgent(host, r.agent);
      toast(`${label((r && r.agent) || { id })}: ${done}`);
    } catch (e) {
      btn.disabled = false;
      toast(`Could not resume: ${e.message}`, 'error');
    }
  }

  // ------------------------------------------------------------------- chat
  //
  // One session: the conversation parsed from the agent's transcript (GET
  // .../chat, long-polled; see feeds), messages and keys typed into its
  // terminal, its questions as a form, its model and effort, and its
  // workflow, files, media and terminal in the panel beside it.

  const chat = {
    open: false,
    host: '',
    id: '',
    agent: null,
    seq: 0, // bumped on open and leave: requests of an old session are dropped
    feed: null, // the conversation (see feeds)
    pending: null, // options of openChat, applied once its view shows
    sending: false,
    held: false, // the last message went into a dialog, without Enter
    model: null, // {model, name, effort, switch, models, efforts}; null: the agent offers no choice
    switching: false, // a model switch is in flight
    busyLine: null, // what the working agent is busy with, at the end of the conversation (see renderBusyLine)
    switchedAt: 0, // when the last one ended: replies read before it are stale
    interrupting: false,
    queueing: false, // a message is being queued
    asks: [], // questions the agent waits on, the first shown first
    answered: new Set(), // ids of questions answered here: late replies may still list them
    askForm: null, // the form of asks[0]
    images: [], // {data (base64), url (data: URL), name} to send with the next message; no data yet while read
    videos: [], // uploaded to the agent's machine for the next message (see addVideos)
    keys: false, // the keys for the terminal's dialogs show
    prUI: new Map(), // pull request URL -> {method, deleteBranch, armed (ms), busy}
    gitUI: {}, // {busy: the git action in flight, armed: when force push was clicked once, note: {text, error}}
  };
  const C = {}; // the chat view's elements

  const chatAPI = (rest) => {
    const base = chat.host ? `/api/hosts/${encodeURIComponent(chat.host)}/agents/` : '/api/agents/';
    return base + encodeURIComponent(chat.id) + '/' + rest;
  };
  const chatLive = () => !!chat.agent && !isFinished(chat.agent);
  const chatDialog = () => chatLive() && chat.agent.state === 'needs_input';
  // chatBusy: the agent works on a turn, or waits on a dialog of one; Stop
  // and Ctrl+C interrupt it then.
  const chatBusy = () => chatLive() && (chat.agent.state === 'working' || chat.agent.state === 'needs_input');

  // A session's draft: what is typed into its message box stays in
  // localStorage until it is sent, across closing the session or reloading.
  const draftKey = (host, id) => 'fleet.draft.' + agentKey(host, id);
  function loadDraft(host, id) {
    try {
      return localStorage.getItem(draftKey(host, id)) || '';
    } catch {
      return '';
    }
  }
  function saveDraft(host, id, text) {
    try {
      if (text) localStorage.setItem(draftKey(host, id), text);
      else localStorage.removeItem(draftKey(host, id));
    } catch {
      // storage full or blocked: the draft lives only in the box
    }
  }

  // openChat shows a session: opts.view 'wf' opens its workflow (run
  // opts.run unfolded), opts.screen its terminal.
  function openChat(host, id, agent, opts) {
    if (!admin) return;
    // A session just started may not be in the lists yet.
    if (agent && !findAgent(host, id)) putAgent(host, agent);
    chat.pending = opts || null;
    go(chatHash(host, id));
  }

  function showChat(host, id) {
    if (chat.open && chat.host === host && chat.id === id && C.root && C.root.isConnected) {
      applyPending();
      return;
    }
    if (chat.open) leaveChat();
    chat.seq++;
    Object.assign(chat, {
      open: true, host, id, agent: findAgent(host, id), sending: false, held: false, model: null, switching: false, switchedAt: 0,
      interrupting: false, queueing: false, asks: [], askForm: null, images: [], videos: [], keys: false, gitUI: {},
    });
    chat.answered.clear();
    chat.prUI.clear();
    resetWorkflows(host, id);
    resetMedia();
    resetFiles();
    insp.tab = '';
    insp.open = insp.open && !small();
    buildChatView();
    let firstReply = true;
    chat.feed = newFeed({
      scroller: C.scroll,
      log: C.log,
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
        // The media follow: in full at first, then with each image; so do
        // the files (read when the session opens, a shell has no chat), a
        // moment after some were written.
        if (r.reset || (r.entries || []).some((e) => e.images && e.images.length)) loadMedia();
        if (r.reset) {
          if (!firstReply) loadFiles();
          firstReply = false;
        } else if ((r.entries || []).some((e) => e.files && e.files.length)) {
          filesSoon(1500);
          treeSoon(1500);
        }
      },
      onStatus: renderChatStatus,
      cacheKey: agentKey(host, id),
      onCopy: (copy) => chatModel(copy.model),
    });
    renderChatHead();
    renderBanners();
    renderDock();
    startFeed(chat.feed);
    loadFiles();
    wfLoop(chat.seq);
    refreshGit(chat.seq);
    if (chat.agent) markSeen(host, chat.agent);
    clearNotices(id);
    renderInsp();
    applyPending();
    if (keyboard() && C.comp) C.comp.ta.focus({ preventScroll: true });
  }

  // applyPending opens what openChat asked for: the workflow or terminal.
  function applyPending() {
    const o = chat.pending;
    chat.pending = null;
    if (!o) return;
    if (o.view === 'wf') openRun(o.run || '');
    else if (o.screen) openInsp('terminal');
  }

  // leaveChat stops following the session shown.
  function leaveChat() {
    if (!chat.open) return;
    chat.open = false;
    chat.seq++;
    chat.videos = dropVideos(chat.videos);
    stopScreen();
    clearTimeout(files.timer);
    revokeFileURL();
    if (chat.feed) stopFeed(chat.feed);
    storeFeed(agentKey(chat.host, chat.id));
    stopFeed(wf.feed);
    wf.sub = null;
    if (MENU && MENU.anchor && C.root && C.root.contains(MENU.anchor)) closeMenu(false);
    insp.panes = null;
    renderInsp();
  }

  // chatAgent takes a newer state of an agent from the socket or a poll.
  function chatAgent(host, a) {
    if (!chat.open || chat.host !== host || chat.id !== a.id) return;
    if (chat.agent && (a.updatedAtMs || 0) < (chat.agent.updatedAtMs || 0)) return;
    const was = chat.agent && chat.agent.state;
    chat.agent = a;
    markSeen(host, a);
    if (a.state !== was && chat.feed) paintPending(chat.feed);
    renderChatHead();
    renderBanners();
    renderDock();
    repaintGit();
    if (insp.open) renderInspTabs();
  }

  function chatEmpty() {
    const a = chat.agent || {};
    return a.adapter === 'shell' ? 'A shell keeps no chat: type into it in its terminal.'
      : chatLive() ? 'No messages yet. The conversation shows here once the first message is sent.'
        : 'This session left no conversation.';
  }

  function buildChatView() {
    C.head = h('header', 'topbar chead');
    C.banners = h('div');
    C.log = h('div', { class: 'col', role: 'log', 'aria-label': 'Conversation', 'aria-live': 'polite' });
    C.scroll = h('div', 'cscroll', C.log);
    C.ask = h('div');
    C.dlg = h('div');
    // The dialog card of the chat shown before went with its view.
    C.dlgPre = C.dlgLogin = null;
    C.keys = h('div');
    C.queue = h('div');
    C.queueKey = '';
    C.compBox = h('div');
    C.status = h('div', { class: 'dock-status', role: 'status', 'aria-live': 'polite' });
    C.comp = null;
    const dock = h('div', 'dock', h('div', 'dock-in', C.ask, C.dlg, C.keys, C.queue, C.compBox, C.status));
    stickToBottom(C.scroll, dock);
    C.root = h('div', 'view chat', C.head, C.banners, C.scroll, dock);
    fill($('main'), C.root);
    wireChatDrop(C.root);
  }

  // ---- header, banners

  function renderChatHead() {
    if (!chat.open || !C.head) return;
    const a = chat.agent || { id: chat.id, name: chat.id, state: 'unspecified' };
    const k = stateKey(a);
    const g = a.git;
    const meta = [];
    meta.push(h('span', { class: 'dev hide-phone', title: (a.adapter || 'agent') + ' session' }, logo(a.adapter, 'i-xs'), h('span', 'mono', a.name || a.id)));
    if (chat.host || S.remote.size) {
      const st = hostState(chat.host);
      meta.push(h('span', { class: 'dev', title: `${hostName(chat.host)}: ${MACHINE_STATE[st][1]}` }, h('span', MACHINE_STATE[st][0]), hostName(chat.host)));
    }
    meta.push(h('span', { class: 'path hide-phone', title: a.cwd || a.path || '' }, shortPath(a.path || a.cwd || '')));
    if (g || (a.pullRequests || []).length) {
      const up = g ? gitUnpushed(g) : 0;
      meta.push(h('button', {
        type: 'button', class: 'meta-btn', 'aria-haspopup': 'dialog', 'aria-expanded': 'false', title: g ? gitTitle(g) : 'Pull requests',
        'aria-label': g ? `Git: branch ${g.branch || 'detached HEAD'}. Fetch, pull, push and pull requests` : 'Pull requests',
        onclick: (ev) => gitPop(ev.currentTarget),
      }, icon(g ? 'branch' : 'pr'),
      g ? [h('span', 'trunc', g.branch || 'detached HEAD'),
        up ? h('span', 'up', '↑' + up) : null, g.upstream && g.behind ? h('span', 'down', '↓' + g.behind) : null,
        g.changed ? h('span', { class: 'dirty', title: `${g.changed} uncommitted` }, '●' + g.changed) : null,
        g.operation || g.conflicts ? h('span', 'dirty', g.operation || 'conflicts') : null,
        !g.upstream && g.branch && !isFinished(a) ? h('span', 'muted', 'not pushed') : null] : 'Pull requests'));
    }
    for (const p of (a.pullRequests || []).filter(prOpen).slice(-2)) meta.push(h('span', 'hide-phone', prChip(p)));

    const acts = [];
    if (!small()) {
      if (!a.note) acts.push(iconBtn('note', 'Add a note', (ev) => noteEditor(ev.currentTarget, chat.host, a)));
      if (chatLive()) acts.push(iconBtn('terminal', 'Terminal', () => toggleInsp('terminal'), { attrs: { 'aria-pressed': String(insp.open && insp.tab === 'terminal') } }));
    }
    const tabs = inspTabs();
    if (tabs.some((t) => t.id !== 'terminal') || small()) {
      const panel = iconBtn(small() ? 'layers' : 'panel-right', 'Workflow, files and media', () => toggleInsp(), { attrs: { 'aria-pressed': String(insp.open && insp.tab !== 'terminal') } });
      if (wf.runs.some((r) => r.status === 'running')) panel.append(h('span', 'dot-mark'));
      acts.push(panel);
    }
    acts.push(iconBtn('more', 'More actions', (ev) => moreMenu(ev.currentTarget)));

    fill(C.head,
      leadBtn(),
      h('div', 'chead-title',
        h('div', 'chead-row', h('h1', { title: label(a) }, label(a)),
          k !== 'idle' && k !== 'running' ? h('span', 'pill ' + (k === 'unspecified' ? 'idle' : k), k === 'working' ? h('span', 'spin') : h('span', 'dot'), h('span', small() ? 'sr-only' : null, KEY_LABEL[k])) : null),
        h('div', 'chead-meta', meta)),
      h('div', 'chead-acts', acts));
  }

  function moreMenu(anchor) {
    const a = chat.agent;
    if (!a) return;
    const act = endAction(a);
    openMenu(anchor, (el, close) => {
      if (small()) {
        el.append(mi({ icon: 'note', label: a.note ? 'Edit the note' : 'Add a note', onSelect: () => { close(false); noteEditor(anchor, chat.host, a); } }));
        if (chatLive()) el.append(mi({ icon: 'terminal', label: 'Terminal', onSelect: () => { close(false); openInsp('terminal'); } }));
      }
      el.append(
        mi({ icon: 'copy', label: 'Copy the attach command', sub: `fleet attach ${a.name || a.id}`, onSelect: () => { close(); copyText(`fleet attach ${a.name || a.id}`); } }),
        mi({ icon: 'folder', label: 'New session in this folder', sub: shortPath(a.path || a.cwd || ''), onSelect: () => { close(false); newIn(chat.host, a.path || a.cwd || ''); } }),
        h('div', 'menu-sep'),
        mi({ icon: act.forget ? 'trash' : 'archive', label: act.label, kbd: keyboard() ? (MAC ? '⌥A' : 'Alt+A') : null, danger: act.forget, onSelect: () => { close(false); confirmEnd(chat.host, a); } }));
    }, { label: 'More actions', alignRight: true });
  }

  function renderBanners() {
    if (!C.banners) return;
    const a = chat.agent;
    const out = [];
    if (a && a.note) {
      out.push(h('div', 'banner note', icon('note'), h('div', 'banner-text', a.note),
        h('div', 'banner-acts', h('button', { type: 'button', class: 'btn sm', onclick: (ev) => noteEditor(ev.currentTarget, chat.host, a) }, 'Edit'))));
    }
    const l = a && !isFinished(a) && a.usageLimit;
    if (l) {
      const text = h('div', 'banner-text', h('strong', null, 'Usage limit reached' + (l.window ? ` (${l.window})` : '')));
      if (l.resetsAtMs) text.append(` · resets ${clock(l.resetsAtMs)}`);
      if (l.detail) text.append(' · ' + l.detail);
      else if (l.resumeAtMs) text.append(` · fleet types “continue” at ${clock(l.resumeAtMs)}`);
      else if (!l.autoResume) text.append(' · auto-resume is off');
      const now = h('button', { type: 'button', class: 'btn sm', title: 'Type “continue” into the session now' }, icon('play'), 'Resume now');
      now.addEventListener('click', () => limitAction(now, { now: true }, 'resumed'));
      const auto = h('button', { type: 'button', class: 'btn sm' }, l.autoResume ? 'Turn auto-resume off' : 'Turn auto-resume on');
      auto.addEventListener('click', () => limitAction(auto, { auto: !l.autoResume }, l.autoResume ? 'auto-resume off' : 'auto-resume on'));
      out.push(h('div', { class: 'banner limit', role: 'status' }, icon('clock'), text, h('div', 'banner-acts', now, auto)));
    }
    fill(C.banners, out);
  }

  // ---- the dock: questions, a dialog, keys, the message box

  function renderDock() {
    if (!chat.open || !C.compBox) return;
    const a = chat.agent;
    renderChatAsk();
    renderDialogCard();
    renderKeys();
    renderQueue();
    if (a && isFinished(a)) {
      C.comp = null;
      fill(C.compBox, h('div', 'banner locked', icon(a.state === 'failed' ? 'alert' : 'archive'),
        h('div', 'banner-text', h('strong', null, a.state === 'failed' ? 'This session failed' : 'This session ended'),
          a.exitCode != null ? ` · exit ${a.exitCode}` : '', a.stateDetail ? ' · ' + a.stateDetail : '', a.updatedAtMs ? [' · ', relTime(a.updatedAtMs, '', true)] : null),
        h('div', 'banner-acts',
          h('button', { type: 'button', class: 'btn sm', onclick: () => newIn(chat.host, a.path || a.cwd || '') }, 'New session here'),
          h('button', { type: 'button', class: 'btn sm', onclick: () => confirmEnd(chat.host, a) }, icon('trash'), 'Delete'))));
    } else {
      if (!C.comp || !C.compBox.contains(C.comp.el)) {
        C.comp = chatComposer();
        fill(C.compBox, C.comp.el);
      }
      C.comp.update();
    }
    renderChatStatus();
  }

  function renderChatStatus() {
    const st = C.status;
    if (!st || !chat.open) return;
    st.className = 'dock-status';
    const a = chat.agent;
    let t = '';
    if (chat.feed && chat.feed.error) {
      st.className = 'dock-status warn';
      t = chat.feed.error;
    } else if (chat.held && chatLive()) {
      st.className = 'dock-status warn';
      t = 'The agent shows a dialog: your text was typed, but Enter was not pressed. Answer with the keys.';
    } else if (!a || !chatLive()) {
      t = '';
    } else if (a.adapter === 'shell') t = 'A shell takes input in its terminal: open it from the header.';
    else if (chat.switching) t = 'Switching the model…';
    else if (askShown()) t = 'A message sent now answers none of the questions: the agent reads it instead.';
    else if (chat.interrupting) t = 'Interrupting…';
    else if (chatDialog()) t = 'In a dialog, Send types your text without pressing Enter: Enter picks the highlighted option.';
    else if (a.state === 'working') t = (a.stateDetail ? a.stateDetail + '…' : 'Working… replies show as the agent writes them') + (keyboard() ? ' · Ctrl+C interrupts' : '');
    else if (a.state === 'starting') t = 'Starting the agent…';
    st.textContent = t;
    renderBusyLine();
  }

  // renderBusyLine shows what a working agent is busy with, when it says
  // (it compacts its conversation), at the end of the conversation.
  function renderBusyLine() {
    const f = chat.feed;
    const a = chat.agent;
    const text = f && a && chatLive() && a.state === 'working' ? a.stateDetail || '' : '';
    if (!text) {
      if (chat.busyLine) chat.busyLine.remove();
      chat.busyLine = null;
      return;
    }
    if (!chat.busyLine) chat.busyLine = h('div', { class: 'meta-line busy', role: 'status' });
    fill(chat.busyLine, h('span', 'spin'), text);
    if (f.log.lastElementChild !== chat.busyLine) f.log.append(chat.busyLine);
  }

  // The keys of the agent's own dialogs (permissions, trust, menus).
  const KEYS = [
    ['escape', 'Esc'], ['up', '↑'], ['down', '↓'], ['left', '←'], ['right', '→'], ['tab', 'Tab'],
    ['enter', 'Enter'], ['1', '1'], ['2', '2'], ['3', '3'], ['4', '4'], ['5', '5'], ['6', '6'], ['ctrl-c', 'Ctrl‑C'],
  ];

  function keysStrip() {
    return h('div', { class: 'keys', role: 'group', 'aria-label': 'Keys for the terminal' }, h('span', 'keys-l', 'Keys'),
      KEYS.map(([k, text]) => h('button', { type: 'button', class: 'key', title: `Press ${text} in the agent’s terminal`, onclick: () => sendKey(k) }, text)));
  }

  function renderKeys() {
    if (!C.keys) return;
    const show = chat.keys && chatLive() && chat.agent.adapter !== 'shell' && !dialogShown();
    fill(C.keys, show ? keysStrip() : null);
  }

  // A dialog in the terminal the chat has no form for (a permission, a
  // trust prompt, a menu): its screen and the keys, under the chat.
  const dialogShown = () => chatLive() && !askShown() && (chatDialog() || chat.held);

  function renderDialogCard() {
    if (!C.dlg) return;
    if (!dialogShown()) {
      if (C.dlgPre) {
        fill(C.dlg);
        C.dlgPre = C.dlgLogin = null;
        screenNeeded();
      }
      return;
    }
    if (!C.dlgPre) {
      C.dlgPre = h('pre', { 'aria-label': 'The bottom of the terminal' }, screen.text || 'Reading the terminal…');
      C.dlgDetail = h('span', 'grow trunc');
      C.dlgLogin = h('div', 'login');
      fill(C.dlg, h('section', { class: 'dlgcard', 'aria-label': 'The agent waits in its terminal' },
        h('div', 'dlgcard-h', icon('alert'), C.dlgDetail,
          h('button', { type: 'button', class: 'btn sm', onclick: () => openInsp('terminal') }, 'Terminal')),
        C.dlgLogin, C.dlgPre, keysStrip()));
      paintLogin();
      // The dialog is at the bottom of the screen.
      C.dlgPre.scrollTop = C.dlgPre.scrollHeight;
      screenNeeded();
    }
    paintDialogDetail();
  }

  function paintDialogDetail() {
    const a = chat.agent;
    C.dlgDetail.textContent = chat.held && !chatDialog() ? 'Your text went into a dialog: answer it with the keys'
      : loginStep() ? loginStep().detail
        : `${AGENT_NAMES[a.adapter] || 'The agent'} waits in its terminal${a.stateDetail ? ': ' + a.stateDetail : ''}`;
  }

  // paintLogin shows the step of signing in the terminal shows (the
  // screen's login): the sign-in page as a button, its one-time code, and
  // a field for the code the page gives back, instead of a URL wrapped over
  // the terminal's lines.
  const loginStep = () => (screen.login && screen.loginSeq === chat.seq ? screen.login : null);

  function paintLogin() {
    if (!C.dlgLogin) return;
    const l = loginStep();
    const key = l ? JSON.stringify(l) : '';
    if (C.dlgLogin.dataset.key === key) return;
    C.dlgLogin.dataset.key = key;
    paintDialogDetail();
    const url = l && /^https:\/\//.test(l.url || '') ? l.url : '';
    // Hidden, the terminal lost its place at the bottom.
    if (C.dlgPre.hidden && !url) {
      C.dlgPre.hidden = false;
      C.dlgPre.scrollTop = C.dlgPre.scrollHeight;
    } else C.dlgPre.hidden = !!url;
    if (!l || (!url && !l.code && !l.paste)) return fill(C.dlgLogin);
    const machine = chat.host ? hostName(chat.host) : (S.server && S.server.name) || 'this machine';
    const rows = [];
    if (url) {
      rows.push(h('div', 'login-row',
        h('button', { type: 'button', class: 'btn primary sm', onclick: () => openOut(url) }, icon('external'), 'Open the sign-in page'),
        h('button', { type: 'button', class: 'btn sm', onclick: (ev) => copyText(url, ev.currentTarget) }, 'Copy link')));
    }
    if (l.local) rows.push(h('p', 'login-note', `This page finishes the sign-in on ${machine} itself, so open it in a browser there. From another device, press Esc and choose Sign in with Device Code.`));
    if (l.code) {
      rows.push(h('div', 'login-row', h('span', 'login-l', 'Code'), h('code', 'login-code', l.code),
        h('button', { type: 'button', class: 'btn sm', onclick: (ev) => copyText(l.code, ev.currentTarget) }, 'Copy')));
    }
    if (l.paste) {
      const inp = h('input', { class: 'input mono', type: 'text', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: url ? 'Paste the code the page shows' : 'Paste it here', 'aria-label': url ? 'Code from the sign-in page' : 'Answer' });
      const btn = h('button', { type: 'submit', class: 'btn primary sm' }, 'Send');
      rows.push(h('form', { class: 'login-row', novalidate: true, onsubmit: (ev) => {
        ev.preventDefault();
        if (inp.value.trim()) sendLoginCode(inp.value.trim(), btn);
      } }, inp, btn));
    }
    fill(C.dlgLogin, rows);
  }

  // sendLoginCode types the code a sign-in page gave into the terminal and
  // presses Enter. The sign-in step counts as a dialog, where a message's
  // Enter is held back, so Enter goes as a key of its own.
  async function sendLoginCode(text, btn) {
    btn.disabled = true;
    try {
      await api('POST', chatAPI('input'), { text });
      await new Promise((r) => setTimeout(r, 300));
      await api('POST', chatAPI('input'), { keys: ['enter'] });
    } catch (e) {
      toast(`Could not send the code: ${e.message}`, 'error');
      btn.disabled = false;
      return;
    }
    chat.held = false;
    renderDock();
    setTimeout(refreshScreen, 500);
  }

  // ---- the message box

  // effortLevel is how many bars an effort gets: its place among the
  // agent's efforts, for names the page does not know.
  const EFFORT_LEVEL = { none: 0, minimal: 1, low: 1, medium: 2, high: 3, xhigh: 4, max: 5, ultra: 6, ultracode: 6 };
  const effortLevel = (id, list) => EFFORT_LEVEL[id] ?? Math.max(1, Math.min(6, (list || []).findIndex((e) => e.id === id) + 1));

  function effortBars(n) {
    return h('span', { class: 'effort-bars', 'aria-hidden': 'true' }, [4, 5.5, 7, 8.5, 10, 12].map((ht, i) => {
      const b = h('span', i < n ? 'on' : null);
      b.style.height = ht + 'px';
      return b;
    }));
  }

  function chatComposer() {
    const ta = h('textarea', { id: 'chat-input', rows: 1, spellcheck: 'true' });
    ta.value = loadDraft(chat.host, chat.id);
    const thumbs = h('div', 'attach-list');
    const file = h('input', { type: 'file', multiple: true, hidden: true, tabindex: '-1', 'aria-hidden': 'true' });
    const attach = h('button', { type: 'button', class: 'cpick icon-only', 'aria-label': 'Attach images or files', title: 'Attach images or files (or paste, or drop them)', onclick: () => file.click() }, icon('clip'));
    const modelBtn = h('button', { type: 'button', class: 'cpick model', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => chatModelMenu(ev.currentTarget) });
    const effortBtn = h('button', { type: 'button', class: 'cpick effort', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => chatEffortMenu(ev.currentTarget) });
    const keysBtn = h('button', { type: 'button', class: 'cpick icon-only', 'aria-label': 'Terminal keys', title: 'Keys for the agent’s own dialogs', 'aria-pressed': 'false', onclick: () => { chat.keys = !chat.keys; renderKeys(); api_.update(); } }, icon('keyboard'));
    const stop = h('button', { type: 'button', class: 'stop-btn', 'aria-label': 'Stop', title: 'Interrupt the agent’s turn, as Esc in its terminal (Ctrl+C)', onclick: interruptChat }, icon('stop'), h('span', 'stop-lbl', 'Stop'));
    const queueBtn = h('button', { type: 'button', class: 'cpick icon-only', 'aria-label': 'Queue', onclick: queueMessage }, icon('queue'));
    const send = h('button', { type: 'submit', class: 'send', 'aria-label': 'Send' }, icon('arrow-up'));
    const prog = sendBar();
    const form = h('form', { class: 'composer', novalidate: true },
      prog.el, thumbs, ta, h('div', 'ctoolbar', attach, modelBtn, effortBtn, h('span', 'grow'), keysBtn, stop, queueBtn, send), file);

    const fit = () => {
      ta.style.height = 'auto';
      ta.style.height = Math.min(ta.scrollHeight + 2, viewHeight() * 0.4) + 'px';
    };
    file.addEventListener('change', () => {
      addChatFiles(file.files);
      file.value = '';
    });
    ta.addEventListener('paste', (ev) => {
      const files = mediaFiles(ev.clipboardData);
      if (!files.length) return;
      ev.preventDefault();
      addChatFiles(files);
    });
    ta.addEventListener('input', () => {
      fit();
      if (chat.open) saveDraft(chat.host, chat.id, ta.value);
      api_.update();
    });
    ta.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && ev.altKey && !ev.shiftKey && !ev.ctrlKey && !ev.metaKey && !ev.isComposing) {
        ev.preventDefault();
        queueMessage();
        return;
      }
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing && !touch()) {
        ev.preventDefault();
        sendMessage();
      }
    });
    form.addEventListener('submit', (ev) => {
      ev.preventDefault();
      sendMessage();
    });
    thumbs.addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-image], button[data-video]');
      if (!b) return;
      if (b.dataset.video) dropVideos(chat.videos.splice(Number(b.dataset.video), 1));
      else chat.images.splice(Number(b.dataset.image), 1);
      api_.paint();
      if (!touch()) ta.focus();
    });

    const api_ = {
      el: form, ta, fit, progress: prog.set,
      paint() {
        renderThumbs(thumbs, chat.images, chat.videos);
        api_.update();
      },
      update() {
        const a = chat.agent || {};
        const live = chatLive();
        const shell = a.adapter === 'shell';
        const short = AGENT_NAMES[a.adapter] || 'the agent';
        ta.disabled = !live || shell;
        ta.setAttribute('aria-label', 'Message ' + short);
        ta.placeholder = shell ? 'A shell takes input in its terminal'
          : askShown() ? 'Or reply in your own words instead'
            : chatDialog() ? 'Type into the dialog (Enter is not pressed)'
              : keyboard() ? `Message ${short} · Enter sends, Shift+Enter adds a line` : 'Message ' + short;
        attach.disabled = !live || shell || chat.sending || askShown();
        stop.hidden = !chatBusy();
        stop.disabled = chat.interrupting;
        queueBtn.hidden = !live || shell;
        queueBtn.disabled = chat.queueing || askShown() || chat.images.length > 0 || (!ta.value.trim() && !chat.videos.length);
        queueBtn.title = chat.images.length ? 'Images cannot be queued: send them now'
          : `Queue it: sent once ${short} is done${keyboard() ? ` (${MAC ? '⌥' : 'Alt+'}Enter)` : ''}`;
        keysBtn.hidden = shell || !live;
        keysBtn.setAttribute('aria-pressed', String(chat.keys || dialogShown()));
        send.disabled = !live || shell || chat.sending || (!ta.value.trim() && !chat.images.length && !chat.videos.length);
        // model and effort
        const m = chat.model;
        modelBtn.hidden = !m;
        effortBtn.hidden = true;
        if (!m) return;
        const cur = m.models.find((x) => x.id === m.model);
        fill(modelBtn, logo(a.adapter), h('span', 'lbl', cur ? cur.label : m.name || 'default model'), icon('chevron-down', 'chev'));
        modelBtn.setAttribute('aria-label', 'Model: ' + (cur ? cur.label : m.name || 'default'));
        const efforts = cur ? m.efforts.filter((e) => cur.efforts.includes(e.id)) : m.efforts;
        const ecur = efforts.find((e) => e.id === m.effort);
        effortBtn.hidden = !efforts.length && !m.effort;
        fill(effortBtn, effortBars(effortLevel(m.effort, m.efforts)), h('span', 'lbl', ecur ? ecur.label : m.effort || 'Effort'), icon('chevron-down', 'chev'));
        effortBtn.setAttribute('aria-label', 'Effort: ' + (ecur ? ecur.label : m.effort || 'default'));
        const can = m.switch && live && !chat.switching;
        const why = !live ? 'the session has ended' : !m.switch ? 'this agent cannot switch a running session: choose when starting one' : 'a switch holds for this session only';
        modelBtn.title = `Model${m.name ? ': ' + m.name : ''} · ${why}`;
        effortBtn.title = `Effort${m.effort ? ': ' + m.effort : ''} · ${why}`;
        modelBtn.disabled = !can && !m.models.length;
        effortBtn.disabled = !can || !efforts.length;
      },
    };
    api_.paint();
    requestAnimationFrame(fit);
    return api_;
  }

  function chatModelMenu(anchor) {
    const m = chat.model;
    if (!m) return;
    const can = m.switch && chatLive() && !chat.switching;
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Model'));
      if (!m.models.some((x) => x.id === m.model)) el.append(mi({ label: m.name || 'default model', checked: true, disabled: true }));
      for (const x of m.models) {
        el.append(mi({ label: x.label, checked: x.id === m.model, disabled: !can && x.id !== m.model, onSelect: () => {
          close();
          if (x.id !== m.model) switchChatModel(x.id, '');
        } }));
      }
      el.append(h('div', 'menu-sep'), h('p', 'menu-note', can ? 'Switches this session only; your defaults stay.'
        : !chatLive() ? 'The session has ended.' : 'This agent cannot switch a running session: choose the model when you start one.'));
    }, { label: 'Model' });
  }

  function chatEffortMenu(anchor) {
    const m = chat.model;
    if (!m) return;
    const cur = m.models.find((x) => x.id === m.model);
    const efforts = cur ? m.efforts.filter((e) => cur.efforts.includes(e.id)) : m.efforts;
    const can = m.switch && chatLive() && !chat.switching;
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Effort'));
      for (const e of efforts) {
        el.append(mi({ custom: effortBars(effortLevel(e.id, m.efforts)), label: e.label, checked: e.id === m.effort, disabled: !can && e.id !== m.effort, onSelect: () => {
          close();
          if (e.id !== m.effort) switchChatModel('', e.id);
        } }));
      }
      el.append(h('div', 'menu-sep'), h('p', 'menu-note', can ? 'For this session only.' : 'This agent cannot change it mid-session.'));
    }, { label: 'Effort' });
  }

  // chatModel takes the model of a chat reply. Replies read while a switch
  // went on, or just before it ended, would undo it on the page.
  function chatModel(m) {
    if (chat.switching || Date.now() - chat.switchedAt < 2000) return;
    if (same(m || null, chat.model)) return;
    chat.model = m || null;
    if (C.comp) C.comp.update();
  }

  // switchChatModel switches the session to model and/or effort; a new
  // model keeps the effort shown if it runs at it.
  async function switchChatModel(model, effort) {
    const m = chat.model;
    if (!m || chat.switching || !chatLive()) return;
    const body = { model: model && model !== m.model ? model : '', effort: '' };
    const to = m.models.find((x) => x.id === (body.model || m.model));
    if (body.model) body.effort = to && to.efforts.includes(m.effort) ? m.effort : '';
    else if (effort && effort !== m.effort) body.effort = effort;
    if (!body.model && !body.effort) return;
    const text = [to ? to.label : m.name, (m.efforts.find((e) => e.id === (body.effort || m.effort)) || {}).label].filter(Boolean).join(' · ');
    const seq = chat.seq;
    chat.switching = true;
    C.comp?.update();
    renderChatStatus();
    try {
      const r = await api('POST', chatAPI('model'), body);
      if (seq !== chat.seq) return;
      chat.model = (r && r.model) || m;
      toast(`${text || 'Switched'} for this session`);
    } catch (e) {
      if (seq !== chat.seq) return;
      toast(`Could not switch: ${e.message}`, 'error');
    }
    chat.switching = false;
    chat.switchedAt = Date.now();
    C.comp?.update();
    renderChatStatus();
  }

  async function sendMessage() {
    if (!C.comp) return;
    const ta = C.comp.ta;
    const text = ta.value;
    const images = chat.images;
    const videos = chat.videos.slice();
    const { host, id } = chat;
    if ((!text.trim() && !images.length && !videos.length) || chat.sending || !chatLive()) return;
    if (askShown() && chat.askForm) {
      if (images.length || videos.length) {
        toast(`Answer the questions first, then send the ${images.length ? 'images' : 'videos'}`, 'warn');
        return;
      }
      // In place of answers: the agent gets the text instead.
      if (await sendAnswer(chat.askForm, { decline: text }) && ta.value === text) {
        ta.value = '';
        C.comp.fit();
        saveDraft(host, id, '');
      }
      return;
    }
    const problem = imagesProblem(images) || videosProblem(videos);
    if (problem) {
      toast(problem, 'warn');
      return;
    }
    const seq = chat.seq;
    chat.sending = true;
    chat.held = false;
    C.comp.update();
    let held = false;
    try {
      // The daemon holds Enter back if the agent shows a dialog, where
      // Enter picks the highlighted option whatever was typed.
      const body = { text: withVideos(text, videos), submit: true };
      let r;
      if (images.length) {
        body.images = images.map((im) => im.data);
        C.comp.progress(0, images.length);
        r = await apiUpload('POST', chatAPI('input'), body, (frac) => {
          if (seq === chat.seq) C.comp?.progress(frac, images.length);
        });
      } else r = await api('POST', chatAPI('input'), body);
      held = !!(r && r.held);
      if (seq === chat.seq || loadDraft(host, id) === text) saveDraft(host, id, '');
      if (seq === chat.seq) {
        ta.value = '';
        C.comp.fit();
        chat.images = chat.images.filter((im) => !images.includes(im));
        chat.videos = chat.videos.filter((v) => !videos.includes(v));
        C.comp.paint();
        C.scroll.scrollTop = C.scroll.scrollHeight;
      }
    } catch (e) {
      toast(`Could not send: ${e.message}`, 'error');
    }
    if (seq !== chat.seq) return;
    C.comp?.progress(null);
    chat.sending = false;
    chat.held = held;
    renderDock();
    if (keyboard()) ta.focus();
  }

  // queueMessage queues the text in the message box (POST .../queue): the
  // daemon types it in once the agent is done with its turn and anything
  // queued before it.
  async function queueMessage() {
    if (!C.comp || chat.queueing || !chatLive() || askShown()) return;
    const ta = C.comp.ta;
    const text = ta.value;
    const videos = chat.videos.slice();
    const { host, id, seq } = chat;
    if (!text.trim() && !videos.length) return;
    if (chat.images.length) {
      toast('Images cannot be queued: send them now, or remove them', 'warn');
      return;
    }
    const problem = videosProblem(videos);
    if (problem) {
      toast(problem, 'warn');
      return;
    }
    chat.queueing = true;
    C.comp.update();
    try {
      const r = await api('POST', chatAPI('queue'), { add: withVideos(text, videos) });
      if (seq === chat.seq || loadDraft(host, id) === text) saveDraft(host, id, '');
      if (seq === chat.seq) {
        ta.value = '';
        C.comp.fit();
        chat.videos = chat.videos.filter((v) => !videos.includes(v));
        C.comp.paint();
      }
      if (r && r.agent) putAgent(host, r.agent);
    } catch (e) {
      toast(`Could not queue: ${e.message}`, 'error');
    }
    if (seq !== chat.seq) return;
    chat.queueing = false;
    renderDock();
    if (keyboard()) ta.focus();
  }

  // editQueue removes a queued message ({remove: id}) or resumes the
  // queue ({paused: false}).
  async function editQueue(body) {
    const { host } = chat;
    try {
      const r = await api('POST', chatAPI('queue'), body);
      if (r && r.agent) putAgent(host, r.agent);
    } catch (e) {
      toast(`Could not ${body.remove ? 'remove it' : 'resume the queue'}: ${e.message}`, 'error');
    }
  }

  // renderQueue shows the messages queued for the agent above the box.
  function renderQueue() {
    if (!C.queue) return;
    const a = chat.agent;
    const q = (chatLive() && a.queue) || [];
    const key = JSON.stringify([q, a && a.queuePaused, a && a.adapter]);
    if (key === C.queueKey) return;
    C.queueKey = key;
    if (!q.length) {
      fill(C.queue);
      return;
    }
    const short = AGENT_NAMES[a.adapter] || 'the agent';
    const resume = a.queuePaused ? h('button', { type: 'button', class: 'btn sm', title: 'Stop pauses the queue: go on sending it', onclick: () => editQueue({ paused: false }) }, icon('play'), 'Resume') : null;
    fill(C.queue, h('section', { class: 'queue' + (a.queuePaused ? ' paused' : ''), 'aria-label': 'Queued messages' },
      h('div', 'queue-h', icon('queue'),
        h('span', 'grow', `${q.length} queued · `, a.queuePaused ? 'paused' : `the next goes in once ${short} is done`), resume),
      h('ol', 'queue-list', q.map((m, i) => h('li', 'queue-item',
        h('span', 'queue-n', String(i + 1)),
        h('span', { class: 'queue-text', title: m.text }, m.text),
        iconBtn('x', `Remove “${m.text.length > 40 ? m.text.slice(0, 40) + '…' : m.text}” from the queue`, () => editQueue({ remove: m.id })))))));
  }

  async function sendKey(key) {
    if (!chatLive()) return;
    chat.held = false;
    try {
      await api('POST', chatAPI('input'), { keys: [key] });
    } catch (e) {
      toast(`Could not send the key: ${e.message}`, 'error');
      return;
    }
    renderDock();
    setTimeout(refreshScreen, 150);
  }

  // interruptChat interrupts the agent's turn as Esc in its terminal does
  // (POST .../interrupt): the daemon waits for the transcript to record it
  // and makes the agent idle. A daemon without that route gets Esc.
  async function interruptChat() {
    if (!chatBusy() || chat.interrupting) return;
    const { host, seq } = chat;
    chat.interrupting = true;
    C.comp?.update();
    renderChatStatus();
    let r = null;
    try {
      r = await api('POST', chatAPI('interrupt'));
    } catch (e) {
      if (e.status === 404 || e.status === 405) {
        r = await api('POST', chatAPI('input'), { keys: ['escape'] }).then(() => ({ interrupted: true }), (e2) => ({ error: e2 }));
      } else r = { error: e };
    }
    if (seq !== chat.seq) return;
    chat.interrupting = false;
    if (r.error) toast(`Could not interrupt: ${r.error.message}`, 'error');
    else if (!r.interrupted) toast('Esc was pressed, but the agent shows no interruption yet', 'warn');
    if (r.agent) putAgent(host, r.agent);
    renderDock();
    setTimeout(refreshScreen, 150);
  }

  // ---- questions
  //
  // Questions the agent asks in a dialog of choices (Claude's
  // AskUserQuestion), as a form above the message box: each chat reply
  // lists those it waits on (asks), POST .../answer answers one. The dialog
  // stays open in the terminal too; whichever answers first wins. A message
  // sent while the form shows replies in place of answers.

  const askShown = () => (chatLive() && chat.asks[0]) || null;

  function chatAsks(asks) {
    asks = (asks || []).filter((a) => !chat.answered.has(a.id));
    if (asks.map((a) => a.id).join(',') === chat.asks.map((a) => a.id).join(',')) return;
    chat.asks = asks;
    renderDock();
  }

  function renderChatAsk() {
    if (!C.ask) return;
    const a = askShown();
    if (!a) {
      if (chat.askForm) fill(C.ask);
      chat.askForm = null;
      return;
    }
    // Built once per question: a reply must not undo the picks.
    if (!chat.askForm || chat.askForm.id !== a.id) {
      chat.askForm = askForm(a);
      fill(C.ask, chat.askForm.el);
    }
    paintAsk();
  }

  // askForm builds the form of a question: per question its options, one to
  // type, a preview of the option looked at, and a note.
  function askForm(a) {
    const form = { id: a.id, sending: false, qs: [] };
    const body = h('div', 'ask-body');
    a.questions.forEach((q, qi) => {
      const name = `ask-${a.id}-${qi}`;
      const type = q.multiSelect ? 'checkbox' : 'radio';
      const fq = { q, opts: [], other: null, note: null };
      const opts = h('div', 'opts');
      // The preview of the option looked at, else of the one picked. Its
      // box keeps its size: options moving under the pointer would take
      // the click meant for another.
      const hasPreview = (q.options || []).some((o) => o.preview);
      const preview = h('pre', 'opt-preview');
      preview.hidden = !hasPreview;
      const showPreview = (o) => {
        if (!hasPreview) return;
        const p = (o && o.preview) || (fq.opts.find((x) => x.input.checked && x.preview) || {}).preview;
        preview.textContent = p || 'Point at an option to see its preview.';
        preview.classList.toggle('empty-p', !p);
      };
      for (const o of q.options || []) {
        const input = h('input', { type, name, value: o.label, onchange: () => { showPreview(o.preview ? o : null); paintAsk(); } });
        const lab = h('label', 'opt', input, h('span', null, h('span', 'opt-l', o.label), o.description ? h('span', 'opt-d', o.description) : null));
        if (o.preview) {
          lab.addEventListener('mouseenter', () => showPreview(o));
          lab.addEventListener('mouseleave', () => showPreview(null));
          lab.addEventListener('focusin', () => showPreview(o));
        }
        fq.opts.push({ label: o.label, input, preview: o.preview || '' });
        opts.append(lab);
      }
      // One of their own, as in the terminal's "Type something".
      const oin = h('input', { type, name, 'aria-label': 'Your own answer' });
      const otext = h('input', { class: 'input', type: 'text', maxlength: 8000, placeholder: 'Something else…', 'aria-label': `Your own answer to: ${q.question}` });
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
      opts.append(h('label', 'opt own', oin, otext));
      const note = h('textarea', { class: 'input', rows: 2, maxlength: 8000, placeholder: 'A note for the agent on this answer', 'aria-label': `Note on: ${q.question}`, hidden: true });
      const addNote = h('button', { type: 'button', class: 'btn ghost sm ask-note-btn', onclick: () => { note.hidden = false; addNote.hidden = true; note.focus(); } }, icon('plus'), 'Note');
      fq.note = note;
      body.append(h('fieldset', 'ask-fs',
        h('legend', 'ask-q', q.header ? h('span', 'ask-tag', q.header) : null, h('span', null, q.question), q.multiSelect ? h('span', 'ask-multi', 'pick any') : null),
        opts, preview, addNote, note));
      showPreview(null);
      form.qs.push(fq);
    });
    form.progress = h('span', 'prog');
    form.send = h('button', { type: 'button', class: 'btn primary sm', onclick: submitAsk }, 'Answer');
    form.more = h('span', 'more');
    const who = chat.agent && chat.agent.adapter === 'claude' ? 'Claude asks' : 'The agent asks';
    form.el = h('section', { class: 'ask', 'aria-label': 'Questions from the agent' },
      h('div', 'ask-h', icon('alert'), who, form.more),
      body,
      h('div', 'ask-foot', h('span', 'hint', 'Answer here or in the terminal, or reply below instead'), form.progress, form.send));
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
    form.more.textContent = chat.asks.length > 1 ? `${chat.asks.length - 1} more after this` : '';
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
      toast(e.status === 404 ? 'That question was answered already, or is gone' : `Could not answer: ${e.message}`, 'error');
      if (e.status !== 404) {
        form.sending = false;
        if (seq === chat.seq) paintAsk();
        return false;
      }
    }
    if (seq !== chat.seq) return ok;
    chat.answered.add(form.id);
    chat.asks = chat.asks.filter((x) => x.id !== form.id);
    renderDock();
    return ok;
  }

  // ---- images and other files sent with a message
  //
  // Images: the daemon pastes each into the agent's prompt, where Claude and
  // Codex show it as [Image #n]. Big photos are scaled down here (the models
  // shrink them anyway) to keep requests small; formats the agents cannot
  // read (HEIC, …) are re-encoded as JPEG. Any other file (a video, a PDF,
  // an archive, …), and an image this browser cannot read, does not go with
  // a message: each is uploaded to the agent's machine as soon as it is
  // picked (POST .../uploads, streamed), and the message gets a line with
  // its path there, for the agent to open with its own tools. They share
  // the videos list; an entry's kind says what it is.

  const MAX_IMAGES = 10;
  const IMAGE_SIDE = 2000; // longest side, px
  const IMAGE_BYTES = 3.5 * 1024 * 1024; // the daemon's limit per image
  const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
  const MAX_VIDEOS = 10;
  const VIDEO_BYTES = 4 * 1024 ** 3; // the daemon's limit per file
  const VIDEO_EXT = /\.(?:mp4|m4v|mov|webm|mkv|avi|3gp|mpe?g|ogv|wmv|flv)$/i;

  const isImage = (f) => f.type.startsWith('image/');
  // Some browsers give no type for some videos (MKV): the name tells.
  const isVideo = (f) => f.type.startsWith('video/') || (!f.type && VIDEO_EXT.test(f.name || ''));
  const isPDF = (f) => f.type === 'application/pdf' || /\.pdf$/i.test(f.name || '');
  // isUpload: a file that goes up to the agent's machine (addVideos).
  const isUpload = (f) => !isImage(f);
  // uploadKind names what f is in the line the message gets for it.
  const uploadKind = (f) => (isVideo(f) ? 'Video' : isPDF(f) ? 'PDF' : 'File');
  const UPLOAD_NAME = { Video: 'video', PDF: 'document.pdf', File: 'file' };
  // mediaFiles is the files pasted or dropped, if any.
  const mediaFiles = (dt) => [...(dt ? dt.files : [])];
  const chatUploads = () => (chat.host ? `/api/hosts/${encodeURIComponent(chat.host)}/uploads` : '/api/uploads');

  async function addChatFiles(files) {
    if (!chatLive() || chat.sending || askShown()) return;
    const seq = chat.seq;
    const upload = (fs) => addVideos(fs, chat.videos, chatUploads(), chat.host ? hostName(chat.host) : '', () => {
      if (seq === chat.seq) C.comp?.paint();
    });
    upload(files);
    await readImages(files, chat.images, () => seq === chat.seq, () => C.comp?.paint(), upload);
  }

  // A file dropped anywhere on the chat goes with the next message; it
  // must not open in the tab.
  function wireChatDrop(el) {
    el.addEventListener('dragover', (ev) => {
      if (ev.dataTransfer && [...ev.dataTransfer.types].includes('Files') && chatLive()) ev.preventDefault();
    });
    el.addEventListener('drop', (ev) => {
      if (!ev.dataTransfer || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      addChatFiles(ev.dataTransfer.files);
    });
  }

  // readImages adds the image files among files to images, up to
  // MAX_IMAGES in all: each at once, without data while it is read (and
  // perhaps scaled down), then ready to send. changed() runs after each
  // step; upload([f]) takes an image this browser cannot read. It stops
  // once alive() is false: the message they were for is gone.
  async function readImages(files, images, alive, changed, upload) {
    const picked = [];
    for (const f of [...files].filter(isImage)) {
      if (images.length >= MAX_IMAGES) {
        toast(`At most ${MAX_IMAGES} images per message`, 'warn');
        break;
      }
      const im = { data: '', url: '', name: f.name || 'pasted image', file: f };
      images.push(im);
      picked.push(im);
    }
    if (!picked.length) return;
    changed();
    for (const im of picked) {
      try {
        const url = await blobURL(await fitImage(im.file));
        if (!alive()) return;
        Object.assign(im, { data: url.slice(url.indexOf(',') + 1), url });
      } catch (e) {
        if (!alive()) return;
        const i = images.indexOf(im);
        if (i >= 0) images.splice(i, 1);
        if (e.unreadable && upload) upload([im.file]);
        else toast(`${im.name}: ${e.message}`, 'error');
      }
      im.file = null;
      changed();
    }
  }

  function imagesProblem(images) {
    return images.some((im) => !im.data) ? 'Wait for the images to be ready.' : '';
  }

  // sendBar shows how far the images going with a message or a start have
  // gone up: set(frac, n) for n images, frac 0 to 1 (1: all sent, the reply
  // awaited); set(null) hides it.
  function sendBar() {
    const lbl = h('span', 'lbl');
    const done = h('span');
    const bar = h('span', 'bar', done);
    const el = h('div', { class: 'send-prog', hidden: true, role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100' }, lbl, bar);
    return {
      el,
      set(frac, n = 1) {
        el.hidden = frac == null;
        if (frac == null) return;
        const pct = Math.round(frac * 100);
        const what = n === 1 ? 'the image' : `${n} images`;
        lbl.textContent = frac < 1 ? `Uploading ${what} · ${pct}%` : `Uploaded ${what} · delivering…`;
        el.setAttribute('aria-valuenow', String(pct));
        el.setAttribute('aria-valuetext', lbl.textContent);
        bar.classList.toggle('wait', frac >= 1);
        done.style.width = pct + '%';
      },
    };
  }

  // fitImage returns f as is if the agents can read it and it is small
  // enough, else scaled down and re-encoded (GIFs lose their animation).
  async function fitImage(f) {
    let bmp;
    try {
      bmp = await createImageBitmap(f);
    } catch {
      throw Object.assign(new Error('this browser cannot read the image'), { unreadable: true });
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

  // addVideos starts uploading the files among files that are no images
  // (all of them if all) to url, the uploads route of the agent's machine
  // (dest names it if it is another one), adding each to videos as {name,
  // size, sent, path, error, xhr, dest, kind}; changed() runs when one is
  // added, done or failed.
  function addVideos(files, videos, url, dest, changed, all = false) {
    let added = false;
    for (const f of [...files].filter((f) => all || isUpload(f))) {
      const kind = uploadKind(f);
      if (videos.length >= MAX_VIDEOS) {
        toast(`At most ${MAX_VIDEOS} files per message`, 'warn');
        break;
      }
      if (f.size > VIDEO_BYTES) {
        toast(`${f.name || UPLOAD_NAME[kind]}: too large (at most 4 GB)`, 'error');
        continue;
      }
      if (!f.size) {
        toast(`${f.name || UPLOAD_NAME[kind]}: empty, or a folder`, 'error');
        continue;
      }
      const v = { name: f.name || UPLOAD_NAME[kind], size: f.size, sent: 0, path: '', error: '', xhr: null, dest, kind };
      v.xhr = uploadFile(url, f, (sent) => {
        v.sent = sent;
        paintVideo(v);
      }, (err, r) => {
        v.xhr = null;
        if (err) v.error = err;
        else v.path = r.path;
        changed();
      });
      videos.push(v);
      added = true;
    }
    if (added) changed();
  }

  // uploadFile posts file to url (XHR, for its progress), named after it.
  // done gets an error message, or '' and the decoded reply.
  function uploadFile(url, file, progress, done) {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url + '?' + new URLSearchParams({ name: file.name || UPLOAD_NAME[uploadKind(file)] }));
    const token = getToken();
    if (token) xhr.setRequestHeader('Authorization', 'Bearer ' + token);
    xhr.upload.onprogress = (ev) => progress(ev.loaded);
    xhr.upload.onload = () => progress(file.size);
    xhr.onload = () => {
      let data = null;
      try {
        data = JSON.parse(xhr.responseText);
      } catch {
        // not JSON
      }
      if (xhr.status === 401) lostAdmin();
      if (xhr.status >= 200 && xhr.status < 300 && data && data.path) done('', data);
      else done((data && data.error) || `upload failed (${xhr.status})`);
    };
    xhr.onerror = () => done('cannot reach the daemon');
    xhr.send(file);
    return xhr;
  }

  // dropVideos stops the uploads of files still on their way and returns
  // an empty list. What has arrived stays on the machine until pruned.
  function dropVideos(videos) {
    for (const v of videos || []) if (v.xhr) v.xhr.abort();
    return [];
  }

  function videosProblem(videos) {
    if (videos.some((v) => v.error)) return 'A file could not be uploaded: remove it.';
    if (videos.some((v) => !v.path)) return 'Wait for the uploads to finish.';
    return '';
  }

  // withVideos is text with a line for each uploaded file's path appended.
  function withVideos(text, videos) {
    if (!videos.length) return text;
    const lines = videos.map((v) => `${v.kind}: ${v.path}`).join('\n');
    return text.trim() ? text.trimEnd() + '\n\n' + lines : lines;
  }

  // renderThumbs shows the images and other files to send in box, each with a
  // button to remove it (data-image or data-video is its index).
  function renderThumbs(box, images, videos = []) {
    box.hidden = !images.length && !videos.length;
    const remove = (kind, i, name) => h('button', { type: 'button', class: 'icon-btn', ['data-' + kind]: String(i), 'aria-label': `Remove ${name}` }, icon('x'));
    fill(box, images.map((im, i) => h('div', im.data ? 'att' : 'att wait', im.data ? h('img', { src: im.url, alt: im.name, title: im.name })
      : h('span', { class: 'spin', role: 'img', 'aria-label': `Preparing ${im.name}`, title: `Preparing ${im.name}` }), remove('image', i, im.name))),
      videos.map((v, i) => {
        v.statusEl = h('span', 'st');
        v.barEl = h('span');
        const el = h('div', { class: 'att video', title: v.path || v.name }, h('span', 'nm', (v.kind === 'Video' ? '▶ ' : '▤ ') + v.name), v.statusEl, h('span', 'bar', v.barEl), remove('video', i, v.name));
        paintVideo(v);
        return el;
      }));
  }

  // paintVideo shows how far v's upload (a file other than an image) is.
  function paintVideo(v) {
    if (!v.statusEl) return;
    const pct = v.path ? 100 : v.size ? Math.min(99, Math.floor((v.sent / v.size) * 100)) : 0;
    // All sent from here: the daemon still stores it, or passes it on.
    const after = !v.path && !v.error && v.sent >= v.size;
    v.statusEl.textContent = v.error ? v.error : v.path ? `${fmtSize(v.size)} · uploaded`
      : after ? (v.dest ? `sending on to ${v.dest}…` : 'saving…') : `uploading ${pct}%`;
    v.statusEl.className = 'st' + (v.error ? ' bad' : '');
    v.barEl.style.width = (v.error ? 0 : after ? 100 : pct) + '%';
    v.barEl.parentNode?.classList.toggle('wait', after);
  }

  // ------------------------------------------------------------------ feeds
  //
  // A feed shows one transcript in a log: its latest entries first, then new
  // ones as they are written (long polls), older ones on request. The
  // session's conversation is one, a workflow agent's another. The tool
  // calls between two messages fold into one line, a group, open while the
  // agent works on them.

  // newFeed makes a feed. o holds its scroller and log elements; url(query)
  // of its chat requests; live() while more may be written; working() while
  // its agent works (a call without output yet is running then); empty(),
  // the note for no transcript; imageURL(query) of its images; and
  // optionally query(q) to add to its long polls, onReply(reply),
  // onStatus() when its error or entries changed, and cacheKey, the agent
  // key its copy is kept under (see the cache) with onCopy(copy) when it
  // shows one.
  function newFeed(o) {
    return Object.assign({
      seq: 0, // bumped on start and stop: loops of an old start stop
      file: '', // transcript the offsets belong to; '' = none yet
      start: 0, // offset of the oldest entry shown
      end: 0, // offset after the newest
      loaded: false,
      earlier: false, // loading older entries
      error: '',
      tools: new Map(), // tool call id -> step (or a card that shows a call)
      orphans: new Map(), // tool call id -> {t, output, error, images}: a result shown before its call
      watch: new Set(), // repaints of the nodes that show workflow runs (see applyWorkflows)
      group: null, // the group the next tool call joins
      groups: [], // every group shown
      day: '', // the day of the last message shown
      last: null, // {el, ts} of the newest message shown, which stamp marks
      stamp: null, // the time under it
      catchup: false, // a copy shows: the next request asks for what came after it, without waiting
    }, o);
  }

  function startFeed(f) {
    stopFeed(f);
    Object.assign(f, { file: '', start: 0, end: 0, loaded: false, earlier: false, error: '', group: null, groups: [], day: '', last: null, catchup: false });
    fill(f.log, h('p', 'empty-note wait', 'Loading…'));
    feedLoop(f, f.seq);
  }

  function stopFeed(f) {
    if (!f) return;
    f.seq++;
    f.tools.clear();
    f.orphans.clear();
    f.watch.clear();
  }

  // feedLoop follows a transcript until the feed stops or the session
  // leaves: the latest entries first, then long polls for more.
  async function feedLoop(f, seq) {
    if (f.cacheKey) {
      // From memory, before the page paints: no "Loading…" in between.
      let copy = feedCopy(f.cacheKey);
      if (copy && copy.then) copy = await copy;
      if (seq !== f.seq) return;
      if (copy && copy.file) {
        feedApply(f, copy, true);
        f.loaded = true;
        f.catchup = true;
        if (f.onCopy) f.onCopy(copy);
      }
    }
    let failures = 0;
    while (chat.open && seq === f.seq) {
      const q = new URLSearchParams();
      if (f.loaded) {
        q.set('file', f.file);
        q.set('after', String(f.end));
        if (!f.catchup) q.set('wait', '1');
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
      if (f.catchup) {
        f.catchup = false;
        // Far behind: the latest entries instead, as for a first look.
        if (r.more) {
          f.loaded = false;
          continue;
        }
      }
      const first = !f.loaded;
      f.loaded = true;
      if (f.onReply) f.onReply(r);
      if (f.cacheKey) keepFeed(f.cacheKey, r, first);
      feedApply(f, r, first);
      // A finished transcript read to the end is done.
      if (!first && !r.more && !r.entries.length && !f.live()) return;
    }
  }

  function feedApply(f, r, first) {
    const { log, scroller } = f;
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
    f.file = r.file || '';
    f.end = r.end || 0;
    if (r.reset || first) {
      f.start = r.start || 0;
      f.tools.clear();
      f.orphans.clear();
      f.watch.clear();
      Object.assign(f, { group: null, groups: [], day: '', last: null });
      fill(log, feedHead(f), entryNodes(f, r.entries));
      stampLast(f);
      if (f.onStatus) f.onStatus();
      scroller.scrollTop = scroller.scrollHeight;
      return;
    }
    const nodes = entryNodes(f, r.entries);
    if (nodes.length) {
      log.querySelector('[data-empty]')?.remove();
      log.append(...nodes);
    }
    stampLast(f);
    if (f.onStatus) f.onStatus();
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  }

  // stampLast puts when the newest message arrived under it.
  function stampLast(f) {
    const l = f.last;
    if (!l || !l.ts) {
      if (f.stamp) f.stamp.remove();
      return;
    }
    if (!f.stamp || Number(f.stamp.dataset.ts) !== l.ts) {
      if (f.stamp) f.stamp.remove();
      f.stamp = relTime(l.ts, 'msg-time', true);
    }
    f.stamp.classList.toggle('mine', l.el.classList.contains('msg-user'));
    if (l.el.nextElementSibling !== f.stamp) l.el.after(f.stamp);
  }

  // stickToBottom keeps a conversation at its end, when it was there, as
  // it gets shorter (a question or a dialog opens under it, a keyboard) or
  // its content taller (images load). Not right after a click or a key in
  // it: what that unfolded stays in view. A dock floats over the
  // scroller's end; the conversation makes room for it (--dock-h).
  function stickToBottom(scroller, dock) {
    let end = true;
    let touched = 0;
    scroller.addEventListener('scroll', () => {
      end = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
    }, { passive: true });
    for (const type of ['pointerdown', 'keydown']) scroller.addEventListener(type, () => (touched = Date.now()), { passive: true });
    const ro = new ResizeObserver(() => {
      if (dock) scroller.style.setProperty('--dock-h', dock.offsetHeight + 'px');
      if (end && Date.now() - touched > 600) scroller.scrollTop = scroller.scrollHeight;
    });
    ro.observe(scroller);
    if (scroller.firstElementChild) ro.observe(scroller.firstElementChild);
    if (dock) ro.observe(dock);
  }

  // feedHead is what goes above the entries: "load earlier", or a note
  // when there is nothing to show.
  function feedHead(f) {
    if (f.start > 0) {
      return [h('button', { type: 'button', class: 'btn sm load-earlier', onclick: (ev) => loadEarlier(f, ev) }, 'Load earlier messages')];
    }
    if (!f.file) return [h('p', { class: 'empty-note', 'data-empty': '1' }, f.empty())];
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
      toast(`Could not load earlier messages: ${e.message}`, 'error');
    }
    f.earlier = false;
    if (seq !== f.seq) return;
    if (!r || r.reset || r.file !== f.file) {
      btn.disabled = false;
      btn.textContent = 'Load earlier messages';
      return;
    }
    const scroller = f.scroller;
    const fromBottom = scroller.scrollHeight - scroller.scrollTop;
    f.start = r.start || 0;
    // The page gets its own groups and days; results in it attach to the
    // calls in it; later results whose call is in it already showed on
    // their own.
    const keep = { tools: f.tools, group: f.group, day: f.day, last: f.last };
    Object.assign(f, { tools: new Map(), group: null, day: '' });
    const nodes = entryNodes(f, r.entries);
    if (f.group) f.group.closed = true;
    for (const t of f.tools.values()) {
      if (!t.done && t.g) {
        t.done = true;
        t.stale = true;
        paintStep(t);
      }
    }
    for (const g of f.groups) paintGroup(g);
    for (const [k, v] of keep.tools) f.tools.set(k, v);
    f.group = keep.group;
    const lastDay = [...nodes].reverse().find((n) => n.classList && n.classList.contains('day'));
    f.day = keep.day;
    f.last = keep.last || f.last;
    btn.replaceWith(...feedHead(f), ...nodes);
    stampLast(f);
    // The page's last day and the next one shown may be the same.
    if (lastDay) {
      let n = lastDay.nextElementSibling;
      while (n && !n.classList.contains('day')) n = n.nextElementSibling;
      if (n && n.textContent === lastDay.textContent) n.remove();
    }
    scroller.scrollTop = scroller.scrollHeight - fromBottom;
  }

  // entryNodes renders entries: a tool call joins the open group (its node
  // is returned when the group starts), a result attaches to its call.
  function entryNodes(f, entries) {
    const out = [];
    for (const e of entries || []) out.push(...entryNode(f, e));
    return out;
  }

  const timeTitle = (el, e) => {
    if (e.ts) el.title = new Date(e.ts).toLocaleString();
    return el;
  };

  function closeGroup(f) {
    const g = f.group;
    if (!g) return;
    f.group = null;
    g.closed = true;
    paintGroup(g);
  }

  function dayLabel(ts) {
    const d = new Date(ts);
    const today = new Date();
    if (d.toDateString() === today.toDateString()) return 'Today';
    if (d.toDateString() === new Date(today.getTime() - DAY).toDateString()) return 'Yesterday';
    return d.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' });
  }

  // The notes that mark a point in the conversation, drawn as a rule.
  const MARKS = { interrupted: 'Interrupted', compacted: 'Conversation compacted' };

  function entryNode(f, e) {
    const main = f === chat.feed;
    switch (e.kind) {
      case 'title':
        return [];
      case 'tool': {
        if (e.name === 'Workflow' && main) {
          closeGroup(f);
          return [workflowNode(f, e)];
        }
        if ((e.name === 'Agent' || e.name === 'Task') && main) {
          closeGroup(f);
          return [agentCallNode(f, e)];
        }
        const fresh = !f.group;
        const g = f.group || newGroup(f);
        addStep(g, e);
        return fresh ? [g.el] : [];
      }
      case 'result': {
        const t = e.id && f.tools.get(e.id);
        if (t) {
          setToolOutput(t, e.output, e.error, e.images, e.ts);
          return [];
        }
        // The call is before the loaded range (or, rarely, written after
        // its result): shown on its own until the call turns up.
        const fresh = !f.group;
        const g = f.group || newGroup(f);
        const o = addStep(g, { kind: 'tool', name: 'result', text: oneLine(e.output) || (e.images ? 'image' : ''), output: e.output, error: e.error, images: e.images, ts: e.ts });
        if (e.id) f.orphans.set(e.id, { t: o, output: e.output, error: e.error, images: e.images });
        return fresh ? [g.el] : [];
      }
      case 'user': {
        closeGroup(f);
        if (!main) return [taskNode(e)];
        const out = [];
        if (e.ts) {
          const day = dayLabel(e.ts);
          if (day !== f.day) {
            f.day = day;
            out.push(h('div', 'day', day));
          }
        }
        const el = timeTitle(h('div', 'msg-user',
          e.images && e.images.length ? imagesNode(f, e.images) : null,
          e.text ? h('div', 'bubble', e.text) : null), e);
        f.last = { el, ts: e.ts };
        out.push(el);
        return out;
      }
      case 'assistant': {
        closeGroup(f);
        const el = timeTitle(md(e.text || ''), e);
        f.last = { el, ts: e.ts };
        return [el];
      }
      case 'task':
        closeGroup(f);
        return [taskEndNode(f, e)];
      default: {
        closeGroup(f);
        const mark = MARKS[e.text];
        return [timeTitle(h('div', mark ? 'meta-line' : 'note-line', mark || e.text || ''), e)];
      }
    }
  }

  const oneLine = (s) => ((s || '').split('\n').find((l) => l.trim()) || '').trim();

  // ---- groups of tool calls

  function newGroup(f) {
    const g = { f, steps: [], open: null, closed: false, t0: 0, t1: 0 };
    g.sum = h('button', { type: 'button', class: 'act-sum', 'aria-expanded': 'false', onclick: () => {
      g.open = !groupOpen(g);
      paintGroup(g);
    } });
    g.list = h('ul', 'steps');
    g.imgs = h('div', 'imgs');
    g.imgs.hidden = true;
    g.el = h('div', 'act', g.sum, g.list, g.imgs);
    f.group = g;
    f.groups.push(g);
    return g;
  }

  const groupLive = (g) => !g.closed && g.f.working() && g.steps.some((t) => !t.done);
  const groupOpen = (g) => (g.open !== null ? g.open : groupLive(g));

  function paintGroup(g) {
    const live = groupLive(g);
    const open = groupOpen(g);
    g.el.classList.toggle('live', live);
    g.list.hidden = !open;
    g.sum.setAttribute('aria-expanded', String(open));
    if (live) {
      const cur = [...g.steps].reverse().find((t) => !t.done) || g.steps[g.steps.length - 1];
      fill(g.sum, h('span', 'spin'), h('span', 'trunc', stepVerb(cur)), g.t0 ? elapsed(g.t0, 0, 'dur') : null);
      return;
    }
    const failed = g.steps.filter((t) => t.done && t.error).length;
    fill(g.sum, icon('chevron-right', 'chev'), h('span', 'trunc', summarize(g.steps)),
      g.t1 > g.t0 && g.t0 ? h('span', 'dur', '· ' + dur(g.t1 - g.t0)) : null,
      failed ? h('span', 'bad', `· ${failed} failed`) : null);
  }

  const READS = new Set(['Read', 'NotebookRead', 'read_file', 'view']);
  const EDITS = new Set(['Edit', 'MultiEdit', 'Write', 'NotebookEdit', 'edit', 'apply_patch', 'write_file']);
  const RUNS = new Set(['Bash', 'shell', 'exec_command', 'local_shell', 'BashOutput']);
  const FINDS = new Set(['Grep', 'Glob', 'LS', 'search', 'find', 'ToolSearch']);
  const WEB = new Set(['WebFetch', 'WebSearch', 'web_search', 'web_fetch']);

  // summarize sums up a group: "Read 3 files, searched twice, ran a command".
  function summarize(steps) {
    const n = (set) => steps.filter((t) => set.has(t.name)).length;
    const parts = [];
    const reads = n(READS);
    const finds = n(FINDS);
    const edits = n(EDITS);
    const runs = n(RUNS);
    const web = n(WEB);
    const other = steps.length - reads - finds - edits - runs - web;
    if (reads) parts.push(reads === 1 ? 'read a file' : `read ${reads} files`);
    if (finds) parts.push(finds === 1 ? 'searched once' : finds === 2 ? 'searched twice' : `searched ${finds} times`);
    if (edits) parts.push(edits === 1 ? 'edited a file' : `edited ${edits} files`);
    if (runs) parts.push(runs === 1 ? 'ran a command' : `ran ${runs} commands`);
    if (web) parts.push(web === 1 ? 'looked something up' : `looked up ${web} things`);
    if (other) parts.push(other === 1 ? `used ${steps.find((t) => ![READS, FINDS, EDITS, RUNS, WEB].some((s) => s.has(t.name))).name}` : `used ${other} more tools`);
    const s = parts.join(', ') || 'Worked';
    return s[0].toUpperCase() + s.slice(1);
  }

  function stepVerb(t) {
    if (!t) return 'Working…';
    const e = t.call;
    const arg = stepArg(e);
    const short = FILE_TOOLS.has(e.name) ? baseName(arg) || arg : arg;
    if (READS.has(e.name)) return 'Reading ' + short;
    if (EDITS.has(e.name)) return 'Editing ' + short;
    if (RUNS.has(e.name)) return 'Running ' + short;
    if (FINDS.has(e.name)) return 'Searching ' + short;
    if (WEB.has(e.name)) return 'Looking up ' + short;
    return `${e.name} ${short}`.trim();
  }

  // Tools whose summary is a file's path.
  const FILE_TOOLS = new Set(['Read', 'Write', 'Edit', 'MultiEdit', 'NotebookEdit', 'NotebookRead']);
  const TOOL_ICON = { Read: 'file', NotebookRead: 'file', Edit: 'pencil', MultiEdit: 'pencil', Write: 'pencil', NotebookEdit: 'pencil', Bash: 'terminal', shell: 'terminal', Grep: 'search', Glob: 'search', WebFetch: 'globe', WebSearch: 'globe', TodoWrite: 'checklist', Skill: 'sliders' };

  // relPath shortens a path in the session's folder to the part in it, so
  // the file's name shows.
  function relPath(p) {
    const cwd = chat.agent && chat.agent.cwd;
    return cwd && p && p.startsWith(cwd + '/') ? p.slice(cwd.length + 1) : shortPath(p);
  }

  const stepArg = (e) => (FILE_TOOLS.has(e.name) ? relPath(e.text) : e.text || '');

  // diffCount counts the lines a diff adds and removes.
  function diffCount(text) {
    let add = 0;
    let del = 0;
    for (const l of (text || '').split('\n')) {
      if (l.startsWith('+') && !l.startsWith('+++')) add++;
      else if (l.startsWith('-') && !l.startsWith('---')) del++;
    }
    return [add, del];
  }

  function addStep(g, e) {
    const f = g.f;
    const t = { f, g, call: e, name: e.name, done: false, stale: false, output: null, error: false, images: null, built: false, open: false };
    t.status = h('span', 'st');
    t.meta = h('span', 'step-meta');
    t.row = h('button', { type: 'button', class: 'step-row', 'aria-expanded': 'false', onclick: () => {
      t.open = !t.open;
      if (t.open) buildTool(t);
      t.body.hidden = !t.open;
      t.row.setAttribute('aria-expanded', String(t.open));
    } }, t.status, h('span', 'step-name', e.name || 'tool'), h('span', 'step-arg', stepArg(e)), t.meta);
    timeTitle(t.row, e);
    t.body = h('div', 'step-detail');
    t.body.hidden = true;
    t.el = h('li', 'step', t.row, t.body);
    g.steps.push(t);
    g.list.append(t.el);
    if (e.ts) {
      if (!g.t0) g.t0 = e.ts;
      g.t1 = Math.max(g.t1, e.ts);
    }
    trackTool(f, t, e);
    paintStep(t);
    paintGroup(g);
    return t;
  }

  function paintStep(t) {
    const e = t.call;
    t.el.classList.toggle('err', t.done && t.error);
    if (!t.done) fill(t.status, t.f.working() ? h('span', 'spin') : h('span', { class: 'st-stale', title: 'No result: interrupted, or waiting' }));
    else fill(t.status, icon(TOOL_ICON[e.name] || (EDITS.has(e.name) ? 'pencil' : RUNS.has(e.name) ? 'terminal' : FINDS.has(e.name) ? 'search' : 'sliders')));
    let meta = '';
    if (!t.done) meta = t.f.working() ? 'running' : '';
    else if (t.error) meta = h('span', 'bad', 'failed');
    else if ((e.name === 'Edit' || e.name === 'MultiEdit' || e.name === 'edit') && e.detail) {
      const [a, d] = diffCount(e.detail);
      meta = [h('span', 'add', '+' + a), ' ', h('span', 'del', '−' + d)];
    } else if (e.name === 'Write') meta = 'written';
    fill(t.meta, meta);
  }

  // trackTool gives a call its output: its own, a result shown before the
  // call turned up, or (Claude) the result that follows.
  function trackTool(f, t, e) {
    const early = e.id && f.orphans.get(e.id);
    if (early) {
      f.orphans.delete(e.id);
      dropStep(early.t);
      f.tools.set(e.id, t);
      setToolOutput(t, early.output, early.error, early.images);
    } else if (e.id) {
      f.tools.set(e.id, t);
    } else if (e.output != null || e.error) {
      setToolOutput(t, e.output, e.error, e.images, e.ts);
    }
  }

  // dropStep removes step t (an orphan result whose call turned up).
  function dropStep(t) {
    if (!t || !t.g) return;
    const g = t.g;
    g.steps = g.steps.filter((x) => x !== t);
    t.el.remove();
    if (!g.steps.length) g.el.remove();
    else paintGroup(g);
  }

  // paintPending shows calls without output as running while the feed's
  // agent works, and as unfinished otherwise (interrupted, or waiting for a
  // permission).
  function paintPending(f) {
    for (const g of f.groups) {
      if (g.steps.some((t) => !t.done)) for (const t of g.steps) if (!t.done) paintStep(t);
      paintGroup(g);
    }
    for (const t of f.tools.values()) if (!t.g && t.paint) t.paint();
  }

  // setToolOutput gives a call what it returned. Images it returned show
  // under its group, folded or not.
  function setToolOutput(t, output, error, images, ts) {
    t.done = true;
    t.output = output || '';
    t.error = !!error;
    t.images = images && images.length ? images : null;
    if (t.onOutput) {
      t.onOutput();
      return;
    }
    if (ts && t.g) t.g.t1 = Math.max(t.g.t1, ts);
    paintStep(t);
    if (t.built) addOutput(t);
    if (t.images && t.g) {
      t.g.imgs.hidden = false;
      t.g.imgs.append(imagesNode(t.f, t.images));
    }
    if (t.g) paintGroup(t.g);
  }

  function buildTool(t) {
    if (t.built) return;
    t.built = true;
    // A Read of an image says all in its summary: the image is what counts.
    if (t.call.detail && !(t.images && t.name === 'Read')) t.body.append(inputView(t.call));
    if (t.done) addOutput(t);
  }

  function addOutput(t) {
    if (t.output == null) return;
    if (t.output) t.body.append(outputView(t));
    else if (!t.images && !t.body.childElementCount) t.body.append(h('p', 'note-line', 'No output'));
  }

  // Tools whose output is the agent's or a page's text, in markdown.
  const MD_OUTPUT = new Set(['Agent', 'Task', 'WebFetch', 'WebSearch']);

  // inputView shows a call's input: a command, a diff, a file's content, a
  // script, a plan, or JSON.
  function inputView(e) {
    const n = e.name;
    if (n === 'ExitPlanMode') return mdView(e.detail, 'Plan');
    const o = { label: 'Input', text: e.detail, wrap: true, max: true };
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
    const o = { label: t.error ? 'Error' : 'Output', text: out, error: t.error, wrap: true, max: true };
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

  // ---- images: pasted into a message, or returned by a tool
  //
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

  // imagesNode shows images of feed f's entries as thumbnails that open
  // full size.
  function imagesNode(f, images) {
    const row = h('div', 'imgs');
    for (const im of images) {
      const url = f.imageURL(new URLSearchParams({ file: f.file, line: String(im.line), n: String(im.n) }));
      const img = h('img', { alt: 'image' });
      const b = h('button', { type: 'button', class: 'img-btn', title: 'Open full size' }, 'image…');
      b.loadImage = () => loadImage(url).then((src) => {
        img.src = src;
        fill(b, img);
      }, (e) => {
        fill(b, 'image: ' + e.message);
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

  // ---- the workflow a call started, and background tasks that ended

  const WF = {
    running: ['working', 'Running'],
    completed: ['idle', 'Completed'],
    done: ['idle', 'Done'],
    failed: ['failed', 'Failed'],
    stopped: ['exited', 'Stopped'],
  };
  const wfPill = (status) => {
    const [cls, text] = WF[status] || WF.stopped;
    return h('span', 'pill ' + cls, status === 'running' ? h('span', 'spin') : h('span', 'dot'), text);
  };

  // workflowNode shows a Workflow call as the run it started, live from the
  // panel's data: its phases, the agents at work, its totals. Its output
  // names the run.
  function workflowNode(f, e) {
    const [name, ...desc] = (e.text || '').split(': ');
    const title = h('div', 'wf-name', name || 'workflow');
    const kind = h('div', 'wf-kind', 'Workflow');
    const pill = h('span', null);
    const time = h('span', 'wf-time');
    const about = h('p', 'wf-desc', desc.join(': '));
    const live = h('div');
    const out = h('div');
    const card = h('section', { class: 'wf', 'aria-label': 'Workflow' },
      h('div', 'wf-h', h('span', 'wf-ic', icon('workflow')), h('div', 'wf-title', title, kind), pill, time), about, live, out);
    timeTitle(card, e);
    const t = { el: card, name: e.name, call: e, done: false, output: null, error: false, run: '' };
    const paint = () => {
      const run = t.run && wf.runs.find((r) => r.id === t.run);
      if (!run) {
        fill(kind, t.error ? 'Workflow · did not start' : !t.done ? 'Workflow · launching…' : 'Workflow · starting…');
        return;
      }
      title.textContent = run.name;
      if (run.description) about.textContent = run.description;
      kind.textContent = run.status === 'running' && run.phase ? `Workflow · ${run.phase}` : 'Workflow';
      fill(pill, wfPill(run.status));
      fill(time, elapsed(run.startedMs, run.status === 'running' ? 0 : run.endedMs || run.updatedMs));
      card.classList.toggle('done', run.status !== 'running');
      fill(live, runLive(run));
    };
    t.onOutput = () => {
      const m = /\bRun ID: (wf_[\w-]+)/.exec(t.output);
      if (m) t.run = m[1];
      // A launch that failed, or said something else than expected.
      if (t.error || !m) fill(out, h('div', 'wf-out', codeView({ label: t.error ? 'Error' : 'Output', text: t.output || '(no output)', error: t.error, wrap: true, max: true })));
      paint();
    };
    if (e.detail) {
      const script = h('details', 'wf-script', h('summary', 'act-sum', icon('chevron-right', 'chev'), 'Script', h('span', 'dur', `· ${e.detail.split('\n').length} lines`)));
      script.addEventListener('toggle', () => {
        if (script.open && script.childElementCount === 1) script.append(h('div', 'wf-out', codeView({ text: e.detail, lang: 'js', tag: 'js', wrap: true, max: true })));
      });
      out.after(script);
    }
    trackTool(f, t, e);
    paint();
    f.watch.add(paint);
    return card;
  }

  // runLive is a run in the chat: its phases at a glance, the agents at
  // work (each opens its conversation), and its totals.
  function runLive(run) {
    const busy = run.agents.filter((a) => a.status === 'running');
    const fin = run.counts.done + run.counts.failed + run.counts.stopped;
    return [
      phaseStrip(run),
      busy.length ? h('ul', 'wf-agents', busy.slice(-3).map((a) => h('li', null, wfAgentBtn(run, a))),
        busy.length > 3 ? h('li', 'more-row', `+${busy.length - 3} more at work`) : null) : null,
      h('div', 'wf-foot',
        h('span', null, `${fin}/${run.agents.length} agents`),
        run.counts.failed ? h('span', 'del', `${run.counts.failed} failed`) : null,
        h('span', null, `${run.toolUses} tool calls`),
        h('span', null, `${fmtTokens(run.tokens)} tokens`),
        h('button', { type: 'button', class: 'btn ghost sm', onclick: () => openRun(run.id) }, 'Open run', icon('chevron-right', 'i-xs'))),
    ];
  }

  // agentCallNode is an Agent call with the subagent it started, live from
  // the panel's data: what it does now, a way to its conversation, and its
  // report once it is done.
  function agentCallNode(f, e) {
    const status = h('span');
    const what = h('span', 'what');
    const open = h('button', { type: 'button', class: 'btn ghost sm', hidden: true }, 'Open', icon('chevron-right', 'i-xs'));
    const report = h('div', 'sub-report');
    const card = h('div', 'sub-call', icon('bot'), h('div', 'grow', h('div', 'trunc', e.text || 'Subagent'), what), status, open);
    const box = timeTitle(h('div', 'act', card, report), e);
    const t = { el: box, name: e.name, call: e, done: false, output: null, error: false };
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
    t.paint = () => {
      const m = find();
      const sig = JSON.stringify([m && m.run.id, m && m.a.status, m && m.a.tool, m && m.a.activity, t.done, t.error]);
      if (sig === shown) return;
      shown = sig;
      if (m) {
        open.hidden = false;
        open.onclick = () => openSub(m.run.id, m.a.id);
        fill(status, wfPill(m.a.status));
        fill(what, m.a.status === 'running' ? [m.a.tool ? h('b', null, m.a.tool + ' ') : null, m.a.activity || 'starting…'] : '');
      } else {
        fill(status, t.done ? wfPill(t.error ? 'failed' : 'done') : chat.feed.working() ? wfPill('running') : null);
      }
    };
    t.onOutput = () => {
      t.paint();
      if (!t.output) return;
      const d = h('details', null, h('summary', 'act-sum', icon('chevron-right', 'chev'), t.error ? 'The error' : 'Its report'));
      d.addEventListener('toggle', () => {
        if (d.open && d.childElementCount === 1) d.append(t.error ? codeView({ label: 'Error', text: t.output, error: true, wrap: true, max: true }) : mdView(t.output, 'Report'));
      });
      fill(report, d);
    };
    trackTool(f, t, e);
    t.paint();
    f.watch.add(t.paint);
    return box;
  }

  // taskEndNode shows that a background task ended: a workflow run, with
  // what it returned, or a background command or agent.
  function taskEndNode(f, e) {
    const st = e.error ? 'failed' : e.name === 'completed' ? 'completed' : 'stopped';
    const call = e.id ? f.tools.get(e.id) : null;
    const runOf = () => (call && call.run && wf.runs.find((r) => r.id === call.run)) || wf.runs.find((r) => r.summary && r.summary === e.text);
    if (!(call && call.name === 'Workflow') && !runOf() && !/^Dynamic workflow\b/.test(e.text || '')) return bgTaskNode(e, st);
    const title = h('div', 'wf-name');
    const kind = h('div', 'wf-kind');
    const openBtn = h('button', { type: 'button', class: 'btn ghost sm', onclick: () => { const r = runOf(); openRun(r ? r.id : ''); } }, 'Open run', icon('chevron-right', 'i-xs'));
    const part = h('div');
    const box = timeTitle(h('section', { class: 'wf done' + (st === 'failed' ? ' failed' : ''), 'aria-label': 'Workflow ended' },
      h('div', 'wf-h', h('span', 'wf-ic ' + (st === 'failed' ? 'bad' : 'ok'), icon(st === 'failed' ? 'alert' : 'check')), h('div', 'wf-title', title, kind), openBtn), part), e);
    let shown = null;
    let folded = true;
    const paint = () => {
      const run = runOf();
      title.textContent = run ? run.name : call ? (call.call.text || '').split(': ')[0] : (/"(.*)"/.exec(e.text || '') || [])[1] || 'workflow';
      title.title = e.text || '';
      openBtn.hidden = !run;
      kind.textContent = `Workflow ${WF[st][1].toLowerCase()}` + (run ? ` · ${plural(run.agents.length, 'agent')} · ${dur((run.endedMs || run.updatedMs) - run.startedMs)}` : '');
      // The notification cuts the result at 8000 characters; the run has
      // all of it.
      const result = (run && run.result) || e.output || '';
      if (result === shown) return;
      shown = result;
      if (!result) {
        fill(part, st === 'failed' ? h('pre', 'result-err', e.text || '') : null);
        return;
      }
      const res = h('div', 'result' + (folded ? ' folded' : ''), resultNode(result));
      const long = result.length > 600 || result.split('\n').length > 8;
      const more = long ? h('div', 'result-more', h('button', { type: 'button', class: 'btn ghost sm', 'aria-expanded': String(!folded), onclick: (ev) => {
        folded = !folded;
        res.classList.toggle('folded', folded);
        ev.currentTarget.textContent = folded ? 'Show the whole result' : 'Show less';
        ev.currentTarget.setAttribute('aria-expanded', String(!folded));
      } }, folded ? 'Show the whole result' : 'Show less')) : null;
      if (!long) res.classList.remove('folded');
      fill(part, res, more);
    };
    paint();
    f.watch.add(paint);
    return box;
  }

  // bgTaskNode shows a background command or agent that ended, and what it
  // returned (an agent's report) when opened.
  function bgTaskNode(e, st) {
    const dot = h('span', 'dot' + (st === 'failed' ? ' bad' : st === 'stopped' ? ' off' : ''));
    if (!e.output) return timeTitle(h('div', 'bg-task', dot, h('span', null, e.text || '')), e);
    const d = h('details', null, h('summary', null, e.text || 'A background task ended'));
    d.addEventListener('toggle', () => {
      if (d.open && d.childElementCount === 1) d.append(mdView(e.output, 'Result'));
    });
    return timeTitle(h('div', 'bg-task', dot, d), e);
  }

  // taskNode shows what a workflow agent was told: written by the script,
  // often pages long, so folded to its start.
  function taskNode(e) {
    const text = e.text || '';
    const body = h('div', 'md', h('p', null, text));
    const box = h('div', 'answered', h('div', 'answered-l', 'Task'), body);
    if (text.length > 600 || text.split('\n').length > 8) {
      body.classList.add('folded-task');
      box.append(h('button', { type: 'button', class: 'btn ghost sm', onclick: (ev) => {
        ev.currentTarget.textContent = body.classList.toggle('folded-task') ? 'Show all' : 'Show less';
      } }, 'Show all'));
    }
    return timeTitle(box, e);
  }

  // resultNode draws a run's result: JSON (mostly objects and lists of
  // them, with markdown text) as labelled parts, text as markdown, and JSON
  // cut short as JSON.
  function resultNode(text) {
    try {
      return resultValue(JSON.parse(text), 0);
    } catch {
      return looksJSON(text) ? codeView({ text, lang: 'json', tag: 'json', wrap: true }) : md(text);
    }
  }

  function resultValue(x, depth) {
    if (typeof x === 'string') return md(x);
    if (x === null || typeof x !== 'object') return h('div', 'mono', String(x));
    if (depth > 2) return codeView({ text: JSON.stringify(x, null, 2), lang: 'json', tag: 'json', wrap: true });
    const box = h('div', depth ? 'res-part nested' : 'res-part');
    const labelOf = (v, i) => {
      const t = v && typeof v === 'object' && !Array.isArray(v) && [v.title, v.name, v.id, v.page, v.key].find((s) => typeof s === 'string' && s);
      return t || String(i + 1);
    };
    const parts = Array.isArray(x) ? x.map((v, i) => [labelOf(v, i), v]) : Object.entries(x);
    if (!parts.length) box.append(h('p', 'note-line', Array.isArray(x) ? 'An empty list' : 'Empty'));
    for (const [k, v] of parts) box.append(h('div', null, h('div', 'res-key', k), resultValue(v, depth + 1)));
    return box;
  }

  // ---------------------------------------------------------------- code

  // A clipped text ends with this note (transcript.Clip).
  const CLIP = /\n… \((\d+) more bytes\)$/;
  // A line of a Read output: its number, then a tab.
  const READ_LINE = /^ *(\d+)(?:\t|→)/;

  // codeView frames o.text as code, highlighted as o.lang ('diff': a diff,
  // its code highlighted as o.inner). o.read shows the line numbers of a
  // Read output in a gutter; o.wrap wraps long lines; o.max caps the
  // height; o.error tints it; o.label or o.tag heads it.
  function codeView(o) {
    let text = o.text || '';
    const clip = CLIP.exec(text);
    if (clip) text = text.slice(0, clip.index);
    let raw = text;
    let body;
    const rows = o.lang === 'diff' || o.read;
    if (o.lang === 'diff') body = diffRows(text, o.inner || '');
    else if (o.read) [body, raw] = readRows(text, o.lang);
    else body = hlInto(h('code'), text, langKey(o.lang));
    const copy = h('button', { type: 'button', class: 'btn' }, 'Copy');
    copy.addEventListener('click', () => copyText(raw, copy));
    return h('div', 'codev' + (o.error ? ' err' : ''),
      h('div', 'codev-h', o.label ? h('span', 'lbl', o.label) : null, o.tag ? h('span', 'tag', o.tag) : null, copy),
      h('pre', [rows ? 'rows' : '', !rows && o.wrap ? 'wrap' : '', o.max ? 'capped' : ''].filter(Boolean).join(' '), body),
      clip ? h('div', 'clipped', `… ${fmtBytes(Number(clip[1]))} more not shown`) : null);
  }

  // mdView frames markdown text, rendered.
  function mdView(text, lbl) {
    const copy = h('button', { type: 'button', class: 'btn' }, 'Copy');
    copy.addEventListener('click', () => copyText(text, copy));
    return h('div', 'codev', h('div', 'codev-h', lbl ? h('span', 'lbl', lbl) : null, copy), h('div', 'codev-md', md(text)));
  }

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
    const box = h('div');
    hlLines(raw, langKey(lang)).forEach((nodes, i) => box.append(h('div', 'gl', h('span', 'ln', nums[i]), h('span', 'lc', nodes.length ? nodes : ' '))));
    return [box, raw];
  }

  // A diff's file header: git's, or Codex's "M path" (A, M, D).
  const DIFF_FILE = /^(?:diff --git |index [\da-f]+\.\.|--- (?:a\/|\/dev\/null)|\+\+\+ (?:b\/|\/dev\/null)|[^+\- @\\])/;

  // diffRows shows a diff: added and removed lines marked and tinted, their
  // code highlighted as lang, or as the file a header names.
  function diffRows(text, lang) {
    const box = h('div');
    let cur = langKey(lang);
    let code = []; // [mark, line] of the current hunk
    const flush = () => {
      if (!code.length) return;
      const lines = hlLines(code.map((c) => c[1]).join('\n'), cur);
      code.forEach(([mark], i) => {
        box.append(h('div', 'dr' + (mark === '+' ? ' add' : mark === '-' ? ' del' : ''),
          h('span', 'sg', mark === '-' ? '−' : mark.trim()), h('span', 'lc', lines[i].length ? lines[i] : ' ')));
      });
      code = [];
    };
    for (const l of text.split('\n')) {
      if (l && DIFF_FILE.test(l)) {
        flush();
        const p = /^diff --git a\/.* b\/(.*)$/.exec(l) || /^\+\+\+ b\/(.*)$/.exec(l) || /^[AMD] (?:.* -> )?(\S.*)$/.exec(l);
        if (p) cur = extLang(p[1]) || langKey(lang);
        box.append(h('div', /^(?:diff --git |[AMD] )/.test(l) ? 'dr-file' : 'dr-meta', l));
      } else if (l.startsWith('@@') || l.startsWith('\\')) {
        flush();
        box.append(h('div', 'dr-hunk', l));
      } else {
        const mark = l[0] === '+' || l[0] === '-' ? l[0] : ' ';
        code.push([mark, l[0] === mark ? l.slice(1) : l]);
      }
    }
    flush();
    return box;
  }

  // ---- highlighting
  //
  // A tokenizer per language family (comments, strings, numbers, keywords,
  // a few more), not a parser: enough to read code by.

  const words = (s) => new Set(s.split(' '));

  // Parts of the patterns. Every group is (?:…): tokens numbers the parts.
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

  // tokenizer builds one from [kind, pattern] parts, tried in order at each
  // position. Kind w is a word (see wordKind), x a word after a lead
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
    for (const [s, k] of tokens(text, name)) el.append(k ? h('span', 'hl-' + k, s) : s);
    return el;
  }

  // hlLines highlights text as lang, a list of nodes per line.
  function hlLines(text, name) {
    const lines = [[]];
    for (const [s, k] of tokens(text, name)) {
      s.split('\n').forEach((part, i) => {
        if (i) lines.push([]);
        if (part) lines[lines.length - 1].push(k ? h('span', 'hl-' + k, part) : part);
      });
    }
    return lines;
  }

  // ---- markdown

  const MD_ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const MD_TABLE_SEP = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

  // md renders the agent's markdown: headings, paragraphs, lists, quotes,
  // tables, rules and fenced code (highlighted), with inline code, bold,
  // italics and links. Nothing is parsed as HTML.
  function md(text) {
    const box = h('div', 'md');
    const lines = text.split('\n');
    let para = [];
    let list = null; // {el, ordered, depth}
    const flush = () => {
      if (para.length) box.append(inline(h('p'), para.join('\n')));
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
        box.append(codeView({ text: src, lang: langKey(fence[3]) || (looksJSON(src) ? 'json' : ''), tag: fence[3] || 'text' }));
        continue;
      }
      if (!l.trim()) {
        end();
        continue;
      }
      const head = /^\s{0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/.exec(l);
      if (head) {
        end();
        box.append(inline(h('h' + Math.min(6, head[1].length + 2)), head[2]));
        continue;
      }
      if (/^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/.test(l)) {
        end();
        box.append(h('hr'));
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
        box.append(h('blockquote', null, md(quote.join('\n'))));
        continue;
      }
      const item = MD_ITEM.exec(l);
      if (item) {
        flush();
        const ordered = /\d/.test(item[2]);
        const depth = Math.min(3, Math.floor(item[1].replace(/\t/g, '    ').length / 2));
        if (!list || list.ordered !== ordered || list.depth !== depth) {
          const el = h(ordered ? 'ol' : 'ul');
          if (ordered && parseInt(item[2], 10) > 1) el.start = parseInt(item[2], 10);
          if (depth) el.style.marginLeft = depth * 18 + 'px';
          list = { el, ordered, depth };
          box.append(el);
        }
        list.el.append(mdItem(item[3]));
        continue;
      }
      if (list && /^\s{2,}\S/.test(l)) {
        // More of the item above.
        inline(list.el.lastElementChild, '\n' + l.trim());
        continue;
      }
      list = null;
      para.push(l);
    }
    end();
    return box;
  }

  function mdItem(text) {
    const task = /^\[([ xX])\]\s+/.exec(text);
    if (!task) return inline(h('li'), text);
    return inline(h('li', { class: 'task', 'data-mark': task[1] === ' ' ? '☐' : '☑' }), text.slice(task[0].length));
  }

  // mdTable renders a table: its head row, the separator (which aligns the
  // columns) and the body rows.
  function mdTable(head, sep, rows) {
    const align = splitRow(sep).map((c) => {
      const s = c.trim();
      return !s.endsWith(':') ? '' : s.startsWith(':') ? 'c' : 'n';
    });
    return h('div', 'tbl', h('table', null,
      h('thead', null, h('tr', null, splitRow(head).map((c, j) => inline(h('th', align[j] || null), c.trim())))),
      h('tbody', null, rows.map((r) => h('tr', null, splitRow(r).map((c, j) => inline(h('td', align[j] || null), c.trim())))))));
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
    if (t[0] === '`') return h('code', null, t.slice(1, -1));
    if (m[1] !== undefined) return link(m[2], m[1]);
    if (t.startsWith('http')) return link(t, t);
    if (t.startsWith('**') || t.startsWith('__')) return inline(h('strong'), t.slice(2, -2));
    if (t.startsWith('~~')) return inline(h('del'), t.slice(2, -2));
    return inline(h('em'), t.slice(1, -1));
  }

  function link(href, text) {
    return h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, text);
  }

  // -------------------------------------------------------------- the panel
  //
  // Beside the chat (over it below 1200 px, a screen of its own on phones):
  // the session's workflow runs and their agents' conversations, the files
  // it wrote and a search through its folder, the images of its
  // conversation, and its terminal.

  const insp = { open: false, tab: '', panes: null };

  // inspTabs are the tabs the open session has.
  function inspTabs() {
    if (!chat.open) return [];
    const out = [];
    if (wf.runs.length || wf.hint) out.push({ id: 'workflow', text: 'Workflow', icon: 'workflow', extra: wf.runs.some((r) => r.status === 'running') ? h('span', 'spin') : wf.runs.length ? h('span', 'n', String(wf.runs.length)) : null });
    if (!files.missing && (files.dir || files.total)) out.push({ id: 'files', text: 'Files', icon: 'files', extra: files.total ? h('span', 'n', String(files.total)) : null });
    if (media.items.length) out.push({ id: 'media', text: 'Media', icon: 'image', extra: h('span', 'n', String(media.items.length)) });
    if (chatLive()) out.push({ id: 'terminal', text: 'Terminal', icon: 'terminal', extra: null });
    return out;
  }

  function openInsp(tab) {
    if (!chat.open) return;
    insp.open = true;
    if (tab) insp.tab = tab;
    renderInsp();
    renderChatHead();
  }

  function closeInsp() {
    insp.open = false;
    renderInsp();
    renderChatHead();
    if (keyboard() && C.comp) C.comp.ta.focus({ preventScroll: true });
  }

  // toggleInsp opens the panel on tab (the terminal) or on its last other
  // tab (the panel button), or closes it when that is what it shows.
  function toggleInsp(tab) {
    const showing = insp.open && (tab ? insp.tab === tab : insp.tab !== 'terminal');
    if (showing) return closeInsp();
    if (!tab && insp.tab === 'terminal') insp.tab = '';
    openInsp(tab || insp.tab);
  }

  // openRun shows the workflow tab with run id unfolded ('' none).
  function openRun(id) {
    if (id) wf.shown.add(id);
    if (wf.sub) closeSub();
    openInsp('workflow');
    const card = id && insp.panes && insp.panes.workflow.list.querySelector(`[data-run="${CSS.escape(id)}"]`);
    if (card) card.scrollIntoView({ block: 'start' });
  }

  function buildPanes() {
    if (insp.panes) return insp.panes;
    const p = {};
    // workflow: the runs, or one agent's conversation
    const list = h('div', 'insp-body');
    const subHead = h('div', 'insp-sub-h');
    const subLog = h('div', 'col');
    const subScroll = h('div', 'sub-log', subLog);
    stickToBottom(subScroll);
    const sub = h('div', { class: 'insp-pane', hidden: true }, subHead, h('p', 'ro-note', 'Read-only: workflow agents take no input'), subScroll);
    p.workflow = { el: h('div', 'insp-pane', list, sub), list, sub, subHead, subLog, subScroll };
    wf.feed = newFeed({
      scroller: subScroll,
      log: subLog,
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
    // files: the list and a search, or one file
    const q = h('input', { type: 'search', placeholder: 'Search files', 'aria-label': 'Search files in the session’s folder', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
    q.addEventListener('input', () => filesSoon(250));
    q.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault();
        loadFiles();
      } else if (ev.key === 'Escape' && q.value) {
        ev.preventDefault();
        ev.stopPropagation();
        q.value = '';
        loadFiles();
      }
    });
    const reload = iconBtn('refresh', 'Look again', () => {
      loadFiles();
      if (files.view === 'tree') refreshTree();
    });
    const fmode = h('div', { class: 'seg files-mode', role: 'group', 'aria-label': 'Which files' });
    const flist = h('div', { class: 'insp-body flush', 'aria-live': 'polite' });
    const browse = h('div', 'insp-pane', h('div', 'files-search', fmode, h('div', 'pop-row', h('label', 'search grow', icon('search', 'i-sm'), q), reload)), flist);
    const fview = h('div', { class: 'insp-pane', hidden: true });
    p.files = { el: h('div', 'insp-pane', browse, fview), q, reload, mode: fmode, list: flist, browse, view: fview };
    // media
    const grid = h('div', 'media-grid');
    p.media = { el: h('div', 'insp-pane', h('div', 'insp-body', grid)), grid };
    // terminal
    const pre = h('pre', { class: 'term', 'aria-label': 'Terminal screen', tabindex: '0' });
    const termNote = h('span', 'grow', 'Live');
    p.terminal = { el: h('div', 'insp-pane', h('div', 'term-pane',
      h('div', 'term-bar', h('span', 'dot'), termNote, h('button', { type: 'button', class: 'btn ghost sm', onclick: () => refreshScreen() }, icon('refresh'), 'Refresh')),
      pre, h('div', 'term-keys'))), pre, note: termNote };
    insp.panes = p;
    return p;
  }

  function renderInsp() {
    const el = $('insp');
    const tabs = inspTabs();
    const show = insp.open && chat.open && U.view === 'chat' && admin && tabs.length > 0;
    el.hidden = !show;
    el.classList.toggle('shown', show);
    if (!show) {
      el.replaceChildren();
      screenNeeded();
      return;
    }
    if (!tabs.some((t) => t.id === insp.tab)) insp.tab = tabs[0].id;
    const p = buildPanes();
    if (!el.querySelector('.insp-h')) {
      el.replaceChildren(h('div', 'insp-h'), p.workflow.el, p.files.el, p.media.el, p.terminal.el);
    }
    renderInspTabs();
    for (const id of ['workflow', 'files', 'media', 'terminal']) p[id].el.hidden = id !== insp.tab;
    if (insp.tab === 'workflow') renderWorkflows();
    else if (insp.tab === 'files') {
      renderFiles();
      if (!files.loaded) loadFiles();
    } else if (insp.tab === 'media') renderMedia();
    else if (insp.tab === 'terminal') {
      fill(p.terminal.el.querySelector('.term-keys'), chat.agent && chat.agent.adapter !== 'shell' ? keysStrip() : null);
      const pre = p.terminal.pre;
      const text = screen.text || 'Reading the terminal…';
      if (pre.textContent !== text) {
        pre.textContent = text;
        pre.scrollTop = pre.scrollHeight;
      }
    }
    screenNeeded();
  }

  // renderInspTabs draws the panel's tab bar.
  function renderInspTabs() {
    const el = $('insp');
    const head = el.querySelector('.insp-h');
    if (!head) return;
    const tabs = inspTabs();
    if (!tabs.some((t) => t.id === insp.tab)) return renderInsp();
    const bar = h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'Session details' }, tabs.map((t) =>
      h('button', {
        type: 'button', class: 'tab', role: 'tab', 'aria-selected': String(insp.tab === t.id), tabindex: insp.tab === t.id ? '0' : '-1',
        onclick: () => {
          insp.tab = t.id;
          renderInsp();
          renderChatHead();
        },
      }, icon(t.icon), t.text, t.extra)));
    bar.addEventListener('keydown', (ev) => {
      if (ev.key !== 'ArrowRight' && ev.key !== 'ArrowLeft') return;
      const i = tabs.findIndex((t) => t.id === insp.tab);
      const j = (i + (ev.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length;
      insp.tab = tabs[j].id;
      renderInsp();
      renderChatHead();
      el.querySelectorAll('.tab')[j]?.focus();
    });
    fill(head, small() ? iconBtn('chevron-left', 'Back to the chat', closeInsp, { cls: 'back-btn' }) : null, bar, small() ? null : iconBtn('x', 'Close the panel', closeInsp));
  }

  // ---- workflows
  //
  // A workflow is a script a Claude Code session runs in the background: it
  // starts many subagents, in phases, and returns a result. The session
  // list shows the runs going on (GET /api/workflows, here and on every
  // joined fleet); the panel shows all of the session's runs, long-polled,
  // and each of their agents' conversations.

  const wf = {
    runs: [], // the open session's runs, oldest first
    v: '', // their version, for the next long poll
    loaded: false,
    hint: 0, // runs the session list knew of when the session opened
    shown: new Set(), // run ids unfolded
    known: new Set(), // run ids seen: a new one unfolds by itself
    cards: new Map(), // run id -> {sig, el}: cards only change with their run
    sub: null, // {run, id}: the workflow agent shown
    feed: null, // its conversation
  };

  function resetWorkflows(host, id) {
    Object.assign(wf, { runs: [], v: '', loaded: false, sub: null, hint: (S.workflows.get(agentKey(host, id)) || []).length });
    wf.shown.clear();
    wf.known.clear();
    wf.cards.clear();
  }

  const subRun = () => wf.sub && wf.runs.find((r) => r.id === wf.sub.run);
  const subAgent = () => {
    const run = subRun();
    return run && run.agents.find((a) => a.id === wf.sub.id);
  };
  // subLive: the agent may write more (also while not known yet).
  const subLive = () => {
    const a = subAgent();
    return !a || a.status === 'running';
  };

  // wfLoop follows the session's runs while it shows; a finished session's
  // runs no longer change.
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
      // At first the runs going on unfold; later every new one.
      if (!wf.known.has(r.id) && (!first || r.status === 'running')) wf.shown.add(r.id);
      wf.known.add(r.id);
    }
    if (first && runs.length) wf.shown.add(runs[runs.length - 1].id);
    for (const f of [chat.feed, wf.feed]) if (f) for (const paint of f.watch) paint();
    renderChatHead();
    if (insp.open) {
      if (insp.tab === 'workflow' && !wf.sub) renderWorkflows();
      else renderInspTabs();
    }
    if (wf.sub) {
      const now = subAgent();
      if (was && now && was.status !== now.status) paintPending(wf.feed);
      renderSubHead();
    }
  }

  function renderWorkflows() {
    const p = insp.panes && insp.panes.workflow;
    if (!p) return;
    p.list.hidden = !!wf.sub;
    p.sub.hidden = !wf.sub;
    if (wf.sub) return renderSubHead();
    if (!wf.loaded || !wf.runs.length) {
      wf.cards.clear();
      fill(p.list, h('p', 'empty-note', !wf.loaded ? 'Loading…' : 'This session has started no workflow.'));
      return;
    }
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
    // Cards already in place stay (a moved element loses its scroll position).
    for (const el of [...p.list.children]) if (!cards.includes(el)) el.remove();
    cards.forEach((el, i) => {
      if (p.list.children[i] !== el) p.list.insertBefore(el, p.list.children[i] || null);
    });
  }

  function runCard(run) {
    const open = wf.shown.has(run.id);
    const fin = run.agents.filter((a) => a.status !== 'running').length;
    const head = h('button', { type: 'button', class: 'run-h', 'aria-expanded': String(open), onclick: () => {
      if (open) wf.shown.delete(run.id);
      else wf.shown.add(run.id);
      renderWorkflows();
    } },
    icon('chevron-right', 'chev'),
    h('div', 'grow',
      h('div', 'chead-row', h('span', 'wf-name', run.name), wfPill(run.status)),
      h('div', 'run-stats', h('span', null, `${fin}/${run.agents.length} agents`), h('span', null, `${run.toolUses} tool calls`),
        h('span', null, `${fmtTokens(run.tokens)} tokens`), elapsed(run.startedMs, run.status === 'running' ? 0 : run.endedMs || run.updatedMs))));
    return h('section', { class: 'run', 'data-run': run.id }, head, phaseStrip(run), open ? runBody(run) : null);
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

  // phaseStrip is a run at a glance: its phases side by side, an agent a
  // bar (many agents: a bar per stretch of the same status).
  function phaseStrip(run) {
    return h('div', 'phases', phaseGroups(run).map((g) => {
      const fin = g.agents.filter((a) => a.status !== 'running').length;
      const now = run.status === 'running' && g.title === run.phase;
      return h('div', 'phase' + (now ? ' now' : ''),
        h('div', 'phase-h', h('span', 'trunc', g.title || 'Agents'), h('span', 'num', g.agents.length ? `${fin}/${g.agents.length}` : '–')),
        h('div', { class: 'bars', role: 'img', 'aria-label': `${g.title || 'Agents'}: ${fin} of ${g.agents.length} agents done` }, bars(g.agents.map((a) => a.status))));
    }));
  }

  const BARS_MAX = 16;
  function bars(statuses) {
    if (!statuses.length) return h('span', null);
    const runs = [];
    for (const s of statuses) {
      const last = runs[runs.length - 1];
      if (statuses.length > BARS_MAX && last && last.s === s) last.n++;
      else runs.push({ s, n: 1 });
    }
    return runs.map(({ s, n }) => {
      const b = h('span', s === 'running' ? 'running' : s === 'failed' ? 'failed' : 'done');
      b.style.flexGrow = String(n);
      return b;
    });
  }

  function runBody(run) {
    const out = [];
    if (run.description) out.push(h('p', 'wf-desc', run.description));
    if (run.status === 'failed' || (run.status === 'stopped' && run.summary)) out.push(h('pre', 'result-err', run.summary || 'No reason given.'));
    else if (run.status === 'stopped') out.push(h('p', 'note-line', 'Stopped: the session ended before the run did.'));
    for (const g of phaseGroups(run)) {
      const busy = g.agents.some((a) => a.status === 'running');
      out.push(h('div', null,
        h('div', 'ph-sec-h', busy ? h('span', 'spin') : null, g.title || 'Agents', g.detail ? h('span', 'muted', g.detail) : null),
        g.agents.length ? h('ul', 'wf-agents', g.agents.map((a) => h('li', null, wfAgentBtn(run, a))))
          : h('p', 'menu-note', run.status === 'running' ? 'Not started yet' : 'Not reached')));
    }
    if (run.logs && run.logs.length) out.push(h('div', 'run-log', h('div', 'out-l', 'Log'), h('pre', 'out', run.logs.join('\n'))));
    if (run.result) out.push(h('div', 'result', resultNode(run.result)));
    return h('div', 'run-body', out);
  }

  function wfAgentBtn(run, a) {
    return h('button', { type: 'button', class: 'wf-agent', onclick: () => openSub(run.id, a.id), 'aria-label': `${a.label || a.id}, ${a.status}. Open its conversation` },
      a.status === 'running' ? h('span', 'spin') : a.status === 'failed' ? icon('alert', 'bad-ic') : icon('check', 'ok-ic'),
      h('span', 'name', a.label || a.id),
      h('span', 'what', a.status === 'running' ? [a.tool ? h('b', null, a.tool + ' ') : null, a.activity || 'starting…'] : `${a.toolUses} tool calls · ${fmtTokens(a.tokens)} tokens`),
      elapsed(a.startedMs, a.status === 'running' ? 0 : a.updatedMs, 'muted num'),
      icon('chevron-right', 'chev'));
  }

  // openSub shows one workflow agent's conversation in the panel.
  function openSub(run, id) {
    openInsp('workflow');
    wf.sub = { run, id };
    renderWorkflows();
    startFeed(wf.feed);
  }

  function closeSub() {
    stopFeed(wf.feed);
    wf.sub = null;
    renderWorkflows();
  }

  function renderSubHead() {
    const p = insp.panes && insp.panes.workflow;
    if (!wf.sub || !p) return;
    const run = subRun();
    const a = subAgent() || { id: wf.sub.id, status: 'running' };
    fill(p.subHead,
      iconBtn('chevron-left', 'Back to ' + (run ? run.name : 'the workflow'), closeSub),
      h('div', 'grow',
        h('div', 'chead-row', h('span', 'insp-sub-title', a.label || a.id), wfPill(a.status)),
        h('div', 'insp-sub-meta', wf.feed.error || [a.phase ? a.phase + ' · ' : '', a.model ? a.model.replace(/^claude-/, '') + ' · ' : '',
          `${a.toolUses || 0} tool calls · ${fmtTokens(a.tokens)} tokens · `, elapsed(a.startedMs, a.status === 'running' ? 0 : a.updatedMs)])));
  }

  // ---- media: every image of the session's conversation
  //
  // GET .../media lists them (offsets into the transcript, like the chat);
  // the page asks again for newer ones whenever the chat shows an image,
  // and loads each as it scrolls into view.

  const media = {
    items: [], // {line, n, type, ts, from, tool, text}, oldest first
    file: '',
    end: 0,
    loaded: false,
    error: '',
    busy: false,
    again: false, // ask once more after the request in flight
    els: new Map(), // file:line:n -> grid cell, kept across renders
  };

  // viewer is what the full-size view steps through: the media, newest
  // first; null for a single image of the chat.
  const viewer = { list: null, i: 0, seq: 0 };

  const mediaKey = (m) => `${media.file}:${m.line}:${m.n}`;
  const mediaURL = (m) => chatAPI('image?' + new URLSearchParams({ file: media.file, line: String(m.line), n: String(m.n) }));

  function mediaLabel(m) {
    if (m.from === 'user') return 'sent by you';
    const t = m.text || m.tool || 'tool output';
    return /^\S+$/.test(t) ? t.split('/').pop() : t; // a path: its file name
  }

  function resetMedia() {
    Object.assign(media, { items: [], file: '', end: 0, loaded: false, error: '', busy: false, again: false });
    media.els.clear();
  }

  async function loadMedia() {
    if (media.busy) {
      media.again = true;
      return;
    }
    const seq = chat.seq;
    media.busy = true;
    try {
      for (;;) {
        media.again = false;
        const q = new URLSearchParams();
        if (media.file) {
          q.set('file', media.file);
          q.set('after', String(media.end));
        }
        const r = await api('GET', chatAPI('media?' + q));
        if (seq !== chat.seq) return;
        if (r.reset) {
          media.items = [];
          media.els.clear();
        }
        const had = media.items.length;
        media.items.push(...(r.media || []));
        Object.assign(media, { file: r.file || '', end: r.end || 0, loaded: true, error: '' });
        if (!had && media.items.length) renderChatHead();
        if (insp.open) {
          if (insp.tab === 'media') renderMedia();
          renderInspTabs();
        }
        if (!r.more && !media.again) break;
      }
    } catch (e) {
      if (seq !== chat.seq) return;
      Object.assign(media, { loaded: true, error: e.status === 404 ? '' : e.message });
    } finally {
      if (seq === chat.seq) media.busy = false;
    }
  }

  function renderMedia() {
    const p = insp.panes && insp.panes.media;
    if (!p) return;
    if (!media.items.length) {
      fill(p.grid, h('p', 'empty-note', !media.loaded ? 'Loading…' : media.error || 'No images in this conversation yet.'));
      return;
    }
    const list = [...media.items].reverse();
    fill(p.grid, list.map((m, i) => {
      const key = mediaKey(m);
      let b = media.els.get(key);
      if (!b) {
        b = mediaThumb(m);
        media.els.set(key, b);
      }
      b.dataset.i = String(i);
      return b;
    }));
  }

  function mediaThumb(m) {
    const text = mediaLabel(m);
    const when = m.ts ? new Date(m.ts).toLocaleString() : '';
    const ph = h('span', 'ph', 'image…');
    const b = h('button', { type: 'button', class: 'media-cell', title: [m.from === 'user' ? 'sent by you' : [m.tool, m.text].filter(Boolean).join(': '), when].filter(Boolean).join('\n'), 'aria-label': `${text}${when ? ', ' + when : ''}. Open full size` },
      ph, h('span', 'cap', text));
    b.loadImage = () => loadImage(mediaURL(m)).then((src) => {
      ph.replaceWith(h('img', { src, alt: '' }));
    }, (e) => {
      ph.textContent = e.message;
    });
    b.addEventListener('click', () => openMedia(Number(b.dataset.i)));
    if (imgSeen) imgSeen.observe(b);
    else b.loadImage();
    return b;
  }

  function openImage(src) {
    viewer.list = null;
    viewer.seq++;
    $('viewer-prev').hidden = $('viewer-next').hidden = true;
    $('viewer-cap').textContent = '';
    showImage(src);
  }

  function showImage(src) {
    const img = $('viewer-img');
    img.src = src;
    const d = $('viewer');
    if (d.open) return;
    d.showModal();
    d.focus(); // not its first button: the arrow keys step from anywhere
  }

  // openMedia shows image i of the media (newest first) full size.
  function openMedia(i) {
    const list = viewer.list && $('viewer').open ? viewer.list : [...media.items].reverse();
    const m = list[i];
    if (!m) return;
    Object.assign(viewer, { list, i });
    const seq = ++viewer.seq;
    $('viewer-prev').hidden = $('viewer-next').hidden = list.length < 2;
    $('viewer-cap').textContent = `${i + 1}/${list.length} · ${mediaLabel(m)}${m.ts ? ' · ' + new Date(m.ts).toLocaleString() : ''}`;
    $('viewer-prev').disabled = i === 0;
    $('viewer-next').disabled = i === list.length - 1;
    $('viewer-img').alt = mediaLabel(m);
    loadImage(mediaURL(m)).then((src) => {
      if (seq === viewer.seq) showImage(src);
    }, (e) => {
      if (seq === viewer.seq) toast(`Could not load the image: ${e.message}`, 'error');
    });
  }

  function wireViewer() {
    const d = $('viewer');
    $('viewer-prev').addEventListener('click', () => openMedia(viewer.i - 1));
    $('viewer-next').addEventListener('click', () => openMedia(viewer.i + 1));
    $('viewer-close').addEventListener('click', () => d.close());
    d.addEventListener('click', (ev) => {
      if (ev.target === d || ev.target.id === 'viewer-img') d.close();
    });
    d.addEventListener('keydown', (ev) => {
      if (!viewer.list || (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight')) return;
      ev.preventDefault();
      openMedia(viewer.i + (ev.key === 'ArrowLeft' ? -1 : 1));
    });
    // Swiping sideways steps through the images too. The image follows the
    // finger, held back past the first and the last; let go far enough or
    // flicked, it slides out and the next one comes in from the other side.
    const img = $('viewer-img');
    let g = null; // {id, x0, y0, dx, on, t, v}
    const move = (x, fade) => {
      img.style.transform = x ? `translateX(${x}px)` : '';
      img.style.opacity = fade ? '0' : '';
    };
    d.addEventListener('touchstart', (ev) => {
      if (g && g.on) return;
      g = null;
      if (!viewer.list || ev.touches.length !== 1) return;
      const t = ev.touches[0];
      g = { id: t.identifier, x0: t.clientX, y0: t.clientY, dx: 0, on: false, t: ev.timeStamp, v: 0 };
    }, { passive: true });
    d.addEventListener('touchmove', (ev) => {
      const t = g && [...ev.touches].find((x) => x.identifier === g.id);
      if (!t) return;
      if (ev.touches.length > 1 && !g.on) return (g = null); // pinching
      const dx = t.clientX - g.x0;
      const dy = t.clientY - g.y0;
      if (!g.on) {
        if (!ev.cancelable || (Math.abs(dy) > 10 && Math.abs(dy) > Math.abs(dx))) return (g = null);
        if (Math.abs(dx) < 10) return;
        g.on = true;
        img.style.transition = 'none';
      }
      ev.preventDefault();
      const edge = (dx > 0 && viewer.i === 0) || (dx < 0 && viewer.i === viewer.list.length - 1);
      const x = edge ? dx * 0.3 : dx;
      const dt = ev.timeStamp - g.t;
      if (dt > 0) g.v = 0.6 * g.v + 0.4 * ((x - g.dx) / dt);
      g.t = ev.timeStamp;
      g.dx = x;
      move(x);
    }, { passive: false });
    const end = (ev) => {
      if (ev.touches.length) return;
      const s = g;
      g = null;
      if (!s || !s.on) return;
      ev.preventDefault(); // no click: it would close the view
      img.style.transition = 'transform 0.2s var(--ease), opacity 0.2s';
      const flick = Math.abs(s.v) > 0.45;
      const dir = ev.type !== 'touchend' ? 0 : flick ? -Math.sign(s.v) : Math.abs(s.dx) > 60 ? -Math.sign(s.dx) : 0;
      const to = viewer.i + dir;
      if (!dir || to < 0 || to >= viewer.list.length) return move(0);
      move(-dir * innerWidth * 0.5, true);
      setTimeout(() => {
        img.style.transition = 'none';
        move(dir * innerWidth * 0.3, true);
        // In once the new image is there (or could not be loaded).
        let done = false;
        const slideIn = () => {
          if (done) return;
          done = true;
          img.removeEventListener('load', slideIn);
          img.style.transition = 'transform 0.22s var(--ease), opacity 0.22s';
          move(0);
        };
        img.addEventListener('load', slideIn);
        setTimeout(slideIn, 1500);
        openMedia(to);
      }, 200);
    };
    d.addEventListener('touchend', end);
    d.addEventListener('touchcancel', end);
    d.addEventListener('close', () => {
      g = null;
      img.style.transition = '';
      move(0);
    });
  }

  // ---- files: what the agent wrote and changed, and a search through its
  // folder
  //
  // GET .../files lists them, newest first: the files its tool calls wrote,
  // wherever they are, and those of its folder changed since the session
  // started; with q, those whose path holds every word of it, and in found
  // the folder's other files that do. GET .../file reads one, in base64.

  const files = {
    list: [], // {path, rel, size, mtimeMs, written, gone}, newest first
    found: [], // with a search: the folder's other files that match
    q: '',
    dir: '', // the agent's folder, '' if it has none
    total: 0, // files written and changed, without a search
    partial: false,
    loaded: false,
    error: '',
    missing: false, // the fleet has no files route
    busy: false,
    again: false,
    timer: 0, // a search typed or files written: waits for a pause
    open: null, // the file shown
    shown: null, // its content: {path, rel, size, mtimeMs, type, text, data, big}
    seq: 0, // file reads: only the latest is shown
    url: '', // object URL of the video or audio shown
    sendNote: null, // {text, kind} of the last send
    view: load('fleet.filesView', 'changed'), // 'changed', or 'tree': the whole folder
  };

  // Images shown from a data: URL (in an <img> an SVG runs no script).
  const IMG_TYPE = /^image\/(?:png|jpeg|gif|webp|avif|bmp|svg\+xml|x-icon|vnd\.microsoft\.icon)$/;
  const FILE_TEXT_MAX = 512 << 10;
  const FILE_KINDS = [
    [/^(?:png|jpe?g|gif|webp|avif|svg|bmp|ico|heic)$/, 'img'],
    [/^(?:mp4|webm|mov|m4v|mkv|mp3|wav|ogg|m4a|flac)$/, 'av'],
    [/^(?:md|mdx|markdown|txt|pdf|docx?|rtf|csv|tsv|xlsx?|pptx?|html?)$/, 'doc'],
  ];

  function b64bytes(s) {
    const bin = atob(s || '');
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function resetFiles() {
    clearTimeout(files.timer);
    revokeFileURL();
    Object.assign(files, { list: [], found: [], q: '', dir: '', total: 0, partial: false, loaded: false, error: '', missing: false, busy: false, again: false, open: null, shown: null, sendNote: null });
    files.seq++;
    resetTree();
  }

  function filesSoon(ms) {
    clearTimeout(files.timer);
    files.timer = setTimeout(loadFiles, ms);
  }

  async function loadFiles() {
    clearTimeout(files.timer);
    if (files.busy) {
      files.again = true;
      return;
    }
    const seq = chat.seq;
    files.busy = true;
    const p = insp.panes && insp.panes.files;
    if (p) p.reload.disabled = true;
    try {
      do {
        files.again = false;
        const q = p ? p.q.value.trim() : files.q;
        const r = await api('GET', chatAPI('files' + (q ? '?' + new URLSearchParams({ q }) : '')));
        if (seq !== chat.seq) return;
        Object.assign(files, { list: r.files || [], found: r.found || [], q, dir: r.dir || '', partial: !!r.partial, loaded: true, error: '', missing: false });
        if (!q) files.total = files.list.length;
      } while (files.again);
    } catch (e) {
      if (seq !== chat.seq) return;
      Object.assign(files, { loaded: true, error: e.message, missing: e.status === 404 && !files.dir });
    } finally {
      if (seq === chat.seq) {
        files.busy = false;
        if (p) p.reload.disabled = false;
      }
    }
    renderChatHead();
    if (insp.open) {
      renderInspTabs();
      if (insp.tab === 'files') renderFiles();
    }
  }

  function renderFiles() {
    const p = insp.panes && insp.panes.files;
    if (!p) return;
    p.browse.hidden = !!files.open;
    p.view.hidden = !files.open;
    if (files.open) return;
    p.q.placeholder = files.dir ? `Search files in ${baseName(files.dir) || '/'}` : 'Search the files written';
    renderFilesMode(p);
    const sec = (text, n) => h('div', 'files-sec', text, n != null ? h('span', 'muted num', ` · ${n}`) : null);
    if (!files.loaded) return fill(p.list, h('p', 'empty-note', 'Loading…'));
    if (files.error) return fill(p.list, h('p', 'empty-note', files.missing ? 'This fleet cannot list files: update it.' : files.error));
    const asTree = !!files.dir && files.view === 'tree';
    // A tree read out whole each time a folder unfolds says too much.
    p.list.setAttribute('aria-live', asTree ? 'off' : 'polite');
    if (asTree) return renderTree(p.list, sec);
    const out = [];
    if (files.q) {
      if (files.list.length) out.push(sec('Written or changed by the agent', files.list.length), fileRows(files.list));
      if (files.found.length) out.push(sec('Elsewhere in the folder', files.found.length), fileRows(files.found));
      if (!files.list.length && !files.found.length) out.push(h('p', 'empty-note', `No file matches “${files.q}”.`));
    } else if (files.list.length) {
      out.push(sec('Written or changed since the session started', files.list.length), fileRows(files.list));
    } else {
      out.push(h('p', 'empty-note', h('strong', null, 'No file written or changed yet'), files.dir ? ['Search to find any file in ', h('span', 'mono', shortPath(files.dir)), '.'] : null));
    }
    if (files.partial) out.push(h('p', 'note-line', 'Not everything is listed: the folder is large.'));
    fill(p.list, out);
  }

  const fileExt = (name) => {
    const dot = name.lastIndexOf('.');
    return dot > 0 ? name.slice(dot + 1).toLowerCase() : '';
  };
  const fileKind = (name) => (FILE_KINDS.find(([re]) => re.test(fileExt(name))) || [])[1] || '';

  function fileBadge(name) {
    const kind = fileKind(name);
    return h('span', 'file-badge' + (kind ? ' ' + kind : ''), fileExt(name).slice(0, 4) || '—');
  }

  function fileRows(list) {
    return list.map((f) => {
      const name = baseName(f.path);
      const where = f.rel ? './' + f.rel.slice(0, -name.length) : f.path.slice(0, -name.length);
      return h('button', { type: 'button', class: 'file-row' + (f.gone ? ' gone' : ''), disabled: !!f.gone, title: f.path + (f.written ? '\nwritten by the agent' : ''), onclick: () => openFile(f) },
        fileBadge(name),
        h('div', 'grow', h('div', 'file-name', name), h('div', 'file-dir', shortPath(where) || './')),
        h('div', 'file-meta', f.gone ? 'deleted' : fmtSize(f.size), f.mtimeMs ? h('div', null, relTime(f.mtimeMs)) : null));
    });
  }

  // ---- files: the session's folder as a tree
  //
  // GET .../dir lists one folder, read when it is unfolded: its folders
  // first, then its files, each with its git status (M, A, D, R, U, I) and
  // a folder whether something inside changed; a chain of folders that
  // each hold only the next comes as one ("src/main/java"). With a search,
  // the tree shows the files that match, in their folders.

  const tree = {
    dirs: new Map(), // folder ('' the agent's) → {entries, partial, error, busy}
    open: new Set(), // the folders unfolded
    root: '', // the agent's folder, as the server names it
    missing: false, // the fleet has no dir route
    focus: '', // the row that takes the focus
    typed: '', // type-ahead: the letters typed lately
    typedAt: 0,
    timer: 0,
  };

  const GIT_TEXT = { M: 'modified', A: 'added', D: 'deleted', R: 'renamed', U: 'untracked', I: 'ignored by git' };

  function resetTree() {
    clearTimeout(tree.timer);
    Object.assign(tree, { dirs: new Map(), open: new Set(), root: '', missing: false, focus: '', typed: '' });
  }

  function setFilesView(v) {
    if (files.view === v) return;
    files.view = v;
    save('fleet.filesView', v);
    renderFiles();
  }

  function renderFilesMode(p) {
    p.mode.hidden = !files.dir || tree.missing;
    if (p.mode.hidden) return;
    const opt = (v, text, extra) => h('button', { type: 'button', 'aria-pressed': String(files.view === v), onclick: () => setFilesView(v) }, text, extra);
    fill(p.mode, opt('changed', 'Changed', files.total ? h('span', 'muted num', String(files.total)) : null), opt('tree', 'Folder'));
  }

  async function loadDir(key) {
    const seq = chat.seq;
    const d = tree.dirs.get(key) || {};
    tree.dirs.set(key, d);
    d.busy = true;
    try {
      const r = await api('GET', chatAPI('dir' + (key ? '?' + new URLSearchParams({ path: key }) : '')));
      if (seq !== chat.seq) return;
      Object.assign(d, { entries: r.entries || [], partial: !!r.partial, error: '' });
      if (!key) tree.root = r.path;
    } catch (e) {
      if (seq !== chat.seq) return;
      // A fleet without the route answers no JSON.
      if (!key && e.status === 404 && /^request failed/.test(e.message)) tree.missing = true;
      Object.assign(d, { error: e.message });
      if (key) tree.open.delete(key);
    } finally {
      if (seq === chat.seq) d.busy = false;
    }
    if (tree.missing) files.view = 'changed';
    if (insp.open && insp.tab === 'files') renderFiles();
  }

  // refreshTree reads the folders shown again; those gone are left out.
  function refreshTree() {
    clearTimeout(tree.timer);
    if (!tree.dirs.has('')) return;
    for (const key of tree.dirs.keys()) {
      if (key && !tree.open.has(key)) tree.dirs.delete(key);
      else if (!tree.dirs.get(key).busy) loadDir(key);
    }
  }

  function treeSoon(ms) {
    if (files.view !== 'tree' || !tree.dirs.has('')) return;
    clearTimeout(tree.timer);
    tree.timer = setTimeout(refreshTree, ms);
  }

  function toggleDir(e) {
    if (!e.dir || e.empty || e.gone) return;
    if (tree.open.has(e.path)) tree.open.delete(e.path);
    else {
      tree.open.add(e.path);
      if (!tree.dirs.has(e.path)) loadDir(e.path);
    }
    tree.focus = e.path;
    renderFiles();
  }

  function treeOpenFile(e) {
    if (e.gone) return;
    tree.focus = e.path;
    const rel = tree.root && e.path.startsWith(tree.root + '/') ? e.path.slice(tree.root.length + 1) : '';
    openFile({ path: e.path, rel, size: e.size || 0, mtimeMs: e.mtimeMs || 0 });
  }

  // treeRows are the rows to show: {e, depth, parent} for an entry, {note,
  // depth} for a line in its place.
  function treeRows() {
    const rows = [];
    const walk = (key, depth) => {
      const d = tree.dirs.get(key);
      if (!d || !d.entries) {
        rows.push({ note: d && d.error ? d.error : 'Loading…', depth });
        return;
      }
      if (!d.entries.length) rows.push({ note: 'Nothing in it', depth });
      for (const e of d.entries) {
        rows.push({ e, depth, parent: key, open: tree.open.has(e.path) });
        if (e.dir && tree.open.has(e.path)) walk(e.path, depth + 1);
      }
      if (d.partial) rows.push({ note: `Only the first ${d.entries.length} are listed.`, depth });
    };
    walk('', 0);
    return rows;
  }

  // matchRows are the rows of a search: the files found in the folder, in
  // their folders, all unfolded.
  function matchRows(list) {
    const top = { kids: new Map() };
    for (const f of list) {
      if (!f.rel) continue;
      const parts = f.rel.split('/');
      let n = top;
      parts.slice(0, -1).forEach((name, i) => {
        if (!n.kids.has(name + '/')) n.kids.set(name + '/', { name, dir: true, path: files.dir + '/' + parts.slice(0, i + 1).join('/'), kids: new Map() });
        n = n.kids.get(name + '/');
      });
      n.kids.set(parts.at(-1), { name: parts.at(-1), path: f.path, size: f.size, mtimeMs: f.mtimeMs, gone: f.gone, changed: f.written });
    }
    const rows = [];
    const walk = (n, depth, parent) => {
      const kids = [...n.kids.values()].sort((a, b) => (b.dir ? 1 : 0) - (a.dir ? 1 : 0) || a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }));
      for (let k of kids) {
        let name = k.name;
        while (k.dir && k.kids.size === 1 && [...k.kids.values()][0].dir) {
          k = [...k.kids.values()][0];
          name += '/' + k.name;
        }
        rows.push({ e: { ...k, name }, depth, parent, open: true, fixed: true });
        if (k.dir) walk(k, depth + 1, k.path);
      }
    };
    walk(top, 0, '');
    return rows;
  }

  function renderTree(list, sec) {
    const rows = [];
    const out = [];
    if (files.q) {
      const all = [...files.list, ...files.found];
      rows.push(...matchRows(all));
      const outside = files.list.filter((f) => !f.rel);
      if (!rows.length && !outside.length) out.push(h('p', 'empty-note', `No file matches “${files.q}”.`));
      if (outside.length) out.push(sec('Outside the folder', outside.length), fileRows(outside));
      if (files.partial) out.push(h('p', 'note-line', 'Not everything is listed: the folder is large.'));
    } else {
      if (!tree.dirs.has('')) loadDir('');
      rows.push(...treeRows());
    }
    const items = rows.filter((r) => r.e);
    if (!items.some((r) => r.e.path === tree.focus)) tree.focus = items.length ? items[0].e.path : '';
    const had = list.contains(document.activeElement) && document.activeElement.matches('.tree-row');
    const el = rows.length ? h('div', { class: 'tree', role: 'tree', 'aria-label': files.q ? 'Files that match' : `Files in ${baseName(files.dir) || '/'}`, onkeydown: (ev) => treeKey(ev, el, rows), onfocusin: treeFocus }, rows.map(treeRow)) : null;
    fill(list, el, out);
    if (had && el) {
      const row = el.querySelector(`.tree-row[data-path="${CSS.escape(tree.focus)}"]`);
      if (row) row.focus({ preventScroll: true });
    }
  }

  function treeRow(row) {
    // Set from script: the page's policy allows no style attribute.
    const deep = (el) => {
      el.style.setProperty('--d', row.depth);
      return el;
    };
    if (row.note) return deep(h('div', { class: 'tree-note', role: 'none' }, row.note));
    const e = row.e;
    const parts = e.name.split('/');
    const last = parts.pop();
    const kind = e.dir ? '' : fileKind(last);
    const said = [e.git ? GIT_TEXT[e.git] : '', e.changed ? (e.dir ? 'changes inside' : 'written by the agent') : '', e.link ? 'a link' : ''].filter(Boolean).join(', ');
    return deep(h('div', {
      class: 'tree-row' + (e.gone ? ' gone' : '') + (e.git === 'I' ? ' ignored' : ''),
      role: 'treeitem',
      tabindex: e.path === tree.focus ? '0' : '-1',
      'aria-level': String(row.depth + 1),
      'aria-expanded': e.dir && !e.empty && !e.gone ? String(row.open) : null,
      'aria-disabled': e.gone ? 'true' : null,
      'aria-description': said || null,
      'data-path': e.path,
      title: (e.gone ? e.path : shortPath(e.path)) + (said ? '\n' + said : '') + (!e.dir && !e.gone ? `\n${fmtSize(e.size || 0)}` : ''),
      onclick: () => {
        if (e.dir) {
          if (!row.fixed) toggleDir(e);
        } else treeOpenFile(e);
      },
    },
      e.dir && !e.empty && !e.gone ? icon('chevron-right', 'chev') : h('span', 'chev'),
      icon(e.dir ? 'folder' : 'file', 'ti' + (kind ? ' ' + kind : '')),
      h('span', 'tree-name', parts.length ? h('span', 'tree-pre', parts.join('/') + '/') : null, last),
      e.git && e.git !== 'I' ? h('span', { class: 'git g-' + e.git, 'aria-hidden': 'true' }, e.git) : e.changed ? h('span', { class: 'git-dot', 'aria-hidden': 'true' }) : null));
  }

  // treeFocus makes the row focused, by a tap or Tab, the one keys move from.
  function treeFocus(ev) {
    const row = ev.target.closest('.tree-row');
    if (!row || row.dataset.path === tree.focus) return;
    tree.focus = row.dataset.path;
    for (const n of ev.currentTarget.querySelectorAll('.tree-row')) n.tabIndex = n === row ? 0 : -1;
  }

  // treeKey moves through the tree from the keyboard: arrows, Home, End,
  // Enter or Space, and letters typed to jump to a name.
  function treeKey(ev, el, rows) {
    const items = rows.filter((r) => r.e);
    const at = Math.max(0, items.findIndex((r) => r.e.path === tree.focus));
    const r = items[at];
    if (!r) return;
    const go = (i) => {
      const to = items[Math.max(0, Math.min(items.length - 1, i))];
      tree.focus = to.e.path;
      for (const n of el.querySelectorAll('.tree-row')) n.tabIndex = n.dataset.path === tree.focus ? 0 : -1;
      const n = el.querySelector(`.tree-row[data-path="${CSS.escape(tree.focus)}"]`);
      if (n) {
        n.focus();
        n.scrollIntoView({ block: 'nearest' });
      }
    };
    const foldable = r.e.dir && !r.e.empty && !r.e.gone && !r.fixed;
    switch (ev.key) {
      case 'ArrowDown': go(at + 1); break;
      case 'ArrowUp': go(at - 1); break;
      case 'Home': go(0); break;
      case 'End': go(items.length - 1); break;
      case 'ArrowRight':
        if (foldable && !r.open) toggleDir(r.e);
        else if (r.e.dir && r.open && items[at + 1] && items[at + 1].parent === r.e.path) go(at + 1);
        break;
      case 'ArrowLeft':
        if (foldable && r.open) toggleDir(r.e);
        else if (r.parent) go(items.findIndex((x) => x.e.path === r.parent));
        break;
      case 'Enter':
      case ' ':
        if (r.e.dir) {
          if (foldable) toggleDir(r.e);
        } else treeOpenFile(r.e);
        break;
      default: {
        if (ev.key.length !== 1 || ev.ctrlKey || ev.metaKey || ev.altKey) return;
        const now = Date.now();
        tree.typed = (now - tree.typedAt < 700 ? tree.typed : '') + ev.key.toLowerCase();
        tree.typedAt = now;
        const from = tree.typed.length > 1 ? at : at + 1;
        for (let k = 0; k < items.length; k++) {
          const i = (from + k) % items.length;
          if (items[i].e.name.toLowerCase().startsWith(tree.typed)) {
            go(i);
            break;
          }
        }
      }
    }
    ev.preventDefault();
  }

  async function openFile(f) {
    const p = insp.panes && insp.panes.files;
    if (!p) return;
    const seq = ++files.seq;
    revokeFileURL();
    Object.assign(files, { open: f, shown: null, sendNote: null });
    renderFiles();
    renderFileView();
    try {
      const r = await api('GET', chatAPI('file?' + new URLSearchParams({ path: f.path })));
      if (seq !== files.seq) return;
      files.shown = r;
    } catch (e) {
      if (seq !== files.seq) return;
      files.shown = { error: e.message };
    }
    renderFileView();
  }

  function closeFile() {
    files.seq++;
    revokeFileURL();
    Object.assign(files, { open: null, shown: null, sendNote: null });
    renderFiles();
  }

  function revokeFileURL() {
    if (files.url) URL.revokeObjectURL(files.url);
    files.url = '';
  }

  function renderFileView() {
    const p = insp.panes && insp.panes.files;
    const f = files.open;
    if (!p || !f) return;
    const r = files.shown;
    const when = r && r.mtimeMs ? new Date(r.mtimeMs).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' }) : '';
    const sendBtn = h('button', { type: 'button', class: 'btn sm', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => sendMenu(ev.currentTarget) }, icon('send'), 'Send to');
    const dl = iconBtn('download', 'Download', downloadFile, { attrs: { disabled: !r || r.error || r.big ? true : null } });
    fill(p.view,
      h('div', 'file-view-h', iconBtn('chevron-left', 'Back to the files', closeFile),
        h('div', 'grow', h('div', 'insp-sub-title', baseName(f.path)), h('div', 'insp-sub-meta mono trunc', [r && !r.error ? fmtSize(r.size) : '', when, f.rel || shortPath(f.path)].filter(Boolean).join(' · '))),
        sendBtn, dl),
      files.sendNote ? h('p', { class: 'send-note', role: 'status' }, files.sendNote.text) : null,
      h('div', 'file-body', !r ? h('p', 'empty-note', 'Loading…') : r.error ? h('p', 'empty-note', r.error) : filePreview(r, baseName(r.path))));
  }

  // filePreview shows a file's content: an image, text (markdown rendered,
  // code highlighted with line numbers), a video or a sound.
  function filePreview(r, name) {
    const type = (r.type || '').split(';')[0].trim().toLowerCase();
    if (r.big) return h('p', 'empty-note', `${fmtSize(r.size)}: too large to open here.`);
    if (IMG_TYPE.test(type) && (!r.text || type === 'image/svg+xml')) {
      const src = `data:${type};base64,${r.data}`;
      return h('div', 'file-img', h('img', { src, alt: name, onclick: () => openImage(src) }));
    }
    if (r.text) {
      let text = new TextDecoder().decode(b64bytes(r.data));
      if (!text) return h('p', 'empty-note', 'The file is empty.');
      const cut = text.length > FILE_TEXT_MAX;
      if (cut) text = text.slice(0, FILE_TEXT_MAX);
      const more = cut ? h('p', 'note-line', 'The rest is not shown: download the file.') : null;
      if (/\.(?:md|mdx|markdown)$/i.test(name)) return [md(text), more];
      const lang = extLang(name);
      const numbered = text.replace(/\n$/, '').split('\n').map((l, i) => `${i + 1}\t${l}`).join('\n');
      return [codeView({ text: numbered, read: true, lang, tag: lang || 'text' }), more];
    }
    if (/^(?:video|audio)\//.test(type)) {
      files.url = URL.createObjectURL(new Blob([b64bytes(r.data)], { type }));
      return h(type.startsWith('video/') ? 'video' : 'audio', { controls: true, playsinline: true, preload: 'metadata', src: files.url });
    }
    return h('p', 'empty-note', `${type || 'A binary file'}: no preview here, download it to open it.`);
  }

  // sendMenu offers the fleet's other machines to send the file shown to
  // (POST .../send): it lands in the uploads folder there.
  function sendMenu(anchor) {
    const f = files.open;
    if (!f) return;
    const targets = S.peers.filter((p) => !p.self && p.id && p.id !== chat.host);
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Send to another machine'));
      if (!targets.length) el.append(h('p', 'menu-note', S.peers.length ? 'No other machine is on the network.' : 'No other machine found yet.'));
      for (const p of targets) {
        const st = hostState(p.id);
        el.append(mi({ custom: h('span', MACHINE_STATE[st] ? MACHINE_STATE[st][0] : 'dot off'), label: p.name || p.host || p.id, sub: 'lands in its uploads folder', onSelect: () => {
          close();
          sendFileTo(p);
        } }));
      }
    }, { label: 'Send to', alignRight: true });
  }

  async function sendFileTo(p) {
    const f = files.open;
    if (!f) return;
    const seq = files.seq;
    const name = p.name || p.host || p.id;
    files.sendNote = { text: `Sending ${baseName(f.path)} to ${name}…` };
    renderFileView();
    try {
      const r = await api('POST', chatAPI('send'), { path: f.path, host: p.id });
      if (seq === files.seq) files.sendNote = { text: `Sent to ${r.name || name}: ${r.path}` };
    } catch (e) {
      if (seq === files.seq) files.sendNote = { text: `Sending to ${name} failed: ${e.message}`, kind: 'error' };
    }
    if (seq === files.seq) renderFileView();
  }

  function downloadFile() {
    const r = files.shown;
    if (!r || r.big || r.error) return;
    const url = URL.createObjectURL(new Blob([b64bytes(r.data)], { type: 'application/octet-stream' }));
    const a = h('a', { href: url, download: baseName(r.path), hidden: true });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  }

  // ---- the terminal: its screen, polled while the panel's terminal tab
  // or the chat's dialog card shows it

  const SCREEN_POLL_MS = 1500;
  const screen = { timer: 0, text: '', busy: false, login: null, loginSeq: 0 };

  // screenNeeded starts or stops the polling as the screen shows or not.
  function screenNeeded() {
    const needed = chat.open && chatLive() && (!!C.dlgPre || (insp.open && insp.tab === 'terminal' && !$('insp').hidden));
    if (needed && !screen.timer && !screen.busy) refreshScreen();
    if (!needed) stopScreen();
  }

  function stopScreen() {
    clearTimeout(screen.timer);
    screen.timer = 0;
  }

  async function refreshScreen() {
    stopScreen();
    if (!chat.open || !chatLive() || screen.busy) return;
    const seq = chat.seq;
    screen.busy = true;
    let text;
    let login = null;
    try {
      const r = await api('GET', chatAPI('screen'));
      text = (r && r.screen) || '';
      login = (r && r.login) || null;
    } catch (e) {
      text = e.message;
    }
    screen.busy = false;
    if (seq !== chat.seq) return;
    screen.text = text;
    screen.login = login;
    screen.loginSeq = seq;
    paintLogin();
    for (const pre of [C.dlgPre, insp.panes && insp.tab === 'terminal' ? insp.panes.terminal.pre : null]) {
      if (!pre || !pre.isConnected) continue;
      // Dialogs and the prompt are at the bottom: stay there, unless
      // scrolled up to read.
      const atBottom = !pre.textContent || pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
      pre.textContent = text;
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    }
    const needed = chatLive() && (!!C.dlgPre || (insp.open && insp.tab === 'terminal' && !$('insp').hidden));
    if (needed) screen.timer = setTimeout(refreshScreen, SCREEN_POLL_MS);
  }

  // ----------------------------------------------------------- new session
  //
  // Start a session: a folder on a machine, the agent CLI with its model
  // and effort, and a prompt, with images and videos. The machine, folder,
  // checkout and options sit above the prompt, the agent's pickers under
  // it. Other machines are reached through /api/hosts/.
  //
  // A folder starts in the most specific root around it, however it was
  // found (a recent session, a root, inside another root), so it always
  // gets the same agents and trust.

  const launch = {
    host: '', // server id; '' = this daemon
    loaded: null, // the host whose adapters and roots were loaded (null: none yet)
    adapters: null, // [{id, name, available, images, models, efforts}] of the machine
    roots: null, // roots of another machine (this one's are S.roots)
    path: '', // chosen folder, absolute; '' = none yet
    want: null, // {path} or {root, sub} asked for before the machine's roots are known
    home: '', // the machine's home folder, as its listings say
    look: '', // folder the picker looks into; '' = recent folders and roots
    q: '', // the folder search
    dirs: new Map(), // path -> {path, entries, truncated, error}, entries null while listed
    adapter: '',
    adapterRoot: '', // host/root whose provider was restored
    gen: 0, // bumped for another machine: stale replies are dropped
    busy: false, // a start is in flight
    making: false, // a new folder is being made
    error: '',
    then: '', // what to show once started, if a shortcut says so
    iso: '', // checkout: '' auto, worktree, pinned (kept, see LAUNCH_OPTS)
    sandbox: '', // '' the server's default, docker, none (kept)
    thenPref: 'chat', // chat, screen, stay (kept)
    name: '',
    branch: '',
    images: [], // {data, url, name} to send with the prompt; no data yet while read
    videos: [], // uploaded to the machine for the prompt
  };

  const launchAPI = (path) => (launch.host ? `/api/hosts/${encodeURIComponent(launch.host)}/${path}` : '/api/' + path);
  const launchRoots = () => (launch.host ? launch.roots || [] : S.roots);
  const inRoot = (r, sub) => (sub ? joinPath(r.path, sub) : r.path);
  const within = (path, dir) => path === dir || path.startsWith(joinPath(dir, ''));
  const parentOf = (p) => (p.lastIndexOf('/') > 0 ? p.slice(0, p.lastIndexOf('/')) : p.length > 1 ? '/' : '');

  // placeOf is where a session in path starts: {root, sub} of the most
  // specific root around it, or null outside every root.
  function placeOf(path) {
    let best = null;
    for (const r of launchRoots()) {
      if (path && within(path, r.path) && (!best || r.path.length > best.path.length)) best = r;
    }
    return best && { root: best, sub: path === best.path ? '' : path.slice(joinPath(best.path, '').length) };
  }

  const launchPlace = () => placeOf(launch.path);
  const launchRoot = () => (launchPlace() || {}).root;

  // tilde writes path from the machine's home as ~, for a path typed on.
  const tilde = (p) => (launch.home && within(p, launch.home) ? '~' + p.slice(launch.home.replace(/\/$/, '').length) : p);

  // launchFor sets up a new session in folder of machine host.
  function launchFor(host, folder) {
    if (launch.host !== host) {
      dropLaunchVideos('the videos went to the other machine: attach them again');
      launch.host = host;
      launch.loaded = null;
    }
    launch.want = folder ? { path: folder } : null;
    launch.path = folder && placeOf(folder) ? folder : launch.path;
    launch.look = '';
    launch.q = '';
  }

  // Checkout, sandbox and what to show once started, kept for the next
  // session.
  const LAUNCH_OPTS = 'fleet.launch.options';
  (() => {
    const o = load(LAUNCH_OPTS, {});
    if (['', 'worktree', 'pinned'].includes(o.iso)) launch.iso = o.iso;
    if (['', 'docker', 'none'].includes(o.sandbox)) launch.sandbox = o.sandbox;
    if (['chat', 'screen', 'stay'].includes(o.then)) launch.thenPref = o.then;
  })();
  const saveLaunchOptions = () => save(LAUNCH_OPTS, { iso: launch.iso, sandbox: launch.sandbox, then: launch.thenPref });

  // closeLaunch resets the composer after a start.
  function closeLaunch() {
    launch.images = [];
    launch.videos = dropVideos(launch.videos);
    launch.name = '';
    launch.branch = '';
    if (N.ta) N.ta.value = '';
    save('fleet.launch.prompt', null);
  }

  function dropLaunchVideos(why) {
    if (!launch.videos.length) return;
    toast(why, 'warn');
    launch.videos = dropVideos(launch.videos);
  }

  // loadLaunchHost loads the adapters and roots of the chosen machine.
  async function loadLaunchHost() {
    const host = launch.host;
    const gen = ++launch.gen;
    launch.loaded = host;
    launch.adapters = null;
    launch.adapterRoot = '';
    launch.roots = null;
    launch.home = '';
    launch.dirs = new Map();
    launch.look = '';
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

  // launchRootsChanged lists the roots for the search, and chooses the
  // last folder a session started in if the chosen one is outside them.
  function launchRootsChanged() {
    if (launch.want && (!launch.host || launch.roots)) {
      const w = launch.want;
      if (w.path && placeOf(w.path)) launch.path = w.path;
      else if (w.root) {
        const r = launchRoots().find((x) => x.name === w.root);
        if (r) launch.path = inRoot(r, w.sub);
      }
      launch.want = null;
    }
    if (!launch.want && !launchPlace()) launch.path = launchRecent(1)[0] || '';
    if (launch.look && !placeOf(launch.look)) launch.look = '';
    for (const r of launchRoots()) listDir(r.path);
    renderLaunch();
  }

  // listDir lists the subfolders of a folder into launch.dirs, once per
  // machine: the search looks through every listing.
  async function listDir(path) {
    if (!path || launch.dirs.has(path)) return;
    const gen = launch.gen;
    const d = { path, entries: null, truncated: false, error: '' };
    launch.dirs.set(path, d);
    try {
      const dir = await api('GET', launchAPI('fs?' + new URLSearchParams({ path })));
      d.entries = dir.entries || [];
      d.truncated = !!dir.truncated;
      if (dir.home && gen === launch.gen) launch.home = dir.home;
    } catch (e) {
      d.entries = [];
      d.error = e.message;
    }
    if (gen === launch.gen) renderLaunch();
  }

  // launchRecent is the folders sessions started in on the chosen machine,
  // newest first: the daemon keeps its last 200 sessions, so every browser
  // and phone sees the same ones.
  const LAUNCH_RECENT = 5;

  function launchRecent(max) {
    const agents = agentsOf(launch.host);
    const out = [];
    for (const a of [...agents.values()].sort((x, y) => y.createdAtMs - x.createdAtMs)) {
      if (a.isolation === 'clone' || !a.path || out.includes(a.path) || !placeOf(a.path)) continue; // a clone runs outside the root
      out.push(a.path);
      if (out.length === max) break;
    }
    return out;
  }

  function selectLaunchHost(id) {
    if (launch.host === id) return;
    dropLaunchVideos('the videos went to the other machine: attach them again');
    launch.host = id;
    launch.path = '';
    launch.want = null;
    launch.q = '';
    loadLaunchHost();
  }

  // Model and effort picks, per agent CLI, kept for the next session.
  const LAUNCH_PICKS = 'fleet.launch.models';
  const launchPicks = () => load(LAUNCH_PICKS, {}) || {};

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

  function saveLaunchPick(model, effort) {
    const picks = launchPicks();
    picks[launch.adapter] = { model, effort };
    save(LAUNCH_PICKS, picks);
    renderLaunch();
  }

  // launchImages reports whether the chosen agent CLI takes images with its
  // first prompt (daemons older than that say nothing).
  function launchImages() {
    const ad = (launch.adapters || []).find((a) => a.id === launch.adapter);
    return !!(ad && ad.images);
  }

  // launchImagesProblem says why the images or videos cannot go with the
  // session.
  function launchImagesProblem() {
    const vp = imagesProblem(launch.images) || videosProblem(launch.videos);
    if (vp) return vp;
    if (!launch.images.length) return '';
    if (!launchImages()) return `${launch.adapter || 'This agent'} takes no images at start: remove them, or choose another agent.`;
    if (!N.ta || (!N.ta.value.trim() && !launch.videos.length)) return 'Images go with a prompt: write one.';
    return '';
  }

  async function addLaunchFiles(files) {
    if (launch.busy) return;
    const gen = launch.gen;
    const alive = () => gen === launch.gen && U.view !== 'chat';
    // An agent that takes no images at start gets them as files.
    const upload = (fs, all = !launchImages()) => addVideos(fs, launch.videos, launchAPI('uploads'), launch.host ? hostName(launch.host) : '', () => {
      if (alive()) renderLaunch();
    }, all);
    upload(files);
    if (launchImages()) await readImages(files, launch.images, alive, renderLaunch, (fs) => upload(fs, true));
  }

  // Last successfully used provider, per machine and root folder. Session
  // history supplies it on other browsers; local storage survives pruning.
  const LAUNCH_PROVIDERS = 'fleet.launch.providers';
  let launchProviders = {};
  try {
    const saved = JSON.parse(localStorage.getItem(LAUNCH_PROVIDERS));
    if (saved && typeof saved === 'object' && !Array.isArray(saved)) launchProviders = saved;
  } catch {
    // Storage can be disabled; keep the choices for this page instead.
  }

  const providerRootKey = (host, root) => JSON.stringify([host || (S.server && S.server.id) || '', root.path]);

  function lastRootProvider(host, root) {
    const saved = launchProviders[providerRootKey(host, root)];
    const agents = host ? (S.remote.get(host) || { agents: new Map() }).agents : S.agents;
    let latest = saved && typeof saved.adapter === 'string'
      ? { adapter: saved.adapter, usedAtMs: Number(saved.usedAtMs) || 0 } : null;
    for (const a of agents.values()) {
      if (a.root !== root.name || !a.adapter) continue;
      if (!latest || a.createdAtMs > latest.usedAtMs) latest = { adapter: a.adapter, usedAtMs: a.createdAtMs || 0 };
    }
    return latest ? latest.adapter : '';
  }

  function rememberRootProvider(host, root, adapter) {
    launchProviders[providerRootKey(host, root)] = { adapter, usedAtMs: Date.now() };
    try {
      localStorage.setItem(LAUNCH_PROVIDERS, JSON.stringify(launchProviders));
    } catch {
      // The in-memory choice still lasts for this page.
    }
  }

  // Restore when the root changes, then keep explicit choices while folder
  // listings render. Unavailable or disallowed providers fall back to
  // Claude Code, Codex or the first installed CLI.
  function pickAdapter(list) {
    if (!launch.adapters) return;
    const root = launchRoot();
    const key = root ? providerRootKey(launch.host, root) : '';
    if (key !== launch.adapterRoot) {
      launch.adapterRoot = key;
      launch.adapter = root ? lastRootProvider(launch.host, root) : '';
    }
    const ok = (id) => list.some((a) => a.id === id && a.available);
    if (ok(launch.adapter)) return;
    launch.adapter = ['claude', 'codex', 'gemini'].find(ok) || (list.find((a) => a.available) || {}).id || '';
  }

  function renderLaunch() {
    if (!N.root || !N.root.isConnected) return;
    const root = launchRoot();
    const notJoined = launch.host && hostState(launch.host) === 'not_joined';
    const allowed = (launch.adapters || []).filter((a) => !root || !root.adapters.length || root.adapters.includes(a.id));
    pickAdapter(allowed);
    paintContext(root);
    // agent, model, effort
    const ad = (launch.adapters || []).find((a) => a.id === launch.adapter);
    fill(N.agentBtn, launch.adapter ? logo(launch.adapter) : icon('bot'), h('span', 'lbl', ad ? ad.name || ad.id : !launch.adapters ? 'Loading…' : 'No agent'), icon('chevron-down', 'chev'));
    N.agentBtn.setAttribute('aria-label', 'Agent: ' + (ad ? ad.name || ad.id : 'none'));
    N.agentBtn.disabled = !launch.adapters;
    const p = launchPick();
    N.modelBtn.hidden = !p || !(p.ad.models || []).length;
    N.effortBtn.hidden = !p || !p.efforts.length;
    if (p) {
      fill(N.modelBtn, h('span', 'lbl', p.model ? p.model.label : 'Default model'), icon('chevron-down', 'chev'));
      N.modelBtn.setAttribute('aria-label', 'Model: ' + (p.model ? p.model.label : 'the CLI’s default'));
      fill(N.effortBtn, effortBars(p.effort ? effortLevel(p.effort.id, p.ad.efforts) : 0), h('span', 'lbl', p.effort ? p.effort.label : 'Default effort'), icon('chevron-down', 'chev'));
      N.effortBtn.setAttribute('aria-label', 'Effort: ' + (p.effort ? p.effort.label : 'the CLI’s default'));
    }
    // A shell takes no prompt and no images: its terminal takes the typing.
    const shell = launch.adapter === 'shell';
    N.ta.disabled = shell;
    N.ta.placeholder = shell ? 'A shell takes no prompt: type into its terminal once it starts'
      : keyboard() ? `What should the agent do? · ${MOD}+Enter starts` : 'What should the agent do?';
    N.attach.disabled = launch.busy || shell;
    N.attach.title = launchImages() || !launch.adapter ? 'Attach images or files (or paste, or drop them)' : `Attach files (${launch.adapter} takes no images at start: they go as files)`;
    renderThumbs(N.thumbs, launch.images, launch.videos);
    // what is in the way, or the keys
    const problem = launchImagesProblem();
    let say = '';
    let warn = true;
    if (notJoined) say = `${hostName(launch.host)} does not have this fleet’s key yet: see Devices to add it.`;
    else if (launch.error) say = launch.error;
    else if (problem) say = problem;
    else if (launch.adapters && !launchRoots().length) say = `No folders on ${hostName(launch.host)} yet: make one from the folder button.`;
    else if (launch.adapters && !allowed.some((a) => a.available)) say = allowed.length ? 'No agent CLI here is installed.' : 'No agent CLI is allowed in this folder.';
    else {
      warn = false;
      say = keyboard() ? `${MOD}+Enter starts · ${MOD}+Shift+Enter starts and shows the terminal · ${MOD}+K folder · ${MAC ? '⌥' : 'Alt+'}1–9 agent` : '';
    }
    N.status.className = 'newv-hint' + (warn ? ' warn' : '');
    fill(N.status, say ? h('span', null, say) : null,
      !warn ? h('span', null, shell ? 'Start opens the shell’s terminal.' : 'Without a prompt the agent starts and waits for your first message.') : null);
    N.start.disabled = !root || !launch.adapter || launch.busy || notJoined || !!problem;
    fill(N.start, launch.busy ? 'Starting…' : 'Start', keyboard() ? h('kbd', { 'aria-hidden': 'true' }, MOD + '↵') : null);
    // recent folders
    const recent = launchRecent(LAUNCH_RECENT);
    fill(N.recent, recent.length ? [h('span', 'recent-l', 'Recent'), recent.map((p2) => h('button', {
      type: 'button', class: 'recent-chip', 'aria-pressed': String(p2 === launch.path), title: shortPath(p2),
      onclick: () => {
        launch.path = p2;
        renderLaunch();
        if (keyboard()) N.ta.focus();
      },
    }, icon('folder'), (launchRoots().find((r) => r.path === p2) || {}).name || baseName(p2)))] : null);
    if (MENU && MENU.repaint && MENU.o.label === 'Folder') MENU.repaint();
  }

  // paintContext draws the chips above the prompt: machine, folder,
  // checkout, options.
  function paintContext(root) {
    const items = [];
    const hosts = pickerHosts();
    if (hosts.length > 1 || launch.host) {
      const st = hostState(launch.host);
      items.push(h('button', { type: 'button', class: 'ctx-chip', 'aria-haspopup': 'menu', 'aria-expanded': 'false', 'aria-label': 'Machine: ' + hostName(launch.host), onclick: (ev) => launchMachineMenu(ev.currentTarget) },
        h('span', MACHINE_STATE[st][0]), hostName(launch.host), icon('chevron-down', 'chev')));
    }
    const row = launch.path ? folderRow(launch.path) : null;
    items.push(h('button', {
      type: 'button', id: 'ns-folder', class: 'ctx-chip' + (launch.path ? '' : ' empty'), 'aria-haspopup': 'dialog', 'aria-expanded': 'false',
      'aria-label': 'Folder: ' + (launch.path ? shortPath(launch.path) : 'none'), title: root ? `${launch.path} · root ${root.name}` : `Choose the folder to start in (${MOD}+K)`,
      onclick: (ev) => folderPicker(ev.currentTarget),
    }, icon('folder'), h('span', 'path', launch.path ? shortPath(launch.path) : !launch.adapters ? 'Loading…' : 'Choose a folder'),
    row && row.git ? h('span', 'tag', 'git') : null, icon('chevron-down', 'chev')));
    const iso = { '': 'Auto checkout', worktree: 'Own worktree', pinned: 'In the folder' }[launch.iso];
    items.push(h('button', { type: 'button', class: 'ctx-chip', 'aria-haspopup': 'menu', 'aria-expanded': 'false', 'aria-label': 'Checkout: ' + iso, onclick: (ev) => isoMenu(ev.currentTarget) }, icon('branch'), iso, icon('chevron-down', 'chev')));
    const set = [];
    if (launch.sandbox === 'docker') set.push('Docker');
    if (launch.sandbox === 'none') set.push('No sandbox');
    if (launch.thenPref === 'screen') set.push('then terminal');
    if (launch.thenPref === 'stay') set.push('then stay');
    if (launch.name) set.push(launch.name);
    if (launch.branch) set.push('⎇ ' + launch.branch);
    items.push(h('button', { type: 'button', class: 'ctx-chip' + (set.length ? ' set' : ''), 'aria-haspopup': 'dialog', 'aria-expanded': 'false', 'aria-label': 'Options' + (set.length ? ': ' + set.join(', ') : ''), onclick: (ev) => optionsPop(ev.currentTarget) },
      icon('sliders'), set.length ? set.join(' · ') : 'Options'));
    fill(N.ctx, items);
  }

  // folderRow describes the row of path: its name (a root's own name, or
  // the folder's), where it is, and whether it is a git repo (if not given,
  // as its parent's listing says).
  function folderRow(path, git) {
    const root = launchRoots().find((r) => r.path === path);
    const name = root ? root.name : baseName(path) || '/';
    if (git == null) {
      const d = launch.dirs.get(parentOf(path));
      git = d && d.entries && (d.entries.find((e) => e.name === baseName(path)) || {}).git;
    }
    return { path, name, git: !!git, where: shortPath(path) };
  }

  const LAUNCH_FOUND_MAX = 50;

  // launchRows is what the folder list shows, as plain data.
  function launchRows(notJoined) {
    const note = (text) => ({ note: text });
    if (notJoined) return [note('Not in this fleet yet')];
    if (!launch.adapters) return [note('Loading…')];
    const roots = launchRoots();
    const q = launch.q.trim().toLowerCase();
    if (!roots.length && !q) return [note('No folders on this machine yet: make one below.')];
    if (q) return searchRows(q);
    const l = launch.look;
    if (l) {
      const d = launch.dirs.get(l);
      const rows = [{ ...folderRow(l), where: 'this folder', here: true }];
      if (!d || !d.entries) rows.push(note('Loading…'));
      else if (d.error) rows.push(note(d.error));
      else {
        for (const e of d.entries) rows.push({ ...folderRow(joinPath(l, e.name), e.git), where: '' });
        if (!d.entries.length) rows.push(note('No folders inside'));
        if (d.truncated) rows.push(note(`Only the first ${d.entries.length} folders are listed: search to find others`));
      }
      return rows;
    }
    // A folder is listed once: recent, else as a root.
    const recent = launchRecent(LAUNCH_RECENT);
    const rows = [];
    if (recent.length) rows.push({ head: 'Recent' }, ...recent.map((p) => folderRow(p)));
    const others = roots.filter((r) => !recent.includes(r.path)).map((r) => folderRow(r.path));
    others.sort((a, b) => a.name.localeCompare(b.name));
    if (others.length) rows.push({ head: `Folders on ${hostName(launch.host)}` }, ...others);
    return rows;
  }

  // searchRows finds folders whose name or path has every word of q:
  // folders of past sessions, roots, the folders in them and in every
  // folder looked into. A name that is nowhere can be made.
  function searchRows(q) {
    const words = q.split(/\s+/);
    const last = words[words.length - 1];
    const found = new Map();
    const addRow = (path, git, rank) => {
      if (found.has(path) || !placeOf(path)) return;
      const row = folderRow(path, git);
      const name = row.name.toLowerCase();
      if (!words.every((w) => (name + ' ' + row.where.toLowerCase()).includes(w))) return;
      const score = name === last ? 0 : name.startsWith(last) ? 1 : name.includes(last) ? 2 : 3;
      found.set(path, { row, score, rank, depth: path.split('/').length });
    };
    const recent = launchRecent();
    recent.forEach((p, i) => addRow(p, null, i));
    for (const r of launchRoots()) addRow(r.path, null, recent.length);
    let listing = false;
    for (const d of launch.dirs.values()) {
      if (!d.entries) listing = true;
      else for (const e of d.entries) addRow(joinPath(d.path, e.name), e.git, recent.length);
    }
    const hits = [...found.values()].sort((a, b) => a.score - b.score || a.rank - b.rank || a.depth - b.depth || a.row.name.localeCompare(b.row.name));
    const rows = hits.slice(0, LAUNCH_FOUND_MAX).map((x) => x.row);
    if (hits.length > LAUNCH_FOUND_MAX) rows.push({ note: `${hits.length - LAUNCH_FOUND_MAX} more: type more to narrow them down` });
    if (listing) rows.push({ note: 'Still listing folders…' });
    else if (!hits.length) rows.push({ note: `No folder matches “${q}”: look inside one (›) to search its folders too` });
    const typed = launch.q.trim();
    if (words.length === 1 && !hits.some((x) => x.score === 0)) rows.push({ make: /^[~/]/.test(typed) ? typed : joinPath('~', typed) });
    return rows;
  }

  // The folder picker: recent folders and roots, the search, or the folders
  // in one folder. A click on a row chooses it, › looks inside; New folder
  // makes one, as a root.
  function folderPicker(anchor) {
    let making = null; // the new folder's path while its form shows
    let note = null; // {text, err} under the form
    openMenu(anchor, (el, close, menu) => {
      const input = h('input', { type: 'search', placeholder: keyboard() ? 'Search folders · ↓ to move · → to look inside' : 'Search folders', 'aria-label': 'Search folders', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', value: launch.q });
      const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Folder you look into' });
      const list = h('div', { role: 'listbox', 'aria-label': 'Folders' });
      const foot = h('div', 'folders-foot');
      const choose = (path) => {
        launch.path = path;
        launch.q = '';
        launch.look = '';
        close(false);
        renderLaunch();
        if (keyboard()) N.ta.focus();
      };
      const lookInto = (path) => {
        launch.look = path;
        launch.q = '';
        input.value = '';
        if (path) listDir(path);
        paint();
        if (keyboard()) input.focus();
      };
      const paint = () => {
        const notJoined = launch.host && hostState(launch.host) === 'not_joined';
        const focusedKey = list.contains(document.activeElement) ? document.activeElement.dataset.key : '';
        crumbs.hidden = !launch.look || !!launch.q.trim();
        if (!crumbs.hidden) {
          const chain = [];
          for (let p = launch.look; p && placeOf(p); p = parentOf(p)) {
            chain.unshift(p);
            if (p === '/') break;
          }
          fill(crumbs, h('button', { type: 'button', onclick: () => lookInto('') }, 'All'),
            chain.map((p, i) => [' / ', p === launch.look ? h('span', { 'aria-current': 'location' }, i ? baseName(p) : shortPath(p)) : h('button', { type: 'button', onclick: () => lookInto(p) }, i ? baseName(p) : shortPath(p))]));
        }
        fill(list, launchRows(notJoined).map((row) => {
          if (row.head) return h('div', 'menu-l', row.head);
          if (row.note) return h('p', 'menu-note', row.note);
          if (row.make) {
            return mi({ icon: 'folder-plus', cls: 'make', label: `New folder ${row.make}`, sub: 'becomes a root, ready to start in', onSelect: () => {
              making = row.make;
              note = null;
              paint();
            } });
          }
          const chosen = row.path === launch.path;
          const b = h('button', { type: 'button', class: 'mi', role: 'option', 'aria-selected': String(chosen), 'data-key': row.path, title: shortPath(row.path), onclick: () => choose(row.path) },
            icon('folder'), h('span', 'grow', h('span', null, row.name), h('span', 'path', row.where)), row.git ? h('span', 'tag', 'git') : null, chosen ? icon('check', 'check') : null);
          return h('div', 'frow', b, row.here ? null : iconBtn('chevron-right', 'Look inside ' + row.name, () => lookInto(row.path), { cls: 'look', attrs: { tabindex: '-1' } }));
        }));
        if (focusedKey) [...list.querySelectorAll('[data-key]')].find((x) => x.dataset.key === focusedKey)?.focus();
        if (making != null) {
          const p = h('input', { class: 'input mono', value: making, placeholder: '~/new-project', 'aria-label': 'New folder', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
          const trust = h('input', { type: 'checkbox', checked: true });
          const create = async () => {
            const path = p.value.trim();
            if (launch.making || !path || /^~?\/*$/.test(path)) return p.focus();
            note = { text: 'Creating…' };
            making = path;
            paint();
            const root = await createFolder(path, trust.checked, (text) => {
              note = { text, err: true };
            });
            if (root) choose(root.path);
            else if (MENU === menu) paint();
          };
          p.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter') {
              ev.preventDefault();
              create();
            } else if (ev.key === 'Escape') {
              ev.preventDefault();
              ev.stopPropagation();
              making = null;
              paint();
            }
          });
          fill(foot, h('div', 'newfolder',
            h('div', 'row2', p, h('button', { type: 'button', class: 'btn primary sm', disabled: launch.making, onclick: create }, launch.making ? 'Creating…' : 'Create'),
              iconBtn('x', 'Cancel the new folder', () => { making = null; paint(); })),
            h('label', 'check-row', trust, h('span', null, h('strong', null, 'Trust it: '), 'agents skip the “Do you trust this folder?” prompt. Safe for a new, empty folder.')),
            note ? h('p', 'menu-note' + (note.err ? ' err' : ''), note.text) : h('p', 'menu-note', `Made on ${hostName(launch.host)}, added as a root, and chosen to start in.`)));
          if (!launch.making) setTimeout(() => p.focus());
        } else {
          fill(foot, mi({ icon: 'folder-plus', label: 'New folder', sub: 'becomes a root, ready to start in', onSelect: () => {
            making = joinPath(launch.look ? tilde(launch.look) : '~', '');
            note = null;
            paint();
          } }));
        }
      };
      input.addEventListener('input', () => {
        launch.q = input.value;
        paint();
      });
      input.addEventListener('keydown', (ev) => {
        const first = list.querySelector('[data-key], .mi');
        if (ev.key === 'ArrowDown' && first) {
          ev.preventDefault();
          first.focus();
        } else if (ev.key === 'Enter') {
          ev.preventDefault(); // with a search, choose the first match
          if (launch.q.trim() && first) first.click();
        } else if (ev.key === 'Backspace' && !input.value && launch.look) {
          ev.preventDefault();
          const up = parentOf(launch.look);
          lookInto(up && placeOf(up) ? up : '');
        }
      });
      list.addEventListener('keydown', (ev) => {
        const b = ev.target.closest('[data-key]');
        if (!b) return;
        if (ev.key === 'ArrowRight') {
          const look = b.parentElement.querySelector('.look');
          if (look) {
            ev.preventDefault();
            look.click();
          }
        } else if ((ev.key === 'ArrowLeft' || ev.key === 'Backspace') && launch.look) {
          ev.preventDefault();
          const up = parentOf(launch.look);
          lookInto(up && placeOf(up) ? up : '');
        }
      });
      el.append(h('div', 'folders', h('label', 'search', icon('search', 'i-sm'), input), crumbs, list, foot));
      menu.repaint = paint;
      paint();
      // The chosen folder in view.
      setTimeout(() => list.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'center' }));
    }, { label: 'Folder', role: 'dialog', wide: true, focus: touch() ? null : 'input' });
  }

  // createFolder makes path on the chosen machine and adds it as a root
  // (its own root, so its agents and trust are its own). It returns the
  // root, or null after telling fail why.
  async function createFolder(path, trust, fail) {
    const host = launch.host;
    const gen = launch.gen;
    launch.making = true;
    let made = null;
    let root = null;
    try {
      made = await api('POST', launchAPI('fs'), { path });
      root = (await api('POST', launchAPI('roots'), { path: made.path, adapters: [], trust })).root;
    } catch (e) {
      fail(made ? `Made ${shortPath(made.path)}, but could not add it as a root: ${e.message}`
        : e.status === 405 ? `${hostName(host)} runs an older fleet: update it to make folders from here.` : e.message);
    }
    launch.making = false;
    if (gen !== launch.gen || !root) return null;
    // Known before the roots are sent again.
    const roots = host ? launch.roots : S.roots;
    if (roots && !roots.some((r) => r.name === root.name)) {
      roots.push(root);
      if (!host) invalidate('roots');
    }
    const parent = parentOf(root.path);
    if (launch.dirs.has(parent)) {
      launch.dirs.delete(parent);
      listDir(parent);
    }
    listDir(root.path);
    toast(`Made ${shortPath(root.path)}${host ? ' on ' + hostName(host) : ''}, a new root`);
    return root;
  }

  function launchMachineMenu(anchor) {
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Machine'));
      for (const x of pickerHosts()) {
        const st = hostState(x.id);
        el.append(mi({
          custom: h('span', MACHINE_STATE[st][0]), label: x.name, sub: x.id ? MACHINE_STATE[st][1] : 'this machine',
          checked: launch.host === x.id, disabled: st === 'unreachable',
          onSelect: () => {
            close();
            selectLaunchHost(x.id);
          },
        }));
      }
    }, { label: 'Machine' });
  }

  function agentMenu(anchor) {
    const root = launchRoot();
    const list = launch.adapters || [];
    const usage = S.usage.get(launch.host || (S.server && S.server.id)) || [];
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Agent on ' + hostName(launch.host)));
      let n = 0;
      for (const a of list) {
        const allowed = !root || !root.adapters.length || root.adapters.includes(a.id);
        const u = usage.find((x) => x.id === a.id);
        const lim = u && u.limits && (u.limits.find((l) => /week/i.test(l.label)) || u.limits[0]);
        const full = u && u.limits && u.limits.find((l) => l.percent >= 100);
        const ok = a.available && allowed;
        if (ok) n++;
        el.append(mi({
          logo: a.id, label: a.name || a.id,
          sub: !a.available ? 'not installed' : !allowed ? `not allowed in ${root.name}` : full ? `${full.label} limit reached${full.resetsAtMs ? ' · resets ' + clock(full.resetsAtMs) : ''}` : u && u.plan ? u.plan : '',
          right: ok && lim ? usageMini(lim) : null,
          kbd: ok && keyboard() && n <= 9 ? (MAC ? '⌥' : 'Alt+') + n : null,
          checked: launch.adapter === a.id, disabled: !ok,
          onSelect: () => {
            close();
            launch.adapter = a.id;
            renderLaunch();
          },
        }));
      }
      if (!list.length) el.append(h('p', 'menu-note', launch.adapters ? 'No agent CLI on this machine.' : 'Loading…'));
    }, { label: 'Agent' });
  }

  function usageMini(l) {
    const pct = Math.max(0, Math.min(100, l.percent));
    const fillEl = h('span', 'meter-fill' + (pct >= 90 ? ' full' : pct >= 70 ? ' warn' : ''));
    fillEl.style.width = pct + '%';
    return h('span', { class: 'usage-mini', title: `${l.label}: ${Math.round(pct)}% used` }, h('span', 'meter-bar', fillEl), `${Math.round(pct)}%`);
  }

  function launchModelMenu(anchor) {
    const p = launchPick();
    if (!p) return;
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', (p.ad.name || p.ad.id) + ' model'));
      el.append(mi({ label: 'Default', sub: 'the CLI’s own setting', checked: !p.model, onSelect: () => { close(); saveLaunchPick('', p.effort ? p.effort.id : ''); } }));
      for (const m of p.ad.models || []) {
        el.append(mi({ label: m.label, checked: !!p.model && p.model.id === m.id, onSelect: () => {
          close();
          saveLaunchPick(m.id, p.effort && m.efforts.includes(p.effort.id) ? p.effort.id : '');
        } }));
      }
      el.append(h('div', 'menu-sep'), h('p', 'menu-note', 'Kept for this agent’s next sessions.'));
    }, { label: 'Model' });
  }

  function launchEffortMenu(anchor) {
    const p = launchPick();
    if (!p) return;
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Effort'));
      el.append(mi({ label: 'Default', sub: 'the CLI’s own setting', checked: !p.effort, onSelect: () => { close(); saveLaunchPick(p.model ? p.model.id : '', ''); } }));
      for (const e of p.efforts) {
        el.append(mi({ custom: effortBars(effortLevel(e.id, p.ad.efforts)), label: e.label, checked: !!p.effort && p.effort.id === e.id, onSelect: () => {
          close();
          saveLaunchPick(p.model ? p.model.id : '', e.id);
        } }));
      }
    }, { label: 'Effort' });
  }

  function isoMenu(anchor) {
    openMenu(anchor, (el, close) => {
      el.append(h('div', 'menu-l', 'Checkout'));
      for (const [v, text, sub] of [['', 'Auto', 'An own worktree in git repositories, else the folder itself'], ['worktree', 'Own worktree', 'A git worktree on a branch of its own'], ['pinned', 'The folder itself', 'Works in the folder, next to you and other agents']]) {
        el.append(mi({ label: text, sub, checked: launch.iso === v, onSelect: () => {
          launch.iso = v;
          saveLaunchOptions();
          close();
          renderLaunch();
        } }));
      }
    }, { label: 'Checkout' });
  }

  function optionsPop(anchor) {
    openMenu(anchor, (el) => {
      const radios = (key, choices) => h('div', { class: 'radio-chips', role: 'radiogroup', 'aria-label': key === 'sandbox' ? 'Sandbox' : 'Then' }, choices.map(([v, text, title]) =>
        h('label', { title }, h('input', { type: 'radio', name: 'opt-' + key, value: v, checked: launch[key] === v, onchange: () => {
          launch[key] = v;
          saveLaunchOptions();
          renderLaunch();
        } }), text)));
      const field = (key, placeholder, max) => h('input', { class: 'input mono', value: launch[key], placeholder, maxlength: max, spellcheck: 'false', autocomplete: 'off', 'aria-label': key === 'name' ? 'Name' : 'Branch', oninput: (ev) => {
        launch[key] = ev.target.value.trim();
        paintContext(launchRoot());
      } });
      el.append(h('div', 'pop-pad',
        h('div', 'pop-title', 'Options'),
        h('div', 'opt-grid',
          h('span', 'field-l', 'Sandbox'), radios('sandbox', [['', 'Server default', 'the server’s [sandbox] setting'], ['docker', 'Docker', 'in a Docker container'], ['none', 'None', 'right on the machine']]),
          h('span', 'field-l', 'Then'), radios('thenPref', [['chat', 'Show chat', 'open the conversation'], ['screen', 'Show terminal', 'open the conversation and its terminal'], ['stay', 'Stay here', 'stay on this page']]),
          h('span', 'field-l', 'Name'), field('name', 'generated', 64),
          h('span', 'field-l', 'Branch'), field('branch', 'fleet/<name>', 200)),
        h('p', 'field-hint', 'Sandbox and Then are kept for the next session; name and branch only for this one.')));
    }, { label: 'Options', role: 'dialog', wide: true, focus: touch() ? null : 'input' });
  }

  // ---- the view

  const N = {}; // the new-session view's elements

  function renderNew() {
    if (!N.root || !N.root.isConnected) buildNew();
    if (launch.loaded !== launch.host) loadLaunchHost();
    else if (launch.want && (!launch.host || launch.roots)) launchRootsChanged();
    else renderLaunch();
  }

  function buildNew() {
    checkHosts();
    const lead = leadBtn();
    N.ta = h('textarea', { id: 'ns-prompt', rows: 4, spellcheck: 'true', 'aria-label': 'What should the agent do?', placeholder: keyboard() ? `What should the agent do? · ${MOD}+Enter starts` : 'What should the agent do?' });
    N.ta.value = load('fleet.launch.prompt', '') || '';
    N.ctx = h('div', 'ctx');
    N.thumbs = h('div', 'attach-list');
    N.file = h('input', { type: 'file', multiple: true, hidden: true, tabindex: '-1', 'aria-hidden': 'true' });
    N.attach = h('button', { type: 'button', class: 'cpick icon-only', 'aria-label': 'Attach images or files', onclick: () => N.file.click() }, icon('clip'));
    N.agentBtn = h('button', { type: 'button', class: 'cpick agent', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => agentMenu(ev.currentTarget) });
    N.modelBtn = h('button', { type: 'button', class: 'cpick model', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => launchModelMenu(ev.currentTarget) });
    N.effortBtn = h('button', { type: 'button', class: 'cpick effort', 'aria-haspopup': 'menu', 'aria-expanded': 'false', onclick: (ev) => launchEffortMenu(ev.currentTarget) });
    N.start = h('button', { type: 'submit', class: 'start-btn' }, 'Start');
    N.status = h('div', { class: 'newv-hint', role: 'status', 'aria-live': 'polite' });
    N.prog = sendBar();
    N.recent = h('div', 'recent');
    const form = h('form', { class: 'composer', novalidate: true, onsubmit: (ev) => startSession(ev) },
      N.ctx, N.prog.el, N.thumbs, N.ta, h('div', 'ctoolbar', N.attach, N.agentBtn, N.modelBtn, N.effortBtn, h('span', 'grow'), N.start), N.file);
    N.root = h('div', 'view',
      lead || small() ? h('div', 'topbar', lead, h('h1', null, 'New session')) : null,
      h('div', 'newv', h('div', 'newv-in',
        h('h1', { class: small() ? 'sr-only' : null }, 'Start a session'),
        h('p', { class: 'newv-sub' + (small() ? ' sr-only' : '') }, 'Pick where and with which agent, then say what to do.'),
        form, N.status, N.recent)));
    fill($('main'), N.root);
    const fit = () => {
      N.ta.style.height = 'auto';
      N.ta.style.height = Math.min(N.ta.scrollHeight + 2, viewHeight() * 0.4) + 'px';
    };
    N.ta.addEventListener('input', () => {
      fit();
      save('fleet.launch.prompt', N.ta.value || null);
      if (launch.images.length) renderLaunch(); // images need a prompt
    });
    N.ta.addEventListener('paste', (ev) => {
      const files = mediaFiles(ev.clipboardData);
      if (!files.length) return;
      ev.preventDefault();
      addLaunchFiles(files);
    });
    N.file.addEventListener('change', () => {
      addLaunchFiles(N.file.files);
      N.file.value = '';
    });
    N.thumbs.addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-image], button[data-video]');
      if (!b || launch.busy) return;
      if (b.dataset.video) dropVideos(launch.videos.splice(Number(b.dataset.video), 1));
      else launch.images.splice(Number(b.dataset.image), 1);
      renderLaunch();
      if (keyboard()) N.ta.focus();
    });
    form.addEventListener('dragover', (ev) => {
      if (ev.dataTransfer && [...ev.dataTransfer.types].includes('Files')) {
        ev.preventDefault();
        form.classList.add('drag');
      }
    });
    form.addEventListener('dragleave', () => form.classList.remove('drag'));
    form.addEventListener('drop', (ev) => {
      form.classList.remove('drag');
      if (!ev.dataTransfer || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      addLaunchFiles(ev.dataTransfer.files);
    });
    requestAnimationFrame(fit);
    if (keyboard()) setTimeout(() => N.ta.focus({ preventScroll: true }));
  }

  async function startSession(ev) {
    if (ev) ev.preventDefault();
    const place = launchPlace();
    if (!place || !launch.adapter || launch.busy || launchImagesProblem()) return;
    const host = launch.host;
    const adapter = launch.adapter;
    const then = launch.then || launch.thenPref || 'chat';
    launch.then = '';
    launch.busy = true;
    launch.error = '';
    renderLaunch();
    const pick = launchPick();
    const images = launch.images.map((im) => im.data);
    try {
      const body = {
        model: (pick && pick.model && pick.model.id) || '',
        effort: (pick && pick.effort && pick.effort.id) || '',
        adapter,
        root: place.root.name,
        path: place.sub,
        prompt: withVideos($('ns-prompt').value, launch.videos),
        name: (launch.name || '').trim(),
        branch: (launch.branch || '').trim(),
        isolation: launch.iso || '',
        sandbox: launch.sandbox || '',
        images,
      };
      let r;
      if (images.length) {
        N.prog.set(0, images.length);
        r = await apiUpload('POST', launchAPI('agents'), body, (frac) => N.prog.set(frac, images.length));
        N.prog.set(null);
      } else r = await api('POST', launchAPI('agents'), body);
      rememberRootProvider(host, place.root, adapter);
      launch.busy = false;
      closeLaunch();
      toast(`Started ${r.agent.name}${host ? ' on ' + hostName(host) : ''}`);
      if (host) {
        const rem = S.remote.get(host) || { agents: new Map(), fails: 0 };
        rem.agents.set(r.agent.id, r.agent);
        S.remote.set(host, rem);
        invalidate('agents');
      }
      if (then !== 'stay') openChat(host, r.agent.id, r.agent, { screen: then === 'screen' });
      else renderLaunch();
      return;
    } catch (e) {
      if (images.length) N.prog.set(null);
      launch.error = e.message;
    }
    launch.busy = false;
    renderLaunch();
  }

  // wireLaunch: anywhere in the new-session view, Ctrl/Cmd+Enter starts the
  // session (with Shift it shows the terminal too), Ctrl/Cmd+K opens the
  // folders, Alt+1–9 chooses the agent CLI.
  function wireLaunch() {
    document.addEventListener('keydown', (ev) => {
      if (ev.isComposing || ev.defaultPrevented || !admin || !N.root || !N.root.isConnected) return;
      if (document.querySelector('dialog[open]')) return;
      if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) {
        ev.preventDefault();
        launch.then = ev.shiftKey ? 'screen' : '';
        startSession();
      } else if (ev.code === 'KeyK' && (ev.ctrlKey || ev.metaKey) && !ev.altKey && !ev.shiftKey) {
        ev.preventDefault();
        if (!(MENU && MENU.o.label === 'Folder')) folderPicker($('ns-folder'));
      } else if (ev.altKey && !ev.ctrlKey && !ev.metaKey && /^Digit[1-9]$/.test(ev.code)) {
        const root = launchRoot();
        const ok = (launch.adapters || []).filter((a) => a.available && (!root || !root.adapters.length || root.adapters.includes(a.id)));
        const a = ok[Number(ev.code.slice(5)) - 1];
        if (!a) return;
        ev.preventDefault();
        launch.adapter = a.id;
        renderLaunch();
      }
    });
  }

  // ---------------------------------------------------------------- devices
  //
  // The fleet's machines (how each is doing, its agent CLIs and their plan
  // usage, its folders) and the apps paired with this one. Everywhere else a
  // machine is a dot with its name.

  const rootsUI = { q: new Map(), all: new Set(), confirm: '' }; // per machine: filter, all shown; root awaiting removal
  let joinCmd = '';
  let joinTimer = 0;

  function renderDevices() {
    const main = $('main');
    const page = main.querySelector('.page');
    const top = page ? page.scrollTop : 0;
    const focused = main.contains(document.activeElement) ? document.activeElement.dataset.focus : '';
    const ms = machines();
    const up = ms.filter((m) => hostUp(m.state)).length;
    const lead = leadBtn();
    const devs = [...S.devices].sort((x, y) => (y.connected ? 1 : 0) - (x.connected ? 1 : 0) || (x.name || '').localeCompare(y.name || ''));
    const view = h('div', 'view',
      lead ? h('div', 'topbar', lead, h('h1', null, 'Devices')) : null,
      h('div', 'page', h('div', 'page-in',
        h('div', 'page-h',
          h('div', null, h('h1', { class: lead ? 'sr-only' : null }, 'Devices'),
            h('p', 'page-sub', `${up} of ${plural(ms.length, 'machine')} online` + (devs.length ? ` · ${devs.filter((d) => d.connected).length} of ${plural(devs.length, 'paired app')} online` : ''))),
          admin ? h('button', { type: 'button', class: 'btn', onclick: openJoin }, icon('plus'), 'Add a machine') : null),
        ms.map(machineCard),
        h('h2', 'sec-title', 'Paired apps', devs.length ? h('span', 'muted', String(devs.length)) : null),
        devs.length ? h('div', 'list-card', devs.map((d) => h('div', 'app-row',
          h('span', 'mach-ic', icon(/ios|android/.test(d.platform) ? 'phone' : d.platform === 'macos' ? 'laptop' : 'terminal'), h('span', 'dot' + (d.connected ? '' : ' off'))),
          h('div', 'grow', h('div', 'app-name', d.name || d.id), h('div', 'app-sub', PLATFORM[d.platform] || d.platform || 'unknown')),
          d.connected ? h('span', 'app-state on', 'online') : h('span', 'app-state', ['seen ', relTime(d.lastSeenAtMs, '', true)])))) : null,
        h('p', 'hint-box', 'Native apps and the fleet CLI on other computers pair once with ', h('code', null, 'fleet pair'), '. This web app needs no pairing.'))));
    fill(main, view);
    const p2 = main.querySelector('.page');
    if (p2) p2.scrollTop = top;
    if (focused) {
      const el = main.querySelector(`[data-focus="${CSS.escape(focused)}"]`);
      if (el) {
        el.focus();
        if (el.setSelectionRange && el.value) el.setSelectionRange(el.value.length, el.value.length);
      }
    }
    for (const m of ms) if (m.state === 'joined' && admin && !S.peerRoots.has(m.id)) loadPeerRoots(m.id);
    if (admin && ms.some((m) => m.state === 'not_joined')) watchJoin();
  }

  const PLATFORM = { macos: 'macOS', ios: 'iOS', ipados: 'iPadOS', linux: 'Linux', windows: 'Windows', android: 'Android' };

  // shortCPU drops trademarks, "CPU", core counts and the clock from a
  // CPU's name: "Intel(R) Core(TM) i7-7700K CPU @ 4.20GHz" is "Intel Core
  // i7-7700K".
  const shortCPU = (m) => (m || '').replace(/\((?:R|TM|C)\)/gi, '').replace(/@.*$/, '')
    .replace(/\b(?:CPU|Processor|\d+-Core)\b/gi, '').replace(/\s+/g, ' ').trim();

  // meter is a reading as a bar: amber from warn percent, red from full.
  // meter is a labelled bar; at, when its limit resets, goes under the
  // value on phones.
  function meter(text, pct, value, warn, full, title, at) {
    const level = pct >= full ? 'full' : pct >= warn ? 'warn' : '';
    const bar = h('span', 'meter-fill ' + level);
    bar.style.width = Math.max(1.5, Math.min(100, pct)) + '%';
    return h('div', { class: 'meter', title },
      h('span', 'meter-l', text),
      h('span', { class: 'meter-bar', role: 'img', 'aria-label': `${text}: ${value}` + (at ? `, resets ${at}` : '') }, bar),
      h('span', 'meter-v' + (level === 'full' ? ' full' : ''), value, at ? h('span', 'meter-at', h('span', 'meter-sep', ' · '), at) : null));
  }

  // USAGE_STALE_MS: usage or stats older than this are marked with when
  // they were read.
  const USAGE_STALE_MS = 10 * 60000;

  function machineCard(m) {
    const p = m.peer || {};
    const s = admin ? S.host.get(m.id === '' ? (S.server && S.server.id) : m.id) : null;
    const usage = admin ? S.usage.get(m.id === '' ? (S.server && S.server.id) : m.id) : null;
    const up = hostUp(m.state);
    const mac = s ? /mac/i.test(s.osName || '') : (S.server && m.state === 'self' && S.server.os === 'darwin');
    const addrs = (p.addrs || []).map((a) => hostPort(a, p.port));
    const spec = [];
    if (s) {
      spec.push(s.osName, shortCPU(s.cpuModel), s.cpus ? `${s.cpus} CPUs` : '', s.memoryTotal ? fmtIEC(s.memoryTotal) : '', s.bootedAtMs ? 'up ' + upFor(Date.now() - s.bootedAtMs) : '');
      if (s.readAtMs && Date.now() - s.readAtMs >= USAGE_STALE_MS) spec.push('as of ' + clock(s.readAtMs));
    } else if (m.state === 'self' && S.server) spec.push(`${S.server.os || '?'}/${S.server.arch || '?'}`);
    if (addrs.length) spec.push(addrs[0]);
    const url = !m.peer || p.self ? '' : peerURL(p);
    const live = allAgents().filter((x) => x.host === m.id && !isFinished(x.a)).length;
    const head = h('div', 'mach-h',
      h('span', 'mach-ic', icon(mac ? 'laptop' : 'server'), h('span', MACHINE_STATE[m.state][0])),
      h('div', 'mach-id',
        h('div', 'mach-name', m.name, m.state === 'self' ? h('span', 'chip ok', 'This machine') : null,
          m.state === 'self' && S.server && S.server.version ? h('span', 'chip plain mono', S.server.version) : null,
          !up && m.state !== 'self' ? h('span', 'mach-state ' + (m.state === 'not_joined' ? 'warn' : m.state === 'unreachable' ? 'bad' : ''), MACHINE_STATE[m.state][1]) : null),
        h('div', 'mach-spec', spec.filter(Boolean).map((t) => h('span', null, t)))),
      h('div', 'mach-acts',
        admin && up ? h('button', { type: 'button', class: 'btn sm', onclick: () => newIn(m.id, '') }, icon('plus'), 'New session') : null,
        url ? h('a', { class: 'btn ghost sm', href: url, target: '_blank', rel: 'noopener noreferrer', title: `Open the dashboard of ${m.name}` }, 'Its dashboard', icon('external', 'i-xs')) : null));
    const out = [head];
    if (m.state === 'not_joined' && admin) {
      out.push(h('div', 'mach-note', h('div', 'join-box',
        h('p', null, 'This machine does not have the fleet key yet. Run this on it once (it starts fleet there if needed):'),
        codeBox(joinCmd || 'loading…', !joinCmd),
        h('p', 'field-hint', 'This page notices by itself. Machines with the same key manage each other, so share it like a password.'))));
      loadJoin();
    } else if (m.state === 'unreachable') {
      out.push(h('p', 'mach-note', 'Seen on the network, but its dashboard does not answer.'));
    } else if (m.state === 'nodash') {
      out.push(h('p', 'mach-note', 'It does not advertise a web dashboard (an older version, or web is off or loopback only), so it cannot be managed from here.'));
    } else if (m.state === 'checking') {
      out.push(h('p', 'mach-note', 'Checking whether it shares the fleet key…'));
    }
    if (up && admin && (s || usage)) out.push(h('div', 'mach-body', healthSec(s), agentsSec(usage)));
    if (up) {
      out.push(h('div', 'mach-foot',
        h('span', null, plural(live, 'live session')),
        live ? h('button', { type: 'button', class: 'link-btn', onclick: () => {
          U.groupBy = 'machine';
          save('fleet.groupBy', 'machine');
          renderSideList();
          if (small()) go('#/');
          else toast('The sessions are grouped by machine now');
        } }, 'Show them') : null));
      if (admin) out.push(updateSec(m), rootsSec(m));
    }
    return h('section', { class: 'mach' + (up ? '' : m.state === 'not_joined' ? ' warn' : ' off'), 'aria-label': m.name }, out);
  }

  // newerRelease: whether release a (v1.2.3) is newer than release b.
  // Development builds are never newer.
  function newerRelease(a, b) {
    const re = /^v(\d+)\.(\d+)\.(\d+)$/;
    const x = re.exec(a || ''), y = re.exec(b || '');
    if (!x || !y) return false;
    for (let i = 1; i <= 3; i++) if (+x[i] !== +y[i]) return +x[i] > +y[i];
    return false;
  }

  // updateSec: whether the machine's daemon updates itself, and what it
  // found. Turning it off, or updating now, asks the daemon.
  function updateSec(m) {
    const id = m.id === '' ? (S.server && S.server.id) : m.id;
    const u = S.update.get(id);
    if (!u) return null;
    const newer = !u.installed && newerRelease(u.latest, u.version);
    const sub = u.installed ? `${u.installed} is installed. fleet restarts on it once no agent is working; the sessions keep running.`
      : newer && u.error ? `${u.latest} is out, but the update failed: ${u.error}`
        : newer ? `${u.latest} is out (this is ${u.version}).`
          : u.unsupported ? `Not on this machine: ${u.unsupported}.`
            : u.error ? `${u.version}. Could not check for updates: ${u.error}`
              : u.latest ? `${u.version} is the newest release.`
                : u.auto ? `${u.version}. Checks for new releases every 6 hours.`
                  : `${u.version}. New releases are not installed.`;
    const sw = h('input', {
      type: 'checkbox', class: 'switch', checked: u.auto, 'aria-label': 'Automatic updates on ' + m.name,
      onchange: (ev) => postUpdate(m, id, { auto: ev.target.checked }, ev.target),
    });
    const now = u.unsupported || u.installed ? null
      : newer ? h('button', { type: 'button', class: 'btn sm', onclick: (ev) => postUpdate(m, id, { install: true }, ev.currentTarget) }, icon('refresh'), 'Update now')
        : h('button', { type: 'button', class: 'btn ghost sm', onclick: (ev) => postUpdate(m, id, { check: true }, ev.currentTarget) }, 'Check now');
    return h('div', 'mach-upd',
      h('label', 'set-row grow', h('div', 'grow', h('div', null, 'Automatic updates'), h('div', 'sub', sub)), sw), now);
  }

  async function postUpdate(m, id, body, el) {
    const base = id === (S.server && S.server.id) ? '/api/' : `/api/hosts/${encodeURIComponent(id)}/`;
    el.disabled = true;
    if (body.install || body.check) el.lastChild.textContent = body.install ? 'Updating…' : 'Checking…';
    try {
      const r = await api('POST', base + 'update', body);
      S.update.set(id, r.update);
      if ((body.install || body.check) && r.update.error) toast(`Could not ${body.install ? 'update' : 'check'} ${m.name}: ${r.update.error}`, 'error');
      else if (body.install && r.update.installed) toast(`${m.name} restarts on ${r.update.installed} once no agent is working`);
    } catch (e) {
      toast(`Could not change the updates of ${m.name}: ${e.message}`, 'error');
    }
    renderDevices();
  }

  function healthSec(s) {
    if (!s) return h('div', 'mach-sec', h('h3', 'sec-h', 'Health'), h('p', 'ag-none', 'Not read yet.'));
    const share = (part, whole) => (whole > 0 ? (100 * part) / whole : 0);
    const busy = share(s.load[0], s.cpus);
    const used = s.memoryTotal - s.memoryAvailable;
    return h('div', 'mach-sec',
      h('h3', 'sec-h', 'Health'),
      meter('Load', busy, s.load[0].toFixed(2), 100, 150, `load ${s.load.map((l) => l.toFixed(2)).join(' · ')} over 1, 5 and 15 minutes, on ${s.cpus} CPUs (${Math.round(busy)}% busy)`),
      s.memoryTotal ? meter('Memory', share(used, s.memoryTotal), Math.round(share(used, s.memoryTotal)) + '%', 80, 90, `${fmtIEC(used)} used of ${fmtIEC(s.memoryTotal)}, ${fmtIEC(s.memoryAvailable)} available`) : null,
      s.swapTotal ? meter('Swap', share(s.swapUsed, s.swapTotal), Math.round(share(s.swapUsed, s.swapTotal)) + '%', 80, 90, `${fmtIEC(s.swapUsed)} used of ${fmtIEC(s.swapTotal)}`) : null,
      (s.disks || []).map((d) => {
        const pct = share(d.used, d.used + d.available); // as df counts it: of what users may fill
        return meter('Disk ' + (d.mount === '/' ? '/' : baseName(d.mount)), pct, Math.round(pct) + '%', 80, 90, `${d.mount}: ${fmtIEC(d.used)} used of ${fmtIEC(d.total)}, ${fmtIEC(d.available)} free`);
      }));
  }

  function agentsSec(list) {
    if (!list) return h('div', 'mach-sec', h('h3', 'sec-h', 'Agents and plan usage'), h('p', 'ag-none', 'Not read yet.'));
    return h('div', 'mach-sec',
      h('h3', 'sec-h', 'Agents and plan usage'),
      list.map((a) => h('div', 'ag',
        h('div', 'ag-h', logo(a.id), a.name || a.id, a.version ? h('span', 'muted', a.version) : null, a.plan ? h('span', 'chip', a.plan) : null),
        !a.available ? h('p', 'ag-none', a.reason || 'Not installed')
          : a.limits && a.limits.length ? [a.limits.map((l) => meter(l.label, l.percent, `${Math.round(l.percent)}%`, 70, 90,
            `${l.label}: ${Math.round(l.percent)}% used` + (l.resetsAtMs ? `, resets ${new Date(l.resetsAtMs).toLocaleString()}` : ''), l.resetsAtMs ? clock(l.resetsAtMs) : '')),
          a.asOfMs && Date.now() - a.asOfMs >= USAGE_STALE_MS ? h('p', 'ag-none', 'as of ' + clock(a.asOfMs)) : null]
            : h('p', 'ag-none', a.error ? 'Usage unknown: ' + a.error : 'No plan limits'))));
  }

  function rootsSec(m) {
    const roots = m.id ? S.peerRoots.get(m.id) : S.roots;
    const box = h('div', 'roots');
    const add = h('button', { type: 'button', class: 'btn sm', onclick: () => openRootPicker(m.id) }, icon('folder-plus'), 'Add folder');
    if (!roots) {
      box.append(h('div', 'roots-h', h('h3', 'sec-h', 'Folders'), add), h('p', 'roots-loading', 'Loading…'));
      return box;
    }
    const q = (rootsUI.q.get(m.id) || '').toLowerCase();
    const words = q.split(/\s+/).filter(Boolean);
    const many = roots.length > 6;
    const found = roots.filter((r) => words.every((w) => (r.name + ' ' + shortPath(r.path)).toLowerCase().includes(w)));
    const all = rootsUI.all.has(m.id);
    const shown = q || all || !many ? found : found.slice(0, 6);
    const filter = many ? h('label', 'search', icon('search', 'i-sm'), h('input', {
      type: 'search', placeholder: 'Filter folders', 'aria-label': 'Filter folders on ' + m.name, value: rootsUI.q.get(m.id) || '', 'data-focus': 'roots-q-' + m.id,
      oninput: (ev) => {
        rootsUI.q.set(m.id, ev.target.value);
        renderDevices();
      },
    })) : null;
    box.append(h('div', 'roots-h', h('h3', 'sec-h', `Folders · ${roots.length}`), filter, add));
    if (!roots.length) box.append(h('p', 'roots-loading', 'No folders yet: agents can only start inside one.'));
    for (const r of shown) box.append(rootRow(m, r));
    if (q && !found.length) box.append(h('p', 'roots-loading', `No folder matches “${q}”.`));
    if (many && !q) box.append(h('button', { type: 'button', class: 'roots-more', 'aria-expanded': String(all), onclick: () => {
      if (all) rootsUI.all.delete(m.id);
      else rootsUI.all.add(m.id);
      renderDevices();
    } }, all ? 'Show fewer' : `Show all ${roots.length} folders`));
    return box;
  }

  function rootRow(m, r) {
    const key = m.id + '\0' + r.name;
    const confirming = rootsUI.confirm === key;
    return h('div', 'root-row',
      h('div', 'root-main',
        h('div', 'root-name', h('span', 'trunc', r.name),
          r.trust ? h('span', { class: 'trust', title: 'Agents skip the “Do you trust this folder?” prompt' }, 'trusted') : null,
          r.adapters && r.adapters.length ? h('span', 'muted mono', r.adapters.join(' · ')) : null),
        h('div', { class: 'root-path', title: r.path }, shortPath(r.path))),
      confirming
        ? h('div', 'root-confirm', h('span', null, 'Remove? Running agents keep going.'),
          h('button', { type: 'button', class: 'btn sm', 'data-focus': 'keep-' + key, onclick: () => { rootsUI.confirm = ''; renderDevices(); } }, 'Keep'),
          h('button', { type: 'button', class: 'btn danger sm', 'data-focus': 'remove-' + key, onclick: () => removeRoot(m.id, r.name) }, 'Remove'))
        : h('div', 'root-acts',
          iconBtn('plus', `New session in ${r.name}`, () => newIn(m.id, r.path)),
          iconBtn('pencil', `Edit ${r.name}`, () => openRootPicker(m.id, r)),
          iconBtn('trash', `Remove ${r.name}`, () => {
            rootsUI.confirm = key;
            renderDevices();
            setTimeout(() => $('main').querySelector(`[data-focus="${CSS.escape('remove-' + key)}"]`)?.focus());
          })));
  }

  async function loadPeerRoots(id) {
    if (!id) return;
    S.peerRoots.set(id, null);
    try {
      const r = await api('GET', `/api/hosts/${encodeURIComponent(id)}/roots`);
      S.peerRoots.set(id, (r && r.roots) || []);
    } catch {
      S.peerRoots.delete(id);
      return;
    }
    if (U.view === 'devices') renderDevices();
  }

  async function removeRoot(host, name) {
    rootsUI.confirm = '';
    try {
      await api('DELETE', (host ? `/api/hosts/${encodeURIComponent(host)}/` : '/api/') + 'roots/' + encodeURIComponent(name));
      toast(`Removed the folder ${name}`);
      if (host) loadPeerRoots(host);
    } catch (e) {
      toast(`Could not remove ${name}: ${e.message}`, 'error');
    }
    if (U.view === 'devices') renderDevices();
  }

  // codeBox is a command to copy.
  function codeBox(text, disabled) {
    const code = h('code', null, text);
    return h('div', 'codebox', code, h('button', { type: 'button', class: 'btn ghost sm', disabled, onclick: (ev) => copyText(code.textContent, ev.currentTarget) }, 'Copy'));
  }

  async function loadJoin() {
    if (joinCmd || !admin) return joinCmd;
    try {
      const r = await api('GET', '/api/join');
      joinCmd = (r && r.command) || '';
    } catch {
      return '';
    }
    if (U.view === 'devices') renderDevices();
    if (dialogShows('join')) openJoin();
    return joinCmd;
  }

  // watchJoin asks the machines without the fleet key again every few
  // seconds while Devices shows them, and loads one once it joined.
  function watchJoin() {
    if (joinTimer) return;
    joinTimer = setTimeout(async () => {
      joinTimer = 0;
      if (U.view !== 'devices' || !admin) return;
      for (const m of machines()) {
        if (m.state !== 'not_joined') continue;
        if (await checkHost(m.id) === 'joined') toast(`${m.name} joined the fleet`);
      }
      if (U.view === 'devices' && machines().some((m) => m.state === 'not_joined')) watchJoin();
    }, 3000);
  }

  function openJoin() {
    loadJoin();
    let shown = false;
    const masked = () => joinCmd.replace(/(--join\s+)(\S+)/, (all, a, k) => a + '•'.repeat(Math.min(24, k.length)));
    const code = h('code', null, joinCmd ? masked() : 'loading…');
    const install = 'curl -fsSL https://raw.githubusercontent.com/floscom/fleet/main/install.sh | sh';
    openDialog({
      id: 'join',
      title: 'Add a machine',
      sub: 'Machines with the same fleet key find each other on the network and over Tailscale, and show up here.',
      body: h('ol', 'steps-list',
        h('li', null, h('div', null, 'Install fleet on it:'), codeBox(install)),
        h('li', null, h('div', null, 'Start it with this fleet’s key, and the folders agents may use:'),
          h('div', 'codebox', code,
            iconBtn('eye', 'Show the key', () => {
              shown = !shown;
              code.textContent = joinCmd ? (shown ? joinCmd : masked()) : 'loading…';
            }),
            h('button', { type: 'button', class: 'btn ghost sm', disabled: !joinCmd, onclick: (ev) => copyText(joinCmd + ' --root ~/code', ev.currentTarget) }, 'Copy'))),
        h('li', null, 'It appears on this page within a few seconds. The key works like a password: share it only with your machines.')),
      foot: [h('button', { type: 'button', class: 'btn primary', onclick: closeDialog }, 'Done')],
    });
  }

  // ---- adding or editing a folder (a root): a folder browser over GET
  // .../fs on the machine, then its name, the agents allowed and trust.

  const picker = { host: '', dir: null, seq: 0, loading: false, error: '', busy: false, editing: null, making: false, adapters: null, roots: null, hidden: false, filter: '', newName: null, els: null };

  const pickerAPI = (path) => (picker.host ? `/api/hosts/${encodeURIComponent(picker.host)}/${path}` : '/api/' + path);
  const pickerRoots = () => (picker.host ? picker.roots || [] : S.roots);
  const rootAt = (path) => pickerRoots().find((r) => r.path === path);

  function rootAround(path) {
    let best = null;
    for (const r of pickerRoots()) {
      const inside = path === r.path || path.startsWith(r.path.endsWith('/') ? r.path : r.path + '/');
      if (inside && (!best || r.path.length > best.path.length)) best = r;
    }
    return best;
  }

  function openRootPicker(host, root) {
    if (!admin) return;
    Object.assign(picker, { host: host || '', dir: null, loading: false, error: '', busy: false, editing: root || null, making: false, adapters: null, roots: null, filter: '', newName: null });
    picker.seq++;
    const name = h('input', { class: 'input mono', value: root ? root.name : '', maxlength: 64, autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Name' });
    const trust = h('input', { type: 'checkbox', checked: !!(root && root.trust) });
    const path = h('input', { class: 'input mono', placeholder: '/path/to/folder', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', 'aria-label': 'Folder path' });
    const filter = h('input', { type: 'search', placeholder: 'Filter folders', 'aria-label': 'Filter folders', autocomplete: 'off', spellcheck: 'false' });
    const hidden = h('input', { type: 'checkbox', checked: picker.hidden });
    const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Current folder' });
    const list = h('div', { class: 'pick-list', role: 'listbox', 'aria-label': 'Folders' });
    const newRow = h('div');
    const agents = h('div', 'radio-chips');
    const status = h('p', { class: 'dlg-status', role: 'status', 'aria-live': 'polite' });
    const save2 = h('button', { type: 'button', class: 'btn primary', onclick: () => saveRoot() }, root ? 'Save' : 'Add as root');
    picker.els = { name, trust, path, filter, hidden, crumbs, list, newRow, agents, status, save: save2 };
    path.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault();
        goToTyped();
      }
    });
    filter.addEventListener('input', () => {
      picker.filter = filter.value.trim().toLowerCase();
      paintPickerList();
    });
    filter.addEventListener('keydown', (ev) => {
      const first = list.querySelector('.mi');
      if (ev.key === 'ArrowDown' && first) {
        ev.preventDefault();
        first.focus();
      } else if (ev.key === 'Enter') {
        ev.preventDefault();
        if (filter.value.trim() && first) first.click();
      } else if (ev.key === 'Backspace' && !filter.value && picker.dir && picker.dir.parent) {
        ev.preventDefault();
        browse(picker.dir.parent);
      }
    });
    list.addEventListener('keydown', (ev) => {
      const items = [...list.querySelectorAll('.mi')];
      const i = items.indexOf(document.activeElement);
      if (ev.key === 'ArrowDown' && i + 1 < items.length) {
        ev.preventDefault();
        items[i + 1].focus();
      } else if (ev.key === 'ArrowUp') {
        ev.preventDefault();
        (i > 0 ? items[i - 1] : filter).focus();
      }
    });
    hidden.addEventListener('change', () => {
      picker.hidden = hidden.checked;
      browse(picker.dir ? picker.dir.path : '');
    });
    openDialog({
      id: 'root',
      wide: true,
      title: root ? `Edit ${root.name}` : 'Add a folder',
      sub: `Agents can start anywhere inside it. A folder on ${hostName(picker.host)}.`,
      body: [
        h('div', 'pop-row', h('label', 'search grow', icon('folder', 'i-sm'), path), h('button', { type: 'button', class: 'btn sm', onclick: goToTyped }, 'Go')),
        h('div', 'pop-row', crumbs, h('span', 'grow'),
          h('button', { type: 'button', class: 'btn ghost sm', onclick: () => { picker.newName = ''; paintNewRow(); } }, icon('folder-plus'), 'New folder'),
          h('button', { type: 'button', class: 'btn ghost sm', onclick: () => browse('') }, '~ home'),
          h('label', 'pr-acts', hidden, 'hidden')),
        newRow,
        h('label', 'search', icon('search', 'i-sm'), filter),
        list,
        h('div', 'opt-grid',
          h('span', 'field-l', 'Name'), name,
          h('span', 'field-l', 'Agents'), h('div', null, agents, h('span', 'field-hint', 'None ticked: all of them'))),
        h('label', 'check-row warn', trust, h('span', null, h('strong', null, 'Trust this folder. '), 'Agents started here skip Claude Code’s and Codex’s “Do you trust this folder?” prompt, so its own hooks, MCP servers and settings run. Only for code you trust.')),
        status,
      ],
      foot: [h('button', { type: 'button', class: 'btn', onclick: closeDialog }, 'Cancel'), save2],
      focus: touch() ? null : filter,
      canClose: () => !picker.busy,
      onClose: () => {
        picker.seq++;
        picker.els = null;
      },
    });
    browse(root ? root.path : '');
    loadPickerAdapters();
    if (picker.host) loadPickerRoots();
    paintPickerAgents();
  }

  async function browse(path) {
    const seq = ++picker.seq;
    picker.loading = true;
    paintPicker();
    const q = new URLSearchParams();
    if (path) q.set('path', path);
    if (picker.hidden) q.set('hidden', '1');
    let dir = null;
    let err = '';
    try {
      dir = await api('GET', pickerAPI('fs?' + q));
    } catch (e) {
      err = e.message;
      if (picker.host && e.notJoined) setHostState(picker.host, 'not_joined');
    }
    if (seq !== picker.seq || !picker.els) return;
    picker.loading = false;
    picker.error = err;
    if (dir) {
      const moved = !picker.dir || picker.dir.path !== dir.path;
      picker.dir = dir;
      picker.els.path.value = dir.path;
      if (moved) {
        picker.filter = '';
        picker.els.filter.value = '';
        picker.els.list.scrollTop = 0;
      }
    }
    paintPicker();
  }

  function goToTyped() {
    let p = picker.els.path.value.trim();
    if (!p) return;
    if (p === '~' || p.startsWith('~/')) p = ((picker.dir && picker.dir.home) || '') + p.slice(1);
    browse(p);
  }

  async function loadPickerAdapters() {
    const host = picker.host;
    let list = [];
    try {
      const r = await api('GET', pickerAPI('adapters'));
      list = (r && r.adapters) || [];
    } catch {
      // not joined or unreachable: the folder list says so
    }
    if (host !== picker.host || !picker.els) return;
    picker.adapters = list;
    paintPickerAgents();
  }

  async function loadPickerRoots() {
    const host = picker.host;
    try {
      const r = await api('GET', pickerAPI('roots'));
      if (host !== picker.host) return;
      picker.roots = (r && r.roots) || [];
      paintPicker();
    } catch {
      // browse reports it
    }
  }

  function paintPickerAgents() {
    const els = picker.els;
    if (!els) return;
    const was = new Set([...els.agents.querySelectorAll('input:checked')].map((i) => i.value));
    const chosen = was.size ? was : new Set((picker.editing && picker.editing.adapters) || []);
    const list = [...(picker.adapters || [])];
    for (const id of chosen) if (!list.some((a) => a.id === id)) list.push({ id, name: id, available: false });
    fill(els.agents, !picker.adapters ? h('span', 'field-hint', 'Loading…') : list.map((a) =>
      h('label', { title: a.available ? a.name : `${a.name}: not installed here` }, h('input', { type: 'checkbox', value: a.id, checked: chosen.has(a.id) }), a.name || a.id, a.available ? '' : ' · n/a')));
  }

  function paintPicker() {
    const els = picker.els;
    if (!els) return;
    const dir = picker.dir;
    // the crumbs
    const parts = dir ? dir.path.split('/').filter(Boolean) : [];
    fill(els.crumbs, dir ? [h('button', { type: 'button', onclick: () => browse('/') }, '/'),
      parts.map((p, i) => [i ? ' / ' : '', i === parts.length - 1 ? h('span', { 'aria-current': 'location' }, p) : h('button', { type: 'button', onclick: () => browse('/' + parts.slice(0, i + 1).join('/')) }, p)])] : null);
    paintPickerList();
    paintNewRow();
    // what Add does, or what is in the way
    const existing = dir && rootAt(dir.path);
    const conflict = existing && (!picker.editing || existing.name !== picker.editing.name);
    const notJoined = picker.host && hostState(picker.host) === 'not_joined';
    const st = els.status;
    st.className = 'dlg-status';
    if (notJoined) fill(st, `${hostName(picker.host)} does not have this fleet’s key yet.`);
    else if (picker.error) {
      st.className = 'dlg-status err';
      fill(st, picker.error, dir ? ` · still in ${shortPath(dir.path)}` : '');
    } else if (!dir) fill(st, 'Loading…');
    else if (conflict) fill(st, `This folder is already the root “${existing.name}”.`);
    else {
      const around = rootAround(dir.path);
      fill(st, picker.editing ? 'Saves ' : 'Adds ', h('span', 'mono', shortPath(dir.path)), around && around !== existing ? ` · inside the root “${around.name}”, where agents can already start` : '');
    }
    els.name.placeholder = picker.editing ? picker.editing.name : dir ? baseName(dir.path) || 'root' : '';
    els.save.disabled = !dir || picker.loading || picker.busy || !!conflict || notJoined;
    els.save.textContent = picker.editing ? (picker.busy ? 'Saving…' : 'Save') : picker.busy ? 'Adding…' : 'Add as root';
  }

  function paintPickerList() {
    const els = picker.els;
    const dir = picker.dir;
    if (!els) return;
    if (!dir) return fill(els.list, h('p', 'menu-note', picker.loading ? 'Loading…' : picker.error || 'Nothing to show.'));
    const f = picker.filter;
    const shown = f ? dir.entries.filter((e) => e.name.toLowerCase().includes(f)) : dir.entries;
    const items = [];
    if (dir.parent && !f) items.push(mi({ icon: 'chevron-left', label: '..', sub: 'the folder above', onSelect: () => browse(dir.parent) }));
    for (const e of shown) {
      const path = joinPath(dir.path, e.name);
      const r = rootAt(path);
      items.push(mi({ icon: 'folder', label: e.name, right: [e.git ? h('span', 'tag', 'git') : null, r ? h('span', 'trust', 'root: ' + r.name) : null], onSelect: () => browse(path) }));
    }
    if (!shown.length) items.push(h('p', 'menu-note', f ? `No folder matches “${f}”.` : 'No folders here.'));
    if (dir.truncated) items.push(h('p', 'menu-note', `Only the first ${dir.entries.length} folders are listed: filter or type a path.`));
    fill(els.list, items);
  }

  function paintNewRow() {
    const els = picker.els;
    if (!els) return;
    if (picker.newName == null) return fill(els.newRow);
    const input = h('input', { class: 'input mono', placeholder: 'name of the new folder', value: picker.newName, autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', 'aria-label': 'Name of the new folder' });
    const make = async () => {
      const n = input.value.trim();
      const dir = picker.dir;
      if (picker.making || !dir || !n || n.startsWith('/')) return input.focus();
      const host = picker.host;
      picker.making = true;
      try {
        const made = await api('POST', pickerAPI('fs'), { path: joinPath(dir.path, n) });
        if (host === picker.host && picker.els) {
          picker.newName = null;
          picker.els.trust.checked = true;
          browse(made.path);
        }
      } catch (e) {
        picker.error = e.status === 405 ? `${hostName(host)} runs an older fleet: update it to make folders from here` : e.message;
        paintPicker();
      }
      picker.making = false;
    };
    input.addEventListener('input', () => {
      picker.newName = input.value;
    });
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault();
        make();
      } else if (ev.key === 'Escape') {
        ev.preventDefault();
        ev.stopPropagation();
        picker.newName = null;
        paintNewRow();
      }
    });
    fill(els.newRow, h('div', 'pop-row', h('label', 'search grow', icon('folder-plus', 'i-sm'), input),
      h('button', { type: 'button', class: 'btn sm primary', onclick: make }, 'Create'),
      iconBtn('x', 'Cancel the new folder', () => { picker.newName = null; paintNewRow(); })));
    setTimeout(() => input.focus());
  }

  async function saveRoot() {
    const els = picker.els;
    const dir = picker.dir;
    const editing = picker.editing;
    const existing = dir && rootAt(dir.path);
    if (!els || !dir || picker.loading || picker.busy || (existing && (!editing || existing.name !== editing.name))) return;
    const name = els.name.value.trim();
    if (/[/\\]/.test(name)) {
      picker.error = 'A root’s name cannot contain / or \\';
      paintPicker();
      return;
    }
    const adapters = [...els.agents.querySelectorAll('input:checked')].map((i) => i.value);
    const host = picker.host;
    picker.busy = true;
    picker.error = '';
    paintPicker();
    try {
      const r = await api(editing ? 'PUT' : 'POST', pickerAPI('roots' + (editing ? '/' + encodeURIComponent(editing.name) : '')), { path: dir.path, name, adapters, trust: els.trust.checked });
      toast(`${editing ? 'Saved' : 'Added'} ${r.root.name}${host ? ' on ' + hostName(host) : ''} → ${shortPath(r.root.path)}`);
      picker.busy = false;
      closeDialog();
      if (host) loadPeerRoots(host);
      return;
    } catch (e) {
      picker.error = e.message;
    }
    picker.busy = false;
    paintPicker();
  }

  // ------------------------------------------------------------------ polls
  //
  // With the admin token: the agents of every other fleet that holds this
  // fleet's key (every 4 s), the workflow runs going on everywhere (3 s),
  // and how each machine and its agent CLIs' plans are doing (a minute), all
  // while the page is visible.

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
      const st = hostStates.get(id);
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
        changed = true;
        chatAgent(host, a);
      }
    }
    return changed;
  }

  const WF_POLL_MS = 3000;
  let wfTimer = 0;
  let wfBusy = false;

  function scheduleWorkflows(delay) {
    clearTimeout(wfTimer);
    if (admin) wfTimer = setTimeout(pollWorkflows, delay);
  }

  // pollWorkflows asks this fleet and every joined one for the runs going
  // on. The list keeps what it needs to draw them.
  async function pollWorkflows() {
    if (!admin || wfBusy) return;
    if (document.hidden) {
      scheduleWorkflows(WF_POLL_MS);
      return;
    }
    wfBusy = true;
    const next = new Map();
    const keep = (host) => {
      for (const [k, v] of S.workflows) if (host ? k.startsWith(host + '/') : !k.includes('/')) next.set(k, v);
    };
    const hosts = ['', ...pickerHosts().filter((x) => x.id && hostStates.get(x.id) === 'joined').map((x) => x.id)];
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
    if (self && !ids.includes(self)) ids.push(self);
    for (const m of [S.usage, S.host, S.update]) for (const id of [...m.keys()]) if (!ids.includes(id)) m.delete(id);
    await Promise.all(ids.map((id) => {
      const base = id === self ? '/api/' : `/api/hosts/${encodeURIComponent(id)}/`;
      // Not joined: nothing to show. Unreachable: keep the last answer.
      return Promise.all([
        api('GET', base + 'usage').then((r) => S.usage.set(id, (r && r.agents) || []), (e) => {
          if (e.notJoined || !S.usage.has(id)) S.usage.set(id, null);
        }),
        api('GET', base + 'host').then((r) => S.host.set(id, (r && r.host) || null), (e) => {
          if (e.notJoined || e.status === 404 || !S.host.has(id)) S.host.set(id, null);
        }),
        // A daemon older than updates answers 404: no Updates row.
        api('GET', base + 'update').then((r) => S.update.set(id, (r && r.update) || null), (e) => {
          if (e.notJoined || e.status === 404 || !S.update.has(id)) S.update.set(id, null);
        }),
      ]);
    }));
    usageBusy = false;
    if (U.view === 'devices') renderDevices();
    scheduleUsage(USAGE_POLL_MS);
  }

  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    scheduleRemote(0);
    scheduleWorkflows(0);
    if (Date.now() - usageAt >= USAGE_POLL_MS) scheduleUsage(0);
  });

  // ----------------------------------------------------------- renders
  //
  // Changes are drawn once per animation frame, by what they touch.

  const dirty = new Set();
  let rafPending = false;
  const RENDER = {
    agents: () => {
      meet();
      renderSideList();
      if (U.view === 'devices') renderDevices();
      else if (U.view === 'new' || (U.view === 'home' && !small())) renderLaunch();
    },
    peers: () => {
      renderSideFoot();
      if (U.view === 'devices') renderDevices();
      else if (U.view === 'new' || (U.view === 'home' && !small())) renderLaunch();
    },
    roots: () => {
      if (U.view === 'devices') renderDevices();
      if (!launch.host && launch.loaded === '') launchRootsChanged();
    },
    devices: () => {
      if (U.view === 'devices') renderDevices();
    },
    server: () => {
      repaintSettings();
      renderSideFoot();
    },
  };

  function invalidate(...parts) {
    updateTitle();
    snapshotSoon();
    for (const p of parts) dirty.add(p);
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(() => {
      rafPending = false;
      const now = [...dirty];
      dirty.clear();
      for (const p of now) RENDER[p]();
    });
  }

  // ------------------------------------------------------------ messages

  const peerKey = (p) => p.id || p.name + '@' + p.host;

  // resetState starts over for a new connection's snapshot. What shows
  // stays meanwhile, no blank list on a reconnect or when the page opens
  // with what it showed last (restoreSnapshot): the snapshot replaces the
  // rest and drops the agents it lacks.
  function resetState() {
    S.unseen = new Set(S.agents.keys());
    S.snapshotDone = false;
    S.peersSeen = false;
  }

  function handle(msg) {
    switch (msg.type) {
      case 'hello':
        resetState();
        S.server = msg.server || null;
        invalidate('server', 'agents', 'peers', 'devices', 'roots');
        // The daemon may have restarted, or the token been rotated.
        checkSession(linkToken);
        linkToken = false;
        break;
      case 'devices':
        S.devices = msg.devices || [];
        invalidate('devices');
        break;
      case 'peers': {
        const next = msg.peers || [];
        // Lists before the server's first network browse (complete=false)
        // hold only this daemon: only a complete one tells new fleets.
        if (S.peersSeen) {
          const old = new Set(S.peers.map(peerKey));
          for (const p of next) if (!p.self && !old.has(peerKey(p))) toast(`A new machine on the network: ${p.name || p.host}`);
        }
        if (msg.complete) S.peersSeen = true;
        S.peers = next;
        invalidate('peers');
        if (admin) {
          checkHosts();
          if (next.some((p) => p.id && !S.usage.has(p.id))) scheduleUsage(0);
        }
        break;
      }
      case 'roots':
        S.roots = msg.roots || [];
        invalidate('roots');
        break;
      case 'agent': {
        const a = msg.agent;
        if (!a || !a.id) break;
        S.agents.set(a.id, a);
        if (S.unseen) S.unseen.delete(a.id);
        invalidate('agents');
        chatAgent('', a);
        break;
      }
      case 'agentRemoved':
        S.agents.delete(msg.id);
        invalidate('agents');
        break;
      case 'ping':
        break; // the daemon is there (heardAt)
      case 'snapshotDone':
        for (const id of S.unseen || []) S.agents.delete(id);
        S.unseen = null;
        S.snapshotDone = true;
        invalidate('agents');
        tryOpenPending();
        if (U.view === 'chat' && admin) renderMain();
        break;
      default:
        // unknown message types are ignored for forward compatibility
    }
  }

  // ---------------------------------------------------------------- socket

  let backoff = 500;
  const BACKOFF_MAX = 3000;
  // The daemon sends a "ping" when it has been quiet for PING_MS. A socket
  // that hears nothing for longer is dead without the browser knowing, as a
  // phone's is after sleeping or changing networks, and is replaced.
  const PING_MS = 15000;
  const STALE_MS = 2 * PING_MS + 5000;
  // A connection attempt that hangs (host asleep, network just changed) is
  // given up after this, not after the browser's own TCP timeout.
  const OPEN_TIMEOUT_MS = 6000;
  let heardAt = 0;
  // The banner waits this long: a page opening, or coming back to the
  // front, mostly connects before, with what it showed still there.
  const CONN_GRACE_MS = 1500;
  let ws = null;
  let reconnectTimer = 0;
  let connTimer = 0;

  function setConn(state) {
    S.online = state === 'live';
    const el = $('conn');
    if (S.online) {
      clearTimeout(connTimer);
      connTimer = 0;
      el.hidden = true;
      S.connShown = false;
    } else {
      fill(el, h('span', 'spin'), state === 'reconnecting' ? 'Connection lost · reconnecting…' : 'Connecting…');
      if (el.hidden && !connTimer) {
        connTimer = setTimeout(() => {
          connTimer = 0;
          el.hidden = false;
          S.connShown = true;
          applyShell();
        }, CONN_GRACE_MS);
      }
    }
    applyShell();
  }

  function connect() {
    const url = (location.protocol === 'https:' ? 'wss' : 'ws') + '://' + location.host + '/ws';
    clearTimeout(reconnectTimer);
    try {
      ws = new WebSocket(url);
    } catch {
      ws = null;
      scheduleReconnect();
      return;
    }
    const sock = ws;
    const openTimer = setTimeout(() => {
      if (sock.readyState === WebSocket.CONNECTING) drop(sock, false);
    }, OPEN_TIMEOUT_MS);
    sock.onopen = () => {
      clearTimeout(openTimer);
      heardAt = Date.now();
      backoff = 500;
      setConn('live');
      checkShell();
    };
    sock.onmessage = (ev) => {
      if (ws !== sock) return;
      heardAt = Date.now();
      if (typeof ev.data !== 'string') return;
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (msg && typeof msg.type === 'string') handle(msg);
    };
    sock.onclose = () => {
      sock.onclose = null;
      clearTimeout(openTimer);
      if (ws !== sock) return;
      ws = null;
      scheduleReconnect();
    };
    sock.onerror = () => {
      // onclose follows and handles the reconnect
    };
  }

  function scheduleReconnect() {
    setConn('reconnecting');
    const wait = backoff;
    backoff = Math.min(BACKOFF_MAX, backoff * 2);
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(connect, wait);
  }

  // drop gives up on sock, which hangs or went silent, and connects again
  // at once (now) or after the backoff.
  function drop(sock, now) {
    if (ws !== sock) return;
    ws = null;
    sock.onclose = null;
    try {
      sock.close();
    } catch {
      // closing anyway
    }
    if (!now) return scheduleReconnect();
    setConn('reconnecting');
    backoff = 500;
    connect();
  }

  // checkSocket replaces an open socket that has heard nothing, not even a
  // ping, for STALE_MS.
  function checkSocket() {
    if (ws && ws.readyState === WebSocket.OPEN && Date.now() - heardAt > STALE_MS) drop(ws, false);
  }

  // reconnectNow tries again at once when the page comes back to the front
  // or the network returns: a phone drops the socket of an app in the
  // background, and the backoff may have grown meanwhile. A socket still
  // trying, or open but silent past a ping, may be one the browser has not
  // seen die: starting over costs a snapshot, waiting may cost a minute.
  function reconnectNow() {
    if (document.hidden) return;
    if (ws) {
      if (ws.readyState === WebSocket.OPEN && Date.now() - heardAt <= PING_MS + 5000) return;
      return drop(ws, true);
    }
    backoff = 500;
    connect();
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

  function getToken() {
    try {
      return localStorage.getItem(TOKEN_KEY) || memToken;
    } catch {
      return memToken;
    }
  }

  function setToken(t) {
    memToken = t;
    try {
      if (t) localStorage.setItem(TOKEN_KEY, t);
      else localStorage.removeItem(TOKEN_KEY);
    } catch {
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
    } catch {
      throw new APIError(0, 'cannot reach the daemon');
    }
    let data = null;
    if (res.status !== 204) {
      try {
        data = await res.json();
      } catch {
        // not JSON
      }
    }
    if (res.status === 401) lostAdmin();
    if (!res.ok) throw new APIError(res.status, (data && data.error) || `request failed (${res.status})`, data && data.notJoined);
    return data;
  }

  // apiUpload is api for a body that takes a while to go up (images in it),
  // over XHR for its progress: progress gets the share sent, 0 to 1 (1 once
  // all is sent and the reply is awaited).
  function apiUpload(method, path, body, progress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open(method, path);
      const token = getToken();
      if (token) xhr.setRequestHeader('Authorization', 'Bearer ' + token);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.upload.onprogress = (ev) => {
        if (ev.lengthComputable && ev.total) progress(Math.min(1, ev.loaded / ev.total));
      };
      xhr.upload.onload = () => progress(1);
      xhr.onload = () => {
        let data = null;
        try {
          data = JSON.parse(xhr.responseText);
        } catch {
          // not JSON
        }
        if (xhr.status === 401) lostAdmin();
        if (xhr.status >= 200 && xhr.status < 300) resolve(data);
        else reject(new APIError(xhr.status, (data && data.error) || `request failed (${xhr.status})`, data && data.notJoined));
      };
      xhr.onerror = () => reject(new APIError(0, 'cannot reach the daemon'));
      xhr.send(JSON.stringify(body));
    });
  }

  function setAdmin(on) {
    if (admin === on) return;
    admin = on;
    if (!on) {
      leaveChat();
      closeMenu(false);
      dropCaches();
      S.remote.clear();
      S.workflows.clear();
      S.usage.clear();
      S.host.clear();
      S.update.clear();
      S.peerRoots.clear();
    } else if (dialogShows('unlock')) closeDialog();
    syncPush();
    tryOpenPending();
    scheduleRemote(0);
    scheduleWorkflows(0);
    scheduleUsage(0);
    if (on) checkHosts();
    renderSideList();
    renderMain();
    repaintSettings();
  }

  // lostAdmin handles a rejected token (rotated, or from another server).
  function lostAdmin() {
    const had = admin;
    setToken('');
    setAdmin(false);
    if (had) toast('Admin access ended: unlock again with the fleet key, or run `fleet web` on the server', 'warn');
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
    } catch {
      return; // daemon unreachable: keep the token, the next hello retries
    }
    if (s && s.admin) {
      setAdmin(true);
      if (fromLink) toast('This browser can now manage the fleet');
      return;
    }
    setToken('');
    setAdmin(false);
    toast(fromLink
      ? 'This admin link is no longer valid: run `fleet web` again'
      : 'The saved admin token is no longer valid: unlock again with the fleet key', 'warn');
  }

  function signOut() {
    // The api call takes the token before it goes.
    if (push.on) stopPush();
    setToken('');
    setAdmin(false);
    toast('Signed out: this browser can only look now');
  }

  // ------------------------------------------------------------------- push
  //
  // Notifications when an agent needs input, finishes or fails (push.go).
  // Browsers offer push to secure contexts only, and iOS only to the web app
  // added to the Home Screen, so Settings says what is missing instead of
  // failing. Subscribing needs the admin token; the service worker (sw.js)
  // shows the notifications.

  const PUSH_OK = window.isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  const push = { key: '', on: false, busy: false, reg: null, endpoint: '' };
  let openPending = ''; // an agent to show once it is known: a tapped notification

  function pushHint() {
    if (!window.isSecureContext) return `Notifications need HTTPS: open this page over https (e.g. tailscale serve), not http://${location.host}.`;
    if (IOS && !navigator.standalone) return 'On iPhone, add this page to the Home Screen (Share › Add to Home Screen) and open it from there.';
    return 'This browser does not support push notifications.';
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
    if (!PUSH_OK || !admin) return repaintSettings();
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
    } catch {
      // e.g. 404: notifications are off on this daemon
    }
    repaintSettings();
  }

  async function toggleNotify() {
    if (!PUSH_OK) return toast(pushHint(), 'warn');
    if (push.busy) return;
    push.busy = true;
    repaintSettings();
    try {
      if (push.on) {
        await stopPush();
        toast('Notifications off on this device');
      } else {
        if (!push.reg || !push.key) throw new Error('not ready yet, try again in a moment');
        // Straight from the click: Safari asks for permission only from a
        // user gesture.
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
        toast('Notifications on: a test notification is on its way');
      }
    } catch (e) {
      if (window.Notification && Notification.permission === 'denied') toast('Notifications are blocked for this site in the browser or system settings', 'warn');
      else toast(`Notifications: ${e.message}`, 'error');
    }
    push.busy = false;
    repaintSettings();
  }

  async function testPush() {
    if (!push.on) return;
    try {
      await api('POST', '/api/push/test', { endpoint: push.endpoint });
      toast('A test notification is on its way');
    } catch (e) {
      if (e.status === 410) await stopPush();
      toast(`Notifications: ${e.message}`, 'error');
      repaintSettings();
    }
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
    if (!openPending || !admin) return;
    const a = S.agents.get(openPending);
    if (!a && !S.snapshotDone) return;
    openPending = '';
    if (a) openChat('', a.id, a);
  }

  function wirePush() {
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

  // ------------------------------------------------------------- the cache
  //
  // The page opens with what it showed last, like an app: the agents,
  // machines and roots (restoreSnapshot), and the conversations opened
  // lately (feedMem; Cache Storage across reloads), shown at once and
  // brought up to date as the daemon answers: a conversation asks for what
  // came after the copy only (see feedLoop). sw.js keeps the transcript
  // images. Signing out drops the conversations and images.

  const SNAP_KEY = 'fleet.snapshot';
  const SNAP_V = 1;
  let snapTimer = 0;

  function restoreSnapshot() {
    const s = load(SNAP_KEY, null);
    if (!s || s.v !== SNAP_V) return;
    S.server = s.server || null;
    S.devices = s.devices || [];
    S.peers = s.peers || [];
    S.roots = s.roots || [];
    for (const a of s.agents || []) if (a && a.id) S.agents.set(a.id, a);
    if (!getToken()) return;
    for (const [host, list] of Object.entries(s.remote || {})) {
      S.remote.set(host, { agents: new Map(list.filter((a) => a && a.id).map((a) => [a.id, a])), fails: 0 });
    }
  }

  // saveSnapshot keeps what the last complete snapshot and the news since
  // made of the lists.
  function saveSnapshot() {
    clearTimeout(snapTimer);
    snapTimer = 0;
    if (!S.snapshotDone) return;
    const remote = {};
    for (const [host, r] of S.remote) remote[host] = [...r.agents.values()];
    save(SNAP_KEY, { v: SNAP_V, server: S.server, devices: S.devices, peers: S.peers, roots: S.roots, agents: [...S.agents.values()], remote });
  }

  function snapshotSoon() {
    if (!snapTimer) snapTimer = setTimeout(saveSnapshot, 2000);
  }

  const FEED_KEEP = 40; // conversations kept
  const FEED_MAX = 600; // entries a kept one grows to with news, else it is fetched anew next time
  const FEEDS = 'fleet-feeds';
  const CACHES_OK = window.isSecureContext && 'caches' in window;
  const feedMem = new Map(); // agent key -> {file, start, end, entries, model, dirty}, the latest used last
  const feedWarm = new Map(); // agent key -> Promise of the copy warmFeed fetches
  const feedURL = (key) => '/feeds/' + encodeURIComponent(key);

  // keepFeed takes reply r of the conversation of agent key into its copy:
  // the latest entries (first, or a reset), else the news.
  function keepFeed(key, r, first) {
    let m = feedMem.get(key);
    if (first || r.reset) m = { file: r.file || '', start: r.start || 0, end: r.end || 0, entries: [...(r.entries || [])], model: r.model || null };
    else if (!m || m.file !== (r.file || '')) return;
    else if (r.entries && r.entries.length) {
      if (m.entries.length + r.entries.length > FEED_MAX) {
        feedMem.delete(key);
        return;
      }
      m.entries.push(...r.entries);
      m.end = r.end || m.end;
      if (r.model) m.model = r.model;
    } else return;
    m.dirty = true;
    feedMem.delete(key);
    feedMem.set(key, m);
    while (feedMem.size > FEED_KEEP) feedMem.delete(feedMem.keys().next().value);
    if (first || r.reset) storeFeed(key);
  }

  // storeFeed writes the copy of agent key's conversation to Cache Storage,
  // dropping the oldest beyond FEED_KEEP (keys come in the order put).
  async function storeFeed(key) {
    const m = feedMem.get(key);
    if (!CACHES_OK || !m || !m.dirty) return;
    m.dirty = false;
    try {
      const c = await caches.open(FEEDS);
      await c.put(feedURL(key), new Response(JSON.stringify(m), { headers: { 'Content-Type': 'application/json' } }));
      const keys = await c.keys();
      for (const k of keys.slice(0, Math.max(0, keys.length - FEED_KEEP))) await c.delete(k);
    } catch {
      // storage full or blocked: the copy lasts this page
    }
  }

  async function readFeed(key) {
    if (!CACHES_OK) return null;
    try {
      const res = await caches.match(feedURL(key), { cacheName: FEEDS });
      const m = res && (await res.json());
      if (!m || !Array.isArray(m.entries)) return null;
      if (!feedMem.has(key)) feedMem.set(key, m);
      return m;
    } catch {
      return null;
    }
  }

  // feedCopy is the copy of agent key's conversation, if there is one: at
  // once from memory, else what warmFeed is fetching, else Cache Storage.
  function feedCopy(key) {
    return feedMem.get(key) || feedWarm.get(key) || readFeed(key);
  }

  // warmFeed fetches a conversation about to open (a session row pressed or
  // pointed at), unless a copy is there already.
  function warmFeed(key) {
    if (!admin || feedMem.has(key) || feedWarm.has(key)) return;
    const { host, id } = splitKey(key);
    const base = host ? `/api/hosts/${encodeURIComponent(host)}/agents/` : '/api/agents/';
    const p = api('GET', base + encodeURIComponent(id) + '/chat').then((r) => {
      keepFeed(key, r, true);
      return feedMem.get(key) || null;
    }, () => null);
    feedWarm.set(key, p);
    p.then(() => feedWarm.delete(key));
  }

  function storeAll() {
    if (snapTimer) saveSnapshot();
    for (const key of feedMem.keys()) storeFeed(key);
  }

  function dropCaches() {
    feedMem.clear();
    if (!CACHES_OK) return;
    for (const name of [FEEDS, 'fleet-img', 'fleet-share']) caches.delete(name).catch(() => {});
  }

  // The app's badge counts the sessions that need you; sw.js sets it from
  // the daemon's count when a notification comes.
  let badgeShown = -1;
  function setBadge(n) {
    if (n === badgeShown || !navigator.setAppBadge) return;
    badgeShown = n;
    (n ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch(() => {});
  }

  // clearNotices closes the notifications about agent id, once its
  // session shows.
  function clearNotices(id) {
    if (!SHELL_OK || document.hidden || !id) return;
    navigator.serviceWorker.getRegistration()
      .then((reg) => (reg ? reg.getNotifications() : []))
      .then((list) => {
        for (const n of list) if (n.data && n.data.agent === id) n.close();
      })
      .catch(() => {});
  }

  // takeShare opens a new session with what another app shared with fleet
  // (sw.js keeps it): the text as the prompt, images and other files attached.
  let sharing = false;
  async function takeShare() {
    if (!CACHES_OK || sharing) return;
    sharing = true;
    let text = '';
    const files = [];
    try {
      const c = await caches.open('fleet-share');
      for (const req of await c.keys()) {
        const res = await c.match(req);
        if (!res) continue;
        if (new URL(req.url).pathname === '/share/text') text = (await res.text()).trim();
        else files.push(new File([await res.blob()], decodeURIComponent(res.headers.get('X-Name') || '') || 'shared', { type: res.headers.get('Content-Type') || '' }));
      }
      await caches.delete('fleet-share');
    } catch {
      // nothing to take
    }
    if (text || files.length) {
      go('#/new');
      const ready = () => admin && U.view === 'new' && N.ta && N.ta.isConnected && launch.loaded === launch.host && !!launch.adapters && !!launch.adapter;
      for (let t = 0; !ready() && t < 100; t++) await sleep(100);
      if (ready()) {
        if (text) {
          N.ta.value = N.ta.value.trim() ? N.ta.value.trimEnd() + '\n\n' + text : text;
          N.ta.dispatchEvent(new Event('input'));
        }
        if (files.length) addLaunchFiles(files);
        if (keyboard()) N.ta.focus();
      } else toast('Could not take what was shared: unlock this browser first', 'warn');
    }
    sharing = false;
  }

  // ------------------------------------------------------------ app shell
  //
  // The service worker (sw.js) keeps the page's files, so it opens without
  // waiting for the daemon. A daemon with other files hands out a new
  // worker, looked for when the socket connects again (a restart) and when
  // the page comes back into view. Once it takes over, the page loads anew:
  // at once if it just opened, else the next time it is hidden.

  const SHELL_OK = window.isSecureContext && 'serviceWorker' in navigator;
  const OPENED = Date.now();
  let shellChecked = 0;

  function wireShell() {
    if (!SHELL_OK) return;
    const had = !!navigator.serviceWorker.controller;
    navigator.serviceWorker.register('/sw.js').catch(() => {});
    navigator.serviceWorker.addEventListener('controllerchange', () => {
      if (!had) return; // the first worker: the page is current
      if (Date.now() - OPENED < 8000) return location.reload();
      addEventListener('visibilitychange', () => {
        if (document.hidden) location.reload();
      });
    });
    addEventListener('visibilitychange', () => {
      if (!document.hidden) checkShell();
    });
  }

  // wireLaunchQueue takes the launches of the installed app while it is
  // open (manifest launch_handler: focus-existing): a shortcut, a share.
  // The first one is the launch that opened the page, taken already.
  function wireLaunchQueue() {
    if (!('launchQueue' in window)) return;
    window.launchQueue.setConsumer((p) => {
      if (!p.targetURL || p.targetURL === LAUNCH_URL) return;
      const u = new URL(p.targetURL);
      const agent = u.searchParams.get('agent');
      if (u.searchParams.has('share')) takeShare();
      else if (agent) openFromPush(agent);
      else if (u.hash && u.hash !== location.hash) location.hash = u.hash;
    });
  }

  // checkShell asks for a newer service worker, once a minute at most.
  function checkShell() {
    if (!SHELL_OK || Date.now() - shellChecked < 60000) return;
    shellChecked = Date.now();
    navigator.serviceWorker.getRegistration().then((reg) => reg?.update()).catch(() => {});
  }

  // ------------------------------------------------------------------- keys
  //
  // N starts a session and / searches, outside text fields. Ctrl+1–9 opens
  // the live sessions in the list's order, also from the message box. In a
  // session Alt+A asks to archive it, Ctrl+C (with nothing selected)
  // interrupts the agent's turn as in its terminal. Esc goes back in the
  // panel or closes it, closes the drawer, and on phones goes back.

  function wireKeys() {
    document.addEventListener('keydown', (ev) => {
      if (ev.defaultPrevented || ev.isComposing) return;
      if (document.querySelector('dialog[open]') || MENU) return;
      const t = ev.target;
      const typing = t.closest && t.closest('input, textarea, select, [contenteditable]');
      if (ev.key === 'Escape') {
        if (insp.open && U.view === 'chat') {
          ev.preventDefault();
          // Out of a workflow agent's conversation or a file first.
          if (insp.tab === 'workflow' && wf.sub) return closeSub();
          if (insp.tab === 'files' && files.open) return closeFile();
          return closeInsp();
        }
        if (U.drawer) {
          ev.preventDefault();
          return toggleSide(false);
        }
        if (typing && t.id === 'side-q') return t.blur();
        if (small() && U.view !== 'home' && !typing) go('#/');
        return;
      }
      if (!admin) return;
      if (ev.ctrlKey && !ev.metaKey && !ev.altKey && !ev.shiftKey && /^[1-9]$/.test(ev.key)) {
        const row = document.querySelectorAll('#side-list .grp:not(.archived) .row')[Number(ev.key) - 1];
        if (!row) return;
        ev.preventDefault();
        if (U.key !== row.dataset.key || U.view !== 'chat') go('#/s/' + encodeURIComponent(row.dataset.key));
        return;
      }
      if (U.view === 'chat' && ev.altKey && !ev.ctrlKey && !ev.metaKey && !ev.shiftKey && ev.code === 'KeyA') {
        ev.preventDefault();
        if (chat.agent) confirmEnd(chat.host, chat.agent);
        return;
      }
      if (U.view === 'chat' && ev.ctrlKey && !ev.altKey && !ev.metaKey && !ev.shiftKey && ev.key.toLowerCase() === 'c' && chatBusy()) {
        if ((t.selectionStart ?? 0) !== (t.selectionEnd ?? 0) || String(getSelection())) return;
        ev.preventDefault();
        interruptChat();
        return;
      }
      if (typing || ev.ctrlKey || ev.metaKey || ev.altKey) return;
      if (ev.key === 'n' || ev.key === 'N') {
        ev.preventDefault();
        go('#/new');
      } else if (ev.key === '/') {
        ev.preventDefault();
        if (small() && U.view !== 'home') go('#/');
        else if (narrow()) toggleSide(true);
        setTimeout(() => $('side-q').focus(), small() ? 300 : 0);
      }
    });
  }

  // --------------------------------------------------------------- gestures
  //
  // Phones: a session or a view swiped to the right goes back to the list,
  // the panel swiped to the right goes back to the chat. The swipe starts
  // anywhere but in a text field or a box scrolled sideways, which the
  // finger scrolls back instead. The screen follows the finger, the list
  // under a session with it; let go past a third of the width, or flicked,
  // it slides out and only then goes back, else it slides back in.

  const SLIDE_MS = 260;
  const standalone = () => !!navigator.standalone || matchMedia('(display-mode: standalone)').matches; // .main, .insp and .side transitions on phones

  function wireGestures() {
    for (const el of [$('main'), $('insp')]) {
      const under = el === $('main') ? $('side') : null;
      let g = null; // {id, x0, y0, dx, on, t, v, w}
      const scrolledLeft = (n) => {
        for (; n && n !== el; n = n.parentElement) if (n.scrollLeft > 0) return true;
        return false;
      };
      const place = (x, w) => {
        el.style.transform = x == null ? '' : `translateX(${x}px)`;
        // The list comes in from 22% to the left, as the screen goes.
        if (under) under.style.transform = x == null ? '' : `translateX(${-22 * (1 - x / w)}%)`;
      };
      const animate = (on) => {
        el.style.transition = on ? '' : 'none';
        if (under) under.style.transition = on ? '' : 'none';
      };
      el.addEventListener('touchstart', (ev) => {
        if (g && g.on) return; // a second finger: the first one leads
        g = null;
        if (!small() || ev.touches.length !== 1 || el.dataset.leaving) return;
        if (el === $('main') && U.view === 'home') return;
        if (ev.target.closest('input, textarea, select, [contenteditable], .keys, .ctoolbar') || scrolledLeft(ev.target)) return;
        const t = ev.touches[0];
        // In the browser the edge swipe is its own back: not twice.
        if (t.clientX < 24 && !standalone()) return;
        g = { id: t.identifier, x0: t.clientX, y0: t.clientY, dx: 0, on: false, t: ev.timeStamp, v: 0, w: el.offsetWidth || innerWidth };
      }, { passive: true });
      el.addEventListener('touchmove', (ev) => {
        const t = g && [...ev.touches].find((x) => x.identifier === g.id);
        if (!t) return;
        const dx = t.clientX - g.x0;
        const dy = t.clientY - g.y0;
        if (!g.on) {
          // Up, down or left: not this gesture. The page may scroll already.
          if (Math.abs(dy) > 10 || dx < -10 || !ev.cancelable) return (g = null);
          if (dx < 12 || dx < 2 * Math.abs(dy)) return;
          g.on = true;
          g.x0 += 12; // from here on, no jump by the slop
          animate(false);
        }
        ev.preventDefault();
        const x = Math.max(0, t.clientX - g.x0);
        const dt = ev.timeStamp - g.t;
        if (dt > 0) g.v = 0.6 * g.v + 0.4 * ((x - g.dx) / dt); // px/ms
        g.t = ev.timeStamp;
        g.dx = x;
        place(x, g.w);
      }, { passive: false });
      const end = (ev) => {
        const s = g;
        if (ev.touches.length) return; // fingers still down
        g = null;
        if (!s || !s.on) return;
        const flick = Math.abs(s.v) > 0.45;
        const back = ev.type === 'touchend' && (flick ? s.v > 0 && s.dx > 24 : s.dx > s.w / 3);
        animate(true);
        if (!back) return place(null);
        // Out first; going back now would hide the panel or empty the
        // screen while it still slides.
        place(s.w, s.w);
        el.dataset.leaving = '1';
        setTimeout(() => {
          if (el === $('insp')) closeInsp();
          else go('#/');
          // The page's own rules now put it where it is: no jump.
          animate(false);
          place(null);
          requestAnimationFrame(() => requestAnimationFrame(() => animate(true)));
          delete el.dataset.leaving;
        }, SLIDE_MS);
      };
      el.addEventListener('touchend', end);
      el.addEventListener('touchcancel', end);
    }
  }

  // --------------------------------------------------------------- haptics
  //
  // Touch feedback. Safari has no navigator.vibrate, but it ticks the Taptic
  // Engine when the user toggles a switch checkbox (<input switch>), also
  // through its label. Since iOS 26.5 a label.click() from script no longer
  // does, only a real tap: so on iOS each control gets a clear label over it
  // with a hidden switch inside (see app.css), the way
  // github.com/tijnjh/ios-haptics does. The tap lands on the label and
  // bubbles to the control's handlers as before, but the label's default
  // action (toggling the switch) displaces the control's own: links and
  // submit buttons get theirs back below. Elsewhere navigator.vibrate
  // stands in.

  const TAPPABLE = 'button, a[href], summary, select, input[type="checkbox"], input[type="radio"]';
  const TYPING = 'textarea, input:not([type="checkbox"]):not([type="radio"])';
  const HAPTIC_HOST = 'button, a[href], summary';

  function addHaptic(el) {
    for (const c of el.children) if (c.hasAttribute('data-haptic')) return;
    const sw = h('input', { type: 'checkbox', switch: true, tabindex: '-1' });
    // The label passes its click on to the switch: keep that copy from
    // reaching the control's handlers a second time.
    sw.addEventListener('click', (ev) => ev.stopPropagation());
    const lab = h('label', { 'data-haptic': true, 'aria-hidden': 'true' }, sw);
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
        if (r.target.matches && r.target.matches(HAPTIC_HOST)) addHaptic(r.target);
        for (const n of r.addedNodes) if (n.nodeType === Node.ELEMENT_NODE) scan(n);
      }
    }).observe(document.body, { childList: true, subtree: true });
    // Whether the control's own default still runs differs between engines,
    // so see after the click what it did and do the rest. A link cannot
    // tell, so it loses its href for the click and is followed here.
    let submits = 0;
    document.addEventListener('submit', () => submits++, true);
    let linkTo = null;
    window.addEventListener('click', (ev) => {
      const lab = ev.target;
      linkTo = null;
      if (!(lab instanceof HTMLLabelElement) || !lab.hasAttribute('data-haptic')) return;
      const el = lab.parentElement;
      if (el.matches('a[href]')) {
        linkTo = el.getAttribute('href');
        el.removeAttribute('href');
        const href = linkTo;
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
    // On window, so after the page's handlers: a prevented default stays so.
    // Still in the tap, so a new tab is no popup.
    window.addEventListener('click', (ev) => {
      const href = linkTo;
      linkTo = null;
      if (href == null || ev.defaultPrevented) return;
      if (ev.target.parentElement.target === '_blank') openOut(href);
      else location.assign(href);
    });
  }

  // ------------------------------------------------------------- keyboard
  //
  // Phones: the screens fill the part of the page the on-screen keyboard
  // leaves (--vvh, from the top of what shows, --vvt), so the message box
  // sits on the keyboard and the header stays; iOS pans the page instead of
  // making it shorter. While the keyboard shows, the bottom safe area (the
  // home indicator, under the keyboard now) is no gap (.kb).

  function wireViewport() {
    const vv = window.visualViewport;
    if (!vv) return;
    const root = document.documentElement;
    let tall = 0; // the most height seen at this width: without a keyboard
    let width = 0;
    const fit = () => {
      if (innerWidth !== width) {
        width = innerWidth;
        tall = 0;
      }
      tall = Math.max(tall, vv.height);
      root.style.setProperty('--vvh', Math.round(vv.height) + 'px');
      root.style.setProperty('--vvt', Math.round(vv.offsetTop) + 'px');
      const t = document.activeElement;
      root.classList.toggle('kb', touch() && !!t && t.matches(TYPING) && vv.height < tall - 120);
    };
    vv.addEventListener('resize', fit);
    vv.addEventListener('scroll', fit);
    document.addEventListener('focusin', fit);
    document.addEventListener('focusout', () => setTimeout(fit, 50));
    fit();
  }

  // openOut opens href in a new tab. Installed on the home screen, iOS
  // would show it in a sheet over the app: Safari takes it instead.
  function openOut(href) {
    if (standalone() && /^https?:/i.test(href)) location.assign('x-safari-' + href);
    else window.open(href, '_blank', 'noopener,noreferrer');
  }

  function wireTouch() {
    // Without a touch listener iOS Safari skips :active.
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

  // ------------------------------------------------------------------ boot

  // tick keeps times fresh: elapsed ones every second, "4m ago" ones too
  // (they change once a minute at most).
  function tick() {
    for (const el of document.querySelectorAll('[data-since]')) {
      const t = dur(Date.now() - Number(el.dataset.since));
      if (el.textContent !== t) el.textContent = t;
    }
    for (const el of document.querySelectorAll('time[data-ts]')) {
      const ts = Number(el.dataset.ts);
      const t = el.dataset.long ? ago(ts) : rel(ts);
      if (el.textContent !== t) el.textContent = t;
    }
  }

  const LAUNCH_URL = location.href;
  const SHARED = new URLSearchParams(location.search).has('share');
  linkToken = takeLinkToken();
  setTheme(theme());
  restoreSnapshot();
  buildSide();
  if ($('toasts').showPopover) {
    $('toasts').popover = 'manual';
    $('toasts').showPopover();
  }
  wireMenus();
  wireDialog();
  wireViewer();
  wireSwipe();
  wireWarm();
  wireLaunch();
  wireKeys();
  wirePush();
  wireShell();
  wireTouch();
  wireViewport();
  wireGestures();
  $('side-scrim').addEventListener('click', () => toggleSide(false));
  addEventListener('hashchange', () => {
    // A `fleet web` link opened in a tab that already shows the page.
    if (takeLinkToken()) {
      checkSession(true);
      return;
    }
    route();
  });
  // Another tab signed in or out.
  addEventListener('storage', (ev) => {
    if (ev.key === TOKEN_KEY || ev.key === null) checkSession(false);
  });
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => setTheme(theme()));
  // Crossing a layout's width: the views draw their bars anew.
  let layout = `${small()}${narrow()}`;
  addEventListener('resize', () => {
    const now = `${small()}${narrow()}`;
    if (now === layout) return;
    layout = now;
    U.drawer = false;
    paintThemeColor();
    N.root = null;
    applyShell();
    renderSideList();
    if (U.view === 'chat') {
      renderChatHead();
      renderInsp();
    } else renderMain();
  });
  // Back to the front: connect again at once, and keep what shows for the
  // next start when going to the back (an app in the back may be ended).
  addEventListener('visibilitychange', () => {
    if (document.hidden) return storeAll();
    reconnectNow();
    if (U.view === 'chat') clearNotices(chat.id);
  });
  addEventListener('pagehide', storeAll);
  addEventListener('online', reconnectNow);
  wireLaunchQueue();
  setInterval(tick, 1000);
  setInterval(checkSocket, 5000);
  setConn('connecting');
  route();
  // A saved token is taken as good until the daemon says otherwise, so the
  // page opens unlocked (checkSession asks on the socket's hello).
  if (getToken()) setAdmin(true);
  connect();
  if (SHARED) {
    history.replaceState(null, '', location.pathname + location.hash);
    takeShare();
  }
})();
