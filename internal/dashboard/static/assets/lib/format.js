const num = new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 });
const compact = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 2 });
export const number = value => Number.isFinite(Number(value)) && value != null ? num.format(value) : '—';
export const tokens = value => value == null ? '—' : compact.format(value);
export const usd = value => value == null ? '—' : new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: Number(value) > 0 && Number(value) < 0.01 ? 6 : 2 }).format(value);
export const cost = row => row.cost_source === 'none' || !row.cost_source ? 'Unpriced' : usd(row.cost);
export const percent = value => value == null ? '—' : number(Number(value) * 100) + '%';
export const ms = value => value == null ? '—' : number(value) + ' ms';
export function utc(value, timeOnly = false) {
  if (!value || !Number.isFinite(Date.parse(value))) return '—';
  return new Intl.DateTimeFormat('en-GB', { timeZone: 'UTC', ...(timeOnly ? {} : { day: '2-digit', month: 'short', year: 'numeric' }), hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }).format(new Date(value)) + (timeOnly ? '' : ' UTC');
}
export const retentionDuration = seconds => seconds === 0 ? 'Keep forever' : seconds == null ? 'Not reported' : number(seconds / 86400) + ' days';
export const reset = seconds => seconds == null ? 'Not reported' : number(Math.max(0, seconds)) + ' seconds';
export const titleCase = value => String(value || 'Unknown').replaceAll('_', ' ').replace(/\b\w/g, c => c.toUpperCase());
export const windows = [['1h', 'Last 1 hour'], ['6h', 'Last 6 hours'], ['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']];
export const bucketFor = window => ({ '1h': '5m', '6h': '15m', '24h': '1h', '7d': '6h', '30d': '1d' })[window] || '1h';
