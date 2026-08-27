import { h } from '../lib/dom.js';
import { defineView } from '../lib/view.js';
import { card, empty, kpi, segmented } from '../components/ui.js';
import { table } from '../components/table.js';
import { stackedBars, hbars } from '../components/chart.js';
import { fmtTokens, fmtUSD, fmtDay, shortID } from '../lib/format.js';
import { prefs } from '../lib/store.js';

const DIMS = [['day', 'Day'], ['hour', 'Hour'], ['harness', 'Harness'], ['model', 'Model'], ['effort', 'Effort'], ['principal', 'Person'], ['source', 'Source'], ['session', 'Session']];
const WINDOWS = [['24h', '24h'], ['7d', '7 days'], ['30d', '30 days'], ['custom', 'Custom']];

export default defineView({
  title: 'Usage',
  async load(ctx, state) {
    state.window = state.window || prefs.get('usage_window', '7d');
    state.dims = state.dims || prefs.get('usage_dims', ['day', 'harness']);
    state.since = state.since || '';
    state.until = state.until || '';
    const since = state.window === 'custom' ? state.since : state.window;
    const q = ctx.api.query({ since, until: state.window === 'custom' ? state.until : '', by: state.dims.join(','), harness: state.harness, model: state.model });
    const report = await ctx.api.get(ctx.api.project(ctx.project, '/usage' + q));
    // The chart always wants a time series by the first non-time dimension; fetch it
    // separately when the table's dimensions would not produce one.
    const timeDim = state.dims.find(d => d === 'day' || d === 'hour');
    const seriesDim = state.dims.find(d => d !== 'day' && d !== 'hour');
    let chartRows = report.rows || [];
    let chartSeries = seriesDim, chartTime = timeDim;
    if (!timeDim) {
      chartTime = state.window === '24h' ? 'hour' : 'day';
      chartSeries = seriesDim || 'harness';
      const cq = ctx.api.query({ since, until: state.window === 'custom' ? state.until : '', by: `${chartTime},${chartSeries}`, harness: state.harness, model: state.model });
      try { chartRows = (await ctx.api.get(ctx.api.project(ctx.project, '/usage' + cq))).rows || []; } catch (_) { chartRows = []; }
    }
    return { report, chartRows, chartSeries: chartSeries || 'harness', chartTime };
  },
  draw({ report, chartRows, chartSeries, chartTime }, ctx, { refresh, state }) {
    const rows = report.rows || [];
    const total = report.total || {};
    const dimKey = { day: 'period', hour: 'period', harness: 'harness', model: 'model', effort: 'reasoning_effort', principal: 'principal', source: 'source', session: 'external_session_id' };
    const dimLabel = { period: state.dims.includes('hour') ? 'Hour' : 'Day', harness: 'Harness', model: 'Model', reasoning_effort: 'Effort', principal: 'Person', source: 'Source', external_session_id: 'Session' };
    const modelText = r => r.redacted && !r.model ? '(undisclosed)' : (r.model || '—');

    const kpis = h('div', { class: 'kpis' },
      kpi({ label: 'Tokens', value: fmtTokens(total.total_tokens), kind: 'accent', sub: `${fmtTokens(total.input_tokens)} in · ${fmtTokens(total.output_tokens)} out` }),
      kpi({ label: 'Cache', value: fmtTokens(total.cache_read_tokens), sub: `read · ${fmtTokens(total.cache_write_tokens)} written` }),
      kpi({ label: 'Calls', value: fmtTokens(total.requests), sub: total.reasoning_tokens ? fmtTokens(total.reasoning_tokens) + ' reasoning tokens' : 'model calls' }),
      kpi({ label: 'Cost', value: fmtUSD(total.cost_usd) === '—' ? '$0' : fmtUSD(total.cost_usd), sub: 'reported by the harness, else catalog list price' }));

    const dimPicker = h('div', { class: 'chips' }, DIMS.map(([k, label]) => {
      const on = state.dims.includes(k);
      return h('button', { class: 'chip sans' + (on ? ' accent' : ''), type: 'button', 'aria-pressed': on, onclick: () => {
        state.dims = on ? state.dims.filter(d => d !== k) : [...state.dims, k];
        if (!state.dims.length) state.dims = ['harness'];
        prefs.set('usage_dims', state.dims); refresh();
      } }, label);
    }));
    const customInputs = state.window === 'custom' ? h('div', { class: 'btn-row' },
      h('input', { type: 'date', value: state.since, onchange: ev => { state.since = ev.target.value; refresh(); } }),
      h('span', { class: 'muted' }, 'to'),
      h('input', { type: 'date', value: state.until, onchange: ev => { state.until = ev.target.value; refresh(); } })) : null;
    const toolbar = h('div', { class: 'toolbar' },
      segmented(WINDOWS.map(([value, label]) => ({ value, label })), state.window, v => { state.window = v; prefs.set('usage_window', v); refresh(); }),
      customInputs, h('div', { class: 'spacer' }), h('span', { class: 'muted' }, 'group by'), dimPicker);

    const chartCard = card({ title: `Tokens by ${chartTime} · stacked by ${chartSeries}`, body: chartRows.length
      ? stackedBars({ rows: chartRows.map(r => ({ ...r, x: r.period, s: chartSeries === 'model' ? modelText(r) : (r[dimKey[chartSeries]] || '—') })), xKey: 'x', seriesKey: 's', valueKey: 'total_tokens', lineKey: 'cost_usd', xLabel: chartTime === 'hour' ? iso => new Date(iso).toLocaleTimeString([], { hour: '2-digit' }) : fmtDay })
      : empty('No usage recorded in this window.', 'conductor usage sync'),
      footer: 'Bars are tokens; the dashed line is cost. Hover a segment for the breakdown.' });

    const dims = state.dims.map(d => dimKey[d]);
    const columns = [
      ...dims.map(k => ({ key: k, label: dimLabel[k], mono: k !== 'principal' && k !== 'period', render: r => k === 'period' ? (r.period ? (state.dims.includes('hour') ? new Date(r.period).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit' }) : fmtDay(r.period)) : '—') : k === 'model' ? modelText(r) : k === 'external_session_id' ? shortID(r[k]) : (r[k] || '—'), sort: k === 'period' ? r => new Date(r.period || 0) : undefined })),
      { key: 'requests', label: 'Calls', num: true, render: r => fmtTokens(r.requests) },
      { key: 'input_tokens', label: 'Input', num: true, render: r => fmtTokens(r.input_tokens) },
      { key: 'cache_read_tokens', label: 'Cache rd', num: true, render: r => fmtTokens(r.cache_read_tokens) },
      { key: 'cache_write_tokens', label: 'Cache wr', num: true, render: r => fmtTokens(r.cache_write_tokens) },
      { key: 'output_tokens', label: 'Output', num: true, render: r => fmtTokens(r.output_tokens) },
      { key: 'total_tokens', label: 'Total', num: true, render: r => h('strong', {}, fmtTokens(r.total_tokens)) },
      { key: 'cost_usd', label: 'Cost', num: true, render: r => fmtUSD(r.cost_usd) },
    ];
    const footer = { requests: fmtTokens(total.requests), input_tokens: fmtTokens(total.input_tokens), cache_read_tokens: fmtTokens(total.cache_read_tokens), cache_write_tokens: fmtTokens(total.cache_write_tokens), output_tokens: fmtTokens(total.output_tokens), total_tokens: fmtTokens(total.total_tokens), cost_usd: fmtUSD(total.cost_usd) };
    if (dims.length) footer[dims[0]] = 'total';
    const tableCard = card({ title: 'Breakdown', flush: true, body: rows.length ? table({ columns, rows, footer, initialSort: { key: 'total_tokens', dir: 'desc' } }) : empty('Nothing to break down yet. Sessions launched through `conductor wrap` report automatically.', 'conductor wrap claude'),
      footer: 'Team totals by day, harness, and model are visible to every member; per-session detail is your own unless you maintain the project.' });

    const seriesDim = state.dims.find(d => d !== 'day' && d !== 'hour');
    const share = seriesDim && rows.length ? card({ title: `Share by ${seriesDim}`, body: hbars({ rows: aggregate(rows, dimKey[seriesDim], modelText), label: r => r.key, value: r => r.total }) }) : null;

    return h('div', { class: 'stack', style: { gap: '20px' } }, toolbar, kpis, chartCard, share ? h('div', { class: 'grid-2' }, tableCard, share) : tableCard);
  },
});

function aggregate(rows, key, modelText) {
  const acc = new Map();
  for (const r of rows) {
    const k = key === 'model' ? modelText(r) : (r[key] || '—');
    acc.set(k, (acc.get(k) || 0) + Number(r.total_tokens || 0));
  }
  return [...acc.entries()].map(([k, total]) => ({ key: k, total })).sort((a, b) => b.total - a.total).slice(0, 12);
}
