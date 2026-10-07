'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'mail-alerts.js'), 'utf8');

class Events {
  listeners = new Map();
  addEventListener(name, handler) { const handlers = this.listeners.get(name) || new Set(); handlers.add(handler); this.listeners.set(name, handlers); }
  removeEventListener(name, handler) { this.listeners.get(name)?.delete(handler); }
  dispatchEvent(event) { for (const handler of this.listeners.get(event.type) || []) handler(event); return true; }
}
class Storage {
  values = new Map();
  getItem(key) { return this.values.get(key) ?? null; }
  setItem(key, value) { this.values.set(key, String(value)); }
}
function createPage(options = {}) {
  const window = new Events();
  const document = new Events();
  const played = [];
  const timers = new Map();
  let timerId = 0;
  let contextCount = 0;
  document.title = '拾光 · Atelier';
  document.hidden = !!options.hidden;
  document.focused = options.focused ?? !options.hidden;
  document.hasFocus = () => document.focused;
  window.localStorage = options.localStorage || new Storage();
  window.sessionStorage = options.sessionStorage || new Storage();
  window.setTimeout = (fn, ms) => { const id = ++timerId; timers.set(id, { fn, ms }); return id; };
  window.clearTimeout = id => timers.delete(id);
  class Context extends Events {
    state = 'suspended';
    currentTime = 0;
    destination = {};
    constructor() { super(); contextCount++; }
    async resume() { if (options.audioFails) throw new Error('Blocked'); this.state = 'running'; this.dispatchEvent({ type: 'statechange' }); }
    async close() { this.state = 'closed'; }
    createOscillator() {
      const oscillator = new Events();
      oscillator.frequency = { setValueAtTime() {} };
      oscillator.connect = oscillator.disconnect = oscillator.stop = () => {};
      oscillator.start = () => played.push(oscillator);
      return oscillator;
    }
    createGain() { return { gain: { setValueAtTime() {}, linearRampToValueAtTime() {}, exponentialRampToValueAtTime() {} }, connect() {}, disconnect() {} }; }
  }
  if (options.audioSupported !== false) window.AudioContext = Context;
  class CustomEvent { constructor(type, values) { this.type = type; this.detail = values?.detail; } }
  vm.runInNewContext(source, { window, document, CustomEvent, console, Date, Promise, Map, Set });
  return { window, document, alerts: window.MailArrivalAlerts, played, timers, contextCount: () => contextCount };
}
const queued = id => ({ id, kind: 'email', status: 'queued', resource: '' });
const allocated = id => ({ id, kind: 'email', status: 'waiting', resource: 'sample@example.test' });

test('queue allocation sounds once; repeated polls and new verification codes do not re-notify', async () => {
  const page = createPage();
  let arrivals = 0;
  page.window.addEventListener('mail-arrival', () => arrivals++);
  assert.equal(page.alerts.getState().audioLocked, true);
  assert.equal(await page.alerts.armAudio(), true);
  page.alerts.observe(queued('one'));
  assert.equal(page.alerts.observe(allocated('one')), true);
  page.alerts.observe(allocated('one'));
  page.alerts.observe({ ...allocated('one'), status: 'received', code: '123456' });
  assert.equal(arrivals, 1);
  assert.equal(page.played.length, 3);
  assert.equal(page.document.title, '【邮箱已就绪】 拾光 · Atelier');
  assert.equal([...page.timers.values()][0].ms, 8000);
  [...page.timers.values()][0].fn();
  assert.equal(page.document.title, '拾光 · Atelier');
});

test('restored allocated orders are silent; pending orders survive reload and notify once', () => {
  const session = new Storage();
  const first = createPage({ sessionStorage: session });
  assert.equal(first.alerts.observe(allocated('old')), false);
  first.alerts.observe(queued('waiting'));
  first.alerts.dispose();
  const second = createPage({ sessionStorage: session });
  assert.equal(second.alerts.observe(allocated('old')), false);
  assert.equal(second.alerts.observe(allocated('waiting')), true);
  const third = createPage({ sessionStorage: session });
  assert.equal(third.alerts.observe(allocated('waiting')), false);
  assert.equal(third.alerts.observe(allocated('new'), { freshAllocation: true }), true);
  assert.equal(third.alerts.observe(allocated('new'), { freshAllocation: true }), false);
  assert.ok(![...session.values.values()].join('').includes('sample@example.test'));
});

test('hidden tabs retain attention until visible and focused, and allow pending mailbox polling', () => {
  const page = createPage({ hidden: true });
  page.alerts.observe(queued('one'));
  page.alerts.observe(allocated('one'));
  assert.equal(page.timers.size, 0);
  assert.equal(page.alerts.shouldPollHidden(queued('two')), true);
  assert.equal(page.alerts.shouldPollHidden(allocated('two')), false);
  page.document.hidden = false;
  page.document.dispatchEvent({ type: 'visibilitychange' });
  assert.equal(page.alerts.getState().attention, true);
  page.document.focused = true;
  page.window.dispatchEvent({ type: 'focus' });
  assert.equal(page.alerts.getState().attention, false);
});

test('title attention persists if the page becomes hidden during the visible reminder', () => {
  const page = createPage();
  page.alerts.observe(allocated('new'), { freshAllocation: true });
  assert.equal(page.timers.size, 1);
  page.document.hidden = true;
  page.document.dispatchEvent({ type: 'visibilitychange' });
  assert.equal(page.timers.size, 0);
  assert.equal(page.alerts.getState().attention, true);
  page.alerts.setBaseTitle('新名称');
  assert.equal(page.document.title, '【邮箱已就绪】 新名称');
  page.document.hidden = false;
  page.document.dispatchEvent({ type: 'visibilitychange' });
  assert.equal(page.document.title, '新名称');
});

