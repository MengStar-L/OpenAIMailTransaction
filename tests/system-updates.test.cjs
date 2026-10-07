'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'system-updates.js'), 'utf8');
const clone = value => JSON.parse(JSON.stringify(value));
const flush = () => new Promise(resolve => setImmediate(resolve));

// A small DOM boundary, as in mail-alerts.test.cjs. The production panel owns
// rendering and event handlers; tests interact only through rendered controls,
// API responses, browser navigation and notifications.
class Element {
  constructor(tagName = 'section', attributes = {}) {
    this.tagName = tagName;
    this.attributes = attributes;
    this.listeners = new Map();
    this.dataset = {};
    this.children = [];
    this.isConnected = true;
    this.textContent = '';
    this.checked = Object.hasOwn(attributes, 'checked');
    this.disabled = Object.hasOwn(attributes, 'disabled');
    for (const [name, value] of Object.entries(attributes)) {
      if (name.startsWith('data-')) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = value;
    }
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  append(child) { this.children.push(child); }
  addEventListener(name, handler) {
    const handlers = this.listeners.get(name) || [];
    handlers.push(handler);
    this.listeners.set(name, handlers);
  }
  async dispatch(name) {
    if (this.disabled) return;
    await Promise.all((this.listeners.get(name) || []).map(handler => handler({ type: name, target: this })));
  }
  set innerHTML(html) {
    this.markup = html;
    this.children = [];
    // All opening tags are visited, including controls nested inside labels.
    for (const match of html.matchAll(/<(input|button|p|span)\b([^>]*)>/g)) {
      const attributes = {};
      for (const attribute of match[2].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) attributes[attribute[1]] = attribute[2] ?? '';
      const child = new Element(match[1], attributes);
      const end = html.indexOf(`</${match[1]}>`, match.index + match[0].length);
      if (end !== -1 && match[1] !== 'input') child.textContent = html.slice(match.index + match[0].length, end).replace(/<[^>]*>/g, '');
      this.children.push(child);
    }
  }
  querySelectorAll(selector) {
    const attribute = selector.match(/^\[([\w-]+)(?:="([^"]*)")?\]$/);
    return this.children.filter(child => attribute
      ? Object.hasOwn(child.attributes, attribute[1]) && (attribute[2] === undefined || child.attributes[attribute[1]] === attribute[2])
      : child.tagName === selector);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

function releaseState(overrides = {}) {
  return {
    current_version: '1.0.0', latest_version: 'v1.1.0', repository: 'example/shiguang',
    release_url: 'https://github.com/example/shiguang/releases/tag/v1.1.0',
    available: true, supported: true, phase: 'idle', error: '',
    last_checked: '2026-10-07T10:00:00Z', settings: { auto_check: true, auto_update: false },
    ...overrides,
  };
}

async function page(initial = releaseState()) {
  const responses = [initial];
  const requests = [];
  const notifications = [];
  const navigations = [];
  const timers = new Map();
  let timerID = 0;
  const document = { hidden: false, createElement: tag => new Element(tag) };
  const window = { location: { assign: value => navigations.push(value) } };
  const container = new Element('main');
  const api = async (address, options = {}) => {
    requests.push({ address, ...clone(options) });
    assert.ok(responses.length, `unexpected request: ${address}`);
    const response = responses.shift();
    if (response instanceof Error) throw response;
    return clone(response);
  };
  vm.runInNewContext(source, {
    window, document, console,
    setInterval: callback => { const id = ++timerID; timers.set(id, callback); return id; },
    clearInterval: id => timers.delete(id),
  });
  window.SystemUpdatePanel.mount(container, { api, icon: () => '', esc: value => String(value ?? ''), notify: (message, error = false) => notifications.push({ message, error }) });
  await flush();
  const host = container.children[0];
  return {
    host, document, requests, responses, notifications, navigations, timers,
    status: () => host.querySelector('[role="status"]')?.textContent,
    async click(selector, response) {
      if (response !== undefined) responses.push(response);
      const control = host.querySelector(selector);
      assert.ok(control, `control absent: ${selector}`);
      await control.dispatch('click');
      await flush();
    },
    async change(setting, checked, response) {
      responses.push(response);
      const control = host.querySelector(`[data-update-setting="${setting}"]`);
      assert.ok(control, `setting absent: ${setting}`);
      control.checked = checked;
      await control.dispatch('change');
      await flush();
    },
    async poll(response) {
      if (response !== undefined) responses.push(response);
      for (const callback of [...timers.values()]) await callback();
      await flush();
    },
  };
}

test('a restarted service returning 401 exits reconnect mode and sends the administrator to login', async () => {
  const p = await page();
  await p.click('[data-update-apply]', releaseState({ phase: 'downloading' }));
  const unauthorized = Object.assign(new Error('登录已失效'), { status: 401 });
  await p.poll(unauthorized);
  assert.deepEqual(p.navigations, ['/admin']);
  assert.equal(p.timers.size, 0, 'an expired session must not keep polling forever');
  assert.ok(p.notifications.some(item => item.message.includes('重新登录')));
  assert.deepEqual(p.requests[1], { address: '/api/admin/updates/apply', method: 'POST', body: {} });
});

test('temporary network failure during a restart keeps reconnecting without discarding the update or redirecting', async () => {
  const p = await page();
  await p.click('[data-update-apply]', releaseState({ phase: 'restarting' }));
  await p.poll(new Error('网络连接中断'));
  assert.equal(p.status(), '正在重新连接');
  assert.equal(p.host.querySelector('[data-update-apply]').disabled, true);
  assert.equal(p.timers.size, 1);
  assert.deepEqual(p.navigations, []);
  assert.equal(p.notifications.length, 0, 'temporary downtime is expected during restart');
  await p.poll(releaseState({ phase: 'restarting' }));
  assert.equal(p.status(), '正在重新连接');
});

test('reconnection with a newer running version acknowledges success once and stops offering that update', async () => {
  const p = await page();
  await p.click('[data-update-apply]', releaseState({ phase: 'downloading' }));
  await p.poll(new Error('connection refused'));
  const updated = releaseState({ current_version: '1.1.0', available: false });
  await p.poll(updated);
  assert.equal(p.status(), '已是最新版本');
  assert.equal(p.host.querySelector('[data-update-apply]').disabled, true);
  assert.equal(p.host.querySelector('[data-update-check]').disabled, false);
  await p.poll(updated);
  assert.deepEqual(p.notifications, [{ message: '程序已更新', error: false }]);
  assert.deepEqual(p.navigations, []);
});

test('preference controls submit both settings coherently and show the server-persisted result', async () => {
  const p = await page(releaseState({ settings: { auto_check: false, auto_update: false } }));
  await p.change('auto_update', true, releaseState({ settings: { auto_check: true, auto_update: true } }));
  assert.deepEqual(p.requests.at(-1), {
    address: '/api/admin/updates/settings', method: 'PUT', body: { auto_check: true, auto_update: true },
  });
  assert.equal(p.host.querySelector('[data-update-setting="auto_check"]').checked, true);
  assert.equal(p.host.querySelector('[data-update-setting="auto_update"]').checked, true);
  await p.change('auto_check', false, releaseState({ settings: { auto_check: false, auto_update: false } }));
  assert.deepEqual(p.requests.at(-1).body, { auto_check: false, auto_update: false });
  assert.equal(p.host.querySelector('[data-update-setting="auto_update"]').checked, false);
  assert.equal(p.notifications.filter(item => item.message === '更新设置已保存').length, 2);
});

test('a failed preference save preserves the previous switches and reports the failure', async () => {
  const p = await page();
  await p.change('auto_update', true, new Error('无法保存更新设置'));
  assert.equal(p.host.querySelector('[data-update-setting="auto_update"]').checked, false);
  assert.equal(p.host.querySelector('[data-update-setting="auto_check"]').checked, true);
  assert.deepEqual(p.notifications, [{ message: '无法保存更新设置', error: true }]);
});

test('leaving the settings page cancels polling and hidden tabs do not issue background refresh requests', async () => {
  const p = await page();
  p.document.hidden = true;
  await p.poll();
  assert.equal(p.requests.length, 1);
  p.host.isConnected = false;
  await p.poll();
  assert.equal(p.timers.size, 0);
  assert.equal(p.requests.length, 1);
});
