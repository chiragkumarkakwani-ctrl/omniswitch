import { h, announce } from "./lib/dom.js";
import { get, signOut, hasCredential, onUnauthorized } from "./lib/api.js";
import { route, navigate, startRouter } from "./lib/router.js";
import { getSession, setSession, setProviders } from "./lib/state.js";
import { shell, updateCircuits } from "./components/shell.js";
import { page, errorBox, button, empty } from "./components/ui.js";
import { closeDialogs } from "./components/dialog.js";
import { signin } from "./views/signin.js";
import { overview } from "./views/overview.js";
import { requests } from "./views/requests.js";
import { trace } from "./views/trace.js";
import { guardrails } from "./views/guardrails.js";
import { mcp } from "./views/mcp.js";
import { access } from "./views/access.js";
import { providers } from "./views/providers.js";
import { playground } from "./views/playground.js";
import { settings } from "./views/settings.js";
const views = {
  overview,
  requests,
  trace,
  guardrails,
  mcp,
  access,
  providers,
  playground,
  settings,
};
const root = document.getElementById("app");
let active,
  flash = "",
  returnTo = location.pathname === "/signin" ? "/" : location.pathname + location.search;
function logOut(expired = false) {
  if (expired && route().name !== "signin") returnTo = location.pathname + location.search;
  active?.abort();
  closeDialogs();
  signOut();
  setSession(null);
  flash = expired
    ? "Your session ended. Sign in again."
    : "Signed out. Your API key has been removed from this tab.";
  navigate("/signin", true);
}
onUnauthorized(() => logOut(true));
function polling(ctx, callback) {
  if (ctx.signal.aborted) return;
  let running = false;
  const timer = setInterval(async () => {
    if (document.hidden || running || ctx.signal.aborted) return;
    running = true;
    try {
      await callback();
    } catch (error) {
      if (error.name !== "AbortError" && error.status !== 401 && !ctx.signal.aborted) {
        const status = document.getElementById("poll-error");
        status?.replaceChildren(errorBox(error));
      }
    } finally {
      running = false;
    }
  }, 15000);
  ctx.signal.addEventListener("abort", () => clearInterval(timer), { once: true });
}
async function render() {
  active?.abort();
  closeDialogs();
  active = new AbortController();
  const ctx = {
    route: route(),
    signal: active.signal,
    refresh: () => render(),
    watch: (callback) => polling(ctx, callback),
    providers: (data) => {
      setProviders(data);
      updateCircuits();
    },
  };
  if (!getSession()) {
    if (ctx.route.name !== "signin") {
      returnTo = location.pathname + location.search;
      navigate("/signin", true);
      return;
    }
    root.replaceChildren(
      signin(ctx, flash, (session) => {
        if (!session.authenticated) {
          flash = "This API key was not accepted.";
          render();
          return;
        }
        setSession(session);
        flash = "";
        const target =
          returnTo.startsWith("/") && !returnTo.startsWith("//") && !returnTo.startsWith("/signin")
            ? returnTo
            : "/";
        navigate(target, true);
      }),
    );
    flash = "";
    document.title = "Sign in · OmniSwitch";
    return;
  }
  if (ctx.route.name === "signin") {
    navigate("/", true);
    return;
  }
  const layout = shell(ctx, () => logOut());
  root.replaceChildren(layout.el);
  updateCircuits();
  layout.main.setAttribute("aria-busy", "true");
  layout.main.append(
    h(
      "div",
      { class: "loading", role: "status" },
      h(
        "div",
        {},
        h("span", { class: "spinner", "aria-hidden": "true" }),
        "Loading " + ctx.route.name + "…",
      ),
    ),
  );
  try {
    const build = views[ctx.route.name];
    const content = build
      ? await build(ctx)
      : h(
          "div",
          {},
          page("Page not found", "This path is not part of the operator console."),
          empty(
            "Choose a console view",
            "Use the navigation to find traffic, policies, keys, or gateway settings.",
            button("Go to Overview", () => navigate("/")),
          ),
        );
    if (ctx.signal.aborted) return;
    layout.main.replaceChildren(content);
    layout.main.removeAttribute("aria-busy");
    const title = document.getElementById("view-title");
    document.title = (title?.textContent || "Console") + " · OmniSwitch";
    title?.focus({ preventScroll: true });
    announce(title?.textContent + " loaded.");
  } catch (error) {
    if (ctx.signal.aborted || error.name === "AbortError" || error.status === 401) return;
    layout.main.removeAttribute("aria-busy");
    layout.main.replaceChildren(
      page("Unable to load this view", "No data is displayed until the gateway responds."),
      errorBox(error),
      button("Try again", ctx.refresh, { icon: "refresh" }),
    );
  }
}
startRouter(render);
root.append(
  h(
    "main",
    { class: "loading", id: "main", role: "status" },
    h(
      "div",
      {},
      h("span", { class: "spinner", "aria-hidden": "true" }),
      "Connecting to OmniSwitch…",
    ),
  ),
);
const hadKey = hasCredential();
get("/api/session", { authFailure: false })
  .then((session) => {
    if (session.authenticated) setSession(session);
    else signOut();
    render();
  })
  .catch((error) => {
    signOut();
    setSession(null);
    flash =
      error.status === 401 ? (hadKey ? "Your session ended. Sign in again." : "") : error.message;
    if (route().name !== "signin") navigate("/signin", true);
    else render();
  });
