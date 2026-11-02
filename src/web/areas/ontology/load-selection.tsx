import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  DatabaseZap,
  History,
  ListChecks,
  LoaderCircle,
  Lock,
} from "lucide-react";
import type { FoundryLoad } from "../../../shared/wfe";
import { Button, ButtonLink, EmptyState, FilterSelect } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { useReadAccess } from "../explore/graph-reader";
import type { Scope } from "../explore/load-scope";
import {
  chooseLoad,
  isPickable,
  loadOptionLabel,
  loadVersion,
  withLoad,
  type LoadChoice,
} from "./model";
import { Callout, canManage } from "./parts";
import { ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";

// The load a page of an ontology reads (Explorer, Records, an entity's and a
// document's view): `?load=<id>` in the URL, absent for the current load (or
// the newest while none is current), and `&compare=<id>`
// for the Explorer's second side. Everything those pages read is scoped to
// it (../explore/load-scope.ts), and the links between them carry it. An
// ontology with no load at all reads nothing: the pages show NothingLoaded.
// A user reading through source grants reads the current load only, which
// their grants cover: `?load=` is ignored, and there is no picker.

export type LoadSelection = {
  /** Every load not deleted, newest first; empty until read. */
  loads: FoundryLoad[];
  /**
   * The page waits for the loads list to know what it reads. Until the list
   * is read, an ontology with no current load and no load job has none, and
   * `choice` says so at once.
   */
  pending: boolean;
  /** What kept the loads list from being read once retries gave up. */
  failed: Error | null;
  /** Reads the loads list again. */
  retry: () => void;
  choice: LoadChoice;
  /** What reads cover; null while it can't be known (no load, missing, the list unread). */
  scope: Scope | null;
  /** The version that names what the page shows: the load's, else the current one. */
  versionId: string;
  /** The Explorer's second side, when `?compare=` names a load. */
  compare: FoundryLoad | null;
  /** Reads another load; the default drops the param. */
  select: (id: string) => void;
  setCompare: (id: string | null) => void;
  /** A path with the page's `?load=` carried over. */
  link: (path: string) => string;
  /** Read through source grants: the current load only, with no picker. */
  shared: boolean;
};

export function useLoadSelection(): LoadSelection {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    [params, setParams] = useSearchParams();
  const query = useQuery(ontologyQueries.loads(ws, o));
  const loads = useMemo(() => query.data?.loads ?? [], [query.data]);
  const pending =
    query.isPending && !!(ontology.currentLoad || ontology.lastLoad);
  const shared = useReadAccess() === "grants";
  const param = shared ? null : params.get("load");
  // The list says which load is current, in step with the loads it lists.
  const currentId =
    loads.find((l) => l.current)?.id ?? ontology.currentLoad?.id;
  const choice = useMemo(
    () => chooseLoad(loads, currentId, param),
    [loads, currentId, param],
  );
  const failed = !query.data ? query.error : null;
  const retry = () => void query.refetch();
  // Nothing is read before the loads are known.
  const scope = useMemo<Scope | null>(
    () =>
      query.data && choice.kind === "load" ? { id: choice.load.id } : null,
    [choice, query.data],
  );
  const current = loads.find((l) => l.current);
  const versionId =
    choice.kind === "load"
      ? choice.load.versionId || ""
      : current?.versionId || ontology.currentVersionId || "";
  const compareId = params.get("compare");
  const compare = (compareId && loads.find((l) => l.id === compareId)) || null;
  const select = useCallback(
    (id: string) =>
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          if (id && id !== currentId) next.set("load", id);
          else next.delete("load");
          // A page's own view (a node, a record) belongs to the load it was in.
          next.delete("node");
          next.delete("record");
          if (next.get("compare") === id) next.delete("compare");
          return next;
        },
        { replace: false },
      ),
    [setParams, currentId],
  );
  const setCompare = useCallback(
    (id: string | null) =>
      setParams((p) => {
        const next = new URLSearchParams(p);
        if (id) next.set("compare", id);
        else next.delete("compare");
        return next;
      }),
    [setParams],
  );
  const link = useCallback((path: string) => withLoad(path, param), [param]);
  return {
    loads,
    pending,
    failed,
    retry,
    choice,
    scope,
    versionId,
    compare,
    select,
    setCompare,
    link,
    shared,
  };
}

/**
 * The load picker of a page's header: the current load (the default), every
 * other load that finished writing, and ones being written or deleted
 * (shown, not chosen).
 */
export function LoadPicker({
  selection,
  label = "Load",
}: {
  selection: LoadSelection;
  label?: string;
}) {
  const { loads, choice } = selection;
  if (!loads.length || selection.shared) return null;
  return (
    <FilterSelect
      label={label}
      className="ex-load-picker"
      value={choice.kind === "load" ? choice.load.id : ""}
      onChange={selection.select}
      options={[
        ...(choice.kind === "missing"
          ? [{ value: "", label: "Deleted load", disabled: true }]
          : []),
        ...loads.map((l) => ({
          value: l.id,
          label: loadOptionLabel(l),
          disabled:
            !isPickable(l) &&
            !(choice.kind === "load" && choice.load.id === l.id),
        })),
      ]}
    />
  );
}

