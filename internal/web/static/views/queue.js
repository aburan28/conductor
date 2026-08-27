import { h } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty, kpi, segmented } from '../components/ui.js';
import { pill, chip } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime, durationBetween } from '../lib/format.js';
import { cancelTicket } from '../lib/actions.js';

export default defineView({
  title: 'Queue',
  async load(ctx) {
    try {
      return { queue: await ctx.api.get(ctx.api.project(ctx.project, '/queue')) };
    } catch (err) {
      if (err.notFound) return { queue: null };
      throw err;
    }
  },
  draw({ queue }, ctx, { refresh, state }) {
    if (!queue) return card({ title: 'Admission queue', body: empty('The admission queue is not available on this server yet. Sessions launch immediately and runners poll for ready tasks.') });
    state.filter = state.filter || 'waiting';
    const policy = queue.policy || {};
    const active = queue.active || {};
    const tickets = (queue.tickets || []);
    const waiting = tickets.filter(t => t.state === 'queued').sort((a, b) => (a.position || 0) - (b.position || 0));
    const list = state.filter === 'waiting' ? waiting : state.filter === 'granted' ? tickets.filter(t => t.state === 'granted') : tickets;
    const canCancel = t => t.principal === ctx.handle || ['maintainer', 'project_admin', 'org_admin'].includes(ctx.role);

    const kpis = h('div', { class: 'kpis' },
      kpi({ label: 'Waiting', value: waiting.length, kind: waiting.length ? 'warn' : '', sub: waiting.length ? 'oldest first' : 'nothing queued' }),
      kpi({ label: 'Active sessions', value: active.sessions ?? 0, unit: policy.max_active_sessions ? `of ${policy.max_active_sessions}` : '', sub: policy.max_active_sessions ? 'project cap' : 'no session cap' }),
      kpi({ label: 'Active attempts', value: active.attempts ?? 0, unit: policy.max_concurrent_attempts ? `of ${policy.max_concurrent_attempts}` : '', sub: 'runner-executed work' }),
      kpi({ label: 'Per person', value: policy.max_sessions_per_principal || '∞', sub: 'sessions each member may hold' }));

    const tbl = table({
      columns: [
        { key: 'position', label: '#', num: true, render: t => t.state === 'queued' ? String(t.position ?? '') : '' },
        { key: 'state', label: 'State', render: t => pill(t.state) },
        { key: 'principal', label: 'Who' },
        { key: 'kind', label: 'Kind', render: t => chip(t.kind, { mono: false, kind: t.kind === 'attempt' ? 'info' : 'accent' }) },
        { key: 'task_ref', label: 'Task', mono: true, render: t => t.task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(t.task_ref)}`, 'data-link': true }, t.task_ref) : '—' },
        { key: 'harness', label: 'Harness · model', render: t => h('div', { class: 'chips' }, t.harness ? chip(t.harness, { mono: false }) : null, t.model ? chip(t.model) : null) },
        { key: 'requested_at', label: 'Waiting', render: t => t.state === 'queued' ? durationBetween(t.requested_at) : relTime(t.requested_at), sort: t => new Date(t.requested_at) },
        { key: 'granted_at', label: 'Granted', render: t => t.granted_at ? relTime(t.granted_at) : '—', sort: t => new Date(t.granted_at || 0) },
        { key: 'expires_at', label: 'Expires', render: t => t.expires_at && ['queued', 'granted'].includes(t.state) ? relTime(t.expires_at) : '—', sort: t => new Date(t.expires_at || 0) },
        { key: 'act', label: '', sortable: false, render: t => ['queued', 'granted'].includes(t.state) && canCancel(t) ? h('button', { class: 'btn sm danger', onclick: async ev => { ev.stopPropagation(); if (await cancelTicket(ctx, t)) refresh(); } }, t.state === 'queued' ? 'Cancel' : 'Release') : '' },
      ],
      rows: list, initialSort: state.filter === 'waiting' ? { key: 'position', dir: 'asc' } : { key: 'requested_at', dir: 'desc' },
      empty: empty(state.filter === 'waiting' ? 'Nobody is waiting. Sessions and attempts are admitted immediately while there is room.' : 'No tickets.', 'conductor queue'),
    });

    return h('div', { class: 'stack', style: { gap: '20px' } },
      kpis,
      card({ title: 'Tickets', flush: true,
        actions: segmented([{ value: 'waiting', label: 'Waiting' }, { value: 'granted', label: 'Granted' }, { value: 'all', label: 'All' }], state.filter, v => { state.filter = v; refresh(); }),
        body: tbl,
        footer: 'When the project is at its cap, `conductor wrap` and `conductor dispatch` wait here in order instead of failing. A granted ticket is released when its session closes or its attempt ends; one that stops heartbeating expires.' }));
  },
});
