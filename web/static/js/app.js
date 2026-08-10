// SM app glue: htmx CSRF, service worker, SSE, toasts, keybindings.

// The active quick filter, read from its canonical DOM reflection (the
// :data-filter attribute Alpine sets on the inbox root). "" when not on the inbox.
function activeFilter() {
  return document.querySelector("[data-filter]")?.dataset.filter || "";
}

// Send the CSRF token on every htmx mutation.
document.addEventListener("htmx:configRequest", (e) => {
  const meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) e.detail.headers["X-CSRF-Token"] = meta.content;
  // Under the Unread filter, opening a message/thread must not mark it read.
  // Signal the server to skip the mark-read-at-open (and not set the delayed
  // pending flag either). POSTs (manual toggle) are unaffected.
  if (activeFilter() === "unread" && (e.detail.verb || "").toLowerCase() === "get"
      && /^\/(messages|t)\/\d+/.test(e.detail.path || "")) {
    e.detail.headers["X-SM-No-Auto-Read"] = "1";
  }
});

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js");
}

// --- App theme (dark/light/system), stored client-side in localStorage. The
// flash-free initial apply lives inline in base.html <head>; these keep it in
// sync when the user switches (Settings) or the OS preference flips. ---
function resolveTheme(mode) {
  if (mode === "dark" || mode === "light") return mode;
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}
function applyTheme() {
  var t = resolveTheme(localStorage.getItem("sm.theme") || "system");
  document.documentElement.dataset.theme = t;
  document.documentElement.style.colorScheme = t;
}
window.setTheme = function (mode) {
  try { localStorage.setItem("sm.theme", mode); } catch (_) {}
  applyTheme();
};
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  if ((localStorage.getItem("sm.theme") || "system") === "system") applyTheme();
});

// --- Alpine UI state (sidebar collapse, per-account tree open/closed, quick
// filter) persisted in localStorage so it survives reloads. Referenced by
// x-data="smUI" on the page roots. Registered on alpine:init so it's available
// regardless of script load order (a bare global raced Alpine's boot). ---
document.addEventListener("alpine:init", () => {
  Alpine.data("smUI", function () {
  const KEY = "sm.ui";
  let saved = {};
  try { saved = JSON.parse(localStorage.getItem(KEY)) || {}; } catch (_) {}
  // A ?filter= in the URL (bookmarked/shared) wins over the saved preference.
  const urlFilter = new URLSearchParams(location.search).get("filter") || "";
  return {
    sidebarOpen: false, // mobile drawer
    collapsed: !!saved.collapsed, // desktop sidebar hidden
    filter: urlFilter || saved.filter || "", // "" | "unread" | "starred"
    accts: saved.accts || {}, // account name -> true when collapsed
    _save() {
      try {
        localStorage.setItem(KEY, JSON.stringify({ collapsed: this.collapsed, filter: this.filter, accts: this.accts }));
      } catch (_) {}
    },
    toggleCollapse() { this.collapsed = !this.collapsed; this._save(); },
    acctOpen(name) { return !this.accts[name]; }, // default open
    // Reassign the whole object (not just a key) so Alpine reliably reacts even
    // for account names it hasn't seen before.
    toggleAcct(name) { this.accts = { ...this.accts, [name]: !this.accts[name] }; this._save(); },
    setFilter(f) { this.filter = this.filter === f ? "" : f; this._save(); this._syncFilterURL(); },
    // Keep the active filter in the address bar so it's bookmarkable, without
    // adding a history entry per toggle.
    _syncFilterURL() {
      const u = new URL(location.href);
      if (this.filter) u.searchParams.set("filter", this.filter);
      else u.searchParams.delete("filter");
      history.replaceState(history.state, "", u);
    },
  };
  });
});

// --- offline banner: sw.js falls back to the last-cached page when a fetch
// fails, but the user should know they're looking at stale data. ---
function updateOfflineBanner() {
  let el = document.getElementById("offline-banner");
  if (navigator.onLine) {
    el?.remove();
    return;
  }
  if (el) return;
  el = document.createElement("div");
  el.id = "offline-banner";
  el.setAttribute("role", "status");
  el.className = "fixed top-0 inset-x-0 z-[70] bg-amber-500 text-zinc-950 text-xs text-center py-1";
  el.textContent = "You're offline — showing the last synced inbox.";
  document.body.prepend(el);
}
window.addEventListener("online", updateOfflineBanner);
window.addEventListener("offline", updateOfflineBanner);
document.addEventListener("DOMContentLoaded", updateOfflineBanner);

// --- toasts ---
// opts.onUndo, when given, adds an "Undo" action and extends the auto-dismiss
// to 10s (5s otherwise). Stack is capped at 3 (oldest drops first).
function toast(msg, opts) {
  const box = document.getElementById("toasts");
  if (!box) return;
  while (box.children.length >= 3) box.firstElementChild.remove();
  const el = document.createElement("div");
  el.className =
    "pointer-events-auto max-w-xs rounded-md bg-zinc-800 border border-zinc-700 text-zinc-100 text-xs px-3 py-2 shadow-lg transition-opacity duration-150 flex items-center gap-3";
  el.setAttribute("role", "status");
  const label = document.createElement("span");
  label.className = "flex-1";
  label.textContent = msg || "Something went wrong.";
  el.appendChild(label);
  const dismiss = () => {
    el.style.opacity = "0";
    setTimeout(() => el.remove(), 200);
  };
  if (opts && opts.onUndo) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = "Undo";
    btn.className = "shrink-0 font-medium text-indigo-400 hover:text-indigo-300";
    btn.onclick = () => { opts.onUndo(); dismiss(); };
    el.appendChild(btn);
  }
  box.appendChild(el);
  setTimeout(dismiss, opts && opts.onUndo ? 10000 : 5000);
}

// Success toast with an Undo action for a just-moved message (archive/delete/
// spam). id/folderId are the message id and its folder *before* the move.
function toastUndo(label, id, folderId) {
  toast(label, {
    onUndo: () => {
      if (!window.htmx) return;
      htmx.ajax("POST", `/messages/${id}/undo-move`, { values: { folder: folderId }, swap: "none" })
        .then(() => document.body.dispatchEvent(new Event("sm:refresh")));
    },
  });
}
window.toastUndo = toastUndo;

// Sidebar list links (All inboxes / folders) swap #message-list via htmx. On
// the filters/drafts pages there is no #message-list, so intercept the click in
// the capture phase — before htmx's handler — and navigate to the link's href
// instead. stopPropagation keeps htmx from also trying (and failing) to swap.
document.addEventListener("click", (e) => {
  const link = e.target.closest && e.target.closest('a[hx-target="#message-list"]');
  if (!link || document.getElementById("message-list")) return;
  const href = link.getAttribute("href");
  if (href && href !== "#") {
    e.preventDefault();
    e.stopPropagation();
    window.location.assign(href);
  }
}, true);

// --- server-sent events ---
(function () {
  if (!window.EventSource) return;
  const es = new EventSource("/events");
  // The stream drops on any network change (wifi/VPN switch, laptop sleep).
  // EventSource reconnects on its own, but events sent while it was down are
  // gone — nothing replays them — so the list would sit stale until the next
  // poll. Refresh once on every reconnect. Skipped on the first open, where
  // the page has just loaded its own fresh data.
  let opened = false;
  es.addEventListener("open", () => {
    if (opened) { document.body.dispatchEvent(new Event("sm:refresh")); refreshUnreadTitle(); }
    opened = true;
  });
  es.addEventListener("new-mail", () => { document.body.dispatchEvent(new Event("sm:refresh")); refreshUnreadTitle(); });
  es.addEventListener("sync-status", () => { document.body.dispatchEvent(new Event("sm:sync")); refreshUnreadTitle(); });
  es.addEventListener("toast", (e) => toast(e.data));
  es.addEventListener("search-started", (e) => searchAcctState(JSON.parse(e.data).account, "start"));
  es.addEventListener("search-results", (e) => appendServerResults(JSON.parse(e.data)));
  es.addEventListener("search-done", (e) => { const d = JSON.parse(e.data); searchAcctState(d.account, "done", d.count); });
})();

// --- unread count in tab title ---
async function refreshUnreadTitle() {
  let n = 0;
  try { n = parseInt(await (await fetch("/unread")).text(), 10) || 0; } catch (e) { return; }
  const base = document.title.replace(/^\(\d+\)\s+/, "");
  document.title = n > 0 ? `(${n}) ${base}` : base;
}
document.addEventListener("DOMContentLoaded", refreshUnreadTitle);

