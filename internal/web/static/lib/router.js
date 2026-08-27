// History-API router. Patterns are '/tasks/:ref' style; the server returns the index page for
// every non-API path, so deep links and reloads land here.
export function createRouter(routes) {
  const compiled = routes.map(r => {
    const keys = [];
    const re = new RegExp('^' + r.path.replace(/\/:([^/]+)/g, (_, k) => { keys.push(k); return '/([^/]+)'; }) + '/?$');
    return { ...r, re, keys };
  });
  let listener = () => {};

  function match(pathname) {
    for (const r of compiled) {
      const m = r.re.exec(pathname);
      if (!m) continue;
      const params = {};
      r.keys.forEach((k, i) => { params[k] = decodeURIComponent(m[i + 1]); });
      return { name: r.name, params, path: pathname };
    }
    return { name: 'not-found', params: {}, path: pathname };
  }

  function current() {
    return match(location.pathname);
  }

  function navigate(path, { replace = false } = {}) {
    if (path === location.pathname + location.search) return;
    history[replace ? 'replaceState' : 'pushState']({}, '', path);
    listener(current());
  }

  function start(fn) {
    listener = fn;
    window.addEventListener('popstate', () => listener(current()));
    document.addEventListener('click', ev => {
      const a = ev.target.closest && ev.target.closest('a[data-link]');
      if (!a || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.button !== 0) return;
      ev.preventDefault();
      navigate(a.getAttribute('href'));
    });
    listener(current());
  }

  return { match, current, navigate, start };
}
