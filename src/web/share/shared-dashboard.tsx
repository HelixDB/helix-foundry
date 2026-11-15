import "../areas/dashboards/dashboards.css";
import { useEffect, useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { CircleAlert, LayoutDashboard } from "lucide-react";
import { EmptyState, SkeletonText, cx, formatDate } from "../kit";
import {
  SHARE_TOKEN,
  parseSnapshot,
  type SharedDashboard,
  type SharedTile,
} from "../areas/dashboards/model";
import { AskVisual } from "../areas/ontology/ask-visual";

// A published dashboard's public page (/share/<token>): what its snapshot
// holds, drawn as the app draws a dashboard's tiles, with no app around it,
// no session and nothing to open (its entities name no id). The browser
// asks the foundry-events door for the snapshot (GET
// /v1/shared-dashboards/{token}), sending no credentials and no referrer,
// and keeps nothing.

type View =
  | { kind: "reading" }
  // No snapshot at this link: never one, unpublished, or its ontology gone.
  | { kind: "missing" }
  // The door's limits for this address: try again in a minute.
  | { kind: "busy" }
  | { kind: "failed" }
  | { kind: "shown"; dashboard: SharedDashboard };

const NONE = () => undefined;

function Tile({ tile }: { tile: SharedTile }) {
  if (!tile.drawn)
    return (
      <figure className="ak-visual db-tile-note" aria-label={tile.title}>
        <div className="ak-visual-head">
          <div className="ak-visual-title">{tile.title}</div>
        </div>
        <p className="db-tile-why">
          <CircleAlert size={14} aria-hidden />
          <span>This tile can’t be shown.</span>
        </p>
      </figure>
    );
  return (
    <AskVisual
      output={tile.drawn.output}
      query={tile.drawn.query}
      hint={{ kind: tile.kind, title: tile.title }}
      href={NONE}
    />
  );
}

export function SharedDashboardPage({
  door,
  token,
}: {
  // The door's origin, as the server read it.
  door: string;
  token: string;
}) {
  const [view, setView] = useState<View>(() =>
    SHARE_TOKEN.test(token) ? { kind: "reading" } : { kind: "missing" },
  );
  useEffect(() => {
    if (!SHARE_TOKEN.test(token)) return;
    const abort = new AbortController();
    fetch(`${door}/v1/shared-dashboards/${encodeURIComponent(token)}`, {
      credentials: "omit",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal: abort.signal,
    })
      .then(async (res) => {
        if (res.status === 404) return setView({ kind: "missing" });
        if (res.status === 429) return setView({ kind: "busy" });
        const dashboard = res.ok ? parseSnapshot(await res.json()) : undefined;
        setView(dashboard ? { kind: "shown", dashboard } : { kind: "failed" });
      })
      .catch(() => {
        if (!abort.signal.aborted) setView({ kind: "failed" });
      });
    return () => abort.abort();
  }, [door, token]);
  const shown = view.kind === "shown" ? view.dashboard : undefined;
  useEffect(() => {
    document.title = shown
      ? `${shown.name} · Helix Foundry`
      : "Shared dashboard · Helix Foundry";
  }, [shown]);
  return (
    <div className="db-public">
      <main className="db-public-main">
        {view.kind === "reading" ? (
          <div aria-busy="true">
            <SkeletonText lines={6} />
          </div>
        ) : !shown ? (
          <div className="db-public-state">
            <EmptyState
              icon={LayoutDashboard}
              size="lg"
              title={
                view.kind === "missing"
                  ? "There’s no dashboard at this link"
                  : view.kind === "busy"
                    ? "Too many tries from here"
                    : "Couldn’t open this dashboard"
              }
              text={
                view.kind === "missing"
                  ? "It may have been unpublished, or the link may be mistyped. Ask whoever shared it for a new one."
                  : view.kind === "busy"
                    ? "Wait a minute, then reload the page."
                    : "Reload the page to try again."
              }
            />
          </div>
        ) : (
          <>
            <header className="db-public-head">
              <h1>{shown.name}</h1>
              {shown.description && <p>{shown.description}</p>}
              <div className="db-public-meta">
                <span>Snapshot from {formatDate(shown.publishedAt)}</span>
              </div>
            </header>
            {/* The tiles draw as the app's do, whose links a router keeps;
                none here names anything to open. */}
            <MemoryRouter>
              <div className="db-grid">
                {shown.rows.map((row) => (
                  <div
                    key={row.map((t) => t.id).join(" ")}
                    className={cx("db-row", `db-row--${row.length}`)}
                  >
                    {row.map((t) => (
                      <div key={t.id} className="db-tile">
                        <Tile tile={t} />
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            </MemoryRouter>
          </>
        )}
        {shown && (
          <footer className="db-public-foot">
            A snapshot shared from{" "}
            <a href="/" rel="noreferrer">
              Helix Foundry
            </a>
            . Whoever shared it chose what it shows; it doesn’t change until
            they publish it again.
          </footer>
        )}
      </main>
    </div>
  );
}
