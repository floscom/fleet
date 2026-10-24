// Service worker: keeps the page's own files so the dashboard opens
// without waiting for the network, shows the daemon's push notifications
// (see push.go) and opens the agent a notification is about.
'use strict';

// The daemon serves this file with the version of its static files
// (assets.go). A new daemon means a new version, so a new worker, which
// fetches the shell again and drops the old copy.
const VERSION = '__SHELL_VERSION__';
const CACHE = 'fleet-shell-' + VERSION;
const SHELL = ['/', '/app.js', '/app.css', '/theme.js', '/favicon.svg', '/manifest.webmanifest', '/icon-192.png'];

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

// The shell comes from the cache; the page itself also when opened with a
// query (?agent= from a notification). Everything else (the API, the
// socket, media, launch screens) goes to the daemon as before.
self.addEventListener('fetch', (ev) => {
  const req = ev.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== location.origin || !SHELL.includes(url.pathname)) return;
  ev.respondWith(caches.match(url.pathname, { cacheName: CACHE }).then((hit) => hit || fetch(req)));
});

// Safari revokes the permission of a site that receives a push without
// showing a notification, so every push shows one.
self.addEventListener('push', (ev) => {
  let n = {};
  try {
    n = ev.data ? ev.data.json() : {};
  } catch (e) {
    n = { body: ev.data.text() };
  }
  ev.waitUntil(self.registration.showNotification(n.title || 'fleet', {
    body: n.body || '',
    tag: n.tag || undefined,
    renotify: !!n.tag,
    icon: '/icon-192.png',
    data: { agent: n.agent || '' },
  }));
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
