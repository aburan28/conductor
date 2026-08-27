import { h, icon } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty, segmented } from '../components/ui.js';
import { pill, chip, tierChip, effortChip } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime, fmtDate } from '../lib/format.js';
import { assignTask, respondToOffer } from '../lib/actions.js';
import { openModal } from '../components/modal.js';

export default defineView({
  title: 'Sessions',
  async load(ctx) {
    const [sessions, caps, tasks] = await settle([
      ctx.api.get(ctx.api.project(ctx.project, '/sessions')),
      ctx.api.get(ctx.api.project(ctx.project, '/capabilities')),
      ctx.api.get(ctx.api.project(ctx.project, '/tasks?open=true')),
    ]);
    return { sessions: (sessions && sessions.sessions) || [], caps: (caps && caps.sessions) || [], tasks: (tasks && tasks.tasks) || [] };
  },
  draw({ sessions, caps, tasks }, ctx, { refresh, state }) {
    state.filter = state.filter || 'live';
    const live = s => !s.closed_at && !['closed', 'stale'].includes(s.state);
    const list = sessions.filter(s => state.filter === 'all' ? true : state.filter === 'live' ? live(s) : !live(s));
    const capOf = id => caps.find(c => c.session_id === id) || {};

    const offer = s => {
      const ready = tasks.filter(t => ['ready', 'proposed'].includes(t.status));
      const modal = openModal({
        title: `Offer a task to ${s.principal}`,
        body: h('div', { class: 'form' }, h('p', { class: 'muted' }, 'Pick a ready task; the session sees it in its inbox and in coord_get_work.'),
          ready.length ? h('div', { class: 'stack' }, ready.map(t =>
            h('button', { class: 'btn', style: { justifyContent: 'flex-start' }, onclick: async () => {
              modal.close();
              const out = await assignTaskDirect(ctx, t.ref, s.id);
              if (out) refresh();
            } }, h('span', { class: 'ref' }, t.ref), ' ', t.title || h('span', { class: 'private' }, 'private work')))) : empty('No ready tasks to offer.', 'conductor task create --title "…"')),
        actions: [{ label: 'Close' }],
      });
    };

    const inbox = async s => {
      let out;
      try { out = await ctx.api.get(`/v1/sessions/${encodeURIComponent(s.id)}/assignments`); } catch (err) { out = { assignments: [] }; }
      const items = out.assignments || [];
      openModal({
        title: `Inbox — ${s.principal} on ${s.harness}`,
        body: items.length ? h('div', { class: 'stack' }, items.map(a => h('div', { class: 'row' },
          h('div', { class: 'what' }, h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(a.task_ref || a.task_id)}`, 'data-link': true }, a.task_ref || a.task_id), ' ', pill(a.state),
            h('div', { class: 'muted' }, describeRequirement(a.requirement), a.rationale ? ' · ' + a.rationale : '')),
          a.state === 'offered' ? h('div', { class: 'btn-row' },
            h('button', { class: 'btn sm primary', onclick: async () => { if (await respondToOffer(ctx, a, true)) refresh(); } }, 'Accept'),
            h('button', { class: 'btn sm', onclick: async () => { if (await respondToOffer(ctx, a, false)) refresh(); } }, 'Decline')) : null))) : empty('Nothing offered to this session.', `conductor task assign T-42 --require-tier ${capOf(s.id).tier || 'T3'}`),
        actions: [{ label: 'Close' }],
      });
    };

    const tbl = table({
      columns: [
        { key: 'principal', label: 'Who', render: s => h('div', {}, h('strong', {}, s.principal), h('div', { class: 'muted', style: { fontSize: '12px' } }, s.kind === 'human' ? '' : s.kind)) },
        { key: 'harness', label: 'Harness', render: s => h('span', {}, s.harness, s.harness_version ? h('span', { class: 'muted' }, ' ' + s.harness_version) : null) },
        { key: 'state', label: 'State', render: s => pill(s.state) },
        { key: 'model', label: 'Capability', sortable: false, render: s => { const c = s.capabilities || {}; return h('div', { class: 'chips' }, c.model ? chip(c.model) : chip('model undisclosed', { mono: false }), tierChip(c.tier), effortChip(c.reasoning_effort, c.max_reasoning_effort), c.roles && c.roles.length ? chip(c.roles.join('/'), { mono: false }) : null, c.model && !c.resolved ? chip('not in catalog', { kind: 'warn', mono: false }) : null); } },
        { key: 'active_task_ref', label: 'Task', mono: true, render: s => s.active_task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(s.active_task_ref)}`, 'data-link': true }, s.active_task_ref) : h('span', { class: 'muted' }, '—') },
        { key: 'machine_id', label: 'Machine · branch', render: s => h('div', { class: 'chips' }, s.machine_id ? chip(s.machine_id) : null, s.branch ? chip(s.branch) : null) },
        { key: 'last_heartbeat', label: 'Heartbeat', render: s => relTime(s.last_heartbeat), sort: s => new Date(s.last_heartbeat) },
        { key: 'started_at', label: 'Started', render: s => fmtDate(s.started_at), sort: s => new Date(s.started_at) },
        { key: 'actions', label: '', sortable: false, render: s => live(s) ? h('div', { class: 'btn-row' },
          h('button', { class: 'btn sm', onclick: ev => { ev.stopPropagation(); inbox(s); } }, 'Inbox', capOf(s.id).queued_offers ? h('span', { class: 'badge warn' }, capOf(s.id).queued_offers) : null),
          h('button', { class: 'btn sm', onclick: ev => { ev.stopPropagation(); offer(s); } }, 'Offer task')) : '' },
      ],
      rows: list, initialSort: { key: 'last_heartbeat', dir: 'desc' },
      empty: empty(state.filter === 'live' ? 'No live sessions.' : 'No sessions match.', 'conductor wrap claude --model claude-opus-5 --effort high'),
    });

    return h('div', { class: 'stack' },
      h('div', { class: 'toolbar' },
        segmented([{ value: 'live', label: 'Live' }, { value: 'closed', label: 'Closed' }, { value: 'all', label: 'All' }], state.filter, v => { state.filter = v; refresh(); }),
        h('div', { class: 'spacer' }), h('span', { class: 'muted' }, `${sessions.filter(live).length} live · ${sessions.length} total`)),
      card({ flush: true, body: tbl, footer: 'A session advertises what it runs; the catalog decides what that is worth. A model the catalog does not know is still usable, just never offered tier-gated work.' }));
  },
});

async function assignTaskDirect(ctx, ref, sessionID) {
  try {
    const out = await ctx.api.post(ctx.api.task(ctx.project, ref, '/assign'), { session_id: sessionID, require: {} });
    const { toast } = await import('../components/toast.js');
    toast(`${ref} offered`);
    return out;
  } catch (err) {
    const { toastError } = await import('../components/toast.js');
    toastError(err, 'Offer failed');
    return null;
  }
}

export function describeRequirement(r) {
  if (!r) return 'no requirement';
  const parts = [];
  if (r.tier) parts.push('tier ≥ ' + r.tier);
  if (r.reasoning_effort) parts.push('effort ≥ ' + r.reasoning_effort);
  if (r.model) parts.push('model ' + r.model);
  if (r.harness) parts.push('harness ' + r.harness);
  if (r.role) parts.push('role ' + r.role);
  for (const c of r.capabilities || []) parts.push('can ' + c);
  return parts.length ? parts.join(', ') : 'no requirement';
}

export { assignTask };
