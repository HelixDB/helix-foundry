// /data/graph: Graph compute. Run GDS-like algorithms, fraud recipes and
// pathfinding over any edge dataset (two id columns), and keep the scores as
// datasets for the Analyst and pipelines.
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Network, ShieldAlert, Signpost, Waypoints } from "lucide-react";
import { api } from "../../api";
import {
  Button,
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  Section,
  TabPanel,
  Tabs,
  useTabParam,
} from "../../kit";
import { useAction, useResources, useWorkspace } from "../../ui";

type Projection = {
  edgeDatasetId: string;
  srcColumn: string;
  dstColumn: string;
  weightColumn: string;
};
const emptyProjection: Projection = {
  edgeDatasetId: "",
  srcColumn: "from_id",
  dstColumn: "to_id",
  weightColumn: "",
};

export default function GraphComputePage() {
  const { id } = useWorkspace();
  const [tab, setTab] = useTabParam(["algorithms", "fraud", "paths"]);
  const datasets = useResources("dataset", { limit: 500 });
  const schema = useQuery({
    queryKey: [id, "graph-schema"],
    queryFn: () => api(`/workspaces/${id}/graph/schema`),
  });
  const items: any[] = datasets.data?.items || [];
  const [projection, setProjection] = useState<Projection>(emptyProjection);
  const set = (patch: Partial<Projection>) =>
    setProjection((p) => ({ ...p, ...patch }));
  const selected = items.find((d) => d.id === projection.edgeDatasetId);
  const columns: string[] = (selected?.data?.profile?.columns || []).map(
    (c: any) => String(c.name),
  );
  const columnOptions = [...new Set([...columns, "from_id", "to_id", "id"])];

  return (
    <Page>
      <PageHeader
        eyebrow="Data"
        title="Graph compute"
        subtitle="Algorithms, fraud recipes and pathfinding over any edge dataset"
      />
      <Section
        title="Projection"
        description="Pick the dataset that holds the edges. Each row is one edge, with a source and a target id column."
      >
        <Panel>
          <div className="hf-field-row">
            <label className="hf-field">
              <span>Edge dataset</span>
              <select
                value={projection.edgeDatasetId}
                onChange={(e) => set({ edgeDatasetId: e.target.value })}
              >
                <option value="">Choose a dataset…</option>
                {items.map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name || d.id}
                  </option>
                ))}
              </select>
            </label>
            <ColumnField
              label="Source column"
              value={projection.srcColumn}
              options={columnOptions}
              onChange={(v) => set({ srcColumn: v })}
            />
            <ColumnField
              label="Target column"
              value={projection.dstColumn}
              options={columnOptions}
              onChange={(v) => set({ dstColumn: v })}
            />
            <ColumnField
              label="Weight column (optional)"
              value={projection.weightColumn}
              options={columnOptions}
              onChange={(v) => set({ weightColumn: v })}
            />
          </div>
        </Panel>
      </Section>
      <Tabs
        label="Graph compute"
        value={tab}
        onChange={setTab}
        items={[
          { id: "algorithms", label: "Algorithms", icon: Network },
          { id: "fraud", label: "Fraud recipes", icon: ShieldAlert },
          { id: "paths", label: "Pathfinding", icon: Waypoints },
        ]}
      />
      <TabPanel id="algorithms" value={tab}>
        <AlgorithmsTab projection={projection} schema={schema.data} />
      </TabPanel>
      <TabPanel id="fraud" value={tab}>
        <FraudTab projection={projection} schema={schema.data} />
      </TabPanel>
      <TabPanel id="paths" value={tab}>
        <PathsTab projection={projection} />
      </TabPanel>
    </Page>
  );
}

function ColumnField({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: string[];
  onChange: (value: string) => void;
}) {
  return (
    <label className="hf-field">
      <span>{label}</span>
      <input
        list="graph-column-options"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="from_id"
      />
      <datalist id="graph-column-options">
        {options.map((c) => (
          <option key={c} value={c} />
        ))}
      </datalist>
    </label>
  );
}

