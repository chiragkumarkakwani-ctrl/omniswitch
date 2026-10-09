import { h } from '../lib/dom.js';
import { button } from './ui.js';
export function table(caption, columns, rows, attrs = {}) {
  return h('div', { class: 'table-scroll', tabindex: '0', role: 'region', 'aria-label': caption }, h('table', { class: 'table', ...attrs },
    h('caption', { class: 'sr-only' }, caption),
    h('thead', {}, h('tr', {}, columns.map(column => h('th', { scope: 'col', 'aria-sort': column.sortDirection || null }, column.sort ? button(column.label, column.sort, { class: 'sort-button' }) : column.label)))),
    h('tbody', {}, rows.map((item, index) => h('tr', {}, columns.map(column => h('td', { class: column.class || '' }, column.render ? column.render(item, index) : item[column.key] ?? '—')))))));
}
