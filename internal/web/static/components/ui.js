import { h, icon, copyText } from '../lib/dom.js';
import { toast } from './toast.js';

export function card({ title, actions, body, footer, flush = false, id }) {
  return h('section', { class: 'card', id },
    title ? h('header', {}, h('h2', {}, title), actions) : null,
    h('div', { class: 'body' + (flush ? ' flush' : '') }, body),
    footer ? h('footer', {}, footer) : null);
}

export function kpi({ label, value, unit, sub, kind = '', onClick }) {
  return h('div', { class: 'kpi ' + kind, role: onClick ? 'button' : null, tabindex: onClick ? 0 : null, onclick: onClick,
    onkeydown: onClick ? ev => { if (ev.key === 'Enter') onClick(); } : null },
    h('div', { class: 'label' }, label),
    h('div', { class: 'value' }, String(value), unit ? h('small', {}, unit) : null),
    sub ? h('div', { class: 'sub' }, sub) : null);
}

export function empty(message, cmd) {
  return h('div', { class: 'empty' }, message, cmd ? h('div', {}, h('code', { class: 'cmd' }, cmd)) : null);
}

export function errorBox(err, retry) {
  const msg = err && err.message ? err.message : String(err);
  return h('div', { class: 'error-box', role: 'alert' }, msg, retry ? h('button', { class: 'btn sm', onclick: retry }, icon('refresh'), 'Retry') : null);
}

export function skeleton(lines = 3) {
  const widths = ['80%', '60%', '70%', '50%', '65%'];
  return h('div', { class: 'skeleton', 'aria-busy': 'true', 'aria-label': 'loading' },
    Array.from({ length: lines }, (_, i) => h('div', { class: 'bar', style: { width: widths[i % widths.length] } })));
}

export function copyButton(text, label = 'Copy') {
  const btn = h('button', { class: 'btn sm copy', type: 'button', 'aria-label': 'copy to clipboard' }, icon('copy', 13), label);
  btn.addEventListener('click', async () => {
    const ok = await copyText(typeof text === 'function' ? text() : text);
    toast(ok ? 'Copied' : 'Copy failed — select and copy by hand', { kind: ok ? 'ok' : 'warn', ttl: 1500 });
  });
  return btn;
}

export function snippet(code, { language } = {}) {
  return h('div', { class: 'snippet' }, h('pre', { 'data-lang': language }, code), copyButton(code));
}

export function meter(part, whole, { warnAt = 0.75, dangerAt = 0.95 } = {}) {
  const frac = whole > 0 ? Math.max(0, Math.min(1, part / whole)) : 0;
  const kind = frac >= dangerAt ? 'danger' : frac >= warnAt ? 'warn' : '';
  return h('div', { class: 'bar-meter', role: 'meter', 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': Math.round(frac * 100) },
    h('div', { class: kind, style: { width: Math.round(frac * 100) + '%' } }));
}

export function segmented(options, value, onChange) {
  const el = h('div', { class: 'seg', role: 'tablist' });
  const render = () => {
    el.replaceChildren(...options.map(o => h('button', { type: 'button', class: o.value === value ? 'active' : '', role: 'tab', 'aria-selected': o.value === value,
      onclick: () => { value = o.value; render(); onChange(value); } }, o.label)));
  };
  render();
  return el;
}

export function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.filter(p => p && p[1] != null && p[1] !== '').map(([k, v]) => [h('dt', {}, k), h('dd', {}, v)]));
}

export function taskLink(ref, project, navigate) {
  return h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(ref)}`, 'data-link': true }, ref);
}

// Minimal Markdown renderer for task cards: headings, lists, fenced code, paragraphs. Text
// is inserted as text nodes, so card content cannot inject markup.
export function markdown(text) {
  const root = h('div', { class: 'md' });
  const lines = String(text || '').split('\n');
  let i = 0;
  let list = null;
  const flushList = () => { if (list) { root.append(list); list = null; } };
  // Skip YAML frontmatter; the structured fields are rendered elsewhere.
  if (lines[0] === '---') { i = 1; while (i < lines.length && lines[i] !== '---') i++; i++; }
  for (; i < lines.length; i++) {
    const line = lines[i];
    if (line.startsWith('```')) {
      flushList();
      const buf = [];
      i++;
      while (i < lines.length && !lines[i].startsWith('```')) buf.push(lines[i++]);
      root.append(h('pre', {}, buf.join('\n')));
      continue;
    }
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) { flushList(); root.append(h('h' + Math.min(3, heading[1].length), {}, heading[2])); continue; }
    const item = /^\s*[-*]\s+(.*)$/.exec(line);
    if (item) { if (!list) list = h('ul'); list.append(h('li', {}, item[1])); continue; }
    flushList();
    if (line.trim()) root.append(h('p', {}, line));
  }
  flushList();
  return root;
}
