import { useId, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleCheck, Plus, TriangleAlert } from "lucide-react";
import type { IndustryPlaybook } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { Button, ButtonLink, formatRelative } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";

export function IndustryNotes() {
  const { id, role, notify } = useWorkspace(),
    title = useId();
  const client = useQueryClient(),
    key = [id, "industry-playbook"];
  const [pending, setPending] = useState(false);
  const { data } = useQuery({
    queryKey: key,
    queryFn: () => api<IndustryPlaybook>(`/workspaces/${id}/industry`),
    refetchInterval: (q) =>
      ["queued", "running"].includes(q.state.data?.scan || "") ? 2500 : 60000,
  });
  if (!data?.playbook) return null;
  const busy = data.scan === "queued" || data.scan === "running";
  const retry = async () => {
    setPending(true);
    try {
      client.setQueryData(
        key,
        await api<IndustryPlaybook>(`/workspaces/${id}/industry/check`, {}),
      );
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  return (
    <section className="so-industry" aria-labelledby={title}>
      <h2 id={title}>To understand your customers better</h2>
      <p className="so-industry-status" role="status">
        {data.scan === "queued"
          ? "Retail checks queued."
          : busy
            ? "Checking your retail data…"
            : data.scan === "failed"
              ? "Couldn’t refresh the retail checks."
              : "Online retail"}
        {data.checkedAt && ` · Checked ${formatRelative(data.checkedAt)}`}
        {data.stale && " · Earlier data"}
      </p>
      {data.suggestions.length > 0 && (
        <ul>
          {data.suggestions.map((s) => (
            <li key={s.id}>
              <Plus size={14} aria-hidden />
              <span>
                {s.text}
                {s.kind === "connect" && (
                  <>
                    {" "}
                    <ButtonLink
                      size="sm"
                      variant="ghost"
                      to={paths.connections(true)}
                    >
                      Connect a source
                    </ButtonLink>
                  </>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      {data.checks.length > 0 && (
        <details className="so-industry-checks">
          <summary>What we checked</summary>
          <ul>
            {data.checks.map((c) => (
              <li key={c.id}>
                {c.state === "passed" ? (
                  <CircleCheck size={14} aria-hidden />
                ) : (
                  <TriangleAlert size={14} aria-hidden />
                )}
                <span>{c.text}</span>
              </li>
            ))}
          </ul>
        </details>
      )}
      {role !== "viewer" && data.scan !== "running" && (
        <Button
          size="sm"
          variant="ghost"
          pending={pending}
          onClick={() => void retry()}
        >
          {data.scan === "failed"
            ? "Retry retail checks"
            : data.scan === "queued"
              ? "Check now"
              : "Check again"}
        </Button>
      )}
    </section>
  );
}