test('separate preferences persist; both off prevent hidden polling and title or audio effects', async () => {
  const local = new Storage();
  const page = createPage({ localStorage: local });
  await page.alerts.armAudio();
  let changes = 0;
  page.window.addEventListener('mail-alerts-change', () => changes++);
  page.alerts.setPreference('sound', false);
  assert.equal(page.alerts.getPreferences().tab, true);
  page.alerts.setPreference('tab', false);
  assert.equal(page.alerts.shouldPollHidden(queued('one')), false);
  assert.equal(page.alerts.observe(allocated('one'), { freshAllocation: true }), true);
  assert.equal(page.document.title, '拾光 · Atelier');
  assert.equal(page.played.length, 0);
  assert.ok(changes >= 2);
  const restored = createPage({ localStorage: local });
  assert.equal(restored.alerts.getPreferences().sound, false);
  assert.equal(restored.alerts.getPreferences().tab, false);
});

test('unsupported or blocked audio does not reject allocation; storage failures also work', async () => {
  const unavailableStorage = { getItem() { throw new Error('Denied'); }, setItem() { throw new Error('Denied'); } };
  const page = createPage({ audioFails: true, localStorage: unavailableStorage, sessionStorage: unavailableStorage });
  assert.equal(await page.alerts.armAudio(), false);
  page.alerts.observe(queued('one'));
  assert.equal(page.alerts.observe(allocated('one')), true);
  assert.equal(page.alerts.getState().audioLocked, true);
  assert.equal(page.alerts.getState().attention, true);
  const unsupported = createPage({ audioSupported: false });
  assert.equal(await unsupported.alerts.armAudio(), false);
  assert.equal(unsupported.alerts.getState().audioSupported, false);
  assert.equal(unsupported.alerts.getState().audioLocked, false);
});

test('phone, terminal orders and stale transitions cannot trigger or re-arm email arrival', () => {
  const page = createPage();
  assert.equal(page.alerts.observe({ ...allocated('phone'), kind: 'phone' }, { freshAllocation: true }), false);
  page.alerts.observe(queued('cancelled'));
  assert.equal(page.alerts.observe({ ...allocated('cancelled'), status: 'cancelled' }), false);
  assert.equal(page.alerts.observe(allocated('cancelled')), false);
  page.alerts.observe(allocated('old'));
  page.alerts.observe(queued('old'));
  assert.equal(page.alerts.observe(allocated('old')), false);
  assert.equal(page.alerts.getState().attention, false);
});

test('genuine first gestures unlock sound; dispose is idempotent and removes listeners', async () => {
  const page = createPage();
  page.document.dispatchEvent({ type: 'pointerdown', isTrusted: false });
  assert.equal(page.contextCount(), 0);
  page.document.dispatchEvent({ type: 'pointerdown', isTrusted: true });
  await Promise.resolve();
  assert.equal(page.alerts.getState().audioLocked, false);
  assert.equal(page.contextCount(), 1);
  page.alerts.observe(allocated('new'), { freshAllocation: true });
  page.alerts.dispose();
  page.alerts.dispose();
  assert.equal(page.document.title, '拾光 · Atelier');
  assert.equal(page.timers.size, 0);
  assert.equal(page.document.listeners.get('pointerdown').size, 0);
  assert.equal(page.alerts.observe(allocated('later'), { freshAllocation: true }), false);
  assert.equal(page.alerts.shouldPollHidden(queued('later')), false);
});

test('observation persistence stays bounded and tolerates corrupt saved records', () => {
  const session = new Storage();
  session.setItem('shiguang.mail.alert.observations.v1', '{broken');
  const page = createPage({ sessionStorage: session });
  for (let index = 0; index < 130; index++) page.alerts.observe(allocated(`old-${index}`));
  const saved = JSON.parse(session.getItem('shiguang.mail.alert.observations.v1'));
  assert.equal(saved.length, 96);
  assert.equal(saved[95][0], 'old-129');
});

test('switching off tab alerts immediately clears existing attention, including a settings change in another tab', () => {
  const local = new Storage();
  const page = createPage({ localStorage: local, hidden: true });
  page.alerts.observe(allocated('new'), { freshAllocation: true });
  page.alerts.setPreference('tab', false);
  assert.equal(page.alerts.getState().attention, false);
  assert.equal(page.document.title, '拾光 · Atelier');
  page.alerts.setPreference('tab', true);
  page.alerts.observe(allocated('another'), { freshAllocation: true });
  local.setItem('shiguang.mail.alert.preferences.v1', JSON.stringify({ sound: false, tab: false }));
  page.window.dispatchEvent({ type: 'storage', key: 'shiguang.mail.alert.preferences.v1' });
  assert.equal(page.alerts.getState().attention, false);
  assert.equal(page.alerts.shouldPollHidden(queued('later')), false);
});

test('back-forward cache preserves the observer, whereas a final page exit disposes it', () => {
  const page = createPage();
  page.alerts.observe(queued('pending'));
  page.window.dispatchEvent({ type: 'pagehide', persisted: true });
  page.window.dispatchEvent({ type: 'pageshow', persisted: true });
  assert.equal(page.alerts.observe(allocated('pending')), true);
  page.window.dispatchEvent({ type: 'pagehide', persisted: false });
  assert.equal(page.alerts.observe(allocated('later'), { freshAllocation: true }), false);
});
