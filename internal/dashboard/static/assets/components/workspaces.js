import { h } from "../lib/dom.js";
import { get, post, query } from "../lib/api.js";
import { hasRole } from "../lib/state.js";
import { setQuery } from "../lib/router.js";
import { panel, button, pill, empty, notice, field, select } from "./ui.js";
import { table } from "./table.js";
import { formDialog } from "./dialog.js";
export async function workspaces(ctx) {
  if (!hasRole("admin")) return notice("Requires admin role", "red");
  const [orgs, spaces, users] = await Promise.all(
    ["orgs", "workspaces", "users"].map((path) => get("/api/" + path, { signal: ctx.signal })),
  );
  const selected =
    spaces.workspaces.find((w) => w.id === ctx.route.query.get("workspace_id")) ||
    spaces.workspaces[0];
  const members = selected
    ? await get(query("/api/workspace-members", { workspace_id: selected.id }), {
        signal: ctx.signal,
      })
    : { members: [] };
  const create = (title, path, fields) =>
    formDialog(title, fields, async (values) => {
      await post("/api/" + path, values, { signal: ctx.signal });
      ctx.refresh();
    });
  const addOrg = () =>
    create("Create organization", "orgs", [
      { name: "name", label: "Organization name", required: true },
    ]);
  const addSpace = () =>
    create("Create workspace", "workspaces", [
      { name: "name", label: "Workspace name", required: true },
      {
        name: "organization_id",
        label: "Organization",
        choices: orgs.organizations.map((o) => [o.id, o.name]),
        required: true,
      },
    ]);
  const addUser = () =>
    create("Create user", "users", [
      { name: "name", label: "Name", required: true },
      { name: "email", label: "Email", type: "email", required: true },
    ]);
  const addMember = () =>
    formDialog(
      "Add workspace member",
      [
        {
          name: "user_id",
          label: "User",
          choices: users.users.map((u) => [u.id, u.name + " · " + u.email]),
          required: true,
        },
        {
          name: "role",
          label: "Role",
          choices: ["viewer", "member", "admin", ...(hasRole("owner") ? ["owner"] : [])],
        },
      ],
      async (values) => {
        await post(
          "/api/workspace-members",
          { ...values, workspace_id: selected.id },
          { signal: ctx.signal },
        );
        ctx.refresh();
      },
    );
  return h(
    "div",
    { class: "stack" },
    h(
      "div",
      { class: "two-col equal" },
      panel(
        "Organizations",
        orgs.organizations.length
          ? table(
              "Organizations",
              [
                { label: "Name", key: "name" },
                { label: "ID", key: "id", class: "mono" },
              ],
              orgs.organizations,
            )
          : empty(
              "No organizations",
              "Create an organization, then add a workspace.",
              button("Create organization", addOrg),
            ),
        button("Create organization", addOrg, { icon: "plus" }),
      ),
      panel(
        "Workspaces",
        spaces.workspaces.length
          ? table(
              "Workspaces",
              [
                { label: "Name", key: "name" },
                {
                  label: "Organization",
                  render: (w) =>
                    orgs.organizations.find((o) => o.id === w.organization_id)?.name ||
                    w.organization_id,
                },
              ],
              spaces.workspaces,
            )
          : empty("No workspaces", "Create a workspace inside an organization."),
        button("Create workspace", addSpace, {
          icon: "plus",
          disabled: !orgs.organizations.length,
        }),
      ),
    ),
    panel(
      "Users",
      users.users.length
        ? table(
            "Users",
            [
              { label: "Name", key: "name" },
              { label: "Email", key: "email" },
              { label: "ID", key: "id", class: "mono" },
            ],
            users.users,
          )
        : empty("No users", "Create a user to assign workspace membership."),
      button("Create user", addUser, { icon: "plus" }),
    ),
    panel(
      "Workspace memberships",
      h(
        "div",
        { class: "stack" },
        field(
          "Workspace",
          select(
            spaces.workspaces.map((w) => [w.id, w.name]),
            selected?.id || "",
            { onChange: (e) => setQuery({ workspace_id: e.target.value }) },
          ),
        ),
        members.members.length
          ? table(
              "Workspace members",
              [
                {
                  label: "User",
                  render: (m) => users.users.find((u) => u.id === m.user_id)?.name || m.user_id,
                },
                { label: "Role", render: (m) => pill(m.role) },
              ],
              members.members,
            )
          : empty(
              "No memberships",
              "Add a user to the selected workspace with the least privilege needed.",
            ),
      ),
      button("Add member", addMember, { icon: "plus", disabled: !selected || !users.users.length }),
    ),
  );
}
