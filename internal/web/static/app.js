// Conductor dashboard — bootstrap, shell, routing, live updates. No framework, no build
// step, no external requests (TestNoExternalRequests enforces that last one).
import { h, icon, replace, clear, debounce } from './lib/dom.js';
import { createStore, prefs } from './lib/store.js';
import { createApi } from './lib/api.js';
import { createRouter } from './lib/router.js';
import { connectStream } from './lib/sse.js';
import { toast } from './components/toast.js';
import { openPalette } from './components/palette.js';
import { openModal } from './components/modal.js';
import { openTaskForm } from './components/task-form.js';
import { renderConnect } from './views/connect.js';
import { openTaskDrawer } from './views/task-detail.js';
import overview from './views/overview.js';
import tasks from './views/tasks.js';
import sessions from './views/sessions.js';
import fleet from './views/fleet.js';
import swarm from './views/swarm.js';
import queue from './views/queue.js';
import conflicts from './views/conflicts.js';
import usage from './views/usage.js';
import events from './views/events.js';
import integrations from './views/integrations.js';
import settings from './views/settings.js';

const NAV = [
  { path: '/', name: 'overview', label: 'Overview', icon: 'overview', view: overview, key: 'o' },
  { path: '/tasks', name: 'tasks', label: 'Tasks', icon: 'tasks', view: tasks, key: 't' },
  { path: '/sessions', name: 'sessions', label: 'Sessions', icon: 'sessions', view: sessions, key: 's' },
  { path: '/fleet', name: 'fleet', label: 'Fleet', icon: 'fleet', view: fleet, key: 'f' },
  { path: '/swarm', name: 'swarm', label: 'Swarm', icon: 'swarm', view: swarm, key: 'w' },
  { path: '/queue', name: 'queue', label: 'Queue', icon: 'queue', view: queue, key: 'q' },
  { path: '/conflicts', name: 'conflicts', label: 'Conflicts', icon: 'conflicts', view: conflicts, key: 'c' },
  { path: '/usage', name: 'usage', label: 'Usage', icon: 'usage', view: usage, key: 'u' },
  { path: '/events', name: 'events', label: 'Events', icon: 'events', view: events, key: 'e' },
  { path: '/integrations', name: 'integrations', label: 'Integrations', icon: 'integrations', view: integrations, key: 'n' },
  { path: '/settings', name: 'settings', label: 'Settings', icon: 'settings', view: settings, key: ',' },
];

const app = document.getElementById('app');
const params = new URLSearchParams(location.search);
// An invite link (from `conductor invite`) carries the token in the URL fragment, which the
// browser never sends to the server, so the credential stays out of every request line and
// access log. `conductor dashboard` historically used the query string; both are accepted,
// fragment first, and either is stripped from the address bar the moment it is read.
const frag = new URLSearchParams((location.hash || '').replace(/^#/, ''));
const demoMode = params.get('demo') === '1' || frag.get('demo') === '1';
const urlToken = frag.get('token') || params.get('token');
const urlProject = frag.get('project') || params.get('project');
if (urlToken) prefs.set('token', urlToken);
if (urlProject) prefs.set('project', urlProject);
if (urlToken || urlProject) {
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
});

applyTheme(store.get().theme);
document.documentElement.dataset.density = prefs.get('density', 'comfortable');

let api = createApi({ token: store.get().token });
let stopStream = null;
let current = null;      // { name, instance, root }
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
  if (!s.token || !s.project) return showConnect();
  try {
    const who = await api.get('/v1/whoami');
    const projects = who.projects || [];
    const mine = projects.find(p => p.slug === s.project || p.id === s.project);
    store.set({ handle: who.principal.handle, role: mine ? mine.role : '', projects, endpoint: who.endpoint || '' });
    prefs.set('handle', who.principal.handle);
    prefs.set('projects', projects);
    if (mine) prefs.set('role', mine.role);
    enter();
  } catch (err) {
    if (err.status === 401) showConnect('Your saved token was not accepted. Sign in again.');
    else showConnect(err.message);
  }
}

