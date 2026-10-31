// SPDX-License-Identifier: AGPL-3.0-only
import assert from 'node:assert/strict';
import test from 'node:test';
import vm from 'node:vm';
import { readFile } from 'node:fs/promises';

const app = await readFile(new URL('./app.js', import.meta.url), 'utf8');
const source = app.slice(app.indexOf('// Keep local intent'), app.indexOf('// Rows remain in the DOM'));

function deferred() {
  let resolve, reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}
function harness(thread = false) {
  const handlers = new Map(), calls = [], updates = [], toasts = [];
  const parent = { id: 'msg-1', dataset: { members: '1 2', unreadMembers: '1 2' } };
  const rows = new Map(['1', '2', '3'].map(id => [id, { hasAttribute: () => true }]));
  const context = {
    Map, WeakMap, Set, Promise,
    document: {
      addEventListener(name, fn) { handlers.set(name, fn); },
      querySelectorAll() { return thread ? [parent] : []; },
      getElementById() { return null; },
      body: { classList: { remove() {} }, dispatchEvent() {} },
    },
    window: {}, activeFilter: () => 'unread',
    rowsFor: id => rows.has(id) ? [rows.get(id)] : [],
    postMarkRead(id, read, wholeThread) {
      const done = deferred(); calls.push({ id, read, wholeThread, ...done }); return done.promise;
    },
    toast: (message, opts) => toasts.push({ message, opts }),
    refreshUnreadTitle() {}, mimuxSyncing: false, smAwaitList: false,
  };
  const sandbox = { ...context, record: (id, read) => updates.push([id, read]) };
  vm.runInNewContext(source + `
    applyReadState = record;
    updateReadEmptyState = () => {};
    globalThis.api = {markRead, readChanges, readQueues};`, sandbox);
  return { ...sandbox.api, calls, updates, toasts, handlers, parent };
}
const tick = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };

test('different messages update immediately and persist concurrently', async () => {
  const h = harness();
  const a = h.markRead('1', true, false), b = h.markRead('3', true, false);
  assert.deepEqual(h.updates, [['1', true], ['3', true]]);
  await tick();
  assert.equal(h.calls.length, 2);
  h.calls.forEach(c => c.resolve()); await Promise.all([a, b]);
});

test('Undo updates immediately but persists after the original write', async () => {
  const h = harness();
  const a = h.markRead('1', true, false, true);
  h.toasts[0].opts.onUndo();
  assert.deepEqual(h.updates, [['1', true], ['1', false]]);
  await tick(); assert.equal(h.calls.length, 1);
  h.calls[0].resolve(); await a; await tick();
  assert.equal(h.calls[1].read, false);
  h.calls[1].resolve(); await tick();
});

test('a failed older request cannot undo newer intent; two failures restore the baseline', async () => {
  const h = harness();
  const a = h.markRead('1', true, false), b = h.markRead('1', false, false);
  await tick(); h.calls[0].reject(new Error('offline')); await a; await tick();
  assert.deepEqual(h.updates, [['1', true], ['1', false]]);
  h.calls[1].reject(new Error('offline')); await b;
  assert.deepEqual(h.updates.at(-1), ['1', false]);
  assert.equal(h.readChanges.size, 0);
});

test('thread Undo restores only messages that were unread', async () => {
  const h = harness(true); h.parent.dataset.unreadMembers = '2';
  const a = h.markRead('1', true, true, true);
  h.toasts[0].opts.onUndo();
  assert.deepEqual(h.updates, [['1', true], ['2', true], ['2', false]]);
  await tick(); assert.equal(h.calls.length, 1);
  h.calls[0].resolve(); await a; await tick();
  assert.equal(h.calls[1].id, '2'); assert.equal(h.calls[1].wholeThread, false);
  h.calls[1].resolve(); await tick();
});

test('stale background list responses are discarded while navigation is allowed', async () => {
  const h = harness(), xhr = {};
  h.handlers.get('htmx:beforeRequest')({ detail: { xhr } });
  const action = h.markRead('1', true, false);
  const event = { target: { id: 'message-list' }, detail: { xhr, shouldSwap: true, requestConfig: { elt: { id: 'message-list' } } } };
  h.handlers.get('htmx:beforeSwap')(event); assert.equal(event.detail.shouldSwap, false);
  event.detail.shouldSwap = true; event.detail.requestConfig.elt.id = 'folder-link';
  h.handlers.get('htmx:beforeSwap')(event); assert.equal(event.detail.shouldSwap, true);
  await tick(); h.calls[0].resolve(); await action;
});

test('pending local state is reapplied after a list response and retires only on a fresh response', async () => {
  const h = harness(), pendingXHR = {};
  const action = h.markRead('1', true, false);
  h.handlers.get('htmx:beforeRequest')({ detail: { xhr: pendingXHR } });
  const target = { id: 'message-list', closest: () => true };
  h.handlers.get('htmx:afterSwap')({ target, detail: { xhr: pendingXHR } });
  assert.deepEqual(h.updates.at(-1), ['1', true]); assert.equal(h.readChanges.size, 1);
  await tick(); h.calls[0].resolve(); await action;
  const xhr = {};
  h.handlers.get('htmx:beforeRequest')({ detail: { xhr } });
  h.handlers.get('htmx:afterSwap')({ target, detail: { xhr } });
  assert.equal(h.readChanges.size, 0);
  const event = { target, detail: { xhr: pendingXHR, shouldSwap: true, requestConfig: { elt: target } } };
  h.handlers.get('htmx:beforeSwap')(event); assert.equal(event.detail.shouldSwap, false);
});

test('gestures on expanded messages use the message ID and do not mark the whole thread', () => {
  const start = app.indexOf('function toggleRowRead(');
  const end = app.indexOf('// --- row gestures', start);
  const calls = [];
  const sandbox = { window: { htmx: {} }, markReadTimer: null, markRead: (...args) => calls.push(args) };
  vm.runInNewContext(app.slice(start, end) + '; globalThis.toggle = toggleRowRead;', sandbox);
  sandbox.toggle({ id: 'msg-s42', dataset: { mid: '42' }, hasAttribute: () => true });
  assert.deepEqual(calls, [['42', true, false, true]]);
});

test('a targeted event for the latest member preserves other unread messages in a collapsed thread', () => {
  const start = app.indexOf('function applyReadState('), end = app.indexOf('function markRead(', start);
  const parent = { dataset: { members: '1 2', unreadMembers: '1 2' } };
  const states = [];
  const sandbox = {
    document: { querySelectorAll: () => [parent], getElementById: () => null },
    rowsFor: () => [],
    setRowRead: () => states.push('read'), setRowUnread: () => states.push('unread'),
    updateReadEmptyState() {},
  };
  vm.runInNewContext(app.slice(start, end) + '; globalThis.apply = applyReadState;', sandbox);
  sandbox.apply('2', true, false);
  assert.equal(parent.dataset.unreadMembers, '1');
  assert.deepEqual(states, ['unread']);
  sandbox.apply('1', true, false);
  assert.deepEqual(states, ['unread', 'read']);
  sandbox.apply('2', false, false);
  assert.equal(parent.dataset.unreadMembers, '2');
  assert.deepEqual(states, ['unread', 'read', 'unread']);
});
