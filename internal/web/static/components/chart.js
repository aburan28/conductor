import { h, svg } from '../lib/dom.js';
import { fmtTokens, fmtUSD, fmtDay } from '../lib/format.js';

export const PALETTE = ['var(--c1)', 'var(--c2)', 'var(--c3)', 'var(--c4)', 'var(--c5)', 'var(--c6)', 'var(--c7)', 'var(--c8)'];

export function colorFor(key, keys) {
  return PALETTE[Math.max(0, keys.indexOf(key)) % PALETTE.length];
}

// Stacked bar chart over a time axis, with an optional overlaid line (cost). Data is a list
// of {x, series, value, line} rows; the chart groups by x and stacks by series.
export function stackedBars({ rows, xKey, seriesKey, valueKey, lineKey, xLabel = fmtDay, valueLabel = fmtTokens, lineLabel = fmtUSD, height = 220 }) {
  const W = 720, H = height, padL = 46, padR = lineKey ? 46 : 12, padT = 12, padB = 26;
  const xs = [...new Set(rows.map(r => r[xKey]))].sort();
  const series = [...new Set(rows.map(r => r[seriesKey] || '—'))].sort();
  const byX = new Map(xs.map(x => [x, { total: 0, line: 0, parts: new Map() }]));
  for (const r of rows) {
    const g = byX.get(r[xKey]);
    const v = Number(r[valueKey] || 0);
    g.parts.set(r[seriesKey] || '—', (g.parts.get(r[seriesKey] || '—') || 0) + v);
    g.total += v;
    g.line += Number(r[lineKey] || 0);
  }
  const maxV = Math.max(1, ...[...byX.values()].map(g => g.total));
  const maxL = Math.max(0.01, ...[...byX.values()].map(g => g.line));
  const innerW = W - padL - padR, innerH = H - padT - padB;
  const slot = innerW / Math.max(1, xs.length);
  const barW = Math.max(4, Math.min(48, slot * 0.66));
  const yOf = v => padT + innerH - (v / maxV) * innerH;
  const yOfLine = v => padT + innerH - (v / maxL) * innerH;

  const wrap = h('div', { class: 'chart' });
  const tip = h('div', { class: 'chart-tip', hidden: true });
  const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': `Stacked bars by ${seriesKey} over ${xs.length} periods; peak ${valueLabel(maxV)}` });

  // grid + y axis
  const grid = svg('g', { class: 'grid' });
  const axis = svg('g', { class: 'axis' });
  for (let i = 0; i <= 4; i++) {
    const v = (maxV / 4) * i;
    grid.append(svg('line', { x1: padL, x2: W - padR, y1: yOf(v), y2: yOf(v) }));
    axis.append(svg('text', { x: padL - 6, y: yOf(v) + 3, 'text-anchor': 'end' }, valueLabel(v)));
    if (lineKey) axis.append(svg('text', { x: W - padR + 6, y: yOfLine((maxL / 4) * i) + 3 }, lineLabel((maxL / 4) * i) === '—' ? '$0' : lineLabel((maxL / 4) * i)));
  }
  root.append(grid, axis);

  const bars = svg('g');
  xs.forEach((x, i) => {
    const g = byX.get(x);
    const cx = padL + slot * i + slot / 2;
    let acc = 0;
    for (const s of series) {
      const v = g.parts.get(s) || 0;
      if (!v) continue;
      const y1 = yOf(acc + v), y0 = yOf(acc);
      const rect = svg('rect', {
        class: 'bar', x: cx - barW / 2, y: y1, width: barW, height: Math.max(0.5, y0 - y1), fill: colorFor(s, series), rx: 1.5,
        onmousemove: ev => showTip(ev, x, s, v, g),
        onmouseleave: () => { tip.hidden = true; },
      });
      rect.append(svg('title', {}, `${xLabel(x)} · ${s}: ${valueLabel(v)}`));
      bars.append(rect);
      acc += v;
    }
    const every = Math.ceil(xs.length / 10);
    if (i % every === 0) axis.append(svg('text', { x: cx, y: H - padB + 14, 'text-anchor': 'middle' }, xLabel(x)));
  });
  root.append(bars);

  if (lineKey && xs.some(x => byX.get(x).line > 0)) {
    const d = xs.map((x, i) => `${i ? 'L' : 'M'}${padL + slot * i + slot / 2},${yOfLine(byX.get(x).line)}`).join(' ');
    root.append(svg('path', { class: 'line', d }));
    xs.forEach((x, i) => root.append(svg('circle', { cx: padL + slot * i + slot / 2, cy: yOfLine(byX.get(x).line), r: 2.5, fill: 'var(--text)' })));
  }

  function showTip(ev, x, s, v, g) {
    tip.replaceChildren(
      h('div', {}, h('strong', {}, xLabel(x)), ' · ', s),
      h('div', {}, valueLabel(v), h('span', { class: 'muted' }, ` of ${valueLabel(g.total)}`)),
      lineKey && g.line ? h('div', { class: 'muted' }, 'cost ' + lineLabel(g.line)) : null);
    tip.hidden = false;
    const r = wrap.getBoundingClientRect();
    tip.style.left = Math.min(r.width - 160, ev.clientX - r.left + 12) + 'px';
    tip.style.top = (ev.clientY - r.top - 10) + 'px';
  }

  wrap.append(root, tip, legend(series));
  return wrap;
}

export function legend(keys) {
  return h('div', { class: 'legend', style: { marginTop: '6px' } },
    keys.map(k => h('span', {}, h('span', { class: 'sw', style: { background: colorFor(k, keys) } }), k)));
}

// Horizontal bars for a single categorical dimension.
export function hbars({ rows, label, value, valueLabel = fmtTokens }) {
  const max = Math.max(1, ...rows.map(r => Number(value(r) || 0)));
  return h('div', { class: 'stack', role: 'list' }, rows.map(r => {
    const v = Number(value(r) || 0);
    return h('div', { role: 'listitem', class: 'row', style: { padding: '5px 0' } },
      h('div', { class: 'who', style: { minWidth: '160px', fontWeight: 500 } }, label(r)),
      h('div', { class: 'what' },
        h('div', { class: 'sparkline', style: { width: Math.max(1, Math.round(100 * v / max)) + '%' }, 'aria-hidden': 'true' })),
      h('div', { class: 'meta' }, valueLabel(v)));
  }));
}
