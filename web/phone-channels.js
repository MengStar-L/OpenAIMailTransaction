/* Country choices and upstream phone channels. No allocation requests are made here. */
'use strict';
(() => {
  const escape = value => String(value ?? '').replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[character]);
  const tiers = { bronze: '星芽', silver: '月辉', gold: '日冕' };
  let sequence = 0;
  const keyFor = channel => `${channel.country}:${channel.provider_id}`;
  function countryCodes(value) {
    const source = String(value ?? '').trim();
    if (source === '*') return '*';
    if (!source) return '';
    const values = source.split(/[\s,，、;；]+/).filter(Boolean);
    if (values.some(code => !/^\d{1,5}$/.test(code))) throw new Error('国家代码请用逗号分隔，或填写 * 选择全部');
    return Array.from(new Set(values.map(code => String(Number(code))))).join(',');
  }
  function normalizeChannels(channels) {
    const seen = new Set();
    return (Array.isArray(channels) ? channels : []).filter(channel => {
      if (!channel || !/^\d+$/.test(String(channel.country)) || !/^\d+$/.test(String(channel.provider_id)) || !Number.isFinite(Number(channel.price)) || Number(channel.price) < 0) return false;
      const key = keyFor(channel);
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    }).map(channel => ({ ...channel, country: String(channel.country), provider_id: String(channel.provider_id), tier: Object.hasOwn(tiers, channel.tier) ? channel.tier : 'unknown' })).sort((left, right) => Number(left.price) - Number(right.price) || left.country.localeCompare(right.country) || left.provider_id.localeCompare(right.provider_id));
  }
  function selectionBody(channel) { return channel ? { phone_country: String(channel.country), phone_provider_id: String(channel.provider_id) } : {}; }
  const refreshIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5m-4.4-4a8 8 0 0 1 13.2-3L20 8M4 16l2.2 3A8 8 0 0 0 19.4 16"/></svg>';

  function catalog(options = {}) {
    const id = `phone-catalog-${++sequence}`;
    let host = null;
    let request = 0;
    let reload = null;
    let cachedLoader = null;
    let filter = '';
    let page = 1;
    const size = 30;
    const state = { channels: [], selected: null, loading: false, refreshing: false, loaded: false, error: '', result: null, key: '' };
    const notify = () => options.onChange?.(state);
    const selectable = options.selectable !== false;
    const matchingChoice = previous => previous ? state.channels.find(channel => keyFor(channel) === keyFor(previous) && Number(channel.price) === Number(previous.price) && Number(channel.count) !== 0) || null : null;
    function repaint() {
      if (!host?.isConnected) return;
      const focused = host.contains(document.activeElement) ? document.activeElement : null;
      const searchFocused = focused?.matches('.phone-channel-search');
      const selection = searchFocused ? [focused.selectionStart, focused.selectionEnd, focused.selectionDirection] : null;
      const radioValue = focused?.matches('input[type="radio"]') ? focused.value : null;
      const scrollTop = host.querySelector('.phone-catalog-list')?.scrollTop || 0;
      status();
      if (searchFocused) {
        const search = host.querySelector('.phone-channel-search');
        search?.focus({ preventScroll: true }); search?.setSelectionRange(...selection);
      } else if (radioValue != null) {
        const choices = Array.from(host.querySelectorAll('input[type="radio"]'));
        (choices.find(input => input.value === radioValue) || choices[0])?.focus({ preventScroll: true });
      }
      const list = host.querySelector('.phone-catalog-list');
      if (list) list.scrollTop = scrollTop;
    }
    function status(message = '') {
      if (!host?.isConnected) return;
      host.hidden = state.result?.kind === 'email';
      const selected = state.selected;
      host.innerHTML = `<section class="phone-catalog${selectable ? '' : ' phone-catalog-preview'}" aria-label="手机号渠道" aria-busy="${state.loading}"><div class="phone-catalog-header"><div><span>可用渠道</span><span class="phone-channel-total">${state.loaded ? state.channels.length : '—'}</span></div><button class="icon-button phone-channel-refresh" type="button" title="刷新渠道" aria-label="刷新渠道" ${state.loading || !reload ? 'disabled' : ''}>${refreshIcon}</button></div>${state.channels.length > 8 ? `<input class="phone-channel-search" type="search" value="${escape(filter)}" placeholder="搜索国家 / 渠道" aria-label="搜索国家或渠道">` : ''}<div class="phone-catalog-list"></div><div class="phone-catalog-message${state.error ? ' error-text' : ''}" role="status" aria-live="polite">${escape(state.loading ? '正在查找渠道…' : state.error || message || (state.result?.resume ? '可继续当前订单' : !state.loaded ? '选择国家并设置价格上限' : !state.channels.length ? '暂无符合价格的可用渠道' : ''))}</div><div class="phone-channel-pages"></div>${selected ? `<div class="phone-selected">已选 ${escape(selected.country_name || selected.country)} · #${escape(selected.provider_id)}</div>` : ''}</section>`;
      host.querySelector('.phone-channel-refresh').addEventListener('click', () => reload?.());
      host.querySelector('.phone-channel-search')?.addEventListener('input', event => { filter = event.target.value; page = 1; rows(); });
      rows();
    }
    function rows() {
      if (!host?.isConnected) return;
      const target = host.querySelector('.phone-catalog-list');
      if (!target) return;
      const search = filter.trim().toLocaleLowerCase();
      const filtered = state.channels.filter(channel => !search || `${channel.country_name || ''} ${channel.country} ${channel.provider_id} ${tiers[channel.tier] || '未评级'}`.toLocaleLowerCase().includes(search));
      const pages = Math.max(1, Math.ceil(filtered.length / size));
      page = Math.min(page, pages);
      const items = filtered.slice((page - 1) * size, page * size);
      target.innerHTML = items.map(channel => {
        const chosen = state.selected && keyFor(state.selected) === keyFor(channel);
        const count = channel.count == null || channel.count === '' ? '余量未知' : Number(channel.count) === -1 || channel.count === 'few' ? '参考少量' : Number.isFinite(Number(channel.count)) ? `参考余量 ${new Intl.NumberFormat('zh-CN').format(Math.max(0, Number(channel.count)))}` : '余量未知';
        const noStock = channel.count != null && channel.count !== '' && Number(channel.count) === 0;
        return `<${selectable ? 'label' : 'div'} class="phone-channel-row${chosen ? ' selected' : ''}${noStock ? ' unavailable' : ''}">${selectable ? `<input type="radio" name="${id}" value="${escape(keyFor(channel))}" ${chosen ? 'checked' : ''} ${state.loading || noStock ? 'disabled' : ''} aria-label="${escape(channel.country_name || channel.country)}，渠道 ${escape(channel.provider_id)}，${escape(tiers[channel.tier] || '未评级')}，${escape(channel.price)} 美元">` : ''}<span class="phone-channel-country"><strong>${escape(channel.country_name || `国家 ${channel.country}`)}</strong><small>${escape(channel.country)} · #${escape(channel.provider_id)}</small></span><span class="phone-tier ${channel.tier}"><span aria-hidden="true">${channel.tier === 'gold' ? '☀' : channel.tier === 'silver' ? '☾' : channel.tier === 'bronze' ? '✧' : '·'}</span>${tiers[channel.tier] || '未评级'}</span><span class="phone-channel-cost"><strong>$${escape(channel.price)}</strong><small>${escape(count)}</small></span></${selectable ? 'label' : 'div'}>`;
      }).join('');
      if (state.loaded && state.channels.length && !items.length) target.innerHTML = '<div class="phone-channel-empty">没有匹配的渠道</div>';
      target.querySelectorAll('input[type="radio"]').forEach(input => input.addEventListener('change', () => {
        if (state.loading || !input.checked) return;
        state.selected = state.channels.find(channel => keyFor(channel) === input.value) || null;
        target.querySelectorAll('.phone-channel-row').forEach(row => row.classList.toggle('selected', row.querySelector('input')?.checked === true));
        let summary = host.querySelector('.phone-selected');
        if (!summary) { summary = document.createElement('div'); summary.className = 'phone-selected'; host.querySelector('.phone-catalog').append(summary); }
        summary.textContent = `已选 ${state.selected.country_name || state.selected.country} · #${state.selected.provider_id}`;
        notify();
      }));
      const pagination = host.querySelector('.phone-channel-pages');
      pagination.innerHTML = pages > 1 ? `<button type="button" class="text-button" data-page="prev" ${page === 1 ? 'disabled' : ''} aria-label="上一页渠道">←</button><span>${page} / ${pages} · ${filtered.length} 个</span><button type="button" class="text-button" data-page="next" ${page === pages ? 'disabled' : ''} aria-label="下一页渠道">→</button>` : '';
      pagination.querySelectorAll('[data-page]').forEach(button => button.addEventListener('click', () => { page += button.dataset.page === 'prev' ? -1 : 1; rows(); host.querySelector('.phone-catalog-list')?.scrollTo?.(0, 0); host.querySelector('.phone-channel-search')?.focus({ preventScroll: true }); }));
    }
    return {
      state,
      mount(element) { host = element; status(); },
      invalidate(message = '') { ++request; state.channels = []; state.selected = null; state.loading = false; state.refreshing = false; state.loaded = false; state.error = ''; state.result = null; state.key = ''; filter = ''; page = 1; reload = null; cachedLoader = null; status(message); notify(); },
      async load(loader, key = '') {
        const epoch = ++request;
        cachedLoader = loader;
        state.refreshing = false;
        reload = () => this.load(loader, key);
        const previous = key === state.key ? state.selected : null;
        state.key = key; state.loading = true; state.loaded = false; state.error = ''; state.channels = []; state.selected = null; state.result = null;
        status(); notify();
        try {
          const result = await loader();
          if (epoch !== request) return null;
          state.result = result; state.channels = normalizeChannels(result.channels); state.loaded = true;
          state.selected = matchingChoice(previous);
          return result;
        } catch (error) {
          if (epoch === request) state.error = error.message || '渠道查询失败，请重试';
          return null;
        } finally { if (epoch === request) { state.loading = false; status(); notify(); } }
      },
      async refresh() {
        if (!cachedLoader || state.loading || state.refreshing || options.canRefresh?.() === false) return null;
        const epoch = ++request;
        state.refreshing = true;
        try {
          const result = await cachedLoader();
          if (epoch !== request || options.canRefresh?.() === false) return null;
          const before = JSON.stringify([state.result, state.channels, state.selected, state.error]);
          const previous = state.selected;
          state.result = result; state.channels = normalizeChannels(result.channels); state.loaded = true; state.error = '';
          state.selected = matchingChoice(previous);
          if (before !== JSON.stringify([state.result, state.channels, state.selected, state.error])) { repaint(); notify(); }
          return result;
        } catch (error) {
          if (epoch === request && options.canRefresh?.() !== false) {
            state.channels = []; state.selected = null; state.loaded = false; state.error = error.message || '渠道查询失败，请重试';
            repaint(); notify();
          }
          return null;
        } finally { if (epoch === request) state.refreshing = false; }
      },
      selectedBody() { return selectionBody(state.selected); },
      focus() { host?.querySelector('input[type="radio"]:not(:disabled), .phone-channel-refresh')?.focus(); },
    };
  }

  function countryEditor(root, { api }) {
    const input = root.querySelector('[name="phone_country"]');
    const panel = root.querySelector('[data-country-panel]');
    const search = root.querySelector('[data-country-search]');
    const list = root.querySelector('[data-country-list]');
    const all = root.querySelector('[data-country-all]');
    const open = root.querySelector('[data-country-open]');
    const info = root.querySelector('[data-country-info]');
    let countries = [];
    let loading = false;
    let loaded = false;
    function selected() { try { const value = countryCodes(input.value); return value === '*' ? '*' : new Set(value.split(',').filter(Boolean)); } catch { return new Set(); } }
    function paint() {
      if (!root.isConnected) return;
      const focusedCountry = list.contains(document.activeElement) ? document.activeElement.value : null;
      const choice = selected();
      all.setAttribute('aria-pressed', String(choice === '*'));
      const term = search.value.trim().toLocaleLowerCase();
      const matches = countries.filter(country => `${country.name} ${country.id}`.toLocaleLowerCase().includes(term));
      list.innerHTML = matches.slice(0, 80).map(country => `<label class="phone-country-option"><input type="checkbox" value="${escape(country.id)}" ${choice === '*' || choice.has(String(country.id)) ? 'checked' : ''}><span>${escape(country.name)}</span><small>${escape(country.id)}</small></label>`).join('');
      if (loaded && !matches.length) list.innerHTML = '<span class="phone-channel-empty">没有匹配的国家</span>';
      if (loaded) info.textContent = matches.length > 80 ? `显示前 80 个，共 ${matches.length} 个；可继续搜索` : `${matches.length} 个国家`;
      list.querySelectorAll('input').forEach(checkbox => checkbox.addEventListener('change', () => {
        const current = selected();
        const next = current === '*' ? new Set(countries.map(country => String(country.id))) : current;
        if (checkbox.checked) next.add(checkbox.value); else next.delete(checkbox.value);
        input.value = Array.from(next).join(',');
        input.setCustomValidity('');
        input.dispatchEvent(new Event('input', { bubbles: true }));
      }));
      if (focusedCountry != null) Array.from(list.querySelectorAll('input')).find(checkbox => checkbox.value === focusedCountry)?.focus({ preventScroll: true });
    }
    async function load() {
      if (loading || loaded) return;
      loading = true; info.textContent = '正在获取国家…';
      try {
        const result = await api('/api/admin/phone/countries');
        if (!root.isConnected) return;
        countries = (Array.isArray(result.countries) ? result.countries : []).filter(country => /^\d+$/.test(String(country.id))).map(country => ({ id: String(country.id), name: String(country.name || country.id) }));
        loaded = true; paint();
      } catch (error) { if (root.isConnected) info.textContent = error.message; }
      finally { loading = false; }
    }
    open.addEventListener('click', () => { panel.hidden = !panel.hidden; open.setAttribute('aria-expanded', String(!panel.hidden)); if (!panel.hidden) { load(); search.focus(); } });
    search.addEventListener('input', paint);
    search.addEventListener('keydown', event => { if (event.key === 'Enter') event.preventDefault(); if (event.key === 'Escape') { panel.hidden = true; open.setAttribute('aria-expanded', 'false'); open.focus(); } });
    all.addEventListener('click', () => { input.value = input.value.trim() === '*' ? '' : '*'; input.setCustomValidity(''); input.dispatchEvent(new Event('input', { bubbles: true })); });
    input.addEventListener('input', () => { input.setCustomValidity(''); paint(); });
    input.addEventListener('blur', () => { try { input.value = countryCodes(input.value); input.setCustomValidity(''); paint(); } catch (error) { input.setCustomValidity(error.message); } });
    input.form?.addEventListener('submit', event => { try { input.value = countryCodes(input.value); } catch (error) { event.preventDefault(); event.stopImmediatePropagation(); input.setCustomValidity(error.message); input.reportValidity(); } }, true);
    paint();
  }
  window.PhoneChannels = { catalog, countryEditor, countryCodes, normalizeChannels, selectionBody };
})();
