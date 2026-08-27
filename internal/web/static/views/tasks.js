import { h, icon, replace } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty, segmented } from '../components/ui.js';
import { pill, chip, riskChip } from '../components/pill.js';
import { table } from '../components/table.js';
import { openTaskForm } from '../components/task-form.js';
import { relTime } from '../lib/format.js';
import { prefs } from '../lib/store.js';

export const COLUMNS = [
  ['Queued', ['proposed', 'ready']],
  ['In flight', ['claimed', 'running']],
  ['Blocked', ['blocked_dependency', 'blocked_conflict', 'blocked_input']],
  ['Verifying', ['verifying', 'review_required', 'merging']],
  ['Done', ['done']],
  ['Failed', ['failed', 'cancelled', 'superseded']],
];

export function taskCard(t) {
  return h('a', { class: 'tcard', href: `/tasks/${encodeURIComponent(t.ref)}`, 'data-link': true },
    h('div', { class: 'head' }, h('span', { class: 'ref' }, t.ref), pill(t.status), t.owner ? h('span', { class: 'owner' }, t.owner) : null),
    h('div', { class: 'title' }, t.title ? t.title : h('span', { class: 'private' }, 'private work')),
    t.labels && t.labels.length ? h('div', { class: 'chips' }, t.labels.map(l => chip(l, { mono: false, kind: 'accent' }))) : null,
    t.scopes && t.scopes.length ? h('div', { class: 'chips' }, t.scopes.slice(0, 4).map(s => chip(s)), t.scopes.length > 4 ? chip('+' + (t.scopes.length - 4)) : null) : null,
    h('div', { class: 'foot' }, t.priority ? h('span', { class: 'prio' }, 'p' + t.priority) : null, riskChip(t.risk_level), t.attempts_count ? h('span', {}, t.attempts_count + ' attempt' + (t.attempts_count === 1 ? '' : 's')) : null,
      h('span', { style: { marginLeft: 'auto' } }, relTime(t.updated_at))));
}

export default defineView({
  title: 'Tasks',
  async load(ctx, state) {
    // Open-only by default; "include finished" drops the filter so Done/Failed fill in.
    const query = state.showAll ? '?limit=400' : '?open=true';
    const out = await ctx.api.get(ctx.api.project(ctx.project, '/tasks' + query));
    return { tasks: out.tasks || [] };
  },
  draw({ tasks }, ctx, { refresh, state }) {
    state.mode = state.mode || prefs.get('tasks_mode', 'board');
    state.q = state.q || '';
    state.owner = state.owner || '';
    state.label = state.label || '';
    const owners = [...new Set(tasks.map(t => t.owner).filter(Boolean))].sort();
    const labels = [...new Set(tasks.flatMap(t => t.labels || []))].sort();

    const search = h('input', { type: 'search', placeholder: 'Filter by ref, title, scope…', value: state.q, oninput: ev => { state.q = ev.target.value; drawBody(); } });
    const ownerSel = h('select', { onchange: ev => { state.owner = ev.target.value; drawBody(); } }, h('option', { value: '' }, 'any owner'), owners.map(o => h('option', { value: o, selected: o === state.owner }, o)));
    const labelSel = h('select', { onchange: ev => { state.label = ev.target.value; drawBody(); } }, h('option', { value: '' }, 'any label'), labels.map(o => h('option', { value: o, selected: o === state.label }, o)));
    const showAll = h('label', { class: 'field', style: { flexDirection: 'row', alignItems: 'center', gap: '6px' } },
      h('input', { type: 'checkbox', checked: !!state.showAll, onchange: ev => { state.showAll = ev.target.checked; refresh(); } }), 'include finished');

    const bodyEl = h('div');
    const filtered = () => tasks.filter(t => {
      if (state.owner && t.owner !== state.owner) return false;
      if (state.label && !(t.labels || []).includes(state.label)) return false;
      if (!state.q) return true;
      const q = state.q.toLowerCase();
      return [t.ref, t.title, t.owner, ...(t.scopes || []), ...(t.labels || []), t.external_ref].filter(Boolean).some(s => String(s).toLowerCase().includes(q));
    });

    function drawBody() {
      const list = filtered();
      if (!tasks.length) return replace(bodyEl, empty('No tasks yet. File one here, or from the CLI.', 'conductor task create --title "…" --scope path:…'));
      if (state.mode === 'list') return replace(bodyEl, card({ flush: true, body: table({
        columns: [
          { key: 'ref', label: 'Ref', mono: true, render: t => h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(t.ref)}`, 'data-link': true }, t.ref) },
          { key: 'status', label: 'Status', render: t => pill(t.status) },
          { key: 'title', label: 'Title', render: t => t.title || h('span', { class: 'private' }, 'private work') },
          { key: 'owner', label: 'Owner' },
          { key: 'priority', label: 'Prio', num: true },
          { key: 'risk_level', label: 'Risk', render: t => riskChip(t.risk_level) || '' },
          { key: 'labels', label: 'Labels', render: t => h('div', { class: 'chips' }, (t.labels || []).map(l => chip(l, { mono: false, kind: 'accent' }))), sortable: false },
          { key: 'scopes', label: 'Scopes', render: t => h('div', { class: 'chips' }, (t.scopes || []).slice(0, 3).map(s => chip(s))), sortable: false },
          { key: 'attempts_count', label: 'Attempts', num: true },
          { key: 'updated_at', label: 'Updated', render: t => relTime(t.updated_at), sort: t => new Date(t.updated_at) },
        ],
        rows: list, initialSort: { key: 'updated_at', dir: 'desc' },
        empty: empty('Nothing matches the filter.'),
      }) }));
      replace(bodyEl, h('div', { class: 'board' }, COLUMNS.map(([label, statuses]) => {
        const items = list.filter(t => statuses.includes(t.status)).sort((a, b) => (b.priority || 0) - (a.priority || 0) || new Date(b.updated_at) - new Date(a.updated_at));
        return h('div', { class: 'col' }, h('h3', {}, label, h('span', { class: 'badge' }, items.length)),
          items.length ? items.map(taskCard) : h('div', { class: 'empty', style: { padding: '10px' } }, '—'));
      })));
    }
    drawBody();

    return h('div', { class: 'stack' },
      h('div', { class: 'toolbar' },
        h('div', { class: 'search' }, icon('search'), search), ownerSel, labels.length ? labelSel : null, showAll,
        h('div', { class: 'spacer' }),
        segmented([{ value: 'board', label: 'Board' }, { value: 'list', label: 'List' }], state.mode, v => { state.mode = v; prefs.set('tasks_mode', v); drawBody(); }),
        h('button', { class: 'btn primary', onclick: () => openTaskForm(ctx, { onCreated: v => { refresh(); ctx.navigate('/tasks/' + encodeURIComponent(v.ref)); } }) }, icon('plus'), 'New task')),
      bodyEl);
  },
});
