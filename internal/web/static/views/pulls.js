import { h, icon, replace } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty, kv, errorBox, skeleton } from '../components/ui.js';
import { pill, chip } from '../components/pill.js';
import { table } from '../components/table.js';
import { openModal, confirmModal } from '../components/modal.js';
import { toast, toastError } from '../components/toast.js';
import { relTime, fmtDate } from '../lib/format.js';

// Pull requests (docs/COLLABORATION.md §4.5): every pull request Conductor has seen on the
// project's repository, with its task, checks, and review. The server keeps titles, states, and
// counts only — never a body, a comment, or a log — so that is all there is to show. A pull
// request of someone's private task arrives redacted (number and state), and is drawn that way.

// A link to GitHub is clickable only when it is an https URL: it came from GitHub, but a
// javascript: URL must never become a link.
const safeLink = (url, text) => /^https:\/\//.test(url || '')
  ? h('a', { href: url, target: '_blank', rel: 'noopener' }, text)
  : h('span', { class: 'mono' }, text);

export const prName = p => `${p.repository}#${p.number}`;

export function stateOf(p) { return p.draft && p.state === 'open' ? 'draft' : p.state; }

export function checksPill(c) {
  if (!c) return h('span', { class: 'muted' }, 'none');
  if (c.failed > 0) return pill('danger', `${c.failed}/${c.total} failed`);
  if (c.pending > 0) return pill('warn', `${c.pending}/${c.total} pending`);
  return pill('ok', `${c.passed}/${c.total} passed`);
}

export function reviewPill(decision) {
  switch (decision) {
    case 'approved': return pill('ok', 'approved');
    case 'changes_requested': return pill('danger', 'changes requested');
    case 'review_required': return pill('warn', 'review required');
  }
  return h('span', { class: 'muted' }, '—');
}

const mergeablePill = m => !m ? null : pill({ clean: 'ok', dirty: 'danger', blocked: 'warn', behind: 'warn' }[m] || 'info', m);

