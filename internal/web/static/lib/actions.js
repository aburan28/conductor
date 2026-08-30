import { h } from './dom.js';
import { toast, toastError } from '../components/toast.js';
import { openModal, confirmModal } from '../components/modal.js';

// Every mutation the dashboard performs, in one place, with the exact request body each
// endpoint expects. Views call these and refresh; they never build request bodies.

export async function claimTask(ctx, ref) {
  try {
    const out = await ctx.api.post(ctx.api.task(ctx.project, ref, '/claim'), { harness: 'dashboard', role: 'implementer', allow_warnings: true });
    toast(`Claimed ${ref}`, { detail: out.fence ? `lease ${String(out.fence.lease_id).slice(0, 8)} · epoch ${out.fence.fencing_epoch}` : '' });
    return out;
  } catch (err) {
    if (err.blocked && err.body) toast(err.body.error || 'Claim refused', { kind: 'warn', detail: err.body.advice || err.body.reason || '' });
    else toastError(err, 'Claim failed');
    return null;
  }
}

export function releaseTask(ctx, ref) {
  return new Promise(resolve => {
    const status = h('select', {}, [['', 'decided by policy'], ['ready', 'ready'], ['proposed', 'proposed'], ['blocked_input', 'blocked (needs input)'], ['cancelled', 'cancelled']]
      .map(([v, l]) => h('option', { value: v }, l)));
    const note = h('input', { type: 'text', placeholder: 'why you are handing it back (optional)' });
    openModal({
      title: `Release ${ref}`,
      body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'Next status', status), h('label', { class: 'field' }, 'Note', note)),
      actions: [{ label: 'Cancel' }, { label: 'Release', kind: 'primary', onClick: async () => {
        try {
          await ctx.api.post(ctx.api.task(ctx.project, ref, '/release'), { next_status: status.value, reason: note.value });
          toast(`Released ${ref}. Its scopes are free.`); resolve(true);
        } catch (err) { toastError(err, 'Release failed'); return false; }
      } }],
      onClose: () => resolve(false),
    });
  });
}

export function transitionTask(ctx, ref, current) {
  return new Promise(resolve => {
    const states = ['proposed', 'ready', 'blocked_input', 'verifying', 'review_required', 'merging', 'done', 'failed', 'cancelled'];
    const sel = h('select', {}, states.map(s => h('option', { value: s, selected: s === current }, s)));
    openModal({
      title: `Transition ${ref}`,
      body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'To status', sel),
        h('div', { class: 'hint' }, 'Illegal transitions are refused by the ledger; this does not bypass leases or evidence.')),
      actions: [{ label: 'Cancel' }, { label: 'Apply', kind: 'primary', onClick: async () => {
        try { await ctx.api.post(ctx.api.task(ctx.project, ref, '/transition'), { to: sel.value }); toast(`${ref} → ${sel.value}`); resolve(true); }
        catch (err) { toastError(err, 'Transition refused'); return false; }
      } }],
      onClose: () => resolve(false),
    });
  });
}

export function cancelTask(ctx, ref) {
  return confirmModal({ title: `Cancel ${ref}?`, message: 'The task leaves the queue and releases its reserved scopes.', confirmLabel: 'Cancel task', kind: 'danger' })
    .then(async ok => {
      if (!ok) return false;
      try { await ctx.api.post(ctx.api.task(ctx.project, ref, '/transition'), { to: 'cancelled' }); toast(`Cancelled ${ref}`); return true; }
      catch (err) { toastError(err, 'Cancel refused'); return false; }
    });
}

