// Run with: node --test internal/web/launch_test.cjs
// Exercise the dashboard's actual launcher helpers without booting its DOM.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

const app = readFileSync(join(__dirname, 'static/app.js'), 'utf8');
const providers = app.slice(app.indexOf("  const LAUNCH_PROVIDERS ="), app.indexOf('  function renderLaunch()'));
const start = app.slice(app.indexOf('  async function startSession('), app.indexOf('  function wireLaunch()'));
const adapters = [{ id: 'claude', available: true }, { id: 'codex', available: true }, { id: 'shell', available: true }];

function dashboard(storage = new Map(), storageBlocked = false) {
  const roots = [{ name: 'one', path: '/code/one' }, { name: 'two', path: '/code/two' }];
  const launch = { host: '', root: 'one', adapter: '', adapterRoot: '', adapters, images: [] };
  const S = { server: { id: 'local' }, agents: new Map(), remote: new Map() };
  const context = vm.createContext({
    S, launch, Date, root: roots[0],
    launchRoot: () => roots.find((r) => r.name === launch.root),
    localStorage: {
      getItem: (key) => { if (storageBlocked) throw Error('blocked'); return storage.get(key) || null; },
      setItem: (key, value) => { if (storageBlocked) throw Error('blocked'); storage.set(key, value); },
    },
    $: () => ({ value: '' }), checkedValue: () => '', launchImagesProblem: () => '',
    renderLaunch: () => {}, launchPick: () => null, launchAPI: () => '/api/agents',
    closeLaunch: () => {}, toast: () => {}, hostName: () => '', openChat: () => {}, invalidate: () => {},
    api: async () => ({ agent: { id: 'new', name: 'new' } }),
  });
  vm.runInContext(providers + '\n' + start, context);
  const pick = (list = adapters) => { context.list = list; vm.runInContext('pickAdapter(list)', context); return launch.adapter; };
  const remember = (host, root, adapter) => {
    Object.assign(context, { rememberedHost: host, rememberedRoot: root, rememberedAdapter: adapter });
    vm.runInContext('rememberRootProvider(rememberedHost, rememberedRoot, rememberedAdapter)', context);
  };
  return { context, roots, launch, S, pick, remember };
}

test('restores different providers per root, including after a page reload', () => {
  const storage = new Map();
  const d = dashboard(storage);
  d.remember('', d.roots[0], 'codex');
  d.remember('', d.roots[1], 'shell');
  assert.equal(d.pick(), 'codex');
  d.launch.root = 'two';
  assert.equal(d.pick(), 'shell');
  d.launch.root = 'one';
  assert.equal(d.pick(), 'codex');
  assert.equal(dashboard(storage).pick(), 'codex');
});

test('uses each machine independently, even with identical root paths', () => {
  const d = dashboard();
  d.remember('', d.roots[0], 'codex');
  d.remember('remote', d.roots[0], 'shell');
  assert.equal(d.pick(), 'codex');
  d.launch.host = 'remote';
  assert.equal(d.pick(), 'shell');
});

test('explicit choices survive rerenders and changes of subfolder', () => {
  const d = dashboard();
  d.remember('', d.roots[0], 'codex');
  assert.equal(d.pick(), 'codex');
  d.launch.adapter = 'shell';
  d.launch.sub = 'nested';
  assert.equal(d.pick(), 'shell');
  d.launch.adapterRoot = ''; // opening the dialog again restores the last launch
  assert.equal(d.pick(), 'codex');
});

test('falls back when remembered provider is unavailable or disallowed', () => {
  const d = dashboard();
  d.remember('', d.roots[0], 'codex');
  assert.equal(d.pick([{ id: 'claude', available: true }, { id: 'codex', available: false }]), 'claude');
  d.launch.adapterRoot = '';
  assert.equal(d.pick([{ id: 'shell', available: true }]), 'shell');
  assert.equal(d.pick([{ id: 'shell', available: false }]), '');
});

test('restores the latest local and remote session history on a fresh browser', () => {
  const d = dashboard();
  d.S.agents.set('old', { root: 'one', adapter: 'claude', createdAtMs: 10 });
  d.S.agents.set('new', { root: 'one', adapter: 'codex', createdAtMs: 20 });
  d.S.agents.set('other', { root: 'two', adapter: 'shell', createdAtMs: 30 });
  assert.equal(d.pick(), 'codex');
  d.S.remote.set('remote', { agents: new Map([['r', { root: 'one', adapter: 'shell', createdAtMs: 40 }]]) });
  d.launch.host = 'remote';
  assert.equal(d.pick(), 'shell');
});

test('newer session history wins over storage; older history does not', () => {
  const d = dashboard();
  d.remember('', d.roots[0], 'codex');
  d.S.agents.set('old', { root: 'one', adapter: 'shell', createdAtMs: 10 });
  assert.equal(d.pick(), 'codex');
  d.S.agents.set('new', { root: 'one', adapter: 'claude', createdAtMs: Date.now() + 1000 });
  d.launch.adapterRoot = '';
  assert.equal(d.pick(), 'claude');
});

test('disabled or malformed storage does not prevent launching', () => {
  for (const value of ['invalid json', '[]', 'null', '123']) {
    assert.equal(dashboard(new Map([['fleet.launch.providers', value]])).pick(), 'claude');
  }
  const d = dashboard(new Map(), true);
  d.remember('', d.roots[0], 'codex');
  assert.equal(d.pick(), 'codex');
});

test('waiting for adapters does not consume the root preference', () => {
  const d = dashboard();
  d.remember('', d.roots[0], 'codex');
  d.launch.adapters = null;
  assert.equal(d.pick([]), '');
  assert.equal(d.launch.adapterRoot, '');
  d.launch.adapters = adapters;
  assert.equal(d.pick(), 'codex');
});

test('successful launches remember the submitted root/provider even if the UI changes in flight', async () => {
  const storage = new Map();
  const d = dashboard(storage);
  d.pick();
  d.launch.adapter = 'codex';
  d.launch.then = 'stay';
  let resolve;
  d.context.api = () => new Promise((r) => { resolve = r; });
  const pending = vm.runInContext('startSession({ preventDefault() {} })', d.context);
  d.launch.root = 'two';
  d.launch.adapter = 'shell';
  resolve({ agent: { id: 'new', name: 'new' } });
  await pending;
  assert.equal(dashboard(storage).pick(), 'codex');
  d.launch.adapterRoot = '';
  assert.equal(d.pick(), 'claude');
});

test('failed launches keep the previous successful preference', async () => {
  const storage = new Map();
  const d = dashboard(storage);
  d.remember('', d.roots[0], 'codex');
  d.pick();
  d.launch.adapter = 'claude';
  d.context.api = async () => { throw Error('start failed'); };
  await vm.runInContext('startSession({ preventDefault() {} })', d.context);
  assert.equal(dashboard(storage).pick(), 'codex');
});
