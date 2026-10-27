// Service worker: keeps the page's own files so the dashboard opens
// without waiting for the network, keeps the transcript images it showed,
// takes what other apps share with fleet (manifest share_target), shows the
// daemon's push notifications (see push.go) with the app badge, and opens
// the agent a notification is about.
'use strict';

// The daemon serves this file with the version of its static files
// (assets.go). A new daemon means a new version, so a new worker, which
// fetches the shell again and drops the old copy.
const VERSION = '__SHELL_VERSION__';
const CACHE = 'fleet-shell-' + VERSION;
const SHELL = ['/', '/app.js', '/app.css', '/theme.js', '/favicon.svg', '/favicon.ico', '/manifest.webmanifest',
  '/icon-192.png', '/icon-512.png', '/icon-maskable-512.png', '/apple-touch-icon.png'];
// Transcript images (GET .../agents/<id>/image?...) never change: they are
// kept here, the latest IMG_KEEP, and dropped on sign-out by the page.
const IMAGES = 'fleet-img';
const IMG_KEEP = 300;
const IMAGE_PATH = /^\/api\/(?:hosts\/[^/]+\/)?agents\/[^/]+\/(?:workflows\/[^/]+\/agents\/[^/]+\/)?image$/;
// What another app shared, until the page takes it (see takeShare in app.js).
const SHARE = 'fleet-share';

self.addEventListener('install', (ev) => {
  ev.waitUntil((async () => {
    const c = await caches.open(CACHE);
    await c.addAll(SHELL.map((u) => new Request(u, { cache: 'reload' })));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (ev) => {
  ev.waitUntil((async () => {
    for (const k of await caches.keys()) {
      if (k.startsWith('fleet-shell-') && k !== CACHE) await caches.delete(k);
    }
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (ev) => {
  const req = ev.request;
  const url = new URL(req.url);
  if (url.origin !== location.origin) return;
  if (req.method === 'POST' && url.pathname === '/share') return ev.respondWith(takeShare(req));
  if (req.method !== 'GET') return;
  // The page, also opened with a query (?agent= from a notification,
  // ?share=), is the shell: from the cache.
  if (req.mode === 'navigate' && (url.pathname === '/' || url.pathname === '/index.html')) {
    return ev.respondWith(fromShell('/', req));
  }
  if (SHELL.includes(url.pathname)) return ev.respondWith(fromShell(url.pathname, req));
  if (IMAGE_PATH.test(url.pathname)) return ev.respondWith(image(req));
  // Everything else (the API, the socket, media) goes to the daemon.
});

async function fromShell(path, req) {
  const hit = await caches.match(path, { cacheName: CACHE });
  return hit || fetch(req.mode === 'navigate' ? path : req);
}

// image answers from the cache, else from the daemon, keeping the answer.
// The cache's keys are in the order they were put: the oldest go first.
async function image(req) {
  const c = await caches.open(IMAGES);
  const hit = await c.match(req.url);
  if (hit) return hit;
  const res = await fetch(req);
  if (res.ok) {
    await c.put(req.url, res.clone());
    const keys = await c.keys();
    for (const k of keys.slice(0, Math.max(0, keys.length - IMG_KEEP))) await c.delete(k);
  }
  return res;
}

// takeShare keeps what was shared and opens a new session with it: the
// text as its prompt, images and other files attached.
async function takeShare(req) {
  try {
    const form = await req.formData();
    const c = await caches.open(SHARE);
    for (const k of await c.keys()) await c.delete(k);
    const text = [form.get('title'), form.get('text'), form.get('url')].filter((s) => typeof s === 'string' && s.trim()).join('\n');
    const files = form.getAll('files').filter((f) => f && typeof f !== 'string');
    await c.put('/share/text', new Response(text));
    for (let i = 0; i < files.length; i++) {
      await c.put('/share/file/' + i, new Response(files[i], {
        headers: { 'Content-Type': files[i].type || 'application/octet-stream', 'X-Name': encodeURIComponent(files[i].name || '') },
      }));
    }
  } catch {
    // nothing usable came: the page opens a new session anyway
  }
  return Response.redirect('/?share=1#/new', 303);
}

// Safari revokes the permission of a site that receives a push without
// showing a notification, so every push shows one.
self.addEventListener('push', (ev) => {
  let n = {};
  try {
    n = ev.data ? ev.data.json() : {};
  } catch (e) {
    n = { body: ev.data.text() };
  }
  ev.waitUntil((async () => {
    // The badge counts the sessions that need you, as the daemon counted
    // them; an open dashboard sets it with the whole fleet's count.
    if (typeof n.badge === 'number' && self.navigator.setAppBadge) {
      await (n.badge ? self.navigator.setAppBadge(n.badge) : self.navigator.clearAppBadge()).catch(() => {});
    }
    await self.registration.showNotification(n.title || 'fleet', {
      body: n.body || '',
      tag: n.tag || undefined,
      renotify: !!n.tag,
      icon: '/icon-192.png',
      timestamp: Date.now(),
      data: { agent: n.agent || '' },
    });
  })());
});

// A tap focuses an open dashboard and has it show the agent, or opens one.
self.addEventListener('notificationclick', (ev) => {
  ev.notification.close();
  const agent = (ev.notification.data && ev.notification.data.agent) || '';
  ev.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const w of wins) {
      if (!('focus' in w)) continue;
      await w.focus();
      if (agent) w.postMessage({ type: 'openAgent', agent });
      return;
    }
    await self.clients.openWindow(agent ? '/?agent=' + encodeURIComponent(agent) : '/');
  })());
});
