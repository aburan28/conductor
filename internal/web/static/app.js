// Conductor dashboard — bootstrap, shell, routing, live updates. No framework, no build
// step, no external requests (TestNoExternalRequests enforces that last one).
import { h, icon, replace, clear, debounce } from './lib/dom.js';
import { createStore, prefs } from './lib/store.js';
import { createApi } from './lib/api.js';
import { createRouter } from './lib/router.js';
import { connectStream } from './lib/sse.js';
import { applyBranding } from './lib/branding.js';
import { toast } from './components/toast.js';
import { openPalette } from './components/palette.js';
import { openModal } from './components/modal.js';
import { openTaskForm } from './components/task-form.js';
import { renderConnect } from './views/connect.js';
import { openTaskDrawer } from './views/task-detail.js';
import home from './views/home.js';
import tasks from './views/tasks.js';
import people from './views/people.js';
import more from './views/more.js';
import fleet from './views/fleet.js';
import swarm from './views/swarm.js';
import queue from './views/queue.js';
import conflicts from './views/conflicts.js';
import usage from './views/usage.js';
import events from './views/events.js';
import integrations from './views/integrations.js';
import settings from './views/settings.js';
import adminView from './views/admin.js';

// The core: what is in flight, the work itself, who is here, and settings. Everything else
// lives under More, where a feature flag (Admin → Features) can hide an area from the menu;
// its address keeps working either way.
const CORE = [
  { path: '/', name: 'home', label: 'Home', icon: 'home', view: home, key: 'h' },
  { path: '/tasks', name: 'tasks', label: 'Tasks', icon: 'tasks', view: tasks, key: 't' },
  { path: '/people', name: 'people', label: 'People', icon: 'people', view: people, key: 'p' },
];
const MORE = [
  { path: '/conflicts', name: 'conflicts', label: 'Conflicts', icon: 'conflicts', view: conflicts, key: 'c', desc: 'Every open collision between two pieces of work, and how to settle it.' },
  { path: '/fleet', name: 'fleet', label: 'Fleet', icon: 'fleet', view: fleet, key: 'f', desc: 'Runners, the models live right now, and the model catalog.' },
  { path: '/swarm', name: 'swarm', label: 'Swarm', icon: 'swarm', view: swarm, key: 'w', feature: 'swarm', desc: 'Capacity teammates pool through their runners and sessions.' },
  { path: '/queue', name: 'queue', label: 'Queue', icon: 'queue', view: queue, key: 'q', feature: 'queue', desc: 'Sessions and attempts waiting for room under the project cap.' },
  { path: '/usage', name: 'usage', label: 'Usage', icon: 'usage', view: usage, key: 'u', desc: 'Tokens and spend by harness, model and person, and usage limits.' },
  { path: '/events', name: 'events', label: 'Events', icon: 'events', view: events, key: 'e', desc: 'The live timeline of everything that happened in this project.' },
  { path: '/integrations', name: 'integrations', label: 'Integrations', icon: 'integrations', view: integrations, key: 'n', desc: 'Connect Claude Code, Cursor, Codex and others to this project.' },
];
const SETTINGS = { path: '/settings', name: 'settings', label: 'Settings', icon: 'settings', view: settings, key: ',' };
const ADMIN = { path: '/admin', name: 'admin', label: 'Admin', icon: 'shield', view: adminView, key: 'a', adminOnly: true };
const MORE_PAGE = { path: '/more', name: 'more', label: 'More', icon: 'more', view: more, key: 'm' };
const ALL = [...CORE, ...MORE, MORE_PAGE, SETTINGS, ADMIN];
// Old addresses keep working.
const REDIRECTS = { '/sessions': '/people', '/overview': '/' };

