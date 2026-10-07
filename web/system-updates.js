'use strict';
(() => {
  const PENDING_KEY = 'atelier.update.pending';
  const activePhases = ['downloading', 'verifying', 'restarting'];
  const phaseLabels = { preparing: '准备更新', checking: '正在检查', downloading: '正在下载', verifying: '正在校验', restarting: '正在重启', reconnecting: '正在重新连接', deferred: '等待订单结束', success: '更新完成', error: '更新未完成', login: '请重新登录', unconfirmed: '等待确认版本' };
  let controller = null;
  const normalVersion = value => String(value || '').replace(/^v/, '').split('+')[0];
  function reachedVersion(current, target) {
    const a = normalVersion(current).split('.'), b = normalVersion(target).split('.');
    if (a.length !== 3 || b.length !== 3 || [...a, ...b].some(part => !/^\d+$/.test(part))) return false;
    for (let i = 0; i < 3; i++) { if (Number(a[i]) !== Number(b[i])) return Number(a[i]) > Number(b[i]); }
    return true;
  }
  function readPending() {
    try {
      const value = JSON.parse(window.sessionStorage.getItem(PENDING_KEY));
      if (value && typeof value.from === 'string' && typeof value.to === 'string' && Number.isFinite(value.started) && Date.now() - value.started < 30 * 60 * 1000) return value;
      window.sessionStorage.removeItem(PENDING_KEY);
    } catch { /* Storage can be disabled; the current page still tracks updates. */ }
    return null;
  }
  function savePending(value) {
    try { if (value) window.sessionStorage.setItem(PENDING_KEY, JSON.stringify(value)); else window.sessionStorage.removeItem(PENDING_KEY); } catch { /* Optional persistence. */ }
  }
  const bytes = value => value < 1048576 ? `${(value / 1024).toFixed(1)} KB` : `${(value / 1048576).toFixed(1)} MB`;

  function createController({ api, icon, esc, notify }) {
    const hosts = new Set();
    const signatures = new WeakMap();
    let state = null, pending = readPending(), progressPhase = pending ? 'reconnecting' : '', progressError = '';
    let busy = false, polling = false, stopped = false, globalWatch = false, reconnecting = !!pending, dismissed = false;
    let dialog = null, reopen = null, lastFocused = null, lastRequestError = '';
    let timer, actionEpoch = 0;
    const installing = () => activePhases.includes(state?.phase);
    const updating = () => !!pending || installing();

    function clearPending() { pending = null; savePending(null); }
    function beginUpdate(phase = 'preparing') {
      if (!pending) {
        pending = { from: state?.current_version || '', to: state?.latest_version || '', started: Date.now() };
        savePending(pending);
        dismissed = false;
      }
      progressPhase = phase; progressError = ''; reconnecting = phase === 'reconnecting';
      renderProgress();
    }
    function ensureDialog() {
      if (dialog) return;
      dialog = document.createElement('dialog');
      dialog.className = 'update-dialog';
      dialog.setAttribute('aria-labelledby', 'update-dialog-title');
      dialog.setAttribute('aria-describedby', 'update-dialog-detail');
      dialog.innerHTML = `<button class="icon-button update-dialog-close" type="button" data-update-dismiss aria-label="收起更新进度">${icon('close')}</button><div class="update-emblem" aria-hidden="true"><img src="/assets/shiguang-icon-128.png" width="72" height="72" alt=""><span class="update-emblem-star">${icon('star')}</span></div><p class="update-dialog-eyebrow">程序更新</p><h2 id="update-dialog-title" role="status" aria-live="polite"></h2><p class="update-dialog-version" data-update-version></p><ol class="update-stages" aria-label="更新阶段">${['准备', '下载', '校验', '重启', '完成'].map((label, index) => `<li data-update-stage="${index}"><span aria-hidden="true">${index + 1}</span>${label}</li>`).join('')}</ol><div class="update-transfer"><div class="update-transfer-heading"><span data-update-transfer-label></span><span data-update-transfer-value></span></div><progress class="update-progress" max="100" aria-label="更新进度"></progress></div><p id="update-dialog-detail" class="update-dialog-detail"></p><div class="update-dialog-actions"><button type="button" class="button button-ghost" data-update-hide>后台继续</button><button type="button" class="button button-primary" data-update-result hidden></button></div><p class="update-dialog-footnote" data-update-footnote>收起窗口不影响更新</p>`;
      dialog.querySelector('[data-update-dismiss]').addEventListener('click', hideProgress);
      dialog.querySelector('[data-update-hide]').addEventListener('click', hideProgress);
      dialog.addEventListener('cancel', event => { event.preventDefault(); hideProgress(); });
      dialog.querySelector('[data-update-result]').addEventListener('click', () => {
        if (progressPhase === 'login') window.location.assign('/admin');
        else if (progressPhase === 'success') window.location.reload();
        else { progressPhase = 'reconnecting'; progressError = ''; reconnecting = true; startTimer(); renderProgress(); refresh(); }
      });
      reopen = document.createElement('button');
      reopen.type = 'button'; reopen.className = 'update-reopen';
      reopen.setAttribute('aria-haspopup', 'dialog');
      reopen.innerHTML = `${icon('refresh')}<span data-update-reopen-label>查看更新进度</span>`;
      reopen.hidden = true;
      reopen.addEventListener('click', () => { dismissed = false; showProgress(); });
      document.body.append(dialog, reopen);
    }
    function showProgress() {
      ensureDialog();
      if (!dialog.open) { lastFocused = document.activeElement; dialog.showModal(); }
      reopen.hidden = true;
    }
    function hideProgress() {
      dismissed = true;
      dialog?.close();
      // A completed/error result stays accessible until the admin leaves the page.
      if (reopen) reopen.hidden = !progressPhase;
      if (lastFocused?.isConnected && !lastFocused.disabled) lastFocused.focus({ preventScroll: true });
      else if (reopen && !reopen.hidden) reopen.focus({ preventScroll: true });
    }
    function renderProgress() {
      if (!progressPhase) return;
      ensureDialog();
      const terminal = ['success', 'error', 'login', 'unconfirmed', 'deferred'].includes(progressPhase);
      const index = { preparing: 0, checking: 0, downloading: 1, verifying: 2, restarting: 3, reconnecting: 3, success: 4 }[progressPhase] ?? -1;
      dialog.dataset.phase = progressPhase;
      dialog.querySelector('#update-dialog-title').textContent = phaseLabels[progressPhase] || '正在更新';
      const target = pending?.to || state?.latest_version || '';
      dialog.querySelector('[data-update-version]').textContent = progressPhase === 'success' ? `当前版本 ${state?.current_version || ''}` : target ? `${pending?.from || state?.current_version || ''} → ${target}` : '';
      dialog.querySelectorAll('[data-update-stage]').forEach(item => {
        const step = Number(item.dataset.updateStage);
        item.classList.toggle('is-complete', index >= 0 && step < index);
        item.classList.toggle('is-active', step === index);
        if (step === index) item.setAttribute('aria-current', 'step'); else item.removeAttribute('aria-current');
      });
      const downloaded = Math.max(0, Number(state?.downloaded_bytes) || 0), total = Math.max(0, Number(state?.total_bytes) || 0);
      const determinate = progressPhase === 'downloading' && total > 0;
      const percent = determinate ? Math.min(100, Math.floor(downloaded / total * 100)) : null;
      const bar = dialog.querySelector('progress');
      if (determinate) bar.value = percent; else if (progressPhase === 'success') bar.value = 100; else bar.removeAttribute('value');
      bar.hidden = terminal && progressPhase !== 'success';
      bar.setAttribute('aria-label', determinate ? '下载进度' : '更新进度');
      dialog.querySelector('[data-update-transfer-label]').textContent = progressPhase === 'downloading' ? '下载文件' : phaseLabels[progressPhase] || '';
      dialog.querySelector('[data-update-transfer-value]').textContent = determinate ? `${bytes(downloaded)} / ${bytes(total)} · ${percent}%` : progressPhase === 'downloading' && downloaded > 0 ? `已下载 ${bytes(downloaded)}` : '';
      const details = { preparing: '正在提交更新请求…', checking: '正在获取最新版本…', downloading: '正在下载发行文件', verifying: '正在核对文件，确保完整', restarting: '正在重启服务，请稍候', reconnecting: '服务重启中，正在等待连接恢复', success: '新版本已运行', deferred: '进行中的订单结束后再安装', login: '登录已失效，重新登录后确认更新结果', unconfirmed: '暂时无法确认更新结果，请稍后重试' };
      dialog.querySelector('#update-dialog-detail').textContent = progressError || details[progressPhase] || '更新未完成，请稍后重试';
      const hide = dialog.querySelector('[data-update-hide]');
      hide.textContent = terminal ? '关闭' : '后台继续';
      const result = dialog.querySelector('[data-update-result]');
      result.hidden = !['success', 'login', 'unconfirmed'].includes(progressPhase);
      result.textContent = progressPhase === 'success' ? '进入新版本' : progressPhase === 'login' ? '重新登录' : '重新连接';
      dialog.querySelector('[data-update-footnote]').textContent = terminal ? '' : '收起窗口不影响更新';
      reopen.querySelector('[data-update-reopen-label]').textContent = terminal ? phaseLabels[progressPhase] : '查看更新进度';
      if (!dismissed && !document.hidden) showProgress();
      else reopen.hidden = !!dialog.open;
    }
    function accept(next) {
      state = next; lastRequestError = '';
      if (pending && normalVersion(next.current_version) !== normalVersion(pending.from) && reachedVersion(next.current_version, pending.to)) {
        progressPhase = 'success'; progressError = ''; reconnecting = false; clearPending();
        notify('程序已更新');
      } else if (activePhases.includes(next.phase)) {
        beginUpdate(reconnecting && next.phase === 'restarting' ? 'reconnecting' : next.phase);
      } else if (pending && ['error', 'deferred'].includes(next.phase)) {
        progressPhase = next.phase; progressError = next.error || ''; reconnecting = false; clearPending();
      } else if (pending && Date.now() - pending.started > 10 * 60 * 1000) {
        reconnecting = false;
        if (next.phase === 'idle') {
          progressPhase = 'error'; progressError = `服务已恢复，当前版本仍为 ${next.current_version}，更新未完成`; clearPending();
        } else progressPhase = 'unconfirmed';
      }
      render();
    }
    function render() {
      for (const host of hosts) {
        if (!host.isConnected) { hosts.delete(host); continue; }
        if (!state) {
          if (lastRequestError) {
            host.innerHTML = `<h2>${icon('refresh')}程序更新</h2><p class="error-text">${esc(lastRequestError)}</p><button type="button" class="button button-ghost button-small">重试</button>`;
            host.querySelector('button').addEventListener('click', refresh);
          }
          continue;
        }
        const signature = JSON.stringify(state) + busy + reconnecting + lastRequestError + !!pending;
        if (signatures.get(host) === signature) continue;
        signatures.set(host, signature);
        const blocked = busy || updating() || state.phase === 'checking';
        const status = phaseLabels[state.phase] || (state.available ? '发现新版本' : state.last_checked && !state.last_checked.startsWith('0001') ? '已是最新版本' : '尚未检查');
        const toggle = (name, label) => `<label class="switch-label" for="update-${name}">${label}<span class="switch"><input id="update-${name}" type="checkbox" data-update-setting="${name}" ${state.settings[name] ? 'checked' : ''} ${busy || (name === 'auto_update' && !state.supported) ? 'disabled' : ''}><span class="switch-track"></span></span></label>`;
        host.innerHTML = `<div class="settings-section-header"><h2>${icon('refresh')}程序更新</h2><span class="pill" role="status">${esc(reconnecting ? '正在重新连接' : status)}</span></div><div class="update-versions"><span>当前 <strong>${esc(state.current_version)}</strong></span>${state.latest_version ? `<span>最新 <strong>${esc(state.latest_version)}</strong></span>` : ''}</div><div class="update-preferences">${toggle('auto_check', '自动检查')}${toggle('auto_update', '自动更新')}</div><div class="update-actions"><button type="button" class="button button-ghost button-small" data-update-check ${blocked ? 'disabled' : ''}>${icon('refresh')}检查更新</button><button type="button" class="button button-primary button-small" data-update-apply ${blocked || !state.available || !state.supported ? 'disabled' : ''}>${icon('download')}立即更新</button>${state.release_url && /^https:\/\/github\.com\//.test(state.release_url) ? `<a href="${esc(state.release_url)}" target="_blank" rel="noopener noreferrer" class="text-button">更新记录</a>` : ''}</div><p class="inline-note update-note">${state.supported ? '每 6 小时检查，订单结束后安装。' : '当前运行方式支持检查，请安装正式发行版后启用自动更新。'}</p><p class="error-text" data-update-error role="alert">${esc(lastRequestError || state.error || '')}</p>`;
        host.querySelector('[data-update-check]').addEventListener('click', () => action('/api/admin/updates/check'));
        host.querySelector('[data-update-apply]').addEventListener('click', () => action('/api/admin/updates/apply'));
        host.querySelectorAll('[data-update-setting]').forEach(input => input.addEventListener('change', async () => {
          const settings = { ...state.settings, [input.dataset.updateSetting]: input.checked };
          if (input.dataset.updateSetting === 'auto_check' && !input.checked) settings.auto_update = false;
          if (settings.auto_update) settings.auto_check = true;
          await action('/api/admin/updates/settings', 'PUT', settings);
        }));
      }
      renderProgress();
    }
    async function action(path, method = 'POST', body = {}) {
      if (busy || !state) return;
      const apply = path.endsWith('/apply');
      ++actionEpoch;
      busy = true;
      if (apply) beginUpdate(); // Render before sending; even slow requests have immediate feedback.
      render();
      try {
        const next = await api(path, { method, body });
        if (stopped) return;
        accept(next);
        if (path.endsWith('/settings')) notify('更新设置已保存');
      } catch (error) {
        if (stopped) return;
        if (apply && (!error.status || error.status >= 500)) {
          // The request may have reached the server. Confirm status; never repeat a mutation.
          progressPhase = 'reconnecting'; reconnecting = true; progressError = '';
        } else {
          if (apply) { progressPhase = 'error'; progressError = error.message; clearPending(); }
          lastRequestError = error.message; notify(error.message, true);
        }
      } finally { busy = false; if (!stopped) render(); }
    }
    function startTimer() { if (!timer && !stopped) timer = setInterval(refresh, 2000); }
    async function refresh() {
      if (stopped) return;
      if (!globalWatch && ![...hosts].some(host => host.isConnected)) { stop(); return; }
      if (busy || polling || document.hidden) return;
      polling = true;
      const epoch = actionEpoch;
      try {
        const next = await api('/api/admin/updates');
        if (!stopped && epoch === actionEpoch) accept(next);
      } catch (error) {
        if (stopped || epoch !== actionEpoch) return;
        if (error.status === 401) {
          clearInterval(timer); timer = null;
          if (updating()) {
            progressPhase = 'login'; progressError = ''; reconnecting = false; dismissed = false; render();
          } else {
            notify('请重新登录后查看程序版本'); window.location.assign('/admin');
          }
        } else if (updating()) {
          reconnecting = true;
          progressPhase = Date.now() - pending?.started > 10 * 60 * 1000 ? 'unconfirmed' : 'reconnecting';
          render();
        } else { lastRequestError = error.message; render(); }
      } finally { polling = false; }
    }
    function stop() {
      stopped = true; clearInterval(timer); timer = null;
      dialog?.close(); dialog?.remove(); reopen?.remove(); hosts.clear();
    }
    return {
      watch() { globalWatch = true; startTimer(); refresh(); },
      mount(container) {
        const host = document.createElement('section');
        host.className = 'panel settings-panel update-panel'; host.setAttribute('aria-label', '程序更新');
        host.innerHTML = `<h2>${icon('refresh')}程序更新</h2><p class="inline-note">正在读取版本…</p>`;
        container.append(host); hosts.add(host); render(); startTimer();
        if (!state) refresh();
      }, stop,
    };
  }
  function ensure(options) { return controller ||= createController(options); }
  window.SystemUpdatePanel = Object.freeze({
    mount(container, options) { ensure(options).mount(container); },
    start(options) { ensure(options).watch(); },
    stop() { controller?.stop(); controller = null; },
  });
})();