// --- search: scope pill, folder tracking, suggestions, server results ---
const SCOPES = ["folder", "account", "all"];
function cycleScope() {
  const pill = document.getElementById("scope-pill");
  const input = document.getElementById("search-scope");
  if (!pill || !input) return;
  const next = SCOPES[(SCOPES.indexOf(input.value) + 1) % SCOPES.length];
  input.value = next;
  pill.textContent = next;
}
// #11: on iOS Safari, tapping this button right after the adjacent search
// input had focus can blur the input without firing "click" (the tap is
// swallowed as a focus-dismiss gesture), so the pill looks unresponsive on
// mobile. Handle touchend directly and skip the synthesized click that would
// otherwise double-fire cycleScope().
(function () {
  const pill = document.getElementById("scope-pill");
  if (!pill) return;
  pill.addEventListener("click", cycleScope);
  pill.addEventListener("touchend", (e) => { e.preventDefault(); cycleScope(); });
})();

// Keep the search form's folder context in sync with the visible list.
function syncSearchFolder() {
  const list = document.getElementById("message-list");
  const f = document.getElementById("search-folder");
  if (list && f && list.dataset.folder != null) f.value = list.dataset.folder;
}
document.addEventListener("htmx:afterSwap", (e) => {
  if (!e.target || e.target.id !== "message-list") return;
  syncSearchFolder();
  // Navigating the list (folder switch, search, g i / 0 / 1-9) with a message
  // open on mobile would leave the fullscreen reading pane sitting on top of
  // the new list. Background refreshes (sm:refresh from new-mail, undo,
  // pull-to-refresh) request from the list element itself — those must keep
  // the open message. On desktop the pane is a normal column, nothing to do.
  if (e.detail?.requestConfig?.elt?.id !== "message-list" &&
      window.matchMedia("(max-width: 767px)").matches) closeReadingPane();
});
document.addEventListener("DOMContentLoaded", syncSearchFolder);

function submitSearch(query) {
  const input = document.getElementById("search");
  const form = document.getElementById("search-form");
  if (!input || !form || !window.htmx) return;
  if (query != null) input.value = query;
  document.getElementById("search-suggest")?.classList.add("hidden");
  htmx.trigger(form, "submit");
}

// Suggestion clicks (recent-query buttons carry data-suggest-query).
document.addEventListener("click", (e) => {
  const q = e.target.closest("[data-suggest-query]");
  if (q) { submitSearch(q.dataset.suggestQuery); return; }
  // Click outside the search box closes the dropdown.
  if (!e.target.closest("#search-form")) document.getElementById("search-suggest")?.classList.add("hidden");
});

function starredSearch() {
  const scope = document.getElementById("search-scope");
  const pill = document.getElementById("scope-pill");
  if (scope) scope.value = "all";
  if (pill) pill.textContent = "all";
  submitSearch("is:starred");
}

// --- streamed server-search results ---
function shownMsgIds() {
  return new Set([...document.querySelectorAll("#message-list [data-msgid]")].map((el) => el.dataset.msgid));
}
function appendServerResults(data) {
  let ul = document.getElementById("search-local-items");
  if (!ul) {
    const list = document.getElementById("message-list");
    if (!list) return;
    ul = document.createElement("ul");
    ul.id = "search-local-items";
    list.appendChild(ul);
  }
  const seen = shownMsgIds();
  const tmp = document.createElement("tbody");
  tmp.innerHTML = data.html;
  [...tmp.children].forEach((row) => {
    const id = row.dataset.msgid;
    if (id && seen.has(id)) return; // dedup against local + already-streamed
    seen.add(id);
    ul.appendChild(row);
    if (window.htmx) htmx.process(row);
  });
}
function searchAcctState(account, state, count) {
  const el = document.querySelector(`[data-search-acct="${CSS.escape(account)}"]`);
  if (!el) return;
  if (state === "done") {
    el.querySelector(".search-spinner")?.classList.remove("animate-spin", "border-t-indigo-400");
    el.querySelector(".search-spinner")?.classList.add("border-t-transparent", "opacity-40");
    const c = el.querySelector(".search-acct-count");
    if (c) c.textContent = (count || 0) + " found";
  }
}

// --- keybinding manager ---
// Central map; entries return false to fall through without preventing default.
function listRows() {
  return [...document.querySelectorAll("#message-list [data-message-row]")];
}
function selectedRow() {
  return document.querySelector("#message-list [data-message-row].bg-zinc-800");
}
function selectRow(row) {
  if (!row) return;
  listRows().forEach((r) => r.classList.remove("bg-zinc-800"));
  row.classList.add("bg-zinc-800");
  row.scrollIntoView({ block: "nearest" });
}
function moveSelection(delta) {
  const rows = listRows();
  if (!rows.length) return true;
  let i = rows.indexOf(selectedRow());
  i = i < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.min(rows.length - 1, Math.max(0, i + delta));
  selectRow(rows[i]);
  return true;
}
function currentId() {
  const r = selectedRow();
  return r ? r.id.replace("msg-", "") : null;
}
function openSelected() {
  const r = selectedRow();
  if (r) r.click();
  return true;
}
function flagSelected(path) {
  const id = currentId();
  if (!id || !window.htmx) return true;
  htmx.ajax("POST", `/messages/${id}/${path}`, { target: `#msg-${id}`, swap: "outerHTML" });
  return true;
}
function moveSelected(path, label) {
  const id = currentId();
  const folderId = selectedRow()?.dataset.folder;
  if (!id || !window.htmx) return true;
  htmx.ajax("POST", `/messages/${id}/${path}`, { swap: "none" }).then(() => {
    closeReadingPane();
    removeRowAnimated(document.getElementById(`msg-${id}`));
    if (folderId) toastUndo(label, id, folderId);
  });
  return true;
}

// Fade + slide a row out, then drop it (see .row-removing in app.css).
function removeRowAnimated(el) {
  if (!el) return;
  el.classList.add("row-removing");
  setTimeout(() => el.remove(), 200);
}
window.removeRowAnimated = removeRowAnimated;

// Reset the reading pane back to its empty placeholder — used by back
// buttons, Escape (mobile), and after archive/delete/spam. Keeping the
// #reading-pane-empty marker is what tells the mobile layout CSS to show the
// list again (see app.css).
function closeReadingPane() {
  const rp = document.getElementById("reading-pane");
  if (rp) rp.innerHTML = '<p id="reading-pane-empty">Select a message to read it here.</p>';
  // Opening a thread pushes ?t=<id>&src=<src> (bookmarkable); drop it now so a
  // refresh on the list reloads the list, not the closed message.
  const p = new URLSearchParams(location.search);
  if (p.has("t")) {
    p.delete("t");
    p.delete("src");
    const qs = p.toString();
    history.replaceState(history.state, "", location.pathname + (qs ? "?" + qs : ""));
  }
}
window.closeReadingPane = closeReadingPane;

// --- thread view: collapse/expand a message, lazy-loading its body iframe ---
function toggleThreadMessage(header) {
  const item = header.closest("[data-thread-msg]");
  if (!item) return;
  const body = item.querySelector("[data-thread-body]");
  const expanded = item.classList.toggle("expanded");
  if (body) {
    body.hidden = !expanded;
    if (expanded) {
      const frame = body.querySelector("iframe[data-src]");
      if (frame && !frame.src) frame.src = frame.dataset.src;
    }
  }
}
window.toggleThreadMessage = toggleThreadMessage;

