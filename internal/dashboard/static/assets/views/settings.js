import { h } from "../lib/dom.js";
import { get, post, patch, remove, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { setQuery } from "../lib/router.js";
import { utc, number, retentionDuration, windows } from "../lib/format.js";
import {
  page,
  scopeLabel,
  panel,
  pill,
  button,
  link,
  empty,
  notice,
  note,
  field,
  input,
  textarea,
  select,
  errorBox,
  descriptionList,
  tabs,
  code,
} from "../components/ui.js";
import { table } from "../components/table.js";
import { formDialog, confirmDelete, dialog } from "../components/dialog.js";
function retention(data, ctx) {
  const policy = data.policy || {},
    last = data.last_run;
  const sweep = button(
    "Sweep now",
    () =>
      formDialog(
        "Sweep expired data",
        [
          {
            name: "confirm",
            label: "Type SWEEP to confirm",
            required: true,
            help: "Expire data past its configured retention period. This cannot be undone.",
          },
        ],
        async () => {
          await post("/api/retention/sweep", {}, { signal: ctx.signal });
          ctx.refresh();
        },
        { confirm: "SWEEP", submitLabel: "Sweep now" },
      ),
    { disabled: !hasRole("admin"), icon: "refresh" },
  );
  return panel(
    "Data retention",
    h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "retention-grid" },
        descriptionList({
          "Request metadata": retentionDuration(policy.request_logs),
          Payloads: retentionDuration(policy.payloads),
          "Guardrail events": retentionDuration(policy.guardrail_events),
          "Shadow requests": retentionDuration(policy.shadow_requests),
          "Denied & limited": retentionDuration(policy.limited_and_denied),
        }),
        descriptionList({
          "Payload logging": data.payload_logging ? "Enabled" : "Disabled",
          "Sweep interval":
            data.sweep_interval_seconds == null
              ? "Not reported"
              : number(data.sweep_interval_seconds) + " seconds",
          "Last sweep": last ? utc(last.finished_at || last.started_at) : "No sweep has run",
          "Next sweep": data.next_run ? utc(data.next_run) : "Not scheduled",
          "Payloads cleared": last ? number(last.payloads_cleared) : "No sweep yet",
        }),
      ),
      last?.deleted
        ? descriptionList(
            Object.fromEntries(
              Object.entries(last.deleted).map(([name, count]) => [
                "Deleted " + name.replaceAll("_", " "),
                number(count),
              ]),
            ),
          )
        : null,
      last?.error ? notice(last.error, "red") : null,
      notice(
        "Config-file retention: 0 keeps data forever. Retention changes require a restart. " +
          "Sweeps expire records; there is no arbitrary payload purge.",
        "purple",
      ),
    ),
    sweep,
  );
}
async function rawEditor(ctx) {
  if (!hasRole("admin")) return notice("Requires admin role", "red");
  const raw = await get("/api/config/raw", { signal: ctx.signal });
  const editor = textarea({
      class: "input mono config-editor",
      value: raw.body,
      rows: 18,
      "aria-label": "Raw configuration",
    }),
    format = select(["yaml", "json"], raw.format),
    status = h("div", { "aria-live": "polite" });
  let validated = "";
  const signature = () => JSON.stringify({ format: format.value, body: editor.value });
  const save = button(
    "Save configuration",
    async (event) => {
      if (validated !== signature()) return;
      const control = event.currentTarget;
      control.disabled = true;
      try {
        const data = await post(
          "/api/config/raw",
          { format: format.value, body: editor.value },
          { signal: ctx.signal },
        );
        status.replaceChildren(
          notice(
            data.saved
              ? "Saved to " + data.path + ". Refresh applied settings to inspect runtime state."
              : "The gateway did not confirm a save.",
            data.saved ? "green" : "amber",
          ),
        );
      } catch (error) {
        if (error.status !== 401 && error.name !== "AbortError")
          status.replaceChildren(errorBox(error));
      } finally {
        control.disabled = validated !== signature();
      }
    },
    { class: "button primary", disabled: true },
  );
  const validate = button(
    "Validate",
    async (event) => {
      const control = event.currentTarget;
      control.disabled = true;
      status.replaceChildren();
      const revision = signature();
      try {
        const data = await post(
          "/api/config/validate",
          JSON.parse(revision),
          { signal: ctx.signal },
        );
        if (signature() !== revision) {
          validated = "";
          save.disabled = true;
          status.replaceChildren(
            note("Changed while validation ran. Validate again before saving."),
          );
          return;
        }
        if (data.valid) {
          validated = revision;
          save.disabled = false;
          status.replaceChildren(
            notice("Valid configuration. You can save this revision.", "green"),
          );
        } else {
          validated = "";
          save.disabled = true;
          status.replaceChildren(
            notice(
              (data.errors || []).map((e) => "Line " + e.line + ": " + e.message).join("\n") ||
                "Configuration is invalid.",
              "red",
            ),
          );
        }
      } catch (error) {
        validated = "";
        save.disabled = true;
        if (error.status !== 401 && error.name !== "AbortError")
          status.replaceChildren(errorBox(error));
      } finally {
        control.disabled = false;
      }
    },
    { icon: "check" },
  );
  const changed = () => {
    validated = "";
    save.disabled = true;
    status.replaceChildren(note("Changed. Validate before saving."));
  };
  editor.addEventListener("input", changed);
  format.addEventListener("change", changed);
  return panel(
    "Raw gateway configuration",
    h(
      "div",
      { class: "stack" },
      notice(
        "Admin-only editor. Environment secrets stay on the host. Validate, save, " +
        "then inspect applied settings after reload.",
        "amber",
      ),
      note(raw.path),
      field("Format", format),
      editor,
      status,
      h("div", { class: "row" }, validate, save, link("Inspect applied config", "/settings")),
    ),
  );
}
async function configs(ctx) {
  const data = await get("/api/configs", { signal: ctx.signal }),
    admin = hasRole("admin"),
    errors = h("div", { "aria-live": "polite" });
  const edit = (c) =>
    formDialog(
      c ? "Edit saved config" : "Create saved config",
      [
        { name: "name", label: "Name", required: true, value: c?.name || "" },
        { name: "description", label: "Description", value: c?.description || "" },
        { name: "format", label: "Format", choices: ["yaml", "json"], value: c?.format || "yaml" },
        {
          name: "body",
          label: "Config body",
          type: "textarea",
          required: true,
          value: c?.body || "",
        },
      ],
      async (values) => {
        const check = await post(
          "/api/config/validate",
          { format: values.format, body: values.body },
          { signal: ctx.signal },
        );
        if (!check.valid)
          throw new Error(
            (check.errors || []).map((e) => "Line " + e.line + ": " + e.message).join("\n") ||
              "Invalid configuration.",
          );
        await post(
          "/api/configs",
          { ...values, ...(c ? { id: c.id, enabled: c.enabled } : { enabled: true }) },
          { signal: ctx.signal },
        );
        ctx.refresh();
      },
    );
  const toggle = async (c) => {
    try {
      await patch(
        query("/api/configs", { id: c.id }),
        { enabled: !c.enabled },
        { signal: ctx.signal },
      );
      ctx.refresh();
    } catch (error) {
      if (error.status !== 401 && error.name !== "AbortError")
        errors.replaceChildren(errorBox(error));
    }
  };
  return panel(
    "Saved configs",
    h(
      "div",
      { class: "stack" },
      errors,
      data.configs.length
        ? table(
            "Saved named configurations",
            [
              {
                label: "Name / description",
                render: (c) =>
                  h(
                    "div",
                    {},
                    h("strong", {}, c.name),
                    h("div", { class: "muted wrap" }, c.description),
                  ),
              },
              { label: "State", render: (c) => pill(c.enabled ? "Enabled" : "Disabled") },
              {
                label: "Format / updated",
                render: (c) =>
                  h("div", {}, c.format, h("div", { class: "muted" }, utc(c.updated_at))),
              },
              {
                label: "Actions",
                render: (c) =>
                  h(
                    "div",
                    { class: "row" },
                    button("Preview", () =>
                      dialog(
                        "Config · " + c.name,
                        code(c.body || "Body not available to this role."),
                        { drawer: true },
                      ),
                    ),
                    button("Edit", () => edit(c), { disabled: !admin }),
                    button(c.enabled ? "Disable" : "Enable", () => toggle(c), { disabled: !admin }),
                    button(
                      "Delete",
                      () =>
                        confirmDelete(c.name, async () => {
                          await remove(query("/api/configs", { id: c.id }), { signal: ctx.signal });
                          ctx.refresh();
                        }),
                      { disabled: !admin, class: "button danger" },
                    ),
                  ),
              },
            ],
            data.configs,
          )
        : empty(
            "No saved configurations",
            "Save a named configuration to keep an inspectable routing or evaluation preset.",
            admin ? button("Create saved config", () => edit(), { class: "button primary" }) : null,
          ),
      note("Saved presets do not replace applied config. Use the raw editor for runtime changes."),
    ),
    button("Create saved config", () => edit(), { disabled: !admin, icon: "plus" }),
  );
}
export async function settings(ctx) {
  const tab = ctx.route.query.get("tab") || "status";
  const [config, version, data] = await Promise.all([
    get("/api/config", { signal: ctx.signal }),
    get("/api/version", { signal: ctx.signal }),
    get("/api/retention", { signal: ctx.signal }),
  ]);
  const root = h(
    "div",
    {},
    page(
      "Settings",
      "Applied runtime configuration, validated file editing, and automatic data retention.",
    ),
    scopeLabel("Settings visible to your key · gateway-wide values"),
    tabs(
      [
        ["status", "Applied settings"],
        ["raw", "Raw configuration"],
        ["configs", "Saved configs"],
        ["preferences", "Console preferences"],
      ],
      tab,
      "/settings",
    ),
  );
  if (tab === "raw") {
    root.append(await rawEditor(ctx));
    return root;
  }
  if (tab === "configs") {
    root.append(await configs(ctx));
    return root;
  }
  if (tab === "preferences") {
    const contrast = input({
        type: "checkbox",
        checked: document.body.classList.contains("high-contrast"),
        onChange: (e) => document.body.classList.toggle("high-contrast", e.target.checked),
      }),
      motion = input({
        type: "checkbox",
        checked: document.body.classList.contains("reduce-motion"),
        onChange: (e) => document.body.classList.toggle("reduce-motion", e.target.checked),
      });
    root.append(
      panel(
        "Console preferences",
        h(
          "div",
          { class: "stack" },
          field(
            "Time window",
            select(windows, ctx.route.window, {
              onChange: (e) => setQuery({ window: e.target.value }),
            }),
          ),
          note("Times use UTC. The URL preserves the window for shareable filters."),
          h("label", { class: "inline-toggle" }, contrast, "Higher contrast"),
          h("label", { class: "inline-toggle" }, motion, "Additional reduced motion"),
          note(
            "Tab-only preferences. System reduced motion is honored. No cookies or local storage.",
          ),
        ),
      ),
    );
    return root;
  }
  root.append(
    h(
      "div",
      { class: "two-col equal" },
      panel(
        "Applied gateway",
        descriptionList({
          Version: version.version,
          Commit: version.commit,
          "Built at": version.built ? utc(version.built) : "Not reported",
          "Config file": config.config_path || "Not returned to this role",
          "Listen address": config.gateway?.listen || "Not reported",
          "Storage driver": config.storage?.driver || "Not reported",
          "Schema version": number(config.storage?.schema_version),
          "Pending migrations": number(config.storage?.pending_migrations),
        }),
        pill("Runtime snapshot", "muted"),
      ),
      panel(
        "Security & observability",
        h(
          "div",
          { class: "stack" },
          descriptionList({
            Authentication: config.auth_enabled ? "Enabled" : "Disabled",
            "API keys": config.identity?.api_keys_enabled ? "Enabled" : "Disabled",
            OIDC: config.identity?.oidc?.enabled ? "Enabled" : "Disabled",
            "Payload logging": config.gateway?.log_payloads ? "Enabled" : "Disabled",
            "Metrics access": config.metrics?.public ? "Public" : "Authenticated",
            "Cache scope": config.gateway?.cache_scope || "Not reported",
            "Pricing verified on": config.pricing?.catalog_verified_on || "Not reported",
          }),
          config.metrics?.public
            ? notice("Metrics are public. Restrict network exposure if the data is sensitive.")
            : null,
          note("Configuration does not verify telemetry export. Check your collector for receipt."),
        ),
      ),
    ),
    h("div", { class: "spaced" }, retention(data, ctx)),
    panel(
      "Configuration management",
      h(
        "div",
        { class: "stack" },
        note(
          "Applied runtime snapshot. Admins can edit; validation does not test " +
          "upstream connectivity.",
        ),
        h(
          "div",
          { class: "row" },
          link("Open raw editor", "/settings?tab=raw", { class: "button" }),
          link("Manage saved configs", "/settings?tab=configs", { class: "button" }),
        ),
      ),
    ),
  );
  return root;
}
