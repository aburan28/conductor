import { h } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, kpi, empty, meter } from '../components/ui.js';
import { pill, chips } from '../components/pill.js';
import { conflictCard } from '../components/conflict.js';
import { eventLine } from './events.js';
import { relTime, fmtTokens, fmtUSD, plural } from '../lib/format.js';

export default defineView({
  title: 'Overview',
  async load(ctx) {
    const p = ctx.project;
    const [status, caps, queue, budget, events] = await settle([
      ctx.api.get(ctx.api.project(p, '/status')),
      ctx.api.get(ctx.api.project(p, '/capabilities')),
      ctx.api.get(ctx.api.project(p, '/queue')),
      ctx.api.get(ctx.api.project(p, '/budget')),
      ctx.api.get(ctx.api.project(p, '/events?limit=40')),
    ]);
    if (!status) throw new Error('status endpoint unavailable');
    return { status, caps, queue, budget, events: (events && events.events) || [] };
  },
  draw({ status, caps, queue, budget, events }, ctx, { refresh }) {
    const counts = status.counts || {};
    const sum = keys => keys.reduce((n, k) => n + (counts[k] || 0), 0);
    const inFlight = sum(['claimed', 'running', 'verifying', 'review_required', 'merging']);
    const blocked = sum(['blocked_dependency', 'blocked_conflict', 'blocked_input']);
    const inv = caps && caps.inventory ? caps.inventory : {};
    const queued = queue && queue.tickets ? queue.tickets.filter(t => t.state === 'queued').length : 0;
    const spent = budget && budget.project ? Number(budget.project.spent_usd || 0) : 0;
    const monthly = budget && budget.project ? Number(budget.project.monthly_usd || 0) : 0;
    const conflicts = status.conflicts || [];
    const go = path => () => ctx.navigate(path);

    const kpis = h('div', { class: 'kpis' },
      kpi({ label: 'In flight', value: inFlight, kind: 'accent', sub: plural(counts.running || 0, 'attempt running'), onClick: go('/tasks') }),
      kpi({ label: 'Ready', value: counts.ready || 0, sub: (counts.proposed || 0) + ' proposed', onClick: go('/tasks') }),
      kpi({ label: 'Blocked', value: blocked, kind: blocked ? 'warn' : '', sub: blocked ? 'dependency, conflict, or input' : 'nothing waiting', onClick: go('/tasks') }),
      kpi({ label: 'Conflicts', value: conflicts.length, kind: conflicts.length ? 'danger' : '', sub: conflicts.length ? 'open, needs a decision' : 'radar is clear', onClick: go('/conflicts') }),
      kpi({ label: 'Live sessions', value: inv.available ?? (status.presence || []).length, unit: inv.sessions != null ? `of ${inv.sessions}` : '', sub: inv.max_tier ? `ceiling ${inv.max_tier} · ${inv.max_reasoning_effort || '—'}` : 'accepting work', onClick: go('/sessions') }),
      kpi({ label: 'Queue', value: queued, kind: queued ? 'warn' : '', sub: queue && queue.policy && queue.policy.max_active_sessions ? `cap ${queue.policy.max_active_sessions} sessions` : queue ? 'no cap configured' : 'not available', onClick: go('/queue') }),
      kpi({ label: 'Spend, 30d', value: monthly || spent ? fmtUSD(spent).replace('—', '$0') : '—', unit: monthly ? `of $${monthly}` : '', kind: monthly && spent / monthly >= 0.95 ? 'danger' : monthly && spent / monthly >= 0.75 ? 'warn' : '', sub: budget && budget.policy && budget.policy.member_tokens ? `${fmtTokens(budget.policy.member_tokens)} tokens per member` : 'set budget.project.monthly_usd', onClick: go('/usage') }));

    const presence = (status.presence || []);
    const presenceCard = card({
      title: 'Live presence — who is working on what',
      actions: h('a', { class: 'btn sm ghost', href: '/sessions', 'data-link': true }, 'All sessions'),
      body: presence.length ? presence.map(e => h('div', { class: 'row' },
        h('div', { class: 'who' }, e.principal, h('small', {}, e.harness + (e.sponsored_by ? ' · for ' + e.sponsored_by : ''))),
        h('div', { class: 'what' },
          h('div', {}, pill(e.state), ' ',
            e.task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(e.task_ref)}`, 'data-link': true }, e.task_ref) : h('span', { class: 'muted' }, 'no task'),
            e.task_title ? h('span', { class: 'muted' }, ' — ' + e.task_title) : (e.task_ref ? h('span', { class: 'private' }, ' — private work') : null)),
          e.branch || (e.scopes && e.scopes.length) ? chips([e.branch, ...(e.scopes || [])].filter(Boolean)) : null),
        h('div', { class: 'meta' }, relTime(e.last_heartbeat)))) : empty('No live sessions. Launch a tool through Conductor so teammates can see you are here.', 'conductor wrap claude'),
      footer: 'Sessions, tasks, and reserved scopes only. Prompts and model output have no field to travel in.',
    });

    const conflictsCard = card({
      title: 'Conflict radar',
      actions: h('a', { class: 'btn sm ghost', href: '/conflicts', 'data-link': true }, 'All conflicts'),
      body: conflicts.length ? conflicts.slice(0, 6).map(c => conflictCard(c, ctx, { onChange: refresh, compact: true })) : empty('No open conflicts. Two efforts about to collide will show up here first.', 'conductor check --summary "…" --scope path:…'),
    });

    const active = status.active || [];
    const activeCard = card({
      title: 'In flight',
      actions: h('a', { class: 'btn sm ghost', href: '/tasks', 'data-link': true }, 'Board'),
      body: active.length ? active.slice(0, 8).map(t => h('div', { class: 'row' },
        h('div', { class: 'who', style: { minWidth: '70px' } }, h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(t.ref)}`, 'data-link': true }, t.ref)),
        h('div', { class: 'what' }, h('div', {}, pill(t.status), ' ', t.title ? t.title : h('span', { class: 'private' }, 'private work'), t.owner ? h('span', { class: 'muted' }, ' · ' + t.owner) : null),
          t.scopes && t.scopes.length ? chips(t.scopes.slice(0, 5)) : null),
        h('div', { class: 'meta' }, relTime(t.updated_at)))) : empty('Nothing in flight. Claim the next ready task, or file one.', 'conductor task claim --next'),
    });

    const members = budget && budget.members ? budget.members : [];
    const budgetCard = card({
      title: 'Budget',
      actions: h('a', { class: 'btn sm ghost', href: '/swarm', 'data-link': true }, 'Swarm & sharing'),
      body: h('div', { class: 'stack' },
        monthly ? h('div', {}, h('div', { class: 'row', style: { padding: '0 0 6px', border: 0 } }, h('div', { class: 'what' }, `Project ${fmtUSD(spent).replace('—', '$0')} of $${monthly} this window`), h('div', { class: 'meta' }, Math.round(100 * spent / monthly) + '%')), meter(spent, monthly)) : h('div', { class: 'muted' }, 'No project dollar budget configured.'),
        members.length && budget.policy && budget.policy.member_tokens ? members.map(m => h('div', { class: 'row', style: { padding: '5px 0' } },
          h('div', { class: 'who', style: { minWidth: '110px' } }, m.handle),
          h('div', { class: 'what' }, meter(m.spent_tokens, m.allowance_tokens + m.shared_in_tokens - m.shared_out_tokens)),
          h('div', { class: 'meta' }, fmtTokens(m.remaining_tokens) + ' left'))) : h('div', { class: 'muted' }, 'Per-member token allowances are off (budget.member.monthly_tokens).')),
    });

    const eventsCard = card({
      title: 'Recent events',
      actions: h('a', { class: 'btn sm ghost', href: '/events', 'data-link': true }, 'Timeline'),
      body: h('div', { class: 'log', id: 'overview-log' }, events.length ? events.slice(0, 30).map(e => eventLine(e)) : empty('No events yet.')),
      flush: false,
    });

    return h('div', { class: 'stack', style: { gap: '20px' } },
      kpis,
      h('div', { class: 'grid-2' }, presenceCard, conflictsCard),
      h('div', { class: 'grid-2' }, activeCard, budgetCard),
      eventsCard);
  },
});