export function assignTask(ctx, ref, sessions = []) {
  return new Promise(resolve => {
    const bySession = h('select', {}, h('option', { value: '' }, '— by requirement instead —'),
      sessions.filter(s => s.available !== false).map(s => h('option', { value: s.session_id || s.id },
        `${s.principal} · ${s.harness}${s.model ? ' · ' + s.model : ''}${s.tier ? ' · ' + s.tier : ''}`)));
    const tier = h('select', {}, ['', 'T1', 'T2', 'T3', 'T4'].map(t => h('option', { value: t }, t || 'any tier')));
    const effort = h('select', {}, ['', 'low', 'medium', 'high', 'xhigh', 'max'].map(e => h('option', { value: e }, e || 'any effort')));
    const harness = h('input', { type: 'text', placeholder: 'any harness' });
    const role = h('select', {}, ['', 'implementer', 'planner', 'verifier', 'reviewer', 'researcher'].map(r => h('option', { value: r }, r || 'any role')));
    openModal({
      title: `Offer ${ref} to a session`,
      body: h('div', { class: 'form' },
        h('label', { class: 'field' }, 'Specific session', bySession),
        h('div', { class: 'hint' }, 'Or name a floor and let Conductor pick the cheapest live session that clears it.'),
        h('div', { class: 'field-row' },
          h('label', { class: 'field' }, 'Tier ≥', tier), h('label', { class: 'field' }, 'Effort ≥', effort),
          h('label', { class: 'field' }, 'Harness', harness), h('label', { class: 'field' }, 'Role', role))),
      actions: [{ label: 'Cancel' }, { label: 'Offer', kind: 'primary', onClick: async () => {
        const body = { require: { tier: tier.value, reasoning_effort: effort.value, harness: harness.value.trim(), role: role.value } };
        if (bySession.value) body.session_id = bySession.value;
        try {
          const out = await ctx.api.post(ctx.api.task(ctx.project, ref, '/assign'), body);
          const who = out.choice ? `${out.choice.principal} (${out.choice.model || out.choice.harness})` : 'a session';
          toast(`${ref} offered to ${who}`); resolve(out);
        } catch (err) {
          if (err.body && err.body.rejected) toast('No session qualifies', { kind: 'warn', detail: err.body.rejected.map(r => `${r.principal}: ${r.reason}`).join(' · ') });
          else toastError(err, 'Offer failed');
          return false;
        }
      } }],
      onClose: () => resolve(null),
    });
  });
}

export function handoffTask(ctx, ref) {
  return new Promise(resolve => {
    const to = h('select', {}, ['', 'claude', 'codex', 'opencode', 'cursor'].map(v => h('option', { value: v }, v || 'whoever picks it up')));
    const next = h('input', { type: 'text', placeholder: 'what the next session should do first' });
    const done = h('textarea', { placeholder: 'completed work, one item per line' });
    openModal({
      title: `Hand off ${ref}`,
      body: h('div', { class: 'form' },
        h('label', { class: 'field' }, 'To harness', to), h('label', { class: 'field' }, 'Next action', next),
        h('label', { class: 'field' }, 'Completed', done),
        h('div', { class: 'hint' }, 'The bundle carries decisions and open questions — never a transcript.')),
      actions: [{ label: 'Cancel' }, { label: 'Hand off', kind: 'primary', onClick: async () => {
        try {
          await ctx.api.post(ctx.api.task(ctx.project, ref, '/handoff'), {
            to_harness: to.value,
            bundle: { recommended_next_action: next.value, completed_work: done.value.split('\n').map(s => s.trim()).filter(Boolean) },
          });
          toast(`Handed off ${ref}`); resolve(true);
        } catch (err) { toastError(err, 'Handoff failed'); return false; }
      } }],
      onClose: () => resolve(false),
    });
  });
}

export async function dispatchTask(ctx, ref) {
  try {
    const t = await ctx.api.post(ctx.api.project(ctx.project, '/queue'), { kind: 'attempt', task: ref });
    toast(t.state === 'granted' ? `${ref} admitted for dispatch` : `${ref} queued at position ${t.position ?? '?'}`);
    return t;
  } catch (err) {
    if (err.notFound) toast('Dispatch queue is not available on this server yet', { kind: 'warn', detail: 'Use: conductor dispatch ' + ref });
    else toastError(err, 'Dispatch failed');
    return null;
  }
}

