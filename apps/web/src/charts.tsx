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
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { DataTable, useWorkspace } from "./ui";
import { api, number } from "./api";
import { paths } from "./paths";
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

// Compact graph view for dashboard tiles: the workspace's graph stats with
// the highest scores read as bars; a plain table otherwise, when a full
// force graph would be too heavy.
function GraphMiniView({ rows, focus }: { rows: any[]; focus?: string }) {
  const { id } = useWorkspace();
  const stats = useQuery({
    queryKey: [id, "graph-stats"],
    queryFn: () => api(`/workspaces/${id}/graph/stats`),
    retry: false,
    staleTime: 30_000,
  });
  const s: any = stats.data,
    first = rows[0] || {},
    fromKey = ["from_id", "from", "src", "node_a"].find((k) => k in first),
    toKey = ["to_id", "to", "dst", "node_b"].find((k) => k in first),
    // A persisted algorithm run's rows rank as bars; other shapes as a table.
    idKey = fromKey || (["node_id", "node", "id", "key"] as const).find((k) => k in first),
    scoreKey = (["score", "pagerank", "degree", "centrality"] as const).find(
      (k) => k in first && typeof first[k] === "number",
    ),
    scored =
      scoreKey && idKey
        ? [...rows]
            .sort((a, b) => Number(b[scoreKey]) - Number(a[scoreKey]))
            .slice(0, 6)
        : [],
    top = scored.reduce((m, r) => Math.max(m, Number(r[scoreKey!])), 0) || 1;
  return (
    <div className="graph-mini">
      <div className="graph-mini-stats">
        {s ? (
          <>
            <span>{number(s.nodeCount)} nodes</span>
            <span>{number(s.edgeCount)} edges</span>
            <span>avg degree {(s.avgDegree || 0).toFixed(1)}</span>
          </>
        ) : (
          <span>
            {number(rows.length)} {fromKey && toKey ? "edges" : "rows"}
          </span>
        )}
        {focus && <span>focus {focus}</span>}
        <Link to={paths.explorer()} className="graph-mini-link">
          Open in explorer
        </Link>
      </div>
      {scored.length > 0 ? (
        <div className="graph-mini-bars">
          {scored.map((r, i) => (
            <div key={i}>
              <span>{String(r[idKey!])}</span>
              <div>
                <i
                  style={{ width: `${(Number(r[scoreKey!]) / top) * 100}%` }}
                />
              </div>
              <em>{number(Number(r[scoreKey!]))}</em>
            </div>
          ))}
        </div>
      ) : rows.length > 0 ? (
        <DataTable rows={rows.slice(0, 8)} />
      ) : (
        <p className="muted">No graph yet — publish the ontology first.</p>
      )}
    </div>
  );
}
