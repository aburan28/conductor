import { h, icon } from '../lib/dom.js';
import { createApi } from '../lib/api.js';

// The connect screen: paste a token, pick a project. The token is verified with /v1/whoami
// before anything else loads, so a stale credential fails here rather than as a wall of 401s.
export function renderConnect(root, { onConnect, error }) {
  const token = h('input', { type: 'password', placeholder: 'cdt_…', autocomplete: 'off', spellcheck: false, 'aria-label': 'token' });
  const projectSel = h('select', { disabled: true }, h('option', { value: '' }, 'verify the token first'));
  const status = h('div', { class: 'hint', style: { minHeight: '18px' } }, error ? h('span', { class: 'risk-high' }, error) : '');
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

  root.replaceChildren(h('div', { class: 'connect' }, h('div', { class: 'card' }, h('div', { class: 'body' },
    h('div', { class: 'brand' }, h('div', { class: 'logo' }, icon('bolt', 15)), h('span', { class: 'name' }, 'Conductor')),
    h('p', { class: 'muted' }, 'Coordinate humans and coding agents on one repository. Sign in with a token from ', h('code', {}, 'conductord bootstrap'), ' or ', h('code', {}, 'conductor member add'), '.'),
    h('form', { class: 'form', onsubmit: ev => { ev.preventDefault(); projectSel.disabled ? verify() : connect(); } },
      h('label', { class: 'field' }, 'Token', token),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', type: 'button', onclick: verify }, 'Verify')),
      h('label', { class: 'field' }, 'Project', projectSel),
      status,
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Open dashboard'), h('a', { class: 'btn ghost', href: '/?demo=1' }, 'Try the demo'))),
    h('p', { class: 'hint', style: { marginTop: '12px' } }, 'Or open the link printed by:'),
    h('pre', {}, 'conductor dashboard'),
    h('p', { class: 'hint' }, 'The token is kept in this browser only and sent to this origin only.')))));
  token.focus();
}
