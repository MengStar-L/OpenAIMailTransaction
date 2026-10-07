'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test, before, after } = require('node:test');

let chromium;
try { ({ chromium } = require('playwright')); } catch { /* Optional local browser runtime; release CI installs it. */ }
const releaseState = (overrides = {}) => ({
  current_version: '1.1.2', latest_version: 'v1.1.3', repository: 'example/shiguang',
  release_url: 'https://github.com/example/shiguang/releases/tag/v1.1.3',
  available: true, supported: true, phase: 'idle', error: '',
  last_checked: '2026-10-07T10:00:00Z', settings: { auto_check: true, auto_update: false },
  ...overrides,
});

if (!chromium) {
  test('update progress browser integration (set NODE_PATH to Playwright)', { skip: true }, () => {});
} else {
  const root = path.join(__dirname, '..');
  const source = fs.readFileSync(path.join(root, 'web/system-updates.js'), 'utf8');
  const styles = ['styles.css', 'system-updates.css'].map(name => fs.readFileSync(path.join(root, 'web', name), 'utf8')).join('\n');
  let browser;
  before(async () => { browser = await chromium.launch({ headless: true }); });
  after(async () => { await browser?.close(); });

  async function page(initial = releaseState(), options = {}) {
    const tab = await browser.newPage({ viewport: options.viewport || { width: 1000, height: 850 } });
    await tab.route('http://shiguang.test/**', async route => {
      const url = new URL(route.request().url());
      if (url.pathname.startsWith('/assets/') && !url.pathname.includes('..')) {
        const filename = path.join(root, 'web', url.pathname);
        if (fs.existsSync(filename)) return route.fulfill({ body: fs.readFileSync(filename), contentType: filename.endsWith('.woff2') ? 'font/woff2' : 'image/png' });
      }
      return route.fulfill({ contentType: 'text/html', body: `<style>${styles}</style><main style="padding:30px;max-width:760px;margin:auto"><button id="outside">后台</button><div id="settings"></div></main>` });
    });
    await tab.goto('http://shiguang.test/admin');
    await tab.evaluate(({ initial, pending }) => {
      if (pending) sessionStorage.setItem('atelier.update.pending', JSON.stringify(pending));
      window.responses = [initial]; window.requests = []; window.notifications = []; window.timers = new Map();
      window.unresolved = new Map(); let nextTimer = 0;
      window.setInterval = callback => { const id = ++nextTimer; window.timers.set(id, callback); return id; };
      window.clearInterval = id => window.timers.delete(id);
      window.api = (address, options = {}) => {
        const index = window.requests.length;
        window.requests.push({ address, ...options, dialogOpen: !!document.querySelector('.update-dialog')?.open });
        const finish = value => { if (value?.errorResponse) throw Object.assign(new Error(value.errorResponse), { status: value.status }); return value; };
        if (window.responses.length) return Promise.resolve(window.responses.shift()).then(finish);
        return new Promise((resolve, reject) => window.unresolved.set(index, value => { try { resolve(finish(value)); } catch (error) { reject(error); } }));
      };
      const paths = { close: '<path d="m6 6 12 12M6 18 18 6"/>', star: '<path d="m12 2 2.6 7.4L22 12l-7.4 2.6L12 22l-2.6-7.4L2 12l7.4-2.6Z"/>', refresh: '<path d="M20 7v5h-5M4 17v-5h5m-4.4-4a8 8 0 0 1 13.2-3L20 8M4 16l2.2 3A8 8 0 0 0 19.4 16"/>', download: '<path d="M12 3v12m-5-5 5 5 5-5M4 15v5h16v-5"/>' };
      window.dependencies = { api: window.api, icon: name => `<svg viewBox="0 0 24 24" aria-hidden="true">${paths[name] || ''}</svg>`, esc: value => String(value ?? '').replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char])), notify: (message, error = false) => window.notifications.push({ message, error }) };
    }, { initial, pending: options.pending });
    await tab.addScriptTag({ content: source });
    await tab.evaluate(global => {
      if (global) window.SystemUpdatePanel.start(window.dependencies);
      else window.SystemUpdatePanel.mount(document.querySelector('#settings'), window.dependencies);
    }, !!options.global);
    await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); });
    return {
      tab,
      async queue(value) { await tab.evaluate(value => window.responses.push(value), value); },
      async click(selector, response) {
        if (response !== undefined) await this.queue(response);
        await tab.locator(selector).click();
        await tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); });
      },
      async poll(response) {
        if (response !== undefined) await this.queue(response);
        await tab.evaluate(async () => { for (const callback of [...window.timers.values()]) await callback(); });
      },
      async requests() { return tab.evaluate(() => window.requests); },
      async notifications() { return tab.evaluate(() => window.notifications); },
    };
  }

  test('manual update opens the dialog before the request and reports real byte progress', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]');
      assert.equal(await p.tab.locator('.update-dialog').getAttribute('open'), '');
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '准备更新');
      assert.equal((await p.requests())[1].dialogOpen, true);
      assert.equal(await p.tab.locator('progress').getAttribute('value'), null);
      await p.tab.evaluate(value => window.unresolved.get(1)(value), releaseState({ phase: 'downloading', downloaded_bytes: 2621440, total_bytes: 10485760 }));
      await p.tab.waitForFunction(() => document.querySelector('progress').value === 25);
      assert.match(await p.tab.locator('[data-update-transfer-value]').innerText(), /2.5 MB \/ 10.0 MB · 25%/);
      assert.equal(await p.tab.locator('[data-update-stage="1"]').getAttribute('aria-current'), 'step');
      await p.poll(releaseState({ phase: 'verifying' }));
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '正在校验');
      assert.equal(await p.tab.locator('progress').getAttribute('value'), null, 'phase bars must not invent an overall percentage');
    } finally { await p.tab.close(); }
  });

  test('unknown download size stays indeterminate, and failed verification is visible', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]', releaseState({ phase: 'downloading', downloaded_bytes: 4096 }));
      assert.equal(await p.tab.locator('progress').getAttribute('value'), null);
      assert.equal(await p.tab.locator('[data-update-transfer-value]').innerText(), '已下载 4.0 KB');
      await p.poll(releaseState({ phase: 'error', error: '发行文件校验失败' }));
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '更新未完成');
      assert.equal(await p.tab.locator('#update-dialog-detail').innerText(), '发行文件校验失败');
      assert.equal(await p.tab.evaluate(() => sessionStorage.getItem('atelier.update.pending')), null);
      assert.deepEqual(await p.notifications(), []);
    } finally { await p.tab.close(); }
  });

  test('Escape restores focus and exposes a reopening button while update polling continues', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]', releaseState({ phase: 'downloading' }));
      await p.tab.keyboard.press('Tab');
      assert.equal(await p.tab.evaluate(() => document.querySelector('.update-dialog').contains(document.activeElement)), true);
      await p.tab.keyboard.press('Escape');
      assert.equal(await p.tab.locator('.update-dialog').getAttribute('open'), null);
      assert.equal(await p.tab.locator('.update-reopen').isVisible(), true);
      assert.equal(await p.tab.evaluate(() => document.activeElement.classList.contains('update-reopen')), true);
      await p.poll(releaseState({ phase: 'verifying' }));
      assert.equal(await p.tab.locator('.update-dialog').getAttribute('open'), null, 'polls must respect a dismissed dialog');
      await p.click('.update-reopen');
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '正在校验');
      assert.equal((await p.requests()).filter(item => item.address.endsWith('/apply')).length, 1);
    } finally { await p.tab.close(); }
  });

  test('network errors reconnect without retrying mutation; success requires the target running version', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]', { errorResponse: '连接中断' });
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '正在重新连接');
      await p.poll({ errorResponse: 'connection refused' });
      assert.deepEqual(await p.notifications(), []);
      await p.poll(releaseState({ current_version: '1.1.2', phase: 'idle' }));
      assert.notEqual(await p.tab.locator('#update-dialog-title').innerText(), '更新完成');
      await p.poll(releaseState({ current_version: '1.1.3', phase: 'idle', available: false }));
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '更新完成');
      assert.equal(await p.tab.locator('[data-update-result]').innerText(), '进入新版本');
      assert.equal(await p.tab.locator('[data-update-apply]').isDisabled(), true);
      await p.poll(releaseState({ current_version: '1.1.3', available: false }));
      assert.deepEqual(await p.notifications(), [{ message: '程序已更新', error: false }]);
      assert.equal((await p.requests()).filter(item => item.address.endsWith('/apply')).length, 1);
    } finally { await p.tab.close(); }
  });

  test('401 during restart asks for login and retains pending version rather than claiming success', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]', releaseState({ phase: 'restarting' }));
      await p.poll({ errorResponse: '登录已失效', status: 401 });
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '请重新登录');
      assert.equal(await p.tab.locator('[data-update-result]').innerText(), '重新登录');
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
      assert.equal(await p.tab.evaluate(() => JSON.parse(sessionStorage.getItem('atelier.update.pending')).to), 'v1.1.3');
      assert.deepEqual(await p.notifications(), []);
    } finally { await p.tab.close(); }
  });

  test('a reloaded frontend confirms a persisted update only against a newer running version', async () => {
    const pending = { from: '1.1.2', to: 'v1.1.3', started: Date.now() };
    const p = await page(releaseState({ current_version: '1.1.3', latest_version: '', available: false }), { global: true, pending });
    try {
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '更新完成');
      assert.equal(await p.tab.evaluate(() => sessionStorage.getItem('atelier.update.pending')), null);
      assert.deepEqual(await p.notifications(), [{ message: '程序已更新', error: false }]);
    } finally { await p.tab.close(); }
    const rollback = await page(releaseState({ current_version: '1.1.1', latest_version: '', available: false }), { global: true, pending });
    try {
      assert.notEqual(await rollback.tab.locator('#update-dialog-title').innerText(), '更新完成');
      assert.deepEqual(await rollback.notifications(), []);
    } finally { await rollback.tab.close(); }
  });

  test('automatic updates appear outside settings and survive navigation between admin tabs', async () => {
    const p = await page(releaseState(), { global: true });
    try {
      assert.equal(await p.tab.locator('.update-dialog').count(), 0);
      await p.poll(releaseState({ phase: 'downloading', settings: { auto_check: true, auto_update: true } }));
      assert.equal(await p.tab.locator('.update-dialog').getAttribute('open'), '');
      await p.tab.evaluate(() => { document.querySelector('#settings').replaceChildren(); });
      await p.poll(releaseState({ phase: 'verifying' }));
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '正在校验');
      assert.equal(await p.tab.evaluate(() => window.timers.size), 1);
      await p.tab.evaluate(() => window.SystemUpdatePanel.stop());
      assert.equal(await p.tab.locator('.update-dialog').count(), 0);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
    } finally { await p.tab.close(); }
  });

  test('a rejected update explains why and does not leave an artificial active update', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]', { errorResponse: '仍有进行中的订单，结束后再更新', status: 409 });
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '更新未完成');
      assert.match(await p.tab.locator('#update-dialog-detail').innerText(), /进行中的订单/);
      assert.equal(await p.tab.evaluate(() => sessionStorage.getItem('atelier.update.pending')), null);
      await p.click('[data-update-hide]');
      assert.equal(await p.tab.locator('[data-update-apply]').isDisabled(), false);
    } finally { await p.tab.close(); }
  });

  test('an old poll response cannot clear a newly requested update', async () => {
    const p = await page();
    try {
      await p.tab.evaluate(() => { window.inflightPoll = [...window.timers.values()][0](); });
      await p.click('[data-update-apply]', releaseState({ phase: 'downloading' }));
      await p.tab.evaluate(value => window.unresolved.get(1)(value), releaseState({ phase: 'error', error: '上次更新失败' }));
      await p.tab.evaluate(() => window.inflightPoll);
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '正在下载');
      assert.equal(await p.tab.evaluate(() => JSON.parse(sessionStorage.getItem('atelier.update.pending')).to), 'v1.1.3');
      assert.deepEqual(await p.notifications(), []);
    } finally { await p.tab.close(); }
  });

  test('logging out during an update request cannot reopen the discarded admin dialog', async () => {
    const p = await page();
    try {
      await p.click('[data-update-apply]');
      await p.tab.evaluate(() => window.SystemUpdatePanel.stop());
      await p.tab.evaluate(value => window.unresolved.get(1)(value), releaseState({ phase: 'downloading' }));
      await p.tab.evaluate(async () => { await Promise.resolve(); await Promise.resolve(); });
      assert.equal(await p.tab.locator('.update-dialog').count(), 0);
      assert.equal(await p.tab.locator('.update-reopen').count(), 0);
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
    } finally { await p.tab.close(); }
  });

  test('a healthy unchanged server after the update deadline resolves failure and re-enables checking', async () => {
    const p = await page(releaseState(), { pending: { from: '1.1.2', to: 'v1.1.3', started: Date.now() - 11 * 60 * 1000 } });
    try {
      assert.equal(await p.tab.locator('#update-dialog-title').innerText(), '更新未完成');
      assert.match(await p.tab.locator('#update-dialog-detail').innerText(), /当前版本仍为 1.1.2/);
      assert.equal(await p.tab.evaluate(() => sessionStorage.getItem('atelier.update.pending')), null);
      await p.click('[data-update-hide]');
      assert.equal(await p.tab.locator('[data-update-check]').isDisabled(), false);
      assert.equal(await p.tab.locator('[data-update-apply]').isDisabled(), false);
      assert.deepEqual(await p.notifications(), []);
    } finally { await p.tab.close(); }
  });

  test('preference saves remain coherent and failed saves preserve the server result', async () => {
    const p = await page(releaseState({ settings: { auto_check: false, auto_update: false } }));
    try {
      await p.click('[data-update-setting="auto_update"]', releaseState({ settings: { auto_check: true, auto_update: true } }));
      assert.deepEqual((await p.requests()).at(-1).body, { auto_check: true, auto_update: true });
      assert.equal(await p.tab.locator('[data-update-setting="auto_check"]').isChecked(), true);
      await p.click('[data-update-setting="auto_check"]', releaseState({ settings: { auto_check: false, auto_update: false } }));
      assert.deepEqual((await p.requests()).at(-1).body, { auto_check: false, auto_update: false });
      await p.click('[data-update-setting="auto_update"]', { errorResponse: '无法保存更新设置', status: 500 });
      assert.equal(await p.tab.locator('[data-update-setting="auto_update"]').isChecked(), false);
      assert.equal((await p.notifications()).at(-1).message, '无法保存更新设置');
      assert.equal(await p.tab.locator('.update-dialog').count(), 0);
    } finally { await p.tab.close(); }
  });

  test('standalone panel leaves stop polling and hidden tabs avoid unnecessary requests', async () => {
    const p = await page();
    try {
      await p.tab.evaluate(() => Object.defineProperty(document, 'hidden', { configurable: true, value: true }));
      await p.poll();
      assert.equal((await p.requests()).length, 1);
      await p.tab.evaluate(() => document.querySelector('#settings').replaceChildren());
      await p.poll();
      assert.equal(await p.tab.evaluate(() => window.timers.size), 0);
    } finally { await p.tab.close(); }
  });

  test('desktop and 320px mobile progress dialogs fit, and reduced motion disables decorative animation', async () => {
    for (const viewport of [{ width: 1200, height: 900 }, { width: 320, height: 640 }]) {
      const p = await page(releaseState(), { viewport });
      try {
        await p.click('[data-update-apply]', releaseState({ phase: 'downloading', downloaded_bytes: 5242880, total_bytes: 12582912 }));
        await p.tab.evaluate(() => document.fonts.ready);
        await p.tab.waitForTimeout(300);
        const box = await p.tab.locator('.update-dialog').boundingBox();
        assert.ok(box.x >= 0 && box.x + box.width <= viewport.width);
        assert.ok(box.y >= 0 && box.y + box.height <= viewport.height);
        assert.equal(await p.tab.locator('.update-dialog').evaluate(el => el.scrollWidth <= el.clientWidth), true);
        if (process.env.UPDATE_UI_SCREENSHOT_DIR) {
          fs.mkdirSync(process.env.UPDATE_UI_SCREENSHOT_DIR, { recursive: true });
          await p.tab.screenshot({ path: path.join(process.env.UPDATE_UI_SCREENSHOT_DIR, `${viewport.width < 400 ? 'mobile' : 'desktop'}-download.png`), animations: 'disabled' });
        }
        await p.tab.emulateMedia({ reducedMotion: 'reduce' });
        assert.equal(await p.tab.locator('.update-emblem').evaluate(el => getComputedStyle(el, '::before').animationName), 'none');
      } finally { await p.tab.close(); }
    }
  });
}
