import { h } from '../lib/dom.js';
import { card, empty } from '../components/ui.js';
import { pill, chip } from '../components/pill.js';
import { table } from '../components/table.js';
import { openModal, confirmModal } from '../components/modal.js';
import { toast, toastError } from '../components/toast.js';
import { relTime } from '../lib/format.js';

// The model-discovery half of the Fleet page: where models come from (inference endpoints),
// what they serve right now, and the dropdown that points a catalog profile at one of them.
// Discovery reads model names only; keys stay in the project secret vault.

const MAINTAINERS = ['maintainer', 'project_admin', 'org_admin'];
export const canManageModels = ctx => MAINTAINERS.includes(ctx.role);

// loadDiscovery fetches endpoints and the models they serve. A server without discovery (or a
// role without access) yields nulls, and the cards then simply do not appear.
export async function loadDiscovery(ctx, settle) {
  const [eps, models] = await settle([
    ctx.api.get(ctx.api.project(ctx.project, '/inference/endpoints')),
    ctx.api.get(ctx.api.project(ctx.project, '/inference/models')),
  ]);
  return { endpoints: eps ? eps.endpoints || [] : null, presets: eps ? eps.presets || [] : [], available: models ? models.models || [] : [] };
}

const stateKind = e => !e.enabled ? 'info' : e.last_status === 'ok' ? 'ok' : e.last_status === 'error' ? 'danger' : 'info';

export function endpointsCard(disc, ctx, refresh) {
  if (!disc.endpoints) return null;
  const manage = canManageModels(ctx);
  const newOnes = disc.available.filter(m => m.new && !m.missing);

  const run = async (label, fn) => {
    try { await fn(); } catch (err) { toastError(err, label); }
    refresh();
  };
  const refreshOne = e => run('Could not refresh', async () => {
    const out = await ctx.api.post(ctx.api.project(ctx.project, `/inference/endpoints/${encodeURIComponent(e.name)}/refresh`), {});
    if (out.refresh_error) toast(`${e.name}: ${out.refresh_error}`, { kind: 'danger', ttl: 8000 });
    else if (out.refresh) toast(`${e.name}: ${out.endpoint.model_count} models`, { detail: summarize(out.refresh) });
  });
  const remove = async e => {
    if (!await confirmModal({ title: `Remove ${e.name}?`, message: 'Its discovered models are forgotten. Profiles that selected from it keep the model they last resolved to.', confirmLabel: 'Remove', kind: 'danger' })) return;
    run('Could not remove', async () => { await ctx.api.del(ctx.api.project(ctx.project, `/inference/endpoints/${encodeURIComponent(e.name)}`)); toast(`Removed ${e.name}`); });
  };

  const body = disc.endpoints.length ? table({ caption: 'Inference endpoints', columns: [
    { key: 'name', label: 'Endpoint', mono: true },
    { key: 'kind', label: 'Kind', render: e => chip(e.kind, { mono: false }) },
    { key: 'last_status', label: 'State', render: e => pill(stateKind(e), !e.enabled ? 'off' : e.last_status) },
    { key: 'model_count', label: 'Models', num: true, render: e => h('button', { class: 'btn sm ghost', onclick: () => browse(e, disc, ctx) }, String(e.model_count)) },
    { key: 'last_refresh_at', label: 'Refreshed', render: e => e.last_refresh_at ? relTime(e.last_refresh_at) : 'never', sort: e => new Date(e.last_refresh_at || 0) },
    { key: 'base_url', label: 'Address', mono: true, render: e => h('div', {}, e.base_url,
      e.last_status === 'error' && e.last_error ? h('small', { class: 'muted', style: { display: 'block' } }, e.last_error) : null,
      e.api_key_secret ? h('small', { class: 'muted', style: { display: 'block' } }, 'key: ' + e.api_key_secret) : null) },
    manage ? { key: 'actions', label: '', sortable: false, render: e => h('div', { class: 'btn-row' },
      h('button', { class: 'btn sm', onclick: () => refreshOne(e) }, 'Refresh'),
      h('button', { class: 'btn sm ghost danger', onclick: () => remove(e) }, 'Remove')) } : null,
  ].filter(Boolean), rows: disc.endpoints })
    : empty('No inference endpoints yet. Add one and models are discovered instead of typed.', 'conductor models endpoints add anthropic');

  const recent = newOnes.length ? h('div', { class: 'chips', style: { padding: '0 16px 12px' } },
    h('span', { class: 'muted' }, 'New in the last week:'), ...newOnes.slice(0, 12).map(m => chip(m.model, { kind: 'info' }))) : null;

  return card({
    title: 'Inference endpoints — where models are discovered', flush: true,
    actions: manage ? h('button', { class: 'btn sm primary', onclick: () => addEndpoint(disc, ctx, refresh) }, 'Add endpoint') : null,
    body: h('div', {}, body, recent),
    footer: 'Each endpoint is asked which models it serves, on a schedule and on demand. Only model names are read; the API key stays in the project secret vault.',
  });
}

