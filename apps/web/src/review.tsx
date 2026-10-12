import { useState, lazy, Suspense } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "./api";
import {
  useWorkspace,
  useResources,
  useAction,
  DataTable,
  Modal,
  Loading,
  Menu,
  fieldName,
} from "./ui";
import { PageHeader } from "./kit";
import { paths } from "./paths";
import { ProposalChanges } from "./proposal-changes";
import type { Resource } from "../../../packages/shared/src";
const ResultPreview = lazy(() =>
  import("./charts").then((m) => ({ default: m.ResultPreview })),
);
export function ProposalReview({ proposal: p }: { proposal: Resource }) {
  const { id, role, ask } = useWorkspace(),
    action = useAction(),
    navigate = useNavigate(),
    [editing, setEditing] = useState(false),
    [json, setJson] = useState("");
  const b = p.data.bundle,
    v = p.data.validation,
    failed = v.checks.filter((c: any) => !c.passed),
    passed = v.checks.filter((c: any) => c.passed),
    published = p.data.status === "published";
  return (
    <section className="proposal-review">
      <h2>{b.summary}</h2>
      {failed.length > 0 && (
        <div className="error-box" role="alert">
          <strong>These checks need attention before publishing</strong>
          {failed.map((c: any) => (
            <p key={c.name}>
              {c.name}: {c.detail}
            </p>
          ))}
        </div>
      )}
      <ProposalChanges proposal={p} />
      {published && <p className="muted">Published to your workspace</p>}
      {b.dashboards.map((d: any) => (
        <section key={d.name} className="review-result">
          <h3>{d.name}</h3>
          <div className="preview-grid">
            {d.widgets.map((w: any, i: number) => {
              const rows = v.previews["chart:" + d.name + ":" + i];
              return (
                <div key={i}>
                  <h4>{w.title}</h4>
                  {Array.isArray(rows) ? (
                    <Suspense fallback={<Loading />}>
                      <ResultPreview widget={w} rows={rows} />
                    </Suspense>
                  ) : (
                    <p className="muted">
                      Preview unavailable until validation passes.
                    </p>
                  )}
                </div>
              );
            })}
          </div>
          <p className="muted small">Tested snapshot preview</p>
        </section>
      ))}
      {b.ontology.length > 0 && (
        <section className="review-result">
          <h3>Business records</h3>
          <div className="simple-list">
            {b.ontology.map((o: any) => (
              <div className="simple-row" key={o.name}>
                <div>
                  <strong>{o.name}</strong>
                  <p>{o.properties.map(fieldName).join(" · ")}</p>
                </div>
              </div>
            ))}
          </div>
          {b.relationships.map((r: any) => (
            <p className="muted" key={r.name}>
              {r.fromType} → {r.toType} · {r.name}
            </p>
          ))}
        </section>
      )}
      {b.assumptions.length > 0 && (
        <section className="review-result">
          <h3>Business assumptions</h3>
          <ul>
            {b.assumptions.map((s: string) => (
              <li key={s}>{s}</li>
            ))}
          </ul>
        </section>
      )}
      <details className="disclosure">
        <summary>Details · {passed.length} checks passed</summary>
        {passed.map((c: any) => (
          <p key={c.name}>
            <strong>{c.name}</strong> — {c.detail}
          </p>
        ))}
        <h3>Prepared transformations</h3>
        {b.pipelines.map((p: any) => (
          <p key={p.name}>
            {p.name} — {p.description}
          </p>
        ))}
        <details>
          <summary>Definitions and evidence</summary>
          <pre>{JSON.stringify(b, null, 2)}</pre>
          {Object.entries(v.previews)
            .filter(([k]) => !k.startsWith("chart:"))
            .map(([k, rows]: any) => (
              <div key={k}>
                <h4>{k}</h4>
                <DataTable
                  rows={Array.isArray(rows) ? rows : rows?.rows || []}
                />
                {rows?.before && (
                  <details>
                    <summary>Before</summary>
                    <DataTable rows={rows.before} />
                  </details>
                )}
              </div>
            ))}
        </details>
      </details>
      {role !== "viewer" && (
        <div className="review-footer">
          <div className="buttons">
            {!published && (
              <>
                <button
                  className="button primary"
                  disabled={action.pending || p.data.status !== "ready"}
                  onClick={() =>
                    action.run(async () => {
                      await api(`/workspaces/${id}/proposals/${p.id}/publish`, {
                        hash: p.data.hash,
                      });
                      navigate(paths.home());
                    }, "Changes published")
                  }
                >
                  Publish
                </button>
                <button
                  className="button"
                  onClick={() =>
                    ask({
                      resourceId: p.id,
                      title: "this proposal",
                      prompt: "Revise this proposal to ",
                    })
                  }
                >
                  Request changes
                </button>
              </>
            )}
          </div>
          <Menu>
            <button
              disabled={action.pending}
              onClick={() =>
                action.run(async () => {
                  const next = await api(
                    `/workspaces/${id}/proposals/${p.id}/validate`,
                    {},
                  );
                  navigate(paths.proposals(next.id));
                })
              }
            >
              Retest against current data
            </button>
            <button
              onClick={() => {
                setJson(JSON.stringify(b, null, 2));
                setEditing(true);
              }}
            >
              Edit definitions
            </button>
            {!published && (
              <button
                onClick={() =>
                  action.run(
                    () => api(`/workspaces/${id}/proposals/${p.id}/reject`, {}),
                    "Proposal rejected",
                  )
                }
              >
                Reject proposal
              </button>
            )}
          </Menu>
        </div>
      )}
      {editing && (
        <Modal title="Edit definitions" onClose={() => setEditing(false)}>
          <div className="pad">
            <textarea
              aria-label="Definitions JSON"
              className="code-editor tall"
              value={json}
              onChange={(e) => setJson(e.target.value)}
            />
            <button
              className="button primary"
              disabled={action.pending}
              onClick={() =>
                action.run(async () => {
                  const next = await api(
                    `/workspaces/${id}/proposals/${p.id}`,
                    JSON.parse(json),
                    "PUT",
                  );
                  setEditing(false);
                  navigate(paths.proposals(next.id));
                })
              }
            >
              Validate changes
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}
export default function ProposalsPage() {
  const q = useResources("proposal"),
    [params, setParams] = useSearchParams();
  const p =
    q.data?.items.find((p: any) => p.id === params.get("selected")) ||
    q.data?.items.find((p: any) => p.data.status === "ready") ||
    q.data?.items[0];
  return (
    <div className="reading-width">
      <PageHeader title="Review changes" />
      {q.isPending ? (
        <Loading />
      ) : q.error ? (
        <p role="alert">{q.error.message}</p>
      ) : p ? (
        <>
          <label className="compact-selector">
            Proposal
            <select
              aria-label="Proposal"
              value={p.id}
              onChange={(e) => setParams({ selected: e.target.value })}
            >
              {q.data.items.map((p: any) => (
                <option key={p.id} value={p.id}>
                  {p.name} · {p.data.status}
                </option>
              ))}
            </select>
          </label>
          <ProposalReview key={p.id} proposal={p} />
        </>
      ) : (
        <p className="muted">No proposed changes to review.</p>
      )}
    </div>
  );
}
