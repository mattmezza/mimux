// SM service worker: network-first for everything (always fresh when online),
// with the cache as an offline-only fallback. This deliberately avoids serving
// stale JS/CSS/templates — a cache-first strategy previously pinned old assets
// in already-open browsers and broke the app after updates.
const CACHE = "sm-v5";
// The icon is deliberately NOT precached: it is served from /icon.svg?v=<hash>
// of the user's colour settings, so a precached URL would be a stale mark the
// moment they change one. The network-first fetch handler below caches it on
// first use anyway, under the versioned URL, which the next change invalidates
// by simply never being requested again.
const STATIC = [
  "/static/css/dist.css",
  "/static/js/htmx.min.js",
  "/static/js/alpinejs.min.js",
  "/static/js/app.js",
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

// --- Push notifications ---------------------------------------------------
// The payload arrives encrypted (only this install can decrypt it) and carries
// the sender, the subject and the account. See internal/mail/notify.go.
self.addEventListener("push", (e) => {
  let d = {};
  try {
    d = e.data ? e.data.json() : {};
  } catch (_) {
    d = {};
  }
  const title = d.title || "New mail";
  e.waitUntil(
    (async () => {
      // Never swallow a push. Chrome requires a visible notification for every
      // one (that is what userVisibleOnly buys) and shows its own "site updated
      // in the background" notice otherwise; Safari can revoke a subscription
      // that repeatedly displays nothing. So when the app is already on screen
      // — where the SSE stream has updated the list anyway — the notification
      // is shown silently and collapsed onto the previous one instead of being
      // skipped.
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      const visible = wins.some((c) => c.visibilityState === "visible");
      await self.registration.showNotification(title, {
        body: d.body || "",
        tag: "sm-" + (d.account || ""),
        renotify: !visible,
        silent: visible,
        icon: "/static/icons/icon-192.png",
        badge: "/static/icons/icon-192.png",
      });
    })()
  );
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  e.waitUntil(
    (async () => {
      // Focus the app if it is already open anywhere, rather than piling up
      // duplicate windows.
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of wins) {
        if ("focus" in c) return c.focus();
      }
      return self.clients.openWindow("/");
    })()
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
