import { h, icon } from '../lib/dom.js';

let host;
function ensureHost() {
  if (!host) {
    host = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' });
    document.body.append(host);
  }
  return host;
}

export function toast(message, { kind = 'ok', detail = '', ttl = 4500 } = {}) {
  const el = h('div', { class: 'toast ' + kind },
    h('div', { class: 't-body' }, message, detail ? h('small', {}, detail) : null),
    h('span', { class: 'x', role: 'button', 'aria-label': 'dismiss', onclick: () => el.remove() }, icon('close', 14)));
  ensureHost().append(el);
  if (ttl > 0) setTimeout(() => { if (el.isConnected) el.remove(); }, ttl);
  return el;
}

export function toastError(err, prefix = '') {
  const msg = err && err.message ? err.message : String(err);
  return toast((prefix ? prefix + ': ' : '') + msg, { kind: 'danger', ttl: 7000 });
}
