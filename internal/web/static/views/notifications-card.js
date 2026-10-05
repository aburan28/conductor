import { h, icon } from '../lib/dom.js';
import { card, empty } from '../components/ui.js';
import { pill } from '../components/pill.js';
import { table } from '../components/table.js';
import { openModal, confirmModal } from '../components/modal.js';
import { toast, toastError } from '../components/toast.js';
import { relTime } from '../lib/format.js';

// The Settings card for notification channels (Slack, Discord, signed webhooks). Managing
// them is for maintainers: anyone else's request is refused, and the card is not shown. A
// channel's URL is a credential, so after creation only a hint of it is ever shown; a
// webhook's signing secret is shown once, in the dialog that creates it.

export async function loadNotifications(ctx) {
  try {
    return await ctx.api.get(ctx.api.project(ctx.project, '/notifications'));
  } catch (_) {
    return null;
  }
}

function status(c) {
  if (c.failures > 0) {
    const retry = c.retry_after && new Date(c.retry_after).getTime() > Date.now() ? ` · retry ${relTime(c.retry_after)}` : '';
    return h('span', { title: c.last_error || '' }, pill('danger', `failing ×${c.failures}`), h('span', { class: 'muted' }, retry));
  }
  if (c.last_success_at) return h('span', { title: 'last delivered ' + relTime(c.last_success_at) }, pill('ok', 'ok'));
  return pill('info', 'nothing sent yet');
}

export function notificationsCard(data, ctx, refresh) {
  if (!data) return null;
  const channels = data.channels || [];
  const catalog = data.events || [];
  const defaults = new Set(data.defaults || []);

  const add = () => {
    const kind = h('select', {}, [['slack', 'Slack incoming webhook'], ['discord', 'Discord webhook'], ['webhook', 'Webhook (signed JSON)']]
      .map(([v, l]) => h('option', { value: v }, l)));
    const url = h('input', { type: 'url', placeholder: 'hooks.slack.com/services/…', required: true, autocomplete: 'off', spellcheck: 'false' });
    const name = h('input', { type: 'text', placeholder: '#eng-agents (optional)' });
    // Defaults first, as the server names them; qualified defaults (task.status_changed:done)
    // are offered as their own boxes.
    const options = [...(data.defaults || []), ...catalog.map(e => e.type).filter(t => !defaults.has(t))];
    const describe = t => {
      const [type, status] = t.split(':');
      const e = catalog.find(x => x.type === type);
      return status ? `task moved to ${status}` : (e ? e.description : t);
    };
    const boxes = options.map(t => {
      const box = h('input', { type: 'checkbox', value: t });
      box.checked = defaults.has(t);
      return h('label', { style: { display: 'flex', gap: '8px', alignItems: 'baseline' } }, box,
        h('span', { class: 'mono' }, t), h('span', { class: 'muted' }, describe(t)));
    });
    openModal({
      title: 'Add a notification channel',
      body: h('div', { class: 'form' },
        h('label', { class: 'field' }, 'Kind', kind),
        h('label', { class: 'field' }, 'URL', url),
        h('label', { class: 'field' }, 'Name', name),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '4px', fontSize: '12px', color: 'var(--text-2)', fontWeight: 500 } }, 'Events',
          h('div', { class: 'stack', style: { maxHeight: '220px', overflow: 'auto', gap: '4px', fontWeight: 400, color: 'var(--text)' } }, boxes)),
        h('div', { class: 'hint' }, 'The URL is stored sealed and never shown again. Private tasks appear as "a private task", with no title, ref, or paths.')),
      actions: [{ label: 'Cancel' }, { label: 'Add channel', kind: 'primary', onClick: async close => {
        if (!url.value.trim()) { url.focus(); return false; }
        const events = boxes.map(b => b.querySelector('input')).filter(b => b.checked).map(b => b.value);
        if (!events.length) { toast('Choose at least one event'); return false; }
        try {
          const out = await ctx.api.post(ctx.api.project(ctx.project, '/notifications'), { kind: kind.value, url: url.value.trim(), name: name.value.trim(), events });
          close();
          if (out.secret) {
            openModal({ title: 'Webhook signing secret', body: h('div', { class: 'stack' },
              h('div', { class: 'token-reveal' }, out.secret),
              h('div', { class: 'hint' }, 'Verify each request\'s X-Conductor-Signature with it (README, "Notifications").'),
              h('div', { class: 'notice warn' }, 'Shown once. It is stored sealed and cannot be shown again.')), actions: [{ label: 'Done' }] });
          } else {
            toast('Channel added');
          }
          refresh();
        } catch (err) { toastError(err, 'Could not add the channel'); return false; }
      } }],
    });
  };

  const test = async c => {
    try {
      const res = await ctx.api.post(ctx.api.project(ctx.project, '/notifications/' + encodeURIComponent(c.id) + '/test'));
      if (res.ok) toast('Test message delivered'); else toastError(new Error(res.error || 'not delivered'), 'Test failed');
    } catch (err) { toastError(err, 'Test failed'); }
  };
  const remove = async c => {
    if (!await confirmModal({ title: 'Remove this channel?', message: `Nothing more is sent to ${c.name || c.url_hint}.`, confirmLabel: 'Remove', kind: 'danger' })) return;
    try { await ctx.api.del(ctx.api.project(ctx.project, '/notifications/' + encodeURIComponent(c.id))); toast('Channel removed'); refresh(); }
    catch (err) { toastError(err, 'Could not remove'); }
  };

  return card({
    title: 'Notifications',
    flush: true,
    actions: h('button', { class: 'btn sm primary', onclick: add }, icon('plus'), 'Add channel'),
    body: channels.length ? table({ columns: [
      { key: 'kind', label: 'Kind', render: c => pill('info', c.kind) },
      { key: 'url_hint', label: 'Destination', render: c => h('span', {}, c.name ? h('strong', {}, c.name + ' ') : null, h('span', { class: 'mono muted' }, c.url_hint)) },
      { key: 'events', label: 'Events', render: c => h('span', { title: (c.events || []).join('\n') }, (c.events || []).length === 1 ? c.events[0] : `${(c.events || []).length} types`) },
      { key: 'status', label: 'Status', render: status, sortable: false },
      { key: 'act', label: '', sortable: false, render: c => h('div', { class: 'btn-row' },
        h('button', { class: 'btn sm', onclick: () => test(c) }, 'Test'),
        h('button', { class: 'btn sm danger', onclick: () => remove(c) }, 'Remove')) },
    ], rows: channels }) : empty('No channels. Send conflicts, freed territory, stalls and merges to Slack, Discord, or a webhook.', 'conductor notify add slack <incoming-webhook-url>'),
    footer: 'Channels are project-wide. Webhooks are signed with HMAC-SHA256 (X-Conductor-Signature).',
  });
}
