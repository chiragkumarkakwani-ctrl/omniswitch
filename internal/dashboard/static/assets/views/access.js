import { h } from "../lib/dom.js";
import { get, post, patch, remove, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { number, tokens, usd, utc, percent, reset } from "../lib/format.js";
import { progress } from "../lib/charts.js";
import {
  page,
  panel,
  metric,
  pill,
  button,
  link,
  scopeLabel,
  empty,
  notice,
  note,
  tabs,
} from "../components/ui.js";
import { table } from "../components/table.js";
import { formDialog, confirmDelete, revealKey, dialog } from "../components/dialog.js";
import { credentials } from "../components/credentials.js";
import { workspaces } from "../components/workspaces.js";
const numeric = (name, label, value = 0, help) => ({
  name,
  label,
  type: "number",
  value,
  min: 0,
  step: name.includes("usd") || name.includes("cost") ? "any" : 1,
  help,
});
function limitFields(key = {}) {
  return [
    numeric(
      "requests_per_minute",
      "Requests per minute",
      key.requests_per_minute || 0,
      "0 = unlimited",
    ),
    numeric("tokens_per_minute", "Tokens per minute", key.tokens_per_minute || 0, "0 = unlimited"),
    numeric(
      "usd_per_day",
      "USD per day",
      key.usd_per_day || 0,
      "Estimated spend is reserved and reconciled. 0 = unlimited",
    ),
  ];
}
export async function access(ctx) {
  const tab = ctx.route.query.get("tab") || "keys";
  const root = h(
    "div",
    {},
    page(
      "Access & Budgets",
      "Gateway client API keys, monthly budgets, and observable usage limits.",
    ),
    scopeLabel(),
    tabs(
      [
        ["keys", "API keys & budgets"],
        ["limits", "Usage limits"],
        ["workspaces", "Workspaces"],
        ["credentials", "Provider credentials"],
      ],
      tab,
      "/access",
    ),
  );
  if (tab === "credentials") {
    root.append(await credentials(ctx));
    return root;
  }
  if (tab === "workspaces") {
    root.append(await workspaces(ctx));
    return root;
  }
  const [data, usage, limits, config] = await Promise.all([
    get("/api/keys", { signal: ctx.signal }),
    get("/api/keys/usage?window=month", { signal: ctx.signal }),
    get("/api/limits/usage", { signal: ctx.signal }),
    get("/api/config", { signal: ctx.signal }),
  ]);
  const admin = hasRole("admin");
  const editLimits = (k) =>
    formDialog("Set key limits · " + k.name, limitFields(k), async (values) => {
      await patch(query("/api/keys", { id: k.id }), values, { signal: ctx.signal });
      ctx.refresh();
    });
  const chooseLimits = () => {
    const modal = dialog(
      "Choose API key",
      data.keys.map((k) =>
        button(
          k.name,
          () => {
            modal.close();
            editLimits(k);
          },
          { class: "button" },
        ),
      ),
    );
  };
  const create = (old) =>
    formDialog(
      old ? "Rotate API key · " + old.name : "Create API key",
      [
        {
          name: "name",
          label: "API key name",
          required: true,
          value: old ? old.name + " replacement" : "",
        },
        {
          name: "role",
          label: "Role",
          value: old?.role || "member",
          choices: ["viewer", "member", "admin", ...(hasRole("owner") ? ["owner"] : [])],
        },
        { name: "workspace_id", label: "Workspace ID · optional", value: old?.workspace_id || "" },
        numeric("expires_in", "Expires after · seconds", 0, "0 = no expiry"),
        numeric(
          "monthly_cost_budget",
          "Monthly cost budget · USD",
          old?.monthly_cost_budget || 0,
          "0 = unlimited",
        ),
        numeric(
          "monthly_token_budget",
          "Monthly token budget",
          old?.monthly_token_budget || 0,
          "0 = unlimited",
        ),
        ...limitFields(old),
      ],
      async (values, modal) => {
        const created = await post("/api/keys", values, { signal: ctx.signal });
        modal.close();
        revealKey(created.key, ctx.refresh);
        created.key = "";
      },
      {
        submitLabel: "Create key",
        success: old
          ? "Replacement created. Update clients, then revoke the old key."
          : "API key created.",
      },
    );
  const budget = (k) => {
    const u = usage.keys.find((item) => item.key_id === k.id);
    return u
      ? h(
          "div",
          { class: "budget-cell" },
          usd(u.cost_usd) + " / " + (u.cost_budget ? usd(u.cost_budget) : "Unlimited"),
          progress(
            u.cost_usd,
            u.cost_budget,
            "Monthly spend",
            u.state === "exceeded" ? "red" : u.state === "near" ? "amber" : "purple",
          ),
          h(
            "div",
            { class: "muted small" },
            tokens(u.tokens) +
              " tokens · " +
              (u.token_budget
                ? percent(u.token_pct) + " of " + tokens(u.token_budget)
                : "Unlimited tokens"),
          ),
        )
      : note("Usage not reported");
  };
  root.append(
    h(
      "div",
      { class: "metric-grid three" },
      metric(
        "Active API keys",
        number(
          data.keys.filter(
            (k) => k.enabled && (!k.expires_at || Date.parse(k.expires_at) > Date.now()),
          ).length,
        ),
        "Visible in your scope",
      ),
      metric(
        "Month-to-date spend",
        usd(usage.keys.reduce((n, k) => n + k.cost_usd, 0)),
        "Resets " + utc(usage.period.end),
      ),
      metric(
        "Keys near or over budget",
        number(usage.keys.filter((k) => k.state !== "ok").length),
        "Server-reported budget state",
      ),
    ),
  );
  if (!admin)
    root.append(
      notice("Requires admin role to manage keys or limits. Your own key and usage are visible."),
    );
  if (tab === "keys")
    root.append(
      panel(
        "Gateway API keys",
        data.keys.length
          ? table(
              "Gateway client API keys",
              [
                {
                  label: "API key",
                  render: (k) =>
                    h(
                      "div",
                      {},
                      h("strong", {}, k.name),
                      h("div", { class: "muted mono" }, k.key_prefix),
                    ),
                },
                {
                  label: "Role / workspace",
                  render: (k) =>
                    h(
                      "div",
                      {},
                      pill(k.role),
                      h("div", { class: "muted" }, k.workspace_id || "No workspace"),
                    ),
                },
                { label: "Monthly budget", render: budget },
                {
                  label: "State / last used",
                  render: (k) =>
                    h(
                      "div",
                      {},
                      pill(
                        k.expires_at && Date.parse(k.expires_at) <= Date.now()
                          ? "Expired"
                          : k.enabled
                            ? "Enabled"
                            : "Disabled",
                      ),
                      h(
                        "div",
                        { class: "muted" },
                        k.last_used_at ? utc(k.last_used_at) : "Never used",
                      ),
                    ),
                },
                {
                  label: "Actions",
                  render: (k) =>
                    h(
                      "div",
                      { class: "row" },
                      button("Set key limits", () => editLimits(k), { disabled: !admin }),
                      button(
                        "Budgets",
                        () =>
                          formDialog(
                            "Monthly budgets · " + k.name,
                            [
                              numeric(
                                "monthly_cost_budget",
                                "Monthly USD budget",
                                k.monthly_cost_budget || 0,
                                "0 = unlimited",
                              ),
                              numeric(
                                "monthly_token_budget",
                                "Monthly token budget",
                                k.monthly_token_budget || 0,
                                "0 = unlimited",
                              ),
                            ],
                            async (values) => {
                              await patch(query("/api/keys", { id: k.id }), values, {
                                signal: ctx.signal,
                              });
                              ctx.refresh();
                            },
                          ),
                        { disabled: !admin },
                      ),
                      button("Rotate", () => create(k), { disabled: !admin }),
                      button(
                        "Revoke",
                        () =>
                          confirmDelete(
                            k.name,
                            async () => {
                              await remove(query("/api/keys", { id: k.id }), {
                                signal: ctx.signal,
                              });
                              ctx.refresh();
                            },
                            "Revocation immediately blocks future requests.",
                          ),
                        { disabled: !admin, class: "button danger" },
                      ),
                    ),
                },
              ],
              data.keys,
            )
          : empty(
              "No API keys in your scope",
              "Create a scoped client key, or ask your administrator for access.",
              admin ? button("Create API key", () => create(), { class: "button primary" }) : null,
            ),
        button("Create API key", () => create(), { disabled: !admin, icon: "plus" }),
      ),
      note(
        "Rotation shows a replacement once. Store it and update clients before " +
        "revoking the old key. 0 = unlimited.",
      ),
    );
  const policies = limits.policies || [];
  root.append(
    panel(
      "Usage limits",
      policies.length
        ? table(
            "Observed limit usage",
            [
              {
                label: "Limit / source",
                render: (p) =>
                  h("div", {}, p.name, h("div", { class: "muted" }, p.scope + " · " + p.source)),
              },
              {
                label: "Usage",
                render: (p) =>
                  h(
                    "div",
                    { class: "budget-cell" },
                    number(p.used) +
                      " / " +
                      (p.limit ? number(p.limit) : "Unlimited") +
                      " " +
                      p.unit,
                    progress(
                      p.used,
                      p.limit,
                      p.name,
                      p.limit && p.used / p.limit >= 0.8 ? "amber" : "purple",
                    ),
                  ),
              },
              { label: "Remaining", render: (p) => number(p.remaining) + " " + p.unit },
              {
                label: "Window / reset",
                render: (p) =>
                  h(
                    "div",
                    {},
                    p.window,
                    h("div", { class: "muted" }, "Resets in " + reset(p.resets_in_seconds)),
                  ),
              },
            ],
            policies,
          )
        : empty(
            "No active usage limits reported",
            "Set per-key limits or define scoped config-file policies.",
            admin ? button("Set key limits", chooseLimits, { disabled: !data.keys.length }) : null,
          ),
      button("Set key limits", chooseLimits, { disabled: !admin || !data.keys.length }),
    ),
    panel(
      "Config-file policies",
      (config.rate_limit?.policies || []).length
        ? table(
            "Configured scoped limit policies",
            [
              { label: "Name", key: "name" },
              { label: "Scope", key: "scope" },
              { label: "Limit", render: (p) => number(p.limit) + " " + p.unit },
              { label: "Window", key: "window" },
              {
                label: "Match",
                render: (policy) =>
                  typeof policy.match === "string"
                    ? policy.match
                    : Object.entries(policy.match || {})
                        .map(([name, value]) => name + "=" + value)
                        .join(", ") || "All within scope",
              },
              { label: "Management", render: () => note("Defined in config file") },
            ],
            config.rate_limit.policies,
          )
        : note("No scoped policies are defined in the effective configuration."),
      link("Edit in Settings", "/settings?tab=raw"),
    ),
    note(
      "Admins edit per-key limits. Workspace policies are read-only here and " +
      "hot-reloaded from config. In-flight token/spend usage can be estimated.",
    ),
  );
  return root;
}