const app = document.getElementById('app');
const params = new URLSearchParams(location.search);
// An invite link (from `conductor invite`) carries the token in the URL fragment, which the
// browser never sends to the server, so the credential stays out of every request line and
// access log. `conductor dashboard` historically used the query string; both are accepted,
// fragment first, and either is stripped from the address bar the moment it is read.
const rawHash = (location.hash || '').replace(/^#/, '');
// Links from before the dashboard used real paths (#/tasks) land on the same page.
if (rawHash.startsWith('/')) history.replaceState({}, '', rawHash);
const frag = new URLSearchParams(rawHash.startsWith('/') ? '' : rawHash);
const demoMode = params.get('demo') === '1' || frag.get('demo') === '1';
const urlToken = frag.get('token') || params.get('token');
const urlProject = frag.get('project') || params.get('project');
// A single sign-on returns here with a one-time ticket (or the reason it failed) in the
// fragment. The ticket is redeemed for a token by boot(); it is worthless without this
// browser's sign-in cookie, and is stripped from the address bar like a token.
const ssoTicket = frag.get('sso');
const ssoError = frag.get('sso_error');
if (urlToken) prefs.set('token', urlToken);
if (urlProject) prefs.set('project', urlProject);
if (urlToken || urlProject || ssoTicket || ssoError) {
  params.delete('token'); params.delete('project');
  const query = params.toString() ? '?' + params : '';
  history.replaceState({}, '', location.pathname + query);
}

const store = createStore({
  token: demoMode ? 'demo' : prefs.get('token', ''),
  project: demoMode ? 'demo' : prefs.get('project', ''),
  handle: prefs.get('handle', ''),
  role: prefs.get('role', ''),
  projects: prefs.get('projects', []),
  theme: prefs.get('theme', 'system'),
  connection: 'idle',
  demo: demoMode,
  org: null,
});

applyTheme(store.get().theme);
document.documentElement.dataset.density = prefs.get('density', 'comfortable');

let api = createApi({ token: store.get().token });
let stopStream = null;
let current = null;      // { name, key, instance, root }
let drawer = null;
let router = null;

boot();

async function boot() {
  const s = store.get();
  if (s.demo) {
    const demo = await import('./demo.js');
    demo.installDemo(api);
    store.set({ handle: 'you', role: 'maintainer', projects: [{ slug: 'demo', role: 'maintainer' }] });
    enter();
    return;
  }
  if (ssoTicket) {
    try {
      const out = await createApi({}).post('/v1/sso/redeem', { ticket: ssoTicket });
      return adoptToken(out.token);
    } catch (err) {
      return showConnect(err.message);
    }
  }
  if (ssoError) return showConnect(ssoError);
  if (!s.token) {
    // On the machine running conductord, in local security mode, the owner is signed in
    // without a token. Anywhere else the request is refused and the token form appears with
    // the server's reason.
    const local = await localSignIn();
    if (local.token) return adoptToken(local.token);
    if (!s.project) return showConnect(undefined, local.note);
  }
  if (!s.token || !s.project) return showConnect();
  try {
    const who = await api.get('/v1/whoami');
    const projects = who.projects || [];
    const mine = projects.find(p => p.slug === s.project || p.id === s.project);
    store.set({ handle: who.principal.handle, role: mine ? mine.role : '', projects });
    prefs.set('handle', who.principal.handle);
    prefs.set('projects', projects);
    if (mine) prefs.set('role', mine.role);
    enter();
  } catch (err) {
    if (err.status === 401) {
      // A locally issued token is revoked when the server switches to enhanced mode; if
      // local sign-in is still allowed, just sign in again. An organization that requires
      // single sign-on says so, and the sign-in screen offers its buttons.
      const local = await localSignIn();
      if (local.token) return adoptToken(local.token);
      showConnect(err.code === 'sso_required' ? err.message : 'Your saved token was not accepted. Sign in again.', local.note);
    } else showConnect(err.message);
  }
}

// localSignIn asks this server for a token for its owner. It returns { token } or { note },
// the server's explanation of why not, to show on the sign-in screen.
async function localSignIn() {
  try {
    const out = await createApi({}).post('/v1/local/session', { client: 'dashboard' });
    return { token: out.token };
  } catch (err) {
    return { note: err.status === 0 ? '' : err.message };
  }
}

// adoptToken verifies a token and either enters the project (when the choice is obvious) or
// hands it to the connect screen to pick one.
async function adoptToken(token) {
  prefs.set('token', token);
  api = createApi({ token });
  store.set({ token });
  const s = store.get();
  try {
    const who = await api.get('/v1/whoami');
    const projects = who.projects || [];
    const want = s.project || (projects.length === 1 ? projects[0].slug : '');
    const mine = projects.find(p => p.slug === want || p.id === want);
    if (!mine) return showConnect(undefined, undefined, token);
    prefs.set('project', mine.slug); prefs.set('handle', who.principal.handle); prefs.set('projects', projects); prefs.set('role', mine.role);
    store.set({ project: mine.slug, handle: who.principal.handle, role: mine.role, projects });
    enter();
  } catch (err) {
    showConnect(err.message);
  }
}

function showConnect(error, note, token) {
  renderConnect(app, {
    error, note, token,
    onConnect: ({ token, project, handle, projects }) => {
      prefs.set('token', token); prefs.set('project', project); prefs.set('handle', handle); prefs.set('projects', projects);
      const mine = (projects || []).find(p => p.slug === project);
      if (mine) prefs.set('role', mine.role);
      store.set({ token, project, handle, projects, role: mine ? mine.role : '' });
      api = createApi({ token });
      enter();
    },
  });
}

function signOut() {
  if (stopStream) stopStream();
  prefs.del('token'); prefs.del('project'); prefs.del('handle'); prefs.del('role');
  store.set({ token: '', project: '' });
  history.replaceState({}, '', '/');
  showConnect();
}

function ctx(extraParams = {}) {
  const s = store.get();
  return {
    api, store,
    project: s.project, handle: s.handle, role: s.role, token: s.token, org: s.org,
    origin: location.origin,
    params: extraParams,
    navigate: p => router.navigate(p),
    openTask: ref => router.navigate('/tasks/' + encodeURIComponent(ref)),
    refreshAll: () => current && current.instance && current.instance.refresh(),
    refreshOrg: loadOrg,
    more: MORE.map(n => ({ path: n.path, label: n.label, icon: n.icon, desc: n.desc, feature: n.feature, enabled: featureOn(n) })),
    isOrgAdmin: isOrgAdmin(),
    signOut,
    setTheme,
  };
}

// ---------------------------------------------------------------------------
// Organization: branding, feature flags, and whether this person administers it
// ---------------------------------------------------------------------------

const featureOn = item => !item.feature || !!(store.get().org && store.get().org.features && store.get().org.features[item.feature]);
const isOrgAdmin = () => !!(store.get().org && store.get().org.is_org_admin);

async function loadOrg() {
  let org = null;
  try { org = await api.get('/v1/org'); } catch (_) { org = null; }
  store.set({ org });
  applyBranding(org && org.branding);
  if (brandNameEl) renderBrand();
  if (navEl) renderNav();
  return org;
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

let contentEl, titleEl, connEl, dotEl, navEl, brandNameEl, brandLogoEl;

function renderBrand() {
  const b = (store.get().org && store.get().org.branding) || {};
  brandNameEl.textContent = b.display_name || 'Conductor';
  replace(brandLogoEl, icon('bolt', 15));
  brandLogoEl.classList.remove('image');
  if (b.has_logo && !store.get().demo) {
    // The logo needs this session's token, which an <img> cannot send; fetch it and show it
    // as a data: URL, which the page's Content-Security-Policy allows for images.
    fetch('/v1/org/logo', { headers: { Authorization: 'Bearer ' + store.get().token } })
      .then(r => (r.ok ? r.blob() : null)).then(blob => {
        if (!blob) return;
        const reader = new FileReader();
        reader.onload = () => { brandLogoEl.classList.add('image'); replace(brandLogoEl, h('img', { src: reader.result, alt: '' })); };
        reader.readAsDataURL(blob);
      }).catch(() => {});
  }
}

function navLink(n) {
  return h('a', { href: n.path, 'data-link': true, dataset: { name: n.name } }, icon(n.icon), h('span', { class: 'label' }, n.label));
}

function renderNav() {
  const moreItems = MORE.filter(featureOn);
  const moreOpen = prefs.get('nav_more_open', false);
  const moreList = h('div', { class: 'nav-more', id: 'nav-more', hidden: !moreOpen }, moreItems.map(navLink),
    h('a', { href: '/more', 'data-link': true, dataset: { name: 'more' }, class: 'all' }, h('span', { class: 'label' }, 'All areas…')));
  const toggle = h('button', { type: 'button', class: 'nav-toggle', 'aria-expanded': String(moreOpen), 'aria-controls': 'nav-more',
    onclick: () => {
      const open = moreList.hidden;
      moreList.hidden = !open;
      toggle.setAttribute('aria-expanded', String(open));
      prefs.set('nav_more_open', open);
    } }, icon('more'), h('span', { class: 'label' }, 'More'), icon('down', 14));
  replace(navEl,
    CORE.map(navLink),
    h('div', { class: 'nav-group' }, toggle, moreList),
    h('div', { class: 'nav-sep', role: 'presentation' }),
    navLink(SETTINGS),
    isOrgAdmin() ? navLink(ADMIN) : null);
  markActive();
}

function markActive() {
  if (!router) return;
  const base = baseName(router.current());
  document.querySelectorAll('.nav a').forEach(a => {
    const on = a.dataset.name === base;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
}

function baseName(route) {
  if (route.name === 'task-detail') return 'tasks';
  if (route.name === 'admin-section') return 'admin';
  return route.name;
}

function enter() {
  const s = store.get();
  clear(app);

  dotEl = h('span', { class: 'dot', 'aria-hidden': 'true' });
  connEl = h('span', {}, 'connecting…');
  const projectSel = h('select', { 'aria-label': 'Project', onchange: ev => switchProject(ev.target.value) },
    (s.projects && s.projects.length ? s.projects : [{ slug: s.project }]).map(p =>
      h('option', { value: p.slug, selected: p.slug === s.project }, p.slug)));

  brandNameEl = h('span', { class: 'name' }, 'Conductor');
  brandLogoEl = h('div', { class: 'logo', 'aria-hidden': 'true' }, icon('bolt', 15));
  navEl = h('div', { class: 'nav' });
  const sidebar = h('nav', { class: 'sidebar', 'aria-label': 'Main' },
    h('div', { class: 'brand' }, brandLogoEl, brandNameEl, s.demo ? h('span', { class: 'demo-badge' }, 'DEMO') : null),
    h('div', { class: 'project-switch' }, projectSel),
    navEl,
    h('div', { class: 'sidebar-foot' },
      h('div', { class: 'conn', role: 'status' }, dotEl, connEl),
      h('div', {}, s.handle || '—', s.role ? h('span', { class: 'muted' }, ' · ' + s.role.replace(/_/g, ' ')) : null)));

  titleEl = h('h1', { id: 'page-title' }, 'Home');
  const themeBtn = h('button', { class: 'btn ghost icon', title: 'Theme', 'aria-label': 'Change theme', onclick: () => {
    const order = ['system', 'light', 'dark'];
    setTheme(order[(order.indexOf(store.get().theme) + 1) % 3]);
  } }, icon('sun'));
  const topbar = h('header', { class: 'topbar' }, titleEl,
    h('button', { class: 'btn ghost', onclick: openCmd, title: 'Command palette', 'aria-label': 'Open the command palette' }, icon('search'),
      h('kbd', {}, navigator.platform && navigator.platform.startsWith('Mac') ? '⌘K' : 'Ctrl K')),
    themeBtn);

  contentEl = h('div', { class: 'content', id: 'main-content', tabindex: '-1' });
  app.append(h('a', { class: 'skip-link', href: '#main-content' }, 'Skip to content'),
    h('div', { class: 'shell' }, sidebar, h('main', { class: 'main', 'aria-labelledby': 'page-title' }, topbar, contentEl)));
  renderBrand();
  renderNav();

  router = createRouter([
    ...ALL.map(n => ({ path: n.path, name: n.name })),
    { path: '/tasks/:ref', name: 'task-detail' },
    { path: '/admin/:section', name: 'admin-section' },
    ...Object.keys(REDIRECTS).map(p => ({ path: p, name: 'redirect' })),
  ]);
  // The organization decides the menu (features, admin) and the branding; the first page
  // waits for it so it does not render twice.
  loadOrg().finally(() => {
    router.start(onRoute);
    startStream();
    startPolling();
    bindKeys();
  });
  if (s.demo) toast('Demo mode: everything on this page is fixture data. Nothing talks to a server.', { kind: 'info', ttl: 6000 });
}

function switchProject(slug) {
  prefs.set('project', slug);
  store.set({ project: slug });
  const mine = (store.get().projects || []).find(p => p.slug === slug);
  store.set({ role: mine ? mine.role : '' });
  if (stopStream) stopStream();
  startStream();
  if (current && current.instance) current.instance.destroy();
  current = null;
  onRoute(router.current());
}

function onRoute(route) {
  if (route.name === 'redirect') return router.navigate(REDIRECTS[route.path.replace(/\/$/, '')] || '/', { replace: true });
  const wantDrawer = route.name === 'task-detail';
  if (drawer) { const d = drawer; drawer = null; d.close(); }

  // Task detail rides on top of the board: mount tasks underneath, then open the drawer.
  const base = baseName(route);
  const nav = ALL.find(n => n.name === base) || CORE[0];
  markActive();

  const key = nav.name + (route.name === 'admin-section' ? ':' + route.params.section : '');
  if (!current || current.key !== key) {
    if (current && current.instance) current.instance.destroy();
    const root = h('div', { class: 'stack page' });
    replace(contentEl, root);
    if (nav.adminOnly && !isOrgAdmin()) {
      root.append(h('div', { class: 'empty' }, 'The admin area is for your organization\'s administrators (org_admin).'));
      current = { name: nav.name, key, root, instance: null };
    } else {
      // An area an administrator turned off keeps its address; it says so instead of vanishing.
      if (!featureOn(nav)) root.append(h('div', { class: 'notice' }, `${nav.label} is turned off for your organization, so it is not in the menu. `,
        isOrgAdmin() ? h('a', { href: '/admin/features', 'data-link': true }, 'Turn it on in Admin → Features.') : 'An administrator can turn it on.'));
      const viewRoot = h('div', { class: 'stack' });
      root.append(viewRoot);
      current = { name: nav.name, key, root, instance: nav.view.render(viewRoot, ctx(route.params)) };
    }
  }
  const orgName = (store.get().org && store.get().org.branding && store.get().org.branding.display_name) || 'Conductor';
  titleEl.textContent = wantDrawer ? 'Task ' + route.params.ref : nav.label;
  document.title = (wantDrawer ? route.params.ref : nav.label) + ' · ' + orgName;
  if (!wantDrawer && contentEl) contentEl.focus({ preventScroll: true });

  if (wantDrawer) {
    drawer = openTaskDrawer(route.params.ref, ctx(route.params), {
      onClose: () => { if (drawer) { drawer = null; router.navigate('/tasks'); } },
    });
  }
}

// ---------------------------------------------------------------------------
// Live updates
// ---------------------------------------------------------------------------

const refreshSoon = debounce(() => {
  if (document.hidden) return;
  if (current && current.instance) current.instance.refresh();
  if (drawer) drawer.refresh();
}, 800);

async function startStream() {
  const s = store.get();
  const setConn = state => {
    if (!dotEl) return;
    dotEl.className = 'dot ' + (state === 'live' ? 'live' : state === 'reconnecting' || state === 'connecting' ? 'warn' : 'off');
    connEl.textContent = state === 'live' ? 'live' : state === 'closed' ? 'offline' : state + '…';
    store.set({ connection: state });
  };
  if (s.demo) {
    const demo = await import('./demo.js');
    stopStream = demo.demoStream(onEvent, setConn);
    return;
  }
  const url = `/v1/projects/${encodeURIComponent(s.project)}/events/stream?token=${encodeURIComponent(s.token)}`;
  stopStream = connectStream(url, { onEvent, onState: setConn });
}

function onEvent(e) {
  const p = e.payload || {};
  switch (e.type) {
    case 'attempt.stalled': toast(`Attempt stalled on ${p.task_ref || 'a task'}`, { kind: 'warn', detail: p.reason || '' }); break;
    case 'budget.exhausted': toast('Budget pause threshold reached — dispatch stops', { kind: 'danger' }); break;
    case 'budget.downshift': toast('Budget downshift threshold reached — non-sensitive work drops a tier', { kind: 'warn' }); break;
    case 'budget.shared': if (p.to === store.get().handle) toast(`${p.from} shared ${p.tokens} tokens with you`, { kind: 'info' }); break;
    case 'lease.expired': toast(`Lease expired on ${p.task_ref || 'a task'} — territory released`, { kind: 'warn' }); break;
    case 'queue.granted': if (p.principal === store.get().handle) toast('Your queue ticket was granted', { kind: 'info' }); break;
    // Territory this person was refused has been released: tell them to try again.
    case 'scope.released': if (p.principal === store.get().handle) toast(`${(p.resources || []).join(', ') || 'Territory'} is free again`, { kind: 'info', detail: `${p.task_ref || 'The holder'} let go${p.waiting_task_ref ? ' — ' + p.waiting_task_ref + ' can carry on' : ''}. Check again before you edit.` }); break;
    // Someone ran into someone else's work. The person it happened to already saw the refusal.
    case 'conflict.blocked': if (p.principal !== store.get().handle) toast(`${p.principal || 'Someone'} is blocked by ${p.task_ref || 'a private task'}`, { kind: 'warn', detail: (p.resources || []).join(', ') }); break;
    case 'conflict.suggest_join': if (p.principal !== store.get().handle) toast(`${p.principal || 'Someone'} is starting work like ${p.task_ref || 'a private task'}`, { kind: 'info', detail: 'Joining may beat duplicating it.' }); break;
    case 'conflict.detected': toast(`Conflict: ${p.task_ref || 'a task'}${p.with_task_ref ? ' and ' + p.with_task_ref : ''}`, { kind: p.severity === 'high' || p.severity === 'critical' ? 'danger' : 'warn', detail: [p.kind, p.severity, (p.resources || p.changed_paths || []).join(', ')].filter(Boolean).join(' · ') }); break;
    case 'github.pr_merged': toast(`${p.task_ref || 'A task'} merged — done, its files are free`, { kind: 'info' }); break;
  }
  // The events view appends live lines itself instead of refetching.
  const st = current && current.instance && current.instance.state;
  if (current && current.name === 'events' && st && st.push) { st.push(e); return; }
  refreshSoon();
}

function startPolling() {
  setInterval(() => {
    if (document.hidden || store.get().connection === 'live') return;
    refreshSoon();
  }, 15000);
  // Even when live, a gentle periodic refresh keeps relative timestamps honest.
  setInterval(() => { if (!document.hidden) refreshSoon(); }, 60000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshSoon(); });
}

// ---------------------------------------------------------------------------
// Theme, palette, keys
// ---------------------------------------------------------------------------

function applyTheme(theme) {
  if (theme === 'system') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
}

function setTheme(theme) {
  prefs.set('theme', theme);
  store.set({ theme });
  applyTheme(theme);
  applyBranding(store.get().org && store.get().org.branding);
  toast('Theme: ' + theme, { ttl: 1200 });
}

// navItems is everything the palette and the g-shortcuts reach: the menu, plus areas a
// feature flag hides from it, which stay one search away.
function navItems() {
  return [...CORE, ...MORE, MORE_PAGE, SETTINGS, ...(isOrgAdmin() ? [ADMIN] : [])];
}

function openCmd() {
  const c = ctx();
  openPalette({
    commands: [
      ...navItems().map(n => ({ label: n.label, group: 'Go', hint: 'g ' + n.key, run: () => router.navigate(n.path) })),
      { label: 'Check before you edit', group: 'Do', run: () => { router.navigate('/'); setTimeout(() => { const el = document.getElementById('check-summary'); if (el) el.focus(); }, 400); } },
      { label: 'New task', group: 'Do', run: () => openTaskForm(c, { onCreated: v => c.openTask(v.ref) }) },
      { label: 'Toggle theme', group: 'Do', run: () => { const o = ['system', 'light', 'dark']; setTheme(o[(o.indexOf(store.get().theme) + 1) % 3]); } },
      { label: 'Keyboard shortcuts', group: 'Help', hint: '?', run: showShortcuts },
      { label: 'Sign out', group: 'Do', run: signOut },
    ],
    dynamic: q => {
      const m = /^t-?(\d+)$/i.exec(q.trim());
      if (m) return [{ label: `Open task T-${m[1]}`, group: 'Go', run: () => c.openTask('T-' + m[1]) }];
      return [];
    },
  });
}

function showShortcuts() {
  openModal({ title: 'Keyboard shortcuts', body: h('div', { class: 'shortcuts' },
    ...navItems().map(n => h('div', {}, h('span', {}, n.label), h('span', {}, h('kbd', {}, 'g'), ' ', h('kbd', {}, n.key)))),
    h('div', {}, h('span', {}, 'Command palette'), h('kbd', {}, '⌘K')),
    h('div', {}, h('span', {}, 'Search / filter'), h('kbd', {}, '/')),
    h('div', {}, h('span', {}, 'Close drawer or modal'), h('kbd', {}, 'esc')),
    h('div', {}, h('span', {}, 'This help'), h('kbd', {}, '?'))),
    actions: [{ label: 'Close' }] });
}

function bindKeys() {
  let pendingG = false;
  document.addEventListener('keydown', ev => {
    const tag = (ev.target.tagName || '').toLowerCase();
    const typing = tag === 'input' || tag === 'select' || tag === 'textarea' || ev.target.isContentEditable;
    if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'k') { ev.preventDefault(); openCmd(); return; }
    if (typing) return;
    if (ev.key === '?') { ev.preventDefault(); showShortcuts(); return; }
    if (ev.key === '/') {
      const search = contentEl && contentEl.querySelector('input[type=search]');
      if (search) { ev.preventDefault(); search.focus(); }
      return;
    }
    if (pendingG) {
      pendingG = false;
      const nav = navItems().find(n => n.key === ev.key.toLowerCase());
      if (nav) { ev.preventDefault(); router.navigate(nav.path); }
      return;
    }
    if (ev.key.toLowerCase() === 'g') pendingG = true;
    setTimeout(() => { pendingG = false; }, 900);
  });
}
