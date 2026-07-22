// SM service worker: cache-first for static assets, network-first for pages.
const CACHE = "sm-v2";
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
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  if (url.pathname.startsWith("/static/")) {
    // Stale-while-revalidate: serve cache instantly (offline-capable) but always
    // refetch in the background so updated JS/CSS propagate on the next reload —
    // no cache-version bump needed for every asset change.
    e.respondWith(
      caches.open(CACHE).then((c) =>
        c.match(e.request).then((hit) => {
          const fetching = fetch(e.request)
            .then((res) => {
              if (res.ok) c.put(e.request, res.clone());
              return res;
            })
            .catch(() => hit);
          return hit || fetching;
        })
      )
    );
    return;
  }
  if (url.pathname === "/events") return; // never intercept SSE
  // Pages: network first, fall back to cache for offline read-only.
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
