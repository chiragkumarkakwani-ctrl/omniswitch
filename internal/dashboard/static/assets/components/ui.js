import { h, announce } from "../lib/dom.js";
import { icon } from "../lib/icons.js";
import { titleCase } from "../lib/format.js";
import { hasRole, getSession } from "../lib/state.js";
let fieldID = 0;
export const button = (label, onClick, attrs = {}) =>
  h(
    "button",
    { type: "button", class: "button", onClick, ...attrs },
    attrs.icon ? icon(attrs.icon) : null,
    label,
  );
export const link = (label, to, attrs = {}) =>
  h("a", { href: to, "data-route": true, class: "text-link", ...attrs }, label);
export const row = (...children) => h("div", { class: "row" }, children);
export const note = (text) => h("p", { class: "muted small" }, text);
export function page(title, description, action) {
  return h(
    "div",
    { class: "page-head" },
    h(
      "div",
      {},
      h("h1", { tabindex: "-1", id: "view-title" }, title),
      h("p", { class: "muted" }, description),
    ),
    action,
  );
}
export function panel(title, content, action, subtitle, attrs = {}) {
  return h(
    "section",
    { class: "panel", ...attrs },
    h(
      "div",
      { class: "panel-head" },
      h("div", {}, h("h2", {}, title), subtitle ? note(subtitle) : null),
      action,
    ),
    h("div", { class: "panel-body" }, content),
  );
}
export function metric(label, value, detail, tone = "", symbol) {
  return h(
    "div",
    { class: "metric " + tone },
    h("div", { class: "metric-label" }, label, symbol ? icon(symbol) : null),
    h("div", { class: "value" }, value),
    h("div", { class: "muted small" }, detail),
  );
}
export function pill(value, tone) {
  const normalized = String(value || "unknown")
    .toLowerCase()
    .replaceAll("_", "-");
  const colors = {
    success: "green",
    allow: "green",
    ok: "green",
    closed: "green",
    active: "green",
    connected: "green",
    enabled: "green",
    visible: "green",
    published: "green",
    error: "red",
    denied: "red",
    deny: "red",
    open: "red",
    exceeded: "red",
    hidden: "red",
    limited: "amber",
    warn: "amber",
    warning: "amber",
    near: "amber",
    "half-open": "amber",
    redact: "purple",
    cached: "purple",
    disabled: "muted",
    expired: "amber",
    beta: "amber",
  };
  return h(
    "span",
    { class: "pill " + (tone || colors[normalized] || "muted") },
    h("span", { class: "dot", "aria-hidden": "true" }),
    titleCase(value),
  );
}
export function notice(message, tone = "amber", action) {
  return h(
    "div",
    { class: "notice " + tone, role: tone === "red" ? "alert" : "status" },
    icon(tone === "red" ? "alert" : "shield"),
    h("div", {}, message),
    action,
  );
}
export const errorBox = (error) =>
  notice(
    error.status === 403 ? "Requires admin role" : error.message || "Unable to load this view.",
    "red",
  );
export function empty(title, text, action, symbol = "requests") {
  return h(
    "div",
    { class: "empty-state" },
    icon(symbol),
    h("h3", {}, title),
    h("p", { class: "muted" }, text),
    action,
  );
}
export function field(label, input, help) {
  if (!input.id) input.id = "field-" + ++fieldID;
  const helpID = input.id + "-help";
  if (help) input.setAttribute("aria-describedby", helpID);
  return h(
    "div",
    { class: "field" },
    h("label", { for: input.id }, label),
    input,
    help ? h("p", { id: helpID, class: "muted small" }, help) : null,
  );
}
// An omitted value keeps the browser's first-option default. Explicit "" is
// reserved for a blank option, not an accidental empty model or role selection.
export function select(options, value, attrs = {}) {
  return h(
    "select",
    { class: "input", ...attrs, value },
    options.map((option) => {
      const [id, label] = Array.isArray(option) ? option : [option, option];
      return h("option", { value: id }, label);
    }),
  );
}
export const input = (attrs = {}) => h("input", { class: "input", ...attrs });
export const textarea = (attrs = {}) =>
  h("textarea", { class: "input mono", rows: 6, spellcheck: "false", ...attrs });
export const adminHint = () => (hasRole("admin") ? null : note("Requires admin role"));
export const scopeLabel = (message = "Showing data for your key") =>
  hasRole("admin")
    ? null
    : h(
        "p",
        { class: "scope-label" },
        icon("lock"),
        message,
        getSession()?.name ? " · " + getSession().name : "",
      );
export async function copy(value, control) {
  try {
    if (navigator.clipboard) await navigator.clipboard.writeText(value);
    else {
      const text = textarea({ class: "sr-only", value });
      document.body.append(text);
      text.select();
      const copied = document.execCommand("copy");
      text.remove();
      if (!copied) throw new Error("Clipboard unavailable");
    }
    announce("Copied to clipboard.");
    if (control) {
      const before = control.textContent;
      control.textContent = "Copied";
      setTimeout(() => {
        if (control.isConnected) control.textContent = before;
      }, 1500);
    }
  } catch {
    announce("Clipboard unavailable. Select and copy the text instead.");
  }
}
export const copyButton = (value, label = "Copy") =>
  button(
    label,
    (event) => copy(typeof value === "function" ? value() : value, event.currentTarget),
    { icon: "copy" },
  );
export const code = (value) => h("pre", { class: "code-panel", tabindex: "0" }, value);
export function curlExample(model = "") {
  return (
    'curl "' +
    location.origin +
    '/v1/chat/completions" \\\n' +
    '  -H "Authorization: Bearer $OMNISWITCH_API_KEY" \\\n' +
    '  -H "Content-Type: application/json" \\\n  -d ' +
    JSON.stringify(
      JSON.stringify({
        model: model || "<configured-model>",
        messages: [{ role: "user", content: "Hello" }],
      }),
    )
  );
}
export function tabs(items, current, path) {
  return h(
    "nav",
    { class: "tabs", "aria-label": "View sections" },
    items.map(([id, label]) =>
      link(label, path + "?tab=" + id, {
        class: "tab" + (id === current ? " active" : ""),
        "aria-current": id === current ? "page" : null,
      }),
    ),
  );
}
export function descriptionList(values) {
  return h(
    "dl",
    { class: "kv" },
    Object.entries(values).map(([label, value]) => [
      h("dt", {}, label),
      h("dd", {}, value ?? "Not reported"),
    ]),
  );
}
export function actionError(target, error) {
  if (error.name !== "AbortError" && error.status !== 401 && target.isConnected)
    target.replaceChildren(errorBox(error));
}