function summarize(r) {
  const parts = [];
  if (r.added && r.added.length) parts.push(`+${r.added.length} new`);
  if (r.retired && r.retired.length) parts.push(`−${r.retired.length} retired`);
  const moved = (r.resolutions || []).filter(x => x.changed).map(x => `${x.alias}→${x.to}`);
  if (moved.length) parts.push(moved.join(', '));
  return parts.join(' · ');
}

function browse(endpoint, disc, ctx) {
  const rows = disc.available.filter(m => m.endpoint === endpoint.name);
  const filter = h('input', { type: 'search', placeholder: 'filter models', 'aria-label': 'filter models' });
  const list = h('div', { class: 'stack' });
  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    const shown = rows.filter(m => !q || m.model.toLowerCase().includes(q) || (m.display_name || '').toLowerCase().includes(q));
    list.replaceChildren(shown.length ? table({ caption: `Models served by ${endpoint.name}`, columns: [
      { key: 'model', label: 'Model', mono: true, render: m => h('div', {}, m.model, m.new ? ' ' : null, m.new ? chip('new', { kind: 'info', mono: false }) : null,
        m.display_name ? h('small', { class: 'muted', style: { display: 'block' } }, m.display_name) : null) },
      { key: 'released', label: 'Released', render: m => m.released ? new Date(m.released).toLocaleDateString() : '—', sort: m => new Date(m.released || 0) },
      { key: 'used_by', label: 'Catalog', sortable: false, render: m => (m.used_by || []).map(u => chip(u, { mono: false })) },
    ], rows: shown }) : empty(rows.length ? 'No model matches the filter.' : 'This endpoint has not reported any models yet.'));
  };
  filter.addEventListener('input', draw);
  draw();
  openModal({ title: `${endpoint.name} — ${rows.length} models`, wide: true, body: h('div', { class: 'stack' }, filter, list), actions: [{ label: 'Close' }] });
}

