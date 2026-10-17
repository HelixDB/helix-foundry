import {
  AreaChart,
  Area,
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { DataTable } from "./ui";
import { number } from "./api";
// A widget's tested result: a metric, a table, or a line or bar chart. Lazy-
// loaded by review.tsx so recharts stays out of the main bundle.
export function ResultPreview({ widget, rows }: { widget: any; rows: any[] }) {
  const keys = Object.keys(rows[0] || {}),
    x = widget.x || keys[0],
    y = widget.y || keys.find((k) => k !== x) || keys[0],
    values = rows.map((r: any) => ({ ...r, [y]: Number(r[y]) }));
  return (
    <>
      {" "}
      {widget.type === "metric" ? (
        <div className="metric-value">
          {typeof rows[0]?.[keys[0]] === "number"
            ? number(rows[0][keys[0]])
            : rows[0]?.[keys[0]] || "0"}
        </div>
      ) : widget.type === "table" ? (
        <DataTable rows={rows} />
      ) : widget.type === "graph" ? (
        <GraphMiniView rows={rows} focus={widget.focus} />
      ) : (
        <div className="chart-wrap">
          <ResponsiveContainer width="100%" height={240}>
            {widget.type === "line" ? (
              <AreaChart data={values}>
                <CartesianGrid strokeDasharray="3 3" vertical={false} />
                <XAxis dataKey={x} tick={{ fontSize: 10 }} />
                <YAxis tick={{ fontSize: 10 }} />
                <Tooltip />
                <Area
                  type="monotone"
                  dataKey={y}
                  stroke="#548b72"
                  fill="#dfebe4"
                />
              </AreaChart>
            ) : (
              <BarChart data={values}>
                <CartesianGrid strokeDasharray="3 3" vertical={false} />
                <XAxis dataKey={x} tick={{ fontSize: 10 }} />
                <YAxis tick={{ fontSize: 10 }} />
                <Tooltip />
                <Bar dataKey={y} fill="#679780" radius={[5, 5, 0, 0]} />
              </BarChart>
            )}
          </ResponsiveContainer>
        </div>
      )}
    </>
  );
}

// Compact graph view for dashboard tiles: shows node/edge counts and a
// simple link table when a full force graph would be too heavy.
function GraphMiniView({ rows, focus }: { rows: any[]; focus?: string }) {
  const nodeCount = rows.length,
    // Infer edges from a from/to or src/dst pair if present.
    first = rows[0] || {},
    fromKey = ["from_id", "from", "src", "node_a"].find((k) => k in first),
    toKey = ["to_id", "to", "dst", "node_b"].find((k) => k in first),
    edges = fromKey && toKey ? rows.length : 0;
  return (
    <div className="graph-mini">
      <div className="graph-mini-stats">
        <span>{nodeCount} rows</span>
        {edges > 0 && <span>{edges} edges</span>}
        {focus && <span>focus: {focus}</span>}
      </div>
      {rows.length > 0 && (
        <DataTable rows={rows.slice(0, 8)} />
      )}
    </div>
  );
}
