/* Service worker for the installable dashboard.
   The demo is static and its data is generated in the browser, so a cached shell works offline.
   Paths are relative to this file, so the app works under any prefix — the site root, or
   kubectl proxy's --www-prefix. */
const CACHE = 'subnet-dashboard-v9';
const SHELL = [
  './',
  'index.html',
  'app.js?v=11',
  'app.css?v=11',
  'manifest.webmanifest',
  'icon.svg',
  'icon-192.png',
  'icon-512.png',
  '../assets/dashboard.css?v=11',
  '../assets/dashboard.js?v=11',
].map((p) => new URL(p, self.location).href);

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
// Only the app's own files are handled. Anything else — above all a Kubernetes API response,
// should this worker ever control a page that reads one — goes straight to the network and
// is never written to the cache.
const base = new URL('./', self.location).href;
self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET' || req.headers.has('Authorization')) return;
  const url = req.url.split('#')[0];
  if (!SHELL.includes(url) && !(req.mode === 'navigate' && url.startsWith(base))) return;
  event.respondWith(
    fetch(req)
      .then((res) => {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit || caches.match(new URL('index.html', self.location).href)))
  );
});
