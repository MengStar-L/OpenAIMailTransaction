'use strict';
(() => {
  let current = null;
  function amountText(value) {
    if (typeof value !== 'string' || !/^\d{1,24}(?:\.\d{1,12})?$/.test(value)) return null;
    const [whole, decimals = ''] = value.split('.');
    const integer = whole.replace(/^0+(?=\d)/, '').replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    const fraction = decimals.replace(/0+$/, '').padEnd(2, '0');
    return `$${integer}.${fraction}`;
  }
  function start(container, { api }) {
    stop();
    // Loading the shared frontend on the redemption page must never query an admin balance.
    if (!container?.isConnected || window.location.pathname.replace(/\/$/, '') !== '/admin') return;
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'pill admin-balance';
    button.setAttribute('aria-label', '账户余额，正在读取');
    button.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="15" rx="4"/><path d="M17 13h4M3 8V6a2 2 0 0 1 1.7-2l11-1"/><circle cx="16" cy="13" r=".7"/></svg><span data-balance-label>余额</span><strong data-balance-amount>—</strong><span class="balance-demo" data-balance-demo hidden>模拟</span>';
    container.append(button);
    let stopped = false, busy = false, followup = false;
    let timer;
    function render(data, error = '') {
      const formatted = data?.available === true && data.currency === 'USD' ? amountText(data.balance) : null;
      const available = formatted !== null;
      const demo = data?.mode === 'demo' || data?.status === 'demo';
      const description = error || data?.message || (available ? '上游账户余额' : '余额暂时不可用');
      const date = data?.updated_at ? new Date(data.updated_at) : null;
      const updated = date && Number.isFinite(date.getTime()) ? `更新于 ${date.toLocaleTimeString('zh-CN', { hour12: false })}` : '';
      button.classList.toggle('balance-unavailable', !available);
      button.querySelector('[data-balance-label]').textContent = available ? '余额' : '余额不可用';
      button.querySelector('[data-balance-amount]').textContent = formatted || '—';
      button.querySelector('[data-balance-demo]').hidden = !demo;
      button.title = [description, available ? `${formatted} USD` : '', demo ? '演示余额，不代表上游真实资金' : '', updated, '点击刷新'].filter(Boolean).join(' · ');
      button.setAttribute('aria-label', `${available ? `账户余额 ${formatted} USD` : '账户余额暂时不可用'}${demo ? '，模拟数据' : ''}，点击刷新`);
    }
    async function refresh(force = false) {
      if (stopped || !button.isConnected) { if (!stopped) dispose(); return; }
      if (document.hidden) { followup ||= force; return; }
      if (busy) { followup ||= force; return; }
      const bypass = force || followup;
      followup = false; busy = true;
      button.setAttribute('aria-busy', 'true');
      try {
        const data = await api(`/api/admin/balance${bypass ? '?refresh=1' : ''}`);
        if (!stopped && button.isConnected) render(data);
      } catch (error) {
        if (stopped) return;
        if (error.status === 401) { dispose(); return; }
        render(null, error.message || '暂时无法获取余额');
      } finally {
        busy = false;
        if (!stopped) {
          button.setAttribute('aria-busy', 'false');
          if (followup && !document.hidden) refresh(true);
        }
      }
    }
    function visible() { if (!document.hidden) refresh(); }
    function dispose() {
      stopped = true; followup = false; clearInterval(timer);
      document.removeEventListener('visibilitychange', visible);
      button.remove();
    }
    button.title = '正在读取账户余额';
    button.addEventListener('click', () => refresh(true));
    document.addEventListener('visibilitychange', visible);
    timer = setInterval(() => refresh(), 15000);
    current = { refresh, dispose };
    refresh();
  }
  function stop() { current?.dispose(); current = null; }
  window.AdminBalance = Object.freeze({ start, stop, refresh(force = false) { return current?.refresh(force); } });
})();
