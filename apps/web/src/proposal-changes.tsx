import { useQuery } from "@tanstack/react-query";
import type { Resource } from "../../../packages/shared/src";
import { api } from "./api";
import { useWorkspace } from "./ui";

export function ProposalChanges({ proposal }: { proposal: Resource }) {
  const { id } = useWorkspace();
  const result = useQuery({
    queryKey: [id, "proposal-comparison", proposal.id, proposal.revision],
    enabled: proposal.data.status !== "published",
    queryFn: () => api(`/workspaces/${id}/proposals/${proposal.id}/comparison`),
  });
  if (proposal.data.status === "published") return null;
  return (
    <section className="review-result" aria-label="Definition changes">
      <h3>Definition changes</h3>
      <p className="muted">
        Compared with the currently published definitions. This describes
        definition changes, not how many data records will change.
      </p>
      {result.isPending ? (
        <p>Loading comparison…</p>
      ) : result.error ? (
        <p role="alert">{result.error.message}</p>
      ) : (
        <>
          {result.data.stale && (
            <p role="alert">
              Inputs or published definitions changed since this proposal was
              tested. Retest before publishing.
            </p>
          )}
          <p>
            {result.data.counts.added} added · {result.data.counts.changed}{" "}
            changed · {result.data.counts.removed} removed
          </p>
          {!result.data.changes.length && <p>No asset definition changes.</p>}
          {result.data.changes.map((c: any) => (
            <details key={c.kind + ":" + c.name} className="disclosure">
              <summary>
                {c.status[0].toUpperCase() + c.status.slice(1)} {c.kind} ·{" "}
                {c.name}
              </summary>
              {c.status === "changed" && (
                <p>Changed fields: {c.fields.join(", ")}</p>
              )}
              {c.before && (
                <>
                  <h4>Current definition</h4>
                  <pre>{JSON.stringify(c.before, null, 2)}</pre>
                </>
              )}
              {c.after && (
                <>
                  <h4>Proposed definition</h4>
                  <pre>{JSON.stringify(c.after, null, 2)}</pre>
                </>
              )}
            </details>
          ))}
          {result.data.metadata.map((m: any) => (
            <details key={m.field} className="disclosure">
              <summary>Updated {m.field}</summary>
              <h4>Current</h4>
              <pre>{JSON.stringify(m.before, null, 2)}</pre>
              <h4>Proposed</h4>
              <pre>{JSON.stringify(m.after, null, 2)}</pre>
            </details>
          ))}
        </>
      )}
    </section>
  );
}
