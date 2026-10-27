import type { CSSProperties } from "react";
import { Check, Database } from "lucide-react";
import { number } from "../../api";
import { logoUrl } from "../../kit";
import { useCurrentTheme, type Theme } from "../../shell/theme";
import "./ontology-scene.css";

// helix-foundry's data feed (apps/web/src/ontology-scene.tsx): a job's steps
// and the sources it reads, shown while a generation or load runs. It groups
// items by `group` when set.

const logos: Record<string, string> = {
  planetscale: "planetscale",
  workos: "workos",
  posthog: "posthog",
  github: "github",
  linear: "linear",
  calcom: "calcom",
  granola: "granola",
  stripe: "stripe",
  s3: "s3",
  postgresql: "postgresql",
  postgres: "postgresql",
  neon: "neon",
  supabase: "supabase",
};
const labels: Record<string, string> = {
  planetscale: "PlanetScale",
  workos: "WorkOS",
  posthog: "PostHog",
  github: "GitHub",
  linear: "Linear",
  calcom: "Cal.com",
  granola: "Granola",
  stripe: "Stripe",
  s3: "Amazon S3",
  postgresql: "PostgreSQL",
  postgres: "PostgreSQL",
  neon: "Neon",
  supabase: "Supabase",
  mysql: "MySQL",
  neo4j: "Neo4j",
  upload: "Uploads",
  rest: "REST API",
};
const connectorLogo = (key: string, theme: Theme) =>
  logos[key] ? logoUrl(logos[key], theme) : undefined;
const sourceName = (key: string) =>
  labels[key] || key.charAt(0).toUpperCase() + key.slice(1);

export type FeedItem = {
  id: string;
  name: string;
  source: string;
  // Groups items under this heading instead of the source's name.
  group?: string;
  rows?: number;
  status: "pending" | "loading" | "ready";
};
export type FeedStep = { label: string; status: "pending" | "active" | "done" };

function SourceMark({ source }: { source: string }) {
  const src = connectorLogo(source, useCurrentTheme());
  return src ? (
    <img src={src} alt="" width={16} height={16} />
  ) : (
    <Database size={15} strokeWidth={1.7} aria-hidden="true" />
  );
}

export function DataFeed({
  items,
  steps,
  unit = { one: "table", many: "tables" },
}: {
  items: FeedItem[];
  steps: FeedStep[];
  // What an item is, counted while it is read: tables, or sources when
  // documents sit beside them. Groups with no row counts end in it too.
  unit?: { one: string; many: string };
}) {
  const groups = new Map<string, FeedItem[]>();
  for (const item of items) {
    const key = item.group ?? item.source;
    groups.set(key, [...(groups.get(key) || []), item]);
  }
  const ready = items.filter((i) => i.status === "ready").length;
  const current =
    steps.find((s) => s.status === "active") ||
    steps.findLast((s) => s.status === "done");
  return (
    <div className="data-feed">
      <p className="feed-sr" role="status" aria-live="polite">
        {current ? `${current.label}. ` : ""}
        {items.length ? `${ready} of ${items.length} ${unit.many} read.` : ""}
      </p>
      <div className="feed-grid">
        <ol className="feed-steps">
          {steps.map((s) => (
            <li key={s.label} className={`feed-step is-${s.status}`}>
              <span className="feed-step-mark" aria-hidden="true">
                {s.status === "done" && <Check size={10} strokeWidth={3.2} />}
              </span>
              <span>
                {s.label}
                <span className="feed-sr">
                  {s.status === "done"
                    ? " (done)"
                    : s.status === "active"
                      ? " (in progress)"
                      : ""}
                </span>
              </span>
            </li>
          ))}
        </ol>
        <div className="feed-sources">
          {[...groups].map(([key, rows]) => {
            const source = rows[0]!.source;
            const done = rows.filter((r) => r.status === "ready");
            const total = done.reduce((n, r) => n + (r.rows || 0), 0);
            // One unit per header: tables while any is loading, then records
            // (or the items themselves when they carry no row counts).
            const all = done.length === rows.length,
              counted = rows.some((r) => r.rows !== undefined);
            return (
              <section key={key} className="feed-source">
                <h4>
                  <SourceMark source={source} />
                  <span>{rows[0]!.group ?? sourceName(source)}</span>
                  <small key={all ? "records" : "tables"}>
                    {all && counted
                      ? `${number(total)} ${total === 1 ? "record" : "records"}`
                      : all
                        ? `${rows.length} ${rows.length === 1 ? unit.one : unit.many}`
                        : `${done.length} of ${rows.length} ${rows.length === 1 ? unit.one : unit.many}`}
                  </small>
                </h4>
                <ul>
                  {rows.map((item, i) => (
                    <li
                      key={item.id}
                      className={`feed-item is-${item.status}`}
                      style={{ "--i": i } as CSSProperties}
                    >
                      <span className="feed-item-mark" aria-hidden="true">
                        {item.status === "ready" && (
                          <Check size={10} strokeWidth={3} />
                        )}
                      </span>
                      <span className="feed-item-name" title={item.name}>
                        {item.name.split(" · ").at(-1)}
                      </span>
                      <span className="feed-item-rows">
                        {item.status === "ready" && item.rows !== undefined ? (
                          <>
                            {number(item.rows)}
                            <span className="feed-sr"> records</span>
                          </>
                        ) : item.status === "loading" ? (
                          <span className="feed-shimmer" />
                        ) : item.status === "pending" ? (
                          "Queued"
                        ) : null}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
            );
          })}
        </div>
      </div>
    </div>
  );
}