// --- resizable message-list column: drag #list-resizer to set --list-w on the
// stable #main-content (so the width survives htmx list swaps), persisted. ---
(function initListResize() {
  const KEY = "sm.listw";
  const MIN = 240, MAX = 700;
  const main = () => document.getElementById("main-content");
  function clamp(px) {
    const m = main();
    const hi = m ? Math.min(MAX, m.clientWidth - 320) : MAX;
    return Math.max(MIN, Math.min(px, hi));
  }
  function apply(px) {
    main()?.style.setProperty("--list-w", px + "px");
    document.getElementById("list-resizer")?.setAttribute("aria-valuenow", String(Math.round(px)));
  }
  function persist(px) { if (px > 0) localStorage.setItem(KEY, String(Math.round(px))); }
  const saved = parseInt(localStorage.getItem(KEY) || "", 10);
  if (saved > 0) apply(saved);
  // Keyboard: focus the separator and use arrow keys (Home/End for min/max).
  document.addEventListener("keydown", (e) => {
    if (!e.target || e.target.id !== "list-resizer") return;
    const cur = parseInt(getComputedStyle(main()).getPropertyValue("--list-w")) || 416;
    let next = cur;
    if (e.key === "ArrowLeft") next = cur - 24;
    else if (e.key === "ArrowRight") next = cur + 24;
    else if (e.key === "Home") next = MIN;
    else if (e.key === "End") next = MAX;
    else return;
    e.preventDefault();
    next = clamp(next);
    apply(next);
    persist(next);
  });
  document.addEventListener("mousedown", (e) => {
    const handle = e.target.closest && e.target.closest("#list-resizer");
    if (!handle) return;
    const m = main();
    if (!m) return;
    e.preventDefault();
    handle.classList.add("dragging");
    const left = m.getBoundingClientRect().left;
    document.body.style.userSelect = "none";
    document.body.style.cursor = "col-resize";
    let last = 0;
    const move = (ev) => {
      last = clamp(ev.clientX - left);
      apply(last);
    };
    const up = () => {
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
      handle.classList.remove("dragging");
      document.body.style.userSelect = "";
      document.body.style.cursor = "";
      persist(last);
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  });
})();

// --- pull-to-refresh (mobile): drag the message list down while it's already
// scrolled to the top to trigger a full re-sync (same as the Refresh button).
// Listeners live on document so they survive htmx list swaps.
(function initPullToRefresh() {
  const THRESHOLD = 70;
  let startY = 0, pulling = false, dist = 0;
  function indicator() {
    let el = document.getElementById("ptr-indicator");
    if (!el) {
      el = document.createElement("div");
      el.id = "ptr-indicator";
      el.innerHTML = '<svg class="w-4 h-4" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99"/></svg>';
      document.body.appendChild(el);
    }
    return el;
  }
  document.addEventListener("touchstart", (e) => {
    const list = e.target.closest && e.target.closest("#message-list");
    pulling = !!list && list.scrollTop <= 0;
    if (pulling) { startY = e.touches[0].clientY; dist = 0; }
  }, { passive: true });
  document.addEventListener("touchmove", (e) => {
    if (!pulling) return;
    dist = e.touches[0].clientY - startY;
    const el = indicator();
    if (dist <= 0) { el.style.opacity = "0"; return; }
    el.style.opacity = String(Math.min(1, dist / THRESHOLD));
    el.style.transform = `translate(-50%, ${Math.round(Math.min(dist, THRESHOLD + 30) * 0.4)}px)`;
    el.classList.toggle("ready", dist > THRESHOLD);
  }, { passive: true });
  document.addEventListener("touchend", () => {
    if (!pulling) return;
    pulling = false;
    const el = indicator();
    if (dist > THRESHOLD && window.htmx) {
      el.classList.add("spinning");
      htmx.ajax("POST", "/refresh", { swap: "none" }).then(() => {
        document.body.dispatchEvent(new Event("sm:refresh"));
        el.classList.remove("spinning", "ready");
        el.style.opacity = "0";
        el.style.transform = "";
      });
      toast("Syncing all accounts…");
    } else {
      el.classList.remove("ready");
      el.style.opacity = "0";
      el.style.transform = "";
    }
    dist = 0;
  });
})();

// --- translate: pull the rendered text out of a message-body iframe and POST
// it to /translate, dropping the result into resultEl. Same-origin iframe, so
// reading its text is allowed. ---
window.translateFrame = function (frame, resultEl) {
  if (!frame || !resultEl || !window.htmx) return;
  // Already translated once this open: toggle the cached panel — no re-fetch,
  // no repeat API/network call. "Show original" inside the panel hides it too.
  if (resultEl.dataset.translated === "1") {
    resultEl.classList.toggle("hidden");
    return;
  }
  let text = "";
  try { text = frame.contentDocument?.body?.innerText || ""; } catch (_) {}
  if (!text.trim()) {
    resultEl.innerHTML = '<p class="text-xs text-zinc-500 px-4 py-2">Let the message finish loading, then translate.</p>';
    return;
  }
  resultEl.innerHTML = '<p class="text-xs text-zinc-500 px-4 py-2">Translating…</p>';
  htmx.ajax("POST", "/translate", { values: { text: text.slice(0, 5000) }, target: resultEl, swap: "innerHTML" })
    .then(() => {
      // Only mark cached on a real translation (not the error partial), so a
      // failed attempt still retries instead of toggling a stale error.
      if (resultEl.querySelector("[data-translated-ok]")) resultEl.dataset.translated = "1";
    });
};

// --- attachments: inline preview (image/pdf/text) without downloading. The
// attachment endpoint is same-origin and served under a locked-down CSP, so
// embedding it here is safe. Toggles the preview panel; loads it only once. ---
window.previewAttachment = function (btn, url, kind) {
  const holder = btn.closest("[data-attachment]")?.querySelector("[data-preview]");
  if (!holder) return;
  if (holder.dataset.loaded) { holder.classList.toggle("hidden"); return; }
  holder.dataset.loaded = "1";
  holder.classList.remove("hidden");
  if (kind === "image") {
    holder.innerHTML = `<img src="${url}" alt="" class="max-h-96 max-w-full rounded">`;
  } else {
    holder.innerHTML = `<iframe src="${url}" class="w-full h-96 rounded border-0 bg-white" title="Attachment preview"></iframe>`;
  }
};

// --- per-message dark/light: email bodies render light by default (many
// hardcode black text). A Settings pref sets the default mode; another remembers
// the per-message choice in localStorage. applyBodyTheme runs on iframe load;
// toggleBodyTheme flips + (when remembering) persists the choice. ---
function bodyThemePrefs() {
  const d = document.getElementById("message-detail");
  return { dark: d?.dataset.darkDefault === "1", remember: d?.dataset.rememberTheme === "1" };
}
function msgIdFromFrame(frame) {
  const src = (frame && (frame.getAttribute("src") || frame.getAttribute("data-src"))) || "";
  const m = src.match(/\/messages\/(\d+)\/body/);
  return m ? m[1] : "";
}
window.applyBodyTheme = function (frame) {
  const doc = frame && frame.contentDocument;
  if (!doc) return;
  const { dark, remember } = bodyThemePrefs();
  let wantDark = dark;
  if (remember) {
    const saved = localStorage.getItem("sm.msgtheme." + msgIdFromFrame(frame));
    if (saved) wantDark = saved === "dark";
  }
  doc.documentElement.classList.toggle("sm-dark", wantDark);
  fitBodyWidth(frame);
  setupBodyZoom(frame);
};

// --- email body zoom: fit-to-width by default, pinch / ctrl+wheel to zoom ---
// Transform-based, NOT CSS zoom (zoom on the iframe's <html> crushed layouts
// on mobile engines — see 9e45774): scale the body from the top-left and widen
// its layout box by the inverse, so the visual width always equals the iframe
// width. Zooming in narrows the layout width, so text reflows larger instead
// of forcing horizontal panning.
function bodyZoom(frame, scale) {
  const doc = frame.contentDocument;
  if (!doc || !doc.body) return;
  frame.dataset.zoom = scale;
  const b = doc.body;
  if (Math.abs(scale - 1) < 0.001) {
    b.style.transform = b.style.width = b.style.boxSizing = doc.documentElement.style.height = "";
    return;
  }
  b.style.transformOrigin = "0 0";
  b.style.transform = "scale(" + scale + ")";
  b.style.boxSizing = "border-box";
  b.style.width = 100 / scale + "%";
  // The layout height doesn't scale with the transform; clamp the document to
  // the visual height so zooming out doesn't leave a dead scroll area.
  doc.documentElement.style.height = b.getBoundingClientRect().height + "px";
}
function fitBodyWidth(frame) {
  const doc = frame.contentDocument;
  if (!doc || !doc.body || !frame.clientWidth) return;
  bodyZoom(frame, 1); // reset so the natural width is measurable
  const fit = Math.min(1, frame.clientWidth / doc.documentElement.scrollWidth);
  frame.dataset.fitZoom = fit;
  if (fit < 1) bodyZoom(frame, fit);
}
function setupBodyZoom(frame) {
  const doc = frame.contentDocument;
  if (!doc) return;
  // Listeners go on each freshly loaded document, so re-loads never stack.
  const clamp = (z) => Math.min(4, Math.max(+frame.dataset.fitZoom || 1, z));
  doc.addEventListener("wheel", (e) => {
    if (!e.ctrlKey) return; // plain scrolling stays scrolling
    e.preventDefault();
    bodyZoom(frame, clamp((+frame.dataset.zoom || 1) * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
  }, { passive: false });
  let pinch = null;
  const dist = (e) => Math.hypot(e.touches[0].clientX - e.touches[1].clientX, e.touches[0].clientY - e.touches[1].clientY);
  doc.addEventListener("touchstart", (e) => {
    if (e.touches.length === 2) pinch = { d: dist(e), z: +frame.dataset.zoom || 1 };
  }, { passive: true });
  doc.addEventListener("touchmove", (e) => {
    if (!pinch || e.touches.length !== 2) return;
    e.preventDefault(); // keep the page itself from scrolling/zooming
    bodyZoom(frame, clamp(pinch.z * (dist(e) / pinch.d)));
  }, { passive: false });
  doc.addEventListener("touchend", () => { pinch = null; });
}
// Re-fit on rotation/resize (e.g. phone flipped landscape), debounced since
// resize fires continuously while dragging a desktop window.
let fitResizeTimer = null;
window.addEventListener("resize", () => {
  clearTimeout(fitResizeTimer);
  fitResizeTimer = setTimeout(() => {
    document.querySelectorAll('iframe[src*="/body"]').forEach((f) => {
      if (f.contentDocument) fitBodyWidth(f);
    });
  }, 150);
});
// The detail-pane star button posts to /star or /unstar and syncs the
// matching list row (hx-target/hx-swap above), but the button itself isn't
// part of that swapped fragment — flip its own state here so the next click
// posts the opposite action instead of repeating the same one forever.
window.toggleStarBtn = function (btn) {
  const nowStarred = btn.getAttribute("hx-post").endsWith("/star");
  btn.setAttribute("hx-post", btn.getAttribute("hx-post").replace(/\/(star|unstar)$/, nowStarred ? "/unstar" : "/star"));
  if (window.htmx) htmx.process(btn);
  btn.classList.toggle("text-amber-400", nowStarred);
  btn.classList.toggle("text-zinc-400", !nowStarred);
  const svg = btn.querySelector("svg");
  if (svg) svg.setAttribute("fill", nowStarred ? "currentColor" : "none");
  btn.title = (nowStarred ? "Unstar" : "Star") + " (s)";
  btn.setAttribute("aria-label", nowStarred ? "Unstar" : "Star");
};
window.toggleBodyTheme = function (frame) {
  const doc = frame && frame.contentDocument;
  if (!doc) return;
  const isDark = doc.documentElement.classList.toggle("sm-dark");
  if (bodyThemePrefs().remember) {
    const id = msgIdFromFrame(frame);
    if (id) localStorage.setItem("sm.msgtheme." + id, isDark ? "dark" : "light");
  }
};

// --- compose ---
function openMessageId() {
  return document.getElementById("message-detail")?.dataset.messageId || null;
}
// Remembers what had focus before compose opened, so closing it can put focus
// back (e.g. on the list) instead of dropping it on <body>.
let preComposeFocus = null;
function openCompose(query) {
  if (!window.htmx) return true;
  preComposeFocus = document.activeElement;
  htmx.ajax("GET", `/compose${query}`, { target: "#compose-root", swap: "innerHTML" });
  return true;
}
function replyCompose(mode) {
  const id = openMessageId();
  if (!id) return true;
  return openCompose(`?reply=${id}&mode=${mode}`);
}
// Serialized snapshot of the compose's initial state, taken after prefill and
// editor init (htmx:afterSwap below). closeCompose() compares the live state to
// this to decide whether to show the close guard. Null when compose is closed.
let composeBaseline = null;
let composeCloseAfterSave = false;

// composeState serializes the user-editable fields into a comparable string.
// The WYSIWYG body is read as the contenteditable's innerHTML — the same
// browser-normalized form the baseline was captured in, so a no-op open/close
// compares equal. Non-wysiwyg reads the textarea directly.
function composeState() {
  const form = document.getElementById("compose-form");
  if (!form) return null;
  const g = (n) => form.querySelector(`[name="${n}"]`)?.value ?? "";
  const editor = document.getElementById("compose-wysiwyg");
  const body = editor ? editor.innerHTML : g("body");
  return JSON.stringify([g("from"), g("to"), g("cc"), g("bcc"), g("subject"), body]);
}
function snapshotComposeBaseline() { composeBaseline = composeState(); }
function markComposeClean() { snapshotComposeBaseline(); }
function isComposeDirty() {
  return composeBaseline !== null && composeState() !== composeBaseline;
}
window.markComposeClean = markComposeClean;

// forceCloseCompose tears down the compose without any guard — used after a
// send/schedule/discard/save-and-close, where there is nothing left to protect.
function forceCloseCompose() {
  const root = document.getElementById("compose-root");
  if (root) root.innerHTML = "";
  if (preComposeFocus && document.contains(preComposeFocus)) preComposeFocus.focus();
  preComposeFocus = null;
  composeBaseline = null;
  composeCloseAfterSave = false;
}
window.forceCloseCompose = forceCloseCompose;

// closeCompose is every user-initiated close path (X button, Escape). Dirty →
// show the guard modal; clean → close immediately with no modal.
function closeCompose() {
  if (isComposeDirty()) {
    document.getElementById("compose-close-guard")?.removeAttribute("hidden");
    return;
  }
  forceCloseCompose();
}
window.closeCompose = closeCompose;

function composeGuardKeep() {
  const m = document.getElementById("compose-close-guard");
  if (m) m.hidden = true;
}
function composeGuardDiscard() {
  const m = document.getElementById("compose-close-guard");
  if (m) m.hidden = true;
  // Discard only drops the newer edits; any draft row saved earlier this session
  // stays as-is (we simply don't UPDATE it).
  forceCloseCompose();
}
function composeGuardSave() {
  const m = document.getElementById("compose-close-guard");
  if (m) m.hidden = true;
  composeCloseAfterSave = true;
  document.getElementById("compose-save-draft")?.click();
}
window.composeGuardKeep = composeGuardKeep;
window.composeGuardDiscard = composeGuardDiscard;
window.composeGuardSave = composeGuardSave;

// onDraftSaved runs after the "Save draft" hx-post returns: confirm, mark the
// session clean, and finish a save-and-close if the guard requested one.
function onDraftSaved(event) {
  if (!event.detail.successful) return;
  toast("Draft saved");
  markComposeClean();
  if (composeCloseAfterSave) forceCloseCompose();
}
window.onDraftSaved = onDraftSaved;

// --- send later / undo send / schedule / attachment reminder ---
// Attachment-hint keywords (English + Italian). Mirrors mail.attachWords in
// internal/mail/attachhint.go — keep the two in sync.
const attachKeywords = /\b(attach|attached|attachment|attachments|attaching|enclosed|allegato|allegati|allegata|allegate|allego)\b/i;

function setSendMode(m) {
  const el = document.getElementById("compose-send-mode");
  if (el) el.value = m;
}

// Fires the compose submit after the attachment-reminder gate. skipGate=true
// bypasses it ("Send anyway"). The primary Send button, "Send now" and the
// schedule/Ctrl+Enter paths all route through here so the gate is enforced once.
function submitCompose(skipGate) {
  const form = document.getElementById("compose-form");
  if (!form) return true;
  document.querySelector("details.send-more[open]")?.removeAttribute("open");
  syncComposeEditor();
  if (!skipGate && needsAttachmentReminder(form)) {
    const modal = document.getElementById("attach-reminder-modal");
    if (modal) { modal.hidden = false; return true; }
  }
  form.requestSubmit();
  return true;
}
window.submitCompose = submitCompose;
window.setSendMode = setSendMode;

function needsAttachmentReminder(form) {
  const files = form.querySelector('input[type=file][name="attachments"]');
  if (files && files.files && files.files.length) return false;
  const subject = form.querySelector('[name="subject"]')?.value || "";
  const body = form.querySelector('textarea[name="body"]')?.value || "";
  // Strip HTML tags so the WYSIWYG markup doesn't hide/emit false keywords.
  const text = (subject + " " + body).replace(/<[^>]+>/g, " ");
  return attachKeywords.test(text);
}

// Handles the 204 from POST /compose: close compose, then show the right toast
// (Undo for delayed send, a confirmation for scheduled send). Non-204 responses
// re-render the form inline with an error, so we leave those alone.
function onComposeResponse(event) {
  const xhr = event.detail.xhr;
  if (!xhr || xhr.status !== 204) return;
  const outboxId = xhr.getResponseHeader("SM-Outbox-Id");
  const scheduled = xhr.getResponseHeader("SM-Scheduled");
  forceCloseCompose();
  if (outboxId) {
    toast("Sending…", {
      onUndo: () => {
        if (!window.htmx) return;
        htmx.ajax("POST", `/outbox/${outboxId}/undo`, { target: "#compose-root", swap: "innerHTML" });
      },
    });
  } else if (scheduled) {
    const d = new Date(scheduled);
    toast("Scheduled for " + (isNaN(d) ? scheduled : d.toLocaleString()) + ".");
  }
}
window.onComposeResponse = onComposeResponse;

// --- schedule picker ---
function openSchedulePicker() {
  document.querySelector("details.send-more[open]")?.removeAttribute("open");
  const tz = document.getElementById("schedule-tz");
  if (tz && !tz.options.length) {
    let zones = [];
    try { zones = Intl.supportedValuesOf("timeZone"); } catch (e) { zones = []; }
    const local = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (!zones.length) zones = [local, "UTC"];
    for (const z of zones) {
      const o = document.createElement("option");
      o.value = z; o.textContent = z;
      if (z === local) o.selected = true;
      tz.appendChild(o);
    }
  }
  const when = document.getElementById("schedule-when");
  if (when && !when.value) {
    const d = new Date(Date.now() + 3600000);
    d.setSeconds(0, 0);
    const pad = (n) => String(n).padStart(2, "0");
    when.value = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
  const modal = document.getElementById("schedule-modal");
  if (modal) modal.hidden = false;
}
window.openSchedulePicker = openSchedulePicker;

// Interpret a wall-clock "YYYY-MM-DDTHH:MM" as local time in IANA zone tz and
// return the corresponding UTC Date. Uses Intl to read tz's offset; DST-boundary
// hours are approximated (acceptable for a send scheduler).
function zonedWallToUTC(wall, tz) {
  const [d, t] = wall.split("T");
  const [Y, Mo, D] = d.split("-").map(Number);
  const [H, Mi] = t.split(":").map(Number);
  const guess = Date.UTC(Y, Mo - 1, D, H, Mi);
  const dtf = new Intl.DateTimeFormat("en-US", {
    timeZone: tz, hour12: false, year: "numeric", month: "2-digit",
    day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
  const p = Object.fromEntries(dtf.formatToParts(new Date(guess)).map((x) => [x.type, x.value]));
  const asUTC = Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour % 24, +p.minute, +p.second);
  return new Date(guess - (asUTC - guess));
}

function confirmSchedule() {
  const when = document.getElementById("schedule-when")?.value;
  const tz = document.getElementById("schedule-tz")?.value;
  if (!when) { toast("Pick a date and time."); return; }
  const utc = zonedWallToUTC(when, tz);
  if (isNaN(utc.getTime())) { toast("Invalid date."); return; }
  document.getElementById("compose-send-at").value = utc.toISOString();
  setSendMode("schedule");
  const modal = document.getElementById("schedule-modal");
  if (modal) modal.hidden = true;
  submitCompose();
}
window.confirmSchedule = confirmSchedule;

// Localize [data-utc] timestamps (e.g. the Drafts-page Scheduled list) to the
// viewer's zone. Server renders a UTC fallback so no-JS still reads sensibly.
function localizeTimes() {
  document.querySelectorAll(".local-time[data-utc]").forEach((el) => {
    const d = new Date(el.dataset.utc);
    if (!isNaN(d)) el.textContent = d.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  });
}
document.addEventListener("DOMContentLoaded", localizeTimes);

// Focus management: compose opens onto the "To" field; a message opening
// moves focus into the reading pane so keyboard/AT users land somewhere sane.
document.addEventListener("htmx:afterSwap", (e) => {
  if (e.target && e.target.id === "compose-root" && e.target.firstElementChild) {
    initComposeEditor();
    // Snapshot the initial state (after prefill + WYSIWYG normalization) so the
    // close guard can tell real edits from a no-op open. Every entry point —
    // new, reply prefill, reopened draft, undo-send — swaps here, so each gets
    // its own clean baseline.
    snapshotComposeBaseline();
    e.target.querySelector('[name="to"]')?.focus();
  }
  if (e.target && e.target.id === "reading-pane") {
    const detail = e.target.querySelector("#message-detail");
    detail?.focus({ preventScroll: true });
    if (detail && detail.dataset.markReadPending) {
      // Delay configured: flip the row once the timer fires.
      scheduleMarkRead(detail);
    } else if (detail && activeFilter() !== "unread") {
      // Delay 0 (or already read): the server marked it read at open, so flip
      // the list row now to match. Under the Unread filter the server skips the
      // read, so we must not flip the row either.
      markRowRead(detail.dataset.messageId);
    }
  }
});

// "⋯" quick-action menus (details/summary): close on outside click, and after
// a menu action is chosen (native behavior would leave them hanging open).
document.addEventListener("click", (e) => {
  document.querySelectorAll("details.msg-actions-more[open]").forEach((d) => {
    if (!d.contains(e.target) || e.target.closest(".msg-actions-more-menu button")) d.removeAttribute("open");
  });
});

// Flip a list row from unread to read in place: drop the dot, un-bold the text,
// and clear data-unread. Adds .just-read so the row stays visible even under an
// active "Unread" quick filter (per requirement — a just-read message shouldn't
// vanish out from under you; the next list refresh removes it correctly).
function markRowRead(id) {
  const row = id && document.getElementById(`msg-${id}`);
  if (!row || !row.hasAttribute("data-unread")) return;
  row.removeAttribute("data-unread");
  row.classList.add("just-read");
  row.querySelector(".unread-dot")?.remove();
  const sender = row.querySelector(".row-sender");
  if (sender) { sender.classList.remove("text-zinc-100", "font-semibold"); sender.classList.add("text-zinc-400"); }
  const subject = row.querySelector(".row-subject");
  if (subject) { subject.classList.remove("text-zinc-100", "font-medium"); subject.classList.add("text-zinc-400"); }
  // Brief highlight so the state change is noticeable when it fires on a delay.
  row.classList.add("read-flash");
  setTimeout(() => row.classList.remove("read-flash"), 700);
}

// Inverse of markRowRead: flip a row back to unread in place (re-add the dot,
// re-bold). Used by the double-click read/unread toggle.
function markRowUnread(id) {
  const row = id && document.getElementById(`msg-${id}`);
  if (!row || row.hasAttribute("data-unread")) return;
  row.setAttribute("data-unread", "");
  row.classList.remove("just-read");
  const senderRow = row.querySelector(".row-sender")?.parentElement;
  if (senderRow && !senderRow.querySelector(".unread-dot")) {
    const dot = document.createElement("span");
    dot.className = "unread-dot w-1.5 h-1.5 rounded-full bg-indigo-500 shrink-0";
    senderRow.insertBefore(dot, senderRow.firstElementChild);
  }
  const sender = row.querySelector(".row-sender");
  if (sender) { sender.classList.add("text-zinc-100", "font-semibold"); sender.classList.remove("text-zinc-400"); }
  const subject = row.querySelector(".row-subject");
  if (subject) { subject.classList.add("text-zinc-100", "font-medium"); subject.classList.remove("text-zinc-400"); }
}
window.markRowUnread = markRowUnread;
window.markRowRead = markRowRead;

// Flip one thread-detail message block's read indicator in place (dot + bold),
// mirroring markRowRead/Unread but for thread_detail's markup.
function setThreadMsgRead(block, read) {
  if (!block || read === !block.hasAttribute("data-unread")) return;
  const sender = block.querySelector(".thread-sender");
  if (read) {
    block.removeAttribute("data-unread");
    block.querySelector(".thread-unread-dot")?.remove();
    if (sender) { sender.classList.remove("text-zinc-100", "font-semibold"); sender.classList.add("text-zinc-300"); }
  } else {
    block.setAttribute("data-unread", "");
    const hdr = block.querySelector(".thread-msg-hdr");
    if (hdr && !hdr.querySelector(".thread-unread-dot")) {
      const dot = document.createElement("span");
      dot.className = "thread-unread-dot w-1.5 h-1.5 rounded-full bg-indigo-500 shrink-0";
      hdr.insertBefore(dot, hdr.firstElementChild);
    }
    if (sender) { sender.classList.add("text-zinc-100", "font-semibold"); sender.classList.remove("text-zinc-300"); }
  }
}

// Recompute the thread's list row from the pane: unread iff any message unread.
// The pane root (#message-detail) carries data-message-id === thread RootID.
function syncThreadRow(pane) {
  const rootId = pane && pane.dataset.messageId;
  if (!rootId) return;
  if (pane.querySelector("[data-thread-msg][data-unread]")) markRowUnread(rootId);
  else markRowRead(rootId);
}

// Thread header "Mark thread read/unread": flip every message in the pane and
// persist each change, then repaint the row. NOTE: one POST per changed
// message — threads are small; a bulk endpoint would need a thread_id the store
// doesn't have (threading is derived from headers at render time).
function markThreadRead(read) {
  const pane = document.getElementById("message-detail");
  if (!pane || !window.htmx) return;
  pane.querySelectorAll("[data-thread-msg]").forEach((block) => {
    if (read === !block.hasAttribute("data-unread")) return; // already in desired state
    const id = block.dataset.msgId;
    setThreadMsgRead(block, read);
    if (id) htmx.ajax("POST", `/messages/${id}/${read ? "read" : "unread"}`, { swap: "none" });
  });
  syncThreadRow(pane);
}

// Per-message read/unread inside a thread (the POST is declarative on the
// button; here we just reflect it in the pane + row).
function onThreadMsgToggled(btn, read) {
  setThreadMsgRead(btn.closest("[data-thread-msg]"), read);
  syncThreadRow(document.getElementById("message-detail"));
}
window.markThreadRead = markThreadRead;
window.onThreadMsgToggled = onThreadMsgToggled;

// Toggle a row's read/unread state in place (the POST response is ignored —
// swapping the single-message fragment would corrupt thread rows).
function toggleRowRead(row) {
  if (!row || !window.htmx) return;
  const id = row.id.replace(/^msg-/, "");
  if (!id) return;
  const makeRead = row.hasAttribute("data-unread"); // unread -> read, else -> unread
  if (markReadTimer) { clearTimeout(markReadTimer); markReadTimer = null; }
  htmx.ajax("POST", `/messages/${id}/${makeRead ? "read" : "unread"}`, { swap: "none" })
    .then(() => { if (makeRead) markRowRead(id); else markRowUnread(id); });
}

// Double-click (desktop) a list row to toggle its read/unread state. The
// single click that precedes it still opens the message.
document.addEventListener("dblclick", (e) => {
  const row = e.target.closest && e.target.closest("#message-list li[data-message-row]");
  if (row) toggleRowRead(row);
});

// #8: double-tap (mobile) toggles read/unread WITHOUT opening the message.
// Touch devices don't get a real dblclick before the row's own hx-get="click"
// navigates away on the first tap, so we hold the first tap's click for the
// double-tap window: a second tap within it cancels the open and toggles
// read/unread instead; otherwise the held tap opens the message as normal.
// Desktop mouse clicks never fire touchend, so this doesn't touch that path.
// A touch that moved more than MOVE_THRESHOLD px between start and end is a
// scroll/drag, not a tap — ignored entirely so lifting a finger mid-scroll
// doesn't open whatever row happens to be underneath.
(function () {
  const DOUBLE_TAP_MS = 300;
  const MOVE_THRESHOLD = 10;
  let pendingRow = null, pendingTimer = null;
  let startX = 0, startY = 0;
  document.addEventListener("touchstart", (e) => {
    const t = e.touches[0];
    if (!t) return;
    startX = t.clientX;
    startY = t.clientY;
  }, { passive: true });
  document.addEventListener("touchend", (e) => {
    const row = e.target.closest && e.target.closest("#message-list li[data-message-row]");
    if (!row || (e.target.closest && e.target.closest(".star-btn"))) return;
    const t = e.changedTouches[0];
    if (t && Math.hypot(t.clientX - startX, t.clientY - startY) > MOVE_THRESHOLD) return;
    e.preventDefault(); // suppress the synthesized click; we drive it ourselves
    if (pendingRow === row) {
      clearTimeout(pendingTimer);
      pendingTimer = null;
      pendingRow = null;
      toggleRowRead(row);
      return;
    }
    pendingRow = row;
    pendingTimer = setTimeout(() => {
      pendingRow = null;
      row.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    }, DOUBLE_TAP_MS);
  });
})();

// Restore a bookmarked open thread (?t=<id>&src=<src>) into the reading pane on
// full page load. The list itself is rendered server-side (handleInbox reads
// src); here we just open the thread and highlight its row.
document.addEventListener("DOMContentLoaded", () => {
  const p = new URLSearchParams(location.search);
  const t = p.get("t");
  if (!t || !window.htmx) return;
  const src = p.get("src") || "u";
  htmx.ajax("GET", `/t/${t}?src=${encodeURIComponent(src)}`, { target: "#reading-pane", swap: "innerHTML" });
  document.getElementById(`msg-${t}`)?.classList.add("bg-zinc-800");
});

// Mark-as-read-after-N-seconds: when the server defers read-marking (a delay is
// configured), the opened message carries data-mark-read-pending + a delay. We
// POST /read once it's been on screen that long, cancelling if the user leaves
// the message first, then flip the list row to read (the POST response is
// ignored — we update the row in place so thread rows and the Unread filter
// keep working).
let markReadTimer = null;
function scheduleMarkRead(detail) {
  if (markReadTimer) { clearTimeout(markReadTimer); markReadTimer = null; }
  if (!detail || !detail.dataset.markReadPending) return;
  const delay = parseInt(detail.dataset.markReadDelay || "0", 10);
  const id = detail.dataset.messageId;
  if (!id || !(delay > 0) || !window.htmx) return;
  markReadTimer = setTimeout(() => {
    // Only fire if this message is still the one on screen, and the user hasn't
    // switched to the Unread filter mid-read (which turns off auto-read).
    const cur = document.querySelector("#message-detail");
    if (cur && cur.dataset.messageId === id && activeFilter() !== "unread") {
      htmx.ajax("POST", `/messages/${id}/read`, { swap: "none" }).then(() => markRowRead(id));
    }
  }, delay * 1000);
}

// Insert an AI draft above the (possibly quoted) compose body, mode-aware.
// In WYSIWYG mode `content` is a sanitized HTML fragment (isHTML=true) and goes
// straight into the contenteditable; plain/markdown modes get raw text prepended
// to the body textarea. The server already formats output per compose mode.
function insertAIDraft(content, isHTML) {
  const editor = document.getElementById("compose-wysiwyg");
  if (editor && isHTML) {
    editor.innerHTML = content + editor.innerHTML;
    syncComposeEditor();
    return;
  }
  if (editor) {
    // plain text into a wysiwyg editor (shouldn't normally happen): paragraph-ize
    const html = content.split(/\n{2,}/).map((p) =>
      `<p>${escapeHTML(p).replace(/\n/g, "<br>")}</p>`).join("");
    editor.innerHTML = html + editor.innerHTML;
    syncComposeEditor();
    return;
  }
  const body = document.querySelector('#compose-form textarea[name="body"]');
  if (body) {
    body.value = content + (body.value ? "\n\n" + body.value : "");
    body.dispatchEvent(new Event("input", { bubbles: true }));
  }
}

// aiAssist drives the compose "Draft with AI" panel (partials/compose.html).
// Replies fetch candidate directions from POST /ai/options; picking one (or the
// free-text box) generates a formatted draft via POST /ai/draft; refine chips
// hit POST /ai/refine. Everything is JSON; the draft is only committed to the
// editor on "Insert", so refining never clobbers the quoted thread.
document.addEventListener("alpine:init", () => {
  Alpine.data("aiAssist", (init) => ({
    kind: init.kind,
    mode: init.mode,
    isReply: init.kind === "reply" || init.kind === "reply_all",
    starters: ["Follow up", "Introduction", "Thank you"],
    open: false,
    loading: false,
    loadingMsg: "",
    stage: "menu",
    options: [],
    showFree: false,
    freeInput: "",
    draft: "",
    draftIsHTML: false,

    toggle() {
      this.open = !this.open;
      if (this.open && this.isReply && !this.options.length && !this.loading) this.loadOptions();
    },
    ctx() { return this.$refs.ctx ? this.$refs.ctx.textContent.trim() : ""; },
    subjectEl() { return document.querySelector('#compose-form [name="subject"]'); },

    async post(url, body) {
      const meta = document.querySelector('meta[name="csrf-token"]');
      const res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": meta ? meta.content : "" },
        body: new URLSearchParams(body).toString(),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || "AI request failed.");
      return data;
    },

    async loadOptions() {
      this.loading = true; this.loadingMsg = "Reading the message…";
      try {
        const data = await this.post("/ai/options", { context: this.ctx() });
        this.options = data.options || [];
      } catch (e) { this.open = false; toast(e.message); }
      finally { this.loading = false; }
    },

    async generate(direction) {
      this.loading = true; this.loadingMsg = "Writing your draft…";
      const wantSubject = !this.isReply && this.subjectEl() && !this.subjectEl().value.trim();
      try {
        const data = await this.post("/ai/draft", {
          mode: this.mode,
          context: this.isReply ? this.ctx() : "",
          direction,
          want_subject: wantSubject ? "1" : "0",
        });
        this.draft = data.draft || "";
        this.draftIsHTML = this.mode === "html";
        if (wantSubject && data.subject) {
          const s = this.subjectEl();
          s.value = data.subject;
          s.dispatchEvent(new Event("input", { bubbles: true }));
        }
        this.stage = "draft";
      } catch (e) { toast(e.message); }
      finally { this.loading = false; }
    },

    async refine(action) {
      this.loading = true; this.loadingMsg = "Refining…";
      try {
        const data = await this.post("/ai/refine", { mode: this.mode, text: this.draft, action });
        this.draft = data.draft || this.draft;
      } catch (e) { toast(e.message); }
      finally { this.loading = false; }
    },

    insert() {
      if (!this.draft.trim()) return;
      insertAIDraft(this.draft, this.draftIsHTML);
      this.open = false; this.stage = "menu"; this.freeInput = ""; this.showFree = false;
    },
  }));
});

function escapeHTML(s) {
  const d = document.createElement("div");
  d.textContent = s;
  return d.innerHTML;
}

// --- WYSIWYG / markdown compose editor ---
// Syncs the contenteditable into the hidden textarea[name=body] so the current
// content rides along on the next save/submit. Called on edit, after a toolbar
// command, before form submit, on AI insert, and before an explicit save.
function syncComposeEditor() {
  const editor = document.getElementById("compose-wysiwyg");
  if (!editor) return;
  const body = document.querySelector('#compose-form textarea[name="body"]');
  if (!body) return;
  body.value = editor.innerHTML;
}

let composeSavedRange = null;
function saveComposeRange() {
  const sel = window.getSelection();
  const editor = document.getElementById("compose-wysiwyg");
  if (sel && sel.rangeCount && editor && editor.contains(sel.anchorNode)) {
    composeSavedRange = sel.getRangeAt(0);
  }
}
// Track the editor selection on every change — crucially this fires even when a
// drag-selection is released outside the editor (e.g. over the toolbar), which
// an editor-local mouseup would miss, leaving a stale range for the next
// command. One global listener; a no-op when no editor is on the page.
document.addEventListener("selectionchange", saveComposeRange);

function restoreComposeRange() {
  const editor = document.getElementById("compose-wysiwyg");
  if (!editor) return;
  editor.focus();
  if (composeSavedRange) {
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(composeSavedRange);
  }
}
function execCompose(cmd, val) {
  restoreComposeRange();
  document.execCommand(cmd, false, val ?? null);
  saveComposeRange();
  syncComposeEditor();
}

// initComposeEditor is idempotent: a data-wired flag on each element guards
// against being called more than once for the same DOM (a re-fired htmx swap
// would otherwise double-bind the toolbar → toggle commands cancel out and the
// link prompt shows twice).
function initComposeEditor() {
  const editor = document.getElementById("compose-wysiwyg");
  if (editor && !editor.dataset.wired) {
    editor.dataset.wired = "1";
    editor.addEventListener("input", syncComposeEditor);
    editor.addEventListener("keydown", (e) => {
      if (e.ctrlKey && e.key === "Enter") { e.preventDefault(); submitCompose(); }
    });
  }
  const toolbar = document.querySelector("[data-compose-toolbar]");
  if (toolbar && !toolbar.dataset.wired) {
    toolbar.dataset.wired = "1";
    // Pressing any toolbar BUTTON must not steal focus/selection from the
    // editor, so execCommand acts on the still-focused editor. Selects and the
    // color <input> are not buttons, so they keep their native behavior.
    toolbar.addEventListener("mousedown", (e) => {
      if (e.target.closest("button")) e.preventDefault();
    });
    toolbar.addEventListener("click", (e) => {
      const cmdBtn = e.target.closest("[data-cmd]");
      if (cmdBtn) { execCompose(cmdBtn.dataset.cmd); return; }
      const blockBtn = e.target.closest("[data-cmd-block]");
      if (blockBtn) { execCompose("formatBlock", blockBtn.dataset.cmdBlock); return; }
      if (e.target.closest("[data-cmd-unlink]")) { execCompose("unlink"); return; }
      if (e.target.closest("[data-cmd-link]")) {
        const url = prompt("Link URL:");
        if (url) execCompose("createLink", url);
      }
    });
    const font = toolbar.querySelector("[data-cmd-font]");
    if (font) font.addEventListener("change", () => { if (font.value) execCompose("fontName", font.value); font.selectedIndex = 0; });
    const size = toolbar.querySelector("[data-cmd-size]");
    if (size) size.addEventListener("change", () => { if (size.value) execCompose("fontSize", size.value); size.selectedIndex = 0; });
    const color = toolbar.querySelector("[data-cmd-color]");
    if (color) color.addEventListener("input", () => execCompose("foreColor", color.value));
  }
  // Markdown Write/Preview tabs.
  const tabs = document.querySelector("[data-md-tabs]");
  if (tabs && !tabs.dataset.wired) {
    tabs.dataset.wired = "1";
    const write = document.querySelector("[data-md-write]");
    const preview = document.querySelector("[data-md-preview]");
    tabs.addEventListener("click", (e) => {
      const t = e.target.closest("[data-md-tab]");
      if (!t) return;
      const showPreview = t.dataset.mdTab === "preview";
      write.classList.toggle("hidden", showPreview);
      preview.classList.toggle("hidden", !showPreview);
      tabs.querySelectorAll("[data-md-tab]").forEach((b) => b.classList.toggle("md-tab-active", b === t));
    });
  }
  initComposeSignature();
}

// --- signatures: auto-insert the identity's linked signature, swap on From
// change, and let the user skip it. All state is per-open (module vars reset in
// initComposeSignature); insertion is marked with a [data-signature] wrapper
// (WYSIWYG) or tracked verbatim (textarea) so it can be cleanly replaced. ---
let composeSigText = "";      // exact block inserted into a textarea body
let composeSigSkipped = false; // user unticked the Signature toggle

function composeSigData() {
  const el = document.getElementById("compose-sig-data");
  if (!el) return null;
  let sigs = {};
  try { sigs = JSON.parse(el.dataset.sigs || "{}") || {}; } catch (e) { sigs = {}; }
  return { el, sigs, kind: el.dataset.kind || "new", mode: el.dataset.mode || "plain" };
}

function sigVariantText(s, mode) {
  if (!s) return "";
  if (mode === "html") return s.html || "";
  if (mode === "markdown") return s.markdown || "";
  return s.plain || "";
}

// applyComposeSignature removes any current signature and, unless it doesn't
// apply (unlinked / first-message-only on a reply) or was skipped, inserts the
// variant for `addr`. Idempotent — safe to call on every From change.
function applyComposeSignature(addr) {
  const d = composeSigData();
  if (!d) return;
  const editor = document.getElementById("compose-wysiwyg");
  const body = document.querySelector('#compose-form textarea[name="body"]');
  if (editor) {
    editor.querySelectorAll("[data-signature]").forEach((n) => n.remove());
  } else if (body && composeSigText && body.value.includes(composeSigText)) {
    body.value = body.value.replace(composeSigText, "");
  }
  composeSigText = "";
  const s = d.sigs[(addr || "").toLowerCase().trim()];
  const isReply = d.kind === "reply" || d.kind === "reply_all";
  const applies = !!s && !(s.apply === "first" && isReply);
  updateSigToggle(applies);
  if (!applies || composeSigSkipped) { if (editor) syncComposeEditor(); return; }
  const variant = sigVariantText(s, d.mode);
  if (editor) {
    const div = document.createElement("div");
    div.setAttribute("data-signature", "");
    div.innerHTML = variant; // sanitized server-side in composeSignatures
    editor.appendChild(div);
    syncComposeEditor();
  } else if (body) {
    composeSigText = "\n\n-- \n" + variant;
    body.value += composeSigText;
  }
}

function updateSigToggle(applies) {
  const wrap = document.getElementById("compose-sig-toggle-wrap");
  if (!wrap) return;
  wrap.hidden = !applies;
  const cb = document.getElementById("compose-sig-toggle");
  if (cb) cb.checked = applies && !composeSigSkipped;
}

function onComposeSigToggle(cb) {
  composeSigSkipped = !cb.checked;
  const from = document.querySelector('#compose-form [name="from"]');
  applyComposeSignature(from ? from.value : (composeSigData()?.el.dataset.from || ""));
}
window.onComposeSigToggle = onComposeSigToggle;

// initComposeSignature wires the From selector and does the initial insert. The
// wired flag makes it run once per compose open.
function initComposeSignature() {
  const el = document.getElementById("compose-sig-data");
  if (!el || el.dataset.wired) return;
  el.dataset.wired = "1";
  composeSigText = "";
  composeSigSkipped = false;
  const fromSel = document.querySelector('#compose-form select[name="from"]');
  if (fromSel) fromSel.addEventListener("change", () => applyComposeSignature(fromSel.value));
  const from = (fromSel ? fromSel.value : "") || el.dataset.from || "";
  if (el.dataset.auto) {
    applyComposeSignature(from);
  } else {
    // Reopened draft / undo restore: body already carries its signature; don't
    // re-insert, but keep the toggle available (skipped so it isn't re-added).
    composeSigSkipped = true;
    applyComposeSignature(from);
  }
}

// Ensure the hidden body textarea is current before the form submits.
document.addEventListener("submit", (e) => {
  if (e.target && e.target.id === "compose-form") syncComposeEditor();
}, true);

// --- h/l: move focus between the three panes (accounts ↔ messages ↔ reading).
// A subtle inset ring (see [data-section-focus] in app.css) shows which pane
// holds focus. ---
function sectionEls() {
  return [document.querySelector("nav"), document.getElementById("message-list"), document.getElementById("reading-pane")];
}
let curSection = 1; // start on the message list
function focusSection(delta) {
  const secs = sectionEls();
  curSection = Math.max(0, Math.min(secs.length - 1, curSection + delta));
  const el = secs[curSection];
  if (!el) return true;
  secs.forEach((s) => s && s.removeAttribute("data-section-focus"));
  el.setAttribute("data-section-focus", "");
  if (el.id === "message-list") {
    const row = selectedRow() || listRows()[0];
    if (row) { selectRow(row); row.focus?.(); }
  } else if (el.id === "reading-pane") {
    (el.querySelector("#message-detail") || el).focus?.({ preventScroll: true });
  } else {
    el.querySelector("a, button")?.focus();
  }
  return true;
}

let goPending = false;
const keymap = {
  "h": () => focusSection(-1),
  "l": () => focusSection(1),
  "/": () => { const s = document.getElementById("search"); if (s) { s.focus(); return true; } },
  "?": () => toggleHelp(),
  "c": () => openCompose(""),
  "j": () => moveSelection(1),
  "k": () => moveSelection(-1),
  "o": () => openSelected(),
  "r": () => flagSelected("read"),
  "u": () => flagSelected("unread"),
  "s": () => { if (goPending) { goPending = false; starredSearch(); return true; } return flagSelected(selectedRow()?.querySelector('[hx-post*="unstar"]') ? "unstar" : "star"); },
  "e": () => moveSelected("archive", "Archived"),
  "d": () => { if (goPending) { goPending = false; window.location.href = "/drafts"; return true; } return moveSelected("delete", "Deleted"); },
  "#": () => moveSelected("delete", "Deleted"),
  "!": () => moveSelected("spam", "Marked as spam"),
  "R": () => replyCompose("reply"),
  "A": () => replyCompose("all"),
  "F": () => replyCompose("forward"),
  "g": () => { goPending = true; setTimeout(() => (goPending = false), 800); return true; },
  "i": () => { if (goPending) { goPending = false; jumpInbox(); return true; } return false; },
  "t": () => { if (goPending) { goPending = false; jumpSent(); return true; } return false; },
  "0": () => jumpUnified(),
};
// 1–9 jump to the Nth configured account's inbox.
for (let n = 1; n <= 9; n++) {
  keymap[String(n)] = () => jumpAccountInbox(n - 1);
}
function jumpUnified() {
  const el = document.querySelector("[data-unified]");
  if (el) { el.click(); return true; }
  window.location.href = "/";
  return true;
}
function jumpAccountInbox(i) {
  const el = document.querySelectorAll("[data-account-inbox]")[i];
  if (el) el.click();
  return true;
}
// g i: current account's inbox, or the unified view when already unified.
function jumpInbox() {
  const el = document.querySelector("[data-account-inbox]") || document.querySelector("[data-unified]");
  if (el) { el.click(); return; }
  window.location.href = "/";
}
// g t: jump to a Sent folder. NOTE: picks the first account's Sent folder
// (matches jumpInbox's single-account-first simplicity) — fine for the
// common single/primary-account case; per-pane "current account" tracking
// would need folder->account plumbing not otherwise needed anywhere.
function jumpSent() {
  document.querySelector("[data-sent]")?.click();
}

function toggleHelp() {
  const el = document.getElementById("help-overlay");
  if (el) el.hidden = !el.hidden;
  return true;
}
document.addEventListener("sm:help", toggleHelp);

function toggleAbout() {
  const el = document.getElementById("about-overlay");
  if (el) el.hidden = !el.hidden;
  return true;
}
document.addEventListener("sm:about", toggleAbout);

document.addEventListener("keydown", (e) => {
  // Ctrl+Enter sends from the compose body field (submit syncs the editor).
  if (e.ctrlKey && e.key === "Enter" && e.target?.matches?.('#compose-form textarea[name="body"]')) {
    submitCompose();
    e.preventDefault();
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const composeRoot = document.getElementById("compose-root");
  if (e.key === "Escape" && composeRoot && composeRoot.firstElementChild) {
    // Dismiss a stacked modal first (guard > schedule/attachment), else the
    // guard routes the close. Only one modal is ever open at a time.
    for (const id of ["compose-close-guard", "schedule-modal", "attach-reminder-modal"]) {
      const m = document.getElementById(id);
      if (m && !m.hidden) { m.hidden = true; e.preventDefault(); return; }
    }
    closeCompose();
    e.preventDefault();
    return;
  }
  const t = e.target;
  if (t instanceof HTMLElement && (t.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName))) {
    // Search box: Tab cycles scope, Escape clears and returns to the inbox.
    if (t.id === "search") {
      if (e.key === "Tab") { cycleScope(); e.preventDefault(); return; }
      if (e.key === "Escape") {
        t.value = "";
        document.getElementById("search-suggest")?.classList.add("hidden");
        t.blur();
        jumpInbox();
        return;
      }
    }
    if (e.key === "Escape") t.blur();
    return;
  }
  if (e.key === "Escape") {
    const about = document.getElementById("about-overlay");
    if (about && !about.hidden) { about.hidden = true; return; }
    const help = document.getElementById("help-overlay");
    if (help && !help.hidden) { help.hidden = true; return; }
    if (window.matchMedia("(max-width: 767px)").matches) closeReadingPane();
    return;
  }
  if (e.key === "Enter") { if (openSelected()) e.preventDefault(); return; }
  const fn = keymap[e.key];
  if (fn && fn() !== false) e.preventDefault();
});

// --- Outgoing page: live countdown for "Sending soon" rows. Ticks every second
// over whatever .out-countdown[data-send-at] elements are in the DOM right now,
// so htmx list-poll swaps (which replace the rows) are picked up automatically
// with no rebinding. No-op on pages that have none. ---
(function () {
  function tick() {
    const els = document.querySelectorAll(".out-countdown[data-send-at]");
    if (!els.length) return;
    const now = Date.now();
    els.forEach((el) => {
      const t = new Date(el.dataset.sendAt).getTime();
      if (isNaN(t)) return;
      const s = Math.round((t - now) / 1000);
      el.textContent = s > 0 ? `Sending in ${s}s…` : "Sending…";
    });
  }
  setInterval(tick, 1000);
  document.addEventListener("DOMContentLoaded", tick);
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.target && e.target.id === "outgoing-list") tick();
  });
})();

// --- preserve #message-list scroll across htmx history navigation. htmx
// restores the body HTML on Back but not an inner overflow container's
// scrollTop, so returning to a search/inbox list would jump to the top. Stash
// the list's scroll keyed by URL when htmx snapshots the page, reapply when it
// restores. Fixes both list<->detail flows (inbox threads and search results).
(function () {
  const pos = Object.create(null);
  const list = () => document.getElementById("message-list");
  const save = () => { const l = list(); if (l) pos[location.href] = l.scrollTop; };
  document.addEventListener("htmx:beforeHistorySave", save);
  document.addEventListener("htmx:historyRestore", () => {
    const y = pos[location.href];
    if (y == null) return;
    requestAnimationFrame(() => { const l = list(); if (l) l.scrollTop = y; });
  });
})();

// --- Hover-prefetch message bodies. Opening a message is two requests: the
// detail shell (fast — sqlite) and the body iframe, which on a message whose
// body isn't cached yet goes out over IMAP and can take seconds. The body URL
// has no side effects (mark-as-read lives on /messages/{id}, not on
// /messages/{id}/body), so warming it on hover is safe. Fetching it fills the
// server's body cache and, via the service worker, the browser's — so the
// click that follows is a local read.
//
// The debounce is the point: firing per row would queue an IMAP fetch for
// every row the cursor sweeps over, onto the single serialized per-account
// command channel that is already what makes a cold body slow. 150ms is under
// the time it takes to move-and-click, so a deliberate hover still wins.
//
// NOTE: mouse only. Add a focus trigger if j/k list nav feels slow.
(function () {
  const WARM_AFTER_MS = 150;
  const warmed = new Set();
  let timer;

  function warm(url) {
    if (warmed.has(url)) return;
    warmed.add(url);
    // Read the response to completion: that's what lets the service worker
    // finish caching its copy, and it's a few kB.
    fetch(url, { credentials: "same-origin" })
      .then((r) => {
        if (!r.ok) throw new Error(r.status);
        return r.text();
      })
      .catch(() => warmed.delete(url)); // let a real click retry it
  }

  document.addEventListener("mouseover", (e) => {
    clearTimeout(timer);
    const row = e.target.closest && e.target.closest("[data-prefetch]");
    if (!row) return;
    const url = row.dataset.prefetch;
    timer = setTimeout(() => warm(url), WARM_AFTER_MS);
  });
})();
