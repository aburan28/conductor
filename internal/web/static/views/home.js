import { h, replace } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty, snippet } from '../components/ui.js';
import { chip, chips } from '../components/pill.js';
import { table } from '../components/table.js';
import { conflictCard } from '../components/conflict.js';
import { relTime, plural } from '../lib/format.js';
import { toastError } from '../components/toast.js';

// Home answers three questions on one screen: can I start this, who is on what, and what is
// waiting on me. Counts are kept to the one sentence at the top; everything below is a list
// of things a person can act on.

// statusWords says a task's state the way a person would.
export const STATUS_WORDS = {
  proposed: 'proposed', ready: 'ready to start', claimed: 'claimed', running: 'in progress',
  blocked_dependency: 'blocked by another task', blocked_conflict: 'blocked by a conflict', blocked_input: 'needs input',
  verifying: 'checking', review_required: 'waiting for review', merging: 'merging', done: 'done',
  failed: 'failed', cancelled: 'cancelled', superseded: 'superseded',
};

const statusKind = s => /blocked|failed/.test(s) ? 'danger' : /review|verifying|merging|proposed/.test(s) ? 'warn' : /running|claimed/.test(s) ? 'accent' : '';

export function statusChip(s) {
  return chip(STATUS_WORDS[s] || String(s || '').replace(/_/g, ' '), { mono: false, kind: statusKind(s) });
}

const REVIEWERS = ['reviewer', 'maintainer', 'project_admin', 'org_admin'];

export default defineView({
  title: 'Home',
  async load(ctx) {
    const p = ctx.project;
    const [status, open, conflicts] = await settle([
      ctx.api.get(ctx.api.project(p, '/status')),
      ctx.api.get(ctx.api.project(p, '/tasks?open=true')),
      ctx.api.get(ctx.api.project(p, '/conflicts')),
    ]);
    if (!status) throw new Error('This project\'s status could not be loaded.');
    return { status, tasks: (open && open.tasks) || [], conflicts: (conflicts && conflicts.conflicts) || [] };
  },
  draw({ status, tasks, conflicts }, ctx, { refresh, state }) {
    const me = ctx.handle;
    const active = status.active || [];
    const presence = status.presence || [];

    // What is waiting on this person, most urgent first.
    const waiting = [];
    for (const c of conflicts) {
      if (c.mine && c.mine.owner === me) waiting.push({ kind: 'danger', ref: c.mine.task_ref,
        text: [`Your ${c.mine.task_ref} may collide with `, c.other && c.other.owner ? c.other.owner + '\'s ' : '', (c.other && c.other.task_ref) || 'another task', '. Decide how to settle it.'],
        action: h('a', { class: 'btn sm', href: '/conflicts', 'data-link': true }, 'Settle') });
    }
    for (const t of tasks) {
      if (t.owner === me && /^blocked/.test(t.status)) waiting.push({ kind: 'warn', ref: t.ref, text: [`Your ${t.ref} is ${STATUS_WORDS[t.status]}.`] });
      else if (t.status === 'review_required' && t.owner !== me && REVIEWERS.includes(ctx.role)) waiting.push({ kind: 'info', ref: t.ref, text: [`${t.ref} by ${t.owner || 'someone'} is waiting for review.`] });
    }

    const summary = [
      active.length ? plural(active.length, 'task') + ' in flight' : 'Nothing in flight',
      conflicts.length ? plural(conflicts.length, 'conflict') : 'no conflicts',
      waiting.length ? plural(waiting.length, 'thing') + ' waiting on you' : 'nothing waiting on you',
    ].join(' · ');

    const checkCard = checkBeforeEdit(ctx, state);

    const waitingCard = card({
      title: 'Waiting on you',
      body: waiting.length ? h('ul', { class: 'items' }, waiting.slice(0, 8).map(w => h('li', { class: 'item ' + w.kind },
        h('span', { class: 'what' }, ...w.text), w.action || h('a', { class: 'btn sm', href: `/tasks/${encodeURIComponent(w.ref)}`, 'data-link': true }, 'Open'))))
        : empty('Nothing needs you right now.'),
    });

    const contestedCard = card({
      title: 'Contested',
      actions: conflicts.length ? h('a', { class: 'btn sm ghost', href: '/conflicts', 'data-link': true }, 'All conflicts') : null,
      body: conflicts.length ? conflicts.slice(0, 4).map(c => conflictCard(c, ctx, { onChange: refresh, compact: true }))
        : empty('No two pieces of work are about to collide.'),
    });

    const inFlight = card({
      title: 'Who is on what',
      flush: true,
      actions: h('a', { class: 'btn sm ghost', href: '/tasks', 'data-link': true }, 'All tasks'),
      body: active.length ? table({
        caption: 'Work in flight',
        columns: [
          { key: 'ref', label: 'Task', render: t => h('span', {}, h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(t.ref)}`, 'data-link': true }, t.ref), ' ',
            t.title ? t.title : h('span', { class: 'private' }, 'private work')) },
          { key: 'owner', label: 'Who', render: t => t.owner || h('span', { class: 'muted' }, '—') },
          { key: 'status', label: 'Status', render: t => statusChip(t.status) },
          { key: 'scopes', label: 'Territory', sortable: false, render: t => t.scopes && t.scopes.length ? chips(t.scopes.slice(0, 3)) : h('span', { class: 'muted' }, 'none reserved') },
          { key: 'updated_at', label: 'Updated', render: t => relTime(t.updated_at), sort: t => new Date(t.updated_at) },
        ],
        rows: active, initialSort: { key: 'updated_at', dir: 'desc' },
      }) : empty('Nothing in flight. Run a check before your next edit, so a teammate sees you coming.', 'conductor check --summary "…" --scope path:…'),
    });

    const here = presence.filter(e => e.state !== 'offline');
    const peopleCard = card({
      title: 'Here now',
      actions: h('a', { class: 'btn sm ghost', href: '/people', 'data-link': true }, 'People'),
      body: here.length ? h('ul', { class: 'items' }, here.slice(0, 8).map(e => h('li', { class: 'item' },
        h('span', { class: 'what' }, h('strong', {}, e.principal), h('span', { class: 'muted' }, ' in ' + e.harness), ' — ',
          e.task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(e.task_ref)}`, 'data-link': true }, e.task_ref) : 'not on a task'),
        h('span', { class: 'meta' }, relTime(e.last_heartbeat)))))
        : empty('Nobody has a session open. Start your tool through Conductor so teammates can see you.', 'conductor wrap claude'),
    });

    return h('div', { class: 'stack home', style: { gap: '20px' } },
      h('p', { class: 'lede' }, summary + '.'),
      checkCard,
      h('div', { class: 'grid-2' }, waitingCard, contestedCard),
      inFlight,
      peopleCard);
  },
});

