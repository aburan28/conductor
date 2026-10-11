import { h, icon } from '../lib/dom.js';

// More: every area beyond the core, with a sentence on what each is for. Areas an
// administrator turned off are listed too, marked off, so their addresses stay discoverable.
export default {
  title: 'More',
  render(root, ctx) {
    const item = m => h('li', { class: 'area' + (m.enabled ? '' : ' off') },
      h('a', { href: m.path, 'data-link': true }, icon(m.icon), h('span', { class: 'area-name' }, m.label)),
      h('p', {}, m.desc),
      m.enabled ? null : h('p', { class: 'hint' }, 'Turned off for your organization. ',
        ctx.isOrgAdmin ? h('a', { href: '/admin/features', 'data-link': true }, 'Admin → Features') : 'An administrator can turn it on.'));
    root.replaceChildren(h('div', { class: 'stack' },
      h('p', { class: 'lede' }, 'The day-to-day loop is Home, Tasks and People. These areas go deeper.'),
      h('ul', { class: 'areas' }, (ctx.more || []).map(item))));
    return { refresh: () => {}, destroy: () => {} };
  },
};
