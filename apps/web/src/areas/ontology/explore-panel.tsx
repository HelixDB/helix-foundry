// Explore panel: run graph algorithms and fraud recipes on a chosen edge
// dataset, show results, and save the current view as a named perspective.
import { useState, useMemo } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  Activity,
  AlertTriangle,
  Bookmark,
  Filter,
  GitBranch,
  Loader2,
  Network,
  Play,
  Search,
  Shield,
  Target,
} from "lucide-react";
import { api } from "../../api";
import { useWorkspace, useResources } from "../../ui";
import { Button, cx } from "../../kit";

type AlgoResult = {
  algorithm: string;
  name: string;
  columns: string[];
  rows: Record<string, unknown>[];
};

type FraudResult = {
  recipe: string;
  name: string;
  findings: {
    findingId: string;
    severity: string;
    score: number;
    nodeIds: string[];
    explanation: string;
  }[];
  scannedNodes: number;
  scannedEdges: number;
  durationMs: number;
};

const algorithms = [
  { kind: "degree", label: "Degree", icon: GitBranch },
  { kind: "pagerank", label: "PageRank", icon: Activity },
  { kind: "wcc", label: "Communities (WCC)", icon: Network },
  { kind: "betweenness", label: "Betweenness", icon: Target },
  { kind: "knn", label: "k-NN Similarity", icon: Network },
  { kind: "node_similarity", label: "Node Similarity", icon: Network },
];

const fraudRecipes = [
  { kind: "money_flow_rings", label: "Money-flow rings" },
  { kind: "structuring", label: "Structuring" },
  { kind: "cashout_trails", label: "Cash-out trails" },
];

const severityColor: Record<string, string> = {
  critical: "text-red-500",
  high: "text-orange-500",
  medium: "text-yellow-500",
  low: "text-blue-500",
};

