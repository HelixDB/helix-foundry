import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowLeft,
  ArrowRight,
  ArrowUp,
  Ellipsis,
  LayoutDashboard,
  Maximize2,
  Move,
  Pencil,
  Pin,
  PinOff,
  Sparkles,
  Trash2,
} from "lucide-react";
import { FOUNDER } from "../../../shared/deployment";
import type {
  DashboardSpec,
  DashboardTile,
  FoundryOntology,
  VisualKind,
} from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  DropdownMenu,
  EmptyState,
  MenuItem,
  MenuSeparator,
  Page,
  PageHeader,
  Select,
  SkeletonText,
  Tag,
  cx,
  formatDate,
  formatRelative,
  useNow,
} from "../../kit";
import { ApiError, sentence } from "../../api";
import { RefreshFailed, useWorkspace } from "../../ui";
import { ontologyPaths } from "../ontology/paths";
import { ontologyQueries } from "../ontology/queries";
import { parseQuery, queryText } from "../ontology/ask-model";
import {
  MAX_DESCRIPTION,
  MAX_NAME,
  MAX_TITLE,
  applyEdit,
  dashboardName,
  dashboardRefusal,
  linkState,
  linkText,
  loadReads,
  movesOf,
  parseSpec,
  tileRows,
  tileTitle,
  type Direction,
  type TileEdit,
} from "./model";
import { dashboardPaths as paths } from "./paths";
import {
  dashboardQueries,
  deleteDashboard,
  keepDashboard,
  pinDashboard,
  updateDashboard,
} from "./queries";
import { ShareDialog, ShareMenu, sharing } from "./publish";
import { Tile, type TileState } from "./tile";

// /dashboards/:dashboardId: a dashboard's tiles in their rows, each read by
// the viewer through its load's :read (the tiles of one load in one call),
// and what the viewer may do with it, as the WFE says (canEdit, canPin,
// canPublish, canUnpublish, canDelete): Edit its name, description and
// tiles (retitle, show as, arrange, remove, and see the read each makes),
// Share it (Copy link, for the workspace; or, for owners and admins, a
// public link: publish.tsx), Pin it for the whole workspace and Delete it.

/**
 * What an edit holds until it is saved: the dashboard it started from (the
 * revision it saves against, and what it is compared with, so a refetch
 * meanwhile never makes it overwrite someone else's change unseen), then
 * the name, description and spec, and each tile's title as typed.
 */
type Draft = {
  base: {
    revision: string;
    name: string;
    description: string;
    spec: DashboardSpec;
  };
  name: string;
  description: string;
  spec: DashboardSpec;
  titles: Record<string, string>;
};

const SHOW_AS: { value: VisualKind | ""; label: string }[] = [
  { value: "", label: "As its result suits" },
  { value: "table", label: "Table" },
  { value: "bar", label: "Bar chart" },
  { value: "line", label: "Line chart" },
  { value: "figure", label: "One figure" },
];

const MOVES: { to: Direction; label: string; icon: typeof ArrowUp }[] = [
  { to: "left", label: "Move left", icon: ArrowLeft },
  { to: "right", label: "Move right", icon: ArrowRight },
  { to: "up", label: "Move up", icon: ArrowUp },
  { to: "down", label: "Move down", icon: ArrowDown },
];

/** The tiles' reads, and each tile's state from them. */
function useTileStates(spec: DashboardSpec | undefined) {
  const { id: ws } = useWorkspace();
  const ontologies = useQuery(ontologyQueries.list(ws));
  const byId = useMemo(
    () =>
      new Map<string, FoundryOntology>(
        (ontologies.data?.ontologies || []).map((o) => [o.id, o]),
      ),
    [ontologies.data],
  );
  const tiles = spec?.tiles || [];
  // A tile of an ontology the list doesn't hold is gone; the rest read.
  const present = ontologies.data
    ? tiles.filter((t) => byId.has(t.ontology_id))
    : tiles;
  const { reads, unloaded } = loadReads(present, (o) =>
    ontologies.data ? (byId.get(o)?.currentLoad?.id ?? null) : undefined,
  );
  const results = useQueries({
    queries: reads.map((r) => dashboardQueries.read(ws, r)),
  });
  return (t: DashboardTile): TileState => {
    // A list that failed before it was ever read (a refetch that fails
    // keeps the last list).
    if (!ontologies.data)
      return ontologies.isError
        ? {
            kind: "failed",
            message: ontologies.error.message,
            retry: () => void ontologies.refetch(),
          }
        : { kind: "waiting" };
    if (!byId.has(t.ontology_id)) return { kind: "gone" };
    if (unloaded.includes(t.id)) return { kind: "unloaded" };
    const i = reads.findIndex((r) => r.tiles.some((x) => x.id === t.id));
    const read = reads[i],
      result = results[i];
    if (!read || !result || result.isPending) return { kind: "waiting" };
    if (result.isError)
      return {
        kind: "failed",
        message: result.error.message,
        retry: () => void result.refetch(),
      };
    const drawn = result.data.get(t.id);
    if (!drawn || drawn.kind === "failed")
      return { kind: "failed", message: drawn?.message || "no answer" };
    return {
      kind: "drawn",
      output: drawn.output,
      query: drawn.query,
      unfilled: drawn.unfilled,
      ontologyId: read.ontologyId,
      loadId: read.loadId,
    };
  };
}

