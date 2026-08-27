import { h, icon } from '../lib/dom.js';

export function pill(state, label) {
  return h('span', { class: 'pill ' + String(state || '').toLowerCase() }, label || String(state || '').replace(/_/g, ' '));
}

export function chip(text, { kind = '', mono = true, title, onRemove } = {}) {
  return h('span', { class: 'chip' + (mono ? '' : ' sans') + (kind ? ' ' + kind : ''), title: title || text },
    text, onRemove ? h('span', { class: 'x', role: 'button', 'aria-label': 'remove', onclick: onRemove }, icon('close', 11)) : null);
}

export function chips(list, opts) {
  return h('div', { class: 'chips' }, (list || []).map(t => chip(t, opts)));
}

export function severityChip(sev) {
  const kind = sev === 'high' || sev === 'critical' ? 'danger' : sev === 'medium' ? 'warn' : 'info';
  return chip(sev, { kind, mono: false });
}

export function riskChip(risk) {
  if (!risk || risk === 'unknown') return null;
  const kind = risk === 'high' || risk === 'critical' ? 'danger' : risk === 'medium' ? 'warn' : '';
  return chip('risk ' + risk, { kind, mono: false });
}

export function tierChip(tier) {
  return tier ? chip('tier ' + tier, { kind: 'info', mono: false }) : null;
}

export function effortChip(effort, ceiling) {
  if (!effort && !ceiling) return null;
  const text = ceiling && ceiling !== effort ? `effort ${effort || '?'} (up to ${ceiling})` : `effort ${effort || ceiling}`;
  return chip(text, { mono: false });
}
