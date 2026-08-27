import { h } from '../lib/dom.js';
import { chips, severityChip } from './pill.js';
import { relTime } from '../lib/format.js';
import { resolveConflict } from '../lib/actions.js';

export function conflictCard(c, ctx, { onChange, compact = false } = {}) {
  const party = p => h('span', {}, h('a', { class: 'ref', href: `/tasks/${encodeURIComponent(p.task_ref)}`, 'data-link': true }, p.task_ref),
    p.owner ? h('span', { class: 'muted' }, ' · ' + p.owner) : null,
    !compact ? (p.title ? h('span', { class: 'muted' }, ' — ' + p.title) : h('span', { class: 'private' }, ' — private work')) : null);
  return h('div', { class: 'conflict ' + (c.severity || '') },
    h('div', { class: 'headline' }, severityChip(c.severity), party(c.mine), '↔', party(c.other),
      c.state && c.state !== 'open' ? h('span', { class: 'pill ' + c.state }, c.state) : null,
      h('span', { class: 'meta', style: { marginLeft: 'auto' } }, relTime(c.detected_at))),
    h('div', { class: 'advice' }, (c.reason || String(c.kind || '').replace(/_/g, ' ')), ' → ', h('strong', {}, String(c.suggestion || '').replace(/_/g, ' '))),
    c.resources && c.resources.length ? chips(c.resources) : null,
    c.state === 'open' || !c.state ? h('div', { class: 'actions btn-row' },
      h('button', { class: 'btn sm', onclick: async () => { if (await resolveConflict(ctx, c) && onChange) onChange(); } }, 'Resolve…')) : null);
}
