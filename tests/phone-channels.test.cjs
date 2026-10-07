'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'phone-channels.js'), 'utf8');
const clone = value => JSON.parse(JSON.stringify(value));
function moduleAPI() { const window = {}; vm.runInNewContext(source, { window }); return window.PhoneChannels; }
const channel = (country = '0', provider_id = '2368', extra = {}) => ({ country, provider_id, country_name: '测试国家', price: '0.12', count: 10, tier: 'bronze', ...extra });
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

test('manual countries accept multiple separators, normalize codes, and preserve all-country semantics', () => {
  const api = moduleAPI();
  assert.equal(api.countryCodes(' 00，1,  01；187、16 '), '0,1,187,16');
  assert.equal(api.countryCodes(' * '), '*');
  assert.equal(api.countryCodes(''), '');
  for (const invalid of ['1,*', '-1', 'country=0', '0.5', '123456']) assert.throws(() => api.countryCodes(invalid));
});

test('catalog sorting does not invent quality tiers or accept malformed channel identifiers', () => {
  const api = moduleAPI();
  const values = api.normalizeChannels([channel(), channel(), channel('1', '2', { tier: 'premium', price: '0.05' }), channel('2', '3', { tier: 'gold' }), channel('0', 'bad'), channel('0', '4', { price: 'NaN' })]);
  assert.equal(values.length, 3);
  assert.equal(values[0].tier, 'unknown');
  assert.equal(values[2].tier, 'gold');
  assert.deepEqual(clone(api.selectionBody(values[0])), { phone_country: '1', phone_provider_id: '2' });
  assert.equal(Object.hasOwn(api.selectionBody(values[0]), 'price'), false);
});

test('a stale CDK response cannot replace a newer channel list', async () => {
  const catalog = moduleAPI().catalog();
  const first = deferred();
  const older = catalog.load(() => first.promise, 'first-cdk');
  await catalog.load(async () => ({ kind: 'phone', channels: [channel('1', '2')] }), 'second-cdk');
  first.resolve({ kind: 'phone', channels: [channel('0', '1')] });
  await older;
  assert.equal(catalog.state.key, 'second-cdk');
  assert.equal(catalog.state.channels[0].provider_id, '2');
  assert.equal(catalog.state.selected, null);
});

test('manual refresh clears selection if the route price changes or its stock disappears', async () => {
  const catalog = moduleAPI().catalog();
  await catalog.load(async () => ({ kind: 'phone', channels: [channel()] }), 'same-cdk');
  catalog.state.selected = catalog.state.channels[0];
  const waiting = deferred();
  const pending = catalog.load(() => waiting.promise, 'same-cdk');
  assert.equal(catalog.state.selected, null);
  waiting.resolve({ kind: 'phone', channels: [channel('0', '2368', { price: '0.13' })] });
  await pending;
  assert.equal(catalog.state.selected, null);
  catalog.state.selected = catalog.state.channels[0];
  await catalog.load(async () => ({ kind: 'phone', channels: [channel('0', '2368', { count: 0 })] }), 'same-cdk');
  assert.equal(catalog.state.selected, null);
});

test('background refresh preserves a stable view and selection, then drops a changed price', async () => {
  let changed = 0;
  let next = { kind: 'phone', channels: [channel()] };
  const catalog = moduleAPI().catalog({ onChange: () => changed++ });
  await catalog.load(async () => next, 'same-cdk');
  catalog.state.selected = catalog.state.channels[0];
  const baseline = changed;
  await catalog.refresh();
  assert.equal(changed, baseline, 'unchanged background responses do not rerender or notify');
  assert.equal(catalog.state.selected.price, '0.12');
  next = { kind: 'phone', channels: [channel('0', '2368', { price: '0.13' })] };
  const waiting = catalog.refresh();
  assert.equal(catalog.state.loading, false);
  assert.equal(catalog.state.refreshing, true);
  assert.equal(catalog.state.channels.length, 1, 'do not flash an empty list while fetching');
  await waiting;
  assert.equal(catalog.state.selected, null);
  assert.equal(changed, baseline + 1);
});

test('live policy refresh removes old routes and later restores channels without choosing or buying them', async () => {
  let next = { kind: 'phone', countries: '0', max_price: '0.20', channels: [channel()] };
  const catalog = moduleAPI().catalog();
  await catalog.load(async () => next, 'old-cdk');
  catalog.state.selected = catalog.state.channels[0];
  next = { kind: 'phone', countries: '1', max_price: '0.10', channels: [channel('1', '9', { price: '0.09' })] };
  await catalog.refresh();
  assert.equal(catalog.state.selected, null);
  assert.equal(catalog.state.result.countries, '1');
  assert.equal(catalog.state.channels[0].provider_id, '9');
  next = { ...next, channels: [...next.channels, channel('1', '10', { price: '0.08' })] };
  await catalog.refresh();
  assert.equal(catalog.state.channels.length, 2);
  assert.equal(catalog.state.selected, null);
});

test('background responses cannot change a purchasing form or overwrite a newer CDK', async () => {
  let mayRefresh = true;
  let next = { kind: 'phone', channels: [channel()] };
  const catalog = moduleAPI().catalog({ canRefresh: () => mayRefresh });
  await catalog.load(async () => next, 'old-cdk');
  const waiting = deferred();
  next = waiting.promise;
  const refreshing = catalog.refresh();
  mayRefresh = false;
  waiting.resolve({ kind: 'phone', channels: [] });
  await refreshing;
  assert.equal(catalog.state.channels.length, 1);
  mayRefresh = true;
  const later = deferred(); next = later.promise;
  const stale = catalog.refresh();
  catalog.invalidate();
  await catalog.load(async () => ({ kind: 'phone', channels: [channel('2', '3')] }), 'new-cdk');
  later.resolve({ kind: 'phone', channels: [] });
  await stale;
  assert.equal(catalog.state.key, 'new-cdk');
  assert.equal(catalog.state.channels[0].provider_id, '3');
});

test('new CDKs never inherit selections and lookup failures remove stale purchase choices', async () => {
  const catalog = moduleAPI().catalog();
  await catalog.load(async () => ({ kind: 'phone', channels: [channel()] }), 'one');
  catalog.state.selected = catalog.state.channels[0];
  await catalog.load(async () => ({ kind: 'phone', channels: [channel()] }), 'two');
  assert.equal(catalog.state.selected, null);
  catalog.state.selected = catalog.state.channels[0];
  await catalog.load(async () => { throw new Error('temporary upstream failure'); }, 'two');
  assert.equal(catalog.state.loaded, false);
  assert.equal(catalog.state.selected, null);
  assert.equal(catalog.state.error, 'temporary upstream failure');
});

test('changing inputs invalidates in-flight results and active orders may resume without choosing a new route', async () => {
  const catalog = moduleAPI().catalog();
  const waiting = deferred();
  const pending = catalog.load(() => waiting.promise, 'one');
  catalog.invalidate();
  waiting.resolve({ kind: 'phone', channels: [channel()] });
  await pending;
  assert.equal(catalog.state.loaded, false);
  assert.equal(catalog.state.channels.length, 0);
  await catalog.load(async () => ({ kind: 'phone', resume: true, channels: [] }), 'active');
  assert.equal(catalog.state.result.resume, true);
  assert.equal(catalog.state.selected, null);
});