function showConnect(error) {
  renderConnect(app, {
    error,
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

// A loopback endpoint is what a daemon derives when it binds a wildcard address without
// --public-url; it is only reachable from the machine itself, so it must never replace the
// address the viewer is actually browsing through.
function isLoopbackOrigin(u) {
  try {
    const host = new URL(u).hostname;
    return host === 'localhost' || host === '::1' || host.startsWith('127.');
  } catch { return true; }
}

function ctx(extraParams = {}) {
  const s = store.get();
  return {
    api, store,
    project: s.project, handle: s.handle, role: s.role, token: s.token,
    origin: s.endpoint && !isLoopbackOrigin(s.endpoint) ? s.endpoint : location.origin,
    params: extraParams,
    navigate: p => router.navigate(p),
    openTask: ref => router.navigate('/tasks/' + encodeURIComponent(ref)),
    refreshAll: () => current && current.instance && current.instance.refresh(),
    signOut,
    setTheme,
  };
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

let contentEl, titleEl, connEl, dotEl;

function enter() {
  const s = store.get();
  clear(app);

  const navLinks = NAV.map(n => h('a', { href: n.path, 'data-link': true, dataset: { name: n.name } },
    icon(n.icon), h('span', { class: 'label' }, n.label)));

  dotEl = h('span', { class: 'dot' });
  connEl = h('span', {}, 'connecting…');
  const projectSel = h('select', { 'aria-label': 'project', onchange: ev => switchProject(ev.target.value) },
    (s.projects && s.projects.length ? s.projects : [{ slug: s.project }]).map(p =>
      h('option', { value: p.slug, selected: p.slug === s.project }, p.slug)));

  const sidebar = h('nav', { class: 'sidebar', 'aria-label': 'primary' },
    h('div', { class: 'brand' }, h('div', { class: 'logo' }, icon('bolt', 15)), h('span', { class: 'name' }, 'Conductor'),
      s.demo ? h('span', { class: 'demo-badge' }, 'DEMO') : null),
    h('div', { class: 'project-switch' }, projectSel),
    h('div', { class: 'nav' }, navLinks),
    h('div', { class: 'sidebar-foot' },
      h('div', { class: 'conn' }, dotEl, connEl),
      h('div', {}, s.handle || '—', s.role ? h('span', { class: 'muted' }, ' · ' + s.role) : null)));

  titleEl = h('h1', {}, 'Overview');
  const themeBtn = h('button', { class: 'btn ghost icon', title: 'theme', 'aria-label': 'toggle theme', onclick: () => {
    const order = ['system', 'light', 'dark'];
    setTheme(order[(order.indexOf(store.get().theme) + 1) % 3]);
  } }, icon('sun'));
  const topbar = h('header', { class: 'topbar' }, titleEl,
    h('button', { class: 'btn ghost', onclick: openCmd, title: 'Command palette' }, icon('search'), h('kbd', {}, navigator.platform && navigator.platform.startsWith('Mac') ? '⌘K' : 'Ctrl K')),
    themeBtn);

  contentEl = h('div', { class: 'content' });
  app.append(h('div', { class: 'shell' }, sidebar, h('main', { class: 'main' }, topbar, contentEl)));

  router = createRouter([
    ...NAV.map(n => ({ path: n.path, name: n.name })),
    { path: '/tasks/:ref', name: 'task-detail' },
  ]);
  router.start(onRoute);
  startStream();
  startPolling();
  bindKeys();
  if (s.demo) toast('Demo mode: everything on this page is fixture data. Nothing talks to a server.', { kind: 'info', ttl: 6000 });
}

function switchProject(slug) {
  prefs.set('project', slug);
  store.set({ project: slug });
  const mine = (store.get().projects || []).find(p => p.slug === slug);
  store.set({ role: mine ? mine.role : '' });
  if (stopStream) stopStream();
  startStream();
  onRoute(router.current());
}

function onRoute(route) {
  const wantDrawer = route.name === 'task-detail';
  if (drawer) { const d = drawer; drawer = null; d.close(); }

  // Task detail rides on top of the board: mount tasks underneath, then open the drawer.
  const base = wantDrawer ? 'tasks' : route.name;
  const nav = NAV.find(n => n.name === base) || NAV[0];

  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('active', a.dataset.name === base));

  if (!current || current.name !== nav.name) {
    if (current && current.instance) current.instance.destroy();
    const root = h('div', { class: 'stack', style: { gap: '20px' } });
    replace(contentEl, root);
    current = { name: nav.name, root, instance: nav.view.render(root, ctx(route.params)) };
  }
  titleEl.textContent = wantDrawer ? 'Task ' + route.params.ref : nav.label;
  document.title = (wantDrawer ? route.params.ref + ' · ' : nav.label + ' · ') + 'Conductor';

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
  toast('Theme: ' + theme, { ttl: 1200 });
}

function openCmd() {
  const c = ctx();
  openPalette({
    commands: [
      ...NAV.map(n => ({ label: n.label, group: 'Go', hint: 'g ' + n.key, run: () => router.navigate(n.path) })),
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
    ...NAV.map(n => h('div', {}, h('span', {}, n.label), h('span', {}, h('kbd', {}, 'g'), ' ', h('kbd', {}, n.key)))),
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
      const nav = NAV.find(n => n.key === ev.key.toLowerCase());
      if (nav) { ev.preventDefault(); router.navigate(nav.path); }
      return;
    }
    if (ev.key.toLowerCase() === 'g') pendingG = true;
    setTimeout(() => { pendingG = false; }, 900);
  });
}
