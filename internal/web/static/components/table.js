import { h, clear } from '../lib/dom.js';

// Sortable table. columns: [{key, label, render(row), sort(row), num, mono, width}]
export function table({ columns, rows, rowKey, onRow, empty, initialSort, footer, caption }) {
  let sort = initialSort || null;
  const wrap = h('div', { class: 'table-wrap' });
  const tbl = h('table', { class: 'tbl' });
  wrap.append(tbl);

  function sorted() {
    if (!sort) return rows;
    const col = columns.find(c => c.key === sort.key);
    if (!col) return rows;
    const get = col.sort || (r => r[col.key]);
    return [...rows].sort((a, b) => {
      const va = get(a), vb = get(b);
      let cmp;
      if (typeof va === 'number' && typeof vb === 'number') cmp = va - vb;
      else if (va instanceof Date && vb instanceof Date) cmp = va - vb;
      else cmp = String(va ?? '').localeCompare(String(vb ?? ''));
      return sort.dir === 'asc' ? cmp : -cmp;
    });
  }

  function render() {
    clear(tbl);
    if (caption) tbl.append(h('caption', { class: 'sr-only' }, caption));
    // A sortable header is a button, so the keyboard reaches it, and aria-sort says which
    // way the column is sorted.
    tbl.append(h('thead', {}, h('tr', {}, columns.map(c => {
      const sorted = sort && sort.key === c.key;
      const toggle = () => {
        sort = sorted ? { key: c.key, dir: sort.dir === 'asc' ? 'desc' : 'asc' } : { key: c.key, dir: c.num ? 'desc' : 'asc' };
        render();
        const again = tbl.querySelector(`th[data-key="${c.key}"] button`);
        if (again) again.focus();
      };
      return h('th', {
        scope: 'col', class: (c.num ? 'num ' : '') + (c.sortable === false ? '' : 'sortable'), style: c.width ? { width: c.width } : null,
        dataset: { key: c.key }, 'aria-sort': sorted ? (sort.dir === 'asc' ? 'ascending' : 'descending') : null,
      }, !c.label ? h('span', { class: 'sr-only' }, c.srLabel || 'Actions') : c.sortable === false ? c.label
        : h('button', { type: 'button', class: 'th-sort', onclick: toggle }, c.label, sorted ? h('span', { class: 'arrow', 'aria-hidden': 'true' }, sort.dir === 'asc' ? '↑' : '↓') : null));
    }))));
    const body = h('tbody');
    const list = sorted();
    if (!list.length) {
      body.append(h('tr', {}, h('td', { colspan: columns.length }, empty || h('div', { class: 'empty' }, 'Nothing here.'))));
    }
    for (const row of list) {
      const tr = h('tr', { class: onRow ? 'clickable' : '', dataset: rowKey ? { key: rowKey(row) } : null, onclick: onRow ? () => onRow(row) : null },
        columns.map(c => h('td', { class: (c.num ? 'num ' : '') + (c.mono ? 'mono' : '') }, c.render ? c.render(row) : row[c.key])));
      body.append(tr);
    }
    tbl.append(body);
    if (footer) tbl.append(h('tfoot', {}, h('tr', {}, columns.map(c => h('td', { class: c.num ? 'num' : '' }, footer[c.key] ?? '')))));
  }
  render();
  wrap.update = next => { rows = next; render(); };
  return wrap;
}
