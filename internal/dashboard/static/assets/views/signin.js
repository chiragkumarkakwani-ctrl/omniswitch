import { h } from "../lib/dom.js";
import { get, signIn } from "../lib/api.js";
import { shell } from "../components/shell.js";
import { button, field, input, notice, errorBox, note, code } from "../components/ui.js";
export function signin(ctx, message, onSuccess) {
  const layout = shell(ctx),
    errors = h(
      "div",
      { "aria-live": "polite" },
      message ? notice(message, message.includes("ended") ? "amber" : "green") : null,
    );
  const key = input({
    type: "password",
    name: "api-key",
    placeholder: "Enter your gateway API key",
    required: true,
    autocomplete: "off",
    autocapitalize: "none",
    spellcheck: "false",
    "aria-label": "Gateway API key",
  });
  const reveal = button(
    "",
    () => {
      const shown = key.type === "password";
      key.type = shown ? "text" : "password";
      reveal.setAttribute("aria-pressed", String(shown));
      reveal.setAttribute("aria-label", shown ? "Hide API key" : "Show API key");
    },
    { icon: "eye", class: "icon-button", "aria-label": "Show API key", "aria-pressed": "false" },
  );
  const submit = button("Sign in to console", null, {
    type: "submit",
    class: "button primary",
    icon: "arrow",
  });
  let busy = false;
  const form = h(
    "form",
    {
      class: "panel signin-form form-grid",
      onSubmit: async (event) => {
        event.preventDefault();
        if (busy || !form.reportValidity()) return;
        busy = true;
        submit.disabled = true;
        submit.textContent = "Validating key…";
        errors.replaceChildren();
        try {
          const session = await signIn(key.value, ctx.signal);
          if (!ctx.signal.aborted) {
            key.value = "";
            onSuccess(session);
          }
        } catch (error) {
          if (error.name !== "AbortError") {
            errors.replaceChildren(
              error.status === 401
                ? notice("This API key was not accepted. Check it and try again.", "red")
                : errorBox(error),
            );
            key.focus();
          }
        } finally {
          busy = false;
          submit.disabled = false;
          submit.textContent = "Sign in to console";
        }
      },
    },
    errors,
    field(
      "Gateway API key",
      h("div", { class: "password-wrap" }, key, reveal),
      "Use a viewer, member, admin, or owner key. Provider credentials cannot be used to sign in.",
    ),
    submit,
    notice(
      "Your key stays in this tab’s session storage. It is never put in the URL or " +
      "local storage. Sign out on shared devices.",
      "purple",
    ),
  );
  form.querySelector("label").htmlFor = key.id = "signin-api-key";
  key.setAttribute(
    "aria-describedby",
    form.querySelector(".password-wrap").getAttribute("aria-describedby"),
  );
  ctx.signal.addEventListener(
    "abort",
    () => {
      key.value = "";
    },
    { once: true },
  );
  const status = layout.el.querySelector("#gateway-state strong"),
    dot = layout.el.querySelector("#gateway-state .dot");
  get("/api/health", { signal: ctx.signal, authFailure: false })
    .then((data) => {
      if (ctx.signal.aborted) return;
      status.textContent = "Gateway reachable · " + data.status;
      dot.className = "dot green";
    })
    .catch((error) => {
      if (error.name !== "AbortError") {
        status.textContent = "Gateway unreachable";
        dot.className = "dot red";
      }
    });
  const steps = [
    [
      "Set your bootstrap key",
      "Supply a strong secret through your deployment’s secret manager.",
      code("OMNISWITCH_BOOTSTRAP_API_KEY"),
    ],
    [
      "Enable authentication",
      "Set OMNISWITCH_AUTH=true and start the gateway. The bootstrap key receives the owner role.",
    ],
    [
      "Sign in, then issue scoped keys",
      "Use the bootstrap key here. Create separate client keys in Access & Budgets," +
      " then rotate or revoke the bootstrap key.",
    ],
  ];
  layout.main.append(
    h(
      "div",
      { class: "signin-wrap" },
      h(
        "section",
        {},
        h("p", { class: "eyebrow" }, "YOUR INSTANCE. YOUR CONTROL."),
        h("h1", { class: "signin-title" }, "Sign in to OmniSwitch"),
        h(
          "p",
          { class: "muted signin-intro" },
          "Connect to your self-hosted control plane to inspect traffic, enforce " +
          "policy, and manage access.",
        ),
        form,
        note("Connects directly to the gateway serving this page."),
      ),
      h(
        "aside",
        { class: "first-run" },
        h("h2", {}, "First time running OmniSwitch?"),
        note("Bootstrap access on the host. No cloud account is needed."),
        h(
          "ol",
          { class: "first-run-list" },
          steps.map(([title, text, extra]) =>
            h("li", {}, h("strong", {}, title), note(text), extra),
          ),
        ),
        note("Already have an instance? Ask your operator for a least-privilege API key."),
      ),
    ),
  );
  return layout.el;
}
