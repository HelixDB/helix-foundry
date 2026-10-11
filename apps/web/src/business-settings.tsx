// Settings › Your business: the industry in the owner's own words, and what
// Foundry knows about the business (targets, normal ranges, facts). Both are
// optional; Foundry infers the industry and starts from typical ranges.
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LoaderCircle, Store, X } from "lucide-react";
import type { BusinessInsights } from "../../../packages/shared/src";
import { api } from "./api";
import { useAction, useWorkspace } from "./ui";

const sourceLabel = { you: "You", learned: "Learned", typical: "Typical" };

export function BusinessSettings() {
  const { id, role } = useWorkspace(),
    action = useAction(),
    qc = useQueryClient(),
    editor = role !== "viewer";
  const query = useQuery({
    queryKey: [id, "business"],
    queryFn: () => api<BusinessInsights>(`/workspaces/${id}/business`),
    refetchInterval: (q) => (q.state.data?.drafting ? 4000 : false),
  });
  const b = query.data;
  const [industry, setIndustry] = useState(""),
    [fact, setFact] = useState("");
  // Shows the owner's words, or what Foundry inferred.
  useEffect(() => {
    if (b) setIndustry(b.description || b.industry?.title || "");
  }, [b?.description, b?.industry?.title]);
  const save = (path: string, body?: unknown, method?: string) =>
    action.run(async () => {
      // useAction refreshes everything after, Home's metrics included.
      qc.setQueryData(
        [id, "business"],
        await api<BusinessInsights>(
          `/workspaces/${id}/business${path}`,
          body,
          method,
        ),
      );
    });
  const unchanged =
    industry.trim() === (b?.description || b?.industry?.title || "");
  return (
    <>
      <section className="panel settings-panel">
        <form
          className="pad"
          onSubmit={(e) => {
            e.preventDefault();
            void save("", { description: industry }, "PATCH");
          }}
        >
          <label>
            Industry
            <input
              value={industry}
              maxLength={200}
              onChange={(e) => setIndustry(e.target.value)}
              placeholder="For example: specialty coffee roaster, wholesale and online"
              disabled={!editor}
            />
          </label>
          <p className="muted">
            {b?.industry?.source === "inferred" && !b.description
              ? "Worked out from your data. "
              : ""}
            Describe it in your own words if you like, for example “specialty
            coffee roaster, wholesale and online” or “B2B office furniture”.
            Foundry adapts what it looks for.
          </p>
          {b?.drafting && (
            <p className="muted" role="status">
              <LoaderCircle size={13} className="spin" aria-hidden /> Learning
              how to read this kind of business…
            </p>
          )}
          {editor && (
            <button className="button" disabled={unchanged || action.pending}>
              <Store size={15} /> Save
            </button>
          )}
        </form>
      </section>
      <section className="panel settings-panel">
        <div className="pad">
          <h3>What Foundry knows about your business</h3>
          <p className="muted">
            Used to judge whether a number looks usual. Yours always win over
            typical ranges.
          </p>
          {b && b.knowledge.length > 0 && (
            <ul className="business-knowledge">
              {b.knowledge.map((k) => (
                <li
                  key={k.id}
                  className={k.source === "typical" ? "is-typical" : undefined}
                >
                  <span>{k.text}</span>
                  <span className={`business-source is-${k.source}`}>
                    {sourceLabel[k.source]}
                  </span>
                  {editor && (
                    <button
                      type="button"
                      className="business-remove"
                      aria-label={`Remove “${k.text}”`}
                      onClick={() =>
                        void save(
                          `/knowledge/${encodeURIComponent(k.id)}`,
                          {},
                          "DELETE",
                        )
                      }
                    >
                      <X size={14} />
                    </button>
                  )}
                </li>
              ))}
            </ul>
          )}
          {editor && (
            <form
              className="business-add"
              onSubmit={(e) => {
                e.preventDefault();
                if (!fact.trim()) return;
                void save("/knowledge", { text: fact }).then(() => setFact(""));
              }}
            >
              <label className="hf-sr-only" htmlFor="business-fact">
                Add something Foundry should know
              </label>
              <input
                id="business-fact"
                value={fact}
                maxLength={300}
                onChange={(e) => setFact(e.target.value)}
                placeholder="Add a target, normal range or fact, e.g. “We pause ads every August”"
              />
              <button
                className="button"
                disabled={!fact.trim() || action.pending}
              >
                Add
              </button>
            </form>
          )}
          <p className="muted">
            You can also just tell the Analyst, for example “our normal return
            rate is about 10%”.
          </p>
        </div>
      </section>
    </>
  );
}
