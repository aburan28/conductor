import { h } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty } from '../components/ui.js';
import { chip } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime } from '../lib/format.js';
import { sessionsBlock } from './sessions.js';

// People: everyone on the project, what each is doing right now, and the sessions they have
// open — presence and sessions on one page.

const live = s => !s.closed_at && !['closed', 'stale'].includes(s.state);

export default defineView({
  title: 'People',
  async load(ctx) {
    const p = ctx.project;
    const [members, sessions, caps, tasks, presence] = await settle([
      ctx.api.get(ctx.api.project(p, '/members')),
      ctx.api.get(ctx.api.project(p, '/sessions')),
      ctx.api.get(ctx.api.project(p, '/capabilities')),
      ctx.api.get(ctx.api.project(p, '/tasks?open=true')),
      ctx.api.get(ctx.api.project(p, '/presence')),
    ]);
    return {
      members: (members && members.members) || [], sessions: (sessions && sessions.sessions) || [],
      caps: (caps && caps.sessions) || [], tasks: (tasks && tasks.tasks) || [], presence: (presence && presence.presence) || [],
    };
  },
  draw(data, ctx, { refresh, state }) {
    const { members, sessions, tasks, presence } = data;
    const byHandle = new Map();
    for (const m of members) byHandle.set(m.handle, { ...m, sessions: [], presence: [], owns: [] });
    const person = handle => {
      if (!byHandle.has(handle)) byHandle.set(handle, { handle, role: '', kind: 'human', sessions: [], presence: [], owns: [] });
      return byHandle.get(handle);
    };
    for (const s of sessions) person(s.principal).sessions.push(s);
    for (const e of presence) person(e.principal).presence.push(e);
    for (const t of tasks) if (t.owner && ['claimed', 'running', 'verifying', 'review_required', 'merging'].includes(t.status)) person(t.owner).owns.push(t);
    const rows = [...byHandle.values()].map(p => {
      const open = p.sessions.filter(live);
      const lastSeen = [...p.sessions.map(s => s.last_heartbeat), ...p.presence.map(e => e.last_heartbeat)].filter(Boolean).sort().pop();
      const onTask = p.presence.find(e => e.task_ref) || null;
      return { ...p, open, lastSeen, onTask, here: open.length > 0 };
    });

    const doing = p => {
      const ref = p.onTask ? p.onTask.task_ref : p.owns.length ? p.owns[0].ref : '';
      if (ref) {
        const t = p.owns.find(x => x.ref === ref);
        return h('span', {}, h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(ref)}`, 'data-link': true }, ref),
          t && t.title ? ' ' + t.title : '', p.owns.length > 1 ? h('span', { class: 'muted' }, ` and ${p.owns.length - 1} more`) : null);
      }
      return h('span', { class: 'muted' }, p.here ? 'not on a task' : '—');
    };

    const roster = card({
      title: 'Team',
      flush: true,
      actions: ['project_admin', 'org_admin'].includes(ctx.role) ? h('a', { class: 'btn sm ghost', href: '/settings#members', 'data-link': true }, 'Manage members') : null,
      body: rows.length ? table({
        caption: 'Project members and what they are doing',
        columns: [
          { key: 'handle', label: 'Person', render: p => h('span', {}, h('strong', {}, p.handle), p.handle === ctx.handle ? h('span', { class: 'muted' }, ' (you)') : null,
            p.kind && p.kind !== 'human' ? h('span', { class: 'muted' }, ' · ' + p.kind.replace(/_/g, ' ')) : null) },
          { key: 'here', label: 'Status', render: p => chip(p.here ? 'online' : 'away', { mono: false, kind: p.here ? 'accent' : '' }), sort: p => (p.here ? 0 : 1) },
          { key: 'doing', label: 'Working on', sortable: false, render: doing },
          { key: 'tools', label: 'Open in', sortable: false, render: p => p.open.length ? h('div', { class: 'chips' }, [...new Set(p.open.map(s => s.harness))].map(x => chip(x, { mono: false }))) : h('span', { class: 'muted' }, '—') },
          { key: 'role', label: 'Role', render: p => p.role ? p.role.replace(/_/g, ' ') : h('span', { class: 'muted' }, '—') },
          { key: 'lastSeen', label: 'Last seen', render: p => p.lastSeen ? relTime(p.lastSeen) : h('span', { class: 'muted' }, 'never'), sort: p => new Date(p.lastSeen || 0) },
        ],
        rows, initialSort: { key: 'lastSeen', dir: 'desc' },
      }) : empty('Nobody is on this project yet. Add a teammate in Settings.', 'conductor member add rachel --role contributor'),
    });
    return h('div', { class: 'stack', style: { gap: '20px' } }, roster, sessionsBlock(data, ctx, refresh, state));
  },
});
