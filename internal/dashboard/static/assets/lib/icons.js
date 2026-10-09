import { svg } from "./dom.js";
const paths = {
  shield: [
    "M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 " +
    "1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 " +
    "1 1 1z",
  ],
  overview: ["M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z"],
  requests: [
    "M8 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-3M8 3v5h5M8 3l5 5M8 " +
    "12h5M8 16h8M16 3v6M13 6h6",
  ],
  guardrails: ["M12 2 3 6v6c0 5 9 10 9 10s9-5 9-10V6zM9 12l2 2 4-4"],
  mcp: ["M9 3h6v6H9zM3 15h6v6H3zM15 15h6v6h-6zM12 9v3M6 15v-3h12v3"],
  access: ["M14 8a5 5 0 1 1-5 5L3 19v2h4v-3h3l4-5M16 6h.01"],
  providers: ["M3 6h6M15 6h6M9 3v6h6V3zM3 18h6M15 18h6M9 15v6h6v-6zM12 9v6"],
  playground: ["m8 5 11 7-11 7z"],
  settings: [
    "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8M9 3h6l1 3 3 1 2 5-2 5-3 1-1 3H9l-1-3-3-1-2-5 2-5 3-1z",
  ],
  server: ["M3 3h18v6H3zM3 15h18v6H3zM6 6h.01M6 18h.01"],
  refresh: ["M20 7v5h-5M4 17v-5h5M6 7a7 7 0 0 1 12-2l2 2M4 17l2 2a7 7 0 0 0 12-2"],
  menu: ["M4 6h16M4 12h16M4 18h16"],
  close: ["m6 6 12 12M6 18 18 6"],
  copy: ["M8 8h12v12H8zM16 8V4H4v12h4"],
  eye: ["M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6"],
  logout: ["M9 3H4v18h5M9 12h12m-4-4 4 4-4 4"],
  plus: ["M12 5v14M5 12h14"],
  arrow: ["M5 12h14m-5-5 5 5-5 5"],
  chevron: ["m9 5 7 7-7 7"],
  download: ["M12 3v12m-4-4 4 4 4-4M5 15v6h14v-6"],
  search: ["M10 3a7 7 0 1 0 0 14 7 7 0 0 0 0-14m5 12 6 6"],
  clock: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18M12 7v5l3 2"],
  lock: ["M5 10h14v11H5zM8 10V6a4 4 0 0 1 8 0v4M12 14v3"],
  check: ["m5 12 4 4L19 6"],
  alert: ["m12 3 10 18H2zM12 9v4M12 17h.01"],
  filter: ["M4 7h16M7 12h10M10 17h4"],
};
export function icon(name) {
  return svg(
    "svg",
    {
      viewBox: "0 0 24 24",
      class: "icon",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": 1.6,
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
      "aria-hidden": "true",
      focusable: "false",
    },
    (paths[name] || paths.shield).map((d) => svg("path", { d })),
  );
}
