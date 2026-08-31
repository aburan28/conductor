import { h, icon } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty, kpi, meter, snippet } from '../components/ui.js';
import { pill, chip, chips } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime, fmtTokens, fmtDate } from '../lib/format.js';
import { shareBudget } from '../lib/actions.js';

export default defineView({
  title: 'Swarm',
  async load(ctx) {
    const [swarm, budget, grants, members] = await settle([
      ctx.api.get(ctx.api.project(ctx.project, '/swarm')),
      ctx.api.get(ctx.api.project(ctx.project, '/budget')),
      ctx.api.get(ctx.api.project(ctx.project, '/budget/grants')),
      ctx.api.get(ctx.api.project(ctx.project, '/members')),
    ]);
    return { swarm, budget, grants: (grants && grants.grants) || [], members: (members && members.members) || [] };
  },
  draw({ swarm, budget, grants, members }, ctx, { refresh }) {
    const contributors = (swarm && swarm.contributors) || [];
    const capacity = (swarm && swarm.capacity) || {};
    const policy = (budget && budget.policy) || {};
    const memberBudgets = (budget && budget.members) || [];
    const origin = ctx.origin;

    const kpis = h('div', { class: 'kpis' },
      kpi({ label: 'Contributors', value: contributors.length, sub: `${capacity.runners || 0} runners · ${capacity.sessions_accepting || 0} sessions accepting` }),
      kpi({ label: 'Free slots', value: capacity.slots_free ?? '—', kind: 'accent', sub: 'attempts that can start now' }),
      kpi({ label: 'Queue depth', value: swarm ? (swarm.queue_depth || 0) : '—', kind: swarm && swarm.queue_depth ? 'warn' : '', onClick: () => ctx.navigate('/queue') }),
      kpi({ label: 'Member allowance', value: policy.member_tokens ? fmtTokens(policy.member_tokens) : 'off', sub: policy.member_tokens ? 'tokens per member, 30-day window' : 'set budget.member.monthly_tokens' }));

    const budgetOf = c => c.budget || memberBudgets.find(m => m.principal_id === c.principal_id || m.handle === c.principal) || null;
    const contributorsCard = card({ title: 'Who is contributing capacity', flush: true, body: !swarm
      ? h('div', { class: 'empty' }, 'The swarm endpoint is not available on this server yet. Runners and sessions still show under Fleet.')
      : contributors.length ? table({
        columns: [
          { key: 'principal', label: 'Who', render: c => h('div', {}, h('strong', {}, c.principal), h('div', { class: 'muted', style: { fontSize: '12px' } }, c.name || '')) },
          { key: 'kind', label: 'Kind', render: c => chip(c.kind, { mono: false, kind: c.kind === 'runner' ? 'info' : 'accent' }) },
          { key: 'state', label: 'State', render: c => pill(c.state) },
          { key: 'harness', label: 'Harnesses', sortable: false, render: c => chips(c.harnesses && c.harnesses.length ? c.harnesses : [c.harness].filter(Boolean)) },
          { key: 'models', label: 'Models', sortable: false, render: c => chips(c.models || []) },
          { key: 'in_flight', label: 'Load', render: c => `${c.in_flight || 0} / ${c.max_concurrency || 1}` },
          { key: 'budget', label: 'Budget position', sortable: false, render: c => { const b = budgetOf(c); if (!b || !policy.member_tokens) return h('span', { class: 'muted' }, '—');
            const cap = (b.allowance_tokens || 0) + (b.shared_in_tokens || 0) - (b.shared_out_tokens || 0);
            return h('div', { style: { minWidth: '140px' } }, meter(b.spent_tokens || 0, cap), h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '2px' } }, `${fmtTokens(b.remaining_tokens)} of ${fmtTokens(cap)} left`)); } },
          { key: 'last_heartbeat', label: 'Heartbeat', render: c => relTime(c.last_heartbeat), sort: c => new Date(c.last_heartbeat) },
        ], rows: contributors }) : empty('Nobody is contributing yet. Any teammate with a harness installed can join from their own machine.', `conductor swarm join --endpoint ${origin} --project ${ctx.project}`),
      footer: 'A contributor executes the team\'s tasks on their own machine, with their own account and budget. Spend lands on the person who ran it; the task\'s owner is charged nothing.' });

    const budgetCard = card({ title: 'Member budgets — this window', flush: true,
      actions: h('button', { class: 'btn sm primary', onclick: async () => { if (await shareBudget(ctx, members.length ? members : memberBudgets)) refresh(); } }, 'Share budget…'),
      body: memberBudgets.length ? table({
        columns: [
          { key: 'handle', label: 'Member', render: m => h('span', {}, h('strong', {}, m.handle), m.handle === ctx.handle ? h('span', { class: 'muted' }, ' (you)') : null) },
          { key: 'allowance_tokens', label: 'Allowance', num: true, render: m => fmtTokens(m.allowance_tokens) },
          { key: 'spent_tokens', label: 'Spent', num: true, render: m => fmtTokens(m.spent_tokens) },
          { key: 'shared_in_tokens', label: 'Received', num: true, render: m => fmtTokens(m.shared_in_tokens) },
          { key: 'shared_out_tokens', label: 'Given', num: true, render: m => fmtTokens(m.shared_out_tokens) },
          { key: 'remaining_tokens', label: 'Remaining', num: true, render: m => h('span', { class: m.remaining_tokens < 0 ? 'risk-high' : '' }, fmtTokens(m.remaining_tokens)) },
          { key: 'bar', label: '', sortable: false, render: m => { const cap = m.allowance_tokens + m.shared_in_tokens - m.shared_out_tokens; return h('div', { style: { minWidth: '120px' } }, meter(m.spent_tokens, cap)); } },
          { key: 'act', label: '', sortable: false, render: m => m.handle !== ctx.handle ? h('button', { class: 'btn sm', onclick: async ev => { ev.stopPropagation(); if (await shareBudget(ctx, members.length ? members : memberBudgets, m.handle)) refresh(); } }, 'Share with') : '' },
        ], rows: memberBudgets, initialSort: { key: 'remaining_tokens', dir: 'asc' } }) : empty('No member budgets: per-member allowances are disabled.', 'budget.member.monthly_tokens: 20000000  # in .conductor/policies.yaml'),
      footer: 'Balances are arithmetic over two ledgers — attempt spend and grants — so nothing can drift and nobody can mint tokens.' });

    const grantsCard = card({ title: 'Transfers', flush: true, body: grants.length ? table({
      columns: [
        { key: 'from_handle', label: 'From' }, { key: 'to_handle', label: 'To' },
        { key: 'tokens', label: 'Tokens', num: true, render: g => fmtTokens(g.tokens) },
        { key: 'note', label: 'Note', render: g => g.note || h('span', { class: 'muted' }, '—') },
        { key: 'created_at', label: 'When', render: g => fmtDate(g.created_at), sort: g => new Date(g.created_at) },
      ], rows: grants, initialSort: { key: 'created_at', dir: 'desc' } }) : empty('No transfers yet.', 'conductor budget share rachel 500k --note "finishing the router refactor"') });

    const join = card({ title: 'How to join the swarm', body: h('div', { class: 'stack' },
      h('p', {}, 'A teammate with headroom points their machine at this project. Their sessions and runner show up here, take ready tasks by capability, and spend their own budget doing it.'),
      snippet([
        `conductor login --endpoint ${origin} --token cdt_… --project ${ctx.project}`,
        `conductor swarm join --endpoint ${origin} --project ${ctx.project}`,
        `conductor worker --concurrency 2   # this machine takes ready tasks`,
        `# or keep an interactive session open to offers:`,
        `conductor wrap --model claude-opus-5 --max-effort xhigh claude`,
      ].join('\n'), { language: 'bash' }),
      h('div', { class: 'hint' }, 'Tokens are issued by a maintainer with `conductor member add <handle>` and shown once. The address above is the daemon\u2019s declared endpoint — set --public-url on conductord if it is wrong.')) });

    return h('div', { class: 'stack', style: { gap: '20px' } }, kpis, contributorsCard, h('div', { class: 'grid-2' }, budgetCard, join), grantsCard);
  },
});
