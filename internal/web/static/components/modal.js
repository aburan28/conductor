import { h, icon } from '../lib/dom.js';

// openModal renders a dialog and returns {close, el}. `body` is a node; `actions` is a list of
// {label, kind, onClick(close)} rendered in the footer. Escape and the scrim close it.
export function openModal({ title, body, actions = [], wide = false, onClose } = {}) {
  const previous = document.activeElement;
  const scrim = h('div', { class: 'modal-scrim' });
  const close = () => {
    if (!scrim.isConnected) return;
    scrim.remove();
    document.removeEventListener('keydown', onKey);
    if (previous && previous.focus) previous.focus();
    if (onClose) onClose();
  };
  const onKey = ev => { if (ev.key === 'Escape') { ev.stopPropagation(); close(); } };
  const footer = actions.length ? h('footer', {}, actions.map(a =>
    h('button', {
      class: 'btn ' + (a.kind || ''), type: a.submit ? 'submit' : 'button', disabled: a.disabled,
      onclick: async ev => {
        ev.preventDefault();
        if (!a.onClick) return close();
        ev.currentTarget.disabled = true;
        try { const r = await a.onClick(close); if (r !== false && !a.keepOpen) close(); }
        catch (err) { console.error(err); }
        finally { if (ev.currentTarget) ev.currentTarget.disabled = false; }
      },
    }, a.label))) : null;
  const dialog = h('div', { class: 'modal' + (wide ? ' wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
    h('header', {}, h('h2', {}, title), h('button', { class: 'btn ghost icon', 'aria-label': 'close', onclick: close }, icon('close'))),
    h('div', { class: 'body' }, body),
    footer);
  scrim.append(dialog);
  scrim.addEventListener('mousedown', ev => { if (ev.target === scrim) close(); });
  document.addEventListener('keydown', onKey);
  document.body.append(scrim);
  const first = dialog.querySelector('input, select, textarea, button.primary, button');
  if (first) first.focus();
  return { close, el: dialog };
}

export function confirmModal({ title, message, confirmLabel = 'Confirm', kind = 'primary' }) {
  return new Promise(resolve => {
    openModal({
      title, body: h('p', {}, message),
      actions: [
        { label: 'Cancel', onClick: close => { resolve(false); close(); } },
        { label: confirmLabel, kind, onClick: close => { resolve(true); close(); } },
      ],
      onClose: () => resolve(false),
    });
  });
}
