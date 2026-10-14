import { h, icon } from '../lib/dom.js';
import { createApi } from '../lib/api.js';
import { applyBranding } from '../lib/branding.js';

// The connect screen: paste a token, pick a project. The token is verified with /v1/whoami
// before anything else loads, so a stale credential fails here rather than as a wall of 401s.
export function renderConnect(root, { onConnect, error, note, token: initialToken }) {
  const token = h('input', { type: 'password', placeholder: 'cdt_…', autocomplete: 'off', spellcheck: false });
  const projectSel = h('select', { disabled: true }, h('option', { value: '' }, 'verify the token first'));
  const status = h('div', { class: 'hint', role: 'status', 'aria-live': 'polite', style: { minHeight: '18px' } }, error ? h('span', { class: 'risk-high' }, error) : '');
  let projects = [];
  let handle = '';

  async function verify() {
    const t = token.value.trim();
    if (!t) { token.focus(); return; }
    status.textContent = 'Verifying…';
    try {
      const who = await createApi({ token: t }).get('/v1/whoami');
      projects = who.projects || [];
      handle = who.principal ? who.principal.handle : '';
      projectSel.replaceChildren(...(projects.length ? projects.map(p => h('option', { value: p.slug }, `${p.slug} · ${p.role}`)) : [h('option', { value: '' }, 'no projects for this token')]));
      projectSel.disabled = !projects.length;
      status.textContent = projects.length ? `Signed in as ${handle}. Choose a project.` : `Signed in as ${handle}, but this token belongs to no project.`;
      if (projects.length === 1) connect();
      else projectSel.focus();
    } catch (err) {
      status.replaceChildren(h('span', { class: 'risk-high' }, err.status === 401 ? 'That token was not accepted.' : err.message));
    }
  }
  function connect() {
    const slug = projectSel.value;
    if (!slug) return;
    onConnect({ token: token.value.trim(), project: slug, handle, projects });
  }
  token.addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); verify(); } });

  // Single sign-on, when the server has providers configured. Each button is a plain link:
  // the server redirects to the provider and back, and the dashboard reloads with a one-time
  // ticket it redeems for a token. `next` brings the person back to the page they asked for.
  const sso = h('div', { class: 'sso' });
  createApi({}).get('/v1/sso/providers').then(out => {
    const providers = (out && out.providers) || [];
    if (!providers.length) return;
    const next = encodeURIComponent(location.pathname + location.search);
    sso.replaceChildren(
      h('div', { class: 'btn-row' }, ...providers.map(p => h('a', {
        class: 'btn primary', href: `/v1/sso/${encodeURIComponent(p.name)}/start?next=${next}`,
      }, `Sign in with ${p.label || p.name}`))),
      h('p', { class: 'hint' }, 'Or paste a token:'));
  }).catch(() => {});

  // The organization's branding and sign-in banner, before anyone has signed in. The banner
  // is plain text: it is inserted as a text node, never as markup.
  const brandName = h('span', { class: 'name' }, 'Conductor');
  const brandLogo = h('div', { class: 'logo', 'aria-hidden': 'true' }, icon('bolt', 15));
  const banner = h('div', { class: 'login-banner', hidden: true, role: 'note' });
  createApi({}).get('/v1/branding').then(b => {
    if (!b) return;
    applyBranding(b);
    if (b.display_name) brandName.textContent = b.display_name;
    if (b.login_banner) { banner.textContent = b.login_banner; banner.hidden = false; }
    if (b.has_logo) {
      brandLogo.classList.add('image');
      brandLogo.replaceChildren(h('img', { src: '/v1/branding/logo?v=' + encodeURIComponent(b.logo_sha256 || ''), alt: '' }));
    }
  }).catch(() => {});

  root.replaceChildren(h('main', { class: 'connect' }, h('div', { class: 'card' }, h('div', { class: 'body' },
    h('h1', { class: 'brand' }, brandLogo, brandName),
    banner,
    h('p', { class: 'muted' }, 'Coordinate humans and coding agents on one repository. Sign in with a token from ', h('code', {}, 'conductord bootstrap'), ' or ', h('code', {}, 'conductor member add'), '.'),
    sso,
    h('form', { class: 'form', onsubmit: ev => { ev.preventDefault(); projectSel.disabled ? verify() : connect(); } },
      h('label', { class: 'field' }, 'Token', token),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', type: 'button', onclick: verify }, 'Verify')),
      h('label', { class: 'field' }, 'Project', projectSel),
      status,
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Open dashboard'), h('a', { class: 'btn ghost', href: '/?demo=1' }, 'Try the demo'))),
    h('p', { class: 'hint', style: { marginTop: '12px' } }, 'Or open the link printed by:'),
    h('pre', {}, 'conductor dashboard'),
    note ? h('p', { class: 'hint' }, note) : null,
    h('p', { class: 'hint' }, 'On the machine running conductord you are signed in automatically, unless it is in enhanced security mode. The token is kept in this browser only and sent to this origin only.')))));
  if (initialToken) { token.value = initialToken; verify(); }
  else token.focus();
}
