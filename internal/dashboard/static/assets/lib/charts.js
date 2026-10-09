import { h, svg } from "./dom.js";
import { number, utc, percent, ms } from "./format.js";
import { table } from "../components/table.js";
let chartID = 0;
const dataTable = (caption, columns, rows) =>
  h(
    "details",
    { class: "chart-table" },
    h("summary", {}, "Show as table"),
    table(caption, columns, rows),
  );
export function areaChart(buckets, label = "Requests") {
  if (!buckets?.length) return h("p", { class: "muted" }, "No buckets recorded for this period.");
  const width = 720,
    height = 170,
    left = 42,
    top = 14,
    bottom = 136;
  const max = Math.max(1, ...buckets.map((b) => Number(b.requests) || 0));
  const x = (i) => left + (i * (width - left - 16)) / Math.max(1, buckets.length - 1);
  const y = (value) => bottom - (Number(value || 0) / max) * (bottom - top);
  const peak = buckets.reduce((a, b) => (b.requests > a.requests ? b : a), buckets[0]);
  const summary =
    label +
    "; " +
    number(buckets.reduce((sum, b) => sum + b.requests, 0)) +
    " requests. Peak " +
    number(peak.requests) +
    " at " +
    utc(peak.start, true) +
    " UTC.";
  const gradient = "area-" + ++chartID;
  const plot = svg(
    "svg",
    {
      viewBox: "0 0 " + width + " " + height,
      class: "area-chart",
      role: "img",
      "aria-label": summary,
    },
    svg("title", {}, summary),
    svg(
      "defs",
      {},
      svg(
        "linearGradient",
        { id: gradient, x1: 0, y1: 0, x2: 0, y2: 1 },
        svg("stop", { offset: "0%", class: "gradient-top" }),
        svg("stop", { offset: "100%", class: "gradient-bottom" }),
      ),
    ),
  );
  for (let i = 0; i <= 3; i++) {
    const value = (max * i) / 3;
    plot.append(
      svg("line", { x1: left, y1: y(value), x2: width - 12, y2: y(value), class: "grid-line" }),
      svg(
        "text",
        { x: left - 8, y: y(value) + 4, "text-anchor": "end", class: "axis" },
        number(value),
      ),
    );
  }
  const points = buckets.map((bucket, i) => x(i) + "," + y(bucket.requests)).join(" ");
  plot.append(
    svg("polygon", {
      points: x(0) + "," + bottom + " " + points + " " + x(buckets.length - 1) + "," + bottom,
      fill: "url(#" + gradient + ")",
    }),
    svg("polyline", { points, class: "series requests" }),
  );
  for (const [key, color] of [
    ["denied", "denied"],
    ["errors", "errors"],
    ["limited", "limited"],
  ]) {
    plot.append(
      svg("polyline", {
        points: buckets.map((b, i) => x(i) + "," + y(b[key])).join(" "),
        class: "series " + color,
      }),
    );
  }
  buckets.forEach((bucket, i) => {
    const description =
      utc(bucket.start, true) +
      " UTC: " +
      number(bucket.requests) +
      " requests, " +
      number(bucket.denied) +
      " denied, " +
      number(bucket.limited) +
      " limited, " +
      number(bucket.errors) +
      " errors.";
    const point = svg(
      "g",
      { tabindex: "0", role: "img", "aria-label": description, class: "chart-point" },
      svg("title", {}, description),
      svg("circle", { cx: x(i), cy: y(bucket.requests), r: 6, class: "point-hit" }),
      svg(
        "g",
        { class: "point-tooltip", "aria-hidden": "true" },
        svg("rect", {
          x: Math.min(width - 180, Math.max(left, x(i) - 90)),
          y: top,
          width: 170,
          height: 34,
          rx: 4,
        }),
        svg(
          "text",
          { x: Math.min(width - 170, Math.max(left + 10, x(i) - 80)), y: top + 21 },
          utc(bucket.start, true) + " · " + number(bucket.requests),
        ),
      ),
    );
    plot.append(point);
    if (i % Math.max(1, Math.round((buckets.length - 1) / 5)) === 0 || i === buckets.length - 1)
      plot.append(
        svg(
          "text",
          { x: x(i), y: height - 8, "text-anchor": "middle", class: "axis" },
          utc(bucket.start, true).slice(0, 5),
        ),
      );
  });
  return h(
    "figure",
    { class: "chart-figure" },
    plot,
    h(
      "figcaption",
      { class: "legend" },
      [
        ["purple", "Requests"],
        ["red", "Denied"],
        ["amber", "Limited"],
        ["muted", "Errors"],
      ].map(([tone, text]) => h("span", {}, h("i", { class: "dot " + tone }), text)),
    ),
    dataTable(
      label + " by UTC bucket",
      [
        { label: "UTC", render: (b) => utc(b.start) },
        ...[
          "requests",
          "allowed",
          "denied",
          "limited",
          "errors",
          "cache_hits",
          "tokens",
          "cost",
          "avg_latency_ms",
        ].map((key) => ({ label: key.replaceAll("_", " "), key })),
      ],
      buckets,
    ),
  );
}
export function progress(used, limit, label, tone = "purple") {
  const ratio = Number(limit) > 0 ? Math.max(0, Number(used) / Number(limit)) : 0;
  const title = Number(limit) > 0 ? label + ": " + percent(ratio) + " used" : label + ": unlimited";
  return svg(
    "svg",
    { viewBox: "0 0 100 5", class: "progress " + tone, role: "img", "aria-label": title },
    svg("title", {}, title),
    svg("rect", { width: 100, height: 5, rx: 2.5, class: "track" }),
    svg("rect", { width: Math.min(100, ratio * 100), height: 5, rx: 2.5, class: "bar" }),
  );
}
export function stackedBar(items, label) {
  const total = items.reduce((n, item) => n + Number(item.count || 0), 0);
  let offset = 0;
  const plot = svg(
    "svg",
    {
      viewBox: "0 0 500 18",
      class: "stacked-bar",
      role: "img",
      "aria-label": label + ": " + number(total),
    },
    svg("title", {}, label),
  );
  items.forEach((item) => {
    const width = total ? (item.count / total) * 500 : 0;
    plot.append(
      svg(
        "rect",
        {
          x: offset,
          width,
          height: 18,
          class: "bar " + item.tone,
          tabindex: "0",
          role: "img",
          "aria-label": item.label + ": " + number(item.count),
        },
        svg("title", {}, item.label + ": " + number(item.count)),
      ),
    );
    offset += width;
  });
  return h(
    "figure",
    { class: "chart-figure" },
    plot,
    h(
      "figcaption",
      { class: "legend" },
      items.map((item) =>
        h("span", {}, h("i", { class: "dot " + item.tone }), item.label + " " + number(item.count)),
      ),
    ),
    dataTable(
      label,
      [
        { label: "Action", key: "label" },
        { label: "Count", key: "count" },
      ],
      items,
    ),
  );
}
export function sparkline(values, label) {
  const max = Math.max(1, ...values);
  return svg(
    "svg",
    { viewBox: "0 0 120 32", class: "sparkline", role: "img", "aria-label": label },
    svg("title", {}, label),
    svg("polyline", {
      points: values
        .map((v, i) => (i / Math.max(1, values.length - 1)) * 120 + "," + (30 - (v / max) * 28))
        .join(" "),
      class: "series requests",
    }),
  );
}
export function waterfall(spans, select) {
  const starts = spans.map((s) => Date.parse(s.start)).filter(Number.isFinite);
  const start = Math.min(...starts);
  const end = Math.max(
    ...spans.map((s) => Date.parse(s.start) + Number(s.duration_ms || 0)).filter(Number.isFinite),
  );
  const duration = Math.max(1, end - start);
  return table(
    "Request span waterfall in milliseconds",
    [
      {
        label: "Span",
        render: (s, i) =>
          h(
            "button",
            {
              class: "span-select",
              type: "button",
              onClick: () => select(s),
              "aria-label": "Inspect span " + (i + 1) + ": " + s.name,
            },
            s.name.replaceAll("_", " "),
            s.provider ? h("span", { class: "muted small" }, s.provider) : null,
          ),
      },
      {
        label: "Timeline",
        render: (s) =>
          svg(
            "svg",
            {
              viewBox: "0 0 500 24",
              class: "waterfall-bar",
              role: "img",
              "aria-label": s.name + ": " + ms(s.duration_ms),
            },
            svg(
              "title",
              {},
              s.name +
                ": starts at " +
                ms(Date.parse(s.start) - start) +
                ", duration " +
                ms(s.duration_ms),
            ),
            svg("line", { x1: 0, y1: 12, x2: 500, y2: 12, class: "grid-line" }),
            svg("rect", {
              x: Number.isFinite(Date.parse(s.start))
                ? ((Date.parse(s.start) - start) / duration) * 490
                : 0,
              y: 5,
              width: Math.max(3, (Number(s.duration_ms || 0) / duration) * 490),
              height: 14,
              rx: 3,
              class:
                "bar " + (s.status === "error" ? "red" : s.metadata?.fallback ? "amber" : "purple"),
            }),
          ),
      },
      {
        label: "Start",
        render: (s) =>
          ms(Number.isFinite(Date.parse(s.start)) ? Date.parse(s.start) - start : null),
        class: "mono",
      },
      { label: "Duration", render: (s) => ms(s.duration_ms), class: "mono" },
      { label: "Status", key: "status" },
    ],
    spans,
  );
}
