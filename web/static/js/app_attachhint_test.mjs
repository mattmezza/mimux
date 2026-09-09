// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const app = await readFile(new URL("./app.js", import.meta.url), "utf8");
const start = app.indexOf("const attachKeywords");
const end = app.indexOf("// Handles the 204", start);
const context = { document: { addEventListener() {} }, window: {} };
vm.runInNewContext(`${app.slice(start, end)}; globalThis.needsAttachmentReminder = needsAttachmentReminder;`, context);

function form(subject, body, attachments = [], nodes = {}) {
  return {
    querySelector(selector) {
      if (selector === 'input[type=file][name="attachments"]') return { files: attachments };
      if (selector === '[name="subject"]') return { value: subject };
      if (selector === 'textarea[name="body"]') return { value: body };
      return nodes[selector] || null;
    },
    querySelectorAll(selector) { return nodes[selector] || []; },
  };
}

test("attachment reminder ignores quoted replies and forwards", () => {
  assert.equal(context.needsAttachmentReminder(form("", "\n\nOn Mon, 20 Jul 2026 10:30, Alice wrote:\n> Please find the invoice attached.")), false);
  assert.equal(context.needsAttachmentReminder(form("", "\n\n---------- Forwarded message ----------\n> Please find the invoice attached.")), false);
  assert.equal(context.needsAttachmentReminder(form("", "<p><br></p><blockquote>Please find the invoice attached.</blockquote>")), false);
});

test("attachment reminder retains sender text and the subject", () => {
  assert.equal(context.needsAttachmentReminder(form("", "I attached the invoice.\n\nOn Mon, Alice wrote:\n> Please find the invoice attached.")), true);
  assert.equal(context.needsAttachmentReminder(form("attached report", "\n\nOn Mon, Alice wrote:\n> Please find the invoice attached.")), true);
  assert.equal(context.needsAttachmentReminder(form("", "On Monday, I wrote: attached files are ready")), true);
});

const fixtures = JSON.parse(await readFile(new URL('../../../internal/mail/testdata/attachment_hint_quotes.json', import.meta.url), 'utf8'));
for (const {name, text, want} of fixtures) {
  test(`quote boundaries: ${name}`, () => {
    assert.equal(context.needsAttachmentReminder(form('', text)), want);
  });
}

test('existing uploads and selected forward attachments suppress the reminder', () => {
  assert.equal(context.needsAttachmentReminder(form('', 'I attached it.', [{}])), false);
  for (const nodes of [
    {'#compose-attachments [data-attachment]': {}},
    {'input[name="forward_attachment"]:checked': {}},
    {'[name="forward_eml_id"]': {value:'42'}},
  ]) assert.equal(context.needsAttachmentReminder(form('', 'I attached it.', [], nodes)), false);
});
