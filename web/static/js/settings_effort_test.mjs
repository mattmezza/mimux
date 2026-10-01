// SPDX-License-Identifier: AGPL-3.0-only
//
// The reasoning-effort card logic lives in an inline <script> inside
// pages/settings.html. It is Alpine-free and DOM-driven, so it cannot be
// imported: the test extracts the IIFE from the template and runs it against a
// minimal DOM stub, then drives it the way a user would (click "Check model
// support", pick an effort, save) and asserts on what the form would submit.
//
// Two defects it pins down:
//   1. the hidden mirror used to back a disabled select was appended on every
//      check and only one was removed on re-enable, so a stale mirror shadowed
//      the selector on save (PostFormValue reads the first occurrence);
//   2. a failed/unknown lookup left a previously narrowed ladder in place even
//      though the status line claimed the full list was kept.
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const html = await readFile(new URL("../../templates/pages/settings.html", import.meta.url), "utf8");
const start = html.indexOf('// "Check model support"');
const end = html.indexOf("})();", start);
assert.ok(start > 0 && end > start, "effort card IIFE not found in settings.html");
const script = html.slice(start, end + "})();".length);

const LADDER = ["", "max", "xhigh", "high", "medium", "low", "minimal", "none"];
const LABEL = {
  "": "Default (model's own)", max: "Max", xhigh: "Extra high", high: "High",
  medium: "Medium", low: "Low", minimal: "Minimal", none: "Off — never reason",
};

// --- minimal DOM -----------------------------------------------------------