export function DashboardPage() {
  const { dashboardId = "" } = useParams();
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    now = useNow(60_000);
  const read = useQuery(dashboardQueries.dashboard(ws, dashboardId));
  const d = read.data?.dashboard;
  const spec = useMemo(
    () => (d ? parseSpec(d.specJson || "") : undefined),
    [d],
  );
  const stateOf = useTileStates(spec);
  const [draft, setDraft] = useState<Draft | null>(null),
    [saving, setSaving] = useState(false),
    [error, setError] = useState(""),
    [changed, setChanged] = useState(false),
    [shareOpen, setShareOpen] = useState(false),
    [deleting, setDeleting] = useState(false),
    [busy, setBusy] = useState(false);
  const crumbs = [{ label: "Dashboards", to: paths.dashboards() }];
  if (read.isPending)
    return (
      <Page width="wide">
        <PageHeader breadcrumbs={crumbs} title="Dashboard" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!d)
    return (
      <Page width="wide">
        <PageHeader breadcrumbs={crumbs} title="Dashboard" />
        <EmptyState
          icon={LayoutDashboard}
          title={
            read.error instanceof ApiError && read.error.status === 404
              ? "There’s no such dashboard"
              : "Couldn’t open the dashboard"
          }
          text={read.error?.message}
          action={
            <>
              <Button onClick={() => void read.refetch()}>Retry</Button>
              <ButtonLink to={paths.dashboards()}>All dashboards</ButtonLink>
            </>
          }
        />
      </Page>
    );
  if (!spec)
    return (
      <Page width="wide">
        <PageHeader breadcrumbs={crumbs} title={d.name || "Dashboard"} />
        <EmptyState
          icon={LayoutDashboard}
          title="This dashboard can’t be shown"
          text="Its tiles aren’t in a form this page reads. Reload the page to try again."
        />
      </Page>
    );

  const editing = !!draft,
    shown = draft?.spec ?? spec,
    rows = tileRows(shown);
  const startEdit = () => {
    setError("");
    setChanged(false);
    const base = {
      revision: d.revision || "",
      name: d.name || "",
      description: d.description || "",
      spec,
    };
    setDraft({
      base,
      name: base.name,
      description: base.description,
      spec,
      titles: Object.fromEntries(spec.tiles.map((t) => [t.id, t.title])),
    });
  };
  const edit = (e: TileEdit) =>
    setDraft((x) => x && { ...x, spec: applyEdit(x.spec, e) });
  const save = async () => {
    if (!draft) return;
    const name = dashboardName(draft.name);
    if (!name) return setError("A name is 1 to 120 characters.");
    let next = draft.spec;
    for (const t of next.tiles) {
      const title = tileTitle(draft.titles[t.id] ?? t.title);
      if (!title)
        return setError(
          `Give every tile a title of 1 to ${MAX_TITLE} characters.`,
        );
      next = applyEdit(next, { kind: "title", tile: t.id, title });
    }
    const { base } = draft,
      description = draft.description.trim(),
      tiles = JSON.stringify(next) !== JSON.stringify(base.spec);
    if (name === base.name && description === base.description && !tiles)
      return setDraft(null);
    setSaving(true);
    setError("");
    try {
      const saved = await updateDashboard(ws, d.id, {
        expectedRevision: base.revision,
        ...(name !== base.name ? { name } : {}),
        ...(description !== base.description ? { description } : {}),
        ...(tiles ? { spec: next } : {}),
      });
      keepDashboard(qc, ws, saved);
      setDraft(null);
      notify("Dashboard saved.");
    } catch (e) {
      if (dashboardRefusal(e) === "FOUNDRY_DASHBOARD_CHANGED") {
        setChanged(true);
        void read.refetch();
      } else setError(sentence((e as Error).message));
    } finally {
      setSaving(false);
    }
  };
  const pin = async (pinned: boolean) => {
    setBusy(true);
    try {
      keepDashboard(
        qc,
        ws,
        await pinDashboard(ws, d.id, pinned, d.revision || ""),
      );
      notify(
        pinned
          ? "Pinned for everyone in the workspace."
          : "Unpinned for everyone in the workspace.",
      );
    } catch (e) {
      if (dashboardRefusal(e) === "FOUNDRY_DASHBOARD_CHANGED") {
        void read.refetch();
        notify(
          "Someone changed this dashboard since you opened it. Look it over, then try again.",
          true,
        );
      } else notify(sentence((e as Error).message), true);
    } finally {
      setBusy(false);
    }
  };
  const link = linkState(d.share, (iso) => formatDate(iso, false));

  const meta = (
    <>
      {d.pinned && <Tag tone="accent">Pinned</Tag>}
      {link && (
        <Tag tone={link.live ? "info" : "warning"}>{linkText(link)}</Tag>
      )}
      {d.updatedAt && (
        <span title={formatDate(d.updatedAt)}>
          Edited {formatRelative(d.updatedAt, now)}
        </span>
      )}
    </>
  );
  const actions = editing ? (
    <>
      <Button onClick={() => setDraft(null)} disabled={saving}>
        Cancel
      </Button>
      <Button
        variant="primary"
        pending={saving}
        disabled={changed}
        onClick={() => void save()}
      >
        Save changes
      </Button>
    </>
  ) : (
    <>
      {d.canEdit && (
        <Button icon={Pencil} onClick={startEdit}>
          Edit
        </Button>
      )}
      <ShareMenu dashboard={d} onPublic={() => setShareOpen(true)} />
      {(d.canPin || d.canDelete) && (
        <DropdownMenu
          label="More actions"
          align="end"
          triggerClassName="hf-btn hf-btn--secondary hf-btn--md hf-btn--icon"
          trigger={<Ellipsis size={16} aria-hidden />}
        >
          {d.canPin && (
            <MenuItem
              icon={d.pinned ? PinOff : Pin}
              disabled={busy}
              onSelect={() => void pin(!d.pinned)}
            >
              {d.pinned ? "Unpin for everyone" : "Pin for everyone"}
            </MenuItem>
          )}
          {d.canPin && d.canDelete && <MenuSeparator />}
          {d.canDelete && (
            <MenuItem icon={Trash2} danger onSelect={() => setDeleting(true)}>
              Delete dashboard
            </MenuItem>
          )}
        </DropdownMenu>
      )}
    </>
  );

  return (
    <Page width="wide" className="db-page">
      <PageHeader
        breadcrumbs={[...crumbs, { label: d.name || "Dashboard" }]}
        title={editing ? `Editing ${d.name}` : d.name || "Dashboard"}
        documentTitle={d.name || "Dashboard"}
        subtitle={editing ? undefined : d.description || undefined}
        meta={editing ? undefined : meta}
        actions={actions}
      />
      <RefreshFailed query={read} />
      {draft && (
        <section className="db-edit-head" aria-label="Name and description">
          <label className="hf-field">
            <span>Name</span>
            <input
              value={draft.name}
              maxLength={MAX_NAME}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </label>
          <label className="hf-field">
            <span>Description (optional)</span>
            <textarea
              rows={2}
              value={draft.description}
              maxLength={MAX_DESCRIPTION}
              onChange={(e) =>
                setDraft({ ...draft, description: e.target.value })
              }
            />
          </label>
          {changed && (
            <div className="db-callout" role="alert">
              <p>
                Someone changed this dashboard while you were editing it.
                Discard your edits to see theirs, then edit it again.
              </p>
              <Button size="sm" onClick={() => setDraft(null)}>
                Discard my edits
              </Button>
            </div>
          )}
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </section>
      )}
      {!rows.length ? (
        <EmptyState
          icon={LayoutDashboard}
          title="No tiles yet"
          text={
            editing
              ? "Every tile is removed. Save to keep it empty, or Cancel to keep its tiles."
              : d.canEdit
                ? FOUNDER
                  ? "Pin a chart or table from an answer, or ask Founder to build tiles for you."
                  : "Pin a chart or table from an Analyst answer."
                : "Whoever made it, or an owner or admin, adds its tiles."
          }
          action={
            !editing &&
            d.canEdit && (
              <ButtonLink to="/ask" icon={Sparkles}>
                {FOUNDER ? "Open Founder" : "Open the Analyst"}
              </ButtonLink>
            )
          }
        />
      ) : (
        <div className="db-grid">
          {rows.map((row) => (
            <div
              key={row.map((t) => t.id).join(" ")}
              className={cx("db-row", `db-row--${row.length}`)}
            >
              {row.map((t) =>
                draft ? (
                  <EditableTile
                    key={t.id}
                    tile={t}
                    state={stateOf(t)}
                    spec={draft.spec}
                    title={draft.titles[t.id] ?? t.title}
                    onTitle={(title) =>
                      setDraft({
                        ...draft,
                        titles: { ...draft.titles, [t.id]: title },
                      })
                    }
                    onEdit={edit}
                  />
                ) : (
                  <div key={t.id} className="db-tile">
                    <Tile tile={t} state={stateOf(t)} />
                  </div>
                ),
              )}
            </div>
          ))}
        </div>
      )}
      {shareOpen && (sharing(d).publish || sharing(d).unpublish) && (
        <ShareDialog dashboard={d} onClose={() => setShareOpen(false)} />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${d.name}?`}
          description={
            d.share
              ? "Its tiles go for everyone in the workspace, and its public link stops showing anything. The data they read stays."
              : "Its tiles go for everyone in the workspace. The data they read stays."
          }
          confirmLabel="Delete dashboard"
          tone="danger"
          pending={busy}
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            setBusy(true);
            try {
              await deleteDashboard(ws, d.id);
              qc.removeQueries({
                queryKey: dashboardQueries.dashboard(ws, d.id).queryKey,
              });
              await qc.invalidateQueries({
                queryKey: dashboardQueries.list(ws).queryKey,
              });
              notify(`Deleted ${d.name}.`);
              navigate(paths.dashboards());
            } catch (e) {
              notify((e as Error).message, true);
              setDeleting(false);
            } finally {
              setBusy(false);
            }
          }}
        />
      )}
    </Page>
  );
}

/** A tile while the dashboard is edited: its title, how it shows, where it sits, and the read it makes. */
function EditableTile({
  tile,
  state,
  spec,
  title,
  onTitle,
  onEdit,
}: {
  tile: DashboardTile;
  state: TileState;
  spec: DashboardSpec;
  title: string;
  onTitle: (title: string) => void;
  onEdit: (e: TileEdit) => void;
}) {
  const { id: ws } = useWorkspace();
  const ontologies = useQuery(ontologyQueries.list(ws));
  const ontology = ontologies.data?.ontologies?.find(
    (o) => o.id === tile.ontology_id,
  );
  const moves = movesOf(spec, tile.id),
    named = title.trim() || tile.title,
    query = parseQuery(tile.query),
    text = query ? queryText(query) : undefined;
  const arrange = (
    <DropdownMenu
      label={`Arrange ${named}`}
      align="end"
      triggerClassName="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
      trigger={<Move size={15} aria-hidden />}
    >
      {MOVES.map((m) => (
        <MenuItem
          key={m.to}
          icon={m.icon}
          disabled={!moves[m.to]}
          onSelect={() => onEdit({ kind: "move", tile: tile.id, to: m.to })}
        >
          {m.label}
        </MenuItem>
      ))}
      <MenuItem
        icon={Maximize2}
        disabled={!moves.full_width}
        onSelect={() => onEdit({ kind: "full_width", tile: tile.id })}
      >
        Full width
      </MenuItem>
      <MenuSeparator />
      <MenuItem
        icon={Trash2}
        danger
        onSelect={() => onEdit({ kind: "remove", tile: tile.id })}
      >
        Remove tile
      </MenuItem>
    </DropdownMenu>
  );
  return (
    <section className="db-tile is-editing" aria-label={`Tile: ${named}`}>
      <div className="db-tile-edit">
        <label className="hf-field">
          <span>Title</span>
          <input
            value={title}
            maxLength={MAX_TITLE}
            aria-invalid={!tileTitle(title)}
            onChange={(e) => onTitle(e.target.value)}
          />
        </label>
        <div className="hf-field">
          <span>Show as</span>
          <Select
            label={`Show ${named} as`}
            value={tile.visual.kind ?? ""}
            options={SHOW_AS}
            onChange={(v) =>
              onEdit({
                kind: "visual",
                tile: tile.id,
                visual: v === "" ? null : v,
              })
            }
          />
        </div>
      </div>
      <Tile tile={{ ...tile, title: named }} state={state} actions={arrange} />
      <details className="db-read">
        <summary>The read</summary>
        <p>
          {tile.load.kind === "current"
            ? "Reads the current load of "
            : "Reads one load, kept as it was, of "}
          {ontology ? (
            <Link to={ontologyPaths.ontology(ontology.id)}>
              {ontology.name}
            </Link>
          ) : (
            "an ontology you can’t open"
          )}
          {text
            ? `: ${text.path}${text.filters.length ? `, ${text.filters.join(", ")}` : ""}; ${text.returns}.`
            : "."}
        </p>
        <pre className="db-code">
          <code>{JSON.stringify(tile.query, null, 2)}</code>
        </pre>
      </details>
    </section>
  );
}
