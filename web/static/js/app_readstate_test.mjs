// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const app = await readFile(new URL("./app.js", import.meta.url), "utf8");
const start = app.indexOf("function rowsFor(");
const end = app.indexOf("// Flip one thread-detail", start);

function classes(initial = []) {
  const values = new Set(initial);
  return {
    add: (...names) => names.forEach((name) => values.add(name)),
    remove: (...names) => names.forEach((name) => values.delete(name)),
    contains: (name) => values.has(name),
  };
}

function messageRow(id, { unread = true, sub = false, thread = false } = {}) {
  const attrs = new Set(unread ? ["data-unread"] : []);
  if (sub) attrs.add("data-mid");
  const toggle = thread ? {
    firstElementChild: { classList: classes(["rotate-90"]) },
    setAttribute(name, value) { this[name] = value; },
  } : null;
  const row = {
    id,
    classList: classes(),
    hasAttribute: (name) => attrs.has(name),
    setAttribute: (name) => attrs.add(name),
    removeAttribute: (name) => attrs.delete(name),
    querySelector(selector) {
      if (selector === ".thread-toggle") return toggle;
      return null;
    },
    closest: () => null,
  };
  return row;
}

function harness() {
  const parent = messageRow("msg-2", { thread: true });
  const first = messageRow("msg-s1", { sub: true });
  const latest = messageRow("msg-s2", { sub: true });
  const box = {
    id: "sub-2",
    classList: classes(),
    querySelector: (selector) => selector === "li[data-mid][data-unread]"
      ? [first, latest].find((row) => row.hasAttribute("data-unread")) || null : null,
    querySelectorAll: (selector) => selector === "li[data-mid]" ? [first, latest] : [],
  };
  first.closest = latest.closest = () => box;
  const nodes = { "msg-2": parent, "msg-s1": first, "msg-s2": latest, "sub-2": box };
  const context = {
    window: {},
    document: { getElementById: (id) => nodes[id] || null },
    activeFilter: () => "unread",
    setTimeout: (fn) => fn(),
  };
  vm.runInNewContext(`${app.slice(start, end)}; globalThis.read = markRowRead; globalThis.unread = markRowUnread;`, context);
  return { context, parent, first, latest, box };
}

test("reading the latest expanded message keeps an unread thread expanded", () => {
  const h = harness();
  h.context.read("2");
  assert.equal(h.latest.hasAttribute("data-unread"), false);
  assert.equal(h.first.hasAttribute("data-unread"), true);
  assert.equal(h.parent.hasAttribute("data-unread"), true);
  assert.equal(h.box.classList.contains("hidden"), false);
});

test("a whole-thread change updates loaded children and collapses the thread", () => {
  const h = harness();
  h.context.read("2", true);
  assert.equal(h.first.hasAttribute("data-unread"), false);
  assert.equal(h.latest.hasAttribute("data-unread"), false);
  assert.equal(h.parent.hasAttribute("data-unread"), false);
  assert.equal(h.box.classList.contains("hidden"), true);
});
