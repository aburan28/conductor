// HTTP client for the control plane. Same-origin, bearer token, uniform error envelope.
// `request` is the single seam: demo mode replaces it with a fixture-backed function.

export class ApiError extends Error {
  constructor(status, code, message, body) {
    super(message || `request failed: ${status}`);
    this.status = status;
    this.code = code || '';
    this.body = body;
  }
  get blocked() { return this.status === 409; }
  get notFound() { return this.status === 404; }
}

const enc = encodeURIComponent;

export function createApi({ token = '', base = '' } = {}) {
  const api = {
    token,
    base,
    async request(method, path, body) {
      const headers = { Accept: 'application/json' };
      if (api.token) headers.Authorization = 'Bearer ' + api.token;
      if (body !== undefined) headers['Content-Type'] = 'application/json';
      let res;
      try {
        res = await fetch(api.base + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
      } catch (err) {
        throw new ApiError(0, 'network', 'cannot reach the control plane: ' + err.message);
      }
      const text = await res.text();
      const isJSON = (res.headers.get('content-type') || '').includes('json');
      let data = null;
      if (text && isJSON) { try { data = JSON.parse(text); } catch (_) { data = null; } }
      if (!res.ok) {
        const msg = (data && (data.error || data.message)) || text.trim() || res.statusText;
        throw new ApiError(res.status, data && data.code, msg, data);
      }
      return isJSON ? data : text;
    },
    get: path => api.request('GET', path),
    post: (path, body = {}) => api.request('POST', path, body),
    patch: (path, body = {}) => api.request('PATCH', path, body),
    del: path => api.request('DELETE', path),
    // Path helpers keep the encoding in one place.
    project: (p, suffix = '') => `/v1/projects/${enc(p)}${suffix}`,
    task: (p, ref, suffix = '') => `/v1/tasks/${enc(ref)}${suffix}?project=${enc(p)}`,
    query(params) {
      const q = Object.entries(params).filter(([, v]) => v !== undefined && v !== null && v !== '')
        .map(([k, v]) => `${enc(k)}=${enc(v)}`).join('&');
      return q ? '?' + q : '';
    },
  };
  return api;
}
