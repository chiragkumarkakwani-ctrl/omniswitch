import { h } from "../lib/dom.js";
import { get, query, download } from "../lib/api.js";
import { setQuery } from "../lib/router.js";
import { hasRole } from "../lib/state.js";
import { utc, ms, tokens, cost, number } from "../lib/format.js";
import {
  page,
  panel,
  pill,
  link,
  button,
  scopeLabel,
  empty,
  field,
  select,
  input,
  note,
  actionError,
} from "../components/ui.js";
import { table } from "../components/table.js";
export async function requests(ctx) {
  const q = ctx.route.query;
  const params = Object.fromEntries(
    [
      "window",
      "since",
      "until",
      "provider",
      "model",
      "endpoint",
      "status",
      "api_key_id",
      "trace_id",
      "session_id",
      "decision",
      "cached",
      "q",
      "sort",
    ].map((name) => [name, q.get(name) || ""]),
  );
  if (!hasRole("admin")) delete params.api_key_id;
  const limit = [50, 100, 500].includes(Number(q.get("limit"))) ? Number(q.get("limit")) : 50;
  const offset = Math.max(0, Number(q.get("offset")) || 0);
  const [data, facets, keys] = await Promise.all([
    get(query("/api/logs", { ...params, limit, offset }), { signal: ctx.signal }),
    get("/api/logs/facets", { signal: ctx.signal }),
    hasRole("admin") ? get("/api/keys", { signal: ctx.signal }) : Promise.resolve(null),
  ]);
  const filters = h("form", { class: "filters" });
  const controls = {};
  const addSelect = (name, label, options) => {
    controls[name] = select([["", "All " + label.toLowerCase()], ...options], params[name]);
    filters.append(field(label, controls[name]));
  };
  const search = input({
    type: "search",
    value: params.q,
    placeholder: "Trace, session, model, or request ID",
    "aria-label": "Search request IDs, trace IDs, session IDs, model names",
  });
  controls.q = search;
  const searchField = field("Search", search);
  searchField.classList.add("search");
  filters.append(searchField);
  addSelect("provider", "Providers", facets.providers);
  addSelect("model", "Models", facets.models);
  addSelect("endpoint", "Endpoints", facets.endpoints);
  addSelect("status", "Statuses", facets.statuses);
  if (keys)
    addSelect(
      "api_key_id",
      "API keys",
      keys.keys.map((k) => [k.id, k.name]),
    );
  const advanced = h("div", { class: "filter-details stack" });
  for (const [name, label] of [
    ["trace_id", "Trace ID"],
    ["session_id", "Session ID"],
    ["since", "Since · UTC (ISO timestamp)"],
    ["until", "Until · UTC (ISO timestamp)"],
  ]) {
    controls[name] = input({
      value: params[name],
      placeholder: name === "since" || name === "until" ? "YYYY-MM-DDTHH:mm:ssZ" : "",
    });
    advanced.append(field(label, controls[name]));
  }
  controls.decision = select(
    [
      ["", "All decisions"],
      ["ALLOW", "Allow"],
      ["DENY", "Deny"],
    ],
    params.decision,
  );
  controls.cached = select(
    [
      ["", "All cache states"],
      ["true", "Cached"],
      ["false", "Not cached"],
    ],
    params.cached,
  );
  advanced.append(field("Decision", controls.decision), field("Cache", controls.cached));
  filters.append(
    h("details", {}, h("summary", {}, "More filters"), advanced),
    button("Apply filters", null, { type: "submit", icon: "filter" }),
    button("Reset", () =>
      setQuery(
        Object.fromEntries(
          Object.keys(params)
            .filter((k) => k !== "window")
            .map((k) => [k, null]),
        ),
      ),
    ),
  );
  filters.addEventListener("submit", (event) => {
    event.preventDefault();
    setQuery({
      ...Object.fromEntries(
        Object.entries(controls).map(([name, control]) => [name, control.value.trim()]),
      ),
      offset: null,
    });
  });
  const errors = h("div", { "aria-live": "polite" });
  const exportButton = hasRole("admin")
    ? button(
        "Export CSV",
        async (event) => {
          const control = event.currentTarget;
          control.disabled = true;
          try {
            await download(query("/api/logs/export", { ...params, format: "csv" }), {
              signal: ctx.signal,
            });
          } catch (error) {
            actionError(errors, error);
          } finally {
            control.disabled = false;
          }
        },
        { icon: "download" },
      )
    : null;
  const columns = [
    {
      label: "Time · UTC",
      render: (r) => utc(r.timestamp, true),
      class: "mono",
      sort: () =>
        setQuery({
          sort: params.sort === "timestamp_asc" ? "timestamp_desc" : "timestamp_asc",
          offset: null,
        }),
      sortDirection: params.sort === "timestamp_asc" ? "ascending" : "descending",
    },
    {
      label: "Trace / session",
      render: (r) =>
        h(
          "div",
          {},
          r.trace_id
            ? link(r.trace_id, "/requests/" + encodeURIComponent(r.trace_id), {
                class: "text-link mono",
              })
            : note("Not recorded"),
          h("div", { class: "muted mono" }, r.session_id || "No session"),
        ),
    },
    {
      label: "Provider / model",
      render: (r) => h("div", {}, r.provider, h("div", { class: "muted mono" }, r.model)),
    },
    { label: "Endpoint", key: "endpoint" },
    { label: "Status", render: (r) => pill(r.status) },
    { label: "Tokens", render: (r) => tokens(r.total_tokens) },
    {
      label: "Latency",
      render: (r) => ms(r.latency_ms),
      sort: () =>
        setQuery({
          sort: params.sort === "latency_desc" ? "latency_asc" : "latency_desc",
          offset: null,
        }),
    },
    {
      label: "Cost",
      render: (r) => cost(r),
      sort: () =>
        setQuery({ sort: params.sort === "cost_desc" ? "cost_asc" : "cost_desc", offset: null }),
    },
    { label: "Cache", render: (r) => (r.cached ? pill("cached") : note("—")) },
  ];
  const rows = data.logs.length
    ? table("Filtered gateway requests", columns, data.logs)
    : empty(
        "No matching requests",
        "Widen the window or reset filters. Denied/limited traffic is recorded here.",
        button("Reset filters", () =>
          setQuery(
            Object.fromEntries(
              Object.keys(params)
                .filter((k) => k !== "window")
                .map((k) => [k, null]),
            ),
          ),
        ),
      );
  const pagination = h(
    "div",
    { class: "pagination" },
    h(
      "span",
      {},
      data.logs.length
        ? number(offset + 1) +
            "–" +
            number(offset + data.logs.length) +
            " of " +
            number(data.total) +
            " requests"
        : "No results · " + number(data.total) + " recorded matches",
    ),
    h(
      "div",
      { class: "row" },
      select(
        [50, 100, 500].map((n) => [n, n + " per page"]),
        limit,
        {
          "aria-label": "Rows per page",
          onChange: (event) => setQuery({ limit: event.target.value, offset: null }),
        },
      ),
      button("Previous", () => setQuery({ offset: Math.max(0, offset - limit) }), {
        disabled: offset === 0,
      }),
      button("Next", () => setQuery({ offset: offset + limit }), {
        disabled: offset + data.logs.length >= data.total,
      }),
    ),
  );
  return h(
    "div",
    {},
    page(
      "Requests",
      "Search traffic and inspect routing, guardrails, and cost in a trace.",
      exportButton,
    ),
    scopeLabel(),
    filters,
    errors,
    panel("Request log", rows, pill(ctx.route.window, "muted")),
    pagination,
    hasRole("admin")
      ? note("CSV: 10,000-row cap, no payloads. Formula-prefixed cells are escaped by the server.")
      : null,
  );
}
