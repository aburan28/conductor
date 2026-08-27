import { h, icon } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty } from '../components/ui.js';
import { fmtTime } from '../lib/format.js';

const TYPE_KIND = t => /stalled|exhausted|expired|failed|block|conflict/.test(t) ? 'danger' : /downshift|released|reclaimed|warn|declined/.test(t) ? 'warn' : /claimed|done|succeeded|granted|shared|unblocked|accepted/.test(t) ? 'accent' : '';

export function eventLine(e, { fresh = false } = {}) {
  const payload = e.payload || {};
  const ref = payload.task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(payload.task_ref)}`, 'data-link': true }, payload.task_ref) : null;
  const rest = Object.entries(payload).filter(([k]) => k !== 'task_ref');
  const short = rest.slice(0, 4).map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`).join('  ');
  const line = h('div', { class: 'ev' + (fresh ? ' new' : ''), dataset: { type: e.type } },
    h('span', { class: 't' }, fmtTime(e.occurred_at)),
    h('span', { class: 'type ' + TYPE_KIND(e.type || '') }, e.type),
    h('span', { class: 'payload', title: 'click to expand' }, ref, ref ? ' ' : '', short, e.actor_principal ? h('span', { class: 'muted' }, '  by ' + String(e.actor_principal).slice(0, 8)) : null));
  line.querySelector('.payload').addEventListener('click', ev => {
    if (ev.target.closest('a')) return;
    line.classList.toggle('open');
    const p = line.querySelector('.payload');
    if (line.classList.contains('open')) p.replaceChildren(ref || '', ref ? '\n' : '', JSON.stringify(payload, null, 2));
    else p.replaceChildren(ref || '', ref ? ' ' : '', short);
  });
  return line;
}

export default defineView({
  title: 'Events',
  async load(ctx) {
    const out = await ctx.api.get(ctx.api.project(ctx.project, '/events?limit=200'));
    return { events: out.events || [] };
  },
  draw({ events }, ctx, { state }) {
    state.q = state.q || '';
    state.paused = !!state.paused;
    const log = h('div', { class: 'log', style: { maxHeight: '70vh' } });
    const filter = e => !state.q || (e.type || '').includes(state.q) || ((e.payload || {}).task_ref || '').toLowerCase().includes(state.q.toLowerCase());
    const draw = () => { const list = events.filter(filter); log.replaceChildren(...(list.length ? list.map(e => eventLine(e)) : [empty('No events match.', 'conductor status')])); };
    draw();
    // Live events arrive through the app's stream; the app calls view.push(event).
    state.push = e => {
      events.unshift(e);
      if (events.length > 500) events.pop();
      if (state.paused || !filter(e)) return;
      log.prepend(eventLine(e, { fresh: true }));
      while (log.childNodes.length > 500) log.removeChild(log.lastChild);
    };
    const pauseBtn = h('button', { class: 'btn sm', 'aria-pressed': state.paused, onclick: () => { state.paused = !state.paused; pauseBtn.replaceChildren(icon(state.paused ? 'play' : 'pause'), state.paused ? 'Resume' : 'Pause'); if (!state.paused) draw(); } }, icon(state.paused ? 'play' : 'pause'), state.paused ? 'Resume' : 'Pause');
    const types = [...new Set(events.map(e => (e.type || '').split('.')[0]))].sort();
    return h('div', { class: 'stack' },
      h('div', { class: 'toolbar' },
        h('div', { class: 'search' }, icon('search'), h('input', { type: 'search', placeholder: 'type prefix or T-42', value: state.q, oninput: ev => { state.q = ev.target.value; draw(); } })),
        h('div', { class: 'chips' }, types.map(t => h('button', { class: 'chip sans' + (state.q === t ? ' accent' : ''), type: 'button', onclick: () => { state.q = state.q === t ? '' : t; draw(); } }, t))),
        h('div', { class: 'spacer' }), pauseBtn),
      card({ title: 'Timeline', body: log, footer: 'Every payload passes an allowlist on the server; a new event type that carried an unexpected key would be narrowed, not leaked. Click a line to expand it.' }));
  },
});