class Node {
  constructor(tag = "div") {
    this.tag = tag;
    this.sel = [];
    this.attrs = {};
    this.children = [];
    this.parentNode = null;
    this.textContent = "";
  }
  setAttribute(name, value) {
    this.attrs[name] = value;
    if (name.startsWith("data-")) this.sel.push("[" + name + "]");
  }
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
  insertBefore(child, ref) {
    child.parentNode = this;
    const i = this.children.indexOf(ref);
    this.children.splice(i < 0 ? this.children.length : i, 0, child);
    return child;
  }
  remove() {
    if (this.parentNode) this.parentNode.children.splice(this.parentNode.children.indexOf(this), 1);
    this.parentNode = null;
  }
  matches(sel) { return this.sel.includes(sel); }
  descendants() {
    const out = [];
    for (const c of this.children) {
      out.push(c);
      if (typeof c.descendants === "function") out.push(...c.descendants());
    }
    return out;
  }
  querySelectorAll(sel) { return this.descendants().filter((n) => n.matches(sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

class Select extends Node {
  constructor(name, values = LADDER) {
    super("select");
    this.name = name;
    this.value = "";
    this.disabled = false;
    this.sel = ["[data-effort-select]"];
    this._options = [];
    values.forEach((v) => this.add(new Option(LABEL[v] || v, v)));
  }
  get options() { return this._options; }
  add(option) { this._options.push(option); this.appendChild(option); }
  get innerHTML() { return ""; }
  set innerHTML(v) { if (v === "") { this._options = []; this.children = []; } }
}

class Option extends Node {
  constructor(text, value) { super("option"); this.textContent = text; this.value = value; }
}

function mkInput(selectors, name = "") {
  const el = new Node("input");
  el.sel = selectors;
  el.name = name;
  el.value = "";
  return el;
}

// A card mirrors the real markup layout closely enough: the select's
// parentNode holds the model input, the select, the status and the button, so
// insertBefore lands a mirror ahead of the select exactly as it does in the
// label in settings.html.
function makeCard(selectName, { global = false } = {}) {
  const card = new Node("div");
  card.sel = ["[data-effort-card]"];
  const model = mkInput(global ? ["[data-effort-model]", "[data-effort-global]"] : ["[data-effort-model]"], selectName.replace(/_reasoning$/, "_model"));
  const select = new Select(selectName);
  const status = new Node("span");
  status.sel = ["[data-effort-status]"];
  const button = new Node("button");
  button.sel = ["[data-effort-check]"];
  button.closest = (sel) => (sel === "[data-effort-card]" ? card : sel === "[data-effort-check]" ? button : null);
  card.appendChild(model);
  card.appendChild(select);
  card.appendChild(status);
  card.appendChild(button);
  return { card, model, select, status, button };
}

// --- harness ---------------------------------------------------------------

function harness(fetchImpl) {
  const root = new Node("body");
  const globalCard = makeCard("ai_reasoning", { global: true });
  const refineCard = makeCard("ai_refine_reasoning");
  // The global card comes first, so its select is the sample the ORDER/LABELS
  // are read from — same as the real page.
  root.appendChild(globalCard.card);
  root.appendChild(refineCard.card);

  const handlers = {};
  const document = {
    querySelector: (sel) => root.querySelector(sel),
    querySelectorAll: (sel) => root.querySelectorAll(sel),
    createElement: (tag) => new Node(tag),
    addEventListener: (type, fn) => { handlers[type] = fn; },
  };
  const sandbox = { document, fetch: fetchImpl, Option, encodeURIComponent };
  vm.runInNewContext(script, vm.createContext(sandbox));

  const tick = () => new Promise((r) => setImmediate(r));
  return {
    globalCard, refineCard, root,
    async edit(card, value) { card.model.value = value; handlers.input({ target: card.model }); await tick(); },
    async click(card) { handlers.click({ target: card.button }); await tick(); await tick(); await tick(); },
    // What the browser would submit for a field: live, enabled controls in
    // document order — PostFormValue takes the first.
    submitted(name) {
      return root.descendants()
        .filter((n) => n.name === name && !n.disabled)
        .map((n) => n.value);
    },
    mirrorCount(select) {
      return select.parentNode.querySelectorAll("[data-effort-mirror]").length;
    },
    optionValues(select) { return select.options.map((o) => o.value); },
  };
}

const response = (body) => ({ ok: true, json: async () => body });
const capable = { found: true, supports_reasoning: true, supported_efforts: ["max", "xhigh", "high", "medium", "low"], mandatory: false, default_enabled: true, default_effort: "medium" };
const noReasoning = { found: true, supports_reasoning: false };

test("a card disabled by a check keeps exactly one mirror, and can still save a value", async () => {
  let next = noReasoning;
  const h = harness(async () => response(next));
  h.refineCard.model.value = "typesafe/jev-router";

  await h.click(h.refineCard);
  assert.equal(h.refineCard.select.disabled, true);
  assert.equal(h.mirrorCount(h.refineCard.select), 1);

  // Re-checking must not stack mirrors.
  await h.click(h.refineCard);
  await h.click(h.refineCard);
  assert.equal(h.mirrorCount(h.refineCard.select), 1, "one mirror per select, however many checks ran");

  // A capable model re-enables the select and clears the mirror, so the value
  // the user picks is the one that gets submitted.
  next = capable;
  h.refineCard.model.value = "anthropic/claude-sonnet-5.5";
  await h.click(h.refineCard);
  assert.equal(h.refineCard.select.disabled, false);
  assert.equal(h.mirrorCount(h.refineCard.select), 0);

  h.refineCard.select.value = "high";
  const values = h.submitted("ai_refine_reasoning");
  assert.equal(values[0], "high", "the chosen effort, not a stale mirror, is submitted first");
  assert.deepEqual(values, ["high"]);
});

test("an unknown model restores the full ladder after a narrow", async () => {
  let next = capable;
  const h = harness(async () => response(next));
  h.refineCard.model.value = "anthropic/claude-sonnet-5.5";
  await h.click(h.refineCard);
  assert.ok(!h.optionValues(h.refineCard.select).includes("none"), "narrowed first");

  next = { found: false };
  h.refineCard.model.value = "typo/model";
  await h.click(h.refineCard);
  assert.match(h.refineCard.status.textContent, /Unknown model/);
  assert.deepEqual(h.optionValues(h.refineCard.select), LADDER, "full list, not the previous model's ladder");
});

test("a lookup failure restores the full ladder after a narrow", async () => {
  let next = capable;
  const fetchImpl = async () => {
    if (next instanceof Error) throw next;
    return response(next);
  };
  const h = harness(fetchImpl);
  h.refineCard.model.value = "anthropic/claude-sonnet-5.5";
  await h.click(h.refineCard);
  assert.ok(!h.optionValues(h.refineCard.select).includes("minimal"), "narrowed first");

  next = new Error("network down");
  await h.click(h.refineCard);
  assert.match(h.refineCard.status.textContent, /keeping the full list/);
  assert.deepEqual(h.optionValues(h.refineCard.select), LADDER);
});


test("editing a model re-enables its selector and removes the hidden mirror", async () => {
  const h = harness(async () => response(noReasoning));
  h.refineCard.model.value = "plain/model";
  h.refineCard.select.value = "high";
  await h.click(h.refineCard);
  await h.edit(h.refineCard, "reasoning/model");
  assert.equal(h.refineCard.select.disabled, false);
  assert.equal(h.mirrorCount(h.refineCard.select), 0);
  assert.deepEqual(h.optionValues(h.refineCard.select), LADDER);
  assert.deepEqual(h.submitted("ai_refine_reasoning"), ["high"]);
  assert.equal(h.refineCard.status.textContent, "");
});

test("editing the global model resets inherited cards but preserves pinned cards", async () => {
  const h = harness(async () => response(capable));
  h.globalCard.model.value = "first/model";
  await h.click(h.refineCard);
  await h.edit(h.globalCard, "second/model");
  assert.deepEqual(h.optionValues(h.refineCard.select), LADDER);
  h.refineCard.model.value = "pinned/model";
  await h.click(h.refineCard);
  const pinned = h.optionValues(h.refineCard.select);
  await h.edit(h.globalCard, "third/model");
  assert.deepEqual(h.optionValues(h.refineCard.select), pinned);
});

test("a stale lookup cannot disable a selector after a model edit", async () => {
  let resolve;
  const h = harness(() => new Promise((r) => { resolve = r; }));
  h.refineCard.model.value = "plain/model";
  await h.click(h.refineCard);
  await h.edit(h.refineCard, "reasoning/model");
  resolve(response(noReasoning));
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.equal(h.refineCard.select.disabled, false);
  assert.deepEqual(h.optionValues(h.refineCard.select), LADDER);
  assert.equal(h.refineCard.status.textContent, "");
});

test("an older lookup failure cannot overwrite a newer successful check", async () => {
  let reject;
  let calls = 0;
  const h = harness(() => ++calls === 1 ? new Promise((_, r) => { reject = r; }) : Promise.resolve(response(capable)));
  h.refineCard.model.value = "reasoning/model";
  await h.click(h.refineCard);
  await h.click(h.refineCard);
  const options = h.optionValues(h.refineCard.select);
  const status = h.refineCard.status.textContent;
  reject(new Error("old failure"));
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(h.optionValues(h.refineCard.select), options);
  assert.equal(h.refineCard.status.textContent, status);
});
