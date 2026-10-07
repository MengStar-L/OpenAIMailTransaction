/* Shiguang — a small place for incoming messages. */
'use strict';
(() => {
  const app = document.getElementById('app');
  const dialog = document.getElementById('dialog');
  const TOKEN_KEY = 'atelier.resource.token';
  const mailAlerts = window.MailArrivalAlerts;
  const icons = {
    arrow: '<path d="M5 12h14M13 6l6 6-6 6"/>',
    back: '<path d="M19 12H5m6-6-6 6 6 6"/>',
    chevron: '<path d="m9 5 7 7-7 7"/>',
    phone: '<rect x="6.5" y="2.5" width="11" height="19" rx="3.5"/><path d="M10 5.5h4m-3 13h2"/><path d="m19 3 .6 1.4L21 5l-1.4.6L19 7l-.6-1.4L17 5l1.4-.6Z"/>',
    email: '<rect x="2.5" y="5" width="19" height="15" rx="4"/><path d="m3.5 7 7 5.5a2.4 2.4 0 0 0 3 0l7-5.5M4 18l5-5m11 5-5-5"/>',
    grid: '<rect x="3" y="3" width="7" height="7" rx="2.5"/><rect x="14" y="14" width="7" height="7" rx="2.5"/><rect x="3" y="14" width="7" height="7" rx="2.5"/><path d="m17.5 2 1.2 3.3L22 6.5l-3.3 1.2L17.5 11l-1.2-3.3L13 6.5l3.3-1.2Z"/>',
    ticket: '<path d="M5 5h14a2 2 0 0 1 2 2v3a2 2 0 0 0 0 4v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-3a2 2 0 0 0 0-4V7a2 2 0 0 1 2-2Z"/><path d="M9 5v2m0 4v2m0 4v2m6-10 .8 2.2L18 12l-2.2.8L15 15l-.8-2.2L12 12l2.2-.8Z"/>',
    settings: '<path d="M5 4v4m0 5v7M12 4v9m0 5v2m7-16v2m0 5v9"/><rect x="2.5" y="8" width="5" height="5" rx="2"/><rect x="9.5" y="13" width="5" height="5" rx="2"/><rect x="16.5" y="6" width="5" height="5" rx="2"/>',
    lock: '<rect x="5" y="10" width="14" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3m-4 5v2"/>',
    logout: '<path d="M10 4H5a1 1 0 0 0-1 1v14a1 1 0 0 0 1 1h5m5-4 4-4-4-4m-7 4h11"/>',
    external: '<path d="M13 4h7v7m0-7L10 14m-1-9H5a1 1 0 0 0-1 1v13a1 1 0 0 0 1 1h13a1 1 0 0 0 1-1v-4"/>',
    copy: '<rect x="8" y="8" width="12" height="13" rx="2"/><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"/>',
    check: '<path d="m5 12 4 4L19 6"/>',
    clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
    close: '<path d="m6 6 12 12M6 18 18 6"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 5 5"/>',
    download: '<path d="M12 3v12m-5-5 5 5 5-5M4 15v5h16v-5"/>',
    refresh: '<path d="M20 7v5h-5M4 17v-5h5m-4.4-4a8 8 0 0 1 13.2-3L20 8M4 16l2.2 3A8 8 0 0 0 19.4 16"/>',
    shield: '<path d="M12 3 4 6v6c0 5 8 9 8 9s8-4 8-9V6z"/><path d="m8 12 3 3 5-6"/>',
    inbox: '<path d="M7 5h10a2 2 0 0 1 1.8 1.1L22 13v5a3 3 0 0 1-3 3H5a3 3 0 0 1-3-3v-5l3.2-6.9A2 2 0 0 1 7 5Z"/><path d="M2 13h6l1.5 3h5l1.5-3h6m-10-11v6m-2-2 2 2 2-2"/>',
    image: '<rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8" cy="8" r="1.5"/><path d="m21 15-6-6L3 21"/>',
    star: '<path d="m12 2 2.6 7.4L22 12l-7.4 2.6L12 22l-2.6-7.4L2 12l7.4-2.6Z"/>',
    moon: '<path d="M20.5 13A8.5 8.5 0 0 1 11 3.5 8.5 8.5 0 1 0 20.5 13Z"/>',
    bell: '<path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/>',
    sound: '<path d="m11 4-6 5H2v6h3l6 5V4Zm4 4a6 6 0 0 1 0 8m3-11a10 10 0 0 1 0 14"/>',
    tab: '<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M3 9h18M7 6.5h1"/>',
  };
  const statusNames = { queued: '排队中', allocating: '分配中', waiting: '等待接码', received: '已收码', next_pending: '接码状态待确认', next_uncertain: '接码状态待确认', completed: '已完成', complete_pending: '完成确认中', cancel_pending: '取消确认中', cancelled: '已取消', expired: '已过期', review: '待核查', failed: '分配失败', available: '可兑换', active: '使用中', used: '已使用（旧记录）', disabled: '已停用', exhausted: '额度已用完' };
  let config = { brand: '拾光 · Atelier', mode: 'demo', configured: false, phone_enabled: true, email_enabled: true };
  let token = readToken();
  let currentOrder = null;
  let serverOffset = 0;
  let pollTimer = null;
  let timerInterval = null;
  let toastTimer = null;
  let requestBusy = false;
  let pollBusy = false;
  let orderEpoch = 0;
  let isAdmin = location.pathname.replace(/\/$/, '') === '/admin';
  let adminTab = new URLSearchParams(location.search).get('tab') || 'overview';
  let adminGeneration = 0;
  let listState = { query: '', status: '', page: 1 };
  let generatedCodes = [];
  let generatedBatch = '';
  let generatedExportURL = '';
  let inventory = { count: null, message: '正在查询库存', allocationStatus: 'unconfirmed' };
  let inventoryTimer = null;
  let inventoryBusy = false;
  let inventoryRefreshPending = false;
  let inventoryGeneration = 0;
  let adminResourcesTimer = null;
  let adminResourcesClock = null;
  const adminResources = { phone: null, email: null };
  const adminResourceBusy = { phone: false, email: false };
  const adminResourcePolling = { phone: false, email: false };
  const adminResourceEpoch = { phone: 0, email: 0 };
  const adminResourceRequests = { phone: '', email: '' };
  const adminResourceRequestMinutes = { phone: null, email: null };
  const adminResourceErrors = { phone: '', email: '' };
  let queueMinutesPreference = readQueuePreference();
  const queueDrafts = new Map();
  const displayedCodes = new Map();

  // Pointer navigation stays clean; keyboard users retain an inset focus cue.
  const focusRoot = document.documentElement;
  focusRoot.dataset.input = 'pointer';
  document.addEventListener('pointerdown', () => { focusRoot.dataset.input = 'pointer'; }, true);
  document.addEventListener('keydown', event => {
    if (!event.altKey && !event.ctrlKey && !event.metaKey && ['Tab', 'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Home', 'End', 'Enter', ' '].includes(event.key)) {
      focusRoot.dataset.input = 'keyboard';
    }
  }, true);
  window.addEventListener('blur', () => { focusRoot.classList.add('window-blurred'); });
  window.addEventListener('focus', () => { focusRoot.classList.remove('window-blurred'); });

  function icon(name, cls = '') { return `<svg class="${cls}" viewBox="0 0 24 24" aria-hidden="true">${icons[name] || icons.inbox}</svg>`; }
  function esc(value) { return String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]); }
  function byId(id) { return document.getElementById(id); }
  function readQueuePreference() { try { const value = Number(localStorage.getItem('shiguang.queue.minutes')); return Number.isInteger(value) && value >= 1 && value <= 1440 ? value : 10; } catch { return 10; } }
  function saveQueuePreference(value) { queueMinutesPreference = value; try { localStorage.setItem('shiguang.queue.minutes', String(value)); } catch { /* Preferences remain available for this page. */ } }
  function queuePreferenceHTML(id, value = queueMinutesPreference, adjustable = false, orderID = '') {
    const draftKey = adjustable ? `${id}:${orderID}` : '';
    const shownValue = queueDrafts.has(draftKey) ? queueDrafts.get(draftKey) : Number(value) || 10;
    return `<div class="queue-preferences${adjustable ? ' queue-inline' : ''}"><label class="queue-preference-label" for="${id}">${icon('clock')}排队等待</label><div class="queue-duration-control"><input class="queue-duration-input" id="${id}" data-queue-draft="${esc(draftKey)}" type="number" inputmode="numeric" min="${Number(config.queue_min_minutes) || 1}" max="${Number(config.queue_max_minutes) || 1440}" step="1" value="${esc(shownValue)}" required aria-label="排队等待分钟数" title="1–1440 分钟，从本次申请开始计时"><span class="queue-duration-unit">分钟</span>${adjustable ? '<button class="queue-save-button text-button" type="button" data-save-queue>调整</button>' : ''}</div></div>`;
  }
  function queuePreferenceValue(id) {
    const input = byId(id);
    if (!input) return queueMinutesPreference;
    const value = Number(input.value);
    if (!Number.isInteger(value) || value < (Number(config.queue_min_minutes) || 1) || value > (Number(config.queue_max_minutes) || 1440)) { input.reportValidity(); input.focus(); throw new Error('排队时间需为 1–1440 分钟。'); }
    saveQueuePreference(value);
    return value;
  }
  function bindQueuePreference(id, onSave) {
    const input = byId(id);
    if (!input) return;
    input.addEventListener('input', () => { if (input.dataset.queueDraft) queueDrafts.set(input.dataset.queueDraft, input.value); const value = Number(input.value); if (input.validity.valid && Number.isInteger(value)) saveQueuePreference(value); });
    if (onSave) input.closest('.queue-preferences').querySelector('[data-save-queue]')?.addEventListener('click', onSave);
  }
  function readToken() { try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch { return ''; } }
  function setToken(value) { token = value; try { if (value) sessionStorage.setItem(TOKEN_KEY, value); else sessionStorage.removeItem(TOKEN_KEY); } catch { /* The session remains usable without browser storage. */ } }
  function syncTime(time) { if (time && Number.isFinite(Date.parse(time))) serverOffset = Date.parse(time) - Date.now(); }
  function now() { return Date.now() + serverOffset; }
  function formatDate(value, short = false) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', ...(short ? {} : { hour: '2-digit', minute: '2-digit', hour12: false }) }).format(date); }
  function brandHTML() {
    const parts = config.brand.split(/\s*[·•]\s*/);
    return `<img class="brand-mark" src="/assets/shiguang-icon-128.png" width="38" height="38" alt="" aria-hidden="true"><span class="brand-name">${esc(parts[0])}</span>`;
  }
  function mascotHTML(compact = false) {
    return `<img class="mail-mascot${compact ? ' compact' : ''}" src="/assets/shiguang-mascot.png" width="512" height="512" alt="" aria-hidden="true" decoding="async">`;
  }
  function modePill() { return config.mode === 'demo' ? '<span class="pill pill-demo"><span class="status-dot"></span>演示模式</span>' : ''; }
  function reminderHTML() {
    return `<div class="reminder-control"><button id="reminder-trigger" class="icon-button reminder-trigger" type="button" aria-label="到达提醒" title="到达提醒" aria-expanded="false" aria-controls="reminder-panel">${icon('bell')}<span class="reminder-dot" aria-hidden="true" hidden></span></button><section id="reminder-panel" class="reminder-panel" aria-labelledby="reminder-title" hidden><h2 id="reminder-title">到达提醒</h2><button class="reminder-option" type="button" data-reminder="sound" aria-pressed="true">${icon('sound')}<span>提示音</span><span class="reminder-switch" aria-hidden="true"></span></button><button class="reminder-option" type="button" data-reminder="tab" aria-pressed="true">${icon('tab')}<span>标签提醒</span><span class="reminder-switch" aria-hidden="true"></span></button><button id="reminder-arm" class="text-button reminder-arm" type="button" hidden>启用声音</button><p id="reminder-unavailable" class="inline-note" hidden>此浏览器暂不支持提示音</p></section></div>`;
  }
  function renderReminderState() {
    if (!mailAlerts) return;
    const preferences = mailAlerts.getPreferences();
    const state = mailAlerts.getState();
    document.querySelectorAll('[data-reminder]').forEach(button => button.setAttribute('aria-pressed', String(preferences[button.dataset.reminder])));
    const trigger = byId('reminder-trigger');
    trigger?.classList.toggle('reminders-muted', !preferences.sound && !preferences.tab);
    if (trigger) trigger.title = !preferences.sound && !preferences.tab ? '到达提醒已关闭' : '到达提醒';
    const dot = trigger?.querySelector('.reminder-dot');
    if (dot) dot.hidden = !state.attention;
    if (byId('reminder-arm')) byId('reminder-arm').hidden = !preferences.sound || !state.audioLocked || !state.audioSupported;
    if (byId('reminder-unavailable')) byId('reminder-unavailable').hidden = !preferences.sound || state.audioSupported;
  }
  function bindReminders() {
    const trigger = byId('reminder-trigger');
    const panel = byId('reminder-panel');
    trigger?.addEventListener('click', () => { panel.hidden = !panel.hidden; trigger.setAttribute('aria-expanded', String(!panel.hidden)); });
    panel?.querySelectorAll('[data-reminder]').forEach(button => button.addEventListener('click', () => { const name = button.dataset.reminder; mailAlerts?.setPreference(name, !mailAlerts.getPreferences()[name]); renderReminderState(); }));
    byId('reminder-arm')?.addEventListener('click', () => mailAlerts?.armAudio());
    renderReminderState();
  }
  function closeReminders(restoreFocus = false) {
    const panel = byId('reminder-panel');
    if (!panel || panel.hidden) return;
    panel.hidden = true;
    byId('reminder-trigger')?.setAttribute('aria-expanded', 'false');
    if (restoreFocus) byId('reminder-trigger')?.focus();
  }
  document.addEventListener('click', event => { if (!event.target.closest('.reminder-control')) closeReminders(); });
  document.addEventListener('keydown', event => { if (event.key === 'Escape') closeReminders(true); });
  document.addEventListener('focusin', event => { if (!event.target.closest('.reminder-control')) closeReminders(); });
  function resumeReminderWatch() {
    if (isAdmin && byId('logout') && !adminResourcesTimer && mailAlerts?.shouldPollHidden(adminResources.email)) startAdminResources();
  }
  window.addEventListener('mail-alerts-change', () => { renderReminderState(); resumeReminderWatch(); });
  window.addEventListener('mail-arrival', () => { notify('邮箱已就绪'); announce('邮箱已就绪'); });
  function inventoryPill() { return `<span class="pill inventory-pill" data-inventory role="status" aria-live="polite" title="${esc(inventory.message)}">${icon('email')}<span>邮箱参考余量 <strong>—</strong></span><span class="inventory-allocation" hidden>暂不可分配</span></span>`; }
  function renderInventory() {
    document.querySelectorAll('[data-inventory]').forEach(el => {
      const available = Number.isFinite(inventory.count) && inventory.count >= 0;
      const refused = inventory.allocationStatus === 'no_stock';
      el.title = [inventory.message || (available ? '上游参考库存，实际以分配结果为准' : '暂时无法获取邮箱库存'), inventory.checkedAt ? `最近分配：${formatDate(inventory.checkedAt)}` : ''].filter(Boolean).join(' · ');
      el.classList.toggle('inventory-unknown', !available);
      el.classList.toggle('inventory-refused', refused);
      el.querySelector('strong').textContent = available ? new Intl.NumberFormat('zh-CN').format(inventory.count) : '—';
      el.querySelector('.inventory-allocation').hidden = !refused;
    });
  }
  async function refreshInventory() {
    if (document.hidden || !document.querySelector('[data-inventory]')) return;
    if (inventoryBusy) { inventoryRefreshPending = true; return; }
    inventoryBusy = true;
    const generation = inventoryGeneration;
    try {
      const result = await api('/api/inventory');
      if (generation !== inventoryGeneration) return;
      const value = result.email || {};
      inventory = { count: typeof value.count === 'number' && Number.isFinite(value.count) && value.count >= 0 ? value.count : null, message: value.message || (value.status === 'unconfigured' ? '邮箱服务尚未配置' : '上游参考库存，实际以分配结果为准'), allocationStatus: value.allocation_status || 'unconfirmed', checkedAt: value.allocation_checked_at };
      renderInventory();
    } catch (error) {
      if (generation !== inventoryGeneration) return;
      inventory = { count: null, message: error.message || '暂时无法获取邮箱库存' };
      renderInventory();
    } finally { inventoryBusy = false; if (inventoryRefreshPending) { inventoryRefreshPending = false; refreshInventory(); } }
  }
  function startInventory() { clearInterval(inventoryTimer); renderInventory(); refreshInventory(); inventoryTimer = setInterval(refreshInventory, 30000); }
  function stopInventory() { clearInterval(inventoryTimer); inventoryTimer = null; ++inventoryGeneration; }
  function badge(status, label = statusNames[status] || status) { return `<span class="status-badge ${esc(status)}"><span class="status-dot"></span>${esc(label)}</span>`; }
  function orderMessage(order) {
    if (order.kind === 'email' && order.status === 'received' && order.can_next_code) {
      if (order.auto_next_state === 'pending') return '正在准备下一封';
      if (order.auto_next_state === 'retrying') return '稍后自动重试';
      if (order.auto_next_state === 'failed') return '自动续收未成功，可重试接收';
    }
    // Keep historical no-stock failures accurate without changing stored orders.
    if (order.kind === 'email' && order.status === 'failed' && ['当前资源暂时售罄，请稍后重试', '当前条件下暂无可分配资源，请稍后重试'].includes(order.message)) return '当前条件下暂无可分配邮箱，参考余量以实际分配结果为准。';
    return order.message;
  }
  function kindLabel(kind) { return `<span class="kind-label ${kind === 'email' ? 'email' : ''}">${icon(kind === 'email' ? 'email' : 'phone')}${kind === 'email' ? '邮箱' : '手机号'}</span>`; }
  function receiptAnimation(orderID, receipt) {
    const key = `${Number(receipt.round) || 1}:${receipt.code}`;
    let seen = displayedCodes.get(orderID);
    if (!seen) { seen = new Set(); displayedCodes.set(orderID, seen); }
    const fresh = !seen.has(key);
    seen.add(key);
    return fresh ? ' code-entry-new' : '';
  }
  function notify(message, error = false) {
    clearTimeout(toastTimer);
    const notice = dialog.open ? byId('dialog-notice') : null;
    if (notice) {
      notice.textContent = message;
      notice.classList.toggle('error-text', error);
      toastTimer = setTimeout(() => { if (notice.isConnected) notice.textContent = ''; }, error ? 6500 : 3200);
      return;
    }
    const region = byId('toast-region');
    region.replaceChildren();
    const item = document.createElement('div');
    item.className = `toast${error ? ' error' : ''}`;
    item.innerHTML = `${icon(error ? 'close' : 'check')}<span>${esc(message)}</span>`;
    region.append(item);
    toastTimer = setTimeout(() => region.replaceChildren(), error ? 6500 : 3200);
  }
  function announce(message) { byId('announcer').textContent = message; }
  function fieldError(id, message) { const el = byId(id); if (el) el.textContent = message || ''; }
  function busy(button, state) { if (!button) return; button.disabled = state; button.classList.toggle('loading', state); button.setAttribute('aria-busy', String(state)); }
  function lockControls(root) {
    const controls = Array.from(root?.querySelectorAll('button, input, select') || [], control => [control, control.disabled]);
    controls.forEach(([control]) => { control.disabled = true; });
    return () => controls.forEach(([control, disabled]) => { if (control.isConnected) control.disabled = disabled; });
  }

  async function api(path, options = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 45000);
    try {
      const headers = { Accept: 'application/json', ...(options.body ? { 'Content-Type': 'application/json' } : {}), ...(options.auth && token ? { Authorization: `Bearer ${token}` } : {}) };
      const response = await fetch(path, { method: options.method || 'GET', credentials: 'same-origin', cache: 'no-store', signal: controller.signal, headers, ...(options.body ? { body: JSON.stringify(options.body) } : {}) });
      let data;
      try { data = await response.json(); } catch { throw new Error('服务暂时未响应，请稍后重试。'); }
      if (!response.ok) {
        const error = new Error(data.error || '操作未完成，请重试。');
        error.status = response.status;
        error.code = data.code;
        throw error;
      }
      syncTime(data.server_time);
      return data;
    } catch (error) {
      if (error.name === 'AbortError') throw new Error('请求超时，请重试以确认当前操作。');
      if (error instanceof TypeError) throw new Error('网络连接中断，请检查连接后重试。');
      throw error;
    } finally { clearTimeout(timeout); }
  }

  function applyBackground(settings) {
    const host = byId('custom-background');
    host.replaceChildren();
    if (!settings.background_url || !['image', 'video'].includes(settings.background_type)) return;
    let url;
    try { url = new URL(settings.background_url, location.origin); if (!['https:', 'http:'].includes(url.protocol)) return; } catch { return; }
    const element = document.createElement(settings.background_type === 'video' ? 'video' : 'img');
    element.src = url.href;
    if (element.tagName === 'VIDEO') {
      element.muted = true;
      element.loop = true;
      element.playsInline = true;
      element.autoplay = !matchMedia('(prefers-reduced-motion: reduce)').matches;
      element.preload = 'metadata';
      const preference = matchMedia('(prefers-reduced-motion: reduce)');
      preference.addEventListener('change', () => { if (preference.matches) element.pause(); else element.play().catch(() => {}); });
    } else { element.alt = ''; element.decoding = 'async'; }
    host.append(element);
  }

  function publicShell() {
    app.innerHTML = `<div class="public-shell"><header class="public-header"><a href="/" class="brand" aria-label="兑换首页">${brandHTML()}</a><div class="header-actions">${inventoryPill()}${modePill()}${reminderHTML()}<a href="/admin" class="icon-button" aria-label="管理后台" title="管理后台">${icon('grid')}</a></div></header><main id="main" class="public-main"><aside class="public-aside animate-in"><div class="mascot-scene">${mascotHTML()}</div><div class="aside-info"><ol class="steps" aria-label="兑换进度"><li class="active" data-step="1"><span class="step-number">${icon('ticket')}</span>兑换</li><li data-step="2"><span class="step-number">${icon('email')}</span>接码</li><li data-step="3"><span class="step-number">${icon('star')}</span>完成</li></ol></div></aside><section id="exchange" class="exchange-card animate-in" aria-label="资源兑换" style="animation-delay:.08s"></section></main><footer class="public-footer"><span class="footer-status">${icon('moon')}${config.mode === 'demo' ? '模拟资源 · 不产生真实接码' : '临时接码'}</span></footer></div>`;
  }
  function updateSteps(step) { document.querySelectorAll('[data-step]').forEach(el => { const n = Number(el.dataset.step); el.classList.toggle('active', n === step); el.classList.toggle('done', n < step); if (n === step) el.setAttribute('aria-current', 'step'); else el.removeAttribute('aria-current'); }); }
  function renderRedeem() {
    stopPolling(); ++orderEpoch; currentOrder = null; updateSteps(1);
    byId('exchange').innerHTML = `<div class="card-top"><div><span class="surface-caption">OpenAI</span><h1>兑换接码</h1></div><span class="small-flower" aria-hidden="true">${icon('star')}</span></div><div class="service-types"><span class="service-tag ${config.phone_enabled ? '' : 'unavailable'}">${icon('phone')}手机号</span><span class="service-tag email ${config.email_enabled ? '' : 'unavailable'}">${icon('email')}邮箱</span></div>${config.mode === 'live' && !config.configured ? '<p class="inline-warning">资源服务尚未配置，请联系管理员。</p>' : ''}<form id="redeem-form" class="redeem-form"><div class="field"><label for="cdk">CDK</label><input id="cdk" class="cdk-input" name="cdk" placeholder="输入兑换码" required maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false" aria-describedby="redeem-error"></div>${config.email_enabled ? queuePreferenceHTML('redeem-queue-minutes') : ''}<button class="button button-primary" type="submit"><span>兑换</span>${icon('arrow')}</button><p id="redeem-error" class="error-text" role="alert"></p></form>${config.mode === 'demo' ? '<div class="demo-shortcuts"><span>体验</span><button type="button" class="demo-shortcut" data-demo="DEMO-PHONE">手机号</button><button type="button" class="demo-shortcut" data-demo="DEMO-EMAIL">邮箱</button></div>' : ''}`;
    byId('redeem-form').addEventListener('submit', redeem);
    bindQueuePreference('redeem-queue-minutes');
    byId('redeem-form').querySelector('[type="submit"]').disabled = config.mode === 'live' && !config.configured;
    document.querySelectorAll('[data-demo]').forEach(button => button.addEventListener('click', () => { byId('cdk').value = button.dataset.demo; byId('cdk').focus(); }));
  }
  async function redeem(event) {
    event.preventDefault();
    if (requestBusy) return;
    const form = event.currentTarget;
    const input = form.querySelector('[name="cdk"]');
    const code = input.value.trim();
    const errorID = 'redeem-error';
    if (config.mode === 'live' && !config.configured) { fieldError(errorID, '资源服务尚未配置，请联系管理员。'); return; }
    if (!code) { fieldError(errorID, '请输入 CDK。'); input.focus(); return; }
    let queueMinutes;
    try { queueMinutes = queuePreferenceValue('redeem-queue-minutes'); } catch (error) { fieldError(errorID, error.message); return; }
    const button = form.querySelector('[type="submit"]');
    const unlock = lockControls(byId('exchange'));
    requestBusy = true; ++orderEpoch; busy(button, true); fieldError(errorID, '');
    try {
      const data = await api('/api/redeem', { method: 'POST', body: { cdk: code, queue_minutes: queueMinutes } });
      setToken(data.token); input.value = ''; showOrder(data.order); announce(orderStatusName(data.order) || '兑换成功'); startPolling(); refreshInventory();
    } catch (error) { fieldError(errorID, error.message); }
    finally { requestBusy = false; busy(button, false); unlock(); }
  }
  function activeStatus(status) { return ['queued', 'allocating', 'waiting', 'received', 'cancel_pending', 'complete_pending', 'next_pending', 'next_uncertain'].includes(status); }
  function queuedBefore(order) { return order.kind === 'email' && !order.resource && (!!order.queue_expires_at || Number(order.queue_attempts) > 0); }
  function queueResultLabel(order) { return queuedBefore(order) ? ({ cancelled: '已取消排队', expired: order.message === '排队已超时，请重试' ? '排队超时' : '排队已结束' })[order.status] || '' : ''; }
  function orderStatusName(order) { return queueResultLabel(order) || statusNames[order.status] || order.status; }
  function queueClock(order) { return !order.resource && !!order.queue_expires_at && ['queued', 'allocating'].includes(order.status); }
  function queueContent(order) {
    return `<div class="queue-waiting" role="status"><span class="queue-orbit" aria-hidden="true">${icon('email')}</span><div><strong>${order.status === 'queued' ? '等待邮箱' : '正在获取邮箱'}</strong><span>有可用邮箱时自动获取</span></div></div>`;
  }
  function sameOrderView(previous, order) {
    if (!previous) return false;
    // Retry bookkeeping changes every few seconds; keep the focused controls and animation in place.
    const view = value => { const { queue_attempts, queue_next_attempt_at, updated_at, ...visible } = value; return visible; };
    return JSON.stringify(view(previous)) === JSON.stringify(view(order));
  }
  function showOrder(order, options = {}) {
    const previous = currentOrder;
    currentOrder = order;
    mailAlerts?.observe(order, options);
    updateSteps(['completed', 'cancelled', 'expired', 'failed'].includes(order.status) ? 3 : 2);
    if (sameOrderView(previous, order)) { updateCountdown(); return; }
    const email = order.kind === 'email';
    const queued = order.status === 'queued';
    const inQueue = queueClock(order);
    const type = email ? '邮箱' : '手机号';
    const codes = Array.isArray(order.codes) && order.codes.length ? order.codes.filter(item => item.code) : order.code ? [{ round: 1, code: order.code, received_at: '' }] : [];
    const round = Math.max(1, Math.min(3, Number(order.mail_round) || 1));
    const pendingNext = ['next_pending', 'next_uncertain'].includes(order.status);
    const waiting = ['waiting', 'allocating', 'cancel_pending', 'complete_pending'].includes(order.status) || pendingNext;
    const hasCode = codes.length > 0;
    const canCancel = (queued || order.status === 'waiting' || (order.status === 'allocating' && !inQueue)) && !hasCode;
    const canComplete = order.status === 'received' || (email && hasCode && ['waiting', 'next_uncertain'].includes(order.status));
    const canNext = email && order.status === 'received' && order.can_next_code && order.auto_next_state === 'failed';
    const finished = !activeStatus(order.status);
    const canReplace = order.can_retry && finished;
    const terminalCopy = { completed: '本次接码已完成', cancelled: '本次资源已释放', expired: '本地等待已结束', review: '资源状态待核查，请联系管理员', failed: '本次资源未能分配' };
    const waitLabel = order.status === 'allocating' ? '正在分配资源' : order.status === 'cancel_pending' ? '正在确认资源释放' : order.status === 'complete_pending' ? '正在确认接码结束' : pendingNext ? '接码状态待确认' : email ? `等待第 ${round} 封验证码` : '等待验证码';
    const codeContent = hasCode ? `<div class="code-history" aria-label="验证码记录">${codes.map((item, index) => `<div class="code-entry${receiptAnimation(order.id, item)}"><div class="code-entry-info">${email ? `<span class="code-round">第 ${Number(item.round) || index + 1} 封</span>` : ''}<span class="code-value ${String(item.code).length > 12 ? 'long-code' : ''}">${esc(item.code)}</span>${item.received_at ? `<time class="code-time" datetime="${esc(item.received_at)}">${formatDate(item.received_at)}</time>` : ''}</div><button class="copy-button" type="button" data-copy-code="${index}" aria-label="复制${email ? `第 ${Number(item.round) || index + 1} 封` : ''}验证码" title="复制验证码">${icon('copy')}</button></div>`).join('')}</div>` : '';
    const stateContent = inQueue ? queueContent(order) : waiting ? `<div class="code-waiting ${hasCode ? 'following-code' : ''}"><span class="waiting-dots" aria-hidden="true"><i></i><i></i><i></i></span>${waitLabel}</div>` : !hasCode ? `<p class="terminal-message">${icon(order.status === 'completed' ? 'check' : 'clock')}${esc(queueResultLabel(order) || terminalCopy[order.status] || '订单已结束')}</p>` : '';
    byId('exchange').innerHTML = `<div class="card-top"><div class="order-heading"><span class="kind-icon ${email ? 'email' : ''}">${icon(order.kind)}</span><div><span class="surface-caption">OpenAI</span><h1>${type}接码</h1></div></div><span class="order-status ${esc(order.status)}"><span class="status-dot"></span>${esc(orderStatusName(order))}</span></div>${order.resource ? `<div class="resource-row"><span class="resource-value ${email ? 'email' : ''}" id="resource-value">${esc(order.resource)}</span><button class="copy-button" type="button" id="copy-resource" aria-label="复制${type}" title="复制${type}">${icon('copy')}</button></div>` : ''}<div class="code-box ${inQueue ? 'queue-box' : ''} ${hasCode ? 'received with-history' : ''}">${codeContent}${stateContent}</div><div class="order-meta"><span>${email ? '有效邮箱' : '有效号码'} ${Number(order.used_count) || 0} / ${Number(order.usage_limit) || 1}</span>${email && order.resource ? `<span>已收 ${codes.length} / 3 封</span>` : ''}${!finished && (inQueue || order.expires_at) ? `<span class="time-left">${icon('clock')}${inQueue ? '排队剩余 ' : '本地等待 '}<span id="countdown">--:--</span></span>` : ''}</div>${!finished ? '<div class="order-progress"><span id="time-progress" style="width:100%"></span></div>' : ''}${!inQueue && order.message && order.message !== queueResultLabel(order) ? `<p class="order-message">${esc(orderMessage(order))}</p>` : ''}${email && (queued || canReplace) ? queuePreferenceHTML('order-queue-minutes', queued ? order.queue_minutes : queueMinutesPreference, queued, order.id) : ''}${canCancel || canComplete || canNext || canReplace ? `<div class="order-actions">${canCancel ? `<button id="cancel-order" class="button button-ghost" type="button">${queued ? '取消排队' : '取消本次'}</button>` : ''}${canComplete ? `<button id="complete-order" class="button ${canNext ? 'button-ghost' : 'button-primary'}" type="button">${icon('check')}${email ? '结束邮箱' : '完成接码'}</button>` : ''}${canNext ? `<button id="next-code" class="button button-primary" type="button">${icon('refresh')}重试接收</button>` : ''}${canReplace ? `<button id="replace-order" class="button button-primary" type="button">${icon('arrow')}${order.status === 'completed' ? '获取下一个' : '重新获取'}</button>` : ''}</div>` : ''}<p id="order-error" class="error-text" role="alert"></p><button class="text-button order-switch" id="switch-cdk" type="button">${activeStatus(order.status) ? '使用其他 CDK' : '返回兑换'}</button>`;
    bindQueuePreference('order-queue-minutes', queued ? () => orderAction('queue-wait') : null);
    byId('copy-resource')?.addEventListener('click', () => copyText(order.resource));
    document.querySelectorAll('[data-copy-code]').forEach(button => button.addEventListener('click', () => copyText(codes[Number(button.dataset.copyCode)].code)));
    byId('cancel-order')?.addEventListener('click', confirmCancel);
    byId('complete-order')?.addEventListener('click', () => orderAction('complete'));
    byId('next-code')?.addEventListener('click', () => orderAction('next-code'));
    byId('replace-order')?.addEventListener('click', () => orderAction('replace'));
    byId('switch-cdk')?.addEventListener('click', () => {
      if (activeStatus(order.status)) openDialog('切换 CDK', `<p class="dialog-body">${inQueue ? '当前排队会继续。' : '当前资源会继续计时。'}再次输入原 CDK 可返回当前订单。</p><div class="dialog-actions"><button class="button button-ghost" data-close>留在此处</button><button class="button button-primary" id="confirm-switch">切换</button></div>`, () => { byId('confirm-switch').addEventListener('click', () => { closeDialog(); setToken(''); renderRedeem(); byId('cdk').focus(); }); });
      else { setToken(''); renderRedeem(); }
    });
    if (previous?.status !== order.status) announce(orderStatusName(order));
    if (previous && queueClock(previous) && !inQueue) refreshInventory();
    updateCountdown();
  }
  function updateCountdown() {
    if (!currentOrder) return;
    const inQueue = queueClock(currentOrder);
    const expiry = Date.parse(inQueue ? currentOrder.queue_expires_at : currentOrder.expires_at);
    const remaining = Number.isFinite(expiry) ? Math.max(0, Math.ceil((expiry - now()) / 1000)) : 0;
    const el = byId('countdown');
    if (el) el.textContent = remaining > 0 ? `${String(Math.floor(remaining / 60)).padStart(2, '0')}:${String(remaining % 60).padStart(2, '0')}` : inQueue ? '正在确认排队结果' : '确认到期状态';
    const progress = byId('time-progress');
    if (progress) { const total = Math.max(1, expiry - Date.parse(currentOrder.created_at)); progress.style.width = `${Math.max(0, Math.min(100, (expiry - now()) / total * 100))}%`; }
    const cancel = byId('cancel-order');
    if (cancel) {
      const queued = currentOrder.status === 'queued';
      const wait = queued ? 0 : Math.max(0, Math.ceil((Date.parse(currentOrder.cancel_after) - now()) / 1000)) || 0;
      cancel.disabled = requestBusy || wait > 0;
      cancel.textContent = queued ? '取消排队' : wait > 0 ? `${wait} 秒后可取消` : '取消本次';
    }
  }
  function startPolling() {
    stopPolling();
    timerInterval = setInterval(updateCountdown, 1000);
    pollTimer = setInterval(() => { if (currentOrder && activeStatus(currentOrder.status) && (!document.hidden || mailAlerts?.shouldPollHidden(currentOrder))) pollOrder(); }, 5000);
  }
  function stopPolling() { clearInterval(pollTimer); clearInterval(timerInterval); pollTimer = null; timerInterval = null; }
  async function pollOrder(initial = false) {
    if (!token || pollBusy || requestBusy) return;
    pollBusy = true;
    const epoch = orderEpoch; const requestToken = token;
    try { const data = await api('/api/orders/current', { auth: true }); if (epoch !== orderEpoch || requestToken !== token) return; showOrder(data.order); if (!activeStatus(data.order.status)) stopPolling(); }
    catch (error) {
      if (epoch !== orderEpoch || requestToken !== token) return;
      if (error.status === 401 || error.status === 404) { setToken(''); renderRedeem(); notify('会话已失效，请重新输入 CDK。'); }
      else if (initial) { renderRedeem(); notify(error.message, true); }
      else { fieldError('order-error', error.message); }
    } finally { pollBusy = false; }
  }
  function confirmCancel() {
    const queued = currentOrder?.status === 'queued';
    openDialog(queued ? '取消排队' : '取消本次接码', `<p class="dialog-body">${queued ? '取消后可重新排队，不扣额度。' : '未收到验证码不扣额度，释放后可重新获取。'}</p><div class="dialog-actions"><button class="button button-ghost" data-close>继续等待</button><button class="button button-primary" id="confirm-cancel">${queued ? '取消排队' : '确认取消'}</button></div>`, () => byId('confirm-cancel').addEventListener('click', () => { closeDialog(); orderAction('cancel'); }));
  }
  async function orderAction(action) {
    if (requestBusy || (currentOrder?.status === 'queued' && !['cancel', 'queue-wait'].includes(action))) return;
    let queueMinutes;
    if (['replace', 'queue-wait'].includes(action)) {
      try { queueMinutes = queuePreferenceValue('order-queue-minutes'); } catch (error) { fieldError('order-error', error.message); return; }
    }
    requestBusy = true; ++orderEpoch;
    const button = action === 'queue-wait' ? byId('order-queue-minutes')?.closest('.queue-preferences').querySelector('[data-save-queue]') : byId({ cancel: 'cancel-order', complete: 'complete-order', 'next-code': 'next-code', replace: 'replace-order' }[action]);
    const body = action === 'next-code' ? { round: Number(currentOrder?.mail_round) || 1 } : ['replace', 'queue-wait'].includes(action) ? { queue_minutes: queueMinutes } : {};
    const unlock = lockControls(byId('exchange'));
    busy(button, true); fieldError('order-error', '');
    const previousOrderID = currentOrder?.id;
    try { const data = await api(`/api/orders/${action}`, { method: 'POST', auth: true, body }); if (action === 'queue-wait') queueDrafts.delete(`order-queue-minutes:${currentOrder.id}`); if (data.token) setToken(data.token); showOrder(data.order, { freshAllocation: action === 'replace' && data.order.id !== previousOrderID }); if (!activeStatus(data.order.status)) stopPolling(); else startPolling(); if (action === 'replace') refreshInventory(); if (action === 'queue-wait') notify(data.order.status === 'queued' ? '排队时间已调整' : '排队已结束'); }
    catch (error) { fieldError('order-error', error.message); }
    finally { requestBusy = false; busy(button, false); unlock(); updateCountdown(); }
  }
  async function copyText(text) {
    let copied = false;
    if (navigator.clipboard?.writeText && window.isSecureContext) {
      try { await navigator.clipboard.writeText(text); copied = true; } catch { /* Try the selection-based path when browser permissions reject Clipboard API. */ }
    }
    if (!copied) {
      const focused = document.activeElement;
      const input = document.createElement('textarea');
      input.value = text;
      input.readOnly = true;
      input.tabIndex = -1;
      input.setAttribute('aria-hidden', 'true');
      input.style.cssText = 'position:fixed;top:0;left:0;width:1px;height:1px;opacity:0;font-size:16px;';
      (dialog.open ? dialog : document.body).append(input);
      try { input.focus({ preventScroll: true }); input.select(); input.setSelectionRange(0, input.value.length); copied = document.execCommand('copy'); }
      catch { copied = false; }
      finally { input.remove(); if (focused?.isConnected) focused.focus({ preventScroll: true }); }
    }
    if (copied) { notify('已复制'); return; }
    const visibleCodes = dialog.open ? byId('generated-codes') : null;
    if (visibleCodes) {
      visibleCodes.focus({ preventScroll: true });
      const range = document.createRange(); range.selectNodeContents(visibleCodes);
      const selection = window.getSelection(); selection?.removeAllRanges(); selection?.addRange(range);
      notify('内容已选中，请手动复制。', true);
      return;
    }
    openDialog('手动复制', '<label for="manual-copy">选中内容后使用系统复制操作</label><textarea id="manual-copy" readonly rows="3" spellcheck="false" style="resize:vertical;font-family:inherit;font-size:16px"></textarea><div class="dialog-actions"><button class="button button-primary" data-close>完成</button></div>', () => {
      const input = byId('manual-copy'); input.value = text; input.focus({ preventScroll: true }); input.select(); input.setSelectionRange(0, input.value.length);
    });
  }

  function renderAdminEntry(content) {
    stopAdminResources(); stopInventory();
    adminResources.phone = null; adminResources.email = null;
    mailAlerts?.clearAttention();
    ++adminGeneration;
    app.innerHTML = `<div class="login-shell"><header class="login-header"><a href="/" class="brand">${brandHTML()}</a><a class="button button-ghost button-small" href="/">${icon('back')}返回兑换</a></header><main id="main" class="login-card animate-in"><span class="login-icon" aria-hidden="true">${icon('moon')}</span>${content}</main>${config.mode === 'demo' ? '<footer class="login-bottom">演示模式</footer>' : ''}</div>`;
  }
  function renderAdminEntryError(error, retry) {
    renderAdminEntry('<h1>管理后台</h1><p id="admin-entry-error" class="error-text" role="alert"></p><button id="admin-entry-retry" class="button button-primary" type="button">重新连接</button>');
    byId('admin-entry-error').textContent = error.message;
    byId('admin-entry-retry').addEventListener('click', retry);
  }
  async function loadAdminAccess(notice = '') {
    renderAdminEntry('<h1>管理后台</h1><p class="inline-note" role="status">正在连接…</p>');
    const generation = adminGeneration;
    try {
      const data = await api('/api/admin/bootstrap');
      if (generation !== adminGeneration) return;
      if (typeof data.setup_required !== 'boolean') throw new Error('管理员状态暂时无法确认，请重试。');
      renderLogin(data.setup_required, notice);
    } catch (error) {
      if (generation === adminGeneration) renderAdminEntryError(error, () => loadAdminAccess(notice));
    }
  }
  function renderLogin(setup = false, notice = '') {
    renderAdminEntry(`<h1>${setup ? '设置管理员' : '管理登录'}</h1>${setup ? '<p class="login-description">设置你的管理密码</p>' : ''}${notice ? `<p class="inline-warning" role="status">${esc(notice)}</p>` : ''}<form id="login-form"><div class="field"><label for="password">${setup ? '新密码' : '管理员密码'}</label><input id="password" type="password" name="password" autocomplete="${setup ? 'new-password' : 'current-password'}" required ${setup ? 'minlength="12" placeholder="12–128 个字符"' : ''} maxlength="256" aria-describedby="login-error"></div>${setup ? '<div class="field"><label for="confirm-password">确认密码</label><input id="confirm-password" type="password" name="confirm_password" autocomplete="new-password" required minlength="12" maxlength="256" aria-describedby="login-error"></div>' : ''}<button class="button button-primary" type="submit">${setup ? '设置并进入' : '进入后台'}${icon('arrow')}</button><p id="login-error" class="error-text" role="alert"></p></form>`);
    byId('login-form').addEventListener('submit', async event => {
      event.preventDefault(); const form = event.currentTarget; const button = form.querySelector('button');
      if (button.disabled) return;
      const password = byId('password').value;
      const confirmation = setup ? byId('confirm-password').value : '';
      fieldError('login-error', '');
      if (setup && (Array.from(password).length < 12 || Array.from(password).length > 128)) { fieldError('login-error', '密码需为 12–128 个字符。'); byId('password').focus(); return; }
      if (setup && !password.trim()) { fieldError('login-error', '密码不能全部为空格。'); byId('password').focus(); return; }
      if (setup && password !== confirmation) { fieldError('login-error', '两次输入的密码不一致。'); byId('confirm-password').focus(); return; }
      busy(button, true);
      try {
        await api(setup ? '/api/admin/setup' : '/api/admin/login', { method: 'POST', body: { password, ...(setup ? { confirm_password: confirmation } : {}) } });
        form.reset(); renderAdminShell(); await showAdminTab(adminTab);
      } catch (error) {
        if (setup && error.status === 409) { form.reset(); await loadAdminAccess('管理员已设置，请使用管理员密码登录。'); }
        else fieldError('login-error', error.message);
      }
      finally { busy(button, false); }
    });
  }
  function renderAdminShell() {
    app.innerHTML = `<div class="admin-shell"><aside class="sidebar"><a class="brand" href="/">${brandHTML()}</a><nav class="admin-nav" aria-label="管理导航"><button class="nav-item" data-tab="overview" title="总览">${icon('grid')}<span>总览</span></button><button class="nav-item" data-tab="resources" title="接码">${icon('inbox')}<span>接码</span></button><button class="nav-item" data-tab="cdks" title="CDK">${icon('ticket')}<span>CDK</span></button><button class="nav-item" data-tab="settings" title="设置">${icon('settings')}<span>设置</span></button></nav><div class="sidebar-footer"><a href="/" class="nav-item" title="兑换页">${icon('external')}<span>兑换页</span></a><button id="logout" class="nav-item" title="退出登录">${icon('logout')}<span>退出登录</span></button></div></aside><div class="admin-content"><header class="admin-topbar resource-topbar"><span class="topbar-title">${icon('star')}管理空间</span><div class="header-actions">${inventoryPill()}${modePill()}${reminderHTML()}</div></header><main id="main"><div class="empty loading-state"><span class="waiting-dots" aria-hidden="true"><i></i><i></i><i></i></span><span>加载中</span></div></main></div></div>`;
    document.querySelectorAll('[data-tab]').forEach(button => button.addEventListener('click', () => showAdminTab(button.dataset.tab)));
    async function logout() { try { await api('/api/admin/logout', { method: 'POST', body: {} }); renderLogin(); } catch (error) { notify(error.message, true); } }
    byId('logout').addEventListener('click', logout);
    bindReminders(); startInventory();
    if (adminTab !== 'resources') restoreAdminArrivalWatch();
  }
  async function restoreAdminArrivalWatch() {
    const shell = byId('logout');
    try {
      const data = await api('/api/admin/resources');
      if (shell !== byId('logout')) return;
      if (!adminResources.email && !adminResourceBusy.email) {
        adminResources.email = (data.orders || []).find(order => order.kind === 'email') || null;
        mailAlerts?.observe(adminResources.email);
        resumeReminderWatch();
      }
    } catch { /* The resource page handles authentication and retry errors. */ }
  }
  async function showAdminTab(tab, push = true) {
    stopAdminResources();
    if (!['overview', 'resources', 'cdks', 'settings'].includes(tab)) tab = 'overview';
    adminTab = tab; const generation = ++adminGeneration;
    document.querySelectorAll('[data-tab]').forEach(el => { const selected = el.dataset.tab === tab; el.classList.toggle('active', selected); if (selected) el.setAttribute('aria-current', 'page'); else el.removeAttribute('aria-current'); });
    if (push) history.replaceState({}, '', `/admin${tab === 'overview' ? '' : `?tab=${tab}`}`);
    byId('main').innerHTML = '<div class="empty loading-state"><span class="waiting-dots" aria-hidden="true"><i></i><i></i><i></i></span><span>加载中</span></div>';
    try {
      if (tab === 'overview') { const data = await api('/api/admin/overview'); if (generation === adminGeneration) renderOverview(data); }
      if (tab === 'resources') { const data = await api('/api/admin/resources'); if (generation === adminGeneration) renderAdminResources(data); }
      if (tab === 'cdks') { const data = await getCDKs(); if (generation === adminGeneration) renderCDKs(data); }
      if (tab === 'settings') { const data = await api('/api/admin/settings'); if (generation === adminGeneration) renderSettings(data); }
    } catch (error) {
      if (generation !== adminGeneration) return;
      if (error.status === 401) { await loadAdminAccess(); return; }
      byId('main').innerHTML = '<div class="panel"><div class="empty"><p id="admin-load-error"></p><button id="retry-admin" class="button button-ghost button-small" style="margin-top:20px">重试</button></div></div>';
      byId('admin-load-error').textContent = error.message;
      byId('retry-admin').addEventListener('click', () => showAdminTab(tab));
    } finally {
      if (generation === adminGeneration && (byId('admin-resource-email') || mailAlerts?.shouldPollHidden(adminResources.email))) startAdminResources();
    }
  }
  function adminRequestKey(kind) { return `atelier.admin.resource.request.${kind}`; }
  function readAdminRequest(kind) {
    try {
      const requestID = localStorage.getItem(adminRequestKey(kind));
      if (requestID) {
        adminResourceRequests[kind] = requestID;
        const minutes = Number(localStorage.getItem(`${adminRequestKey(kind)}.queue_minutes`));
        adminResourceRequestMinutes[kind] = Number.isInteger(minutes) && minutes >= 1 && minutes <= 1440 ? minutes : null;
      }
    } catch { /* The server also restores active orders after a reload. */ }
    return adminResourceRequests[kind];
  }
  function saveAdminRequest(kind, value, queueMinutes = null) {
    adminResourceRequests[kind] = value;
    adminResourceRequestMinutes[kind] = value ? queueMinutes : null;
    try {
      if (value) localStorage.setItem(adminRequestKey(kind), value); else localStorage.removeItem(adminRequestKey(kind));
      if (value && queueMinutes != null) localStorage.setItem(`${adminRequestKey(kind)}.queue_minutes`, String(queueMinutes)); else localStorage.removeItem(`${adminRequestKey(kind)}.queue_minutes`);
    } catch { /* Keep the pending request in this page until the server confirms it. */ }
  }
  function newRequestID() {
    if (crypto.randomUUID) return crypto.randomUUID();
    return Array.from(crypto.getRandomValues(new Uint8Array(24)), value => value.toString(16).padStart(2, '0')).join('');
  }
  function adminResourceTerminal(order) { return !order || ['completed', 'cancelled', 'expired', 'failed'].includes(order.status); }
  function renderAdminResources(data) {
    for (const kind of ['phone', 'email']) {
      adminResources[kind] = (data.orders || []).find(order => order.kind === kind) || null;
      mailAlerts?.observe(adminResources[kind]);
      if (adminResources[kind]?.request_id === readAdminRequest(kind)) saveAdminRequest(kind, '');
    }
    byId('main').innerHTML = `<div class="animate-in direct-resource-page"><div class="page-title"><h1>接码</h1><button class="icon-button" id="refresh-resources" aria-label="刷新资源" title="刷新资源">${icon('refresh')}</button></div><div class="direct-resource-grid"><section id="admin-resource-phone" class="panel direct-resource-card" aria-label="手机号接码"></section><section id="admin-resource-email" class="panel direct-resource-card" aria-label="邮箱接码"></section></div></div>`;
    byId('refresh-resources').addEventListener('click', async event => {
      if (Object.values(adminResourceBusy).some(Boolean)) return;
      const button = event.currentTarget; busy(button, true);
      try { await showAdminTab('resources'); } finally { busy(button, false); }
    });
    renderAdminResource('phone'); renderAdminResource('email');
    startAdminResources();
  }
  function renderAdminResource(kind) {
    const card = byId(`admin-resource-${kind}`);
    if (!card) return;
    const order = adminResources[kind];
    const email = kind === 'email';
    const type = email ? '邮箱' : '手机号';
    const pending = readAdminRequest(kind);
    const heading = `<div class="direct-resource-heading"><div class="order-heading"><span class="kind-icon ${email ? 'email' : ''}">${icon(kind)}</span><h2>${type}</h2></div>${order ? badge(order.status, orderStatusName(order)) : `<span class="small-flower" aria-hidden="true">${icon('star')}</span>`}</div>`;
    if (!order) {
      card.innerHTML = `${heading}<div class="direct-resource-empty ${email ? 'email' : ''}">${icon(kind)}<span>尚未领取</span></div>${email ? queuePreferenceHTML('admin-email-queue-minutes') : ''}<button class="button button-primary direct-acquire" type="button" data-resource-action="create">${icon(pending ? 'refresh' : 'plus')}${pending ? '重试获取' : `获取${type}`}</button><p id="admin-resource-error-${kind}" class="error-text" role="alert">${esc(adminResourceErrors[kind])}</p>`;
    } else {
      const codes = Array.isArray(order.codes) && order.codes.length ? order.codes.filter(item => item.code) : order.code ? [{ round: 1, code: order.code }] : [];
      const round = Math.max(1, Math.min(3, Number(order.mail_round) || 1));
      const hasCode = codes.length > 0;
      const queued = order.status === 'queued';
      const inQueue = queueClock(order);
      const pendingNext = ['next_pending', 'next_uncertain'].includes(order.status);
      const waiting = ['waiting', 'allocating', 'cancel_pending', 'complete_pending'].includes(order.status) || pendingNext;
      const canCancel = (queued || order.status === 'waiting') && !hasCode;
      const canComplete = order.status === 'received' || (email && hasCode && ['waiting', 'next_uncertain'].includes(order.status));
      const canNext = email && order.status === 'received' && order.can_next_code && order.auto_next_state === 'failed';
      const canCreate = adminResourceTerminal(order);
      const active = activeStatus(order.status);
      const waitLabel = order.status === 'allocating' ? '正在分配资源' : order.status === 'cancel_pending' ? '正在确认资源释放' : order.status === 'complete_pending' ? '正在确认接码结束' : pendingNext ? '接码状态待确认' : email ? `等待第 ${round} 封验证码` : '等待验证码';
      const history = codes.map((item, index) => `<div class="code-entry${receiptAnimation(order.id, item)}"><div class="code-entry-info">${email ? `<span class="code-round">第 ${Number(item.round) || index + 1} 封</span>` : ''}<span class="code-value ${String(item.code).length > 12 ? 'long-code' : ''}">${esc(item.code)}</span>${item.received_at ? `<time class="code-time" datetime="${esc(item.received_at)}">${formatDate(item.received_at)}</time>` : ''}</div><button class="copy-button" type="button" data-admin-copy-code="${index}" aria-label="复制${email ? `第 ${Number(item.round) || index + 1} 封` : ''}验证码">${icon('copy')}</button></div>`).join('');
      card.innerHTML = `${heading}${order.resource ? `<div class="resource-row"><span class="resource-value ${email ? 'email' : ''}">${esc(order.resource)}</span><button class="copy-button" type="button" data-admin-copy-resource aria-label="复制${type}" title="复制${type}">${icon('copy')}</button></div>` : ''}<div class="code-box ${inQueue ? 'queue-box' : ''} ${hasCode ? 'received with-history' : ''}">${hasCode ? `<div class="code-history" aria-label="验证码记录">${history}</div>` : ''}${inQueue ? queueContent(order) : waiting ? `<div class="code-waiting ${hasCode ? 'following-code' : ''}"><span class="waiting-dots" aria-hidden="true"><i></i><i></i><i></i></span>${waitLabel}</div>` : !hasCode ? `<p class="terminal-message">${icon(order.status === 'completed' ? 'check' : 'clock')}${esc(orderStatusName(order))}</p>` : ''}</div><div class="order-meta">${email && order.resource ? `<span>已收 ${codes.length} / 3 封</span>` : ''}${active && (inQueue || order.expires_at) ? `<span class="time-left">${icon('clock')}${inQueue ? '排队剩余 ' : '本地等待 '}<span data-admin-countdown>--:--</span></span>` : ''}</div>${active ? '<div class="order-progress"><span data-admin-progress></span></div>' : ''}${!inQueue && order.message && order.message !== queueResultLabel(order) ? `<p class="order-message">${esc(orderMessage(order))}</p>` : ''}${email && (queued || canCreate) ? queuePreferenceHTML('admin-email-queue-minutes', queued ? order.queue_minutes : queueMinutesPreference, queued, order.id) : ''}<div class="order-actions">${canCancel ? `<button class="button button-ghost" type="button" data-resource-action="cancel">${queued ? '取消排队' : '取消本次'}</button>` : ''}${canComplete ? `<button class="button ${canNext ? 'button-ghost' : 'button-primary'}" type="button" data-resource-action="complete">${icon('check')}${email ? '结束邮箱' : '完成接码'}</button>` : ''}${canNext ? `<button class="button button-primary" type="button" data-resource-action="next-code">${icon('refresh')}重试接收</button>` : ''}${canCreate ? `<button class="button button-primary" type="button" data-resource-action="create">${icon(pending ? 'refresh' : 'plus')}${pending ? '重试获取' : `获取${type}`}</button>` : ''}${['review', 'next_uncertain'].includes(order.status) ? '<button class="button button-ghost" type="button" data-admin-resolve>核查</button>' : ''}</div><p id="admin-resource-error-${kind}" class="error-text" role="alert">${esc(adminResourceErrors[kind])}</p>`;
      card.querySelector('[data-admin-copy-resource]')?.addEventListener('click', () => copyText(order.resource));
      card.querySelectorAll('[data-admin-copy-code]').forEach(button => button.addEventListener('click', () => copyText(codes[Number(button.dataset.adminCopyCode)].code)));
      card.querySelector('[data-admin-resolve]')?.addEventListener('click', () => resolveDialog(order));
    }
    if (email) {
      bindQueuePreference('admin-email-queue-minutes', order?.status === 'queued' ? () => adminResourceAction(kind, 'queue-wait') : null);
      const queueInput = byId('admin-email-queue-minutes');
      if (queueInput) {
        const confirming = !!pending && adminResourceTerminal(order);
        queueInput.disabled = adminResourceBusy[kind] || confirming;
        if (confirming) {
          queueInput.value = adminResourceRequestMinutes[kind] ?? '';
          queueInput.placeholder = '待确认';
          queueInput.title = '正在确认上次申请，重试获取后可调整';
        }
        const save = queueInput.closest('.queue-preferences').querySelector('[data-save-queue]');
        if (save) save.disabled = adminResourceBusy[kind];
      }
    }
    card.querySelectorAll('[data-resource-action]').forEach(button => {
      button.disabled = adminResourceBusy[kind];
      button.addEventListener('click', () => adminResourceAction(kind, button.dataset.resourceAction));
    });
    updateAdminResourceCountdowns();
  }
  async function adminResourceAction(kind, action) {
    if (adminResourceBusy[kind]) return;
    const order = adminResources[kind];
    if (action !== 'create' && !order) return;
    if (order?.status === 'queued' && !['cancel', 'queue-wait'].includes(action)) return;
    if (action === 'create' && !adminResourceTerminal(order)) return;
    const pendingRequest = action === 'create' ? readAdminRequest(kind) : '';
    let queueMinutes;
    if (kind === 'email' && ['create', 'queue-wait'].includes(action)) {
      try { queueMinutes = pendingRequest ? adminResourceRequestMinutes[kind] ?? undefined : queuePreferenceValue('admin-email-queue-minutes'); } catch (error) { fieldError(`admin-resource-error-${kind}`, error.message); return; }
    }
    adminResourceBusy[kind] = true; ++adminResourceEpoch[kind]; adminResourceErrors[kind] = '';
    const generation = adminGeneration;
    let requestID = '';
    if (action === 'create') { requestID = pendingRequest || newRequestID(); saveAdminRequest(kind, requestID, queueMinutes); }
    const button = byId(`admin-resource-${kind}`)?.querySelector(action === 'queue-wait' ? '[data-save-queue]' : `[data-resource-action="${action}"]`);
    byId(`admin-resource-${kind}`)?.querySelectorAll('[data-resource-action], .queue-duration-input, [data-save-queue]').forEach(el => { el.disabled = true; });
    busy(button, true); fieldError(`admin-resource-error-${kind}`, '');
    try {
      const path = action === 'create' ? '/api/admin/resources' : `/api/admin/resources/${encodeURIComponent(order.id)}/${action}`;
      const body = action === 'create' ? { kind, request_id: requestID, ...(kind === 'email' ? { queue_minutes: queueMinutes } : {}) } : action === 'next-code' ? { round: Number(order.mail_round) || 1 } : action === 'queue-wait' ? { queue_minutes: queueMinutes } : {};
      const data = await api(path, { method: 'POST', body });
      if (action === 'create' && readAdminRequest(kind) === requestID) saveAdminRequest(kind, '');
      if (action === 'queue-wait') { queueDrafts.delete(`admin-email-queue-minutes:${order.id}`); notify(data.order.status === 'queued' ? '排队时间已调整' : '排队已结束'); }
      adminResources[kind] = data.order;
      mailAlerts?.observe(data.order, { freshAllocation: action === 'create' && data.order.id !== order?.id });
      if (generation === adminGeneration) renderAdminResource(kind);
      refreshInventory();
    } catch (error) {
      adminResourceErrors[kind] = error.message;
      if (generation === adminGeneration) {
        if (error.status === 401) await loadAdminAccess();
        else renderAdminResource(kind);
      }
    } finally {
      adminResourceBusy[kind] = false;
      if (byId(`admin-resource-${kind}`)) renderAdminResource(kind);
      resumeReminderWatch();
    }
  }
  function updateAdminResourceCountdowns() {
    for (const kind of ['phone', 'email']) {
      const order = adminResources[kind];
      const card = byId(`admin-resource-${kind}`);
      if (!order || !card) continue;
      const inQueue = queueClock(order);
      const expiry = Date.parse(inQueue ? order.queue_expires_at : order.expires_at);
      const seconds = Number.isFinite(expiry) ? Math.max(0, Math.ceil((expiry - now()) / 1000)) : 0;
      const countdown = card.querySelector('[data-admin-countdown]');
      if (countdown) countdown.textContent = seconds > 0 ? `${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}` : inQueue ? '正在确认排队结果' : '确认到期状态';
      const progress = card.querySelector('[data-admin-progress]');
      if (progress) { const duration = Math.max(1, expiry - Date.parse(order.created_at)); progress.style.width = `${Math.max(0, Math.min(100, (expiry - now()) / duration * 100))}%`; }
      const cancel = card.querySelector('[data-resource-action="cancel"]');
      if (cancel) {
        const queued = order.status === 'queued';
        const seconds = queued ? 0 : Math.max(0, Math.ceil((Date.parse(order.cancel_after) - now()) / 1000)) || 0;
        cancel.disabled = adminResourceBusy[kind] || seconds > 0;
        cancel.textContent = queued ? '取消排队' : seconds > 0 ? `${seconds} 秒后可取消` : '取消本次';
      }
    }
  }
  async function pollAdminResource(kind) {
    const order = adminResources[kind];
    const watchQueuedEmail = mailAlerts?.shouldPollHidden(order);
    if ((document.hidden && !watchQueuedEmail) || !order || !activeStatus(order.status) || adminResourceBusy[kind] || adminResourcePolling[kind] || (!byId(`admin-resource-${kind}`) && !watchQueuedEmail)) return;
    adminResourcePolling[kind] = true;
    const generation = adminGeneration;
    const epoch = adminResourceEpoch[kind];
    try {
      const data = await api(`/api/admin/resources/${encodeURIComponent(order.id)}`);
      if (generation !== adminGeneration || epoch !== adminResourceEpoch[kind]) return;
      const previous = adminResources[kind];
      const changed = !sameOrderView(previous, data.order);
      adminResources[kind] = data.order; adminResourceErrors[kind] = '';
      mailAlerts?.observe(data.order);
      if (changed) renderAdminResource(kind);
      else { fieldError(`admin-resource-error-${kind}`, ''); updateAdminResourceCountdowns(); }
      if (previous && queueClock(previous) && !queueClock(data.order)) { refreshInventory(); announce(orderStatusName(data.order)); }
    } catch (error) {
      if (generation !== adminGeneration || epoch !== adminResourceEpoch[kind]) return;
      if (error.status === 401) await loadAdminAccess();
      else { adminResourceErrors[kind] = error.message; fieldError(`admin-resource-error-${kind}`, error.message); }
    } finally { adminResourcePolling[kind] = false; }
  }
  function startAdminResources() {
    stopAdminResources();
    adminResourcesClock = setInterval(() => { if (!document.hidden) updateAdminResourceCountdowns(); }, 1000);
    adminResourcesTimer = setInterval(() => { pollAdminResource('phone'); pollAdminResource('email'); }, 5000);
  }
  function stopAdminResources() { clearInterval(adminResourcesTimer); clearInterval(adminResourcesClock); adminResourcesTimer = null; adminResourcesClock = null; }

  function renderOverview(data) {
    const s = data.stats || {};
    const stats = [['全部 CDK', s.total, 'ticket'], ['可兑换', s.available, 'star'], ['进行中', s.active, 'clock'], ['已完成', s.completed, 'check'], ['待核查', s.review, 'shield']];
    byId('main').innerHTML = `<div class="animate-in"><div class="page-title"><h1>总览</h1></div><div class="stats-grid">${stats.map(([label, value, mark], i) => `<div class="stat-card animate-in" style="animation-delay:${i * .045}s"><span class="stat-label">${label}</span><strong class="stat-value">${Number(value) || 0}</strong><span class="stat-decoration" aria-hidden="true">${icon(mark)}</span></div>`).join('')}</div><section class="panel"><div class="panel-header"><h2>最近订单</h2><button class="icon-button" id="refresh-overview" aria-label="刷新订单" title="刷新订单" style="width:31px;height:31px">${icon('refresh')}</button></div>${data.orders?.length ? `<div class="table-wrap" tabindex="0" aria-label="最近订单，可横向滚动"><table><thead><tr><th>资源</th><th>CDK</th><th>状态</th><th>时间</th><th>操作</th></tr></thead><tbody>${data.orders.map(order => `<tr><td>${kindLabel(order.kind)}<span class="table-sub table-resource" title="${esc(order.resource || '')}">${esc(order.resource || '待分配')}</span></td><td><div class="table-code-line"><span class="table-code">${order.source === 'admin' ? '管理员' : esc(order.cdk_code || order.masked_code)}</span>${order.cdk_code ? `<button class="copy-button table-copy" type="button" data-copy-order-cdk="${esc(order.id)}" aria-label="复制订单 CDK" title="复制 CDK">${icon('copy')}</button>` : ''}</div>${order.note ? `<span class="table-sub table-note" title="${esc(order.note)}">${esc(order.note)}</span>` : ''}</td><td>${badge(order.status, orderStatusName(order))}</td><td>${formatDate(order.created_at)}${order.source === 'admin' ? '' : `<span class="table-sub">已用 ${Number(order.used_count) || 0} / ${Number(order.usage_limit) || 1}</span>`}</td><td>${['review', 'next_uncertain'].includes(order.status) ? `<button class="row-action" data-resolve="${esc(order.id)}">核查</button>` : `<button class="row-action" data-view-order="${esc(order.id)}">详情</button>`}</td></tr>`).join('')}</tbody></table></div>` : `<div class="empty">${icon('inbox')}暂无订单</div>`}</section></div>`;
    byId('refresh-overview').addEventListener('click', () => showAdminTab('overview'));
    document.querySelectorAll('[data-resolve]').forEach(button => button.addEventListener('click', () => resolveDialog(data.orders.find(order => order.id === button.dataset.resolve))));
    document.querySelectorAll('[data-view-order]').forEach(button => button.addEventListener('click', () => orderDetails(data.orders.find(order => order.id === button.dataset.viewOrder))));
    document.querySelectorAll('[data-copy-order-cdk]').forEach(button => button.addEventListener('click', () => {
      const order = data.orders.find(item => item.id === button.dataset.copyOrderCdk);
      if (order?.cdk_code) copyText(order.cdk_code);
    }));
  }
  function orderDetails(order) {
    if (!order) return;
    openDialog('订单详情', `<div class="dialog-body"><p>${kindLabel(order.kind)}</p><p style="margin-top:16px;overflow-wrap:anywhere">${esc(order.resource || '待分配')}</p><p style="margin:10px 0">${badge(order.status, orderStatusName(order))}</p><p class="inline-note" style="overflow-wrap:anywhere">订单：${esc(order.id)}<br>上游：${esc(order.provider_id || '—')}<br>创建：${formatDate(order.created_at)}<br>${queuedBefore(order) ? '排队截止' : '本地等待截止'}：${formatDate(queuedBefore(order) ? order.queue_expires_at : order.expires_at)}${Array.isArray(order.codes) && order.codes.length ? order.codes.map(item => `<br>第 ${Number(item.round) || 1} 封：${esc(item.code)}`).join('') : order.code ? `<br>验证码：${esc(order.code)}` : ''}${order.note ? `<br>备注：${esc(order.note)}` : ''}${order.message ? `<br>${esc(orderMessage(order))}` : ''}</p></div><div class="dialog-actions"><button class="button button-secondary" data-close>关闭</button></div>`);
  }
  function resolveDialog(order) {
    if (!order) return;
    const hasReceipt = Boolean(order.code) || (Array.isArray(order.codes) && order.codes.length > 0);
    openDialog('核查订单', `<p class="dialog-body">请先在上游确认资源的实际状态。</p><p class="inline-note" style="margin-top:12px;overflow-wrap:anywhere">上游编号：${esc(order.provider_id || '—')}<br>资源：${esc(order.resource || '—')}</p><form id="resolve-form"><div class="resolution-choices"><label class="resolution-choice"><input type="radio" name="resolution" value="released" required ${hasReceipt ? 'disabled' : ''}><span><strong>已释放</strong><span>${order.source === 'admin' ? (hasReceipt ? '本资源已收码' : '确认资源已释放') : (hasReceipt ? '本资源已收码，不可恢复额度' : '未收码不扣额度')}</span></span></label><label class="resolution-choice"><input type="radio" name="resolution" value="consumed" required><span><strong>已消耗</strong><span>${order.source === 'admin' ? '结束本次资源' : (hasReceipt ? '结束资源，已用额度不重复扣除' : '计入 1 个有效资源')}</span></span></label></div><p id="resolve-error" class="error-text" role="alert"></p><div class="dialog-actions"><button type="button" class="button button-ghost" data-close>取消</button><button type="submit" class="button button-primary">确认处理</button></div></form>`, () => {
      byId('resolve-form').addEventListener('submit', async event => {
        event.preventDefault(); const button = event.currentTarget.querySelector('[type="submit"]'); busy(button, true); fieldError('resolve-error', '');
        try { await api(`/api/admin/orders/${encodeURIComponent(order.id)}/resolve`, { method: 'POST', body: { resolution: new FormData(event.currentTarget).get('resolution') } }); closeDialog(); notify('订单已处理'); showAdminTab('overview'); }
        catch (error) { fieldError('resolve-error', error.message); } finally { busy(button, false); }
      });
    });
  }

  function getCDKs() { return api(`/api/admin/cdks?${new URLSearchParams({ query: listState.query, status: listState.status, page: listState.page })}`); }
  function renderCDKs(data) {
    const page = Number(data.page) || 1;
    const pages = Math.max(1, Math.ceil((Number(data.total) || 0) / (Number(data.page_size) || 50)));
    byId('main').innerHTML = `<div class="animate-in cdk-page">
      <div class="page-title"><div class="title-with-count"><h1>CDK</h1><span class="count-chip">${Number(data.total) || 0}</span></div><button id="create-cdk" class="button button-primary">${icon('plus')}生成 CDK</button></div>
      <section class="panel cdk-panel">
        <form id="cdk-filter" class="cdk-toolbar" role="search">
          <div class="search-field">${icon('search')}<label for="cdk-query" class="sr-only">搜索批次或备注</label><input id="cdk-query" name="query" value="${esc(listState.query)}" placeholder="搜索批次 / 备注" maxlength="200"></div>
          <label class="sr-only" for="cdk-status">CDK 状态</label><select id="cdk-status" name="status"><option value="">全部状态</option>${['available', 'active', 'used', 'disabled', 'review', 'exhausted'].map(s => `<option value="${s}" ${listState.status === s ? 'selected' : ''}>${statusNames[s]}</option>`).join('')}</select>
          <button type="submit" class="icon-button cdk-search-button" aria-label="搜索">${icon('arrow')}</button>
        </form>
        ${data.items?.length ? `<div class="table-wrap" tabindex="0" aria-label="CDK 列表，可横向滚动"><table class="cdk-table">
          <thead><tr><th scope="col">CDK</th><th scope="col">批次</th><th scope="col">类型</th><th scope="col">状态</th><th scope="col">已用 / 额度</th><th scope="col">有效期</th><th scope="col">备注</th><th scope="col">操作</th></tr></thead>
          <tbody>${data.items.map(cdk => `<tr>
            <td class="cdk-code-cell"><div class="table-code-line"><span class="table-code">${esc(cdk.code || cdk.masked_code)}</span>${cdk.code ? `<button class="copy-button table-copy" type="button" data-copy-cdk="${esc(cdk.id)}" aria-label="复制 CDK ${esc(cdk.code)}" title="复制 CDK">${icon('copy')}</button>` : `<button class="row-action" type="button" data-restore-cdk="${esc(cdk.id)}" title="补录完整 CDK">补录</button>`}</div></td>
            <td class="cdk-batch-cell" title="${esc(cdk.batch_id)}"><span>${esc(cdk.batch_id?.slice(-12) || '—')}</span></td>
            <td>${kindLabel(cdk.kind)}</td><td>${badge(cdk.status)}</td>
            <td class="cdk-quota-cell">${Number(cdk.used_count) || 0} <span class="muted">/ ${Number(cdk.usage_limit) || 1}</span></td>
            <td class="cdk-date-cell">${formatDate(cdk.expires_at)}<span class="table-sub">创建 ${formatDate(cdk.created_at, true)}</span></td>
            <td class="table-note" title="${esc(cdk.note)}">${esc(cdk.note || '—')}</td>
            <td>${cdk.status === 'available' ? `<button class="row-action" data-disable="${esc(cdk.id)}">停用</button>` : '<span class="muted">—</span>'}</td>
          </tr>`).join('')}</tbody>
        </table></div>` : `<div class="empty">${icon('ticket')}${listState.query || listState.status ? '未找到匹配的 CDK' : '暂无 CDK'}</div>`}
        <div class="pagination"><span>共 ${Number(data.total) || 0} 个</span><div class="pagination-buttons"><button type="button" id="prev-page" ${page <= 1 ? 'disabled' : ''} aria-label="上一页">${icon('back')}</button><span>${page} / ${pages}</span><button type="button" id="next-page" ${page >= pages ? 'disabled' : ''} aria-label="下一页">${icon('arrow')}</button></div></div>
      </section>
    </div>`;
    byId('create-cdk').addEventListener('click', createCDKDialog);
    byId('cdk-filter').addEventListener('submit', event => { event.preventDefault(); listState = { query: byId('cdk-query').value.trim(), status: byId('cdk-status').value, page: 1 }; showAdminTab('cdks'); });
    byId('cdk-status').addEventListener('change', () => byId('cdk-filter').requestSubmit());
    byId('prev-page').addEventListener('click', () => { listState.page = Math.max(1, page - 1); showAdminTab('cdks'); });
    byId('next-page').addEventListener('click', () => { listState.page = page + 1; showAdminTab('cdks'); });
    document.querySelectorAll('[data-disable]').forEach(button => button.addEventListener('click', () => disableCDK(button.dataset.disable)));
    document.querySelectorAll('[data-copy-cdk]').forEach(button => button.addEventListener('click', () => {
      const cdk = data.items.find(item => item.id === button.dataset.copyCdk);
      if (cdk?.code) copyText(cdk.code);
    }));
    document.querySelectorAll('[data-restore-cdk]').forEach(button => button.addEventListener('click', () => restoreCDK(data.items.find(item => item.id === button.dataset.restoreCdk))));
  }
  function restoreCDK(cdk) {
    if (!cdk) return;
    openDialog('补录完整 CDK', `<p class="inline-note">旧记录未保存完整码，可从已导出的文件补录。</p><form id="restore-cdk-form" class="dialog-form"><div class="field"><label for="restore-cdk-code">CDK</label><input id="restore-cdk-code" name="code" value="" placeholder="${esc(cdk.masked_code)}" required maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false"></div><p id="restore-cdk-error" class="error-text" role="alert"></p><div class="dialog-actions"><button class="button button-ghost" type="button" data-close>取消</button><button class="button button-primary" type="submit">保存</button></div></form>`, () => {
      byId('restore-cdk-form').addEventListener('submit', async event => {
        event.preventDefault(); const form = event.currentTarget; const button = form.querySelector('[type="submit"]'); busy(button, true); fieldError('restore-cdk-error', '');
        try { await api(`/api/admin/cdks/${encodeURIComponent(cdk.id)}/code`, { method: 'POST', body: { code: new FormData(form).get('code').trim() } }); closeDialog(); notify('CDK 已补录'); showAdminTab('cdks'); }
        catch (error) { fieldError('restore-cdk-error', error.message); } finally { busy(button, false); }
      });
    });
  }
  function disableCDK(id) {
    openDialog('停用 CDK', '<p class="dialog-body">停用后，这个 CDK 将无法兑换资源。</p><p id="disable-error" class="error-text" role="alert"></p><div class="dialog-actions"><button class="button button-ghost" data-close>取消</button><button class="button button-danger" id="confirm-disable">确认停用</button></div>', () => {
      byId('confirm-disable').addEventListener('click', async event => {
        busy(event.currentTarget, true);
        try { await api(`/api/admin/cdks/${encodeURIComponent(id)}/disable`, { method: 'POST', body: {} }); closeDialog(); notify('CDK 已停用'); showAdminTab('cdks'); }
        catch (error) { fieldError('disable-error', error.message); } finally { busy(byId('confirm-disable'), false); }
      });
    });
  }
  function createCDKDialog() {
    openDialog('生成 CDK', `<form id="create-cdk-form" class="dialog-form"><div class="field"><label for="new-kind">资源类型</label><select id="new-kind" name="kind"><option value="phone">OpenAI 手机号</option><option value="email">OpenAI 邮箱</option></select></div><div class="field-grid"><div class="field"><label for="new-quantity">数量</label><input id="new-quantity" name="quantity" type="number" min="1" max="200" value="10" required></div><div class="field"><label for="new-days">CDK 有效天数</label><input id="new-days" name="expires_days" type="number" min="1" max="365" value="30" required></div></div><div class="field"><label for="new-usage-limit" id="usage-limit-label">有效号码数</label><input id="new-usage-limit" name="usage_limit" type="number" min="1" max="1000" value="3" required><p class="inline-note" id="usage-limit-note">首次收到验证码计 1 次。</p></div><div class="field"><label for="new-note">备注</label><input id="new-note" name="note" maxlength="200" placeholder="可选"></div><p id="create-cdk-error" class="error-text" role="alert"></p><div class="dialog-actions"><button type="button" class="button button-ghost" data-close>取消</button><button type="submit" class="button button-primary">生成</button></div></form>`, () => {
      byId('new-kind').addEventListener('change', event => {
        const email = event.target.value === 'email';
        byId('usage-limit-label').textContent = email ? '有效邮箱数' : '有效号码数';
        byId('usage-limit-note').textContent = email ? '每邮箱首次收码计 1 次，可接收 3 封。' : '首次收到验证码计 1 次。';
      });
      byId('create-cdk-form').addEventListener('submit', async event => {
        event.preventDefault(); const form = new FormData(event.currentTarget); const button = event.currentTarget.querySelector('[type="submit"]'); busy(button, true); fieldError('create-cdk-error', '');
        try {
          const data = await api('/api/admin/cdks', { method: 'POST', body: { kind: form.get('kind'), quantity: Number(form.get('quantity')), expires_days: Number(form.get('expires_days')), usage_limit: Number(form.get('usage_limit')), note: form.get('note') } });
          generatedCodes = data.codes; generatedBatch = data.batch_id;
          generatedExportURL = typeof data.export_url === 'string' && /^\/api\/admin\/exports\/[a-zA-Z0-9]{1,256}$/.test(data.export_url) ? data.export_url : '';
          showGeneratedCodes(); showAdminTab('cdks');
        } catch (error) { fieldError('create-cdk-error', error.message); } finally { busy(button, false); }
      });
    });
  }
  function showGeneratedCodes() {
    const exportControl = generatedExportURL
      ? `<a href="${esc(generatedExportURL)}" download class="button button-primary button-small" id="export-codes">${icon('download')}导出 CSV</a>`
      : `<button class="button button-primary button-small" id="export-codes">${icon('download')}导出 CSV</button>`;
    openDialog('CDK 已生成', `<div class="generated-meta"><span>${generatedCodes.length} 个</span><span title="${esc(generatedBatch)}">${esc(generatedBatch.slice(-12))}</span></div><pre class="new-codes" id="generated-codes" tabindex="0"></pre><div class="dialog-actions" style="justify-content:space-between"><button class="button button-ghost button-small" id="copy-all-codes">${icon('copy')}复制全部</button>${exportControl}<button class="button button-secondary button-small" data-close>完成</button></div>`, () => {
      byId('generated-codes').textContent = generatedCodes.join('\n');
      byId('copy-all-codes').addEventListener('click', () => copyText(generatedCodes.join('\n')));
      if (!generatedExportURL) byId('export-codes').addEventListener('click', () => {
        const csvCell = value => `"${String(value).replace(/"/g, '""')}"`;
        const csv = '\uFEFFCDK,批次\r\n' + generatedCodes.map(code => `${csvCell(code)},${csvCell(generatedBatch)}`).join('\r\n');
        const url = URL.createObjectURL(new Blob([csv], { type: 'text/csv;charset=utf-8;' }));
        const link = document.createElement('a'); link.href = url; link.download = `CDK-${generatedBatch.replace(/[^a-zA-Z0-9_-]/g, '')}.csv`; (dialog.open ? dialog : document.body).append(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url), 60000); notify('已开始导出');
      });
    });
  }

  function renderSettings(settings) {
    function field(name, label, extra = '') { return `<div class="field"><label for="setting-${name}">${label}</label><input id="setting-${name}" name="${name}" value="${esc(settings[name])}" ${extra}></div>`; }
    function toggle(name, label) { return `<label class="switch-label" for="setting-${name}">${label}<span class="switch"><input id="setting-${name}" type="checkbox" name="${name}" ${settings[name] ? 'checked' : ''}><span class="switch-track"></span></span></label>`; }
    byId('main').innerHTML = `<div class="animate-in"><div class="page-title"><h1>设置</h1></div><form id="settings-form"><div class="settings-grid"><section class="panel settings-panel full"><div class="settings-section-header"><h2>${icon('shield')}服务连接</h2><span id="api-key-status" class="pill ${settings.api_configured ? 'pill-live' : 'pill-demo'}" role="status">${settings.api_configured ? '已配置' : '未配置'}</span></div><div class="field"><label for="setting-api_key">API 密钥</label><input id="setting-api_key" name="api_key" type="password" value="" autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="512" placeholder="${settings.api_configured ? '已保存，输入可替换' : '输入 SMSBower API 密钥'}"></div></section><section class="panel settings-panel"><h2>${icon('phone')}手机号</h2>${toggle('phone_enabled', '开放兑换')}<div class="field-grid">${field('phone_service', '服务代码', 'required maxlength="32"')}${field('phone_country', '国家代码', 'inputmode="numeric" maxlength="5" placeholder="国家编号"')}</div><div class="field-grid">${field('phone_max_price', '价格上限', 'inputmode="decimal" maxlength="30" placeholder="生成 CDK 前必填"')}${field('phone_ttl_minutes', '本地等待（分钟）', 'type="number" min="3" max="60" required')}</div></section><section class="panel settings-panel"><h2>${icon('email')}邮箱</h2>${toggle('email_enabled', '开放兑换')}<div class="field-grid">${field('email_service', '服务代码', 'required maxlength="32"')}${field('email_domain', '邮箱域名', 'maxlength="128" placeholder="上游默认"')}</div><div class="field-grid">${field('email_max_price', '价格上限', 'inputmode="decimal" maxlength="30" placeholder="生成 CDK 前必填"')}${field('email_ttl_minutes', '本地等待（分钟）', 'type="number" min="1" max="60" required')}</div></section><section class="panel settings-panel full"><h2>${icon('image')}外观</h2><div class="field-grid">${field('brand', '站点名称', 'required maxlength="24"')}<div class="field"><label for="setting-background_type">背景</label><select id="setting-background_type" name="background_type">${[['none', '奶白色'], ['image', '图片'], ['video', '视频']].map(([value, label]) => `<option value="${value}" ${settings.background_type === value ? 'selected' : ''}>${label}</option>`).join('')}</select></div></div><div class="field" id="background-url-field" ${settings.background_type === 'none' ? 'hidden' : ''}><label for="setting-background_url">媒体地址</label><input id="setting-background_url" name="background_url" value="${esc(settings.background_url)}" placeholder="https://…" maxlength="2000"></div></section></div><div class="settings-bottom"><p id="settings-error" class="error-text" role="alert"></p><span id="settings-saved" class="saved-note" role="status"></span><button type="submit" class="button button-primary">${icon('check')}保存设置</button></div></form></div>`;
    window.SystemUpdatePanel?.mount(byId('settings-form').parentElement, { api, icon, esc, notify });
    byId('setting-background_type').addEventListener('change', event => { byId('background-url-field').hidden = event.target.value === 'none'; byId('setting-background_url').required = event.target.value !== 'none'; });
    byId('settings-form').addEventListener('input', () => { byId('settings-saved').textContent = ''; });
    byId('settings-form').addEventListener('submit', async event => {
      event.preventDefault(); const form = new FormData(event.currentTarget); const button = event.currentTarget.querySelector('[type="submit"]'); busy(button, true); fieldError('settings-error', '');
      const body = Object.fromEntries(form.entries());
      body.phone_enabled = form.has('phone_enabled'); body.email_enabled = form.has('email_enabled'); body.phone_ttl_minutes = Number(body.phone_ttl_minutes); body.email_ttl_minutes = Number(body.email_ttl_minutes);
      try {
        const result = await api('/api/admin/settings', { method: 'PUT', body });
        config = { ...config, ...result, configured: result.api_configured }; applyBackground(config); mailAlerts?.setBaseTitle(config.brand || '拾光 · Atelier');
        const keyInput = byId('setting-api_key');
        if (keyInput) { keyInput.value = ''; keyInput.placeholder = result.api_configured ? '已保存，输入可替换' : '输入 SMSBower API 密钥'; }
        const keyStatus = byId('api-key-status');
        if (keyStatus) { keyStatus.textContent = result.api_configured ? '已配置' : '未配置'; keyStatus.className = `pill ${result.api_configured ? 'pill-live' : 'pill-demo'}`; }
        const saved = byId('settings-saved');
        if (saved) saved.textContent = '已保存';
        document.querySelectorAll('.brand').forEach(el => { el.innerHTML = brandHTML(); }); refreshInventory(); notify('设置已保存');
      } catch (error) { fieldError('settings-error', error.message); } finally { busy(button, false); }
    });
  }

  function openDialog(title, content, callback) {
    if (dialog.open) dialog.close();
    dialog.innerHTML = `<div class="dialog-header"><h2 id="dialog-title">${esc(title)}</h2><button type="button" class="icon-button" data-close aria-label="关闭">${icon('close')}</button></div>${content}<p id="dialog-notice" class="dialog-notice" role="status" aria-live="polite"></p>`;
    dialog.querySelectorAll('[data-close]').forEach(button => button.addEventListener('click', closeDialog));
    dialog.showModal(); callback?.();
  }
  function closeDialog() { dialog.close(); }
  dialog.addEventListener('close', () => { if (!dialog.open) { generatedCodes = []; generatedBatch = ''; generatedExportURL = ''; dialog.replaceChildren(); } });
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    refreshInventory();
    if (!isAdmin && token && currentOrder && activeStatus(currentOrder.status)) pollOrder();
    if (isAdmin && byId('admin-resource-email')) { updateAdminResourceCountdowns(); pollAdminResource('phone'); pollAdminResource('email'); }
  });
  window.addEventListener('pagehide', () => { stopPolling(); stopInventory(); stopAdminResources(); });
  window.addEventListener('pageshow', event => {
    if (!event.persisted) return;
    if (document.querySelector('[data-inventory]')) startInventory();
    if (!isAdmin && token && currentOrder && activeStatus(currentOrder.status)) { startPolling(); pollOrder(); }
    if (isAdmin && byId('logout') && (byId('admin-resource-email') || mailAlerts?.shouldPollHidden(adminResources.email))) { startAdminResources(); pollAdminResource('phone'); pollAdminResource('email'); }
  });

  async function init() {
    try { config = { ...config, ...await api('/api/config') }; mailAlerts?.setBaseTitle(config.brand || '拾光 · Atelier'); applyBackground(config); }
    catch (error) {
      app.innerHTML = `<main id="main" class="boot"><div style="text-align:center;padding:30px"><span class="boot-mark" aria-hidden="true">${icon('star')}</span><p id="boot-error" class="error-text" style="margin:20px 0"></p><button id="boot-retry" class="button button-primary">重新连接</button></div></main>`;
      byId('boot-error').textContent = error.message; byId('boot-retry').addEventListener('click', init); return;
    }
    if (isAdmin) {
      try { await api('/api/admin/session'); renderAdminShell(); await showAdminTab(adminTab); }
      catch (error) { if (error.status === 401) await loadAdminAccess(); else renderAdminEntryError(error, init); }
    } else {
      publicShell(); bindReminders(); startInventory();
      if (token) { byId('exchange').innerHTML = '<div class="empty">正在恢复订单…</div>'; await pollOrder(true); if (currentOrder && activeStatus(currentOrder.status)) startPolling(); }
      else renderRedeem();
    }
  }
  init();
})();
