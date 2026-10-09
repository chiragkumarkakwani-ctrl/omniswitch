// This closure is the only owner of the sign-in credential and its tab storage.
let apiKey = '';
let unauthorized = () => {};
try {
  apiKey = sessionStorage.getItem('omniswitch.apiKey') || sessionStorage.getItem('omniswitch.playgroundKey') || '';
  sessionStorage.removeItem('omniswitch.playgroundKey');
  if (apiKey) sessionStorage.setItem('omniswitch.apiKey', apiKey);
} catch { /* Storage may be disabled; this tab can still use a memory-only key. */ }
export class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}
export const onUnauthorized = callback => { unauthorized = callback; };
export const hasCredential = () => !!apiKey;
export function signOut() {
  apiKey = '';
  try { sessionStorage.removeItem('omniswitch.apiKey'); sessionStorage.removeItem('omniswitch.playgroundKey'); } catch { /* No persistent fallback. */ }
}
export async function signIn(candidate, signal) {
  signOut();
  apiKey = candidate.trim();
  try {
    const session = await get('/api/session', { signal, authFailure: false });
    try { if (apiKey) sessionStorage.setItem('omniswitch.apiKey', apiKey); } catch { /* Memory only. */ }
    return session;
  } catch (error) { signOut(); throw error; }
}
async function raw(path, options = {}) {
  if (!path.startsWith('/') || path.startsWith('//')) throw new TypeError('API paths must be same-origin');
  const { data, overrideKey, authFailure = true, ...init } = options;
  const headers = new Headers(init.headers || {});
  const credential = overrideKey === undefined ? apiKey : overrideKey;
  if (credential) headers.set('Authorization', 'Bearer ' + credential);
  if (data !== undefined) headers.set('Content-Type', 'application/json');
  let response;
  try {
    response = await fetch(path, { ...init, headers, body: data === undefined ? init.body : JSON.stringify(data), credentials: 'omit', redirect: 'error', cache: 'no-store' });
  } catch (error) {
    if (error.name === 'AbortError') throw error;
    throw new APIError(0, 'Gateway unreachable. Check the connection and try again.');
  }
  if (!response.ok) {
    let body;
    try { body = await response.json(); } catch { body = null; }
    if (response.status === 401 && authFailure) { signOut(); unauthorized(); }
    const message = response.status === 403 ? 'Requires admin role' : typeof body?.error === 'string' ? body.error : body?.error?.message || 'Request failed (' + response.status + ').';
    throw new APIError(response.status, message);
  }
  return response;
}
export async function request(path, options) {
  const response = await raw(path, options);
  return response.status === 204 ? null : response.json();
}
export const get = (path, options) => request(path, options);
export const post = (path, data, options) => request(path, { ...options, method: 'POST', data });
export const patch = (path, data, options) => request(path, { ...options, method: 'PATCH', data });
export const remove = (path, options) => request(path, { ...options, method: 'DELETE' });
export function query(path, params) {
  const search = new URLSearchParams();
  for (const [name, value] of Object.entries(params || {})) if (value !== '' && value != null) search.set(name, value);
  return path + (search.size ? '?' + search : '');
}
export async function download(path, options) {
  const response = await raw(path, options);
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  const name = response.headers.get('Content-Disposition')?.match(/filename="?([^";]+)"?/)?.[1] || 'omniswitch-requests.csv';
  link.href = url; link.download = name.replaceAll('/', '-'); link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export async function chat(body, options, onChunk) {
  const started = performance.now();
  const response = await raw('/v1/chat/completions', { ...options, method: 'POST', data: body });
  const result = { traceID: response.headers.get('x-omniswitch-trace-id') || '', usage: null, text: '', model: body.model };
  if (!response.headers.get('Content-Type')?.includes('text/event-stream')) {
    const data = await response.json();
    if (data.error) throw new APIError(502, data.error.message || 'Upstream error');
    result.text = data.choices?.[0]?.message?.content || '';
    result.usage = data.usage; result.model = data.model;
    onChunk(result.text);
  } else {
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '', doneEvent = false;
    const consume = event => {
      const payload = event.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n');
      if (!payload) return;
      if (payload === '[DONE]') { doneEvent = true; return; }
      const chunk = JSON.parse(payload);
      if (chunk.error) throw new APIError(502, chunk.error.message || 'Upstream error');
      if (chunk.usage) result.usage = chunk.usage;
      if (chunk.model) result.model = chunk.model;
      const content = chunk.choices?.[0]?.delta?.content;
      if (typeof content === 'string') { result.text += content; onChunk(content); }
    };
    try {
      while (!doneEvent) {
        const { value, done } = await reader.read();
        buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
        // Normalize only complete CRLFs, including pairs split across chunks.
        buffer = buffer.replaceAll('\r\n', '\n');
        let boundary;
        while ((boundary = buffer.indexOf('\n\n')) >= 0) { consume(buffer.slice(0, boundary)); buffer = buffer.slice(boundary + 2); }
        if (done) { if (buffer.trim()) consume(buffer); break; }
      }
    } finally { await reader.cancel(); reader.releaseLock(); }
    if (!doneEvent) throw new APIError(502, 'The stream ended before completion. The partial response is shown.');
  }
  result.elapsed = performance.now() - started;
  return result;
}
