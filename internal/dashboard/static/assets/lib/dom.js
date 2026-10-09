// All server-provided content enters the document as text, never markup.
const listeners = new WeakMap();
function build(tag, attrs, children, namespace) {
  const el = namespace ? document.createElementNS(namespace, tag) : document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key.startsWith("on")) {
      if (typeof value !== "function") throw new TypeError("Listeners must be functions");
      const type = key.slice(2).toLowerCase();
      el.addEventListener(type, value);
      if (!listeners.has(el)) listeners.set(el, new Map());
      listeners.get(el).set(type, value);
    } else if (key === "style") {
      throw new TypeError("Use CSS classes or SVG geometry");
    } else if (key === "value") {
      el.value = value;
    } else {
      el.setAttribute(key === "className" ? "class" : key, value === true ? "" : String(value));
    }
  }
  for (const child of children.flat(Infinity)) {
    if (child !== null && child !== undefined && child !== false) {
      el.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
  }
  if (attrs?.value !== undefined) el.value = attrs.value;
  return el;
}
export const h = (tag, attrs, ...children) => build(tag, attrs, children);
// This is an XML namespace identifier, not an external resource URL.
export const svg = (tag, attrs, ...children) =>
  build(tag, attrs, children, "http://www.w3.org/2000/svg");
export const clear = (el) => el.replaceChildren();
// Polling patches only changed text/attributes and keeps focused nodes and open tables.
export function update(node, next) {
  if (node.nodeType !== next.nodeType || node.nodeName !== next.nodeName) {
    node.replaceWith(next);
    return;
  }
  if (node.nodeType === Node.TEXT_NODE) {
    if (node.data !== next.data) node.data = next.data;
    return;
  }
  for (const attr of [...node.attributes])
    if (!next.hasAttribute(attr.name) && !(node.tagName === "DETAILS" && attr.name === "open"))
      node.removeAttribute(attr.name);
  for (const attr of next.attributes)
    if (node.getAttribute(attr.name) !== attr.value) node.setAttribute(attr.name, attr.value);
  for (const [type, callback] of listeners.get(node) || [])
    node.removeEventListener(type, callback);
  for (const [type, callback] of listeners.get(next) || []) node.addEventListener(type, callback);
  listeners.set(node, listeners.get(next) || new Map());
  const children = [...next.childNodes];
  children.forEach((child, index) => {
    const key = child.nodeType === Node.ELEMENT_NODE ? child.getAttribute("data-key") : null;
    let current = node.childNodes[index];
    if (key && current?.getAttribute?.("data-key") !== key) {
      const found = [...node.children].find((el) => el.getAttribute("data-key") === key);
      if (found) {
        node.insertBefore(found, current || null);
        current = found;
      } else {
        node.insertBefore(child, current || null);
        return;
      }
    }
    if (!current) node.append(child);
    else update(current, child);
  });
  while (node.childNodes.length > children.length) node.lastChild.remove();
}
export const announce = (message) => {
  document.getElementById("announcer").textContent = message;
};
export function metadata(value) {
  if (value && typeof value === "object") return value;
  try {
    return JSON.parse(value || "{}");
  } catch {
    return {};
  }
}
