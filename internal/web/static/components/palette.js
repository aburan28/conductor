import { h, clear, icon } from '../lib/dom.js';

// Command palette: fuzzy filter over a list of {label, group, hint, run}. `dynamic(q)` can
// add query-dependent entries such as "Open task T-42".
export function openPalette({ commands, dynamic }) {
  const previous = document.activeElement;
  const input = h('input', { type: 'text', placeholder: 'Jump to a view, open a task by ref, or run an action…', 'aria-label': 'Command' });
  const list = h('ul', { role: 'listbox' });
  const scrim = h('div', { class: 'palette-scrim' }, h('div', { class: 'palette' }, input, list));
  let items = [];
  let active = 0;

  const close = () => {
    scrim.remove();
    document.removeEventListener('keydown', onKey, true);
    if (previous && previous.focus) previous.focus();
  };

  function score(text, q) {
    text = text.toLowerCase();
    if (!q) return 1;
    if (text.startsWith(q)) return 3;
    if (text.includes(q)) return 2;
    // subsequence match
    let i = 0;
    for (const ch of text) if (ch === q[i]) i++;
    return i === q.length ? 1 : 0;
  }

  function render() {
    const q = input.value.trim().toLowerCase();
    items = [...(dynamic ? dynamic(input.value.trim()) : []), ...commands]
      .map(c => ({ c, s: score(c.label, q) })).filter(x => x.s > 0)
      .sort((a, b) => b.s - a.s).slice(0, 14).map(x => x.c);
    active = Math.min(active, Math.max(0, items.length - 1));
    clear(list);
    if (!items.length) list.append(h('li', { class: 'muted' }, 'No matches'));
    items.forEach((c, i) => list.append(h('li', {
      class: i === active ? 'active' : '', role: 'option', 'aria-selected': i === active,
      onmousemove: () => { if (active !== i) { active = i; render(); } },
      onclick: () => { close(); c.run(); },
    }, c.group ? h('span', { class: 'grp' }, c.group) : null, c.label, c.hint ? h('span', { class: 'k' }, c.hint) : null)));
  }

  function onKey(ev) {
    if (ev.key === 'Escape') { ev.preventDefault(); close(); }
    else if (ev.key === 'ArrowDown') { ev.preventDefault(); active = (active + 1) % Math.max(1, items.length); render(); }
    else if (ev.key === 'ArrowUp') { ev.preventDefault(); active = (active - 1 + items.length) % Math.max(1, items.length); render(); }
    else if (ev.key === 'Enter') { ev.preventDefault(); const c = items[active]; if (c) { close(); c.run(); } }
  }

  input.addEventListener('input', () => { active = 0; render(); });
  scrim.addEventListener('mousedown', ev => { if (ev.target === scrim) close(); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(scrim);
  render();
  input.focus();
  return { close };
}

export function paletteButton(onOpen) {
  return h('button', { class: 'btn ghost', onclick: onOpen, title: 'Command palette (⌘K)' }, icon('search'), h('kbd', {}, '⌘K'));
}
