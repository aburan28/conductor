import { h } from '../lib/dom.js';
import { card, meter } from '../components/ui.js';
import { relTime } from '../lib/format.js';

// The usage-limits card (docs/USAGE_LIMITS.md). Your own logins in detail — how much of each
// rolling window is used and when it resets — and, for the rest of the team, a count of
// logins near their limit and nothing more. A control plane without the endpoint, or the
// demo, simply shows no card.

export async function loadQuota(ctx) {
  try {
    return await ctx.api.get(ctx.api.project(ctx.project, '/quota'));
  } catch (_) {
    return null;
  }
}

function resetsIn(iso) {
  if (!iso) return '—';
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return 'now';
  const m = Math.round(ms / 60000), d = Math.floor(m / 1440), hh = Math.floor((m % 1440) / 60);
  return d ? `${d}d ${hh}h` : hh ? `${hh}h ${m % 60}m` : `${m}m`;
}

function usedPercent(s) {
  if (s.resets_at && new Date(s.resets_at).getTime() <= Date.now()) return 0;
  if (typeof s.used_percent === 'number') return s.used_percent;
  if (typeof s.used === 'number' && s.limit > 0) return (s.used / s.limit) * 100;
  return s.limit_reached ? 100 : null;
}

const LEVEL = { warning: ['warn', 'warning'], critical: ['danger', 'critical'], exhausted: ['danger', 'exhausted'] };

export function quotaCard(data) {
  if (!data) return null;
  const mine = (data.mine && data.mine.snapshots) || [];
  if (!mine.length && !data.logins) return null;
  const th = (data.mine && data.mine.thresholds) || { warn_percent: 80, critical_percent: 95 };

  const rows = mine.map(s => {
    const pct = usedPercent(s);
    const lvl = LEVEL[s.level];
    return h('div', { class: 'quota-row', style: { display: 'grid', gridTemplateColumns: 'minmax(9em, 1.4fr) 5em minmax(6em, 2fr) 4em 5.5em', gap: '10px', alignItems: 'center', padding: '6px 0' } },
      h('div', {}, h('strong', {}, s.harness), ' ', h('span', { class: 'muted' }, s.account),
        s.source_kind === 'undocumented' ? h('span', { class: 'muted', title: 'read from an endpoint the vendor does not document' }, ' · undocumented') : null),
      h('span', { class: 'mono' }, s.window),
      pct == null ? h('span', { class: 'muted' }, 'unknown') : meter(pct, 100, { warnAt: th.warn_percent / 100, dangerAt: th.critical_percent / 100 }),
      h('span', { class: 'mono' }, pct == null ? '—' : Math.round(pct) + '%'),
      lvl ? h('span', { class: 'pill ' + lvl[0] }, lvl[1]) : h('span', { class: 'muted', title: 'observed ' + relTime(s.observed_at) }, resetsIn(s.resets_at)));
  });

  const team = data.logins
    ? `Team: ${data.near_limit} of ${data.logins} logins reported in the last ${data.window_hours}h are near their limit` + (data.exhausted ? `, ${data.exhausted} exhausted.` : '.')
    : '';
  return card({
    title: 'Usage limits',
    body: rows.length ? h('div', {}, rows) : h('div', { class: 'muted' }, 'None of your logins has reported yet. ', h('code', { class: 'cmd' }, 'conductor quota statusline install')),
    footer: [team, 'Only you see your logins.'].filter(Boolean).join(' '),
  });
}
