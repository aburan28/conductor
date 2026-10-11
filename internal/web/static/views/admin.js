import { h, icon, replace } from '../lib/dom.js';
import { settle } from '../lib/view.js';
import { card, empty, kv, snippet, skeleton, errorBox } from '../components/ui.js';
import { chip, pill } from '../components/pill.js';
import { table } from '../components/table.js';
import { openModal, confirmModal } from '../components/modal.js';
import { toast, toastError } from '../components/toast.js';
import { fmtDate, relTime } from '../lib/format.js';

// The admin area, for an organization's administrators (org_admin). Every setting the
// server's config file decides is shown as managed by it and cannot be edited here; the
// server refuses such a change anyway. Role changes and identity unlinking go through the
// same project routes as everywhere else, so their rules (no grant above your own role, the
// last administrator) apply unchanged.

const SECTIONS = [
  ['organization', 'Organization'],
  ['authentication', 'Authentication'],
  ['provisioning', 'Provisioning'],
  ['members', 'Members & roles'],
  ['features', 'Features'],
  ['audit', 'Audit log'],
  ['configuration', 'Configuration'],
];

const ROLES = ['observer', 'reviewer', 'contributor', 'maintainer', 'project_admin', 'org_admin'];

export default {
  title: 'Admin',
  render(root, ctx) {
    const section = SECTIONS.some(([id]) => id === ctx.params.section) ? ctx.params.section : 'organization';
    let alive = true;
    const body = h('div', { class: 'stack', style: { gap: '16px' } }, skeleton(6));
    root.replaceChildren(h('div', { class: 'admin' },
      h('nav', { class: 'toc', 'aria-label': 'Admin sections' }, h('ul', {}, SECTIONS.map(([id, label]) =>
        h('li', {}, h('a', { href: '/admin/' + id, 'data-link': true, 'aria-current': id === section ? 'page' : null, class: id === section ? 'active' : '' }, label))))),
      h('div', { class: 'admin-body' }, h('h2', { class: 'section-title' }, SECTIONS.find(([id]) => id === section)[1]), body)));

    async function refresh() {
      try {
        const node = await RENDER[section](ctx, refresh);
        if (alive) replace(body, node);
      } catch (err) {
        if (alive) replace(body, errorBox(err, refresh));
      }
    }
    refresh();
    return { refresh, destroy: () => { alive = false; } };
  },
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const lockedNote = () => h('span', { class: 'locked' }, icon('lock', 12), 'Managed by your administrator\'s config file');

async function patchPolicy(ctx, patch, done) {
  try {
    await ctx.api.patch('/v1/admin/policy', patch);
    toast('Saved');
    if (ctx.refreshOrg) ctx.refreshOrg();
    if (done) done();
    return true;
  } catch (err) {
    toastError(err, 'Not saved');
    return false;
  }
}

function field(label, control, { locked, hint } = {}) {
  if (locked) {
    for (const el of [control, ...control.querySelectorAll('input, select, textarea')]) {
      if ('disabled' in el) el.disabled = true;
    }
  }
  return h('label', { class: 'field' + (locked ? ' is-locked' : '') }, label, control, locked ? lockedNote() : null, hint ? h('span', { class: 'hint' }, hint) : null);
}

function toggle(label, checked, { locked, hint, onChange } = {}) {
  const input = h('input', { type: 'checkbox', checked, disabled: locked, onchange: ev => onChange && onChange(ev.target.checked) });
  return h('div', { class: 'toggle-row' }, h('label', { class: 'toggle' }, input, h('span', {}, label)),
    locked ? lockedNote() : null, hint ? h('p', { class: 'hint' }, hint) : null);
}

const list = v => (v || []).join(', ');
const splitList = v => v.split(/[,\s]+/).map(x => x.trim()).filter(Boolean);

async function loadPolicy(ctx) {
  const out = await ctx.api.get('/v1/admin/policy');
  out.isLocked = key => (out.locked || []).includes(key);
  return out;
}

// ---------------------------------------------------------------------------
// Sections
// ---------------------------------------------------------------------------

const RENDER = {
  async organization(ctx, refresh) {
    const pol = await loadPolicy(ctx);
    const b = pol.policy.branding || {};
    const name = h('input', { type: 'text', value: b.display_name || '', maxlength: 80, placeholder: 'Acme Engineering' });
    const color = h('input', { type: 'text', value: b.accent_color || '', placeholder: '#2f6f4e', pattern: '#[0-9a-fA-F]{3,6}', spellcheck: false });
    const picker = h('input', { type: 'color', value: b.accent_color || '#2f6f4e', 'aria-label': 'Pick the accent color', oninput: ev => { color.value = ev.target.value; } });
    const banner = h('textarea', { rows: 3, maxlength: 2000, placeholder: 'Authorized use only. Activity is logged.' }, b.login_banner || '');
    const save = () => {
      const branding = {};
      if (!pol.isLocked('branding.display_name')) branding.display_name = name.value;
      if (!pol.isLocked('branding.accent_color')) branding.accent_color = color.value;
      if (!pol.isLocked('branding.login_banner')) branding.login_banner = banner.value;
      return patchPolicy(ctx, { branding }, refresh);
    };
    const logoLocked = pol.isLocked('branding.logo');
    const preview = h('div', { class: 'logo-preview', 'aria-hidden': 'true' }, icon('bolt', 15));
    if (pol.branding && pol.branding.has_logo) {
      fetch('/v1/org/logo', { headers: { Authorization: 'Bearer ' + ctx.token } }).then(r => (r.ok ? r.blob() : null)).then(blob => {
        if (!blob) return;
        const fr = new FileReader();
        fr.onload = () => preview.replaceChildren(h('img', { src: fr.result, alt: '' }));
        fr.readAsDataURL(blob);
      }).catch(() => {});
    }
    const file = h('input', { type: 'file', accept: 'image/png,image/jpeg,image/gif', disabled: logoLocked, 'aria-label': 'Logo file (PNG, JPEG or GIF, at most 64 KiB)',
      onchange: ev => {
        const f = ev.target.files[0];
        if (!f) return;
        if (f.size > 64 * 1024) { toast('That file is larger than 64 KiB', { kind: 'warn' }); return; }
        const fr = new FileReader();
        fr.onload = async () => {
          const data = String(fr.result).split(',')[1] || '';
          try { await ctx.api.request('PUT', '/v1/admin/branding/logo', { content_type: f.type, data }); toast('Logo saved'); if (ctx.refreshOrg) ctx.refreshOrg(); refresh(); }
          catch (err) { toastError(err, 'Logo not saved'); }
        };
        fr.readAsDataURL(f);
      } });
    const removeLogo = async () => {
      try { await ctx.api.del('/v1/admin/branding/logo'); toast('Logo removed'); if (ctx.refreshOrg) ctx.refreshOrg(); refresh(); }
      catch (err) { toastError(err, 'Logo not removed'); }
    };
    return h('div', { class: 'stack', style: { gap: '16px' } },
      card({ title: 'Branding', body: h('form', { class: 'form', onsubmit: ev => { ev.preventDefault(); save(); } },
        field('Display name', name, { locked: pol.isLocked('branding.display_name'), hint: 'Shown in the sidebar, the browser tab and the sign-in page.' }),
        field('Accent color', h('div', { class: 'btn-row' }, color, picker), { locked: pol.isLocked('branding.accent_color'),
          hint: 'A hex color. It must be dark enough to read against white (3:1); the server checks.' }),
        field('Sign-in banner', banner, { locked: pol.isLocked('branding.login_banner'), hint: 'Plain text on the sign-in page, such as an acceptable-use notice.' }),
        h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Save branding'))),
        footer: 'Branding is public: the sign-in page shows it to anyone who opens this server.' }),
      card({ title: 'Logo', body: h('div', { class: 'stack' },
        h('div', { class: 'btn-row' }, preview, logoLocked ? lockedNote() : h('label', { class: 'btn' }, 'Upload…', file),
          pol.branding && pol.branding.has_logo && !logoLocked ? h('button', { class: 'btn danger', type: 'button', onclick: removeLogo }, 'Remove') : null),
        h('p', { class: 'hint' }, 'PNG, JPEG or GIF, at most 64 KiB and 1024 pixels a side. SVG is not accepted: it can carry script.')) }));
  },

  async authentication(ctx, refresh) {
    const pol = await loadPolicy(ctx);
    const p = pol.policy;
    const providers = pol.providers || [];
    const providersCard = card({ title: 'Single sign-on providers', flush: true, body: providers.length ? table({
      caption: 'Configured identity providers',
      columns: [
        { key: 'label', label: 'Provider', render: x => h('span', {}, h('strong', {}, x.label), h('span', { class: 'muted' }, ' · ' + x.name + ' · ' + x.type)) },
        { key: 'restricted', label: 'Admits', sortable: false, render: x => x.restricted ? [...(x.domains || []).map(d => '@' + d), ...(x.orgs || [])].join(', ') : 'anyone the provider signs in' },
        { key: 'trust', label: 'Entra trust', sortable: false, render: x => (x.trusted_email_domains || []).length ? x.trusted_email_domains.join(', ') : h('span', { class: 'muted' }, '—') },
        { key: 'redirect_uri', label: 'Redirect URI', sortable: false, render: x => h('code', {}, x.redirect_uri) },
      ], rows: providers }) : empty('No single sign-on provider is configured on this server. Add one to the config file (sso.providers) or pass --sso-provider.'),
    footer: 'Providers are server configuration. Register the redirect URI with each provider exactly as shown.' });

    const requireCard = card({ title: 'Require single sign-on', body: h('div', { class: 'stack' },
      toggle('People sign in only through single sign-on', p.require_sso, { locked: pol.isLocked('require_sso'),
        hint: 'A person\'s token is accepted only if single sign-on minted it (local sign-in on the server\'s own machine follows its security mode). Runners and other service accounts keep their tokens. Tokens from before are refused, not revoked: turning this off restores them. Turn it on while signed in through single sign-on.',
        onChange: v => patchPolicy(ctx, { require_sso: v }, refresh) })) });

    const domains = h('input', { type: 'text', value: list(p.allowed_domains), placeholder: 'acme.com, acme.co.uk' });
    const orgs = h('input', { type: 'text', value: list(p.allowed_github_orgs), placeholder: 'acme' });
    const project = h('input', { type: 'text', value: p.default_project || '', placeholder: ctx.project });
    const role = h('select', {}, ['contributor', 'reviewer', 'observer'].map(r => h('option', { value: r, selected: p.default_role === r }, r)));
    const autoProv = h('input', { type: 'checkbox', checked: p.auto_provision, disabled: pol.isLocked('auto_provision') });
    const admission = card({ title: 'Who may sign in', body: h('form', { class: 'form', onsubmit: ev => {
      ev.preventDefault();
      const patch = {};
      if (!pol.isLocked('allowed_domains')) patch.allowed_domains = splitList(domains.value);
      if (!pol.isLocked('allowed_github_orgs')) patch.allowed_github_orgs = splitList(orgs.value);
      if (!pol.isLocked('default_project')) patch.default_project = project.value.trim();
      if (!pol.isLocked('default_role')) patch.default_role = role.value;
      if (!pol.isLocked('auto_provision')) patch.auto_provision = autoProv.checked;
      patchPolicy(ctx, patch, refresh);
    } },
      h('div', { class: 'field-row' },
        field('Allowed email domains', domains, { locked: pol.isLocked('allowed_domains'), hint: 'Empty admits any domain the provider verifies.' }),
        field('Allowed GitHub organizations', orgs, { locked: pol.isLocked('allowed_github_orgs') })),
      h('div', { class: 'toggle-row' }, h('label', { class: 'toggle' }, autoProv, h('span', {}, 'Create an account for a first sign-in from an allowed domain or organization')),
        pol.isLocked('auto_provision') ? lockedNote() : null),
      h('div', { class: 'field-row' },
        field('New accounts join', project, { locked: pol.isLocked('default_project'), hint: 'A project slug. SCIM-created accounts join it too.' }),
        field('With the role', role, { locked: pol.isLocked('default_role'), hint: 'Contributor at most: more is granted by a person.' })),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Save'))) });

    const rules = (p.group_rules || []).map(r => ({ ...r }));
    const max = h('select', {}, ['observer', 'reviewer', 'contributor', 'maintainer', 'project_admin'].map(r => h('option', { value: r, selected: p.max_group_role === r }, r)));
    const rulesBody = h('div');
    const drawRules = () => replace(rulesBody, rules.length ? table({ caption: 'Group to role rules', columns: [
      { key: 'group', label: 'Group', render: r => h('code', {}, r.group) },
      { key: 'project', label: 'Project', render: r => r.project || h('span', { class: 'muted' }, 'default') },
      { key: 'role', label: 'Role' },
      { key: 'x', label: '', sortable: false, render: r => pol.isLocked('group_rules') ? '' : h('button', { class: 'btn sm', type: 'button', 'aria-label': 'Remove the rule for ' + r.group,
        onclick: () => { rules.splice(rules.indexOf(r), 1); drawRules(); } }, 'Remove') },
    ], rows: rules }) : empty('No rules. Roles are granted only by people.'));
    drawRules();
    const g = h('input', { type: 'text', placeholder: 'engineering or acme/platform', 'aria-label': 'Group' });
    const gp = h('input', { type: 'text', placeholder: 'project slug (default if empty)', 'aria-label': 'Project' });
    const gr = h('select', { 'aria-label': 'Role' }, ['observer', 'reviewer', 'contributor', 'maintainer', 'project_admin'].map(r => h('option', { value: r, selected: r === 'contributor' }, r)));
    const mapping = card({ title: 'Group → role mapping', body: h('div', { class: 'stack' },
      h('p', { class: 'hint' }, 'Applied at every sign-in, from the token\'s groups claim, GitHub teams (org/team) and SCIM groups. Mapping adds members and moves roles up or down to the ceiling; it never touches a role above the ceiling, never demotes a project\'s last administrator, and never removes anyone.'),
      field('Highest role a group can grant', max, { locked: pol.isLocked('max_group_role'), hint: 'org_admin is never granted by a group.' }),
      rulesBody,
      pol.isLocked('group_rules') ? lockedNote() : h('div', { class: 'field-row' }, g, gp, gr,
        h('button', { class: 'btn', type: 'button', onclick: () => { if (!g.value.trim()) return g.focus(); rules.push({ group: g.value.trim(), project: gp.value.trim(), role: gr.value }); g.value = ''; drawRules(); } }, icon('plus'), 'Add rule')),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'button', onclick: () => {
        const patch = {};
        if (!pol.isLocked('max_group_role')) patch.max_group_role = max.value;
        if (!pol.isLocked('group_rules')) patch.group_rules = rules;
        patchPolicy(ctx, patch, refresh);
      } }, 'Save mapping'))) });

    const human = h('input', { type: 'text', value: p.human_token_max_ttl || '', placeholder: '90d' });
    const service = h('input', { type: 'text', value: p.service_token_max_ttl || '', placeholder: 'no limit' });
    const session = h('input', { type: 'text', value: p.sso_session_ttl || '', placeholder: (pol.server_sso && pol.server_sso.token_ttl) || '12h' });
    const ttls = card({ title: 'Token lifetimes', body: h('form', { class: 'form', onsubmit: ev => {
      ev.preventDefault();
      const patch = {};
      if (!pol.isLocked('human_token_max_ttl')) patch.human_token_max_ttl = human.value.trim() || '0';
      if (!pol.isLocked('service_token_max_ttl')) patch.service_token_max_ttl = service.value.trim() || '0';
      if (!pol.isLocked('sso_session_ttl')) patch.sso_session_ttl = session.value.trim() || '0';
      patchPolicy(ctx, patch, refresh);
    } },
      h('div', { class: 'field-row' },
        field('Longest a person\'s token lives', human, { locked: pol.isLocked('human_token_max_ttl'), hint: 'Up to 90d. Durations such as 12h, 7d.' }),
        field('Longest a service token lives', service, { locked: pol.isLocked('service_token_max_ttl'), hint: 'Runners and bots. Empty: no limit.' }),
        field('Single sign-on session', session, { locked: pol.isLocked('sso_session_ttl'), hint: 'How long a sign-in lasts before signing in again.' })),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Save lifetimes'))) });

    const problems = (pol.problems || []).length ? h('div', { class: 'notice warn', role: 'alert' }, 'The policy in force has problems: ', pol.problems.join('; ')) : null;
    return h('div', { class: 'stack', style: { gap: '16px' } }, problems, providersCard, requireCard, admission, mapping, ttls);
  },

  async provisioning(ctx, refresh) {
    const out = await ctx.api.get('/v1/admin/scim/tokens');
    const tokens = out.tokens || [];
    const mint = () => {
      const name = h('input', { type: 'text', value: 'okta', maxlength: 64 });
      openModal({ title: 'New SCIM token', body: h('div', { class: 'form' }, field('Name', name, { hint: 'Which identity provider will use it.' })),
        actions: [{ label: 'Cancel' }, { label: 'Create', kind: 'primary', onClick: async close => {
          try {
            const t = await ctx.api.post('/v1/admin/scim/tokens', { name: name.value.trim() || 'scim' });
            close();
            openModal({ title: 'SCIM token for ' + t.name, body: h('div', { class: 'stack' },
              h('p', {}, 'Give your identity provider these two values. The token is shown once and stored only as a hash.'),
              h('div', { class: 'kv-label' }, 'Base URL'), snippet(t.base_url),
              h('div', { class: 'kv-label' }, 'Bearer token'), snippet(t.token)), actions: [{ label: 'Done' }] });
            refresh();
          } catch (err) { toastError(err, 'Not created'); return false; }
        } }] });
    };
    const revoke = async t => {
      if (!await confirmModal({ title: `Revoke ${t.name}?`, message: 'The identity provider using it stops provisioning at once.', confirmLabel: 'Revoke', kind: 'danger' })) return;
      try { await ctx.api.del('/v1/admin/scim/tokens/' + encodeURIComponent(t.id)); toast('Revoked'); refresh(); } catch (err) { toastError(err, 'Not revoked'); }
    };
    return h('div', { class: 'stack', style: { gap: '16px' } },
      card({ title: 'SCIM provisioning', body: h('div', { class: 'stack' },
        h('p', {}, 'Your identity provider (Okta, Microsoft Entra ID) creates, updates, deactivates and deletes people here, and pushes groups for role mapping. Deactivating someone ends every session they have at once.'),
        kv([['SCIM base URL', h('code', {}, out.base_url)], ['Authentication', 'HTTP header, Bearer token']])) }),
      card({ title: 'Tokens', flush: true, actions: h('button', { class: 'btn sm primary', onclick: mint }, icon('plus'), 'New token'),
        body: tokens.length ? table({ caption: 'SCIM tokens', columns: [
          { key: 'name', label: 'Name' },
          { key: 'created_by', label: 'Created by' },
          { key: 'created_at', label: 'Created', render: t => fmtDate(t.created_at), sort: t => new Date(t.created_at) },
          { key: 'last_used_at', label: 'Last used', render: t => t.last_used_at ? relTime(t.last_used_at) : 'never' },
          { key: 'state', label: 'State', render: t => t.revoked_at ? pill('danger', 'revoked') : pill('ok', 'active') },
          { key: 'x', label: '', sortable: false, render: t => t.revoked_at ? '' : h('button', { class: 'btn sm danger', onclick: () => revoke(t) }, 'Revoke') },
        ], rows: tokens }) : empty('No SCIM token yet. Create one, then paste it and the base URL into your identity provider.') }));
  },

  async members(ctx, refresh) {
    const out = await ctx.api.get('/v1/admin/members');
    const members = out.members || [];
    const setRole = async (m, ms, role) => {
      try {
        await ctx.api.patch(ctx.api.project(ms.project, '/members/' + encodeURIComponent(m.handle)), { role });
        toast(`${m.handle} is now ${role.replace(/_/g, ' ')} in ${ms.project}`);
        refresh();
      } catch (err) { toastError(err, 'Role not changed'); refresh(); }
    };
    const unlink = async (m, id) => {
      const ms = (m.memberships || [])[0];
      if (!ms) return toast('Not a member of any project', { kind: 'warn' });
      if (!await confirmModal({ title: `Unlink ${id.provider} from ${m.handle}?`, message: 'They can no longer sign in with that account, and sessions it opened end now.', confirmLabel: 'Unlink', kind: 'danger' })) return;
      try { await ctx.api.del(ctx.api.project(ms.project, `/members/${encodeURIComponent(m.handle)}/identities/${encodeURIComponent(id.provider)}`)); toast('Unlinked'); refresh(); }
      catch (err) { toastError(err, 'Not unlinked'); }
    };
    const setActive = async (m, active) => {
      if (!active && !await confirmModal({ title: `Deactivate ${m.handle}?`, message: 'Every token they hold is revoked and they cannot sign in until reactivated. Their history and memberships are kept.', confirmLabel: 'Deactivate', kind: 'danger' })) return;
      try { await ctx.api.post(`/v1/admin/members/${encodeURIComponent(m.handle)}/${active ? 'reactivate' : 'deactivate'}`); toast(active ? 'Reactivated' : 'Deactivated'); refresh(); }
      catch (err) { toastError(err, 'Not changed'); }
    };
    const roleSelect = (m, ms) => h('select', { 'aria-label': `${m.handle}'s role in ${ms.project}`, disabled: !!m.deactivated_at,
      onchange: ev => setRole(m, ms, ev.target.value) },
      [...(ms.role === 'runner' ? ['runner'] : ROLES)].map(r => h('option', { value: r, selected: r === ms.role }, r.replace(/_/g, ' '))));
    return card({ title: 'Everyone in the organization', flush: true,
      body: members.length ? table({ caption: 'Organization members', columns: [
        { key: 'handle', label: 'Person', render: m => h('div', {}, h('strong', {}, m.handle), m.handle === ctx.handle ? h('span', { class: 'muted' }, ' (you)') : null,
          h('div', { class: 'muted small' }, [m.display_name !== m.handle ? m.display_name : '', m.email].filter(Boolean).join(' · ')),
          m.kind !== 'human' ? chip(m.kind.replace(/_/g, ' '), { mono: false }) : null,
          m.scim_managed ? chip('SCIM', { mono: false, kind: 'info' }) : null) },
        { key: 'memberships', label: 'Projects and roles', sortable: false, render: m => (m.memberships || []).length
          ? h('div', { class: 'stack tight' }, m.memberships.map(ms => h('div', { class: 'btn-row' }, h('span', { class: 'mono' }, ms.project), roleSelect(m, ms))))
          : h('span', { class: 'muted' }, 'no project') },
        { key: 'identities', label: 'Signs in with', sortable: false, render: m => (m.identities || []).length
          ? h('div', { class: 'chips' }, m.identities.map(id => h('span', { class: 'chip sans' }, id.provider + (id.email ? ' · ' + id.email : ''),
            h('button', { class: 'x', type: 'button', 'aria-label': `Unlink ${id.provider} from ${m.handle}`, onclick: () => unlink(m, id) }, icon('close', 11)))))
          : h('span', { class: 'muted' }, 'tokens only') },
        { key: 'last_seen_at', label: 'Last seen', render: m => m.last_seen_at ? relTime(m.last_seen_at) : 'never', sort: m => new Date(m.last_seen_at || 0) },
        { key: 'state', label: 'State', render: m => m.deactivated_at ? pill('danger', 'deactivated') : pill('ok', 'active'), sort: m => (m.deactivated_at ? 1 : 0) },
        { key: 'x', label: '', sortable: false, render: m => m.handle === ctx.handle ? '' : m.deactivated_at
          ? h('button', { class: 'btn sm', onclick: () => setActive(m, true) }, 'Reactivate')
          : h('button', { class: 'btn sm danger', onclick: () => setActive(m, false) }, 'Deactivate') },
      ], rows: members }) : empty('Nobody here yet.'),
      footer: 'Role changes follow the project rules: nobody grants a role above their own, and a project\'s last administrator cannot be demoted.' });
  },

  async features(ctx, refresh) {
    const pol = await loadPolicy(ctx);
    return card({ title: 'Advanced areas', body: h('div', { class: 'stack' },
      h('p', { class: 'hint' }, 'A new organization starts with the essentials: checking before an edit, tasks, people, conflicts. Turn on what your team uses. Turning an area off hides it from the menu; it changes no one\'s access, and its address still works.'),
      h('ul', { class: 'features' }, (pol.features || []).map(f => h('li', {},
        toggle(f.label, f.enabled, { locked: f.locked, hint: f.description, onChange: v => patchPolicy(ctx, { features: { [f.key]: v } }, refresh) }))))) });
  },

  async audit(ctx) {
    const state = { actor: '', action: '', since: '', until: '' };
    const actor = h('input', { type: 'search', placeholder: 'handle', 'aria-label': 'Actor' });
    const action = h('input', { type: 'text', placeholder: 'sso.  or  member.role_changed', 'aria-label': 'Action', list: 'audit-actions' });
    const since = h('input', { type: 'date', 'aria-label': 'From' });
    const until = h('input', { type: 'date', 'aria-label': 'Until' });
    const results = h('div');
    const datalist = h('datalist', { id: 'audit-actions' });
    let before = 0;
    let rows = [];
    const query = extra => ctx.api.query({ actor: state.actor, action: state.action, since: state.since, until: state.until, ...extra });
    const load = async (more = false) => {
      Object.assign(state, { actor: actor.value.trim(), action: action.value.trim(), since: since.value, until: until.value });
      if (!more) { before = 0; rows = []; }
      try {
        const out = await ctx.api.get('/v1/admin/audit' + query({ before: before || '', limit: 100 }));
        rows = rows.concat(out.entries || []);
        before = out.next_before || 0;
        datalist.replaceChildren(...(out.actions || []).map(a => h('option', { value: a })));
        draw();
      } catch (err) { toastError(err, 'Audit log'); }
    };
    const draw = () => replace(results, rows.length ? h('div', { class: 'stack' }, table({ caption: 'Audit records', columns: [
      { key: 'at', label: 'When', render: e => fmtDate(e.at), sort: e => new Date(e.at) },
      { key: 'actor', label: 'Actor', render: e => e.actor || h('span', { class: 'muted' }, e.detail && e.detail.kind === 'scim' ? 'identity provider (SCIM)' : 'system') },
      { key: 'action', label: 'Action', render: e => h('code', {}, e.action) },
      { key: 'target', label: 'Target', sortable: false, render: e => (e.detail && e.detail.principal) || e.target_id || '' },
      { key: 'project', label: 'Project' },
      { key: 'detail', label: 'Detail', sortable: false, render: e => h('span', { class: 'mono small' }, Object.entries(e.detail || {}).filter(([k]) => k !== 'principal').map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`).join('  ')) },
    ], rows, initialSort: { key: 'at', dir: 'desc' } }),
    before ? h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => load(true) }, 'Load older')) : null)
      : empty('No audit records match.'));
    const download = format => {
      // A download needs the bearer token, which a plain link cannot send.
      fetch('/v1/admin/audit' + query({ format }), { headers: { Authorization: 'Bearer ' + ctx.token } })
        .then(async r => {
          if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
          const blob = await r.blob();
          const a = h('a', { href: URL.createObjectURL(blob), download: `conductor-audit.${format}` });
          document.body.append(a); a.click(); a.remove();
          setTimeout(() => URL.revokeObjectURL(a.href), 1000);
        }).catch(err => toastError(err, 'Export failed'));
    };
    load();
    return card({ title: 'Audit log', body: h('div', { class: 'stack' },
      h('form', { class: 'toolbar', onsubmit: ev => { ev.preventDefault(); load(); } },
        field('Actor', actor), field('Action', action), field('From', since), field('Until', until), datalist,
        h('button', { class: 'btn primary', type: 'submit' }, 'Filter'), h('div', { class: 'spacer' }),
        h('button', { class: 'btn', type: 'button', onclick: () => download('csv') }, 'Export CSV'),
        h('button', { class: 'btn', type: 'button', onclick: () => download('jsonl') }, 'Export JSON lines')),
      results), footer: 'An action ending in "." matches a family (sso. is every sign-in event). Exports include every matching record and are themselves audited.' });
  },

  async configuration(ctx) {
    const [cfg, pol] = await settle([ctx.api.get('/v1/admin/config'), loadPolicy(ctx)]);
    if (!cfg) return card({ title: 'Server configuration', body: empty('The server\'s configuration is shown to the administrators of the organization that runs it.') });
    const settings = (cfg.settings || []).filter(e => e.source !== 'default' || e.value);
    return h('div', { class: 'stack', style: { gap: '16px' } },
      card({ title: 'Config file', body: cfg.config_file ? kv([['File', h('code', {}, cfg.config_file)],
        ['Locks', (cfg.locked || []).length ? h('div', { class: 'chips' }, cfg.locked.map(k => chip(k))) : 'nothing']])
        : h('p', {}, 'This server runs without a config file. Settings come from flags and the environment. ', h('code', {}, 'conductord --config conductor.yaml'), ' adds one; ', h('code', {}, 'conductord config check'), ' validates it.') }),
      card({ title: 'Effective settings', flush: true, body: table({ caption: 'Effective server settings', columns: [
        { key: 'key', label: 'Setting', render: e => h('code', {}, e.key) },
        { key: 'value', label: 'Value', render: e => e.secret ? h('span', { class: 'muted' }, e.value || 'unset', ' (secret)') : h('span', { class: 'mono' }, e.value) },
        { key: 'source', label: 'From', render: e => chip({ flag: 'command line', env: 'environment', file: 'config file', default: 'default' }[e.source] || e.source, { mono: false, kind: e.source === 'file' ? 'info' : '' }) },
      ], rows: settings }), footer: 'A command-line flag wins over its environment variable, which wins over the config file. Secrets are never shown.' }),
      pol && pol.policy ? card({ title: 'Organization policy in force', body: h('pre', { class: 'json' }, JSON.stringify(pol.policy, null, 2)) }) : null);
  },
};
