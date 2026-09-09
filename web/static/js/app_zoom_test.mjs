// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const app = await readFile(new URL("./app.js", import.meta.url), "utf8");
const start = app.indexOf("function bodyZoom(");
const end = app.indexOf("// Re-fit on rotation", start);

test("touch zoom keeps the element under the pinch midpoint", () => {
  const listeners = {};
  const scrolls = [];
  const frame = { dataset: { zoom: "1", fitZoom: "1" }, clientWidth: 360 };
  const anchor = {
    getBoundingClientRect() {
      const zoom = Number(frame.dataset.zoom);
      return zoom === 1
        ? { left: 100, top: 200, width: 100, height: 100 }
        : { left: 50, top: 100, width: 200, height: 200 };
    },
  };
  const body = {
    style: {},
    getBoundingClientRect: () => ({ height: 1000 * Number(frame.dataset.zoom) }),
  };
  const doc = {
    body,
    documentElement: { style: {}, scrollWidth: 360 },
    elementFromPoint: () => anchor,
    addEventListener: (name, fn) => { listeners[name] = fn; },
    defaultView: { scrollBy: (x, y) => scrolls.push([x, y]) },
  };
  frame.contentDocument = doc;
  const context = { window: {}, document: {}, console };
  vm.runInNewContext(`${app.slice(start, end)}; globalThis.setup = setupBodyZoom;`, context);
  context.setup(frame);

  const touches = (x1, y1, x2, y2) => [
    { clientX: x1, clientY: y1 },
    { clientX: x2, clientY: y2 },
  ];
  listeners.touchstart({ touches: touches(100, 200, 200, 300) });
  let prevented = false;
  listeners.touchmove({
    touches: touches(50, 150, 250, 350),
    preventDefault: () => { prevented = true; },
  });

  assert.equal(Number(frame.dataset.zoom), 2);
  assert.equal(prevented, true);
  assert.deepEqual(scrolls, [[0, -50]]);
});