/** "Compare with…": the Explorer's second side, any other load that finished writing. */
export function ComparePicker({ selection }: { selection: LoadSelection }) {
  const { loads, choice, compare } = selection;
  const shown = choice.kind === "load" ? choice.load.id : "";
  const others = loads.filter((l) => l.id !== shown && isPickable(l));
  if (choice.kind !== "load" || !others.length || selection.shared) return null;
  return (
    <FilterSelect
      label="Compare with"
      className="ex-load-picker"
      value={compare?.id || ""}
      onChange={(id) => selection.setCompare(id || null)}
      options={[
        { value: "", label: "Nothing" },
        ...others.map((l) => ({ value: l.id, label: loadOptionLabel(l) })),
      ]}
    />
  );
}

/**
 * What a page says about its load: the loads that couldn't be read, with a
 * retry; a load still being written (the
 * default while none is current); or one that is gone, with the way back.
 */
export function LoadNotice({ selection }: { selection: LoadSelection }) {
  const { choice, loads, failed } = selection;
  if (failed)
    return (
      <EmptyState
        icon={DatabaseZap}
        title="Couldn’t load the loads"
        text={failed.message}
        action={<Button onClick={selection.retry}>Retry</Button>}
      />
    );
  if (selection.pending) return null;
  const current = loads.find((l) => l.current);
  if (choice.kind === "missing")
    return (
      <Callout
        icon={History}
        tone="muted"
        title="This load was deleted"
        text="What it wrote is gone from the database."
        action={
          (current || loads.length > 0) && (
            <Button size="sm" onClick={() => selection.select("")}>
              {current
                ? `Show the current load (${loadVersion(current)})`
                : "Show the newest load"}
            </Button>
          )
        }
      />
    );
  if (choice.kind !== "load") return null;
  const l = choice.load;
  if (l.status !== "writing" && l.status !== "deleting") return null;
  const live = l.jobs?.find(
    (j) => j.status === "queued" || j.status === "running",
  );
  const p = live?.progress;
  return (
    <Callout
      icon={LoaderCircle}
      live
      title={
        l.status === "writing"
          ? `${loadVersion(l)} is still being written`
          : `${loadVersion(l)} is being deleted`
      }
      text={
        l.status === "writing"
          ? p?.documentsTotal
            ? `${p.documentsDone || 0} of ${p.documentsTotal} documents read so far; what it wrote shows as it goes.`
            : "What it wrote shows as it goes."
          : "What it wrote leaves the database as the delete runs."
      }
    />
  );
}

/**
 * What the Explorer, Records, an entity's page and Ask show in place of
 * reading the database while the ontology has no load at all (`choice` is
 * "none"): for owners and admins, Load data (the Loads page, its Load
 * dialog open) once a version is current, else Finish setting up (the
 * overview); members are told who does it. `purpose` ends the sentence:
 * "explore its entities and facts".
 */
/**
 * In place of a page of an ontology the user reads only through source
 * grants (../explore/graph-reader.ts) when those don't cover its current
 * load.
 */
export function NotShared() {
  return (
    <EmptyState
      icon={Lock}
      title="Not shared with you"
      text="You read this ontology through the sources an owner or admin shares with you, and they don’t cover all of its current load. Ask an owner or admin of this workspace to share them."
    />
  );
}

export function NothingLoaded({ purpose }: { purpose: string }) {
  const { role } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const manage = canManage(role),
    database = ontology.databaseName || "its database";
  if (!ontology.currentVersionId)
    return (
      <EmptyState
        icon={ListChecks}
        title="Nothing loaded yet"
        text={
          manage
            ? `${ontologyName(ontology)} has no current schema version yet. Finish setting it up, then load it into ${database} to ${purpose} here.`
            : `${ontologyName(ontology)} isn’t set up yet. An owner or admin of this workspace finishes setting it up and loads it into ${database}.`
        }
        action={
          manage && (
            <ButtonLink
              variant="primary"
              icon={ListChecks}
              to={paths.ontology(o)}
            >
              Finish setting up
            </ButtonLink>
          )
        }
      />
    );
  return (
    <EmptyState
      icon={DatabaseZap}
      title="Nothing loaded yet"
      text={
        manage
          ? `Load the current schema version into ${database} to ${purpose} here.`
          : `An owner or admin of this workspace loads the current schema version into ${database}; then you can ${purpose} here.`
      }
      action={
        manage && (
          <ButtonLink
            variant="primary"
            icon={DatabaseZap}
            to={paths.newLoad(o)}
          >
            Load data
          </ButtonLink>
        )
      }
    />
  );
}
