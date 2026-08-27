import { h } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty, kpi } from '../components/ui.js';
import { pill, chip, chips, tierChip, effortChip } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime } from '../lib/format.js';

export default defineView({
  title: 'Fleet',
  async load(ctx) {
    const [caps, runners, models] = await settle([
      ctx.api.get(ctx.api.project(ctx.project, '/capabilities')),
      ctx.api.get(ctx.api.project(ctx.project, '/runners')),
      ctx.api.get(ctx.api.project(ctx.project, '/models')),
    ]);
    return { inv: (caps && caps.inventory) || {}, sessions: (caps && caps.sessions) || [], runners: (runners && runners.runners) || [], profiles: (models && models.profiles) || [] };
  },
  draw({ inv, sessions, runners, profiles }, ctx) {
    const kpis = h('div', { class: 'kpis' },
      kpi({ label: 'Sessions accepting', value: inv.available || 0, unit: `of ${inv.sessions || 0}` }),
      kpi({ label: 'Ceiling tier', value: inv.max_tier || '—', kind: 'accent', sub: 'highest tier accepting work' }),
      kpi({ label: 'Ceiling effort', value: inv.max_reasoning_effort || '—', sub: 'highest reasoning effort available' }),
      kpi({ label: 'Runners online', value: runners.filter(r => r.state === 'online').length, unit: `of ${runners.length}`, sub: runners.reduce((n, r) => n + (r.max_concurrency || 0) - (r.in_flight || 0), 0) + ' free slots' }),
      kpi({ label: 'Harnesses', value: (inv.harnesses || []).length, sub: (inv.harnesses || []).join(', ') || 'none live' }));

    const modelsLive = card({ title: 'Models live right now', flush: true, body: inv.models && inv.models.length ? table({
      columns: [
        { key: 'model', label: 'Model', mono: true },
        { key: 'harness', label: 'Harness' },
        { key: 'tier', label: 'Tier', render: m => tierChip(m.tier) || h('span', { class: 'muted' }, 'not in catalog') },
        { key: 'max_reasoning_effort', label: 'Effort ceiling' },
        { key: 'sessions', label: 'Sessions', num: true },
        { key: 'available', label: 'Accepting', num: true },
      ], rows: inv.models }) : empty('No models are live. Wrapped sessions advertise what they run.', 'conductor wrap claude --model claude-opus-5 --max-effort xhigh'),
      footer: inv.gaps && inv.gaps.length ? 'Gaps: ' + inv.gaps.join('; ') : 'Tier and effort are always shown; the model name follows publishModelIdentity.' });

    const sessionCard = card({ title: 'Session capabilities', flush: true, body: sessions.length ? table({
      columns: [
        { key: 'principal', label: 'Who' },
        { key: 'harness', label: 'Harness' },
        { key: 'state', label: 'State', render: s => pill(s.state) },
        { key: 'model', label: 'Model', mono: true, render: s => s.model || h('span', { class: 'muted' }, 'undisclosed') },
        { key: 'tier', label: 'Tier / effort', render: s => h('div', { class: 'chips' }, tierChip(s.tier), effortChip(s.reasoning_effort, s.max_reasoning_effort), !s.resolved && s.model ? chip('not in catalog', { kind: 'warn', mono: false }) : null) },
        { key: 'capabilities', label: 'Capabilities', sortable: false, render: s => chips(s.capabilities || [], { mono: false }) },
        { key: 'active_task_ref', label: 'On', mono: true, render: s => s.active_task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(s.active_task_ref)}`, 'data-link': true }, s.active_task_ref) : '—' },
        { key: 'last_heartbeat', label: 'Heartbeat', render: s => relTime(s.last_heartbeat), sort: s => new Date(s.last_heartbeat) },
      ], rows: sessions }) : empty('No live sessions.') });

    const runnersCard = card({ title: 'Runners — machines that execute attempts', flush: true, body: runners.length ? table({
      columns: [
        { key: 'name', label: 'Name' },
        { key: 'state', label: 'State', render: r => pill(r.state) },
        { key: 'in_flight', label: 'Load', render: r => `${r.in_flight || 0} / ${r.max_concurrency || 1}` },
        { key: 'harnesses', label: 'Harnesses', sortable: false, render: r => chips((r.capabilities && r.capabilities.harnesses) || []) },
        { key: 'models', label: 'Models', sortable: false, render: r => chips((r.capabilities && r.capabilities.models) || []) },
        { key: 'platform', label: 'Platform', render: r => (r.capabilities && r.capabilities.platform) || '—' },
        { key: 'heartbeat_at', label: 'Heartbeat', render: r => relTime(r.heartbeat_at), sort: r => new Date(r.heartbeat_at) },
      ], rows: runners }) : empty('No runners registered. Start one on any machine with a harness installed.', 'conductor worker --concurrency 2') });

    const byAlias = new Map();
    for (const p of profiles) { if (!byAlias.has(p.alias)) byAlias.set(p.alias, []); byAlias.get(p.alias).push(p); }
    const catalog = card({ title: 'Model catalog — what the policy can resolve to', body: profiles.length ? h('div', { class: 'stack' }, [...byAlias.entries()].sort().map(([alias, list]) =>
      h('div', { class: 'row' },
        h('div', { class: 'who', style: { minWidth: '170px' } }, h('span', { class: 'mono' }, alias), h('small', {}, `${list.length} profile${list.length === 1 ? '' : 's'}`)),
        h('div', { class: 'what' }, list.map(p => h('div', { class: 'chips', style: { marginBottom: '4px' } },
          chip(p.model || '(no model id)', { kind: p.enabled ? '' : 'warn' }), chip(p.harness, { mono: false }), p.provider ? chip(p.provider, { mono: false }) : null, tierChip(p.tier),
          p.reasoning_effort ? chip('effort ' + p.reasoning_effort, { mono: false }) : null,
          p.input_cost_per_mtok != null ? chip(`$${p.input_cost_per_mtok}/${p.output_cost_per_mtok ?? '?'} per Mtok`, { mono: false }) : chip(p.billing || 'tokens', { mono: false }),
          ...(p.capabilities || []).map(c => chip(c, { mono: false, kind: 'info' })),
          !p.enabled ? chip('disabled', { kind: 'danger', mono: false }) : null))))))
      : empty('The catalog is empty. Declare profiles in .conductor/models.yaml and re-run bootstrap.', 'conductor models'),
      footer: 'Aliases are roles (worker.fast, planner.frontier); profiles bind them to a concrete model on a harness. Policies name aliases, never models.' });

    return h('div', { class: 'stack', style: { gap: '20px' } }, kpis, modelsLive, sessionCard, runnersCard, catalog);
  },
});
