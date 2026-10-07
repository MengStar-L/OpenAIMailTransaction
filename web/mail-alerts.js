/* A mailbox arriving is different from a verification message arriving. */
'use strict';
(() => {
  const PREFERENCES_KEY = 'shiguang.mail.alert.preferences.v1';
  const OBSERVATIONS_KEY = 'shiguang.mail.alert.observations.v1';
  const MAX_OBSERVATIONS = 96;
  const OBSERVATION_LIFETIME = 48 * 60 * 60 * 1000;
  const TITLE_PREFIX = '【邮箱已就绪】';
  const READY_STATUSES = new Set(['waiting', 'received', 'next_pending', 'next_uncertain']);
  const WAITING_STATUSES = new Set(['queued', 'allocating']);
  const AudioContextClass = window.AudioContext || window.webkitAudioContext;
  let preferences = readPreferences();
  let observations = readObservations();
  let baseTitle = document.title.replace(/^【邮箱已就绪】\s*/, '') || '拾光 · Atelier';
  let attention = false;
  let titleTimer = null;
  let audioContext = null;
  let disposed = false;
  const tones = new Set();

  function readJSON(storageName, key) {
    try { return JSON.parse(window[storageName].getItem(key)); } catch { return null; }
  }
  function writeJSON(storageName, key, value) {
    try { window[storageName].setItem(key, JSON.stringify(value)); } catch { /* Private browsing may deny storage. */ }
  }
  function readPreferences() {
    const saved = readJSON('localStorage', PREFERENCES_KEY);
    return { sound: saved?.sound !== false, tab: saved?.tab !== false };
  }
  function readObservations() {
    const saved = readJSON('sessionStorage', OBSERVATIONS_KEY);
    const result = new Map();
    if (!Array.isArray(saved)) return result;
    const cutoff = Date.now() - OBSERVATION_LIFETIME;
    for (const item of saved.slice(-MAX_OBSERVATIONS)) {
      if (Array.isArray(item) && typeof item[0] === 'string' && item[0].length <= 128 &&
          ['pending', 'seen'].includes(item[1]) && Number.isFinite(item[2]) && item[2] > cutoff && item[2] <= Date.now()) {
        result.set(item[0], { state: item[1], at: item[2] });
      }
    }
    return result;
  }
  function remember(id, state) {
    if (observations.get(id)?.state === state) return;
    observations.delete(id);
    observations.set(id, { state, at: Date.now() });
    const cutoff = Date.now() - OBSERVATION_LIFETIME;
    for (const [key, value] of observations) {
      if (value.at <= cutoff) observations.delete(key);
    }
    while (observations.size > MAX_OBSERVATIONS) observations.delete(observations.keys().next().value);
    // Only opaque order IDs and transition markers are persisted, never addresses or codes.
    writeJSON('sessionStorage', OBSERVATIONS_KEY, [...observations].map(([key, value]) => [key, value.state, value.at]));
  }
  function getPreferences() { return { ...preferences }; }
  function getState() {
    return {
      preferences: getPreferences(),
      audioSupported: !!AudioContextClass,
      audioLocked: !!AudioContextClass && preferences.sound && audioContext?.state !== 'running',
      attention,
    };
  }
  function emitChange() {
    if (!disposed) window.dispatchEvent(new CustomEvent('mail-alerts-change', { detail: getState() }));
  }
  function isVisibleAndFocused() {
    return !document.hidden && (typeof document.hasFocus !== 'function' || document.hasFocus());
  }
  function clearTitleTimer() {
    if (titleTimer !== null) window.clearTimeout(titleTimer);
    titleTimer = null;
  }
  function clearAttention() {
    clearTitleTimer();
    const changed = attention;
    attention = false;
    document.title = baseTitle;
    if (changed) emitChange();
  }
  function showAttention() {
    if (!preferences.tab || disposed) return;
    clearTitleTimer();
    attention = true;
    document.title = `${TITLE_PREFIX} ${baseTitle}`;
    if (isVisibleAndFocused()) titleTimer = window.setTimeout(clearAttention, 8000);
    emitChange();
  }
  function setBaseTitle(title) {
    baseTitle = String(title || '拾光 · Atelier').replace(/^【邮箱已就绪】\s*/, '');
    document.title = attention ? `${TITLE_PREFIX} ${baseTitle}` : baseTitle;
  }
  function stopTones() {
    for (const tone of tones) {
      try { tone.oscillator.stop(); } catch { /* A completed oscillator may already be stopped. */ }
      try { tone.oscillator.disconnect(); tone.gain.disconnect(); } catch { /* Best effort cleanup. */ }
    }
    tones.clear();
  }
  async function armAudio() {
    if (disposed || !preferences.sound || !AudioContextClass) return false;
    try {
      if (!audioContext || audioContext.state === 'closed') {
        audioContext = new AudioContextClass();
        audioContext.addEventListener?.('statechange', emitChange);
      }
      if (audioContext.state !== 'running') await audioContext.resume();
      emitChange();
      return audioContext.state === 'running';
    } catch {
      // A denied gesture or audio device failure must not interrupt allocation.
      emitChange();
      return false;
    }
  }
  function playChime() {
    if (!preferences.sound || !audioContext || audioContext.state !== 'running' || disposed) return false;
    try {
      const start = audioContext.currentTime + 0.02;
      [659.25, 830.61, 987.77].forEach((frequency, index) => {
        const oscillator = audioContext.createOscillator();
        const gain = audioContext.createGain();
        const at = start + index * 0.17;
        const tone = { oscillator, gain };
        oscillator.type = 'sine';
        oscillator.frequency.setValueAtTime(frequency, at);
        gain.gain.setValueAtTime(0, at);
        gain.gain.linearRampToValueAtTime(0.075, at + 0.018);
        gain.gain.exponentialRampToValueAtTime(0.001, at + 0.56);
        oscillator.connect(gain);
        gain.connect(audioContext.destination);
        oscillator.addEventListener('ended', () => {
          tones.delete(tone);
          oscillator.disconnect();
          gain.disconnect();
        }, { once: true });
        tones.add(tone);
        oscillator.start(at);
        oscillator.stop(at + 0.6);
      });
      return true;
    } catch {
      stopTones();
      emitChange();
      return false;
    }
  }
  function setPreference(name, enabled) {
    if (disposed || !['sound', 'tab'].includes(name)) return getPreferences();
    preferences[name] = !!enabled;
    writeJSON('localStorage', PREFERENCES_KEY, preferences);
    if (name === 'tab' && !enabled) clearAttention();
    if (name === 'sound') {
      if (enabled) void armAudio();
      else stopTones();
    }
    emitChange();
    return getPreferences();
  }
  function observe(order, options = {}) {
    if (disposed || !order || order.kind !== 'email' || typeof order.id !== 'string' || !order.id || order.id.length > 128) return false;
    const previous = observations.get(order.id);
    const resource = typeof order.resource === 'string' && order.resource.trim().length > 0;
    if (!resource && WAITING_STATUSES.has(order.status)) {
      // Once an allocation has been seen, stale polling responses cannot re-arm it.
      if (previous?.state !== 'seen') remember(order.id, 'pending');
      return false;
    }
    if (!resource || !READY_STATUSES.has(order.status)) {
      remember(order.id, 'seen');
      return false;
    }
    const arrived = previous?.state !== 'seen' && (previous?.state === 'pending' || options.freshAllocation === true);
    remember(order.id, 'seen');
    if (!arrived) return false;
    // Persist before effects so polling, reloads, and failed audio cannot duplicate alerts.
    playChime();
    showAttention();
    window.dispatchEvent(new CustomEvent('mail-arrival', { detail: { kind: 'email' } }));
    return true;
  }
  function shouldPollHidden(order) {
    return !disposed && (preferences.sound || preferences.tab) && order?.kind === 'email' &&
      !order.resource && WAITING_STATUSES.has(order.status);
  }
  function onGesture(event) {
    if (event.isTrusted === false || (event.type === 'keydown' && (event.repeat || event.ctrlKey || event.metaKey || event.altKey))) return;
    if (getState().audioLocked) void armAudio();
  }
  function onVisibilityChange() {
    if (document.hidden) clearTitleTimer();
    else if (isVisibleAndFocused()) clearAttention();
  }
  function onFocus() { if (isVisibleAndFocused()) clearAttention(); }
  function onStorage(event) {
    if (event.key !== PREFERENCES_KEY) return;
    preferences = readPreferences();
    if (!preferences.tab) clearAttention();
    if (!preferences.sound) stopTones();
    emitChange();
  }
  function onPageHide(event) {
    if (!event.persisted) dispose();
    else { clearTitleTimer(); stopTones(); }
  }
  function onPageShow() { onVisibilityChange(); emitChange(); }
  function dispose() {
    if (disposed) return;
    clearAttention();
    disposed = true;
    stopTones();
    document.removeEventListener('pointerdown', onGesture, true);
    document.removeEventListener('keydown', onGesture, true);
    document.removeEventListener('visibilitychange', onVisibilityChange);
    window.removeEventListener('focus', onFocus);
    window.removeEventListener('storage', onStorage);
    window.removeEventListener('pagehide', onPageHide);
    window.removeEventListener('pageshow', onPageShow);
    if (audioContext) {
      audioContext.removeEventListener?.('statechange', emitChange);
      try { Promise.resolve(audioContext.close()).catch(() => {}); } catch { /* Context may already be closed. */ }
    }
  }

  document.addEventListener('pointerdown', onGesture, true);
  document.addEventListener('keydown', onGesture, true);
  document.addEventListener('visibilitychange', onVisibilityChange);
  window.addEventListener('focus', onFocus);
  window.addEventListener('storage', onStorage);
  window.addEventListener('pagehide', onPageHide);
  window.addEventListener('pageshow', onPageShow);
  window.MailArrivalAlerts = Object.freeze({
    observe, getPreferences, setPreference, armAudio, getState,
    setBaseTitle, clearAttention, shouldPollHidden, dispose,
  });
})();
