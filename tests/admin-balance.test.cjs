'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test, before, after } = require('node:test');
let chromium;
try { ({ chromium } = require('playwright')); } catch { /* Installed in release CI, optional locally. */ }
const balance = (overrides = {}) => ({ balance: '2.1419', currency: 'USD', mode: 'live', available: true, status: 'available', message: '', updated_at: '2026-10-08T01:02:03Z', ...overrides });

if (!chromium) {
  test('admin balance browser integration (set NODE_PATH to Playwright)', { skip: true }, () => {});
} else {
  const root = path.join(__dirname, '..');
  const source = fs.readFileSync(path.join(root, 'web/admin-balance.js'), 'utf8');
  const styles = ['styles.css', 'mail-alerts.css', 'admin-balance.css'].map(file => fs.readFileSync(path.join(root, 'web', file), 'utf8')).join('\n');
  let browser;
  before(async () => { browser = await chromium.launch({ headless: true }); });
  after(async () => { await browser?.close(); });
  async function page(initial = balance(), options = {}) {
    const tab = await browser.newPage({ viewport: options.viewport || { width: 1000, height: 650 } });
    const errors = [];
    tab.on('pageerror', error => errors.push(error.message));
    await tab.route('http://shiguang.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname.startsWith('/assets/') && !url.pathname.includes('..')) {
        const file = path.join(root, 'web', url.pathname);
        if (fs.existsSync(file)) return route.fulfill({ body: fs.readFileSync(file), contentType: file.endsWith('.woff2') ? 'font/woff2' : 'image/png' });
      }
      return route.fulfill({ contentType: 'text/html; charset=utf-8', body: `<style>${styles}</style><div class="admin-shell"><div class="admin-content"><header class="admin-topbar resource-topbar"><span class="topbar-title">管理空间</span><div class="header-actions"><span class="pill inventory-pill"><svg viewBox="0 0 24 24"><rect x="2" y="5" width="20" height="14" rx="4"/><path d="m3 7 9 6 9-6"/></svg><span>邮箱参考余量 <strong>18</strong></span><span class="inventory-allocation" hidden>暂不可分配</span></span><span id="admin-balance-slot"></span><button class="icon-button reminder-trigger" aria-label="到达提醒"><svg viewBox="0 0 24 24"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/></svg></button></div></header><main id="main"><div class="page-title"><h1>总览</h1></div><section class="panel" style="padding:24px"><h2>最近订单</h2><p class="inline-note" style="margin-top:14px">暂无订单</p></section></main></div></div>` });
    });
    await tab.goto(`http://shiguang.test/${options.public ? '' : 'admin'}`);
    await tab.evaluate(initial => {
      window.responses = initial === null ? [] : [initial]; window.requests = []; window.timers = new Map(); window.pending = new Map();
      let timerID = 0;
      window.setInterval = (callback, interval) => { const id = ++timerID; window.timers.set(id, { callback, interval }); return id; };
      window.clearInterval = id => window.timers.delete(id);
      window.api = address => {
        const id = window.requests.length; window.requests.push(address);
        const finish = value => { if (value?.errorResponse) throw Object.assign(new Error(value.errorResponse), { status: value.status }); return value; };
        if (window.responses.length) return Promise.resolve(window.responses.shift()).then(finish);
        return new Promise((resolve, reject) => window.pending.set(id, value => { try { resolve(finish(value)); } catch (error) { reject(error); } }));
      };
    }, initial);
    await tab.addScriptTag({ content: source });
    await tab.evaluate(() => window.AdminBalance.start(document.querySelector('#admin-balance-slot'), { api: window.api }));
    await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); });
    return {
      tab, errors,
      async response(data) { await tab.evaluate(data => window.responses.push(data), data); },
      async settle(id, data) { await tab.evaluate(({ id, data }) => window.pending.get(id)(data), { id, data }); await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); }); },
      async poll(data) {
        if (data !== undefined) await this.response(data);
        await tab.evaluate(async () => { for (const timer of [...window.timers.values()]) await timer.callback(); });
        await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); });
      },
      async requests() { return tab.evaluate(() => window.requests); },
      async visible(value) { await tab.evaluate(value => { Object.defineProperty(document, 'hidden', { configurable: true, value: !value }); document.dispatchEvent(new Event('visibilitychange')); }, value); await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); }); },
    };
  }

  test('balance starts unknown and displays exact USD precision including zero', async () => {
    const p = await page(null);
    try {
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '—');
      assert.equal(await p.tab.locator('.admin-balance').getAttribute('aria-busy'), 'true');
      await p.settle(0, balance());
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$2.1419');
      assert.match(await p.tab.locator('.admin-balance').getAttribute('title'), /USD/);
      await p.poll(balance({ balance: '0' }));
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$0.00');
      assert.equal(await p.tab.locator('[data-balance-label]').innerText(), '余额');
      assert.equal(await p.tab.locator('.admin-balance').isVisible(), true);
      assert.deepEqual(p.errors, []);
    } finally { await p.tab.close(); }
  });

  test('polls every 15 seconds only while visible and refreshes upon returning', async () => {
    const p = await page();
    try {
      assert.deepEqual(await p.tab.evaluate(() => [...window.timers.values()].map(timer => timer.interval)), [15000]);
      await p.visible(false); await p.poll();
      assert.equal((await p.requests()).length, 1);
      await p.response(balance({ balance: '1.00' }));
      await p.visible(true);
      assert.equal((await p.requests()).length, 2);
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$1.00');
      await p.poll(balance({ balance: '1.20' }));
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$1.20');
    } finally { await p.tab.close(); }
  });

  test('manual and post-operation refresh bypass the cache and coalesce while a request is pending', async () => {
    const p = await page(null);
    try {
      await p.tab.evaluate(() => { window.AdminBalance.refresh(true); window.AdminBalance.refresh(true); });
      assert.equal((await p.requests()).length, 1);
      await p.settle(0, balance());
      assert.deepEqual(await p.requests(), ['/api/admin/balance', '/api/admin/balance?refresh=1']);
      await p.settle(1, balance({ balance: '2.10' }));
      await p.response(balance({ balance: '2.12' }));
      await p.tab.locator('.admin-balance').click();
      assert.equal((await p.requests()).at(-1), '/api/admin/balance?refresh=1');
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$2.12');
      await p.visible(false);
      await p.tab.evaluate(() => window.AdminBalance.refresh(true));
      await p.response(balance({ balance: '2.15' }));
      await p.visible(true);
      assert.equal((await p.requests()).at(-1), '/api/admin/balance?refresh=1');
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$2.15');
    } finally { await p.tab.close(); }
  });

  test('failure and malformed amounts remove the previous balance and explicitly show unavailable', async () => {
    const p = await page();
    try {
      await p.poll({ errorResponse: '上游连接暂时不可用', status: 502 });
      assert.equal(await p.tab.locator('[data-balance-label]').innerText(), '余额不可用');
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '—');
      assert.match(await p.tab.locator('.admin-balance').getAttribute('title'), /上游连接暂时不可用/);
      for (const invalid of ['NaN', 'Infinity', '<img onerror=alert(1)>', '1e10', '']) {
        await p.poll(balance({ balance: invalid }));
        assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '—');
      }
      await p.poll(balance({ balance: null, available: false, status: 'unconfigured', message: '尚未配置 API 密钥' }));
      assert.match(await p.tab.locator('.admin-balance').getAttribute('title'), /尚未配置 API 密钥/);
      await p.poll(balance());
      assert.equal(await p.tab.locator('[data-balance-label]').innerText(), '余额');
    } finally { await p.tab.close(); }
  });

  test('demo balance is visibly identified as simulated funds', async () => {
    const p = await page(balance({ balance: '100.00', mode: 'demo', status: 'demo' }));
    try {
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$100.00');
      assert.equal(await p.tab.locator('[data-balance-demo]').isVisible(), true);
      assert.match(await p.tab.locator('.admin-balance').getAttribute('title'), /不代表上游真实资金/);
      assert.match(await p.tab.locator('.admin-balance').getAttribute('aria-label'), /模拟数据/);
    } finally { await p.tab.close(); }
  });

  test('logout removes balance and listeners; a late response cannot expose the amount again', async () => {
    const p = await page(null);
    try {
      await p.tab.evaluate(() => window.AdminBalance.stop());
      await p.settle(0, balance({ balance: '888.88' }));
      assert.equal(await p.tab.locator('.admin-balance').count(), 0);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
      await p.visible(false); await p.visible(true);
      assert.equal((await p.requests()).length, 1);
      assert.equal(await p.tab.locator('body').innerText().then(text => text.includes('888.88')), false);
    } finally { await p.tab.close(); }
  });

  test('a restarted admin shell has one timer and ignores the prior login response', async () => {
    const p = await page(null);
    try {
      await p.response(balance({ balance: '9.87' }));
      await p.tab.evaluate(() => window.AdminBalance.start(document.querySelector('#admin-balance-slot'), { api: window.api }));
      await p.settle(0, balance({ balance: '999.99' }));
      assert.equal(await p.tab.locator('.admin-balance').count(), 1);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 1);
      assert.equal(await p.tab.locator('[data-balance-amount]').innerText(), '$9.87');
      await p.poll({ errorResponse: '登录已失效', status: 401 });
      assert.equal(await p.tab.locator('.admin-balance').count(), 0);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
    } finally { await p.tab.close(); }
  });

  test('redemption pages never request or render administrator balance', async () => {
    const p = await page(balance(), { public: true });
    try {
      assert.equal(await p.tab.locator('.admin-balance').count(), 0);
      assert.deepEqual(await p.requests(), []);
      await p.visible(false); await p.visible(true);
      assert.deepEqual(await p.requests(), []);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
    } finally { await p.tab.close(); }
  });

  test('desktop and 320px headers keep balance and inventory legible without horizontal overflow', async () => {
    for (const viewport of [{ width: 1100, height: 650 }, { width: 320, height: 600 }]) {
      const p = await page(balance(), { viewport });
      try {
        await p.tab.evaluate(() => document.fonts.ready);
        assert.equal(await p.tab.locator('.inventory-pill').isVisible(), true);
        const badge = await p.tab.locator('.admin-balance').boundingBox();
        const inventory = await p.tab.locator('.inventory-pill').boundingBox();
        assert.ok(badge.x >= 0 && badge.x + badge.width <= viewport.width);
        assert.ok(inventory.x >= 0 && inventory.x + inventory.width <= viewport.width);
        assert.ok(Math.abs(inventory.y - badge.y) < 4, 'inventory and balance should share a row for ordinary amounts');
        assert.equal(await p.tab.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
        if (process.env.BALANCE_UI_SCREENSHOT_DIR) {
          fs.mkdirSync(process.env.BALANCE_UI_SCREENSHOT_DIR, { recursive: true });
          await p.tab.screenshot({ path: path.join(process.env.BALANCE_UI_SCREENSHOT_DIR, `${viewport.width < 400 ? 'mobile' : 'desktop'}-balance.png`), animations: 'disabled' });
        }
        await p.tab.evaluate(() => { document.querySelector('.inventory-allocation').hidden = false; });
        await p.poll(balance({ balance: '123456789012345678901234.123456789012', mode: 'demo', status: 'demo' }));
        assert.equal(await p.tab.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'large balances, stock errors and demo labels must also fit');
        assert.deepEqual(p.errors, []);
      } finally { await p.tab.close(); }
    }
  });
}
