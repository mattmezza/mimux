// SM service worker: network-first for everything (always fresh when online),
// with the cache as an offline-only fallback. This deliberately avoids serving
// stale JS/CSS/templates — a cache-first strategy previously pinned old assets
// in already-open browsers and broke the app after updates.
const CACHE = "sm-v3";
const STATIC = [
  "/static/css/dist.css",
  "/static/js/htmx.min.js",
  "/static/js/alpinejs.min.js",
  "/static/js/app.js",
  "/static/icons/favicon.svg",
];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(STATIC)));
  self.skipWaiting();
});

self.addEventListener("activate", (e) => {
  // Drop old caches, then take control of already-open pages immediately so a
  // refresh isn't needed for the new worker (and fresh assets) to apply.
  e.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  if (url.pathname === "/events") return; // never intercept SSE
  // Network-first: try the network (fresh), cache a copy, and fall back to the
  // cache only when offline.
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request))
  );
});
