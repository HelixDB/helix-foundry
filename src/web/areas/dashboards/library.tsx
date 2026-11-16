import { useEffect, useMemo, useRef, useState, type RefObject } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Clock, Ellipsis, ExternalLink, Link2 } from "lucide-react";
import type { FoundryDashboard } from "../../../shared/wfe";
import {
  DropdownMenu,
  MenuItem,
  Tag,
  cx,
  formatDate,
  formatRelative,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { useCopyLink } from "./publish";
import {
  linkState,
  linkText,
  parseSpec,
  previewRows,
  type PreviewTile,
  type TileGlyph,
} from "./model";
import { dashboardPaths as paths } from "./paths";
import { dashboardQueries } from "./queries";

// The library's cards (/dashboards): a miniature of the dashboard's first
// rows of tiles, cropped with a fade, then its name, when it was edited and
// whether it is pinned or published, and a menu to copy its link for the
// workspace or open it. The list leaves specs out, so a card
// reads its dashboard (GET, no tile read) once it is near the view; the
// miniature is drawn from the spec alone, never from a tile's data, since
// tile reads share each ontology's limiter of one read a second. The name's
// link covers the card, so its title holds the description and edit date.

/** Whether `ref` has come near the view (once true, it stays). */
function useNear(ref: RefObject<HTMLElement | null>) {
  const [near, setNear] = useState(
    () => typeof IntersectionObserver === "undefined",
  );
  useEffect(() => {
    const el = ref.current;
    if (near || !el) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setNear(true);
      },
      { rootMargin: "200px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [near, ref]);
  return near;
}

/** A tile's drawing in the miniature: a number, bars, a line or rows. */
function Glyph({ glyph }: { glyph: TileGlyph }) {
  switch (glyph) {
    case "figure":
      return <span className="db-mini-figure" />;
    case "bar":
      return (
        <svg
          className="db-mini-chart"
          viewBox="0 0 60 28"
          preserveAspectRatio="none"
        >
          {[14, 22, 10, 26, 18, 8].map((h, i) => (
            <rect
              key={i}
              x={2 + i * 10}
              y={28 - h}
              width={6}
              height={h}
              rx={1}
            />
          ))}
        </svg>
      );
    case "line":
      return (
        <svg
          className="db-mini-chart"
          viewBox="0 0 60 28"
          preserveAspectRatio="none"
        >
          <polyline points="0,22 10,18 20,20 30,11 40,14 50,6 60,9" />
        </svg>
      );
    case "table":
      return (
        <span className="db-mini-table">
          {[0, 1, 2, 3].map((i) => (
            <span key={i} />
          ))}
        </span>
      );
  }
}

/** A dashboard's first rows, schematic: a header line, then each row's tiles. */
function Miniature({
  rows,
}: {
  // Undefined while the spec is read; null when it couldn't be.
  rows: PreviewTile[][] | null | undefined;
}) {
  return (
    <div className="db-mini" aria-hidden>
      <span className="db-mini-head" />
      {rows === undefined ? (
        <div className="db-mini-row">
          <span className="db-mini-tile is-waiting" />
          <span className="db-mini-tile is-waiting" />
        </div>
      ) : rows === null ? (
        <div className="db-mini-row">
          <span className="db-mini-tile is-empty">Preview unavailable</span>
        </div>
      ) : !rows.length ? (
        <div className="db-mini-row">
          <span className="db-mini-tile is-empty">No tiles yet</span>
        </div>
      ) : (
        rows.map((row) => (
          <div
            key={row.map((t) => t.id).join(" ")}
            className={cx(
              "db-mini-row",
              row.every((t) => t.glyph === "figure") && "is-figures",
            )}
          >
            {row.map((t) => (
              <span key={t.id} className={cx("db-mini-tile", `is-${t.glyph}`)}>
                <span className="db-mini-title">{t.title}</span>
                <Glyph glyph={t.glyph} />
              </span>
            ))}
          </div>
        ))
      )}
    </div>
  );
}

/** One dashboard in the library: its miniature, name, edit time and badges; it opens the dashboard. */
export function DashboardCard({
  dashboard: d,
  now,
}: {
  dashboard: FoundryDashboard;
  now: number;
}) {
  const { id: ws } = useWorkspace(),
    copy = useCopyLink();
  const ref = useRef<HTMLElement>(null),
    near = useNear(ref);
  const read = useQuery({
    ...dashboardQueries.dashboard(ws, d.id),
    enabled: near,
    // The miniature changes only with the spec; the page reads it afresh.
    staleTime: 5 * 60_000,
  });
  const spec = read.data?.dashboard.specJson;
  const rows = useMemo(() => {
    if (spec === undefined) return read.isError ? null : undefined;
    const parsed = parseSpec(spec);
    return parsed ? previewRows(parsed) : [];
  }, [spec, read.isError]);
  const name = d.name || "Untitled",
    link = linkState(d.share, (iso) => formatDate(iso, false));
  return (
    <article ref={ref} className="db-card">
      <Miniature rows={rows} />
      <div className="db-card-body">
        <h2 className="db-card-name">
          <Link
            to={paths.dashboard(d.id)}
            className="db-card-link"
            title={[
              name,
              d.description,
              d.updatedAt && `Edited ${formatDate(d.updatedAt)}`,
            ]
              .filter(Boolean)
              .join("\n")}
          >
            {name}
          </Link>
        </h2>
        {d.description && <p className="db-card-desc">{d.description}</p>}
        <div className="db-card-meta">
          {d.updatedAt && (
            <span className="db-card-time">
              <Clock size={13} aria-hidden />
              {formatRelative(d.updatedAt, now)}
            </span>
          )}
          {d.pinned && <Tag tone="accent">Pinned</Tag>}
          {link && (
            <Tag tone={link.live ? "info" : "warning"}>{linkText(link)}</Tag>
          )}
          <DropdownMenu
            label={`More actions for ${name}`}
            align="end"
            className="db-card-menu"
            triggerClassName="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
            trigger={<Ellipsis size={15} aria-hidden />}
          >
            <MenuItem icon={Link2} onSelect={() => copy(d, "card")}>
              Copy link
            </MenuItem>
            <MenuItem icon={ExternalLink} to={paths.dashboard(d.id)}>
              Open
            </MenuItem>
          </DropdownMenu>
        </div>
      </div>
    </article>
  );
}
