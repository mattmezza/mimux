// SPDX-License-Identifier: AGPL-3.0-only
import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const app = await readFile(new URL('./app.js', import.meta.url), 'utf8');
function harness() {
  const controls = [{disabled:false}, {disabled:false}];
  const labels = [{hidden:false}], progress = [{hidden:true}];
  const attrs = {};
  const listeners = {};
  const modal = {hidden:true};
  let requests = 0, valid = true;
  const form = {
    reportValidity: () => valid,
    setAttribute: (k,v) => { attrs[k] = v; },
    contains: el => el === form || controls.includes(el),
    requestSubmit() {
      const e = {detail:{elt:form}, defaultPrevented:false, preventDefault(){this.defaultPrevented=true;}};
      listeners['htmx:beforeRequest'](e);
      if (!e.defaultPrevented) requests++;
    },
  };
  const root = {
    querySelector: sel => sel === '#compose-form' ? form : {setAttribute(){}},
    querySelectorAll: sel => sel.includes('control') ? controls : sel.includes('label') ? labels : progress,
  };
  const context = {window:{}, document:{
    getElementById: id => ({'compose-window':root,'compose-form':form,'attach-reminder-modal':modal})[id],
    querySelector: () => null,
    addEventListener: (name,fn) => {listeners[name]=fn;},
  }, syncComposeEditor(){}, clearComposeAutosaveTimer(){}};
  const state = app.slice(app.indexOf('let composeSending ='), app.indexOf('function clearComposeAutosaveTimer'));
  const send = app.slice(app.indexOf('function setSendMode('), app.indexOf('function needsAttachmentReminder('));
  vm.runInNewContext(state + send + '\nfunction needsAttachmentReminder(){return globalThis.needsReminder;}\nglobalThis.submit = submitCompose; globalThis.busy = setComposeSending;',context);
  return {context,controls,labels,progress,attrs,modal,requests:()=>requests,invalid:()=>{valid=false;}};
}

test('send locks controls, shows progress, and rejects duplicate submissions', () => {
  const h=harness(); h.context.submit(); h.context.submit();
  assert.equal(h.requests(),1);
  assert.ok(h.controls.every(c=>c.disabled));
  assert.equal(h.attrs['aria-busy'],'true');
  assert.equal(h.progress[0].hidden,false);
  assert.equal(h.labels[0].hidden,true);
  h.context.busy(false);
  assert.ok(h.controls.every(c=>!c.disabled));
  assert.equal(h.progress[0].hidden,true);
  h.context.submit(); assert.equal(h.requests(),2);
});

test('validation and reminder gate keep controls idle; Send anyway uses the busy state', () => {
  const invalid=harness(); invalid.invalid(); invalid.context.submit();
  assert.equal(invalid.requests(),0); assert.equal(invalid.progress[0].hidden,true);
  const h=harness(); h.context.needsReminder=true; h.context.submit();
  assert.equal(h.requests(),0); assert.equal(h.modal.hidden,false);
  assert.equal(h.progress[0].hidden,true);
  h.context.submit(true);
  assert.equal(h.requests(),1); assert.equal(h.progress[0].hidden,false);
});
