import { h, icon } from '../lib/dom.js';
import { defineView, settle } from '../lib/view.js';
import { card, empty, kv, segmented, snippet } from '../components/ui.js';
import { chip, pill } from '../components/pill.js';
import { table } from '../components/table.js';
import { openModal, confirmModal } from '../components/modal.js';
import { toast, toastError } from '../components/toast.js';
import { fmtDate, relTime } from '../lib/format.js';
import { prefs } from '../lib/store.js';

export default defineView({
  title: 'Settings',
  async load(ctx) {
    const [project, members, tokens, security] = await settle([
      ctx.api.get(ctx.api.project(ctx.project)),
      ctx.api.get(ctx.api.project(ctx.project, '/members')),
      ctx.api.get('/v1/tokens'),
      ctx.api.get('/v1/security'),
    ]);
    return { project, members: (members && members.members) || [], tokens: (tokens && tokens.tokens) || [], security };
  },
  draw({ project, members, tokens, security }, ctx, { refresh }) {
    const cfg = (project && project.config) || {};
    const canAdmin = ['maintainer', 'project_admin', 'org_admin'].includes(ctx.role);

    const connection = card({ title: 'Connection', body: h('div', { class: 'stack' },
      kv([['Endpoint', h('span', { class: 'mono' }, ctx.origin)], ['Signed in as', ctx.handle], ['Role', ctx.role || '—'], ['Project', h('span', { class: 'mono' }, ctx.project)], ['Project id', project ? h('span', { class: 'mono' }, project.id) : null]]),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn danger', onclick: () => ctx.signOut() }, 'Sign out'),
        h('a', { class: 'btn', href: '/?demo=1' }, 'Open demo mode'))) });

    const appearance = card({ title: 'Appearance', body: h('div', { class: 'stack' },
      h('div', { class: 'toolbar' }, h('span', {}, 'Theme'), segmented([{ value: 'system', label: 'System' }, { value: 'light', label: 'Light' }, { value: 'dark', label: 'Dark' }], ctx.store.get().theme, v => ctx.setTheme(v))),
      h('div', { class: 'toolbar' }, h('span', {}, 'Density'), segmented([{ value: 'comfortable', label: 'Comfortable' }, { value: 'compact', label: 'Compact' }], prefs.get('density', 'comfortable'), v => { prefs.set('density', v); document.documentElement.dataset.density = v; })),
      h('div', { class: 'hint' }, 'Press ? anywhere for keyboard shortcuts.')) });

    const invite = () => {
      const handle = h('input', { type: 'text', placeholder: 'rachel', required: true });
      const role = h('select', {}, ['contributor', 'maintainer', 'reviewer', 'observer', 'project_admin'].map(r => h('option', { value: r }, r)));
      openModal({ title: 'Add a member', body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'Handle', handle), h('label', { class: 'field' }, 'Role', role), h('div', { class: 'hint' }, 'A new account\'s token is shown once; send it over a private channel. An existing account is added without one.')),
        actions: [{ label: 'Cancel' }, { label: 'Add member', kind: 'primary', onClick: async close => {
          if (!handle.value.trim()) { handle.focus(); return false; }
          try {
            const out = await ctx.api.post(ctx.api.project(ctx.project, '/members'), { handle: handle.value.trim(), role: role.value });
            close();
            // An existing account is added without a token: it keeps its own login.
            if (!out.token) {
              openModal({ title: `Added ${out.handle || handle.value.trim()}`, body: h('div', { class: 'stack' }, h('div', { class: 'notice' }, out.note || 'Added to the project. No token was minted.'), snippet(`conductor login --project ${ctx.project}`)), actions: [{ label: 'Done' }] });
              refresh();
              return;
            }
            openModal({ title: `Token for ${handle.value.trim()}`, body: h('div', { class: 'stack' }, h('div', { class: 'token-reveal' }, out.token), snippet(`conductor login --endpoint ${ctx.origin} --token ${out.token} --project ${ctx.project}`), h('div', { class: 'notice warn' }, 'Shown once. It is stored only as a hash.')), actions: [{ label: 'Done' }] });
            refresh();
          } catch (err) { toastError(err, 'Could not add member'); return false; }
        } }] });
    };
    const remove = async m => {
      if (!await confirmModal({ title: `Remove ${m.handle}?`, message: 'Their tokens are revoked and their live sessions lose access.', confirmLabel: 'Remove', kind: 'danger' })) return;
      try { await ctx.api.del(ctx.api.project(ctx.project, '/members/' + encodeURIComponent(m.handle))); toast(`Removed ${m.handle}`); refresh(); } catch (err) { toastError(err, 'Could not remove'); }
    };
    const membersCard = card({ title: 'Members', flush: true, actions: canAdmin ? h('button', { class: 'btn sm primary', onclick: invite }, icon('plus'), 'Add member') : null,
      body: members.length ? table({ columns: [
        { key: 'handle', label: 'Handle', render: m => h('span', {}, h('strong', {}, m.handle), m.handle === ctx.handle ? h('span', { class: 'muted' }, ' (you)') : null) },
        { key: 'kind', label: 'Kind', render: m => chip(m.kind || 'human', { mono: false }) },
        { key: 'role', label: 'Role', render: m => pill('info', m.role) },
        { key: 'act', label: '', sortable: false, render: m => canAdmin && m.handle !== ctx.handle ? h('button', { class: 'btn sm danger', onclick: () => remove(m) }, 'Remove') : '' },
      ], rows: members }) : empty('No members listed.', 'conductor member add rachel --role contributor') });

    const mint = () => {
      const name = h('input', { type: 'text', placeholder: 'laptop', value: 'dashboard' });
      openModal({ title: 'Create a token', body: h('div', { class: 'form' }, h('label', { class: 'field' }, 'Name', name)),
        actions: [{ label: 'Cancel' }, { label: 'Create', kind: 'primary', onClick: async close => {
          try { const out = await ctx.api.post('/v1/tokens', { name: name.value.trim() || 'dashboard' }); close();
            openModal({ title: 'New token', body: h('div', { class: 'stack' }, h('div', { class: 'token-reveal' }, out.token), h('div', { class: 'notice warn' }, 'Shown once.')), actions: [{ label: 'Done' }] }); refresh(); }
          catch (err) { toastError(err, 'Could not create token'); return false; }
        } }] });
    };
    const revoke = async t => {
      if (!await confirmModal({ title: `Revoke ${t.name}?`, message: 'Anything using it stops working immediately — including this dashboard if it is the token you signed in with.', confirmLabel: 'Revoke', kind: 'danger' })) return;
      try { await ctx.api.del('/v1/tokens/' + encodeURIComponent(t.name)); toast('Revoked'); refresh(); } catch (err) { toastError(err, 'Could not revoke'); }
    };
    const tokensCard = card({ title: 'Your tokens', flush: true, actions: h('button', { class: 'btn sm', onclick: mint }, icon('plus'), 'New token'),
      body: tokens.length ? table({ columns: [
        { key: 'name', label: 'Name', mono: true },
        { key: 'created_at', label: 'Created', render: t => fmtDate(t.created_at), sort: t => new Date(t.created_at) },
        { key: 'last_used_at', label: 'Last used', render: t => t.last_used_at ? relTime(t.last_used_at) : '—', sort: t => new Date(t.last_used_at || 0) },
        { key: 'expires_at', label: 'Expires', render: t => t.expires_at ? fmtDate(t.expires_at) : 'never' },
        { key: 'revoked_at', label: 'State', render: t => t.revoked_at ? pill('danger', 'revoked') : pill('ok', 'active') },
        { key: 'act', label: '', sortable: false, render: t => t.revoked_at ? '' : h('button', { class: 'btn sm danger', onclick: () => revoke(t) }, 'Revoke') },
      ], rows: tokens }) : empty('No tokens listed.', 'conductor token create --save') });

    const setMode = async mode => {
      const tighten = mode === 'enhanced';
      if (!await confirmModal({
        title: tighten ? 'Require tokens everywhere?' : 'Allow local sign-in?',
        message: tighten
          ? 'Local sign-in turns off and every token it issued is revoked — including this dashboard\'s, if that is how you signed in. Use this on a machine other people can log in to.'
          : 'The machine\'s owner will be signed in automatically from this machine, without a token. Only do this on a machine nobody else uses.',
        confirmLabel: tighten ? 'Require tokens' : 'Allow local sign-in', kind: tighten ? 'primary' : 'danger' })) return;
      try { await ctx.api.post('/v1/security', { security_mode: mode }); toast(tighten ? 'Enhanced security on' : 'Local sign-in on'); refresh(); }
      catch (err) { toastError(err, 'Could not change the security mode'); }
    };
    const securityCard = security ? card({ title: 'Security', body: h('div', { class: 'stack' },
      kv([['Mode', pill(security.security_mode === 'enhanced' ? 'ok' : 'info', security.security_mode)],
        ['Set by', { flag: 'conductord --security-mode (pinned)', setting: 'saved choice', default: 'default for how conductord listens' }[security.mode_source] || security.mode_source],
        ['Machine owner', security.owner ? security.owner + (security.you_are_owner ? ' (you)' : '') : 'none']]),
      h('p', { class: 'hint' }, security.security_mode === 'enhanced'
        ? 'Every client needs a token, on this machine and everywhere else.'
        : 'The machine\'s owner is signed in automatically on this machine. Everyone else, everywhere else, still needs a token.'),
      security.mode_source === 'flag' ? null : h('div', { class: 'btn-row' }, security.security_mode === 'enhanced'
        ? (security.you_are_owner ? h('button', { class: 'btn', onclick: () => setMode('local') }, 'Allow local sign-in') : null)
        : h('button', { class: 'btn primary', onclick: () => setMode('enhanced') }, 'Require tokens everywhere'))) }) : null;

    const budget = cfg.budget || {};
    const policyCard = card({ title: 'Project policy — from .conductor/', body: project ? kv([
      ['Default branch', project.default_branch], ['Repository', project.repo_path ? h('span', { class: 'mono' }, project.repo_path) : null],
      ['Claim mode', cfg.claim_mode], ['Default visibility', cfg.default_visibility],
      ['Lease TTL', cfg.lease_ttl], ['Heartbeat', cfg.heartbeat_interval], ['Offline grace', cfg.offline_grace], ['Stall after', cfg.stalled_turn_timeout],
      ['Write conflicts', cfg.write_conflict_policy], ['Read/write', cfg.read_write_conflict_policy], ['Duplicate threshold', cfg.duplicate_threshold != null ? String(cfg.duplicate_threshold) : null],
      ['Max concurrent attempts', String(cfg.max_concurrent_attempts ?? '')], ['Max per principal', String(cfg.max_per_principal ?? '')], ['Max attempts', String(cfg.max_attempts ?? '')],
      ['Max active sessions', cfg.max_active_sessions != null ? String(cfg.max_active_sessions) : 'unlimited'],
      ['Monthly budget', budget.monthly_usd ? `$${budget.monthly_usd} · downshift ${budget.downshift_at} · pause ${budget.pause_at}` : 'none'],
      ['Member tokens', budget.member_tokens ? String(budget.member_tokens) : 'off'],
      ['Required checks', cfg.required_checks && cfg.required_checks.length ? h('div', { class: 'chips' }, cfg.required_checks.map(c => chip(c))) : 'none'],
      ['Protected scopes', cfg.protected_scopes && cfg.protected_scopes.length ? h('div', { class: 'chips' }, cfg.protected_scopes.map(c => chip(c))) : 'none'],
      ['Publish model identity', String(!!cfg.publish_model_identity)], ['Publish harness identity', String(!!cfg.publish_harness_identity)],
      ['Workflow SHA', project.workflow_sha ? h('span', { class: 'mono' }, project.workflow_sha.slice(0, 12)) : null], ['Config SHA', project.config_sha ? h('span', { class: 'mono' }, project.config_sha.slice(0, 12)) : null],
    ]) : empty('Project details unavailable.'), footer: 'Policy lives in the repository and is re-read by `conductord bootstrap`. Every attempt records the hashes that were in force.' });

    const privacy = card({ title: 'Privacy', body: h('div', { class: 'stack' },
      h('p', {}, 'Conductor shares intent and territory: who holds what, what shape of work they are doing, and where two efforts are about to collide. It never shares prompts, model output, or conversation.'),
      h('ul', { class: 'rationale', style: { color: 'var(--text)' } },
        h('li', {}, 'The schema has no column for a prompt; the event payload passes an allowlist; harness adapters drop assistant text at the parse boundary.'),
        h('li', {}, 'Private tasks expose owner, status, and reserved scopes — enough to avoid a collision, nothing about why.'),
        h('li', {}, 'Duplicate detection compares HMAC\'d token sets under a per-tenant key; the server never sees either sentence.'),
        h('li', {}, 'This dashboard makes no external requests of any kind; a test enforces it.'))) });

    return h('div', { class: 'stack', style: { gap: '20px' } }, h('div', { class: 'grid-2' }, connection, appearance), securityCard, membersCard, tokensCard, policyCard, privacy);
  },
});
