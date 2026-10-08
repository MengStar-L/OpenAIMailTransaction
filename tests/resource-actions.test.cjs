'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test, before, after } = require('node:test');

let chromium;
try { ({ chromium } = require('playwright')); } catch { /* Optional local browser runtime. */ }

if (!chromium) {
  test('resource action browser integration (set NODE_PATH to Playwright)', { skip: true }, () => {});
} else {
  const web = path.join(__dirname, '../web');
  let browser;
  before(async () => { browser = await chromium.launch({ headless: true }); });
  after(async () => { await browser?.close(); });

  function order(kind = 'phone', changes = {}) {
    const now = Date.now();
    return {
      id: `${kind}-fixture`, kind, status: 'waiting', resource: kind === 'phone' ? '+573142177950' : '+mail+tag@example.test',
      created_at: new Date(now).toISOString(), expires_at: new Date(now + 20 * 60000).toISOString(),
      cancel_after: new Date(now + 120000).toISOString(), used_count: 0, usage_limit: 1, can_retry: true,
      codes: [], message: '等待验证码', ...changes,
    };
  }

  async function fixture({ admin = false, resource = order(), fallbackCopy = false, freshCDK = false, clock = false, initialStorage = {} } = {}) {
    const tab = await browser.newPage({ viewport: { width: 1200, height: 900 } });
    const state = { order: resource, cancelRequests: [], errors: [], releaseCancel: null, allocationRequests: [], allocationResult: null, allocationErrors: [], catalogRequests: 0, waitAllocation: false, releaseAllocation: null, catalogChannels: null };
    tab.on('pageerror', error => state.errors.push(error.message));
    if (clock) await tab.clock.install();
    await tab.addInitScript(({ fallbackCopy, freshCDK, initialStorage }) => {
      if (!freshCDK) sessionStorage.setItem('atelier.resource.token', 'fixture-token');
      if (!sessionStorage.getItem('fixture-seeded')) {
        Object.entries(initialStorage).forEach(([key, value]) => localStorage.setItem(key, value));
        sessionStorage.setItem('fixture-seeded', 'true');
      }
      window.copies = [];
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: fallbackCopy ? undefined : {
        writeText: async value => window.copies.push(value),
      } });
      document.execCommand = command => {
        if (command !== 'copy') return false;
        window.copies.push(document.activeElement.value);
        return true;
      };
    }, { fallbackCopy, freshCDK, initialStorage });
    await tab.route('**/*', async route => {
      const request = route.request();
      const url = new URL(request.url());
      const json = value => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) });
      if (url.pathname === '/api/config') return json({ brand: '拾光 · Atelier', mode: 'live', configured: true, phone_enabled: true, email_enabled: true });
      if (url.pathname === '/api/inventory') return json({ email: { count: 6 } });
      if (url.pathname === '/api/admin/session') return json({});
      if (request.method() === 'POST' && ['/api/admin/resources', '/api/orders/replace', '/api/redeem'].includes(url.pathname)) {
        state.allocationRequests.push(request.postDataJSON());
        if (state.waitAllocation) await new Promise(resolve => { state.releaseAllocation = resolve; });
        const rejection = state.allocationErrors.shift();
        if (rejection?.abort) return route.abort('failed');
        if (rejection) return route.fulfill({ status: rejection.status, contentType: 'application/json', body: JSON.stringify({ error: rejection.message || '请选择可用的手机号渠道', code: rejection.code }) });
        state.order = state.allocationResult || order('phone', { id: 'failed-allocation-fixture', status: 'failed', resource: '', message: '所选渠道暂时无法分配，请选择其他渠道' });
        return json({ token: 'fixture-token', order: state.order });
      }
      if (url.pathname === '/api/admin/resources') return json({ orders: state.order ? [state.order] : [] });
      if (url.pathname === '/api/orders/current' || url.pathname === `/api/admin/resources/${resource?.id}`) return json({ order: state.order });
      if (url.pathname === '/api/orders/cancel' || url.pathname === `/api/admin/resources/${resource?.id}/cancel`) {
        state.cancelRequests.push({ method: request.method(), headers: request.headers(), body: request.postDataJSON() });
        await new Promise(resolve => { state.releaseCancel = resolve; });
        state.order = { ...state.order, status: 'cancel_pending', message: '已申请取消，正在等待上游释放' };
        return json({ order: state.order });
      }
      if (url.pathname.includes('/phone/channels')) {
        ++state.catalogRequests;
        return json({ kind: 'phone', channels: state.catalogChannels || (state.allocationRequests.length ? ['12'] : ['11', '12']).map(provider_id => ({ country: '0', country_name: '测试国家', provider_id, price: '0.1', count: 10, tier: 'bronze' })) });
      }
      if (url.pathname.startsWith('/api/')) return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'Unexpected fixture request' }) });
      if (url.pathname === '/' || url.pathname === '/admin') return route.fulfill({ contentType: 'text/html', body: fs.readFileSync(path.join(web, 'index.html')) });
      const name = path.basename(url.pathname);
      if (/^[\w-]+\.(?:js|css)$/.test(name)) return route.fulfill({ contentType: name.endsWith('.js') ? 'application/javascript' : 'text/css', body: fs.readFileSync(path.join(web, name)) });
      return route.fulfill({ status: 204, body: '' });
    });
    await tab.goto(`https://shiguang.test/${admin ? 'admin?tab=resources' : ''}`);
    await tab.locator(freshCDK ? '#cdk' : admin ? resource ? `[data-admin-copy-resource]` : '#admin-resource-phone' : '#copy-resource').waitFor();
    return { tab, state };
  }

  for (const admin of [false, true]) {
    test(`${admin ? 'admin' : 'user'} cancels immediately once and cannot replace until release is confirmed`, async () => {
      const { tab, state } = await fixture({ admin });
      const button = tab.locator(admin ? '#admin-resource-phone [data-resource-action="cancel"]' : '#cancel-order');
      const replacement = tab.locator(admin ? '#admin-resource-phone [data-resource-action="create"]' : '#replace-order');
      try {
        assert.equal(await button.isEnabled(), true, 'upstream cancellation time must not disable the request');
        assert.equal(await button.textContent(), '取消本次');
        await button.click();
        await tab.waitForFunction(() => document.querySelector('[aria-busy="true"]'));
        await button.dispatchEvent('click');
        assert.equal(await tab.locator('dialog[open]').count(), 0, 'one click sends cancellation without a confirmation dialog');
        assert.equal(await button.isDisabled(), true, 'duplicate submits are disabled while the request is pending');
        await new Promise(resolve => setImmediate(resolve));
        assert.equal(state.cancelRequests.length, 1);
        assert.equal(state.cancelRequests[0].method, 'POST');
        if (!admin) assert.equal(state.cancelRequests[0].headers.authorization, 'Bearer fixture-token');
        state.releaseCancel();
        await tab.locator(admin ? '#admin-resource-phone .cancel_pending' : '.order-status.cancel_pending').waitFor();
        assert.equal(await replacement.count(), 0, 'pending upstream release cannot expose a new allocation');
        assert.match(await tab.locator(admin ? '#admin-resource-phone' : '#exchange').innerText(), /正在确认资源释放/);
        state.order = { ...state.order, status: 'cancelled', message: '资源已释放' };
        await tab.reload();
        await replacement.waitFor();
        assert.deepEqual(state.errors, []);
      } finally { state.releaseCancel?.(); await tab.close(); }
    });
  }

  test('user and admin copy phone numbers without + through Clipboard API and HTTP-compatible fallback', async () => {
    for (const admin of [false, true]) for (const fallbackCopy of [false, true]) {
      const { tab, state } = await fixture({ admin, fallbackCopy });
      try {
        const copy = tab.locator(admin ? '#admin-resource-phone [data-admin-copy-resource]' : '#copy-resource');
        assert.equal(await tab.locator(admin ? '#admin-resource-phone .resource-value' : '#resource-value').textContent(), '+573142177950');
        await copy.click();
        assert.deepEqual(await tab.evaluate(() => window.copies), ['573142177950']);
        assert.deepEqual(state.errors, []);
      } finally { await tab.close(); }
    }
  });

  test('newly allocated phones can be cancelled before the first countdown tick', async () => {
    for (const freshCDK of [true, false]) {
      const { tab, state } = await fixture({ freshCDK, clock: true, resource: order('phone', { status: 'cancelled' }) });
      try {
        await tab.clock.pauseAt(new Date(Date.now() + 1000));
        state.allocationResult = order('phone', { id: 'new-phone-fixture' });
        state.waitAllocation = true;
        if (freshCDK) { await tab.locator('#cdk').fill('NEW-CDK-FIXTURE'); await tab.clock.runFor(700); }
        await tab.locator(freshCDK ? '#redeem-phone-channels input[type="radio"]' : '#order-phone-channels input[type="radio"]').first().check();
        await tab.locator(freshCDK ? '#redeem-form button[type="submit"]' : '#replace-order').click();
        await tab.waitForFunction(() => document.querySelector('[aria-busy="true"]'));
        assert.equal(state.allocationRequests.length, 1);
        state.releaseAllocation();
        await tab.locator('#cancel-order').waitFor();
        assert.equal(await tab.locator('#cancel-order').isEnabled(), true, 'allocation completion must unlock the freshly rendered cancel button without a timer tick');
        await tab.locator('#cancel-order').click();
        assert.equal(state.cancelRequests.length, 1);
        state.releaseCancel();
        await tab.locator('.order-status.cancel_pending').waitFor();
        assert.deepEqual(state.errors, []);
      } finally { state.releaseAllocation?.(); state.releaseCancel?.(); await tab.close(); }
    }
  });

  test('phone numbers already without +, email plus tags and verification codes retain their original values', async () => {
    for (const admin of [false, true]) for (const kind of ['phone', 'email']) {
      const resource = order(kind, { status: 'received', resource: kind === 'phone' ? '573142177950' : '+mail+tag@example.test', codes: [{ round: 1, code: '001234' }] });
      const { tab, state } = await fixture({ admin, resource });
      try {
        await tab.locator(admin ? `[data-admin-copy-resource]` : '#copy-resource').click();
        await tab.locator(admin ? '[data-admin-copy-code]' : '[data-copy-code]').click();
        assert.deepEqual(await tab.evaluate(() => window.copies), [resource.resource, '001234']);
        assert.deepEqual(state.errors, []);
      } finally { await tab.close(); }
    }
  });

  test('definite allocation failures refresh channels and require a new choice without auto-purchasing', async () => {
    for (const admin of [false, true]) {
      const { tab, state } = await fixture({ admin, resource: order('phone', { status: 'cancelled' }) });
      const catalog = tab.locator(admin ? '#admin-phone-channels' : '#order-phone-channels');
      const acquire = tab.locator(admin ? '#admin-resource-phone [data-resource-action="create"]' : '#replace-order');
      try {
        state.waitAllocation = true;
        await catalog.locator('input[type="radio"]').first().check();
        assert.equal(await acquire.isEnabled(), true);
        await acquire.click();
        await acquire.dispatchEvent('click');
        assert.equal(await acquire.textContent(), '获取中…');
        assert.equal(await acquire.isDisabled(), true);
        assert.equal(state.allocationRequests.length, 1);
        state.releaseAllocation();
        await tab.waitForFunction(admin => document.querySelector(admin ? '#admin-resource-phone .failed' : '.order-status.failed'), admin);
        await tab.waitForFunction(selector => document.querySelectorAll(`${selector} input[type="radio"]`).length === 1, admin ? '#admin-phone-channels' : '#order-phone-channels');
        assert.equal(state.allocationRequests.length, 1);
        assert.equal(state.catalogRequests, 2);
        assert.equal(await catalog.locator('input:checked').count(), 0);
        assert.match(await catalog.innerText(), /#12/);
        assert.equal(await acquire.isDisabled(), true);
        assert.deepEqual(state.errors, []);
      } finally { state.releaseAllocation?.(); await tab.close(); }
    }
  });

  test('visible CDK and replacement choices refresh on focus and periodically without losing stable controls', async () => {
    for (const freshCDK of [true, false]) {
      const { tab, state } = await fixture({ freshCDK, clock: true, resource: order('phone', { status: 'cancelled' }) });
      const selector = freshCDK ? '#redeem-phone-channels' : '#order-phone-channels';
      const catalog = tab.locator(selector);
      try {
        if (freshCDK) { await tab.locator('#cdk').fill('OLD-CDK-FIXTURE'); await tab.clock.runFor(700); }
        await catalog.locator('input[type="radio"]').first().check();
        const original = await catalog.locator('input[type="radio"]').first().elementHandle();
        const unchanged = tab.waitForResponse(response => response.url().includes('/phone/channels'));
        await tab.evaluate(() => window.dispatchEvent(new Event('focus')));
        await unchanged;
        assert.equal(await original.evaluate(input => input.isConnected), true, 'unchanged results preserve the existing focused controls');
        state.catalogChannels = [{ country: '1', country_name: '新国家', provider_id: '20', price: '0.08', count: 10, tier: 'silver' }];
        await tab.evaluate(() => window.dispatchEvent(new Event('focus')));
        await tab.waitForFunction(selector => document.querySelector(selector)?.innerText.includes('#20'), selector);
        assert.equal(await catalog.locator('input:checked').count(), 0, 'changed live policy clears the old choice');
        state.catalogChannels.push({ country: '1', country_name: '新国家', provider_id: '21', price: '0.09', count: 5, tier: 'bronze' });
        await tab.clock.runFor(10001);
        await tab.waitForFunction(selector => document.querySelector(selector)?.innerText.includes('#21'), selector);
        assert.equal(await catalog.locator('input:checked').count(), 0, 'restored channels never select themselves');
        assert.equal(state.allocationRequests.length, 0);
        assert.deepEqual(state.errors, []);
      } finally { await tab.close(); }
    }
  });

  const adminPhoneKey = 'atelier.admin.resource.request.phone';
  const phoneAcquire = '#admin-resource-phone [data-resource-action="create"]';

  test('legacy admin retries repair missing or malformed channel choices while preserving the request ID', async () => {
    for (const cached of [null, '{broken', JSON.stringify({ phone_country: '0', phone_provider_id: '' })]) {
      const requestID = 'legacy-admin-request-0001';
      const initialStorage = { [adminPhoneKey]: requestID };
      if (cached != null) initialStorage[`${adminPhoneKey}.phone_choice`] = cached;
      const { tab, state } = await fixture({ admin: true, resource: null, initialStorage });
      try {
        const acquire = tab.locator(phoneAcquire);
        const choice = tab.locator('#admin-phone-channels input[value="0:12"]');
        await choice.waitFor();
        assert.equal(await acquire.isDisabled(), true, 'an incomplete pending choice still requires a visible selection');
        await choice.check();
        state.allocationResult = order('phone', { request_id: requestID });
        await acquire.click();
        await tab.locator('[data-admin-copy-resource]').waitFor();
        assert.deepEqual(state.allocationRequests, [{ kind: 'phone', request_id: requestID, phone_country: '0', phone_provider_id: '12' }]);
        assert.equal(await tab.evaluate(key => localStorage.getItem(key), adminPhoneKey), null);
        assert.deepEqual(state.errors, []);
      } finally { await tab.close(); }
    }
  });

  test('explicit pre-allocation rejection clears only that request and accepts a newly selected channel', async () => {
    const { tab, state } = await fixture({ admin: true, resource: null });
    try {
      state.allocationErrors.push({ status: 409, code: 'admin_allocation_rejected' });
      await tab.locator('#admin-phone-channels input[value="0:11"]').check();
      await tab.locator(phoneAcquire).click();
      await tab.waitForFunction(() => document.querySelector('#admin-resource-error-phone')?.textContent.includes('请选择可用'));
      await tab.locator('#admin-phone-channels input[value="0:12"]').waitFor();
      assert.equal(await tab.evaluate(key => localStorage.getItem(key), adminPhoneKey), null);
      assert.equal(await tab.locator(phoneAcquire).isDisabled(), true, 'rejected choices require an explicit new choice');
      await tab.locator('#admin-phone-channels input[value="0:12"]').check();
      state.allocationResult = order();
      await tab.locator(phoneAcquire).click();
      await tab.locator('[data-admin-copy-resource]').waitFor();
      assert.equal(state.allocationRequests.length, 2);
      assert.equal(state.allocationRequests[0].phone_provider_id, '11');
      assert.equal(state.allocationRequests[1].phone_provider_id, '12');
      assert.notEqual(state.allocationRequests[1].request_id, state.allocationRequests[0].request_id);
      assert.deepEqual(state.errors, []);
    } finally { await tab.close(); }
  });

  test('uncertain admin purchases retain their original ID and channel through retry and reload', async () => {
    for (const rejection of [{ abort: true }, { status: 503 }, { status: 409 }]) {
      const { tab, state } = await fixture({ admin: true, resource: null });
      try {
        state.allocationErrors.push(rejection);
        await tab.locator('#admin-phone-channels input[value="0:11"]').check();
        await tab.locator(phoneAcquire).click();
        await tab.waitForFunction(() => document.querySelector('#admin-resource-error-phone')?.textContent);
        assert.equal(await tab.locator(phoneAcquire).innerText(), '确认上次申请');
        assert.equal(await tab.locator('#admin-phone-channels input').count(), 0, 'unknown purchases must not pretend a new channel can be chosen');
        assert.match(await tab.locator('#admin-resource-phone').innerText(), /#11/);
        const firstRequest = state.allocationRequests[0];
        assert.equal(await tab.evaluate(key => localStorage.getItem(key), adminPhoneKey), firstRequest.request_id);
        await tab.reload();
        await tab.locator(phoneAcquire).waitFor();
        assert.equal(await tab.locator(phoneAcquire).innerText(), '确认上次申请');
        assert.equal(await tab.locator('#admin-phone-channels input').count(), 0);
        state.allocationResult = order('phone', { request_id: firstRequest.request_id });
        await tab.locator(phoneAcquire).click();
        await tab.locator('[data-admin-copy-resource]').waitFor();
        assert.deepEqual(state.allocationRequests, [firstRequest, firstRequest], 'a lost response never creates a second purchase identity');
        assert.equal(await tab.evaluate(key => localStorage.getItem(key), adminPhoneKey), null);
        assert.deepEqual(state.errors, []);
      } finally { await tab.close(); }
    }
  });

  test('a delayed validation rejection cannot erase a newer request saved by another tab', async () => {
    for (const rejection of [{ status: 409, code: 'admin_allocation_rejected' }, { status: 400 }, { status: 422 }]) {
      const { tab, state } = await fixture({ admin: true, resource: null });
      try {
        state.waitAllocation = true;
        state.allocationErrors.push(rejection);
        await tab.locator('#admin-phone-channels input[value="0:11"]').check();
        await tab.locator(phoneAcquire).click();
        await tab.waitForFunction(() => document.querySelector('[aria-busy="true"]'));
        const newRequestID = 'newer-admin-request-0002';
        await tab.evaluate(({ key, requestID }) => {
          localStorage.setItem(key, requestID);
          localStorage.setItem(`${key}.phone_choice`, JSON.stringify({ phone_country: '0', phone_provider_id: '12' }));
        }, { key: adminPhoneKey, requestID: newRequestID });
        state.releaseAllocation();
        await tab.waitForFunction(() => document.querySelector('#admin-resource-error-phone')?.textContent);
        assert.equal(await tab.evaluate(key => localStorage.getItem(key), adminPhoneKey), newRequestID);
        assert.match(await tab.locator('#admin-resource-phone').innerText(), /#12/);
        assert.equal(await tab.locator(phoneAcquire).innerText(), '确认上次申请');
        assert.deepEqual(state.errors, []);
      } finally { state.releaseAllocation?.(); await tab.close(); }
    }
  });

  test('another tab changing or clearing a pending purchase requires the updated action to be shown first', async () => {
    const { tab, state } = await fixture({ admin: true, resource: null });
    try {
      await tab.locator('#admin-phone-channels input[value="0:11"]').check();
      const otherRequestID = 'other-tab-request-0001';
      await tab.evaluate(({ key, requestID }) => {
        localStorage.setItem(key, requestID);
        localStorage.setItem(`${key}.phone_choice`, JSON.stringify({ phone_country: '0', phone_provider_id: '12' }));
      }, { key: adminPhoneKey, requestID: otherRequestID });
      await tab.locator(phoneAcquire).click();
      assert.equal(state.allocationRequests.length, 0, 'a purchase click must not silently confirm a different pending channel');
      assert.equal(await tab.locator(phoneAcquire).innerText(), '确认上次申请');
      assert.match(await tab.locator('#admin-resource-phone').innerText(), /#12/);
      await tab.evaluate(key => {
        localStorage.removeItem(key);
        localStorage.removeItem(`${key}.phone_choice`);
      }, adminPhoneKey);
      await tab.locator(phoneAcquire).click();
      assert.equal(state.allocationRequests.length, 0, 'a confirmation click must not silently become a new purchase after another tab resolves it');
      assert.equal(await tab.locator(phoneAcquire).innerText(), '获取手机号');
      await tab.locator('#admin-phone-channels input[value="0:12"]').check();
      state.allocationResult = order();
      await tab.locator(phoneAcquire).click();
      await tab.locator('[data-admin-copy-resource]').waitFor();
      assert.equal(state.allocationRequests.length, 1);
      assert.notEqual(state.allocationRequests[0].request_id, otherRequestID);
      assert.equal(state.allocationRequests[0].phone_provider_id, '12');
      assert.deepEqual(state.errors, []);
    } finally { await tab.close(); }
  });
}
