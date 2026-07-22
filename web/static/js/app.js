// SM app glue: htmx CSRF, service worker, SSE, toasts, keybindings.

// Send the CSRF token on every htmx mutation.
document.addEventListener("htmx:configRequest", (e) => {
  const meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) e.detail.headers["X-CSRF-Token"] = meta.content;
});

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js");
}

// --- Alpine UI state (sidebar collapse, per-account tree open/closed, quick
// filter) persisted in localStorage so it survives reloads. Referenced by
// x-data="smUI()" on the page roots. ---
window.smUI = function () {
  const KEY = "sm.ui";
  let saved = {};
  try { saved = JSON.parse(localStorage.getItem(KEY)) || {}; } catch (_) {}
  return {
    sidebarOpen: false, // mobile drawer
    collapsed: !!saved.collapsed, // desktop sidebar hidden
    filter: saved.filter || "", // "" | "unread" | "starred"
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
    setFilter(f) { this.filter = this.filter === f ? "" : f; this._save(); },
  };
};

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
  es.addEventListener("new-mail", () => document.body.dispatchEvent(new Event("sm:refresh")));
  es.addEventListener("sync-status", () => document.body.dispatchEvent(new Event("sm:sync")));
  es.addEventListener("toast", (e) => toast(e.data));
  es.addEventListener("search-started", (e) => searchAcctState(JSON.parse(e.data).account, "start"));
  es.addEventListener("search-results", (e) => appendServerResults(JSON.parse(e.data)));
  es.addEventListener("search-done", (e) => { const d = JSON.parse(e.data); searchAcctState(d.account, "done", d.count); });
})();

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
document.getElementById("scope-pill")?.addEventListener("click", cycleScope);

// Keep the search form's folder context in sync with the visible list.
function syncSearchFolder() {
  const list = document.getElementById("message-list");
  const f = document.getElementById("search-folder");
  if (list && f && list.dataset.folder != null) f.value = list.dataset.folder;
}
document.addEventListener("htmx:afterSwap", (e) => {
  if (e.target && e.target.id === "message-list") syncSearchFolder();
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
  const main = () => document.getElementById("main-content");
  function apply(px) { main()?.style.setProperty("--list-w", px + "px"); }
  const saved = parseInt(localStorage.getItem(KEY) || "", 10);
  if (saved > 0) apply(saved);
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
      last = Math.max(240, Math.min(ev.clientX - left, Math.min(700, m.clientWidth - 320)));
      apply(last);
    };
    const up = () => {
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
      handle.classList.remove("dragging");
      document.body.style.userSelect = "";
      document.body.style.cursor = "";
      if (last > 0) localStorage.setItem(KEY, String(Math.round(last)));
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  });
})();

// --- translate: pull the rendered text out of a message-body iframe and POST
// it to /translate, dropping the result into resultEl. Same-origin iframe, so
// reading its text is allowed. ---
window.translateFrame = function (frame, resultEl) {
  if (!frame || !resultEl || !window.htmx) return;
  let text = "";
  try { text = frame.contentDocument?.body?.innerText || ""; } catch (_) {}
  if (!text.trim()) {
    resultEl.innerHTML = '<p class="text-xs text-zinc-500 px-4 py-2">Let the message finish loading, then translate.</p>';
    return;
  }
  resultEl.innerHTML = '<p class="text-xs text-zinc-500 px-4 py-2">Translating…</p>';
  htmx.ajax("POST", "/translate", { values: { text: text.slice(0, 5000) }, target: resultEl, swap: "innerHTML" });
};

// --- per-message dark/light toggle: email bodies render on a light background
// (many hardcode black text); this flips just that iframe to dark on demand. ---
window.toggleBodyTheme = function (frame) {
  const doc = frame && frame.contentDocument;
  if (!doc) return;
  doc.documentElement.classList.toggle("sm-dark");
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
function closeCompose() {
  const root = document.getElementById("compose-root");
  if (root) root.innerHTML = "";
  if (preComposeFocus && document.contains(preComposeFocus)) preComposeFocus.focus();
  preComposeFocus = null;
}
window.closeCompose = closeCompose;

// Focus management: compose opens onto the "To" field; a message opening
// moves focus into the reading pane so keyboard/AT users land somewhere sane.
document.addEventListener("htmx:afterSwap", (e) => {
  if (e.target && e.target.id === "compose-root" && e.target.firstElementChild) {
    e.target.querySelector('[name="to"]')?.focus();
  }
  if (e.target && e.target.id === "reading-pane") {
    e.target.querySelector("#message-detail")?.focus({ preventScroll: true });
    scheduleMarkRead(e.target.querySelector("#message-detail"));
  }
});

// Mark-as-read-after-N-seconds: when the server defers read-marking (a delay is
// configured), the opened message carries data-mark-read-pending + a delay. We
// POST /read once it's been on screen that long, cancelling if the user leaves
// the message first.
let markReadTimer = null;
function scheduleMarkRead(detail) {
  if (markReadTimer) { clearTimeout(markReadTimer); markReadTimer = null; }
  if (!detail || !detail.dataset.markReadPending) return;
  const delay = parseInt(detail.dataset.markReadDelay || "0", 10);
  const id = detail.dataset.messageId;
  if (!id || !(delay > 0) || !window.htmx) return;
  markReadTimer = setTimeout(() => {
    // Only fire if this message is still the one on screen.
    const cur = document.querySelector("#message-detail");
    if (cur && cur.dataset.messageId === id) {
      htmx.ajax("POST", `/messages/${id}/read`, { swap: "none" });
    }
  }, delay * 1000);
}

// Insert an AI-drafted textarea's contents into the compose body (shared by
// the ai_compose_result and ai_reply_result partials, which both render a
// textarea followed by a ".ai-insert-draft" button).
document.addEventListener("click", (e) => {
  const btn = e.target.closest(".ai-insert-draft");
  if (!btn) return;
  const draft = btn.previousElementSibling;
  const body = document.querySelector('#compose-form textarea[name="body"]');
  if (body && draft && "value" in draft) {
    body.value = draft.value;
    body.dispatchEvent(new Event("input", { bubbles: true }));
  }
});

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
// g t: jump to a Sent folder. ponytail: picks the first account's Sent folder
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

document.addEventListener("keydown", (e) => {
  // Ctrl+Enter sends from the compose body field (draft is already autosaved).
  if (e.ctrlKey && e.key === "Enter" && e.target?.matches?.('#compose-form textarea[name="body"]')) {
    e.target.closest("form")?.requestSubmit();
    e.preventDefault();
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const composeRoot = document.getElementById("compose-root");
  if (e.key === "Escape" && composeRoot && composeRoot.firstElementChild) {
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
    const help = document.getElementById("help-overlay");
    if (help && !help.hidden) { help.hidden = true; return; }
    if (window.matchMedia("(max-width: 767px)").matches) closeReadingPane();
    return;
  }
  if (e.key === "Enter") { if (openSelected()) e.preventDefault(); return; }
  const fn = keymap[e.key];
  if (fn && fn() !== false) e.preventDefault();
});
