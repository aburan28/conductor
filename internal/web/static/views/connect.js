import { h, icon } from '../lib/dom.js';
import { createApi } from '../lib/api.js';

// The connect screen: sign in with a username and password, or paste a token. Both paths
// end the same way — a verified token and a project choice — so the rest of the dashboard
// never needs to know which one was used. Credentials are verified before anything else
// loads, so a stale password fails here rather than as a wall of 401s.
export function renderConnect(root, { onConnect, error }) {
  const username = h('input', { type: 'text', placeholder: 'alice', autocomplete: 'username', spellcheck: false, 'aria-label': 'username' });
  const password = h('input', { type: 'password', placeholder: '••••••••', autocomplete: 'current-password', 'aria-label': 'password' });
  const token = h('input', { type: 'password', placeholder: 'cdt_…', autocomplete: 'off', spellcheck: false, 'aria-label': 'token' });
  const projectSel = h('select', { disabled: true }, h('option', { value: '' }, 'sign in first'));
  const status = h('div', { class: 'hint', style: { minHeight: '18px' } }, error ? h('span', { class: 'risk-high' }, error) : '');
  let projects = [];
  let handle = '';
  let pendingToken = '';

  // accept accepted credentials and finish the flow: fill the project picker, auto-connect
  // when there is exactly one choice.
  function accept(t, who) {
    pendingToken = t;
    projects = who.projects || [];
    handle = who.principal ? who.principal.handle : '';
    projectSel.replaceChildren(...(projects.length ? projects.map(p => h('option', { value: p.slug }, `${p.slug} · ${p.role}`)) : [h('option', { value: '' }, `signed in as ${handle}, but no projects are attached`)]));
    projectSel.disabled = !projects.length;
    status.textContent = projects.length ? `Signed in as ${handle}. Choose a project.` : `Signed in as ${handle}, but this credential belongs to no project.`;
    if (projects.length === 1) connect();
    else projectSel.focus();
  }

  async function signIn() {
    const u = username.value.trim();
    if (!u || !password.value) { (u ? password : username).focus(); return; }
    status.textContent = 'Signing in…';
    try {
      const who = await createApi().post('/v1/login', { username: u, password: password.value });
      accept(who.token, who);
    } catch (err) {
      status.replaceChildren(h('span', { class: 'risk-high' }, err.status === 401 ? 'That username and password were not accepted.' : err.message));
    }
  }

  async function verify() {
    const t = token.value.trim();
    if (!t) { token.focus(); return; }
    status.textContent = 'Verifying…';
    try {
      const who = await createApi({ token: t }).get('/v1/whoami');
      accept(t, who);
    } catch (err) {
      status.replaceChildren(h('span', { class: 'risk-high' }, err.status === 401 ? 'That token was not accepted.' : err.message));
    }
  }
  function connect() {
    const slug = projectSel.value;
    if (!slug || !pendingToken) return;
    onConnect({ token: pendingToken, project: slug, handle, projects });
  }
  // Submit before any credential is accepted: whichever field is filled decides the path.
  function signInOrVerify() {
    if (password.value) signIn();
    else if (token.value.trim()) verify();
    else signIn();
  }
  username.addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); signIn(); } });
  password.addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); signIn(); } });
  token.addEventListener('keydown', ev => { if (ev.key === 'Enter') { ev.preventDefault(); verify(); } });

  root.replaceChildren(h('div', { class: 'connect' }, h('div', { class: 'card' }, h('div', { class: 'body' },
    h('div', { class: 'brand' }, h('div', { class: 'logo' }, icon('bolt', 15)), h('span', { class: 'name' }, 'Conductor')),
    h('p', { class: 'muted' }, 'Coordinate humans and coding agents on one repository.'),
    h('form', { class: 'form', onsubmit: ev => { ev.preventDefault(); projectSel.disabled ? signInOrVerify() : connect(); } },
      h('label', { class: 'field' }, 'Username', username),
      h('label', { class: 'field' }, 'Password', password),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'button', onclick: signIn }, 'Sign in')),
      h('p', { class: 'hint', style: { margin: '14px 0 0' } }, 'Or paste a token from ', h('code', {}, 'conductord bootstrap'), ' or ', h('code', {}, 'conductor member add'), ':'),
      h('label', { class: 'field' }, 'Token', token),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', type: 'button', onclick: verify }, 'Verify')),
      h('label', { class: 'field' }, 'Project', projectSel),
      status,
      h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Open dashboard'), h('a', { class: 'btn ghost', href: '/?demo=1' }, 'Try the demo'))),
    h('p', { class: 'hint', style: { marginTop: '12px' } }, 'No password yet? Sign in once with a token, then set one under Settings.'),
    h('pre', {}, 'conductor dashboard'),
    h('p', { class: 'hint' }, 'Credentials are kept in this browser only and sent to this origin only.')))));
  username.focus();
}
