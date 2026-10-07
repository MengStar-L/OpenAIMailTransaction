'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test, before, after } = require('node:test');

// Browser integration checks are optional locally/CI; expose an installed
// Playwright through NODE_PATH to run them. No browser dependency ships in Go.
let chromium;
try { ({ chromium } = require('playwright')); } catch { /* Optional test runtime. */ }

if (!chromium) {
  test('select menu browser integration (set NODE_PATH to Playwright)', { skip: true }, () => {});
} else {
  const script = fs.readFileSync(path.join(__dirname, '../web/selects.js'), 'utf8');
  const css = fs.readFileSync(path.join(__dirname, '../web/selects.css'), 'utf8');
  const baseCSS = fs.readFileSync(path.join(__dirname, '../web/styles.css'), 'utf8');
  let browser;
  before(async () => { browser = await chromium.launch({ headless: true }); });
  after(async () => { await browser?.close(); });

  async function page(html, viewport = { width: 900, height: 700 }) {
    const tab = await browser.newPage({ viewport });
    await tab.setContent(`<style>${baseCSS}\n${css}</style><main style="padding:30px;max-width:650px">${html}</main>`);
    await tab.addScriptTag({ content: script });
    return tab;
  }
  const choices = '<option value="a">Alpha</option><option value="b" disabled>Beta</option><option value="c">Charlie</option><option value="h" hidden>Hidden</option><option value="d">Delta</option>';

  test('selection commits once through native form events; Escape leaves the value unchanged', async () => {
    const tab = await page(`<form><label for="kind">类型</label><select id="kind" name="kind">${choices}</select><button id="after">Next</button></form>`);
    try {
      await tab.evaluate(() => {
        window.events = [];
        for (const type of ['input', 'change']) document.querySelector('select').addEventListener(type, event => window.events.push([event.type, event.target.value]));
      });
      const trigger = tab.getByRole('combobox', { name: '类型' });
      await trigger.focus();
      await trigger.press('ArrowDown');
      await trigger.press('ArrowDown');
      await trigger.press('Escape');
      assert.equal(await tab.locator('select').inputValue(), 'a');
      assert.deepEqual(await tab.evaluate(() => window.events), []);
      await trigger.press('ArrowDown');
      await trigger.press('ArrowDown');
      await trigger.press('Enter');
      assert.equal(await tab.locator('select').inputValue(), 'c');
      assert.equal(await tab.evaluate(() => new FormData(document.querySelector('form')).get('kind')), 'c');
      assert.deepEqual(await tab.evaluate(() => window.events), [['input', 'c'], ['change', 'c']]);
      await trigger.click();
      await trigger.press('End');
      await trigger.press('Home');
      await trigger.press('Tab');
      assert.equal(await tab.locator('.sg-select-menu').count(), 0);
      assert.equal(await tab.evaluate(() => document.activeElement.id), 'after');
    } finally { await tab.close(); }
  });

  test('typeahead supports prefixes and cycling repeated initial letters', async () => {
    const tab = await page('<label for="country">国家</label><select id="country"><option>Japan</option><option>China</option><option>Chile</option><option>Canada</option></select>');
    try {
      const trigger = tab.getByRole('combobox');
      await trigger.focus();
      await trigger.press('c');
      await trigger.press('a');
      await trigger.press('Enter');
      assert.equal(await tab.locator('select').inputValue(), 'Canada');
      await trigger.click();
      await trigger.press('c');
      await trigger.press('c');
      await trigger.press('Enter');
      assert.equal(await tab.locator('select').inputValue(), 'Chile');
    } finally { await tab.close(); }
  });

  test('option mutations, disabled fieldsets, programmatic values and form reset stay in sync', async () => {
    const tab = await page(`<form><fieldset><label for="kind">类型</label><select id="kind">${choices}</select></fieldset></form>`);
    try {
      await tab.evaluate(() => { document.querySelector('select').value = 'd'; window.ShiguangSelects.refresh(); });
      assert.match(await tab.getByRole('combobox').innerText(), /Delta/);
      await tab.evaluate(() => document.querySelector('select').options[4].textContent = 'Dawn');
      await tab.waitForFunction(() => document.querySelector('.sg-select-value').textContent === 'Dawn');
      await tab.evaluate(() => document.querySelector('fieldset').disabled = true);
      await tab.waitForFunction(() => document.querySelector('.sg-select-trigger').disabled);
      await tab.evaluate(() => { document.querySelector('fieldset').disabled = false; document.querySelector('form').reset(); });
      await tab.waitForFunction(() => document.querySelector('.sg-select-value').textContent === 'Alpha');
      assert.equal(await tab.getByRole('combobox').isDisabled(), false);
    } finally { await tab.close(); }
  });

  test('multiple selection retains native values and cannot choose a disabled option', async () => {
    const tab = await page(`<label for="countries">国家</label><select id="countries" name="countries" multiple>${choices}</select>`);
    try {
      await tab.getByRole('combobox').click();
      await tab.getByRole('option', { name: 'Alpha' }).click();
      await tab.getByRole('option', { name: 'Charlie' }).click();
      // Clicking a disabled option is deliberately ignored by the component.
      await tab.getByRole('option', { name: 'Beta' }).dispatchEvent('click');
      assert.deepEqual(await tab.locator('select').evaluate(element => Array.from(element.selectedOptions, option => option.value)), ['a', 'c']);
      assert.equal(await tab.getByRole('combobox').getAttribute('aria-expanded'), 'true');
      assert.equal(await tab.getByRole('listbox').getAttribute('aria-multiselectable'), 'true');
      assert.equal(await tab.getByRole('option', { name: 'Hidden' }).count(), 0);
    } finally { await tab.close(); }
  });

  test('label and native validation direct focus to the visible control', async () => {
    const tab = await page('<form><label for="resource">资源</label><select id="resource" required><option value="">请选择</option><option value="mail">邮箱</option></select><button>保存</button></form>');
    try {
      await tab.locator('label').click();
      assert.equal(await tab.evaluate(() => document.activeElement.className), 'sg-select-trigger');
      await tab.getByText('保存', { exact: true }).click();
      assert.equal(await tab.evaluate(() => document.activeElement.className), 'sg-select-trigger');
      assert.equal(await tab.getByRole('combobox').getAttribute('aria-invalid'), 'true');
      await tab.getByRole('combobox').click();
      await tab.getByRole('option', { name: '邮箱' }).click();
      assert.equal(await tab.locator('form').evaluate(form => form.checkValidity()), true);
      assert.equal(await tab.getByRole('combobox').getAttribute('aria-invalid'), null);
    } finally { await tab.close(); }
  });

  test('modal menu enters top layer, fits a narrow screen and Escape keeps its dialog open', async () => {
    const tab = await page(`<dialog><label for="modal-select">背景</label><select id="modal-select">${Array.from({ length: 60 }, (_, index) => `<option value="${index}">渠道 ${index} · 很长的渠道名称</option>`).join('')}</select></dialog>`, { width: 320, height: 560 });
    try {
      await tab.locator('dialog').evaluate(dialog => dialog.showModal());
      await tab.getByRole('combobox').click();
      await tab.getByRole('combobox').press('End');
      const bounds = await tab.getByRole('listbox').boundingBox();
      assert.ok(bounds.x >= 0 && bounds.x + bounds.width <= 320);
      assert.ok(bounds.y >= 0 && bounds.y + bounds.height <= 560);
      assert.equal(await tab.getByRole('listbox').evaluate(list => list.matches(':popover-open')), true);
      assert.ok(await tab.getByRole('listbox').evaluate(list => list.scrollTop > 0));
      await tab.getByRole('combobox').press('Escape');
      assert.equal(await tab.locator('dialog').evaluate(dialog => dialog.open), true);
      assert.equal(await tab.locator('.sg-select-menu').count(), 0);
    } finally { await tab.close(); }
  });

  test('repeated page replacement cleans detached popovers without duplicating controls or observer loops', async () => {
    const tab = await page(`<div id="dynamic"><select aria-label="筛选">${choices}</select></div>`);
    try {
      for (let index = 0; index < 25; index++) {
        await tab.getByRole('combobox').click();
        await tab.locator('#dynamic').evaluate((container, options) => { container.innerHTML = `<select aria-label="筛选">${options}</select>`; }, choices);
        await tab.waitForFunction(() => document.querySelectorAll('.sg-select-trigger').length === 1 && document.querySelectorAll('.sg-select-menu').length === 0);
      }
      const stable = await tab.evaluate(async () => {
        let changes = 0;
        const observer = new MutationObserver(records => { changes += records.length; });
        observer.observe(document.body, { attributes: true, subtree: true, childList: true });
        await new Promise(resolve => setTimeout(resolve, 75));
        observer.disconnect();
        return changes;
      });
      assert.equal(stable, 0);
      await tab.getByRole('combobox').click();
      await tab.locator('main').click({ position: { x: 2, y: 2 } });
      assert.equal(await tab.locator('.sg-select-menu').count(), 0);
    } finally { await tab.close(); }
  });
}
