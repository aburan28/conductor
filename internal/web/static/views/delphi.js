import { h } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty } from '../components/ui.js';
import { relTime } from '../lib/format.js';

// Delphi keeps request and answer text at the provider. Drafts and fetched answers stay
// in this page's memory only; navigation or reload discards them.
const view = defineView({
  title: 'Delphi',
  async load(ctx) {
    const base = ctx.api.project(ctx.project, '/delphi');
    const [profiles, list] = await Promise.all([
      ctx.api.get(base + '/profiles'),
      ctx.api.get(base + '?limit=100'),
    ]);
    return { profiles: profiles.profiles || [], rows: list.delegations || [] };
  },
  draw({ profiles, rows }, ctx, { refresh, state }) {
    state.draft ||= { profile: '', provider: 'anthropic', model: 'claude-mythos-5-1', effort: '', maxTokens: '8192', system: '', text: '' };
    if (!profiles.length && !state.draft.profile) state.draft.profile = 'custom';
    const draft = state.draft;
    const base = ctx.api.project(ctx.project, '/delphi');
    const canWrite = ['contributor', 'maintainer', 'project_admin', 'org_admin'].includes(ctx.role);
    const selected = profiles.find(p => p.name === draft.profile);

    const profile = h('select', { value: draft.profile, onchange: ev => { draft.profile = ev.target.value; refresh(); } },
      h('option', { value: '', selected: draft.profile === '' }, 'Choose a model profile'),
      profiles.map(p => h('option', { value: p.name, selected: draft.profile === p.name }, `${p.name} · ${p.provider}/${p.model}`)),
      h('option', { value: 'custom', selected: draft.profile === 'custom' }, 'Custom provider and model'));
    const custom = draft.profile === 'custom' ? h('div', { class: 'field-row' },
      field('Provider', h('select', { value: draft.provider, onchange: ev => { draft.provider = ev.target.value; } },
        h('option', { value: 'anthropic', selected: draft.provider === 'anthropic' }, 'Anthropic'),
        h('option', { value: 'openai', selected: draft.provider === 'openai' }, 'OpenAI'))),
      field('Model ID', h('input', { type: 'text', value: draft.model, placeholder: 'claude-mythos-5-1', oninput: ev => { draft.model = ev.target.value; } }))) : null;
    const system = h('textarea', { rows: 3, value: draft.system, placeholder: 'Optional instructions', oninput: ev => { draft.system = ev.target.value; } });
    const prompt = h('textarea', { rows: 8, value: draft.text, placeholder: 'What should this model assess?', required: true,
      oninput: ev => { draft.text = ev.target.value; } });
    const effort = h('select', { value: draft.effort, onchange: ev => { draft.effort = ev.target.value; } },
      [['', 'Profile or provider default'], ['low', 'Low'], ['medium', 'Medium'], ['high', 'High'], ['max', 'Max']]
        .map(([v, label]) => h('option', { value: v, selected: draft.effort === v }, label)));
    const tokens = h('input', { type: 'number', min: 1, max: 128000, value: draft.maxTokens,
      oninput: ev => { draft.maxTokens = ev.target.value; } });

    const submit = h('button', { class: 'btn primary', type: 'button', disabled: !canWrite,
      onclick: async () => {
        if (!state.pending) {
          if (!draft.text.trim()) { state.message = 'Enter a question before launching.'; refresh(); return; }
          if (!draft.profile || (draft.profile === 'custom' && !draft.model.trim())) {
            state.message = 'Choose a profile or enter a model ID.'; refresh(); return;
          }
          const maxTokens = Number(draft.maxTokens);
          if (!Number.isInteger(maxTokens) || maxTokens < 1 || maxTokens > 128000) {
            state.message = 'Set an output token ceiling between 1 and 128000.'; refresh(); return;
          }
          const random = crypto.getRandomValues(new Uint8Array(12));
          const body = { ref: 'ui-' + Array.from(random, b => b.toString(16).padStart(2, '0')).join(''), text: draft.text,
            max_tokens: maxTokens };
          if (draft.system) body.system = draft.system;
          if (draft.effort) body.effort = draft.effort;
          if (draft.profile === 'custom') {
            body.provider = draft.provider;
            body.model = draft.model.trim();
          } else body.profile = draft.profile;
          state.pending = body;
        }
        submit.disabled = true;
        try {
          const result = await ctx.api.post(base, state.pending);
          state.selectedRef = result.ref;
          state.pending = null;
          state.message = result.state === 'submitting'
            ? `Request ${result.ref} is submitting. Its provider outcome is not yet known; check its status before retrying.`
            : `Request ${result.ref} launched with ${result.provider}/${result.model}.`;
          state.draft = { ...draft, system: '', text: '' };
          await refresh();
        } catch (err) {
          // An ambiguous network/server failure may have reached the provider. Keep the
          // same ref and body for an idempotent retry; let validation errors be edited.
          if (err.status >= 400 && err.status < 500) state.pending = null;
          state.message = err.message + (state.pending ? ' Retry uses the same request reference and text.' : '');
          await refresh();
        }
      } }, state.pending ? 'Retry same request' : 'Launch batch');

    const form = card({ title: 'New model call', body: h('div', { class: 'stack' },
      h('p', { class: 'muted' }, 'Choose a catalog profile or a provider model. The server uses its configured project credential or workload identity; the browser never handles either one.'),
      field('Model profile', profile),
      selected ? h('div', { class: 'notice' }, `Resolved to ${selected.provider}/${selected.model}${selected.effort ? ' · ' + selected.effort + ' effort' : ''}.`) : null,
      custom,
      h('div', { class: 'field-row' }, field('Effort', effort), field('Output token ceiling', tokens)),
      field('Instructions (optional)', system), field('Question', prompt),
      state.pending ? h('div', { class: 'notice warn' }, `Submission outcome is unknown. The next click retries reference ${state.pending.ref} with exactly the same body.`) : null,
      state.message ? h('div', { class: 'notice', role: 'status' }, state.message) : null,
      h('div', { class: 'btn-row' }, submit, state.pending ? h('button', { class: 'btn', type: 'button', onclick: async () => {
        try {
          const d = await ctx.api.get(base + '/' + encodeURIComponent(state.pending.ref));
          state.selectedRef = d.ref;
          state.pending = null;
          state.message = `Found ${d.ref} in state ${d.state}.`;
          await refresh();
        } catch (err) { state.message = err.message; await refresh(); }
      } }, 'Check status') : null),
      h('p', { class: 'muted' }, 'A batch can take time. Conductor stores request metadata, but not your question or the model answer. Fetched answers remain visible only while this page is open.')) });

    const active = rows.filter(d => d.state === 'running' || d.state === 'submitting');
    const chosen = rows.find(d => d.ref === state.selectedRef);
    const history = card({ title: `Delegations · ${active.length} in progress`, body: rows.length
      ? h('div', { class: 'stack' }, rows.map(d => h('div', { class: 'delphi-row' },
        h('div', { class: 'stack', style: { gap: '3px' } },
          h('strong', { class: 'mono' }, d.ref),
          h('span', { class: 'muted' }, `${d.provider}/${d.model} · ${d.status || d.state} · ${relTime(d.updated_at)}`)),
        h('div', { class: 'btn-row' },
          h('button', { class: 'btn sm', type: 'button', onclick: () => { state.selectedRef = d.ref; state.answer = null; refresh(); } }, 'Details'),
          canWrite && d.state === 'finished' && (d.status === 'succeeded' || d.status === 'refused') ? h('button', { class: 'btn sm', type: 'button', onclick: async () => {
            try {
              state.answer = await ctx.api.get(base + '/' + encodeURIComponent(d.ref) + '/answer');
              state.selectedRef = d.ref;
              await refresh();
            } catch (err) { state.message = err.message; await refresh(); }
          } }, 'Read answer') : null,
          canWrite && d.state === 'running' ? h('button', { class: 'btn sm', type: 'button', onclick: async () => {
            if (!confirm(`Cancel batch ${d.ref}?`)) return;
            try { await ctx.api.post(base + '/' + encodeURIComponent(d.ref) + '/cancel'); await refresh(); }
            catch (err) { state.message = err.message; await refresh(); }
          } }, 'Cancel') : null))))
      : empty('No Delphi calls in this project yet. Choose a model and launch one above.'),
      footer: 'A “submitting” request may already exist at the provider. Check its state before making another call.' });

    const detail = chosen ? card({ title: `Request ${chosen.ref}`, body: h('div', { class: 'stack' },
      h('div', {}, `${chosen.provider}/${chosen.model} · ${chosen.status || chosen.state}`),
      h('div', { class: 'muted' }, `${chosen.input_tokens || 0} input · ${chosen.output_tokens || 0} output tokens${chosen.cost_usd ? ' · $' + Number(chosen.cost_usd).toFixed(4) : ''}`),
      chosen.error_detail ? h('div', { class: 'notice warn' }, chosen.error_detail) : null,
      state.answer && state.answer.delegation?.ref === chosen.ref
        ? (state.answer.text ? h('pre', { class: 'delphi-answer', tabindex: '0' }, state.answer.text)
          : h('div', { class: 'notice' }, state.answer.unavailable || state.answer.refusal?.category || 'No answer text is available.')) : null) }) : null;

    return h('div', { class: 'stack' }, form, history, detail);
  },
});

function field(label, control) { return h('label', { class: 'field' }, label, control); }

export default {
  title: view.title,
  render(root, ctx) {
    const instance = view.render(root, ctx);
    const timer = setInterval(() => instance.refresh(), 15000);
    return { ...instance, destroy: () => { clearInterval(timer); instance.destroy(); } };
  },
};
