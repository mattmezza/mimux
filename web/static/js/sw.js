// SM service worker: network-first for everything (always fresh when online),
// with the cache as an offline-only fallback. This deliberately avoids serving
// stale JS/CSS/templates — a cache-first strategy previously pinned old assets
// in already-open browsers and broke the app after updates.
const CACHE = "sm-v7";
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
// the sender, the subject, the account and the URL that opens this very message.
// See internal/mail/notify.go.
self.addEventListener("push", (e) => {
  let d = {};
  try {
    d = e.data ? e.data.json() : {};
  } catch (_) {
    d = {};
  }
  const title = d.title || "New mail";
  // The server sends an absolute URL (ntfy needs one); re-base it onto this
  // origin so a BaseURL that doesn't match where the app is actually open still
  // navigates in place rather than opening a second, foreign window.
  let url = "/";
  try {
    const u = new URL(d.url || "/", location.origin); // "" or missing => the app root
    url = u.pathname + u.search;
  } catch (_) {}
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
        data: { url },
        renotify: !visible,
        silent: visible,
        icon: "/static/icons/icon-192.png",
        // NOT the icon: Android renders the badge as a silhouette of the alpha
        // channel, and the app icon is an opaque plate — its silhouette is a
        // blank rounded square. /icon.svg?p=badge is the same mark in opaque
        // white on nothing, which is what that derivation needs.
        // ponytail: the icon stays the stock-palette PNG rather than the
        // themed /icon.svg — that URL is cached immutable and unversioned here,
        // so it would go stale on a colour change. Wire the version in if the
        // notification icon ever needs to follow the user's colours.
        badge: "/icon.svg?p=badge",
      });
    })()
  );
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  // Where the push said to go. Collapsed notifications (same tag) carry the
  // newest message's URL, which is the one the user is tapping about.
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(
    (async () => {
      // Focus the app if it is already open anywhere, rather than piling up
      // duplicate windows — and point it at the message.
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of wins) {
        if ("focus" in c) {
          await c.focus();
          // navigate() is same-origin only and can reject (it also fails on a
          // client the worker doesn't control); a focused window is still
          // better than a second one, so failure is not fatal.
          if ("navigate" in c) await c.navigate(url).catch(() => {});
          return;
        }
      }
      return self.clients.openWindow(url);
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
