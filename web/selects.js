/* Accessible select menus; native controls remain the form's source of truth. */
'use strict';
(() => {
  if (window.ShiguangSelects) return;
  const records = new Set();
  const bySelect = new WeakMap();
  let openRecord = null;
  let sequence = 0;
  let refreshQueued = false;
  let positionFrame = 0;
  const observerOptions = { childList: true, subtree: true, characterData: true, attributes: true,
    attributeFilter: ['disabled', 'selected', 'value', 'label', 'hidden', 'multiple', 'required', 'aria-label', 'aria-labelledby', 'aria-describedby', 'aria-invalid', 'data-placeholder'] };
  const observer = new MutationObserver(changes => {
    if (changes.some(change => !change.target.closest?.('[data-sg-select-ui]'))) scheduleRefresh();
  });

  function attribute(node, name, value) {
    if (value === null || value === undefined || value === false) {
      if (node.hasAttribute(name)) node.removeAttribute(name);
    } else if (node.getAttribute(name) !== String(value)) node.setAttribute(name, String(value));
  }
  function text(node, value) { if (node.textContent !== value) node.textContent = value; }
  function eligible(option) { return !option.hidden && !option.disabled && !option.parentElement?.disabled && !option.parentElement?.hidden; }
  function enabledIndices(record) { return Array.from(record.select.options).flatMap((option, index) => eligible(option) ? [index] : []); }
  function scheduleRefresh() {
    if (refreshQueued) return;
    refreshQueued = true;
    queueMicrotask(() => { refreshQueued = false; refresh(); });
  }

  function enhance(select) {
    if (bySelect.has(select) || select.hasAttribute('data-native-select')) return;
    const id = `sg-select-${++sequence}`;
    const wrapper = document.createElement('span');
    wrapper.className = 'sg-select';
    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'sg-select-trigger';
    trigger.dataset.sgSelectUi = '';
    trigger.setAttribute('role', 'combobox');
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', `${id}-list`);
    const value = document.createElement('span');
    value.className = 'sg-select-value';
    value.id = `${id}-value`;
    const arrow = document.createElement('span');
    arrow.className = 'sg-select-arrow';
    arrow.setAttribute('aria-hidden', 'true');
    trigger.append(value, arrow);
    const popup = document.createElement('div');
    popup.id = `${id}-list`;
    popup.className = 'sg-select-menu';
    popup.dataset.sgSelectUi = '';
    popup.setAttribute('role', 'listbox');
    popup.hidden = true;
    const record = { select, wrapper, trigger, value, popup, rows: [], active: -1, signature: '', search: '', searchAt: 0,
      originalTabIndex: select.getAttribute('tabindex'), originalAriaHidden: select.getAttribute('aria-hidden'),
      popover: typeof popup.showPopover === 'function' };
    if (record.popover) popup.setAttribute('popover', 'manual');
    select.before(wrapper);
    wrapper.append(select, trigger);
    select.classList.add('sg-select-source');
    select.tabIndex = -1;
    select.setAttribute('aria-hidden', 'true');
    bySelect.set(select, record);
    records.add(record);
    trigger.addEventListener('click', () => openRecord === record ? close() : open(record));
    trigger.addEventListener('keydown', event => keydown(record, event));
    popup.addEventListener('pointerdown', event => event.preventDefault());
    popup.addEventListener('click', event => {
      const row = event.target.closest('[data-option-index]');
      if (row) choose(record, Number(row.dataset.optionIndex));
    });
    popup.addEventListener('pointermove', event => {
      const row = event.target.closest('[data-option-index]');
      if (row && event.pointerType !== 'touch') setActive(record, Number(row.dataset.optionIndex), false);
    });
    sync(record);
  }

  function sync(record) {
    const { select, wrapper, trigger, value, popup } = record;
    const options = Array.from(select.options);
    const selected = options.filter(option => option.selected).map(option => option.label);
    const placeholder = select.dataset.placeholder || '请选择';
    const summary = selected.length > 2 ? `${selected.slice(0, 2).join('、')} +${selected.length - 2}` : selected.join('、');
    text(value, summary || placeholder);
    attribute(trigger, 'title', selected.join('、') || placeholder);
    trigger.disabled = select.matches(':disabled');
    wrapper.hidden = select.hidden;
    attribute(trigger, 'aria-required', select.required ? 'true' : null);
    if (record.invalid && select.validity.valid) record.invalid = false;
    attribute(trigger, 'aria-invalid', select.getAttribute('aria-invalid') || (record.invalid ? 'true' : null));
    attribute(trigger, 'aria-describedby', select.getAttribute('aria-describedby'));
    const name = select.getAttribute('aria-label') || Array.from(select.labels || []).map(label => label.textContent.trim()).join(' ') || select.name || placeholder;
    const labelledBy = select.getAttribute('aria-labelledby');
    attribute(trigger, 'aria-labelledby', labelledBy);
    attribute(trigger, 'aria-label', labelledBy ? null : name);
    attribute(popup, 'aria-labelledby', labelledBy);
    attribute(popup, 'aria-label', labelledBy ? null : name);
    attribute(popup, 'aria-multiselectable', select.multiple ? 'true' : null);
    const signature = JSON.stringify(options.map(option => [option.label, option.value, option.selected, option.disabled,
      option.hidden, option.parentElement?.label, option.parentElement?.disabled, option.parentElement?.hidden]));
    if (record.signature !== signature) {
      record.signature = signature;
      const fragment = document.createDocumentFragment();
      record.rows = [];
      let previousGroup = null;
      options.forEach((option, index) => {
        if (option.hidden || option.parentElement?.hidden) return;
        const group = option.parentElement?.tagName === 'OPTGROUP' ? option.parentElement : null;
        if (group && group !== previousGroup) {
          const heading = document.createElement('div');
          heading.className = 'sg-select-group';
          heading.setAttribute('role', 'presentation');
          heading.textContent = group.label;
          fragment.append(heading);
        }
        previousGroup = group;
        const row = document.createElement('div');
        row.className = 'sg-select-option';
        row.id = `${popup.id}-${index}`;
        row.dataset.optionIndex = String(index);
        row.setAttribute('role', 'option');
        row.setAttribute('aria-selected', String(option.selected));
        if (!eligible(option)) row.setAttribute('aria-disabled', 'true');
        const label = document.createElement('span');
        label.textContent = option.label;
        const mark = document.createElement('span');
        mark.className = 'sg-select-check';
        mark.setAttribute('aria-hidden', 'true');
        row.append(label, mark);
        fragment.append(row);
        record.rows[index] = row;
      });
      if (!record.rows.length) {
        const empty = document.createElement('div');
        empty.className = 'sg-select-empty';
        empty.textContent = '暂无选项';
        fragment.append(empty);
      }
      popup.replaceChildren(fragment);
      if (openRecord === record) {
        const allowed = enabledIndices(record);
        setActive(record, allowed.includes(record.active) ? record.active : (allowed.includes(select.selectedIndex) ? select.selectedIndex : allowed[0] ?? -1));
        position(record);
      }
    }
    if (openRecord === record && (trigger.disabled || wrapper.hidden)) close();
  }

  function open(record) {
    sync(record);
    if (record.trigger.disabled || record.wrapper.hidden) return;
    close();
    openRecord = record;
    record.search = '';
    // A popover enters the top layer even inside an open modal dialog.
    // Legacy browsers instead keep the menu inside that dialog.
    const host = record.select.closest('dialog[open]') || document.body;
    host.append(record.popup);
    record.popup.hidden = false;
    if (record.popover) {
      try { record.popup.showPopover(); } catch { record.popover = false; record.popup.removeAttribute('popover'); }
    }
    record.trigger.setAttribute('aria-expanded', 'true');
    record.wrapper.classList.add('is-open');
    position(record);
    const allowed = enabledIndices(record);
    setActive(record, allowed.includes(record.select.selectedIndex) ? record.select.selectedIndex : allowed[0] ?? -1);
  }

  function close() {
    if (!openRecord) return;
    const record = openRecord;
    openRecord = null;
    if (record.popover) { try { record.popup.hidePopover(); } catch { /* A removed dialog may already close its popover. */ } }
    record.popup.hidden = true;
    record.popup.remove();
    record.trigger.setAttribute('aria-expanded', 'false');
    record.trigger.removeAttribute('aria-activedescendant');
    record.wrapper.classList.remove('is-open');
  }

  function position(record) {
    if (openRecord !== record || !record.trigger.isConnected) return;
    const rect = record.trigger.getBoundingClientRect();
    const viewport = window.visualViewport;
    const viewWidth = viewport?.width || window.innerWidth;
    const viewHeight = viewport?.height || window.innerHeight;
    const offsetX = viewport?.offsetLeft || 0;
    const offsetY = viewport?.offsetTop || 0;
    const margin = 10;
    const width = Math.min(Math.max(rect.width, 168), viewWidth - margin * 2);
    const below = Math.max(0, offsetY + viewHeight - rect.bottom - margin - 6);
    const above = Math.max(0, rect.top - offsetY - margin - 6);
    const upward = below < Math.min(240, record.popup.scrollHeight || 240) && above > below;
    const maxHeight = Math.min(288, upward ? above : below);
    record.popup.style.width = `${width}px`;
    record.popup.style.maxHeight = `${maxHeight}px`;
    const height = Math.min(record.popup.scrollHeight + 2, maxHeight);
    let left = Math.max(offsetX + margin, Math.min(rect.left, offsetX + viewWidth - width - margin));
    let top = upward ? rect.top - height - 6 : rect.bottom + 6;
    // Without popover support, a transformed dialog is the fixed-position containing block.
    const dialog = !record.popover && record.popup.closest('dialog');
    if (dialog && getComputedStyle(dialog).transform !== 'none') {
      const bounds = dialog.getBoundingClientRect();
      left -= bounds.left + dialog.clientLeft;
      top -= bounds.top + dialog.clientTop;
    }
    record.popup.style.left = `${left}px`;
    record.popup.style.top = `${top}px`;
    record.popup.dataset.side = upward ? 'top' : 'bottom';
  }

  function setActive(record, index, scroll = true) {
    if (index < 0) {
      record.active = -1;
      record.trigger.removeAttribute('aria-activedescendant');
      return;
    }
    if (!eligible(record.select.options[index] || { hidden: true })) return;
    record.active = index;
    record.rows.forEach((row, rowIndex) => row?.classList.toggle('is-active', rowIndex === index));
    const row = record.rows[index];
    if (!row) return;
    record.trigger.setAttribute('aria-activedescendant', row.id);
    if (scroll) {
      const top = row.offsetTop;
      const bottom = top + row.offsetHeight;
      if (top < record.popup.scrollTop) record.popup.scrollTop = top;
      else if (bottom > record.popup.scrollTop + record.popup.clientHeight) record.popup.scrollTop = bottom - record.popup.clientHeight;
    }
  }

  function choose(record, index) {
    const option = record.select.options[index];
    if (!option || !eligible(option) || record.select.matches(':disabled')) return;
    const previous = Array.from(record.select.selectedOptions, item => item.index).join(',');
    if (record.select.multiple) option.selected = !option.selected;
    else record.select.selectedIndex = index;
    const changed = previous !== Array.from(record.select.selectedOptions, item => item.index).join(',');
    sync(record);
    if (!record.select.multiple) close();
    record.trigger.focus({ preventScroll: true });
    if (changed) {
      record.select.dispatchEvent(new Event('input', { bubbles: true }));
      record.select.dispatchEvent(new Event('change', { bubbles: true }));
    }
  }

  function keydown(record, event) {
    if (record.trigger.disabled) return;
    const isOpen = openRecord === record;
    if (event.key === 'Tab') { close(); return; }
    if (event.key === 'Escape') {
      if (isOpen) { event.preventDefault(); event.stopPropagation(); close(); }
      return;
    }
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      if (isOpen) choose(record, record.active); else open(record);
      return;
    }
    if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      if (event.altKey && event.key === 'ArrowUp') { close(); return; }
      if (!isOpen) open(record);
      const allowed = enabledIndices(record);
      if (!allowed.length) return;
      let target = record.active;
      if (event.key === 'Home') target = allowed[0];
      else if (event.key === 'End') target = allowed.at(-1);
      else if (isOpen) target = allowed[Math.max(0, Math.min(allowed.length - 1, allowed.indexOf(record.active) + (event.key === 'ArrowDown' ? 1 : -1)))];
      setActive(record, target);
      return;
    }
    if (event.key.length !== 1 || event.ctrlKey || event.metaKey || event.altKey || event.isComposing) return;
    event.preventDefault();
    const now = Date.now();
    record.search = now - record.searchAt < 650 ? record.search + event.key : event.key;
    record.searchAt = now;
    const query = record.search.toLocaleLowerCase();
    const repeated = Array.from(query).every(letter => letter === query[0]);
    const prefix = repeated ? query[0] : query;
    const allowed = enabledIndices(record);
    const from = allowed.indexOf(isOpen ? record.active : record.select.selectedIndex);
    const ordered = repeated ? [...allowed.slice(from + 1), ...allowed.slice(0, from + 1)] : allowed;
    const target = ordered.find(index => record.select.options[index].label.trim().toLocaleLowerCase().startsWith(prefix));
    if (target === undefined) return;
    if (!isOpen) {
      const search = record.search;
      open(record);
      record.search = search;
    }
    setActive(record, target);
  }

  function dispose(record) {
    if (openRecord === record) close();
    record.popup.remove();
    record.trigger.remove();
    record.select.classList.remove('sg-select-source');
    attribute(record.select, 'tabindex', record.originalTabIndex);
    attribute(record.select, 'aria-hidden', record.originalAriaHidden);
    if (record.wrapper.contains(record.select)) record.wrapper.replaceWith(record.select);
    else record.wrapper.remove();
    bySelect.delete(record.select);
    records.delete(record);
  }

  function refresh(root = document) {
    observer.disconnect();
    try {
      for (const record of records) {
        if (!record.select.isConnected || !record.wrapper.contains(record.select) || record.select.hasAttribute('data-native-select')) dispose(record);
      }
      if (root.matches?.('select')) enhance(root);
      root.querySelectorAll?.('select').forEach(enhance);
      for (const record of records) sync(record);
    } finally { observer.observe(document.documentElement, observerOptions); }
  }

  function schedulePosition(event) {
    if (!openRecord || event?.target instanceof Node && openRecord.popup.contains(event.target) || positionFrame) return;
    positionFrame = requestAnimationFrame(() => {
      positionFrame = 0;
      if (openRecord) position(openRecord);
    });
  }
  document.addEventListener('pointerdown', event => {
    if (openRecord && !openRecord.wrapper.contains(event.target) && !openRecord.popup.contains(event.target)) close();
  }, true);
  document.addEventListener('focusin', event => {
    const record = bySelect.get(event.target);
    if (record) { sync(record); record.trigger.focus({ preventScroll: true }); }
    else if (openRecord && !openRecord.wrapper.contains(event.target) && !openRecord.popup.contains(event.target)) close();
  });
  for (const name of ['input', 'change']) document.addEventListener(name, event => {
    const record = bySelect.get(event.target);
    if (record) sync(record);
  });
  document.addEventListener('invalid', event => {
    const record = bySelect.get(event.target);
    if (record) { record.invalid = true; sync(record); record.trigger.focus(); }
  }, true);
  document.addEventListener('reset', () => setTimeout(refresh, 0), true);
  document.addEventListener('close', event => { if (openRecord && event.target.contains(openRecord.select)) close(); }, true);
  document.addEventListener('cancel', event => {
    if (openRecord && event.target.contains(openRecord.select)) { event.preventDefault(); close(); }
  }, true);
  window.addEventListener('resize', schedulePosition);
  window.addEventListener('scroll', schedulePosition, true);
  window.visualViewport?.addEventListener('resize', schedulePosition);
  window.visualViewport?.addEventListener('scroll', schedulePosition);
  window.ShiguangSelects = Object.freeze({ refresh, close });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => refresh(), { once: true });
  else refresh();
})();