// scopeOf turns what someone typed into a scope: "internal/billing/" is a directory,
// anything else a path, and an explicit "dir:" or "path:" is kept.
export function scopeOf(raw) {
  const s = raw.trim();
  if (!s) return null;
  if (/^[a-z]+:/.test(s)) return s;
  return s.endsWith('/') ? 'dir:' + s.replace(/\/+$/, '') : 'path:' + s;
}

// checkBeforeEdit is the product's one essential question, asked from the dashboard:
// is anyone already working on what I am about to change?
function checkBeforeEdit(ctx, state) {
  const summary = h('input', { type: 'text', id: 'check-summary', placeholder: 'Fix rounding in invoice totals', value: state.checkSummary || '', autocomplete: 'off' });
  const paths = h('input', { type: 'text', id: 'check-paths', placeholder: 'internal/billing/invoice.go, docs/', value: state.checkPaths || '', autocomplete: 'off', spellcheck: false });
  const result = h('div', { class: 'check-result', 'aria-live': 'polite' });
  if (state.checkResult) replace(result, state.checkResult);

  async function run(ev) {
    ev.preventDefault();
    state.checkSummary = summary.value;
    state.checkPaths = paths.value;
    const scopes = paths.value.split(/[,\n]/).map(scopeOf).filter(Boolean);
    if (!summary.value.trim() && !scopes.length) { summary.focus(); return; }
    replace(result, h('div', { class: 'muted' }, 'Checking…'));
    try {
      const d = await ctx.api.post(ctx.api.project(ctx.project, '/intents/check'), {
        summary: summary.value.trim(), scopes: scopes.map(resource => ({ resource, mode: 'write_exclusive' })) });
      state.checkResult = renderDecision(d, summary.value.trim(), scopes);
      replace(result, state.checkResult);
    } catch (err) {
      replace(result, '');
      toastError(err, 'Check failed');
    }
  }

  return card({
    title: 'Can I start?',
    body: h('form', { class: 'check-form', onsubmit: run },
      h('div', { class: 'field-row' },
        h('label', { class: 'field' }, 'What are you about to change?', summary),
        h('label', { class: 'field' }, 'Files or directories (optional)', paths)),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Check'),
        h('span', { class: 'hint' }, 'Nothing is claimed by checking. Agents run the same check before every edit.')),
      result),
  });
}

function renderDecision(d, summary, scopes) {
  const conflicts = d.conflicts || [];
  const dups = d.duplicates || [];
  const quoted = (summary || '…').replace(/"/g, '\\"');
  const scopeArgs = scopes.map(s => ' --scope ' + s).join('');
  if (!conflicts.length && !dups.length) {
    return h('div', { class: 'stack' },
      h('div', { class: 'notice ok', role: 'status' }, h('strong', {}, 'Clear. '), 'Nobody is working there. Claim the work when you start, so the next person to check sees you.'),
      snippet(`conductor task create --title "${quoted}"${scopeArgs}`));
  }
  return h('div', { class: 'stack' },
    h('div', { class: 'notice warn', role: 'status' }, h('strong', {}, conflicts.length ? 'Someone is already there. ' : 'This looks like work already under way. '), d.advice || ''),
    conflicts.length ? h('ul', { class: 'items' }, conflicts.map(c => h('li', { class: 'item ' + (c.outcome === 'block_conflict' ? 'danger' : 'warn') },
      h('span', { class: 'what' }, h('strong', {}, c.holder_owner || 'Someone'), ' holds ', h('code', {}, c.resource), ' for ',
        c.holder_task_ref ? h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(c.holder_task_ref)}`, 'data-link': true }, c.holder_task_ref) : 'a task',
        c.holder_task_title ? ' — ' + c.holder_task_title : '', c.held_since ? h('span', { class: 'muted' }, ' · since ' + relTime(c.held_since)) : null)))) : null,
    dups.length ? h('ul', { class: 'items' }, dups.map(x => h('li', { class: 'item info' },
      h('span', { class: 'what' }, 'Similar: ', h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(x.task_ref)}`, 'data-link': true }, x.task_ref),
        x.title ? ' — ' + x.title : '', x.owner ? h('span', { class: 'muted' }, ' · ' + x.owner) : null)))) : null);
}
