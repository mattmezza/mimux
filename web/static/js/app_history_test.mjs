// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const app = await readFile(new URL("./app.js", import.meta.url), "utf8");
const start = app.indexOf('// htmx snapshots the current page');
const end = app.indexOf('// The reading pane\'s header', start);

function restore(search, paneContent) {
  let restored;
  let closes = 0;
  const pane = { querySelector: (selector) => selector === "#reading-pane-empty" && paneContent === "empty" ? {} : null };
  vm.runInNewContext(app.slice(start, end), {
    document: {
      addEventListener: (name, fn) => { if (name === "htmx:historyRestore") restored = fn; },
      getElementById: (id) => id === "reading-pane" ? pane : null,
    },
    location: { search },
    URLSearchParams,
    closeReadingPane: () => { closes++; },
  });
  restored();
  return closes;
}

test("Back to the list clears a reading skeleton restored from htmx history", () => {
  assert.equal(restore("?f=1", "skeleton"), 1);
  assert.equal(restore("?q=hello", "message"), 1);
  assert.equal(restore("?f=1", "empty"), 0);
});

test("restoring a detail URL keeps its reading pane", () => {
  assert.equal(restore("?t=42&src=inbox", "message"), 0);
  assert.equal(restore("?q=hello&m=42", "message"), 0);
});
