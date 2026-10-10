import { h } from "../lib/dom.js";
import { get, post, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { number, utc } from "../lib/format.js";
import { stackedBar } from "../lib/charts.js";
import {
  page,
  panel,
  code,
  metric,
  pill,
  link,
  button,
  scopeLabel,
  empty,
  field,
  textarea,
  input,
  notice,
  errorBox,
  note,
  tabs,
  descriptionList,
} from "../components/ui.js";
import { table } from "../components/table.js";
function replay(ctx) {
  if (!hasRole("admin")) return notice("Requires admin role", "red");
  const candidate = textarea({
    required: true,
    rows: 8,
    placeholder: "Paste the candidate policy YAML",
  });
  const baseline = textarea({ rows: 6, placeholder: "Optional baseline policy YAML" });
  const name = input({ required: true, placeholder: "Policy name" });
  const upload = input({
    type: "file",
    accept: ".json,.jsonl,application/json",
    required: true,
    class: "input file-input",
  });
  const results = h("div", { "aria-live": "polite" }),
    errors = h("div", { "aria-live": "polite" });
  const run = button("Run replay", null, {
    type: "submit",
    class: "button primary",
    icon: "playground",
  });
  const form = h(
    "form",
    {
      class: "stack",
      onSubmit: async (event) => {
        event.preventDefault();
        if (!form.reportValidity()) return;
        run.disabled = true;
        errors.replaceChildren();
        results.replaceChildren();
        try {
          const maximumBytes = 1024 * 1024;
          const file = upload.files[0];
          if (file.size > maximumBytes) throw new Error("Use a file no larger than 1 MiB.");
          const text = await file.text();
          const records = text.trim().startsWith("[")
            ? JSON.parse(text)
            : text
                .trim()
                .split(/\r?\n/)
                .filter(Boolean)
                .map((line) => JSON.parse(line));
          if (
            !Array.isArray(records) ||
            !records.length ||
            records.length > 200 ||
            records.some((r) => !r.agent || !r.tool || !r.action || !r.resource || !r.session)
          )
            throw new Error(
              "Upload 1–200 ToolRequests with agent, tool, action, resource, and session.",
            );
          const bodyFor = (policy) => ({
            policies: [{ name: name.value, body: policy }],
            requests: records,
          });
          const candidateBody = bodyFor(candidate.value);
          const baselineBody = baseline.value.trim() ? bodyFor(baseline.value) : null;
          const oversized = [candidateBody, baselineBody]
            .filter(Boolean)
            .some((body) => new TextEncoder().encode(JSON.stringify(body)).length > maximumBytes);
          if (oversized)
            throw new Error("Each replay request, including policy and contexts, must fit in 1 MiB.");
          const current = await post("/api/evals/policy", candidateBody, { signal: ctx.signal });
          const previous = baselineBody
            ? await post("/api/evals/policy", baselineBody, { signal: ctx.signal })
            : null;
          if (ctx.signal.aborted) return;
          const rows = current.results.map((r, i) => ({
            ...r,
            baseline: previous?.results[i]?.decision,
            changed: previous
              ? previous.results[i]?.decision?.allowed !== r.decision?.allowed ||
                previous.results[i]?.decision?.rule !== r.decision?.rule
              : null,
          }));
          results.replaceChildren(
            h(
              "div",
              { class: "metric-grid" },
              metric("Requests", number(current.total), "Uploaded contexts"),
              metric("Allowed", number(current.allowed), "Candidate policy"),
              metric("Denied", number(current.denied), "Candidate policy", "red"),
              metric(
                "Changed",
                previous ? number(rows.filter((r) => r.changed).length) : "—",
                previous ? "Compared with uploaded baseline" : "No baseline supplied",
              ),
            ),
            table(
              "Policy replay decisions",
              [
                {
                  label: "Context",
                  render: (r) =>
                    h(
                      "div",
                      {},
                      r.request?.tool?.name,
                      h("div", { class: "muted" }, r.request?.agent?.id),
                    ),
                },
                {
                  label: "Baseline",
                  render: (r) =>
                    r.baseline ? pill(r.baseline.allowed ? "ALLOW" : "DENY") : note("Not supplied"),
                },
                {
                  label: "Candidate",
                  render: (r) =>
                    r.decision ? pill(r.decision.allowed ? "ALLOW" : "DENY") : pill("error"),
                },
                {
                  label: "Rule / reason",
                  render: (r) =>
                    h(
                      "div",
                      {},
                      r.decision?.rule,
                      h("div", { class: "muted wrap" }, r.error || r.decision?.reason),
                      r.decision?.trace?.length
                        ? h(
                            "details",
                            { class: "inline-details" },
                            h("summary", {}, "Rule trace"),
                            code(JSON.stringify(r.decision.trace, null, 2)),
                          )
                        : null,
                    ),
                },
                {
                  label: "Diff",
                  render: (r) =>
                    r.changed == null
                      ? "—"
                      : pill(r.changed ? "Changed" : "Unchanged", r.changed ? "amber" : "muted"),
                },
              ],
              rows,
            ),
          );
        } catch (error) {
          if (error.name !== "AbortError" && error.status !== 401)
            errors.replaceChildren(errorBox(error));
        } finally {
          run.disabled = false;
        }
      },
    },
    notice(
      "Upload JSON/JSONL ToolRequest contexts; metadata lacks replay arguments. " +
      "Uses recorded identities and never invokes tools.",
    ),
    h(
      "div",
      { class: "form-row" },
      h(
        "div",
        { class: "stack" },
        field("Policy name", name),
        field("Candidate policy", candidate),
      ),
      h(
        "div",
        { class: "stack" },
        field("Baseline policy · optional", baseline),
        field(
          "Recorded tool contexts",
          upload,
          "Up to 200 records. File and each request must fit in 1 MiB, including policy bodies.",
        ),
      ),
    ),
    errors,
    run,
  );
  return h("div", { class: "stack" }, panel("Policy replay & diff", form), results);
}
export async function guardrails(ctx) {
  const tab = ctx.route.query.get("tab") || "checks";
  const root = h(
    "div",
    {},
    page(
      "Guardrails & Policy",
      "Inspect applied checks. Replay tool contexts before policy changes.",
    ),
    scopeLabel(),
    tabs(
      [
        ["checks", "Active checks"],
        ["events", "Decision feed"],
        ["replay", "Policy replay & diff"],
      ],
      tab,
      "/guardrails",
    ),
  );
  if (tab === "replay") {
    root.append(replay(ctx));
    return root;
  }
  const [config, events, summary] = await Promise.all([
    get("/api/config", { signal: ctx.signal }),
    get(query("/api/guardrails/events", { window: ctx.route.window, limit: 50 }), {
      signal: ctx.signal,
    }),
    get(query("/api/guardrails/summary", { window: ctx.route.window }), { signal: ctx.signal }),
  ]);
  const actions = config.guardrails?.actions || {};
  const counts = Object.entries(summary.by_action)
    .filter(([, n]) => n > 0)
    .map(([label, count]) => ({
      label,
      count,
      tone: label === "deny" ? "red" : label === "redact" ? "purple" : "amber",
    }));
  root.append(
    h(
      "div",
      { class: "metric-grid three" },
      metric("Matched events", number(summary.matched), "Selected period"),
      metric("Denied", number(summary.by_action.deny || 0), "Matched action: deny", "red"),
      metric("Redacted", number(summary.by_action.redact || 0), "Matched action: redact", "purple"),
    ),
  );
  if (tab === "checks")
    root.append(
      h(
        "div",
        { class: "two-col" },
        panel(
          "Applied guardrail checks",
          Object.keys(actions).length
            ? table(
                "Applied input and output checks",
                [
                  { label: "Check", render: (r) => r.type.replaceAll("_", " ") },
                  { label: "Action", render: (r) => pill(r.action) },
                  { label: "Source", render: () => note("Effective config") },
                ],
                Object.entries(actions).map(([type, action]) => ({ type, action })),
              )
            : empty(
                "No configured checks",
                "Enable guardrails in your gateway configuration.",
                link("Edit in Settings", "/settings?tab=raw"),
              ),
          link("Edit in Settings", "/settings?tab=raw"),
        ),
        panel(
          "Policy & enforcement",
          h(
            "div",
            { class: "stack" },
            descriptionList({
              "Stream handling": config.guardrails?.stream_buffer
                ? "Buffered for output checks"
                : "Unbuffered",
              "Custom rules": number(config.guardrails?.rule_count),
              "Tool rules": number(config.guardrails?.tool_rule_count),
              "LLM judges": number(config.guardrails?.llm_judge_count),
              "Moderation API": config.guardrails?.moderation_api ? "Enabled" : "Disabled",
            }),
            summary.matched
              ? stackedBar(counts, "Matched actions")
              : note("No matched actions in this period."),
            link("Open policy replay", "/guardrails?tab=replay"),
          ),
        ),
      ),
    );
  root.append(
    h(
      "div",
      { class: "spaced" },
      panel(
        "Recent policy decisions",
        events.events.length
          ? table(
              "Recent guardrail decisions",
              [
                { label: "Time · UTC", render: (e) => utc(e.timestamp, true) },
                { label: "Check", key: "type" },
                { label: "Action", render: (e) => pill(e.action) },
                { label: "Reason", key: "details", class: "wrap" },
                {
                  label: "Trace",
                  render: (e) =>
                    link(e.trace_id, "/requests/" + encodeURIComponent(e.trace_id), {
                      class: "text-link mono",
                    }),
                },
              ],
              events.events,
            )
          : empty(
              "No matched decisions",
              "Widen the window or test an enabled guardrail.",
              link("Try Playground", "/playground"),
            ),
        pill(number(events.total) + " events", "muted"),
      ),
    ),
  );
  return root;
}
