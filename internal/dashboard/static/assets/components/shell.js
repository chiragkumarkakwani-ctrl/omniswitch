import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";
import { windows } from "../lib/format.js";
import { getSession, getProviders } from "../lib/state.js";
import { link, button, pill, notice, select, note } from "./ui.js";
import { setQuery } from "../lib/router.js";
const links = [
  ["overview", "/", "Overview"],
  ["requests", "/requests", "Requests"],
  ["guardrails", "/guardrails", "Guardrails & Policy"],
  ["mcp", "/mcp", "MCP & Agents"],
  ["access", "/access", "Access & Budgets"],
  ["providers", "/providers", "Providers & Routing"],
  ["playground", "/playground", "Playground"],
  ["settings", "/settings", "Settings"],
];
let bootstrapDismissed = false;
export function brand() {
  return h(
    "div",
    { class: "brand" },
    h("span", { class: "brand-mark" }, icon("shield")),
    h("div", { class: "brand-name" }, "OmniSwitch"),
  );
}
export function updateCircuits() {
  const target = document.getElementById("circuit-summary"),
    data = getProviders();
  if (!target) return;
  if (!data) {
    target.textContent = getSession() ? "Circuit status not loaded" : "Sign-in required";
    return;
  }
  const open = data.providers.filter((p) => p.circuit?.state === "open").length,
    half = data.providers.filter((p) => p.circuit?.state === "half_open").length;
  target.replaceChildren(
    pill(
      data.providers.length
        ? open
          ? open + " open circuit" + (open === 1 ? "" : "s")
          : half
            ? half + " half-open"
            : "Circuits closed"
        : "No providers",
      open ? "amber" : half ? "amber" : data.providers.length ? "green" : "muted",
    ),
  );
}
export function shell(ctx, onSignOut) {
  const session = getSession(),
    authenticated = !!session,
    current = ctx.route.name === "trace" ? "requests" : ctx.route.name;
  const nav = h("nav", { "aria-label": "Primary" });
  for (const [name, href, label] of links) {
    if (name === "overview") nav.append(h("p", { class: "nav-group" }, "CONTROL PLANE"));
    if (name === "providers") nav.append(h("p", { class: "nav-group" }, "OPERATIONS"));
    const attrs = {
      class: "nav-link" + (current === name ? " active" : ""),
      "aria-label": authenticated ? label : label + ": sign in required",
      title: label,
    };
    const item = authenticated
      ? link("", href, { ...attrs, "aria-current": current === name ? "page" : null })
      : h("button", { ...attrs, type: "button", disabled: true });
    item.append(icon(name), h("span", { class: "nav-label" }, label));
    if (name === "mcp") item.append(h("span", { class: "nav-badge" }, "Beta"));
    nav.append(item);
  }
  const main = h("main", { class: "main", id: "main", tabindex: "-1" }),
    workspace = h("div", { class: "workspace" });
  const closeMenu = () => {
    document.body.classList.remove("menu-open");
    overlay.remove();
    workspace.inert = false;
    aside.removeAttribute("role");
    aside.removeAttribute("aria-modal");
    menu.setAttribute("aria-expanded", "false");
    menu.focus();
  };
  const overlay = h("button", {
    class: "menu-overlay",
    "aria-label": "Close navigation",
    onClick: closeMenu,
  });
  const close = button("Close navigation", closeMenu, {
    class: "icon-button menu-button",
    icon: "close",
  });
  const menu = button(
    "",
    () => {
      const opened = document.body.classList.toggle("menu-open");
      menu.setAttribute("aria-expanded", String(opened));
      if (opened) {
        document.body.append(overlay);
        workspace.inert = true;
        aside.setAttribute("role", "dialog");
        aside.setAttribute("aria-modal", "true");
        close.focus();
      } else closeMenu();
    },
    {
      icon: "menu",
      class: "icon-button menu-button",
      "aria-label": "Open navigation",
      "aria-expanded": "false",
      "aria-controls": "primary-sidebar",
    },
  );
  const keyboard = (event) => {
    if (!document.body.classList.contains("menu-open")) return;
    if (event.key === "Escape") {
      closeMenu();
      return;
    }
    if (event.key === "Tab") {
      const focus = [...aside.querySelectorAll("button:not(:disabled),a")];
      const first = focus[0],
        last = focus.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }
  };
  window.addEventListener("keydown", keyboard);
  ctx.signal.addEventListener(
    "abort",
    () => {
      window.removeEventListener("keydown", keyboard);
      overlay.remove();
      document.body.classList.remove("menu-open");
      workspace.inert = false;
    },
    { once: true },
  );
  const time = select(windows, ctx.route.window, {
    "aria-label": "Time window",
    disabled: !authenticated,
    onChange: (event) => setQuery({ window: event.target.value, offset: null }),
  });
  const circuit = h(
    "span",
    { id: "circuit-summary", class: "small muted" },
    authenticated ? "Circuit status not loaded" : "Sign-in required",
  );
  const gateway = h(
    "div",
    { class: "gateway-state", id: "gateway-state", role: "status" },
    menu,
    h("span", { class: "dot " + (authenticated ? "green" : "muted") }),
    h("strong", {}, authenticated ? "Gateway connected" : "Checking connection…"),
  );
  const header = h(
    "header",
    { class: "topbar" },
    gateway,
    h(
      "div",
      { class: "topbar-right" },
      circuit,
      authenticated
        ? h(
            "div",
            { class: "account" },
            h("span", { class: "account-name" }, session.name || "API key session"),
            pill(session.role),
          )
        : null,
      time,
      button("", ctx.refresh, {
        icon: "refresh",
        class: "icon-button",
        "aria-label": "Refresh view",
        title: "Refresh view",
        disabled: !authenticated,
      }),
      authenticated
        ? button("", onSignOut, {
            icon: "logout",
            class: "icon-button",
            "aria-label": "Sign out",
            title: "Sign out",
          })
        : null,
    ),
  );
  const banners = h("div", { class: "auth-banner" });
  if (authenticated && !session.auth_enabled)
    banners.append(
      notice(
        "Authentication is disabled. Anyone who can reach this gateway has admin " +
        "access. Enable authentication before exposing it.",
        "red",
      ),
    );
  if (session?.is_bootstrap && !bootstrapDismissed) {
    const banner = notice(
      "Bootstrap administrator. Create scoped API keys for clients, store them " +
      "safely, then rotate or revoke this bootstrap key.",
      "amber",
      h(
        "div",
        { class: "row" },
        link("Create API key", "/access"),
        button(
          "Dismiss",
          () => {
            bootstrapDismissed = true;
            banner.remove();
          },
          { class: "button subtle" },
        ),
      ),
    );
    banners.append(banner);
  }
  const aside = h(
    "aside",
    { class: "sidebar", id: "primary-sidebar", "aria-label": "Operator navigation" },
    close,
    brand(),
    h(
      "div",
      { class: "instance-card" },
      icon("server"),
      h(
        "div",
        { class: "nav-label" },
        h("strong", {}, session?.gateway?.listen || "This gateway"),
        note("Self-hosted instance"),
      ),
    ),
    nav,
    h(
      "div",
      { class: "sidebar-footer" },
      h("div", { class: "row" }, h("span", { class: "dot green" }), "Self-hosted control plane"),
      h("p", { class: "mono" }, session?.gateway?.storage || "Authentication required"),
      session?.gateway?.version ? h("p", { class: "mono" }, "v" + session.gateway.version) : null,
      h("p", {}, authenticated ? "Times shown in UTC" : "API key authentication"),
    ),
  );
  workspace.append(header, banners, main);
  return { el: h("div", { class: "app-shell" }, aside, workspace), main };
}
