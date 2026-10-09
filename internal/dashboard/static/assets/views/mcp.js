import { h } from "../lib/dom.js";
import { get, query } from "../lib/api.js";
import { setQuery } from "../lib/router.js";
import { hasRole, getSession } from "../lib/state.js";
import { number, ms, utc, titleCase } from "../lib/format.js";
import {
  page,
  panel,
  pill,
  link,
  scopeLabel,
  empty,
  select,
  field,
  note,
  notice,
  descriptionList,
  tabs,
} from "../components/ui.js";
import { table } from "../components/table.js";
export async function mcp(ctx) {
  const tab = ctx.route.query.get("tab") || "mcp";
  const root = h(
    "div",
    {},
    page(
      "MCP & Agents",
      "Inspect MCP targets, tool policy, and published A2A capabilities.",
      pill("beta"),
    ),
    scopeLabel(),
    tabs(
      [
        ["mcp", "MCP tools & decisions"],
        ["a2a", "A2A capabilities"],
      ],
      tab,
      "/mcp",
    ),
  );
  if (tab === "a2a") {
    const config = await get("/api/config", { signal: ctx.signal });
    const a = config.a2a || {};
    if (!a.enabled) {
      root.append(
        empty(
          "A2A is disabled",
          "Enable A2A in the gateway config to publish an Agent Card and mapped skills.",
          link("Review settings", "/settings?tab=raw"),
          "mcp",
        ),
      );
      return root;
    }
    const card = await get("/.well-known/agent-card.json", { signal: ctx.signal }).catch((error) => {
      if (error.status === 404) return null;
      throw error;
    });
    if (!card) {
      root.append(
        empty(
          "Agent Card unavailable",
          "A2A is configured, but no Agent Card was returned by this gateway.",
          link("Review settings", "/settings"),
          "mcp",
        ),
      );
      return root;
    }
    root.append(
      panel(
        "Published A2A capabilities",
        h(
          "div",
          { class: "stack" },
          descriptionList({
            Agent: card.name,
            "Agent Card version": card.version,
            Interface: card.supportedInterfaces
              ?.map((i) => i.protocolBinding + " " + i.protocolVersion)
              .join(", "),
            Streaming: card.capabilities?.streaming ? titleCase(a.streaming_mode) : "Disabled",
            "Task lifecycle": a.task_lifecycle ? "Enabled" : "Disabled",
            "Task store": titleCase(a.task_store),
            Cancellation: titleCase(a.cancel),
            "Push notifications": card.capabilities?.pushNotifications
              ? "Published"
              : "Not published",
            "JSON-RPC methods": (a.methods || []).join(", "),
          }),
          a.task_store === "in_memory"
            ? notice("Task state is in memory and lost on restart. No durable orchestration.")
            : null,
          a.cancel === "best_effort"
            ? notice("Cancellation is best effort, not guaranteed upstream interruption.")
            : null,
          note("Streaming is SSE task events, not token generation or a downstream A2A proxy."),
          card.skills?.length
            ? table(
                "Published agent skills",
                [
                  { label: "Skill", key: "name" },
                  { label: "Description", key: "description", class: "wrap" },
                  { label: "Tags", render: (skill) => (skill.tags || []).join(", ") },
                ],
                card.skills,
              )
            : empty(
                "No published agent skills",
                "Add a mapped skill to the A2A config to publish it here.",
                link("Review settings", "/settings?tab=raw"),
                "mcp",
              ),
        ),
      ),
    );
    return root;
  }
  const [targets, decisions, keys] = await Promise.all([
    get("/api/mcp/targets", { signal: ctx.signal }),
    get(query("/api/mcp/decisions", { window: ctx.route.window, limit: 50 }), {
      signal: ctx.signal,
    }),
    hasRole("admin") ? get("/api/keys", { signal: ctx.signal }) : null,
  ]);
  const selected =
    targets.targets.find((t) => t.name === ctx.route.query.get("target")) || targets.targets[0];
  const asKey = hasRole("admin") ? ctx.route.query.get("as_key") || "" : "";
  const data = selected
    ? await get(query("/api/mcp/tools", { target: selected.name, as_key: asKey }), {
        signal: ctx.signal,
      })
    : {
        principal: {
          key_id: getSession().key_id,
          workspace_id: getSession().workspace_id,
          role: getSession().role,
        },
        tools: [],
      };
  const selector = keys
    ? field(
        "As API key",
        select([["", "Current sign-in key"], ...keys.keys.map((k) => [k.id, k.name])], asKey, {
          onChange: (e) => setQuery({ as_key: e.target.value }),
        }),
      )
    : null;
  const targetCards = targets.targets.length
    ? h(
        "div",
        { class: "target-list" },
        targets.targets.map((t) =>
          h(
            "button",
            {
              type: "button",
              class: "target-card" + (t.name === selected?.name ? " active" : ""),
              onClick: () => setQuery({ target: t.name }),
              "aria-pressed": t.name === selected?.name,
            },
            h(
              "div",
              { class: "row spread" },
              h("strong", {}, t.name),
              pill(t.connected ? "Connected" : "Disconnected", t.connected ? "green" : "amber"),
            ),
            h(
              "div",
              { class: "muted small" },
              t.transport + " · " + number(t.tool_count) + " tools",
            ),
            t.last_error ? note(t.last_error) : null,
            hasRole("admin") && t.policy_path ? note("Policy: " + t.policy_path) : null,
            t.token_exchange?.configured
              ? note("Token exchange · " + t.token_exchange.token_url_host)
              : null,
          ),
        ),
      )
    : empty(
        targets.enabled ? "No MCP targets configured" : "MCP is disabled",
        "Enable MCP and add a target in your config file.",
        link("Edit in Settings", "/settings?tab=raw"),
        "mcp",
      );
  const catalog = data.tools.length
    ? table(
        "MCP tools and dry-run policy decisions",
        [
          {
            label: "Tool",
            render: (t) =>
              h(
                "div",
                {},
                h("strong", { class: "mono" }, t.namespaced_name),
                h("div", { class: "muted tool-description" }, t.description),
              ),
          },
          { label: "List", render: (t) => pill(t.list.allowed ? "Visible" : "Hidden") },
          { label: "Call", render: (t) => pill(t.call.allowed ? "ALLOW" : "DENY") },
          {
            label: "Rule / reason",
            render: (t) =>
              h(
                "div",
                { class: "tool-description" },
                t.call.rule || t.list.rule || "Default policy",
                h("div", { class: "muted" }, t.call.reason || t.list.reason),
              ),
          },
        ],
        data.tools,
      )
    : empty(
        "No tools discovered",
        "Select a connected target and inspect its configuration or policy.",
        "",
        "mcp",
      );
  if (selector) {
    selector.classList.add("principal-selector");
    root.append(selector);
  }
  root.append(
    h(
      "p",
      { class: "scope-label" },
      "Evaluated principal: " +
        (data.principal.key_id || "No key") +
        " · " +
        data.principal.role +
        " · Workspace " +
        (data.principal.workspace_id || "unscoped"),
    ),
    h(
      "div",
      { class: "two-col mcp-layout" },
      panel("Connected targets", targetCards, pill(targets.targets.length + " targets", "muted")),
      panel(
        "Tool catalog",
        h(
          "div",
          { class: "stack" },
          catalog,
          data.tools.some((t) => t.call.evaluated_without_arguments)
            ? notice(
                "Evaluated without arguments. Argument-dependent rules can change on a real call.",
              )
            : null,
        ),
        link("Edit policy in Settings", "/settings?tab=raw"),
      ),
    ),
    h(
      "div",
      { class: "spaced" },
      panel(
        "Recent tool decisions",
        decisions.decisions.length
          ? table(
              "Recent MCP policy decisions",
              [
                { label: "Time · UTC", render: (d) => utc(d.timestamp, true) },
                {
                  label: "Agent / target",
                  render: (d) => h("div", {}, d.agent_id, h("div", { class: "muted" }, d.target)),
                },
                {
                  label: "Tool / action",
                  render: (d) =>
                    h("div", { class: "mono" }, d.tool, h("div", { class: "muted" }, d.action)),
                },
                { label: "Decision", render: (d) => pill(d.decision) },
                {
                  label: "Rule / reason",
                  render: (d) =>
                    h(
                      "div",
                      { class: "tool-description" },
                      d.rule,
                      h("div", { class: "muted" }, d.reason),
                    ),
                },
                { label: "Latency", render: (d) => ms(d.latency_ms) },
                {
                  label: "Trace",
                  render: (d) =>
                    d.trace_id
                      ? link(d.trace_id, "/requests/" + encodeURIComponent(d.trace_id), {
                          class: "text-link mono",
                        })
                      : note("Not recorded"),
                },
              ],
              decisions.decisions,
            )
          : empty(
              "No tool decisions recorded",
              "Call a configured MCP tool with a scoped key to record a decision.",
              link("Review API keys", "/access"),
            ),
      ),
    ),
    note("MCP/A2A are beta; no durable orchestration."),
    link("Inspect A2A capabilities", "/mcp?tab=a2a"),
  );
  return root;
}
