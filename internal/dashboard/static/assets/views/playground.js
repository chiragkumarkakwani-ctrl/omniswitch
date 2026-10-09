import { h, announce } from "../lib/dom.js";
import { get, post, chat, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { tokens, ms, cost, number } from "../lib/format.js";
import {
  page,
  panel,
  button,
  pill,
  link,
  empty,
  notice,
  note,
  field,
  input,
  textarea,
  select,
  errorBox,
  copyButton,
  descriptionList,
} from "../components/ui.js";
import { formDialog, dialog } from "../components/dialog.js";
const shellQuote = (value) => "'" + value.replaceAll("'", "'\\''") + "'";
export async function playground(ctx) {
  const [models, prompts, config, savedConfigs] = await Promise.all([
    get("/v1/models", { signal: ctx.signal }),
    get("/api/prompts", { signal: ctx.signal }),
    get("/api/config", { signal: ctx.signal }),
    get("/api/configs", { signal: ctx.signal }),
  ]);
  const canSend = hasRole("member"),
    model = select((models.data || []).map((m) => m.id)),
    provider = select([["", "Use configured route"], ...(config.providers?.names || [])], "");
  const preset = select([
    ["", "Use applied routing"],
    ...(savedConfigs.configs || [])
      .filter((item) => item.enabled)
      .map((item) => [item.id, item.name]),
  ]);
  const systemPrompt = textarea({
    rows: 3,
    placeholder: "Optional system instructions",
  });
  const topP = input({
    type: "number",
    min: 0,
    max: 1,
    step: "any",
    placeholder: "Upstream default",
  });
  const temperature = input({
      type: "number",
      min: 0,
      max: 2,
      step: "any",
      placeholder: "Upstream default",
    }),
    maximum = input({ type: "number", min: 1, step: 1, placeholder: "Upstream default" }),
    session = input({ placeholder: "Optional client session ID" }),
    override = input({
      type: "password",
      autocomplete: "off",
      placeholder: "Use sign-in key",
      spellcheck: "false",
    });
  const stream = input({ type: "checkbox", checked: true, "aria-label": "Stream response" });
  const composer = textarea({
      rows: 3,
      placeholder: "Enter a message for this gateway",
      required: true,
      "aria-label": "Message",
    }),
    messages = [],
    history = h("div", {
      class: "chat-messages",
      role: "log",
      "aria-label": "Conversation",
      "aria-live": "off",
    });
  const stats = h("div", { "aria-live": "polite" }),
    errors = h("div", { "aria-live": "polite" });
  let controller = null;
  let requestGeneration = 0;
  const routingHeaders = () => ({
    ...(provider.value ? { "x-omniswitch-provider": provider.value } : {}),
    ...(preset.value ? { "x-omniswitch-config": preset.value } : {}),
    ...(session.value ? { "x-omniswitch-session-id": session.value } : {}),
  });
  const requestBody = () => ({
    model: model.value,
    messages: [
      ...(systemPrompt.value ? [{ role: "system", content: systemPrompt.value }] : []),
      ...messages,
      { role: "user", content: composer.value },
    ],
    stream: stream.checked,
    ...(stream.checked ? { stream_options: { include_usage: true } } : {}),
    ...(temperature.value ? { temperature: Number(temperature.value) } : {}),
    ...(topP.value ? { top_p: Number(topP.value) } : {}),
    ...(maximum.value ? { max_tokens: Number(maximum.value) } : {}),
  });
  const send = button("Send request", null, {
    type: "submit",
    class: "button primary",
    icon: "playground",
    disabled: !canSend || !models.data?.length,
  });
  const stop = button("Stop", () => controller?.abort(), { disabled: true });
  const curl = () => {
    const headers = [
      '-H "Authorization: Bearer $OMNISWITCH_API_KEY"',
      "-H " + shellQuote("Content-Type: application/json"),
    ];
    for (const [name, value] of Object.entries(routingHeaders())) {
      headers.push("-H " + shellQuote(name + ": " + value));
    }
    return (
      "curl " +
      shellQuote(location.origin + "/v1/chat/completions") +
      " \\\n  " +
      headers.join(" \\\n  ") +
      " \\\n  -d " +
      shellQuote(JSON.stringify(requestBody()))
    );
  };
  const initial = () =>
    history.replaceChildren(
      empty(
        "Ready for a governed request",
        "Choose a configured model. Uses your sign-in key; Advanced permits a one-off key.",
        "",
        "playground",
      ),
    );
  initial();
  const form = h(
    "form",
    {
      class: "stack",
      onSubmit: async (event) => {
        event.preventDefault();
        if (!canSend || !form.reportValidity() || controller) return;
        const body = requestBody(),
          message = composer.value;
        if (!message.trim()) return;
        const generation = ++requestGeneration;
        const requestController = new AbortController();
        controller = requestController;
        const abort = () => requestController.abort();
        ctx.signal.addEventListener("abort", abort, { once: true });
        send.disabled = true;
        stop.disabled = false;
        errors.replaceChildren();
        stats.replaceChildren(note("Request in progress…"));
        if (!messages.length) history.replaceChildren();
        history.append(
          h("article", { class: "chat-message user" }, note("You"), h("pre", {}, message)),
        );
        const output = h("pre", {}, "");
        history.append(h("article", { class: "chat-message" }, note("Assistant"), output));
        let result;
        try {
          result = await chat(
            body,
            {
              signal: requestController.signal,
              headers: routingHeaders(),
              overrideKey: override.value || undefined,
            },
            (chunk) => {
              if (generation !== requestGeneration || ctx.signal.aborted) return;
              output.textContent += chunk;
              history.scrollTop = history.scrollHeight;
            },
          );
          if (ctx.signal.aborted || generation !== requestGeneration) return;
          messages.push(
            { role: "user", content: message },
            { role: "assistant", content: result.text },
          );
          composer.value = "";
          stats.replaceChildren(
            descriptionList({
              Model: result.model,
              "Input tokens": tokens(result.usage?.prompt_tokens),
              "Output tokens": tokens(result.usage?.completion_tokens),
              "Total tokens": tokens(result.usage?.total_tokens),
            }),
            result.traceID
              ? link(
                  "Open request trace · " + result.traceID,
                  "/requests/" + encodeURIComponent(result.traceID),
                  { class: "text-link mono" },
                )
              : note("The gateway did not return a trace ID."),
          );
          if (result.traceID)
            try {
              const detail = await get("/api/traces/" + encodeURIComponent(result.traceID), {
                signal: ctx.signal,
              });
              if (ctx.signal.aborted || generation !== requestGeneration) return;
              if (detail.logs?.[0])
                stats.append(
                  note(
                    "Gateway latency " +
                      ms(detail.logs[0].latency_ms) +
                      " · Cost " +
                      cost(detail.logs[0]),
                  ),
                );
            } catch (error) {
              if (ctx.signal.aborted || generation !== requestGeneration) return;
              if (error.status === 401) throw error;
              stats.append(note("Trace not available to this sign-in key yet."));
            }
          announce("Response completed.");
        } catch (error) {
          if (ctx.signal.aborted || generation !== requestGeneration) return;
          errors.replaceChildren(
            error.name === "AbortError"
              ? notice("Stream stopped. The partial response is shown.")
              : errorBox(error),
          );
          stats.replaceChildren(note("Request did not complete."));
        } finally {
          ctx.signal.removeEventListener("abort", abort);
          if (controller === requestController) {
            controller = null;
            override.value = "";
            send.disabled = !canSend || !models.data?.length;
            stop.disabled = true;
          }
        }
      },
    },
    h(
      "div",
      { class: "form-row" },
      field("Model", model),
      field("Provider override", provider),
      field("Saved routing config", preset, "Applies to this request, not the gateway config file."),
    ),
    h(
      "details",
      { class: "advanced" },
      h("summary", {}, "Advanced request controls"),
      h(
        "div",
        { class: "form-row" },
        field("System prompt · optional", systemPrompt),
        field("Temperature · optional", temperature),
        field("Top P · optional", topP),
        field("Max output tokens · optional", maximum),
        field("Client session ID · optional", session),
        field(
          "One-off API key · optional",
          override,
          "Memory only; cleared after use. Never copied into cURL.",
        ),
      ),
      h("label", { class: "inline-toggle" }, stream, "Stream response"),
    ),
    history,
    errors,
    h("div", { class: "chat-composer" }, composer, h("div", { class: "stack" }, send, stop)),
    h(
      "div",
      { class: "row" },
      copyButton(curl, "Copy as cURL"),
      button("Reset conversation", () => {
        requestGeneration++;
        controller?.abort();
        controller = null;
        send.disabled = !canSend || !models.data?.length;
        stop.disabled = true;
        messages.length = 0;
        composer.value = "";
        override.value = "";
        stats.replaceChildren();
        errors.replaceChildren();
        initial();
      }),
    ),
    stats,
  );
  ctx.signal.addEventListener(
    "abort",
    () => {
      controller?.abort();
      override.value = "";
      messages.length = 0;
    },
    { once: true },
  );
  const promptSelect = select([
    ["", "Choose a saved prompt"],
    ...(prompts.prompts || []).map((p) => [p.id, p.name + " · v" + p.version]),
  ]);
  const variables = h("div", { class: "stack" }),
    promptErrors = h("div", { "aria-live": "polite" });
  let selected = null,
    controls = {};
  const choose = (p) => {
    selected = p;
    controls = {};
    variables.replaceChildren();
    if (!p) return;
    for (const name of p.variables || []) {
      controls[name] = textarea({ rows: 3, placeholder: name });
      variables.append(field(name, controls[name]));
    }
    variables.append(note("Selected version " + number(p.version)));
  };
  promptSelect.addEventListener("change", () =>
    choose(prompts.prompts.find((p) => p.id === promptSelect.value)),
  );
  const apply = button(
    "Apply prompt",
    async (event) => {
      if (!selected) return;
      event.currentTarget.disabled = true;
      try {
        const data = await post(
          "/api/prompts/render",
          {
            prompt_id: selected.id,
            variables: Object.fromEntries(Object.entries(controls).map(([k, v]) => [k, v.value])),
          },
          { signal: ctx.signal },
        );
        composer.value = data.rendered;
        composer.focus();
        announce("Prompt applied. Send when ready.");
      } catch (error) {
        if (error.name !== "AbortError" && error.status !== 401)
          promptErrors.replaceChildren(errorBox(error));
      } finally {
        event.currentTarget.disabled = false;
      }
    },
    { disabled: !canSend },
  );
  const versions = button("Version history", async () => {
    if (!selected) return;
    try {
      const data = await get(query("/api/prompts/versions", { name: selected.name }), {
        signal: ctx.signal,
      });
      const modal = dialog(
        "Versions · " + selected.name,
        (data.prompts || []).map((p) =>
          button("Use version " + p.version, () => {
            choose(p);
            modal.close();
          }),
        ),
      );
    } catch (error) {
      if (error.name !== "AbortError" && error.status !== 401)
        promptErrors.replaceChildren(errorBox(error));
    }
  });
  const save = button(
    "Save prompt",
    () =>
      formDialog(
        "Save versioned prompt",
        [
          { name: "name", label: "Prompt name", required: true },
          {
            name: "template",
            label: "Template",
            type: "textarea",
            required: true,
            value: composer.value,
            help: "Use {{name}} variables. Reusing a name creates a version.",
          },
        ],
        async (values) => {
          const p = await post("/api/prompts", values, { signal: ctx.signal });
          prompts.prompts ||= [];
          prompts.prompts.push(p);
          promptSelect.append(h("option", { value: p.id }, p.name + " · v" + p.version));
          promptSelect.value = p.id;
          choose(p);
        },
      ),
    { disabled: !canSend, icon: "plus" },
  );
  return h(
    "div",
    {},
    page(
      "Playground",
      "Stream a gateway response and inspect its trace.",
      pill("Live request", "amber"),
    ),
    !canSend ? notice("Requires member role to send requests or save prompts.") : null,
    !models.data?.length
      ? notice(
          "No models configured. Define a model route in Settings.",
          "amber",
          link("Review settings", "/settings?tab=raw"),
        )
      : null,
    h(
      "div",
      { class: "two-col" },
      panel("Chat completion", form),
      panel(
        "Saved prompt library",
        h(
          "div",
          { class: "stack" },
          field("Saved prompt", promptSelect),
          prompts.prompts?.length
            ? null
            : empty("No saved prompts", "Save a reusable template with named variables.", save),
          variables,
          promptErrors,
          h("div", { class: "row" }, apply, versions),
          note("Rendered by the gateway; sent upstream only when you click Send request."),
        ),
        save,
      ),
    ),
  );
}
