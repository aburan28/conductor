// A minimal reactive store: one object, patch-style updates, subscribers notified with the
// keys that changed. Views subscribe for what they care about and unsubscribe on destroy.
export function createStore(initial) {
  let state = { ...initial };
  const subs = new Set();
  return {
    get: () => state,
    set(patch) {
      const next = typeof patch === 'function' ? patch(state) : patch;
      const changed = Object.keys(next).filter(k => state[k] !== next[k]);
      if (!changed.length) return;
      state = { ...state, ...next };
      for (const fn of subs) {
        try { fn(state, changed); } catch (err) { console.error(err); }
      }
    },
    subscribe(fn, keys) {
      const wrapped = keys ? (s, changed) => { if (changed.some(k => keys.includes(k))) fn(s, changed); } : fn;
      subs.add(wrapped);
      return () => subs.delete(wrapped);
    },
  };
}

// Persisted preferences, tolerant of storage being unavailable (private windows, sandboxes).
export const prefs = {
  get(key, fallback) {
    try { const v = localStorage.getItem('conductor_' + key); return v == null ? fallback : JSON.parse(v); }
    catch (_) { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem('conductor_' + key, JSON.stringify(value)); } catch (_) { /* ignore */ }
  },
  del(key) {
    try { localStorage.removeItem('conductor_' + key); } catch (_) { /* ignore */ }
  },
};