export function ExplorePanel({
  onHighlight,
}: {
  onHighlight?: (nodeIds: string[]) => void;
}) {
  const { id: workspaceId, role } = useWorkspace();
  const datasets = useResources("dataset");
  const [tab, setTab] = useState<"algorithm" | "fraud">("algorithm");
  const [searchTerm, setSearchTerm] = useState("");
  const [severityFilter, setSeverityFilter] = useState<string>("all");
  const [edgeDatasetId, setEdgeDatasetId] = useState("");
  const [algorithm, setAlgorithm] = useState("degree");
  const [recipe, setRecipe] = useState("money_flow_rings");
  const [srcColumn, setSrcColumn] = useState("from_id");
  const [dstColumn, setDstColumn] = useState("to_id");
  const [amountColumn, setAmountColumn] = useState("amount");
  const [result, setResult] = useState<AlgoResult | null>(null);
  const [fraudResult, setFraudResult] = useState<FraudResult | null>(null);
  const [perspectiveName, setPerspectiveName] = useState("");
  const [saved, setSaved] = useState(false);

  const edgeDatasets = (datasets.data?.items || []).filter((d: any) => {
    const cols = d.data?.profile?.columns?.map((c: any) => c.name) || [];
    return cols.includes(srcColumn) && cols.includes(dstColumn);
  });

  const runAlgo = useMutation({
    mutationFn: async () => {
      const r = await api<AlgoResult>(`/workspaces/${workspaceId}/graph/algorithms`, {
        method: "POST",
        body: JSON.stringify({
          kind: algorithm,
          name: algorithm,
          projection: {
            name: algorithm,
            edgeDatasetId,
            srcColumn,
            dstColumn,
            edgeTypes: [],
          },
          params: { topK: 10, sampleSize: 100, maxDepth: 6 },
          persist: false,
        }),
      });
      setResult(r);
      setFraudResult(null);
      return r;
    },
  });

  const runFraud = useMutation({
    mutationFn: async () => {
      const r = await api<FraudResult>(
        `/workspaces/${workspaceId}/graph/fraud/scan`,
        {
          method: "POST",
          body: JSON.stringify({
            recipe,
            name: recipe,
            projection: {
              name: recipe,
              edgeDatasetId,
              srcColumn,
              dstColumn,
              edgeTypes: [],
            },
            params: {
              amountColumn,
              maxDepth: 5,
              topN: 20,
            },
            persist: false,
          }),
        },
      );
      setFraudResult(r);
      setResult(null);
      return r;
    },
  });

  const savePerspective = () => {
    if (!perspectiveName.trim()) return;
    // Save as a query resource with the current view settings.
    api(`/workspaces/${workspaceId}/queries`, {
      method: "POST",
      body: JSON.stringify({
        name: `Perspective: ${perspectiveName}`,
        inputs: edgeDatasetId ? [edgeDatasetId] : [],
        sql: `-- Perspective saved from Explore\n-- algorithm=${algorithm} recipe=${recipe} src=${srcColumn} dst=${dstColumn}`,
      }),
    }).then(() => setSaved(true));
  };

  const filteredRows = useMemo(() => {
    if (!result || !searchTerm) return result?.rows.slice(0, 20) || [];
    return result.rows
      .filter((row) =>
        Object.values(row).some((v) =>
          String(v).toLowerCase().includes(searchTerm.toLowerCase()),
        ),
      )
      .slice(0, 20);
  }, [result, searchTerm]);

  const filteredFindings = useMemo(() => {
    if (!fraudResult) return [];
    return fraudResult.findings.filter((f) => {
      if (severityFilter !== "all" && f.severity !== severityFilter) return false;
      if (searchTerm && !f.explanation.toLowerCase().includes(searchTerm.toLowerCase())) return false;
      return true;
    });
  }, [fraudResult, searchTerm, severityFilter]);

  return (
    <div className="explore-panel">
      <div className="explore-panel-header">
        <Network size={16} />
        <span>Graph analysis</span>
      </div>

      {/* Edge dataset selector */}
      <div className="explore-field">
        <label>Edge dataset</label>
        <select
          value={edgeDatasetId}
          onChange={(e: any) => setEdgeDatasetId(e.target.value)}
        >
          <option value="">Select a dataset…</option>
          {edgeDatasets.map((d: any) => (
            <option key={d.id} value={d.id}>
              {d.name}
            </option>
          ))}
        </select>
      </div>

      <div className="explore-field-row">
        <div className="explore-field">
          <label>Source column</label>
          <input
            value={srcColumn}
            onChange={(e: any) => setSrcColumn(e.target.value)}
            className="input"
          />
        </div>
        <div className="explore-field">
          <label>Target column</label>
          <input
            value={dstColumn}
            onChange={(e: any) => setDstColumn(e.target.value)}
            className="input"
          />
        </div>
      </div>

      {/* Search & filter */}
      <div className="explore-field">
        <label>Search results</label>
        <div className="explore-search">
          <Search size={14} />
          <input
            value={searchTerm}
            onChange={(e: any) => setSearchTerm(e.target.value)}
            placeholder="Filter by node, explanation…"
            className="input"
          />
        </div>
      </div>
      {tab === "fraud" && (
        <div className="explore-field">
          <label>Severity</label>
          <select
            value={severityFilter}
            onChange={(e: any) => setSeverityFilter(e.target.value)}
            className="input"
          >
            <option value="all">All severities</option>
            <option value="critical">Critical</option>
            <option value="high">High</option>
            <option value="medium">Medium</option>
            <option value="low">Low</option>
          </select>
        </div>
      )}

      {/* Tabs */}
      <div className="explore-tabs">
        <button
          className={cx("explore-tab", tab === "algorithm" && "active")}
          onClick={() => setTab("algorithm")}
        >
          <Activity size={14} /> Algorithms
        </button>
        <button
          className={cx("explore-tab", tab === "fraud" && "active")}
          onClick={() => setTab("fraud")}
        >
          <Shield size={14} /> Fraud / AML
        </button>
      </div>

      {tab === "algorithm" ? (
        <>
          <div className="explore-field">
            <label>Algorithm</label>
            <select
              value={algorithm}
              onChange={(e: any) => setAlgorithm(e.target.value)}
            >
              {algorithms.map((a) => (
                <option key={a.kind} value={a.kind}>
                  {a.label}
                </option>
              ))}
            </select>
          </div>
          <Button
            onClick={() => runAlgo.mutate()}
            disabled={!edgeDatasetId || runAlgo.isPending}
          >
            {runAlgo.isPending ? (
              <Loader2 size={14} className="animate-spin" />
            ) : (
              <Play size={14} />
            )}
            Run
          </Button>
        </>
      ) : (
        <>
          <div className="explore-field">
            <label>Recipe</label>
            <select
              value={recipe}
              onChange={(e: any) => setRecipe(e.target.value)}
            >
              {fraudRecipes.map((r) => (
                <option key={r.kind} value={r.kind}>
                  {r.label}
                </option>
              ))}
            </select>
          </div>
          <div className="explore-field">
            <label>Amount column</label>
            <input
              value={amountColumn}
              onChange={(e: any) => setAmountColumn(e.target.value)}
              className="input"
            />
          </div>
          <Button
            onClick={() => runFraud.mutate()}
            disabled={!edgeDatasetId || runFraud.isPending}
          >
            {runFraud.isPending ? (
              <Loader2 size={14} className="animate-spin" />
            ) : (
              <AlertTriangle size={14} />
            )}
            Scan
          </Button>
        </>
      )}

      {/* Results */}
      {result && (
        <div className="explore-results">
          <div className="explore-results-header">
            {result.name} — {result.rows.length} rows
          </div>
          <div className="explore-results-table">
            <table>
              <thead>
                <tr>
                  {result.columns.slice(0, 4).map((c) => (
                    <th key={c}>{c}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {filteredRows.map((row, i) => (
                  <tr key={i}>
                    {result.columns.slice(0, 4).map((c) => (
                      <td key={c}>{String(row[c] ?? "")}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {fraudResult && (
        <div className="explore-results">
          <div className="explore-results-header">
            <AlertTriangle size={14} /> {fraudResult.findings.length} findings (
            {fraudResult.durationMs}ms)
          </div>
          {filteredFindings.map((f) => (
            <div
              key={f.findingId}
              className="explore-finding"
              onClick={() => onHighlight?.(f.nodeIds)}
            >
              <span className={cx("severity", severityColor[f.severity])}>
                {f.severity}
              </span>
              <span className="finding-explanation">{f.explanation}</span>
            </div>
          ))}
          {filteredFindings.length === 0 && (
            <div className="explore-empty">No findings detected.</div>
          )}
        </div>
      )}

      {/* Save perspective */}
      <div className="explore-perspective">
        <Bookmark size={14} />
        <input
          value={perspectiveName}
          onChange={(e: any) => {
            setPerspectiveName(e.target.value);
            setSaved(false);
          }}
          placeholder="Perspective name…"
          className="input"
        />
        <Button
          onClick={savePerspective}
          disabled={!perspectiveName.trim() || saved}
          variant="ghost"
        >
          {saved ? "Saved" : "Save"}
        </Button>
      </div>
    </div>
  );
}
