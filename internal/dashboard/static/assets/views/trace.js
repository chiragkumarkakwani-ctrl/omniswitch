import { h, metadata } from "../lib/dom.js";
import { get, post } from "../lib/api.js";
import { getSession, hasRole } from "../lib/state.js";
import { tokens, ms, cost, utc, number } from "../lib/format.js";
import { waterfall } from "../lib/charts.js";
import {
  page,
  panel,
  metric,
  pill,
  link,
  button,
  scopeLabel,
  empty,
  notice,
  code,
  descriptionList,
  note,
} from "../components/ui.js";
import { table } from "../components/table.js";
import { formDialog } from "../components/dialog.js";
export async function trace(ctx) {
  const data = await get("/api/traces/" + encodeURIComponent(ctx.route.id), {
    signal: ctx.signal,
  }).catch((error) => {
    if (error.status === 404) return { logs: [] };
    throw error;
  });
  const log = data.logs?.[0];
  if (!log)
    return h(
      "div",
      {},
      page(
        "Trace not found",
        "Missing, expired, or outside your key’s scope.",
        link("Back to Requests", "/requests"),
      ),
      scopeLabel(),
      empty(
        "No visible trace data",
        "Open a recent request or try Playground.",
        link("Open Requests", "/requests"),
      ),
    );
  const meta = metadata(log.metadata);
  const recordedSpans = data.spans || meta._spans;
  const spans = (Array.isArray(recordedSpans) ? recordedSpans : [])
    .filter(
      (span) =>
        span &&
        typeof span.name === "string" &&
        Number.isFinite(Date.parse(span.start)) &&
        typeof span.duration_ms === "number" &&
        span.duration_ms >= 0,
    )
    .map((span) => ({ ...span, metadata: metadata(span.metadata) }))
    .sort((a, b) => Date.parse(a.start) - Date.parse(b.start));
  const inspector = h(
    "div",
    { class: "stack" },
    note("Select a span for recorded routing and retry details."),
  );
  const inspect = (s) =>
    inspector.replaceChildren(
      descriptionList({
        Span: s.name,
        Provider: s.provider || "Gateway",
        Start: utc(s.start),
        Duration: ms(s.duration_ms),
        Status: pill(s.status),
      }),
      code(JSON.stringify(s.metadata, null, 2)),
    );
  if (spans.length) inspect(spans[0]);
  const feedback = hasRole("member")
    ? button(
        "Add feedback",
        () =>
          formDialog(
            "Trace feedback",
            [
              {
                name: "rating",
                label: "Rating",
                value: "neutral",
                choices: [
                  ["good", "Good"],
                  ["neutral", "Neutral"],
                  ["bad", "Bad"],
                ],
              },
              { name: "comment", label: "Comment", type: "textarea" },
            ],
            async (values) => {
              await post(
                "/api/feedback",
                {
                  trace_id: data.trace_id,
                  rating: values.rating,
                  score: { good: 1, neutral: 0, bad: -1 }[values.rating],
                  comment: values.comment,
                },
                { signal: ctx.signal },
              );
              ctx.refresh();
            },
          ),
        { icon: "plus" },
      )
    : null;
  const events = data.guardrail_events || [];
  const eventTable = events.length
    ? table(
        "Trace guardrail events",
        [
          { label: "Time · UTC", render: (e) => utc(e.timestamp, true) },
          { label: "Check", key: "type" },
          { label: "Action", render: (e) => pill(e.action) },
          { label: "Matched", render: (e) => (e.matched ? "Yes" : "No") },
          { label: "Details", key: "details", class: "wrap" },
        ],
        events,
      )
    : note("No guardrail events recorded for this trace.");
  const payloads = getSession().gateway?.log_payloads;
  const requestPayload = log.request_payload || meta.request_body || meta.request;
  const responsePayload = log.response_payload || meta.response_body || meta.response;
  const payloadPanel = !payloads
    ? notice(
        "Payloads are not logged. Enable log_payloads only if privacy and retention" +
        " permit. Old requests cannot be reconstructed.",
        "amber",
        link("Review settings", "/settings"),
      )
    : panel(
        "Logged payloads",
        requestPayload || responsePayload
          ? h(
              "details",
              {},
              h("summary", {}, "Show logged request and response"),
              code(JSON.stringify({ request: requestPayload, response: responsePayload }, null, 2)),
            )
          : note("No payload was stored for this trace."),
      );
  return h(
    "div",
    {},
    page(
      "Request trace",
      "Follow policy, routing, and recorded upstream attempts.",
      link("Back to Requests", "/requests"),
    ),
    scopeLabel(),
    h(
      "div",
      { class: "row" },
      pill(log.status),
      h("span", { class: "mono" }, data.trace_id),
      note(utc(log.timestamp)),
    ),
    h(
      "div",
      { class: "metric-grid" },
      metric("Model", log.model, log.provider),
      metric(
        "Tokens",
        tokens(log.total_tokens),
        number(log.input_tokens) + " input · " + number(log.output_tokens) + " output",
      ),
      metric(
        "End-to-end latency",
        ms(log.latency_ms),
        log.cached ? "Served from cache" : "Recorded gateway duration",
      ),
      metric("Cost", cost(log), "Source: " + (log.cost_source || "none")),
    ),
    h(
      "div",
      { class: "two-col" },
      panel(
        "Trace waterfall",
        spans.length
          ? waterfall(spans, inspect)
          : empty(
              "No span timings recorded",
              "New instrumented requests include span timings.",
              link("Send a request", "/playground"),
            ),
        pill(spans.length + " spans", "muted"),
        "Timings are recorded, not inferred",
      ),
      panel("Span inspector", inspector),
    ),
    h("div", { class: "spaced" }, payloadPanel),
    panel("Guardrail events", eventTable),
    panel(
      "Shadow requests",
      data.shadow_logs?.length
        ? table(
            "Shadow request comparison",
            [
              { label: "Provider", key: "provider" },
              { label: "Model", key: "model" },
              { label: "Status", render: (s) => pill(s.status) },
              { label: "Latency", render: (s) => ms(s.latency_ms) },
              { label: "Cost", render: (s) => cost(s) },
            ],
            data.shadow_logs,
          )
        : note("No shadow request. Configure shadow routing in Settings."),
    ),
    panel(
      "Feedback",
      data.feedback?.length
        ? table(
            "Trace feedback",
            [
              { label: "Time · UTC", render: (f) => utc(f.timestamp, true) },
              { label: "Rating", key: "rating" },
              { label: "Comment", key: "comment", class: "wrap" },
            ],
            data.feedback,
          )
        : note("No feedback yet. Members can add a rating and comment."),
      feedback,
    ),
  );
}
