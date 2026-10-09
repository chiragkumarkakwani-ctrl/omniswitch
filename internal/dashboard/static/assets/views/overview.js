import { h, update } from "../lib/dom.js";
import { get, query } from "../lib/api.js";
import { number, tokens, usd, ms, percent, utc, bucketFor } from "../lib/format.js";
import { areaChart, stackedBar } from "../lib/charts.js";
import {
  page,
  metric,
  panel,
  pill,
  link,
  scopeLabel,
  empty,
  code,
  copyButton,
  curlExample,
  descriptionList,
  note,
} from "../components/ui.js";
import { table } from "../components/table.js";
async function load(ctx) {
  const options = { signal: ctx.signal },
    window = ctx.route.window;
  const [series, latency, summary, providers, logs] = await Promise.all([
    get(query("/api/metrics/timeseries", { window, bucket: bucketFor(window) }), options),
    get(query("/api/metrics/latency", { window }), options),
    get(query("/api/guardrails/summary", { window }), options),
    get(query("/api/providers/health", { window }), options),
    get(query("/api/logs", { window, limit: 8, sort: "timestamp_desc" }), options),
  ]);
  return { series, latency, summary, providers, logs };
}
function content(data, ctx) {
  const { series, latency, summary, logs, providers } = data,
    total = (key) => series.buckets.reduce((n, b) => n + Number(b[key] || 0), 0),
    requests = total("requests");
  const counts = Object.entries(summary.by_action)
    .filter(([, n]) => n > 0)
    .map(([label, count]) => ({
      label,
      count,
      tone: label === "deny" ? "amber" : label === "redact" ? "purple" : "muted",
    }));
  const top = summary.by_type?.reduce((a, b) => (b.count > (a?.count || 0) ? b : a), null);
  const metrics = h(
    "div",
    { class: "metric-grid six" },
    metric("Requests", number(requests), number(total("allowed")) + " allowed"),
    metric(
      "Avg. latency",
      latency.count ? ms(latency.avg_ms) : "—",
      latency.count ? "p95 " + ms(latency.p95_ms) : "No measurements",
    ),
    metric("Recorded spend", usd(total("cost")), "USD · Known pricing"),
    metric(
      "Total tokens",
      tokens(total("tokens")),
      percent(requests ? total("cache_hits") / requests : 0) + " cache hit rate",
    ),
    metric("Denied", number(total("denied")), number(total("limited")) + " limited", "amber"),
    metric("Errors", number(total("errors")), "Recorded request errors", "red"),
  );
  const traffic = requests
    ? areaChart(series.buckets)
    : empty(
        "No traffic yet",
        "Send a request through Playground or connect an existing client.",
        link("Open Playground", "/playground"),
      );
  const safety = h(
    "div",
    { class: "stack" },
    h(
      "div",
      { class: "row" },
      h("strong", { class: "value" }, number(summary.matched)),
      note("matched decisions"),
    ),
    summary.matched
      ? stackedBar(counts, "Guardrail activity")
      : note("No matched events in this period."),
    top
      ? descriptionList({
          "Most frequent check": top.type.replaceAll("_", " ") + " · " + number(top.count),
        })
      : null,
  );
  const upstreams = providers.providers.length
    ? h(
        "div",
        { class: "provider-list" },
        providers.providers.map((p) =>
          h(
            "div",
            { class: "provider-row", "data-key": p.name },
            h(
              "div",
              {},
              h("strong", {}, p.name),
              note(
                p.requests
                  ? ms(p.avg_latency_ms) + " avg · " + percent(p.error_rate) + " errors"
                  : "No traffic measured",
              ),
            ),
            pill(p.circuit.state),
          ),
        ),
      )
    : empty(
        "No providers configured",
        "Define an upstream and its credentials.",
        link("Review settings", "/settings?tab=raw"),
      );
  const recent = logs.logs.length
    ? table(
        "Recent gateway requests",
        [
          {
            label: "Time · UTC",
            render: (r) =>
              link(utc(r.timestamp, true), "/requests/" + encodeURIComponent(r.trace_id), {
                class: "text-link mono",
                "aria-label": "Open trace " + r.trace_id,
              }),
          },
          { label: "Model / provider", render: (r) => h("div", {}, r.model + " · " + r.provider) },
          { label: "Status", render: (r) => pill(r.status) },
          { label: "Latency", render: (r) => ms(r.latency_ms) },
        ],
        logs.logs.slice(0, 4),
      )
    : empty(
        "No requests recorded",
        "Send your first request; denied and limited requests also appear here.",
        link("Send a request", "/playground"),
      );
  const firstRun = !requests
    ? panel(
        "Get this gateway ready",
        h(
          "div",
          { class: "stack" },
          h(
            "ol",
            { class: "first-run-list" },
            h("li", {}, link("Create a scoped API key", "/access"), " for each client."),
            h(
              "li",
              {},
              link("Add Provider credentials", "/providers?tab=credentials"),
              " for a configured upstream.",
            ),
            h("li", {}, link("Try Playground", "/playground"), " and inspect the trace."),
          ),
          code(curlExample()),
          copyButton(curlExample(), "Copy request example"),
        ),
      )
    : null;
  return h(
    "div",
    {},
    metrics,
    h(
      "div",
      { class: "two-col overview-charts" },
      panel(
        "Request traffic",
        traffic,
        pill(series.window, "muted"),
        "UTC buckets · " + series.bucket,
      ),
      panel("Guardrail activity", safety, link("View decisions", "/guardrails")),
    ),
    h(
      "div",
      { class: "two-col overview-bottom spaced" },
      panel("Provider circuits", upstreams, link("Inspect", "/providers")),
      panel("Recent requests", recent, link("View all requests", "/requests")),
    ),
    firstRun,
  );
}
export async function overview(ctx) {
  const data = await load(ctx);
  ctx.providers(data.providers);
  const live = content(data, ctx);
  ctx.watch(async () => {
    const fresh = await load(ctx);
    if (ctx.signal.aborted) return;
    ctx.providers(fresh.providers);
    document.getElementById("poll-error")?.replaceChildren();
    update(live, content(fresh, ctx));
  });
  return h(
    "div",
    {},
    page(
      "Overview",
      "Your gateway at a glance. Traffic, policy, and upstream health.",
      link("Inspect requests", "/requests", { class: "button" }),
    ),
    scopeLabel(),
    h("div", { id: "poll-error" }),
    live,
  );
}