function AlgorithmsTab({
  projection,
  schema,
}: {
  projection: Projection;
  schema: any;
}) {
  const { id } = useWorkspace(),
    action = useAction();
  const [kind, setKind] = useState("pagerank"),
    [name, setName] = useState(""),
    [persist, setPersist] = useState(true),
    [params, setParams] = useState<Record<string, string>>({
      damping: "0.85",
      maxIterations: "20",
      tolerance: "1e-6",
      topK: "10",
      sampleSize: "200",
      maxDepth: "6",
      sourceId: "",
      targetId: "",
    });
  const [result, setResult] = useState<any>(null);
  const catalog: any[] = schema?.algorithms || [];
  const setParam = (k: string, v: string) =>
    setParams((p) => ({ ...p, [k]: v }));
  const run = () =>
    action.run(async () => {
      const spec: any = {
        kind,
        name: name || kind,
        projection: {
          name: name || kind,
          edgeDatasetId: projection.edgeDatasetId,
          srcColumn: projection.srcColumn,
          dstColumn: projection.dstColumn,
          ...(projection.weightColumn
            ? { weightColumn: projection.weightColumn }
            : {}),
        },
        persist,
      };
      spec.params = {};
      if (kind === "pagerank") {
        spec.params.damping = Number(params.damping);
        spec.params.maxIterations = Number(params.maxIterations);
        spec.params.tolerance = Number(params.tolerance);
      }
      if (kind === "node_similarity") {
        spec.params.topK = Number(params.topK);
        spec.params.sampleSize = Number(params.sampleSize);
      }
      if (kind === "shortest_path") {
        spec.params.sourceId = params.sourceId;
        spec.params.targetId = params.targetId;
        spec.params.maxDepth = Number(params.maxDepth);
      }
      const r = await api(`/workspaces/${id}/graph/algorithms`, spec);
      setResult(r);
      return `Computed ${r.rows.length} rows`;
    }, "Algorithm finished");
  return (
    <Section
      title="Algorithms"
      description={
        catalog.find((c) => c.kind === kind)?.description ||
        "Centrality, communities, similarity and paths."
      }
      actions={
        <Button
          variant="primary"
          onClick={run}
          disabled={!projection.edgeDatasetId || action.pending}
        >
          {action.pending ? "Running…" : "Run algorithm"}
        </Button>
      }
    >
      <Panel>
        <div className="hf-field-row">
          <label className="hf-field">
            <span>Algorithm</span>
            <select value={kind} onChange={(e) => setKind(e.target.value)}>
              {(catalog.length
                ? catalog.map((c) => c.kind)
                : ["degree", "wcc", "pagerank", "louvain", "node_similarity", "shortest_path"]
              ).map((k: string) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          <label className="hf-field">
            <span>Name</span>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={kind}
            />
          </label>
          {kind === "pagerank" && (
            <>
              <label className="hf-field">
                <span>Damping</span>
                <input
                  type="number"
                  step="0.05"
                  min="0"
                  max="1"
                  value={params.damping}
                  onChange={(e) => setParam("damping", e.target.value)}
                />
              </label>
              <label className="hf-field">
                <span>Max iterations</span>
                <input
                  type="number"
                  min="1"
                  max="100"
                  value={params.maxIterations}
                  onChange={(e) => setParam("maxIterations", e.target.value)}
                />
              </label>
            </>
          )}
          {kind === "node_similarity" && (
            <label className="hf-field">
              <span>Top-k pairs</span>
              <input
                type="number"
                min="1"
                max="100"
                value={params.topK}
                onChange={(e) => setParam("topK", e.target.value)}
              />
            </label>
          )}
          {kind === "shortest_path" && (
            <>
              <label className="hf-field">
                <span>Source node id</span>
                <input
                  value={params.sourceId}
                  onChange={(e) => setParam("sourceId", e.target.value)}
                />
              </label>
              <label className="hf-field">
                <span>Target node id</span>
                <input
                  value={params.targetId}
                  onChange={(e) => setParam("targetId", e.target.value)}
                />
              </label>
              <label className="hf-field">
                <span>Max depth</span>
                <input
                  type="number"
                  min="1"
                  max="10"
                  value={params.maxDepth}
                  onChange={(e) => setParam("maxDepth", e.target.value)}
                />
              </label>
            </>
          )}
          <label className="hf-field">
            <span>Keep scores as a dataset</span>
            <input
              type="checkbox"
              checked={persist}
              onChange={(e) => setPersist(e.target.checked)}
            />
          </label>
        </div>
      </Panel>
      {result ? (
        <Panel
          title="Results"
          subtitle={
            result.datasetId
              ? `Saved as dataset ${result.datasetId}`
              : `${result.rows.length} rows`
          }
        >
          <DataGrid rows={result.rows} label="Algorithm results" />
        </Panel>
      ) : (
        <EmptyState
          icon={Network}
          title="No results yet"
          text="Choose an edge dataset and run an algorithm to score its nodes."
        />
      )}
    </Section>
  );
}

function FraudTab({ projection, schema }: { projection: Projection; schema: any }) {
  const { id } = useWorkspace(),
    action = useAction();
  const [recipe, setRecipe] = useState("money_flow_rings"),
    [name, setName] = useState(""),
    [persist, setPersist] = useState(true),
    [amountColumn, setAmountColumn] = useState("amount"),
    [threshold, setThreshold] = useState(""),
    [minRingSize, setMinRingSize] = useState("3"),
    [maxDepth, setMaxDepth] = useState("5"),
    [topN, setTopN] = useState("50");
  const [result, setResult] = useState<any>(null);
  const catalog: any[] = schema?.fraudRecipes || [];
  const run = () =>
    action.run(async () => {
      const scan: any = {
        recipe,
        name: name || recipe,
        projection: {
          name: name || recipe,
          edgeDatasetId: projection.edgeDatasetId,
          srcColumn: projection.srcColumn,
          dstColumn: projection.dstColumn,
        },
        persist,
        params: {
          maxDepth: Number(maxDepth),
          minRingSize: Number(minRingSize),
          topN: Number(topN),
          ...(amountColumn ? { amountColumn } : {}),
          ...(threshold ? { threshold: Number(threshold) } : {}),
        },
      };
      const r = await api(`/workspaces/${id}/graph/fraud/scan`, scan);
      setResult(r);
      return `Found ${r.findings.length} findings`;
    }, "Fraud scan finished");
  return (
    <Section
      title="Fraud recipes"
      description={
        catalog.find((c) => c.kind === recipe)?.description ||
        "Money-flow rings, structuring and cash-out trails."
      }
      actions={
        <Button
          variant="primary"
          onClick={run}
          disabled={!projection.edgeDatasetId || action.pending}
        >
          {action.pending ? "Scanning…" : "Run scan"}
        </Button>
      }
    >
      <Panel>
        <div className="hf-field-row">
          <label className="hf-field">
            <span>Recipe</span>
            <select value={recipe} onChange={(e) => setRecipe(e.target.value)}>
              {(catalog.length
                ? catalog.map((c) => c.kind)
                : ["money_flow_rings", "structuring", "cashout_trails"]
              ).map((k: string) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          <label className="hf-field">
            <span>Name</span>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={recipe}
            />
          </label>
          <label className="hf-field">
            <span>Amount column</span>
            <input
              value={amountColumn}
              onChange={(e) => setAmountColumn(e.target.value)}
              placeholder="amount"
            />
          </label>
          <label className="hf-field">
            <span>Threshold (optional)</span>
            <input
              type="number"
              min="0"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Min ring size</span>
            <input
              type="number"
              min="2"
              max="10"
              value={minRingSize}
              onChange={(e) => setMinRingSize(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Max depth</span>
            <input
              type="number"
              min="2"
              max="8"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Top findings</span>
            <input
              type="number"
              min="1"
              max="500"
              value={topN}
              onChange={(e) => setTopN(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Keep findings as a dataset</span>
            <input
              type="checkbox"
              checked={persist}
              onChange={(e) => setPersist(e.target.checked)}
            />
          </label>
        </div>
      </Panel>
      {result ? (
        <Panel
          title="Findings"
          subtitle={`${result.findings.length} findings · ${result.scannedEdges} edges scanned`}
        >
          {result.findings.length ? (
            <DataGrid
              label="Fraud findings"
              rows={result.findings.map((f: any) => ({
                severity: f.severity,
                score: f.score,
                nodes: f.nodeIds.join(", "),
                amount: f.amount ?? "",
                explanation: f.explanation,
              }))}
            />
          ) : (
            <EmptyState
              icon={ShieldAlert}
              title="No findings"
              text="This recipe found nothing on the selected edges. Try another recipe or lower the threshold."
            />
          )}
        </Panel>
      ) : (
        <EmptyState
          icon={ShieldAlert}
          title="No scan yet"
          text="Run a recipe to look for suspicious money flows in the selected edges."
        />
      )}
    </Section>
  );
}

function PathsTab({ projection }: { projection: Projection }) {
  const { id } = useWorkspace(),
    action = useAction();
  const [sourceId, setSourceId] = useState(""),
    [targetId, setTargetId] = useState(""),
    [maxDepth, setMaxDepth] = useState("6");
  const [path, setPath] = useState<any>(null);
  const run = () =>
    action.run(async () => {
      const r = await api(`/workspaces/${id}/graph/paths`, {
        projection: {
          name: "paths",
          edgeDatasetId: projection.edgeDatasetId,
          srcColumn: projection.srcColumn,
          dstColumn: projection.dstColumn,
        },
        sourceId,
        targetId,
        maxDepth: Number(maxDepth),
      });
      setPath(r);
      return r ? `Path of length ${r.length}` : "No path found";
    }, "Pathfinding finished");
  return (
    <Section
      title="Pathfinding"
      description="Shortest path between two node ids in the selected edges."
      actions={
        <Button
          variant="primary"
          onClick={run}
          disabled={
            !projection.edgeDatasetId || !sourceId || !targetId || action.pending
          }
        >
          {action.pending ? "Searching…" : "Find path"}
        </Button>
      }
    >
      <Panel>
        <div className="hf-field-row">
          <label className="hf-field">
            <span>Source node id</span>
            <input
              value={sourceId}
              onChange={(e) => setSourceId(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Target node id</span>
            <input
              value={targetId}
              onChange={(e) => setTargetId(e.target.value)}
            />
          </label>
          <label className="hf-field">
            <span>Max depth</span>
            <input
              type="number"
              min="1"
              max="10"
              value={maxDepth}
              onChange={(e) => setMaxDepth(e.target.value)}
            />
          </label>
        </div>
      </Panel>
      {path ? (
        <Panel title="Path" subtitle={`${path.length} hops`}>
          <p className="hf-mono">{path.nodeIds.join(" → ")}</p>
        </Panel>
      ) : (
        <EmptyState
          icon={Signpost}
          title="No path yet"
          text="Give a source and a target node id to trace how they connect."
        />
      )}
    </Section>
  );
}
