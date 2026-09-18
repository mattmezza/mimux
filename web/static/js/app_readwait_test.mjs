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
function harness() {
  const timers = new Map();
  let nextId = 1;
  const hint = { hidden: true };
  const scope = { querySelector: (selector) => (selector === "[data-reading-waiting]" ? hint : null) };
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
  const fire = () => [...timers.values()].forEach((t) => t.fn());
  return { context, hint, scope, timers, fire };
}

test("the skeleton says it is waiting only once the request is slow", () => {
  const h = harness();
  h.context.arm(h.scope);
  assert.equal(h.hint.hidden, true, "the hint must not show while the request is still quick");
  const ms = [...h.timers.values()][0].ms;
  assert.ok(ms > 1000 && ms <= 5000, `the hint waits ${ms}ms: too eager or not soon enough`);
  h.fire();
  assert.equal(h.hint.hidden, false);
});

test("a second request cannot leave the previous timer armed", () => {
  const h = harness();
  h.context.arm(h.scope);
  h.context.arm(h.scope);
  assert.equal(h.timers.size, 1, "arming twice left two timers: a stale one could reveal the hint on the next pane");
});

test("clearing on response leaves nothing to fire", () => {
  const h = harness();
  h.context.arm(h.scope);
  h.context.clear();
  assert.equal(h.timers.size, 0);
  h.fire();
  assert.equal(h.hint.hidden, true, "a cleared timer still revealed the hint");
});

test("a pane without the hint is not an error", () => {
  const h = harness();
  h.context.arm({ querySelector: () => null });
  assert.equal(h.timers.size, 0);
  h.context.arm(undefined);
  assert.equal(h.timers.size, 0);
});
