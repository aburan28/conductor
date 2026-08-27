import { h } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty, segmented } from '../components/ui.js';
import { conflictCard } from '../components/conflict.js';

export default defineView({
  title: 'Conflicts',
  async load(ctx, state) {
    const open = state.includeResolved ? 'false' : 'true';
    const out = await ctx.api.get(ctx.api.project(ctx.project, `/conflicts?open=${open}`));
    return { conflicts: out.conflicts || [] };
  },
  draw({ conflicts }, ctx, { refresh, state }) {
    state.sev = state.sev || 'all';
    const list = conflicts.filter(c => state.sev === 'all' || (state.sev === 'high' ? ['high', 'critical'].includes(c.severity) : c.severity === state.sev));
    const byKind = {};
    for (const c of conflicts) byKind[c.kind] = (byKind[c.kind] || 0) + 1;
    return h('div', { class: 'stack' },
      h('div', { class: 'toolbar' },
        segmented([{ value: 'all', label: 'All' }, { value: 'high', label: 'High+' }, { value: 'medium', label: 'Medium' }, { value: 'low', label: 'Low' }], state.sev, v => { state.sev = v; refresh(); }),
        h('label', { class: 'field', style: { flexDirection: 'row', alignItems: 'center', gap: '6px' } },
          h('input', { type: 'checkbox', checked: !!state.includeResolved, onchange: ev => { state.includeResolved = ev.target.checked; refresh(); } }), 'include resolved'),
        h('div', { class: 'spacer' }),
        h('span', { class: 'muted' }, Object.entries(byKind).map(([k, n]) => `${n} ${k.replace(/_/g, ' ')}`).join(' · '))),
      card({ title: state.includeResolved ? 'All conflicts' : 'Open conflicts', body: list.length ? list.map(c => conflictCard(c, ctx, { onChange: refresh }))
        : empty('No conflicts. Overlapping scopes, duplicate intents, and merge risk from observed diffs all land here.', 'conductor conflicts'),
        footer: 'A conflict names tasks, owners, and resources — never why the other side wants the file. Similarity scores are withheld on purpose: repeated probing against a score would reconstruct private work.' }));
  },
});
