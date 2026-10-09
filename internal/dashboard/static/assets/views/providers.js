import { h, update } from "../lib/dom.js";
import { get, post, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { number, usd, percent, ms, utc } from "../lib/format.js";
import { progress } from "../lib/charts.js";
import {
  page,
  panel,
  pill,
  link,
  button,
  scopeLabel,
  empty,
  notice,
  note,
  tabs,
  descriptionList,
  code,
  input,
  field,
} from "../components/ui.js";
import { table } from "../components/table.js";
import { formDialog, dialog } from "../components/dialog.js";
import { credentials } from "../components/credentials.js";
function cards(data, ctx) {
  return h(
    "div",
    { class: "provider-grid" },
    data.providers.map((p) =>
      h(
        "section",
        { class: "provider-card", "data-key": p.name },
        h("div", { class: "row spread" }, h("h2", {}, p.name), pill(p.circuit.state)),
        note(p.type),
        descriptionList({
          Requests: number(p.requests),
          "Avg latency": p.requests ? ms(p.avg_latency_ms) : "No measurements",
          "Error rate": p.requests ? percent(p.error_rate) : "No measurements",
          "Recorded cost": usd(p.cost),
          "Circuit failures": number(p.circuit.failures) + " / " + number(p.circuit.threshold),
        }),
        p.circuit.open_until
          ? h(
              "p",
              { class: "circuit-cooldown small amber", "data-until": p.circuit.open_until },
              "Open until " + utc(p.circuit.open_until),
            )
          : null,
        p.circuit.last_failure ? note("Last failure " + utc(p.circuit.last_failure)) : null,
        p.circuit.state !== "closed"
          ? button(
              "Reset circuit",
              () =>
                formDialog(
                  "Reset circuit · " + p.name,
                  [
                    {
                      name: "confirm",
                      label: "Type " + p.name + " to confirm",
                      required: true,
                      help: "Resetting allows new requests to probe this upstream.",
                    },
                  ],
                  async () => {
                    await post(
                      "/api/providers/" + encodeURIComponent(p.name) + "/circuit/reset",
                      {},
                      { signal: ctx.signal },
                    );
                    ctx.refresh();
                  },
                  { confirm: p.name, submitLabel: "Reset circuit" },
                ),
              {
                disabled: !hasRole("admin"),
                icon: "refresh",
                title: hasRole("admin") ? "Reset " + p.name : "Requires admin role",
              },
            )
          : null,
      ),
    ),
  );
}
export async function providers(ctx) {
  const tab = ctx.route.query.get("tab") || "monitor";
  const root = h(
    "div",
    {},
    page(
      "Providers & Routing",
      "Inspect upstream circuits, routing strategies, and sourced pricing.",
    ),
    scopeLabel(),
    tabs(
      [
        ["monitor", "Provider health"],
        ["routing", "Routing"],
        ["catalog", "Pricing catalog"],
        ["credentials", "Provider credentials"],
      ],
      tab,
      "/providers",
    ),
  );
  if (tab === "credentials") {
    root.append(await credentials(ctx));
    return root;
  }
  const [health, config, catalog] = await Promise.all([
    get(query("/api/providers/health", { window: ctx.route.window }), { signal: ctx.signal }),
    get("/api/config", { signal: ctx.signal }),
    get("/api/catalog", { signal: ctx.signal }),
  ]);
  ctx.providers(health);
  const live = cards(health, ctx);
  root.append(h("div", { id: "poll-error" }));
  if (tab === "monitor")
    root.append(
      health.providers.length
        ? live
        : panel(
            "Provider health",
            empty(
              "No providers configured",
              "Configure a provider, then store its credentials.",
              link("Review settings", "/settings?tab=raw"),
            ),
          ),
      note("Recorded provider traffic · " + ctx.route.window + ". Runtime circuit snapshot."),
    );
  const routes = config.routes || [];
  const edit = hasRole("admin")
    ? link("Edit in Settings", "/settings?tab=raw")
    : note("Requires admin role to edit routing");
  if (tab === "monitor" || tab === "routing")
    root.append(
      panel(
        "Applied routing",
        routes.length
          ? table(
              "Effective model routing",
              [
                {
                  label: "Model / strategy",
                  render: (r) =>
                    h(
                      "div",
                      {},
                      h("strong", { class: "mono" }, r.model),
                      h("div", { class: "muted" }, r.strategy || "Not reported"),
                    ),
                },
                {
                  label: "Primary / fallback",
                  render: (r) =>
                    h(
                      "div",
                      {},
                      r.provider,
                      h(
                        "div",
                        { class: "muted" },
                        (r.fallbacks || []).join(" → ") || "No fallback",
                      ),
                    ),
                },
                {
                  label: "Retries / timeout",
                  render: (r) =>
                    h(
                      "div",
                      {},
                      number(r.max_retries) + " retries · " + r.retry_backoff,
                      h(
                        "div",
                        { class: "muted" },
                        r.timeout + " · codes " + (r.retry_codes || []).join(", "),
                      ),
                    ),
                },
                {
                  label: "Variants / shadow",
                  render: (r) =>
                    r.variants?.length
                      ? h(
                          "div",
                          { class: "budget-cell" },
                          r.variants.map((v) =>
                            h(
                              "div",
                              {},
                              v.name + " · " + v.provider + " · " + number(v.weight),
                              progress(
                                v.weight,
                                r.variants.reduce((n, item) => n + item.weight, 0),
                                v.name,
                              ),
                            ),
                          ),
                        )
                      : note(
                          r.shadow_provider
                            ? "Shadow: " + r.shadow_provider
                            : "No weighted variants",
                        ),
                },
                {
                  label: "Parameters",
                  render: (r) =>
                    button("Inspect", () =>
                      dialog("Route · " + r.model, code(JSON.stringify(r, null, 2)), {
                        drawer: true,
                      }),
                    ),
                },
              ],
              routes,
            )
          : empty(
              "No routes configured",
              "Define a model route and provider to send a request.",
              edit,
            ),
        edit,
      ),
    );
  if (tab === "catalog") {
    const list = h("div", {}),
      search = input({ type: "search", placeholder: "Filter model or provider" });
    const render = () => {
      const models = catalog.models.filter((m) =>
        (m.provider + " " + m.id).toLowerCase().includes(search.value.toLowerCase()),
      );
      list.replaceChildren(
        models.length
          ? table(
              "Sourced model prices per million tokens",
              [
                {
                  label: "Provider / model",
                  render: (m) => h("div", {}, m.provider, h("div", { class: "mono" }, m.id)),
                },
                { label: "Input / 1M", render: (m) => usd(m.input_per_million) },
                { label: "Output / 1M", render: (m) => usd(m.output_per_million) },
                {
                  label: "Source",
                  render: (m) => {
                    let url;
                    try {
                      url = new URL(m.source_url);
                    } catch {
                      return note("Source not reported");
                    }
                    return ["https:", "http:"].includes(url.protocol)
                      ? h(
                          "a",
                          {
                            href: url.href,
                            target: "_blank",
                            rel: "noreferrer noopener",
                            class: "text-link",
                          },
                          url.hostname,
                        )
                      : note("Source not reported");
                  },
                },
              ],
              models,
            )
          : empty(
              "No matching priced models",
              "Clear the filter or review pricing overrides in Settings.",
            ),
      );
    };
    search.addEventListener("input", render);
    render();
    root.append(
      panel(
        "Embedded pricing catalog",
        h(
          "div",
          { class: "stack" },
          field("Filter catalog", search),
          list,
          notice("Unknown models are Unpriced. Catalog estimates are not an invoice.", "purple"),
        ),
        pill("Verified " + (catalog.verified_on || "not reported"), "muted"),
      ),
    );
  }
  ctx.watch(async () => {
    const data = await get(query("/api/providers/health", { window: ctx.route.window }), {
      signal: ctx.signal,
    });
    if (ctx.signal.aborted) return;
    ctx.providers(data);
    document.getElementById("poll-error")?.replaceChildren();
    if (live.isConnected) update(live, cards(data, ctx));
  });
  if (ctx.signal.aborted) return root;
  const countdown = setInterval(() => {
    if (document.hidden || ctx.signal.aborted) return;
    root.querySelectorAll(".circuit-cooldown").forEach((node) => {
      const remaining = Math.max(
        0,
        Math.ceil((Date.parse(node.dataset.until) - Date.now()) / 1000),
      );
      node.textContent = remaining
        ? "Cooldown " + number(remaining) + " s · " + utc(node.dataset.until)
        : "Cooldown elapsed; waiting for a probe";
    });
  }, 1000);
  ctx.signal.addEventListener("abort", () => clearInterval(countdown), { once: true });
  return root;
}
