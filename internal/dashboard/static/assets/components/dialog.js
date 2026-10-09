import { h, announce } from '../lib/dom.js';
import { button, field, input, select, textarea, errorBox, copyButton, note } from './ui.js';
const active = new Set();
export function dialog(title, content, options = {}) {
  const previous = document.activeElement;
  const heading = h('h2', { id: 'dialog-title-' + Math.random().toString(36).slice(2) }, title);
  const el = h('dialog', { class: 'modal' + (options.drawer ? ' drawer' : ''), 'aria-labelledby': heading.id });
  const body = h('div', { class: 'dialog-body' }, content);
  const close = () => { if (el.open) el.close(); };
  el.append(h('div', { class: 'dialog-head' }, heading, button('Close', close, { icon: 'close', class: 'icon-button', 'aria-label': 'Close ' + title })), body);
  const destroy = () => {
    options.onClose?.();
    el.querySelectorAll('input, textarea').forEach(control => { control.value = ''; });
    el.replaceChildren(); el.remove(); active.delete(el);
    if (previous?.isConnected) previous.focus();
  };
  el.addEventListener('close', destroy, { once: true });
  document.body.append(el); active.add(el); el.showModal();
  return { el, body, close };
}
export function closeDialogs() { for (const el of [...active]) el.close(); }
export function formDialog(title, fields, submit, options = {}) {
  const controls = {};
  const errors = h('div', { class: 'form-errors', 'aria-live': 'polite' });
  let busy = false;
  const form = h('form', { class: 'form-grid' });
  const save = button(options.submitLabel || 'Save', null, { type: 'submit', class: 'button primary' });
  for (const spec of fields) {
    const attrs = { name: spec.name, value: spec.value ?? '', required: spec.required, min: spec.min, max: spec.max, step: spec.step, autocomplete: 'off', spellcheck: 'false' };
    const control = spec.choices ? select(spec.choices, attrs.value, attrs) : spec.type === 'textarea' ? textarea(attrs) : input({ ...attrs, type: spec.type || 'text' });
    controls[spec.name] = control;
    form.append(field(spec.label, control, spec.help));
  }
  let modal;
  form.append(errors, h('div', { class: 'dialog-actions' }, button('Cancel', () => modal.close()), save));
  form.addEventListener('submit', async event => {
    event.preventDefault(); if (busy || !form.reportValidity()) return;
    const values = {};
    for (const spec of fields) values[spec.name] = spec.type === 'number' ? Number(controls[spec.name].value) : controls[spec.name].value;
    if (options.confirm && values.confirm !== options.confirm) { errors.replaceChildren(errorBox(new Error('Type the exact name to confirm.'))); return; }
    busy = true; save.disabled = true; save.textContent = 'Working…'; errors.replaceChildren();
    try { await submit(values, modal); if (modal.el.open) modal.close(); announce(options.success || 'Saved.'); }
    catch (error) { if (error.name !== 'AbortError' && error.status !== 401 && modal.el.isConnected) errors.replaceChildren(errorBox(error)); }
    finally { for (const spec of fields) if (spec.type === 'password') { controls[spec.name].value = ''; values[spec.name] = ''; } busy = false; save.disabled = false; save.textContent = options.submitLabel || 'Save'; }
  });
  modal = dialog(title, form, options);
  return modal;
}
export function revealKey(value) {
  let secret = value;
  const text = h('pre', { class: 'code-panel key-secret', tabindex: '0' }, secret);
  const modal = dialog('API key created', [note('Shown once. Store this key in your secret manager before closing.'), text, copyButton(() => secret, 'Copy API key'), button('I have saved this key', () => modal.close(), { class: 'button primary' })], { onClose: () => { secret = ''; text.textContent = ''; } });
}
export function confirmDelete(name, action, help) {
  return formDialog('Delete ' + name, [{ name: 'confirm', label: 'Type ' + name + ' to confirm', required: true, help }], action, { confirm: name, submitLabel: 'Delete', success: 'Deleted.' });
}
