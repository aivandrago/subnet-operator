/* Service worker for the installable dashboard.
   The app is static and its data is generated in the browser, so a cached shell works offline. */
const CACHE = 'subnet-dashboard-v7';
const SHELL = [
  '/dashboard/',
  '/dashboard/index.html',
  '/dashboard/manifest.webmanifest',
  '/dashboard/icon.svg',
  '/dashboard/icon-192.png',
  '/dashboard/icon-512.png',
  '/assets/dashboard.css?v=7',
  '/assets/dashboard.js?v=7',
];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// Network first, so a deployed change shows up immediately; the cache covers offline use.
self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET' || new URL(req.url).origin !== self.location.origin) return;
  event.respondWith(
    fetch(req)
      .then((res) => {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit || caches.match('/dashboard/index.html')))
  );
});
