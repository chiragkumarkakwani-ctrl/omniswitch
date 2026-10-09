import { windows } from "./format.js";
let listener = () => {};
export function route() {
  const query = new URLSearchParams(location.search);
  if (!windows.some(([value]) => value === query.get("window"))) query.set("window", "24h");
  const match = location.pathname.match(/^\/requests\/([^/]+)$/);
  let id = "";
  if (match) {
    try {
      id = decodeURIComponent(match[1]);
    } catch {
      /* Invalid trace IDs have an empty state. */
    }
  }
  return {
    path: location.pathname,
    name: match
      ? "trace"
      : {
          "/": "overview",
          "/signin": "signin",
          "/requests": "requests",
          "/guardrails": "guardrails",
          "/mcp": "mcp",
          "/access": "access",
          "/providers": "providers",
          "/playground": "playground",
          "/settings": "settings",
        }[location.pathname] || "notfound",
    id,
    query,
    window: query.get("window"),
  };
}
export function navigate(destination, replace = false) {
  const url = new URL(destination, location.origin);
  if (url.origin !== location.origin) return;
  if (url.pathname !== "/signin" && !url.searchParams.has("window"))
    url.searchParams.set("window", route().window);
  history[replace ? "replaceState" : "pushState"]({}, "", url.pathname + url.search + url.hash);
  listener();
}
export function setQuery(values, replace = false) {
  const url = new URL(location.href);
  for (const [key, value] of Object.entries(values))
    if (value === "" || value == null) url.searchParams.delete(key);
    else url.searchParams.set(key, value);
  navigate(url.pathname + url.search, replace);
}
export function startRouter(callback) {
  listener = callback;
  window.addEventListener("popstate", listener);
  document.addEventListener("click", (event) => {
    const link = event.target.closest("a[data-route]");
    if (
      !link ||
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.altKey ||
      event.shiftKey ||
      link.target ||
      link.download
    )
      return;
    const url = new URL(link.href);
    if (url.origin !== location.origin) return;
    event.preventDefault();
    navigate(url.pathname + url.search + url.hash);
  });
}
