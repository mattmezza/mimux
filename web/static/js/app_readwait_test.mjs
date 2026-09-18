// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const app = await readFile(new URL("./app.js", import.meta.url), "utf8");
const start = app.indexOf("const READING_WAIT_MS");
const end = app.indexOf("// The active quick filter", start);

// The waiting hint is deliberately not a spinner state: it exists because a
// reading-pane request can sit queued behind a sync sweep, and the skeleton has
// to stop claiming the message is about to appear.
//
// Each request gets its own identity and its own hint element, the way htmx's
// per-request detail carries both (detail.xhr, detail.target): the pane replaces
// its skeleton on every request, so the hint a request armed belongs to markup
// the next request has already thrown away.
function harness() {
  const timers = new Map();
  let nextId = 1;
  const context = {
    window: {},
    document: {},
    setTimeout: (fn, ms) => {
      const id = nextId++;
      timers.set(id, { fn, ms });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
  };
  vm.runInNewContext(`${app.slice(start, end)}; globalThis.arm = armReadingWait; globalThis.clear = clearReadingWait;`, context);
  const request = () => {
    const hint = { hidden: true };
    return {
      xhr: {},
      hint,
      scope: { querySelector: (selector) => (selector === "[data-reading-waiting]" ? hint : null) },
    };
  };
  const fire = () => [...timers.values()].forEach((t) => t.fn());
  return { context, request, timers, fire };
}

test("the skeleton says it is waiting only once the request is slow", () => {
  const h = harness();
  const r = h.request();
  h.context.arm(r.scope, r.xhr);
  assert.equal(r.hint.hidden, true, "the hint must not show while the request is still quick");
  const ms = [...h.timers.values()][0].ms;
  assert.ok(ms > 1000 && ms <= 5000, `the hint waits ${ms}ms: too eager or not soon enough`);
  h.fire();
  assert.equal(r.hint.hidden, false);
});

test("a second request cannot leave the previous timer armed", () => {
  const h = harness();
  const r = h.request();
  h.context.arm(r.scope, r.xhr);
  h.context.arm(r.scope, r.xhr);
  assert.equal(h.timers.size, 1, "arming twice left two timers: a stale one could reveal the hint on the next pane");
});

test("clearing on response leaves nothing to fire", () => {
  const h = harness();
  const r = h.request();
  h.context.arm(r.scope, r.xhr);
  h.context.clear(r.xhr);
  assert.equal(h.timers.size, 0);
  h.fire();
  assert.equal(r.hint.hidden, true, "a cleared timer still revealed the hint");
});

test("an earlier request's response cannot explain away a later one's wait", () => {
  const h = harness();
  const a = h.request();
  const b = h.request();
  h.context.arm(a.scope, a.xhr);
  h.context.arm(b.scope, b.xhr);
  h.context.clear(a.xhr); // A answered; B is still queued behind the sync.
  assert.equal(h.timers.size, 1, "A's response cleared B's timer: the slow pane drops back to an unexplained skeleton");
  h.fire();
  assert.equal(b.hint.hidden, false, "the request that is still waiting never said so");
  assert.equal(a.hint.hidden, true, "a request that answered revealed its own stale hint");
});

test("a pane without the hint is not an error", () => {
  const h = harness();
  h.context.arm({ querySelector: () => null }, {});
  assert.equal(h.timers.size, 0);
  h.context.arm(undefined, {});
  assert.equal(h.timers.size, 0);
  h.context.clear({});
  assert.equal(h.timers.size, 0);
});