function taskCell(p, ctx) {
  if (p.redacted) return h('span', { class: 'private' }, 'private task');
  if (!p.task_ref) return h('span', { class: 'muted' }, 'unlinked');
  return h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(p.task_ref)}`, 'data-link': true,
    onclick: ev => { ev.stopPropagation(); } }, p.task_ref);
}

const pullsPath = (ctx, query = {}) => ctx.api.project(ctx.project, '/pulls' + ctx.api.query(query));

// Link a pull request to a task by hand. A person's link outranks every automatic one.
function linkDialog(ctx, p, done) {
  const input = h('input', { type: 'text', placeholder: 'T-42', 'aria-label': 'Task', autofocus: true });
  openModal({
    title: `Link ${prName(p)} to a task`,
    body: h('div', { class: 'form' },
      h('label', { class: 'field' }, 'Task', input),
      h('div', { class: 'hint' }, 'A link made here outranks the branch, the Conductor-Task trailer and the reference in the description, and stays until you unlink it. When the pull request merges, the task is done.')),
    actions: [{ label: 'Cancel' }, { label: 'Link', kind: 'primary', onClick: async () => {
      const ref = input.value.trim();
      if (!ref) return false;
      try {
        await ctx.api.post(ctx.api.task(ctx.project, ref, '/pull-requests'), { pull_request: prName(p) });
        toast(`Linked ${prName(p)} to ${ref}`);
        done();
      } catch (err) { toastError(err, 'Link failed'); return false; }
    } }],
  });
  input.focus();
}

async function unlink(ctx, p, done) {
  const ok = await confirmModal({ title: `Unlink ${prName(p)} from ${p.task_ref}?`,
    message: 'Conductor stops treating this pull request as that task\'s work. It will not be linked again automatically.', confirmLabel: 'Unlink' });
  if (!ok) return;
  try {
    await ctx.api.del(ctx.api.task(ctx.project, p.task_ref, `/pull-requests/${p.repository}/${p.number}`));
    toast(`Unlinked ${prName(p)}`);
    done();
  } catch (err) { toastError(err, 'Unlink failed'); }
}

async function refreshFromGitHub(ctx, p, done) {
  try {
    await ctx.api.post(ctx.api.project(ctx.project, `/pulls/${p.repository}/${p.number}/refresh`), {});
    toast(`Refreshed ${prName(p)} from GitHub`);
    done();
  } catch (err) {
    if (err.status === 429) toast('Refreshed a moment ago', { kind: 'warn', detail: 'GitHub is read at most every few seconds per pull request.' });
    else if (err.status === 409) toast(err.message, { kind: 'warn' });
    else toastError(err, 'Refresh failed');
  }
}

// The drawer for one pull request. It reads the detail endpoint, which adds the task and its
// attempts as this viewer may see them.
export function openPullDrawer(p0, ctx, { onChange } = {}) {
  const body = h('div', { class: 'drawer-body' }, skeleton(8));
  const scrim = h('div', { class: 'drawer-scrim', onclick: () => close() });
  const drawer = h('div', { class: 'drawer', role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Pull request ' + prName(p0) },
    h('div', { class: 'drawer-bar' },
      h('button', { class: 'btn ghost icon', 'aria-label': 'Close pull request', onclick: () => close() }, icon('back')),
      h('span', { class: 'ref', style: { fontSize: '13px' } }, prName(p0)), h('div', { style: { flex: 1 } }),
      h('button', { class: 'btn ghost sm', onclick: () => refresh() }, icon('refresh'), 'Reload')),
    body);
  let alive = true;
  const onKey = ev => { if (ev.key === 'Escape' && !document.querySelector('.modal-scrim')) close(); };
  document.addEventListener('keydown', onKey);
  document.body.append(scrim, drawer);

  function close() {
    if (!alive) return;
    alive = false;
    scrim.remove(); drawer.remove();
    document.removeEventListener('keydown', onKey);
  }
  const changed = () => { refresh(); if (onChange) onChange(); };

  async function refresh() {
    try {
      const d = await ctx.api.get(ctx.api.project(ctx.project, `/pulls/${p0.repository}/${p0.number}`));
      if (alive) replace(body, render(d));
    } catch (err) {
      if (alive) replace(body, errorBox(err, refresh));
    }
  }

  function render({ pull: p, task, attempts }) {
    if (p.redacted) {
      return h('div', { class: 'stack', style: { gap: '16px' } },
        h('div', { class: 'detail-head' }, h('h2', {}, h('span', { class: 'private' }, 'pull request of a private task')), pill(p.state)),
        card({ title: 'Why there is little here', body: h('p', { class: 'muted' }, 'This pull request belongs to a private task. Its number and state are shown so nobody is surprised by a merge; everything else stays with the task\'s owner.') }),
        h('div', { class: 'btn-row' }, safeLink(p.url, 'Open on GitHub')));
    }
    const head = h('div', { class: 'detail-head' },
      h('h2', {}, p.title || h('span', { class: 'muted' }, 'title not read yet')), pill(stateOf(p)), p.from_fork ? chip('fork', { mono: false, kind: 'warn' }) : null);
    const actions = h('div', { class: 'btn-row' },
      p.state === 'open' ? h('button', { class: 'btn', onclick: () => refreshFromGitHub(ctx, p, changed) }, icon('refresh'), 'Refresh from GitHub') : null,
      p.task_ref ? h('button', { class: 'btn', onclick: () => unlink(ctx, p, changed) }, 'Unlink from ' + p.task_ref)
        : h('button', { class: 'btn primary', onclick: () => linkDialog(ctx, p, changed) }, 'Link to task…'),
      p.task_ref ? h('button', { class: 'btn', onclick: () => linkDialog(ctx, p, changed) }, 'Move to another task…') : null,
      h('a', { class: 'btn', href: /^https:\/\//.test(p.url) ? p.url : null, target: '_blank', rel: 'noopener' }, icon('external'), 'Open on GitHub'));
    const facts = card({ title: 'Facts', body: kv([
      ['Repository', h('span', { class: 'mono' }, p.repository)],
      ['Author', p.author ? h('span', {}, p.author, p.author_handle ? h('span', { class: 'muted' }, ` (${p.author_handle})`) : null) : null],
      ['Branch', p.head_ref ? h('span', { class: 'mono' }, `${p.head_ref} → ${p.base_ref || '?'}`) : null],
      ['Head', p.head_sha ? h('span', { class: 'mono' }, p.head_sha.slice(0, 12)) : null],
      ['Task', p.task_ref ? h('span', {}, taskCell(p, ctx), ' ', pill(p.task_status || 'ready'),
        p.link_source ? h('span', { class: 'muted' }, ` · linked by ${p.link_source.replace('_', ' ')}${p.primary ? ', primary' : ''}`) : null)
        : h('span', { class: 'muted' }, 'not linked to a task')],
      ['Checks', checksPill(p.checks)],
      ['Review', reviewPill(p.review_decision)],
      ['Unresolved threads', p.unresolved_threads ? String(p.unresolved_threads) : null],
      ['Mergeable', mergeablePill(p.mergeable)],
      ['Size', p.changed_files ? `+${p.additions} −${p.deletions} in ${p.changed_files} file${p.changed_files === 1 ? '' : 's'}` : null],
      ['Updated', p.updated_at ? relTime(p.updated_at) : null],
      ['First seen', fmtDate(p.created_at)],
    ]), footer: 'Conductor keeps this pull request\'s title, state, and counts. Its description, comments, and CI logs are never stored; an agent reads them on its own machine through gh when it needs them.' });
    const attemptsCard = task && attempts && attempts.length ? card({ title: `Attempts on ${task.ref}`, flush: true, body: table({
      columns: [
        { key: 'attempt_number', label: '#', num: true },
        { key: 'state', label: 'State', render: a => pill(a.state) },
        { key: 'harness', label: 'Harness' },
        { key: 'branch', label: 'Branch', mono: true },
      ], rows: attempts, initialSort: { key: 'attempt_number', dir: 'desc' } }) }) : null;
    return h('div', { class: 'stack', style: { gap: '16px' } }, head, actions, facts, attemptsCard);
  }

  refresh();
  return { close, refresh };
}

// The Pull requests card on a task: all of its pull requests, the primary first.
export function pullsCard(ctx, task, pulls, { onChange } = {}) {
  const open = p => openPullDrawer(p, ctx, { onChange });
  const linkBtn = h('button', { class: 'btn sm', onclick: () => linkByNumber(ctx, task, onChange) }, 'Link a pull request…');
  return card({ title: pulls.length > 1 ? `Pull requests (${pulls.length})` : 'Pull requests', actions: linkBtn, flush: pulls.length > 0,
    body: pulls.length ? table({
      columns: [
        // The first pull request linked is the task's primary: the one its card and the CLI name.
        { key: 'number', label: 'PR', num: true, render: p => h('span', {}, h('span', { class: 'mono' }, '#' + p.number), p.primary ? [' ', chip('primary', { mono: false, kind: 'accent' })] : null) },
        { key: 'state', label: 'State', render: p => pill(stateOf(p)) },
        { key: 'title', label: 'Title', render: p => p.redacted ? h('span', { class: 'private' }, 'private task') : (p.title || h('span', { class: 'muted' }, '—')) },
        { key: 'checks', label: 'Checks', sortable: false, render: p => p.redacted ? '' : checksPill(p.checks) },
        { key: 'review_decision', label: 'Review', sortable: false, render: p => p.redacted ? '' : reviewPill(p.review_decision) },
      ], rows: pulls, onRow: open, initialSort: { key: 'number', dir: 'desc' } })
      : empty('No pull request is linked yet. One is linked when its branch, a Conductor-Task trailer, or a T-ref in its description names this task.', `conductor task link-pr ${task.ref} 118`) });
}

function linkByNumber(ctx, task, done) {
  const input = h('input', { type: 'text', placeholder: '118, owner/repo#118, or a URL', 'aria-label': 'Pull request', autofocus: true });
  openModal({
    title: `Link a pull request to ${task.ref}`,
    body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'Pull request', input),
      h('div', { class: 'hint' }, 'A link made here outranks the branch, trailer and description rules, and stays until you unlink it.')),
    actions: [{ label: 'Cancel' }, { label: 'Link', kind: 'primary', onClick: async () => {
      const v = input.value.trim();
      if (!v) return false;
      try {
        await ctx.api.post(ctx.api.task(ctx.project, task.ref, '/pull-requests'), { pull_request: v });
        toast(`Linked ${v} to ${task.ref}`);
        done();
      } catch (err) { toastError(err, 'Link failed'); return false; }
    } }],
  });
  input.focus();
}

const FILTERS = [
  ['mine', 'Mine'], ['unlinked', 'Unlinked'], ['needs_review', 'Needs review'], ['failing', 'Failing checks'],
];

export default defineView({
  title: 'Pull requests',
  async load(ctx, state) {
    state.state = state.state || 'open';
    state.flags = state.flags || {};
    const query = { state: state.state };
    for (const [key] of FILTERS) if (state.flags[key]) query[key] = '1';
    const out = await ctx.api.get(pullsPath(ctx, query));
    return { pulls: out.pulls || [] };
  },
  draw({ pulls }, ctx, { refresh, state }) {
    state.q = state.q || '';
    const bodyEl = h('div');
    const match = p => {
      if (!state.q) return true;
      const q = state.q.toLowerCase();
      return [prName(p), p.title, p.head_ref, p.task_ref, p.author, p.author_handle].filter(Boolean).some(s => String(s).toLowerCase().includes(q));
    };
    const search = h('input', { type: 'search', placeholder: 'Filter by number, title, branch, task, author…', 'aria-label': 'Filter pull requests', value: state.q,
      oninput: ev => { state.q = ev.target.value; drawBody(); } });
    const stateSel = h('select', { 'aria-label': 'State', onchange: ev => { state.state = ev.target.value; refresh(); } },
      [['open', 'open'], ['merged', 'merged'], ['closed', 'closed'], ['all', 'all states']].map(([v, l]) => h('option', { value: v, selected: v === state.state }, l)));
    const flags = FILTERS.map(([key, label]) => h('label', { class: 'field', style: { flexDirection: 'row', alignItems: 'center', gap: '6px' } },
      h('input', { type: 'checkbox', checked: !!state.flags[key], onchange: ev => { state.flags[key] = ev.target.checked; refresh(); } }), label));

    function drawBody() {
      const list = pulls.filter(match);
      if (!pulls.length) return replace(bodyEl, empty(
        state.state === 'open' && !Object.values(state.flags).some(Boolean)
          ? 'No open pull requests yet. Link this project to its repository and install the GitHub App, and every pull request shows up here.'
          : 'No pull request matches.', 'conductor github status'));
      replace(bodyEl, card({ flush: true, body: table({
        columns: [
          { key: 'number', label: 'PR', num: true, render: p => h('span', { class: 'mono' }, '#' + p.number), sort: p => p.number },
          { key: 'state', label: 'State', render: p => pill(stateOf(p)) },
          { key: 'title', label: 'Title', render: p => p.redacted ? h('span', { class: 'private' }, 'pull request of a private task') : (p.title || h('span', { class: 'muted' }, '—')) },
          { key: 'head_ref', label: 'Branch', mono: true },
          { key: 'task_ref', label: 'Task', render: p => taskCell(p, ctx) },
          { key: 'checks', label: 'Checks', sortable: false, render: p => p.redacted ? '' : checksPill(p.checks) },
          { key: 'review_decision', label: 'Review', sortable: false, render: p => p.redacted ? '' : reviewPill(p.review_decision) },
          { key: 'author', label: 'Author', render: p => p.author_handle || p.author || '' },
          { key: 'updated_at', label: 'Updated', render: p => relTime(p.updated_at), sort: p => new Date(p.updated_at) },
        ],
        rows: list, rowKey: p => prName(p), onRow: p => openPullDrawer(p, ctx, { onChange: refresh }),
        initialSort: { key: 'updated_at', dir: 'desc' }, empty: empty('Nothing matches the filter.'),
        caption: 'Pull requests on the project\'s repository',
      }), footer: 'Titles, states, and counts only. Conductor never stores a pull request\'s description, comments, or CI logs.' }));
    }
    drawBody();
    return h('div', { class: 'stack' },
      h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), search), stateSel, ...flags),
      bodyEl);
  },
});
