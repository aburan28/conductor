import { h, icon } from '../lib/dom.js';
import { openModal } from './modal.js';
import { chip } from './pill.js';
import { toast, toastError } from './toast.js';

const MODES = ['write_exclusive', 'read_shared', 'review_shared', 'speculative_write', 'protected_exclusive'];

// The "New task" form. Everything typed here is coordination metadata that the team can see
// per the chosen visibility — the form says so, because a title is the one place a private
// prompt tends to leak.
export function openTaskForm(ctx, { onCreated } = {}) {
  const title = h('input', { type: 'text', required: true, placeholder: 'Add retry-aware model routing' });
  const objective = h('textarea', { placeholder: 'What done looks like. Keep it structural; it is visible to the team.' });
  const criteria = h('textarea', { placeholder: 'Acceptance criteria, one per line' });
  const priority = h('input', { type: 'number', value: 0, min: -100, max: 1000 });
  const risk = h('select', {}, ['unknown', 'low', 'medium', 'high', 'critical'].map(r => h('option', { value: r }, r)));
  const visibility = h('select', {}, [['', 'project default'], ['team_summary', 'team summary'], ['team_artifacts', 'team artifacts'], ['private', 'private (scopes only)']].map(([v, l]) => h('option', { value: v }, l)));
  const ready = h('input', { type: 'checkbox', checked: true });
  const externalRef = h('input', { type: 'text', placeholder: 'ISSUE-123' });
  const modelAlias = h('input', { type: 'text', placeholder: 'worker.general', list: 'alias-list' });
  const harness = h('select', {}, ['', 'claude', 'codex', 'opencode'].map(v => h('option', { value: v }, v || 'any')));

  const scopes = [];
  const scopeList = h('div', { class: 'chips' });
  const scopeInput = h('input', { type: 'text', placeholder: 'path:internal/api/handlers.go or dir:internal/router' });
  const scopeMode = h('select', {}, MODES.map(m => h('option', { value: m }, m)));
  const drawScopes = () => scopeList.replaceChildren(...scopes.map((s, i) => chip(`${s.resource} (${s.mode.split('_')[0]})`, { onRemove: () => { scopes.splice(i, 1); drawScopes(); } })));
  const addScope = () => {
    const v = scopeInput.value.trim();
    if (!v) return;
    scopes.push({ resource: v.includes(':') ? v : 'path:' + v, mode: scopeMode.value });
    scopeInput.value = '';
    drawScopes();
  };
  scopeInput.addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); addScope(); } });

  const labels = [];
  const labelList = h('div', { class: 'chips' });
  const labelInput = h('input', { type: 'text', placeholder: 'docs, backend, cheap — Enter to add' });
  const drawLabels = () => labelList.replaceChildren(...labels.map((l, i) => chip(l, { mono: false, kind: 'accent', onRemove: () => { labels.splice(i, 1); drawLabels(); } })));
  labelInput.addEventListener('keydown', ev => {
    if (ev.key === 'Enter' || ev.key === ',') {
      ev.preventDefault();
      const v = labelInput.value.trim().replace(/,$/, '');
      if (v && !labels.includes(v)) { labels.push(v); drawLabels(); }
      labelInput.value = '';
    }
  });
  const dependsOn = h('input', { type: 'text', placeholder: 'T-12, T-14' });

  const body = h('form', { class: 'form', onsubmit: ev => ev.preventDefault() },
    h('label', { class: 'field' }, 'Title', title),
    h('label', { class: 'field' }, 'Objective', objective),
    h('label', { class: 'field' }, 'Acceptance criteria', criteria),
    h('div', { class: 'field' }, h('span', {}, 'Scopes — the territory this task will hold'),
      h('div', { class: 'btn-row' }, scopeInput, scopeMode, h('button', { class: 'btn', type: 'button', onclick: addScope }, icon('plus'), 'Add')),
      scopeList),
    h('div', { class: 'field-row' },
      h('label', { class: 'field' }, 'Priority', priority),
      h('label', { class: 'field' }, 'Risk', risk),
      h('label', { class: 'field' }, 'Visibility', visibility),
      h('label', { class: 'field' }, 'External ref', externalRef)),
    h('div', { class: 'field' }, h('span', {}, 'Labels'), labelInput, labelList),
    h('div', { class: 'field-row' },
      h('label', { class: 'field' }, 'Depends on', dependsOn),
      h('label', { class: 'field' }, 'Model alias', modelAlias),
      h('label', { class: 'field' }, 'Harness', harness)),
    h('label', { class: 'field', style: { flexDirection: 'row', alignItems: 'center', gap: '8px' } }, ready, 'Ready for dispatch immediately'),
    h('div', { class: 'hint' }, 'Titles and objectives are visible to the project per visibility. Never paste private context here.'));

  openModal({
    title: 'New task', body, wide: true,
    actions: [{ label: 'Cancel' }, { label: 'Create task', kind: 'primary', submit: true, onClick: async () => {
      if (!title.value.trim()) { title.focus(); return false; }
      const payload = {
        title: title.value.trim(), objective: objective.value.trim(), external_ref: externalRef.value.trim(),
        status: ready.checked ? 'ready' : 'proposed', visibility: visibility.value, priority: Number(priority.value || 0),
        risk_level: risk.value, model_alias: modelAlias.value.trim(), harness: harness.value,
        acceptance_criteria: criteria.value.split('\n').map(s => s.trim()).filter(Boolean).map(text => ({ text })),
        scopes, labels,
        depends_on: dependsOn.value.split(/[,\s]+/).map(s => s.trim()).filter(Boolean),
      };
      try {
        const view = await ctx.api.post(ctx.api.project(ctx.project, '/tasks'), payload);
        toast(`Created ${view.ref}`, { detail: view.title });
        if (onCreated) onCreated(view);
      } catch (err) {
        if (err.blocked) toast('Refused: ' + err.message, { kind: 'warn', detail: err.body && err.body.advice });
        else toastError(err, 'Create failed');
        return false;
      }
    } }],
  });
}