export function resolveConflict(ctx, conflict) {
  return new Promise(resolve => {
    const state = h('select', {}, [['resolved', 'resolved — it is handled'], ['acknowledged', 'acknowledged — seen, still open'], ['ignored', 'ignored — not a real conflict']]
      .map(([v, l]) => h('option', { value: v }, l)));
    const note = h('input', { type: 'text', placeholder: 'why (required when ignoring)' });
    openModal({
      title: `Resolve ${conflict.mine.task_ref} ↔ ${conflict.other.task_ref}`,
      body: h('div', { class: 'form' }, h('p', { class: 'muted' }, conflict.reason || conflict.kind),
        h('label', { class: 'field' }, 'Mark as', state), h('label', { class: 'field' }, 'Note', note)),
      actions: [{ label: 'Cancel' }, { label: 'Apply', kind: 'primary', onClick: async () => {
        if (state.value === 'ignored' && !note.value.trim()) { note.focus(); return false; }
        try { await ctx.api.post(`/v1/conflicts/${encodeURIComponent(conflict.id)}/resolve`, { state: state.value, note: note.value }); toast(`Conflict marked ${state.value}`); resolve(true); }
        catch (err) { toastError(err, 'Could not resolve'); return false; }
      } }],
      onClose: () => resolve(false),
    });
  });
}

export function shareBudget(ctx, members = [], preset = '') {
  return new Promise(resolve => {
    const to = h('select', {}, members.filter(m => m.handle !== ctx.handle).map(m => h('option', { value: m.handle, selected: m.handle === preset }, m.handle)));
    const tokens = h('input', { type: 'text', placeholder: '500k, 2m, 250000', required: true });
    const note = h('input', { type: 'text', placeholder: 'what it is for (optional)' });
    openModal({
      title: 'Share token budget',
      body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'To', to), h('label', { class: 'field' }, 'Tokens', tokens), h('label', { class: 'field' }, 'Note', note),
        h('div', { class: 'hint' }, 'Grants are checked against your live balance and are not revocable — the recipient can share back.')),
      actions: [{ label: 'Cancel' }, { label: 'Share', kind: 'primary', onClick: async () => {
        const n = parseTokens(tokens.value);
        if (!n) { tokens.focus(); return false; }
        try { const out = await ctx.api.post(ctx.api.project(ctx.project, '/budget/share'), { to: to.value, tokens: n, note: note.value }); toast(`Shared ${tokens.value} tokens with ${to.value}`); resolve(out); }
        catch (err) { toastError(err, 'Share failed'); return false; }
      } }],
      onClose: () => resolve(null),
    });
  });
}

export function parseTokens(s) {
  const m = /^\s*([\d.]+)\s*([kmb]?)\s*$/i.exec(String(s || '').replace(/,/g, ''));
  if (!m) return 0;
  const mult = { k: 1e3, m: 1e6, b: 1e9, '': 1 }[m[2].toLowerCase()];
  return Math.round(parseFloat(m[1]) * mult);
}

export async function cancelTicket(ctx, ticket) {
  const ok = await confirmModal({ title: 'Leave the queue?', message: `Ticket ${String(ticket.id).slice(0, 8)} (${ticket.kind}) will be cancelled.`, confirmLabel: 'Cancel ticket', kind: 'danger' });
  if (!ok) return false;
  try { await ctx.api.del(`/v1/queue/${encodeURIComponent(ticket.id)}`); toast('Ticket cancelled'); return true; }
  catch (err) { toastError(err, 'Could not cancel'); return false; }
}

export async function respondToOffer(ctx, assignment, accept) {
  try {
    await ctx.api.post(`/v1/assignments/${encodeURIComponent(assignment.id)}/respond`, { accept, note: '' });
    toast(`${accept ? 'Accepted' : 'Declined'} ${assignment.task_ref || 'offer'}`);
    return true;
  } catch (err) { toastError(err, 'Could not respond'); return false; }
}

// Pause/resume a session from the dashboard. The request rides the session's next
// heartbeat, so it takes effect within ~20s; the pending state shows in the sessions
// view until the sidecar acknowledges it.
export async function pauseSession(ctx, session) {
  return sessionControl(ctx, session, 'pause');
}

export async function resumeSession(ctx, session) {
  return sessionControl(ctx, session, 'resume');
}

async function sessionControl(ctx, session, control) {
  try {
    await ctx.api.post(`/v1/sessions/${encodeURIComponent(session.id)}/${control}`, {});
    toast(`${control === 'pause' ? 'Pause' : 'Resume'} requested for ${session.principal}'s ${session.harness}`, { detail: 'Picked up on the session\u2019s next heartbeat.' });
    return true;
  } catch (err) { toastError(err, `Could not ${control} session`); return false; }
}
