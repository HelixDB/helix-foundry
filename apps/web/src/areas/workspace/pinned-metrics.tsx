import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Plus, RefreshCw } from "lucide-react";
import type {
  PinnedMetric,
  PinnedMetricSpec,
  PinnedMetrics as Response,
} from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import { Button, Dialog, ConfirmDialog } from "../../kit";
import { KpiTile } from "./metrics";

export function PinnedMetrics() {
  const { id, role, notify } = useWorkspace();
  const [editing, setEditing] = useState<PinnedMetric | "new" | null>(null);
  const [removing, setRemoving] = useState<PinnedMetric | null>(null);
  const [pending, setPending] = useState(false);
  const base = `/workspaces/${id}/home/pinned-metrics`;
  const query = useQuery({
    queryKey: [id, "pinned-metrics"],
    queryFn: () => api<Response>(base),
  });
  const data = query.data;
  if (!data || (!data.datasets.length && !data.items.length))
    return query.error ? (
      <p role="alert">Couldn’t load pinned metrics.</p>
    ) : null;
  return (
    <section aria-label="Pinned metrics" className="wk-metrics wk-pinned">
      <div className="wk-metrics-head">
        <div>
          <h2>Pinned metrics</h2>
          <p>Your definitions · evaluated on current snapshots without AI</p>
        </div>
        <div className="buttons">
          <Button
            size="sm"
            variant="ghost"
            icon={RefreshCw}
            pending={query.isFetching}
            onClick={() => void query.refetch()}
          >
            Refresh pinned metrics
          </Button>
          {role !== "viewer" && (
            <Button
              size="sm"
              icon={Plus}
              disabled={!data.datasets.length || data.items.length >= 8}
              onClick={() => setEditing("new")}
            >
              Pin metric
            </Button>
          )}
        </div>
      </div>
      {!data.items.length ? (
        <p className="wk-metrics-note">
          Choose the numbers you want to keep on Home.
        </p>
      ) : (
        <div className="wk-metrics-kpis">
          {data.items.map((pin) => (
            <div key={pin.id}>
              <KpiTile
                m={{
                  id: pin.id,
                  title: pin.spec.title,
                  why: pin.spec.why,
                  kind: "kpi",
                  format: pin.spec.format,
                  dataset: pin.dataset,
                  window: pin.spec.timeColumn ? "30d" : undefined,
                  result: pin.result,
                }}
              />
              <details className="disclosure">
                <summary>Definition and evidence</summary>
                <p>
                  {pin.dataset} ·{" "}
                  {pin.version
                    ? `Snapshot ${pin.version}`
                    : "Dataset unavailable"}
                </p>
                <p>Evaluated {new Date(pin.computedAt).toLocaleString()}</p>
                {pin.result.error && <p role="alert">{pin.result.error}</p>}
                <pre>
                  {pin.sql ||
                    "The saved definition is unavailable on the current schema."}
                </pre>
              </details>
              {role !== "viewer" && (
                <div className="buttons">
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setEditing(pin)}
                  >
                    Edit {pin.spec.title}
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setRemoving(pin)}
                  >
                    Remove {pin.spec.title}
                  </Button>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
      {editing && (
        <MetricEditor
          key={typeof editing === "string" ? editing : editing.id}
          datasets={data.datasets}
          pin={typeof editing === "string" ? undefined : editing}
          onClose={() => setEditing(null)}
          onSave={async (spec) => {
            await api(
              base + (typeof editing === "string" ? "" : `/${editing.id}`),
              {
                spec,
                ...(typeof editing === "string"
                  ? {}
                  : { revision: editing.revision }),
              },
              typeof editing === "string" ? "POST" : "PUT",
            );
            await query.refetch();
            setEditing(null);
          }}
        />
      )}
      {removing && (
        <ConfirmDialog
          title="Remove pinned metric?"
          description={`Remove ${removing.spec.title} from Home. Imported data and automatic metrics are unchanged.`}
          confirmLabel="Remove metric"
          pending={pending}
          onCancel={() => setRemoving(null)}
          onConfirm={async () => {
            setPending(true);
            try {
              await api(
                base + `/${removing.id}`,
                { revision: removing.revision },
                "DELETE",
              );
              await query.refetch();
              setRemoving(null);
            } catch (e) {
              notify((e as Error).message, true);
            } finally {
              setPending(false);
            }
          }}
        />
      )}
    </section>
  );
}
function MetricEditor({
  datasets,
  pin,
  onClose,
  onSave,
}: {
  datasets: Response["datasets"];
  pin?: PinnedMetric;
  onClose: () => void;
  onSave: (spec: PinnedMetricSpec) => Promise<void>;
}) {
  const [spec, setSpec] = useState<PinnedMetricSpec>(
    pin?.spec || {
      title: "",
      why: "",
      kind: "kpi",
      datasetId: datasets[0]?.id || "",
      measure: "count",
      format: "number",
      scale: "none",
    },
  );
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const dataset = datasets.find((d) => d.id === spec.datasetId);
  const update = (next: Partial<PinnedMetricSpec>) =>
    setSpec((old) => ({ ...old, ...next }));
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setPending(true);
    setError("");
    try {
      await onSave(spec);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <Dialog
      title={pin ? "Edit pinned metric" : "Pin a metric"}
      description="Choose a count, sum or average. The query must run before the definition is saved."
      onClose={onClose}
    >
      <form onSubmit={(event) => void submit(event)}>
        <label className="hf-field">
          Title
          <input
            data-autofocus
            required
            maxLength={60}
            value={spec.title}
            onChange={(e) => update({ title: e.target.value })}
          />
        </label>
        <label className="hf-field">
          Dataset
          <select
            value={spec.datasetId}
            onChange={(e) =>
              update({
                datasetId: e.target.value,
                column: undefined,
                timeColumn: undefined,
                measure: "count",
                format: "number",
                scale: "none",
              })
            }
          >
            {!dataset && (
              <option value={spec.datasetId}>Unavailable dataset</option>
            )}
            {datasets.map((d) => (
              <option key={d.id} value={d.id}>
                {d.name}
              </option>
            ))}
          </select>
        </label>
        <label className="hf-field">
          Measure
          <select
            value={spec.measure}
            onChange={(e) => {
              const measure = e.target.value as PinnedMetricSpec["measure"];
              update({
                measure,
                column:
                  measure === "count"
                    ? undefined
                    : dataset?.numericColumns.includes(spec.column || "")
                      ? spec.column
                      : dataset?.numericColumns[0],
                ...(measure === "count"
                  ? ({ format: "number", scale: "none" } as const)
                  : {}),
              });
            }}
          >
            <option value="count">Count rows</option>
            <option value="sum" disabled={!dataset?.numericColumns.length}>
              Sum
            </option>
            <option value="avg" disabled={!dataset?.numericColumns.length}>
              Average
            </option>
          </select>
        </label>
        {spec.measure !== "count" && (
          <label className="hf-field">
            Numeric column
            <select
              required
              value={spec.column || ""}
              onChange={(e) => update({ column: e.target.value })}
            >
              <option value="">Choose column</option>
              {dataset?.numericColumns.map((c) => (
                <option key={c}>{c}</option>
              ))}
            </select>
          </label>
        )}
        <label className="hf-field">
          Time window
          <select
            value={spec.timeColumn || ""}
            onChange={(e) =>
              update({ timeColumn: e.target.value || undefined })
            }
          >
            <option value="">All rows</option>
            {dataset?.timeColumns.map((c) => (
              <option key={c} value={c}>
                Last 30 days by {c}
              </option>
            ))}
          </select>
        </label>
        {spec.measure !== "count" && (
          <>
            <label className="hf-field">
              Format
              <select
                value={spec.format}
                onChange={(e) =>
                  update({
                    format: e.target.value as PinnedMetricSpec["format"],
                  })
                }
              >
                <option value="number">Number</option>
                <option value="currency">Currency (USD)</option>
                <option value="percent">Percent</option>
              </select>
            </label>
            <label className="hf-field">
              Units
              <select
                value={spec.scale}
                onChange={(e) =>
                  update({ scale: e.target.value as PinnedMetricSpec["scale"] })
                }
              >
                <option value="none">As stored</option>
                <option value="cents">
                  Minor units / cents (divide by 100)
                </option>
              </select>
            </label>
          </>
        )}
        <label className="hf-field">
          Description
          <input
            maxLength={200}
            value={spec.why}
            onChange={(e) => update({ why: e.target.value })}
          />
        </label>
        {error && <p role="alert">{error}</p>}
        <div className="buttons">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!dataset}
          >
            Save metric
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
