'use strict';
(() => {
  function mount(container, { api, icon, esc, notify }) {
    const host = document.createElement('section');
    host.className = 'panel settings-panel update-panel';
    host.setAttribute('aria-label', '程序更新');
    host.innerHTML = `<h2>${icon('refresh')}程序更新</h2><p class="inline-note">正在读取版本…</p>`;
    container.append(host);
    let state = null;
    let busy = false;
    let lastRender = '';
    let reconnecting = false;
    const phaseLabels = { checking: '正在检查', downloading: '正在下载', verifying: '正在校验', restarting: '正在重启', deferred: '等待订单结束', error: '更新未完成' };
    const installing = () => state && ['downloading', 'verifying', 'restarting'].includes(state.phase);
    function render(force = false) {
      if (!host.isConnected || !state) return;
      const signature = JSON.stringify(state) + busy + reconnecting;
      if (!force && lastRender === signature) return;
      lastRender = signature;
      const blocked = busy || installing() || state.phase === 'checking';
      const status = phaseLabels[state.phase] || (state.available ? '发现新版本' : state.last_checked && !state.last_checked.startsWith('0001') ? '已是最新版本' : '尚未检查');
      const toggle = (name, label) => `<label class="switch-label" for="update-${name}">${label}<span class="switch"><input id="update-${name}" type="checkbox" data-update-setting="${name}" ${state.settings[name] ? 'checked' : ''} ${busy || (name === 'auto_update' && !state.supported) ? 'disabled' : ''}><span class="switch-track"></span></span></label>`;
      host.innerHTML = `<div class="settings-section-header"><h2>${icon('refresh')}程序更新</h2><span class="pill" role="status">${esc(reconnecting ? '正在重新连接' : status)}</span></div><div class="update-versions"><span>当前 <strong>${esc(state.current_version)}</strong></span>${state.latest_version ? `<span>最新 <strong>${esc(state.latest_version)}</strong></span>` : ''}</div><div class="update-preferences">${toggle('auto_check', '自动检查')}${toggle('auto_update', '自动更新')}</div><div class="update-actions"><button type="button" class="button button-ghost button-small" data-update-check ${blocked ? 'disabled' : ''}>${icon('refresh')}检查更新</button><button type="button" class="button button-primary button-small" data-update-apply ${blocked || !state.available || !state.supported ? 'disabled' : ''}>${icon('download')}立即更新</button>${state.release_url && /^https:\/\/github\.com\//.test(state.release_url) ? `<a href="${esc(state.release_url)}" target="_blank" rel="noopener noreferrer" class="text-button">更新记录</a>` : ''}</div><p class="inline-note update-note">${state.supported ? '每 6 小时检查，订单结束后安装。' : '当前运行方式支持检查，请安装正式发行版后启用自动更新。'}</p><p class="error-text" data-update-error role="alert">${esc(state.error || '')}</p>`;
      host.querySelector('[data-update-check]').addEventListener('click', () => action('/api/admin/updates/check'));
      host.querySelector('[data-update-apply]').addEventListener('click', () => action('/api/admin/updates/apply'));
      host.querySelectorAll('[data-update-setting]').forEach(input => input.addEventListener('change', async () => {
        const settings = { ...state.settings, [input.dataset.updateSetting]: input.checked };
        if (input.dataset.updateSetting === 'auto_check' && !input.checked) settings.auto_update = false;
        if (settings.auto_update) settings.auto_check = true;
        await action('/api/admin/updates/settings', 'PUT', settings);
      }));
    }
    async function action(path, method = 'POST', body = {}) {
      if (busy || !state) return;
      busy = true; render();
      try {
        state = await api(path, { method, body });
        reconnecting = path.endsWith('/apply');
        if (path.endsWith('/settings')) notify('更新设置已保存');
      } catch (error) {
        notify(error.message, true);
      } finally { busy = false; render(true); }
    }
    async function refresh() {
      if (!host.isConnected) { clearInterval(timer); return; }
      if (busy || document.hidden) return;
      try {
        const next = await api('/api/admin/updates');
        if (reconnecting && next.current_version !== state?.current_version) notify('程序已更新');
        if (!['downloading', 'verifying', 'restarting'].includes(next.phase)) reconnecting = false;
        state = next; render();
      } catch (error) {
        if (!host.isConnected) return;
        if (error.status === 401) {
          clearInterval(timer);
          notify('请重新登录后查看程序版本');
          window.location.assign('/admin');
          return;
        }
        if (installing() || reconnecting) { reconnecting = true; render(); }
        else if (state) { host.querySelector('[data-update-error]').textContent = error.message; }
        else {
          host.innerHTML = `<h2>${icon('refresh')}程序更新</h2><p class="error-text">${esc(error.message)}</p><button type="button" class="button button-ghost button-small">重试</button>`;
          host.querySelector('button').addEventListener('click', refresh);
        }
      }
    }
    const timer = setInterval(refresh, 5000);
    refresh();
  }
  window.SystemUpdatePanel = Object.freeze({ mount });
})();
