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

test('refresh clears selection during lookup and retains it only while the same route is available', async () => {
  const catalog = moduleAPI().catalog();
  await catalog.load(async () => ({ kind: 'phone', channels: [channel()] }), 'same-cdk');
  catalog.state.selected = catalog.state.channels[0];
  const waiting = deferred();
  const pending = catalog.load(() => waiting.promise, 'same-cdk');
  assert.equal(catalog.state.selected, null);
  waiting.resolve({ kind: 'phone', channels: [channel('0', '2368', { price: '0.13' })] });
  await pending;
  assert.equal(catalog.state.selected.price, '0.13');
  await catalog.load(async () => ({ kind: 'phone', channels: [channel('0', '2368', { count: 0 })] }), 'same-cdk');
  assert.equal(catalog.state.selected, null);
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
