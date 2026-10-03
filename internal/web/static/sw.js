// Service worker: shows the daemon's push notifications (see push.go) and
// opens the agent a notification is about. It caches nothing.
'use strict';

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (ev) => ev.waitUntil(self.clients.claim()));

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