function addEndpoint(disc, ctx, refresh) {
  const presets = disc.presets || [];
  const preset = h('select', {}, h('option', { value: '' }, 'Custom…'), ...presets.map(p => h('option', { value: p.name }, p.name + (p.local ? ' (local)' : ''))));
  const name = h('input', { type: 'text', placeholder: 'anthropic', required: true, pattern: '[a-z0-9][a-z0-9._-]*' });
  const kind = h('select', {}, ['anthropic', 'openai', 'openai_compatible', 'ollama'].map(k => h('option', { value: k }, k)));
  const url = h('input', { type: 'url', placeholder: 'http://localhost:8000/v1', required: true });
  const key = h('select', {}, h('option', { value: '' }, 'No key'));
  // Offer the project's existing secret names; the preset's usual one is chosen if it exists.
  ctx.api.get(ctx.api.project(ctx.project, '/secrets')).then(out => {
    for (const s of (out && out.secrets) || []) key.append(h('option', { value: s.name }, s.name));
  }).catch(() => {});
  const fill = () => {
    const p = presets.find(x => x.name === preset.value);
    if (!p) return;
    name.value = p.name; kind.value = p.kind; url.value = p.base_url;
    if (p.key_hint && [...key.options].some(o => o.value === p.key_hint)) key.value = p.key_hint;
    else if (!p.key_hint) key.value = '';
  };
  preset.addEventListener('change', fill);

  openModal({
    title: 'Add an inference endpoint',
    body: h('div', { class: 'form' },
      h('label', { class: 'field' }, 'Preset', preset),
      h('label', { class: 'field' }, 'Name', name),
      h('label', { class: 'field' }, 'Kind', kind),
      h('label', { class: 'field' }, 'Address', url),
      h('label', { class: 'field' }, 'API key (project secret)', key),
      h('div', { class: 'hint' }, 'Store the key first with `conductor secrets set NAME`. Plain http is accepted only for localhost and private addresses, for a local model server.')),
    actions: [{ label: 'Cancel' }, { label: 'Add and discover', kind: 'primary', onClick: async close => {
      if (!name.value.trim() || !url.value.trim()) { (name.value.trim() ? url : name).focus(); return false; }
      try {
        const out = await ctx.api.post(ctx.api.project(ctx.project, '/inference/endpoints'), {
          name: name.value.trim(), kind: kind.value, base_url: url.value.trim(), api_key_secret: key.value,
        });
        if (out.refresh_error) toast(`Added ${out.endpoint.name}, but listing failed: ${out.refresh_error}`, { kind: 'danger', ttl: 9000 });
        else toast(`Added ${out.endpoint.name}: ${out.endpoint.model_count} models discovered`);
        close(); refresh();
      } catch (err) { toastError(err, 'Could not add endpoint'); return false; }
    } }],
  });
}

// modelPicker is the dropdown on a catalog profile: every model the endpoints serve, the
// profile's own endpoint first, newest first. Choosing one pins it; "Follow discovery" lets go.
export function modelPicker(profile, disc, ctx, refresh) {
  if (!canManageModels(ctx) || !disc.endpoints || !disc.endpoints.length) return null;
  const prefix = (profile.select && profile.select.prefix) || '';
  const own = profile.select && profile.select.endpoint;
  const groups = new Map();
  for (const m of disc.available) {
    if (m.missing) continue;
    if (!groups.has(m.endpoint)) groups.set(m.endpoint, []);
    groups.get(m.endpoint).push(m);
  }
  const order = [...groups.keys()].sort((a, b) => (b === own) - (a === own) || a.localeCompare(b));
  const current = profile.model || '';
  const select = h('select', { 'aria-label': `Model for ${profile.alias} on ${profile.harness}`, class: 'model-picker' },
    h('option', { value: '' }, current ? 'Change model…' : 'Choose a model…'),
    ...order.map(ep => h('optgroup', { label: ep }, groups.get(ep).map(m =>
      h('option', { value: prefix + m.model, selected: prefix + m.model === current }, m.model + (m.new ? '  · new' : ''))))));
  select.addEventListener('change', async () => {
    if (!select.value) return;
    const model = select.value;
    try {
      await ctx.api.put(ctx.api.project(ctx.project, `/models/profiles/${encodeURIComponent(profile.alias)}/${encodeURIComponent(profile.harness)}/pin`), { model });
      toast(`${profile.alias} on ${profile.harness} is pinned to ${model}`);
    } catch (err) { toastError(err, 'Could not pin'); }
    refresh();
  });
  const release = profile.pinned ? h('button', { class: 'btn sm ghost', title: 'Let discovery choose again', onclick: async () => {
    try {
      await ctx.api.del(ctx.api.project(ctx.project, `/models/profiles/${encodeURIComponent(profile.alias)}/${encodeURIComponent(profile.harness)}/pin`));
      toast(`${profile.alias} on ${profile.harness} released`);
    } catch (err) { toastError(err, 'Could not release'); }
    refresh();
  } }, profile.select ? 'Follow discovery' : 'Unpin') : null;
  return h('div', { class: 'btn-row' }, select, release);
}
