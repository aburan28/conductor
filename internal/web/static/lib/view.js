import { replace } from './dom.js';
import { skeleton, errorBox } from '../components/ui.js';
import { toastError } from '../components/toast.js';

// defineView standardizes the load → draw loop every page follows. The first load shows a
// skeleton and a retryable error; later refreshes (SSE-driven, polling) keep the current
// DOM until fresh data arrives, so the page never flickers back to a skeleton.
export function defineView({ title, load, draw }) {
  return {
    title,
    render(root, ctx) {
      let alive = true;
      let first = true;
      let inflight = null;
      const state = {};
      async function refresh() {
        if (inflight) return inflight;
        inflight = (async () => {
          try {
            const data = await load(ctx, state);
            if (!alive) return;
            replace(root, draw(data, ctx, { refresh, state }));
          } catch (err) {
            if (!alive) return;
            if (first) replace(root, errorBox(err, () => { first = true; refresh(); }));
            else toastError(err, 'Refresh failed');
          } finally {
            first = false;
            inflight = null;
          }
        })();
        return inflight;
      }
      replace(root, skeleton(6));
      refresh();
      return { refresh, state, destroy: () => { alive = false; } };
    },
  };
}

// settle resolves a list of promises individually so one failing endpoint (a 404 from a
// feature the server does not have yet) does not blank the whole page.
export async function settle(promises) {
  const results = await Promise.allSettled(promises);
  return results.map(r => (r.status === 'fulfilled' ? r.value : null));
}
