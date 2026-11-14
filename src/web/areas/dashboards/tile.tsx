import { useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { CircleAlert } from "lucide-react";
import type { EntityRef } from "../../../shared/foundry-read";
import type {
  DashboardTile,
  OntologyQuery,
  QueryOutput,
} from "../../../shared/wfe";
import { Button, Skeleton } from "../../kit";
import { useWorkspace } from "../../ui";
import { entityId } from "../explore/model";
import { namingOf } from "../explore/naming";
import { AskVisual } from "../ontology/ask-visual";
import { withLoad } from "../ontology/model";
import { ontologyPaths } from "../ontology/paths";
import { ontologyQueries } from "../ontology/queries";

// One tile of a dashboard: its query's output drawn as the Analyst draws an
// answer's visual (ask-visual.tsx: a figure, a chart with its table a
// toggle away, or the table), titled by the tile and shown as it asks, an
// entity it names a link to its page in the load the tile read. A tile
// that has nothing to draw says why in its place.

/** What a tile has to show, as the page read it. */
export type TileState =
  | { kind: "waiting" }
  // Its ontology has no current load.
  | { kind: "unloaded" }
  // Its ontology is deleted, or the viewer can't open it.
  | { kind: "gone" }
  | { kind: "failed"; message: string; retry?: () => void }
  | {
      kind: "drawn";
      output: QueryOutput;
      query: OntologyQuery;
      ontologyId: string;
      loadId: string;
    };

const HEIGHT = 248;

/** Whether an output names entities: only then is the load's version read to link them. */
const namesEntities = (o: QueryOutput) =>
  o.kind === "rows" &&
  o.rows.some((r) =>
    r.cells.some(
      (c) =>
        typeof c === "object" &&
        c !== null &&
        "id" in c &&
        "type" in c &&
        "name" in c,
    ),
  );

/**
 * Where an entity of a load opens: its page, reading that load. Its id says
 * whether the type is keyed, which the load's version tells, so the version
 * is read only for a tile whose output names entities.
 */
function useEntityHref(
  ontologyId: string,
  loadId: string,
  enabled: boolean,
): (r: EntityRef) => string | undefined {
  const { id: ws } = useWorkspace();
  const loads = useQuery({
    ...ontologyQueries.loads(ws, ontologyId),
    enabled,
  });
  const versionId =
    loads.data?.loads?.find((l) => l.id === loadId)?.versionId || "";
  const version = useQuery({
    ...ontologyQueries.version(ws, ontologyId, versionId),
    enabled: enabled && !!versionId,
  });
  const naming = useMemo(() => namingOf(version.data?.version), [version.data]);
  return (r) => {
    const t = naming.types.get(r.type);
    if (!t) return undefined;
    const id = entityId(
      t.key.length ? { type: r.type, key: r.id } : { type: r.type, name: r.id },
    );
    return withLoad(ontologyPaths.entity(ontologyId, id), loadId);
  };
}

function Drawn({
  tile,
  state,
  actions,
}: {
  tile: DashboardTile;
  state: Extract<TileState, { kind: "drawn" }>;
  actions?: ReactNode;
}) {
  const href = useEntityHref(
    state.ontologyId,
    state.loadId,
    namesEntities(state.output),
  );
  return (
    <AskVisual
      output={state.output}
      query={state.query}
      hint={{ kind: tile.visual.kind, title: tile.title }}
      href={href}
      actions={actions}
    />
  );
}

/** Why a tile has nothing to draw, in words. */
const NOTES: Record<
  Exclude<TileState["kind"], "drawn" | "waiting" | "failed">,
  string
> = {
  unloaded:
    "Its ontology has nothing loaded yet. It shows here once a load of it is current.",
  gone: "It reads an ontology that was deleted or isn’t shared with you.",
};

/** A tile as its state has it, with `actions` in its head. */
export function Tile({
  tile,
  state,
  actions,
}: {
  tile: DashboardTile;
  state: TileState;
  actions?: ReactNode;
}) {
  switch (state.kind) {
    case "drawn":
      return <Drawn tile={tile} state={state} actions={actions} />;
    case "waiting":
      return (
        <div role="status" aria-label={`Reading ${tile.title}`}>
          <Skeleton className="ak-visual-wait" width="100%" height={HEIGHT} />
        </div>
      );
    default:
      return (
        <figure className="ak-visual db-tile-note" aria-label={tile.title}>
          <div className="ak-visual-head">
            <div className="ak-visual-title">{tile.title}</div>
            {actions && <div className="ak-visual-actions">{actions}</div>}
          </div>
          <p
            className="db-tile-why"
            role={state.kind === "failed" ? "alert" : undefined}
          >
            <CircleAlert size={14} aria-hidden />
            <span>
              {state.kind === "failed"
                ? `Couldn’t read it: ${state.message}`
                : NOTES[state.kind]}
            </span>
          </p>
          {state.kind === "failed" && state.retry && (
            <div>
              <Button size="sm" onClick={state.retry}>
                Retry
              </Button>
            </div>
          )}
        </figure>
      );
  }
}
