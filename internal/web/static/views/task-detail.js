import { h, icon, replace } from '../lib/dom.js';
import { settle } from '../lib/view.js';
import { card, empty, kv, markdown, skeleton, errorBox } from '../components/ui.js';
import { pill, chip, chips, riskChip, tierChip } from '../components/pill.js';
import { table } from '../components/table.js';
import { relTime, fmtDate, fmtTokens, fmtUSD, fmtDuration, durationBetween } from '../lib/format.js';
import { claimTask, releaseTask, assignTask, transitionTask, handoffTask, dispatchTask, cancelTask } from '../lib/actions.js';
import { connectStream } from '../lib/sse.js';

// Task detail renders inside a drawer over the board on wide screens and as a full-width
// panel on narrow ones (the CSS decides). It is a route (/tasks/T-42), so it deep-links.
export function openTaskDrawer(ref, ctx, { onClose } = {}) {
  const body = h('div', { class: 'drawer-body' }, skeleton(8));
  const titleEl = h('span', { class: 'ref', style: { fontSize: '13px' } }, ref);
  const scrim = h('div', { class: 'drawer-scrim', onclick: () => close() });
  const drawer = h('aside', { class: 'drawer', role: 'dialog', 'aria-label': 'Task ' + ref },
    h('div', { class: 'drawer-bar' },
      h('button', { class: 'btn ghost icon', 'aria-label': 'close', onclick: () => close() }, icon('back')),
      titleEl, h('div', { class: 'spacer', style: { flex: 1 } }),
      h('button', { class: 'btn ghost sm', onclick: () => refresh() }, icon('refresh'), 'Refresh')),
    body);
  let alive = true;
  const onKey = ev => { if (ev.key === 'Escape') close(); };
  // Live log streams outlive a render; they are closed here and re-created per refresh.
  const disposers = [];
  document.addEventListener('keydown', onKey);
  document.body.append(scrim, drawer);

  function close() {
    if (!alive) return;
    alive = false;
    disposers.splice(0).forEach(d => d());
    scrim.remove(); drawer.remove();
    document.removeEventListener('keydown', onKey);
    if (onClose) onClose();
  }

  async function refresh() {
    disposers.splice(0).forEach(d => d());
    const p = ctx.project;
    const t = suffix => ctx.api.task(p, ref, suffix);
    const [task, attempts, validation, decisions, handoff, cardText, caps, explain] = await settle([
      ctx.api.get(t('')),
      ctx.api.get(t('/attempts')),
      ctx.api.get(t('/validation')),
      ctx.api.get(t('/decisions')),
      ctx.api.get(t('/handoff')),
      ctx.api.get(t('/card')),
      ctx.api.get(ctx.api.project(p, '/capabilities')),
      ctx.api.post(t('/route/explain'), {}),
    ]);
    if (!alive) return;
    if (!task) { replace(body, errorBox(new Error(`Task ${ref} could not be loaded`), refresh)); return; }
    replace(body, render({ task, attempts: (attempts && attempts.attempts) || [], validation: (validation && validation.results) || task.validation || [],
      decisions: (decisions && decisions.decisions) || [], handoff: handoff && handoff.bundle ? handoff.bundle : handoff && handoff.task_ref ? handoff : null,
      cardText: typeof cardText === 'string' ? cardText : '', sessions: (caps && caps.sessions) || [], explain }));
  }

  // The live attempt log: subscribe over SSE, append into a monospace pane, follow the
  // bottom unless the user scrolled up. Stopped on drawer close, on re-render, and by the
  // button.
  function attemptLogs(ref) {
    const pane = h('pre', { class: 'log-stream', 'aria-live': 'polite' });
    const stateEl = h('span', { class: 'muted', style: { fontSize: '12px' } }, 'connecting…');
    let stop = null;
    let follow = true;

    pane.addEventListener('scroll', () => {
      follow = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24;
    });
    const append = text => {
      pane.append(document.createTextNode(text));
      if (follow) pane.scrollTop = pane.scrollHeight;
    };

    const btn = h('button', { class: 'btn sm' }, 'Stop');
    function halt() {
      if (!stop) return;
      stop();
      stop = null;
      btn.textContent = 'Start';
    }
    function start() {
      if (stop) return;
      btn.textContent = 'Stop';
      stateEl.textContent = 'connecting…';
      const url = `/v1/tasks/${encodeURIComponent(ref)}/logs?project=${encodeURIComponent(ctx.project)}&token=${encodeURIComponent(ctx.api.token)}`;
      stop = connectStream(url, {
        onEvent: ev => {
          if (ev.type === 'log') { append(ev.text || ''); stateEl.textContent = ''; }
          else if (ev.type === 'waiting') stateEl.textContent = ev.reason || 'waiting…';
          else if (ev.type === 'done') { stateEl.textContent = ev.state ? 'attempt ' + ev.state : 'ended'; halt(); }
        },
        onState: st => { if (st !== 'live' && stop) stateEl.textContent = st + '…'; },
      });
    }
    btn.addEventListener('click', () => (stop ? halt() : start()));
    disposers.push(halt);
    // Demo mode has no server to stream from; the fixture API cannot carry SSE.
    if (ctx.store.get().demo) stateEl.textContent = 'demo mode — no live stream';
    else start();

    return card({
      title: 'Attempt log', flush: true,
      actions: h('div', { class: 'btn-row' }, stateEl, btn),
      body: pane,
      footer: 'Raw harness output, tailed from the attempt worktree (.conductor/attempt.log) while the attempt runs. The tree — and the log with it — is removed when the attempt succeeds.',
    });
  }

  function render({ task, attempts, validation, decisions, handoff, cardText, sessions, explain }) {
    const act = async fn => { const r = await fn(); if (r) { refresh(); if (ctx.refreshAll) ctx.refreshAll(); } };
    const isOpen = !['done', 'cancelled', 'superseded', 'failed'].includes(task.status);
    const actions = h('div', { class: 'btn-row' },
      ['ready', 'proposed', 'blocked_input'].includes(task.status) ? h('button', { class: 'btn primary', onclick: () => act(() => claimTask(ctx, task.ref)) }, 'Claim') : null,
      ['claimed', 'running'].includes(task.status) ? h('button', { class: 'btn', onclick: () => act(() => releaseTask(ctx, task.ref)) }, 'Release') : null,
      isOpen ? h('button', { class: 'btn', onclick: () => act(() => assignTask(ctx, task.ref, sessions)) }, 'Offer to session…') : null,
      ['ready', 'proposed'].includes(task.status) ? h('button', { class: 'btn', onclick: () => act(() => dispatchTask(ctx, task.ref)) }, icon('bolt'), 'Dispatch now') : null,
      ['claimed', 'running'].includes(task.status) ? h('button', { class: 'btn', onclick: () => act(() => handoffTask(ctx, task.ref)) }, 'Hand off…') : null,
      h('button', { class: 'btn', onclick: () => act(() => transitionTask(ctx, task.ref, task.status)) }, 'Transition…'),
      isOpen ? h('button', { class: 'btn danger', onclick: () => act(() => cancelTask(ctx, task.ref)) }, 'Cancel') : null);

    const head = h('div', { class: 'detail-head' },
      h('h2', {}, task.title ? task.title : h('span', { class: 'private' }, 'private work')),
      pill(task.status), task.owner ? chip(task.owner, { mono: false }) : null, riskChip(task.risk_level),
      task.priority ? chip('p' + task.priority, { mono: false }) : null, task.visibility ? chip(task.visibility, { mono: false }) : null,
      task.external_ref ? chip(task.external_ref) : null,
      ...(task.labels || []).map(l => chip(l, { mono: false, kind: 'accent' })));

    const criteria = (task.acceptance_criteria || []);
    const overview = card({ title: 'Objective', body: h('div', { class: 'stack' },
      task.objective ? h('p', {}, task.objective) : h('p', { class: 'muted' }, task.redacted ? 'Objective withheld: this task is private to its owner.' : 'No objective recorded.'),
      criteria.length ? h('div', {}, h('h3', { style: { marginBottom: '6px' } }, 'Acceptance criteria'), h('ul', { class: 'criteria' }, criteria.map(c =>
        h('li', {}, h('span', { class: 'mark ' + (c.status || ''), 'aria-label': c.status || 'pending' }, c.status === 'pass' ? '✓' : c.status === 'fail' ? '✕' : ''), h('span', {}, c.text, c.evidence ? h('span', { class: 'muted' }, ' · ' + c.evidence) : null))))) : null) });

    const facts = card({ title: 'Facts', body: kv([
      ['Ref', h('span', { class: 'mono' }, task.ref)],
      ['Owner', task.owner || '—'],
      ['Harness', task.harness || 'any'],
      ['Model alias', task.model_alias || 'by policy'],
      ['Fencing epoch', String(task.fencing_epoch ?? 0)],
      ['Attempts', String(task.attempts_count || 0)],
      ['Branch', task.branch ? h('span', { class: 'mono' }, task.branch) : null],
      ['Commit', task.commit_sha ? h('span', { class: 'mono' }, task.commit_sha.slice(0, 12)) : null],
      ['Depends on', task.depends_on && task.depends_on.length ? h('div', { class: 'chips' }, task.depends_on.map(d => h('a', { class: 'chip', href: `/tasks/${encodeURIComponent(d)}`, 'data-link': true }, d))) : null],
      ['Scopes', task.scopes && task.scopes.length ? chips(task.scopes) : h('span', { class: 'muted' }, 'none reserved')],
      ['Created', fmtDate(task.created_at)],
      ['Updated', relTime(task.updated_at)],
    ]) });

    const route = card({ title: 'Route preview — what the dispatch policy would decide', body: !explain
      ? h('div', { class: 'muted' }, 'Route explanation is not available on this server. Try: ', h('code', {}, `conductor route ${task.ref} --explain`))
      : h('div', { class: 'stack' },
        explain.decision ? h('div', { class: 'chips' },
          chip('lane ' + (explain.decision.lane || '—'), { mono: false, kind: 'accent' }), chip(explain.decision.role || 'implementer', { mono: false }),
          chip((explain.decision.harness || '?') + ' · ' + (explain.decision.model || '?')), tierChip(explain.decision.tier),
          explain.decision.reasoning_effort ? chip('effort ' + explain.decision.reasoning_effort, { mono: false }) : null,
          explain.decision.rule ? chip('rule ' + explain.decision.rule, { mono: false, kind: 'info' }) : null) : h('div', { class: 'muted' }, 'No decision returned.'),
        explain.decision && explain.decision.rationale ? h('ul', { class: 'rationale' }, explain.decision.rationale.map(r => h('li', {}, r))) : null,
        explain.candidates && explain.candidates.length ? table({ columns: [
          { key: 'lane', label: 'Lane' }, { key: 'model', label: 'Model', mono: true }, { key: 'harness', label: 'Harness' },
          { key: 'eligible', label: 'Eligible', render: c => c.eligible ? pill('ok', 'yes') : pill('danger', 'no') }, { key: 'reason', label: 'Why' },
        ], rows: explain.candidates }) : null) });

    const attemptsCard = card({ title: 'Attempts', flush: true, body: attempts.length ? table({
      columns: [
        { key: 'attempt_number', label: '#', num: true },
        { key: 'state', label: 'State', render: a => pill(a.state) },
        { key: 'role', label: 'Role' },
        { key: 'harness', label: 'Harness' },
        { key: 'resolved_model', label: 'Model', mono: true, render: a => a.resolved_model || a.model_alias || h('span', { class: 'muted' }, '—') },
        { key: 'reasoning_effort', label: 'Effort' },
        { key: 'branch', label: 'Branch', mono: true },
        { key: 'tokens_in', label: 'In', num: true, render: a => fmtTokens(a.tokens_in) },
        { key: 'tokens_out', label: 'Out', num: true, render: a => fmtTokens(a.tokens_out) },
        { key: 'cost_usd', label: 'Cost', num: true, render: a => fmtUSD(a.cost_usd) },
        { key: 'started_at', label: 'Duration', render: a => durationBetween(a.started_at, a.ended_at), sort: a => new Date(a.started_at || 0) },
        { key: 'failure_class', label: 'Failure', render: a => a.failure_class ? chip(a.failure_class, { kind: 'danger', mono: false }) : '' },
      ], rows: attempts, initialSort: { key: 'attempt_number', dir: 'desc' } }) : empty('No attempts yet.', `conductor dispatch ${task.ref}`) });

    const validationCard = card({ title: 'Validation — runner-attested', flush: true, body: validation.length ? table({
      columns: [
        { key: 'command', label: 'Command', mono: true },
        { key: 'exit_code', label: 'Exit', num: true, render: v => v.exit_code === 0 ? pill('ok', '0') : pill('danger', String(v.exit_code)) },
        { key: 'duration_ms', label: 'Duration', num: true, render: v => fmtDuration(v.duration_ms) },
        { key: 'created_at', label: 'When', render: v => relTime(v.created_at), sort: v => new Date(v.created_at) },
      ], rows: validation }) : empty('No checks recorded. A model saying "tests pass" is not evidence; the runner records exit codes here.') });

    const decisionsCard = card({ title: 'Policy decisions', body: decisions.length ? h('div', { class: 'stack' }, decisions.map(d => h('div', { class: 'row' },
      h('div', { class: 'who', style: { minWidth: '90px' } }, chip(d.kind, { mono: false, kind: 'info' })),
      h('div', { class: 'what' }, h('div', {}, d.decision), d.rationale && Object.keys(d.rationale).length ? h('ul', { class: 'rationale' },
        (Array.isArray(d.rationale.rationale) ? d.rationale.rationale : Object.entries(d.rationale).map(([k, v]) => `${k}: ${typeof v === 'object' ? JSON.stringify(v) : v}`)).map(r => h('li', {}, r))) : null),
      h('div', { class: 'meta' }, relTime(d.created_at))))) : empty('No routing decisions recorded yet.') });

    const handoffCard = handoff ? card({ title: 'Handoff bundle', body: h('div', { class: 'stack' },
      handoff.recommended_next_action ? h('div', {}, h('strong', {}, 'Next: '), handoff.recommended_next_action) : null,
      handoff.completed_work && handoff.completed_work.length ? h('div', {}, h('h3', {}, 'Completed'), h('ul', {}, handoff.completed_work.map(x => h('li', {}, x)))) : null,
      handoff.open_questions && handoff.open_questions.length ? h('div', {}, h('h3', {}, 'Open questions'), h('ul', {}, handoff.open_questions.map(x => h('li', {}, x)))) : null,
      handoff.assumptions && handoff.assumptions.length ? h('div', {}, h('h3', {}, 'Assumptions'), h('ul', {}, handoff.assumptions.map(x => h('li', {}, x)))) : null,
      handoff.decisions && handoff.decisions.length ? h('div', {}, h('h3', {}, 'Decisions'), h('ul', {}, handoff.decisions.map(x => h('li', {}, x.summary || JSON.stringify(x))))) : null,
      h('div', { class: 'muted', style: { fontSize: '12px' } }, 'A bundle carries decisions and open questions, never the conversation that produced them.')),
    }) : null;

    const cardEl = cardText ? card({ title: 'Task card', body: markdown(cardText), footer: 'Rendered from the same Markdown the CLI exports with `conductor task export`.' }) : null;

    const logsCard = ['claimed', 'running'].includes(task.status) ? attemptLogs(ref) : null;

    return h('div', { class: 'stack', style: { gap: '16px' } },
      head, actions,
      h('div', { class: 'detail-grid' },
        h('div', { class: 'stack', style: { gap: '16px' } }, overview, route, logsCard, attemptsCard, validationCard, decisionsCard, handoffCard, cardEl),
        h('div', { class: 'stack', style: { gap: '16px' } }, facts)));
  }

  refresh();
  return { close, refresh };
}

// Fallback when the drawer cannot mount over a board (direct navigation on a narrow device).
export default {
  title: 'Task',
  render(root, ctx) {
    const drawer = openTaskDrawer(ctx.params.ref, ctx, { onClose: () => ctx.navigate('/tasks') });
    return { refresh: () => drawer.refresh(), destroy: () => drawer.close() };
  },
};
