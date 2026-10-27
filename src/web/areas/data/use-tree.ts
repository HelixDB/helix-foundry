import { useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { DataLayoutResponse } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { connectorLabel } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import {
  connectorKindsQuery,
  connectorsQuery,
  dataLayoutQuery,
  putDataLayout,
  useSetsAfterSyncs,
} from "./data-api";
import {
  EMPTY_LAYOUT,
  applyChange,
  parseLayout,
  prune,
  toStored,
  type Layout,
  type LayoutChange,
  type TreeData,
} from "./layout";
import { KINDS, isEventSource } from "./model";

// The Data page's lists and its layout, and the changes owners and admins
// make to the layout. A change shows at once (the stored layout with every
// change still saving applied on top) and saves in turn, one PUT at a time
// at the revision the last one left. A PUT refused because someone else
// wrote first reads the layout and the lists again and makes the same
// change on top, once; if that fails too, or for any other refusal, the
// change is undone and a toast says so. Nothing is written unless the
// connector, set, document and layout lists are all read in full: a write
// drops ids it doesn't find. A sync that ends may have changed its
// connection's own sets, so the sets and their documents are read again.

/** The gRPC code the WFE answers a stale revision with. */
const FAILED_PRECONDITION = 9;
const stale = (e: unknown) =>
  e instanceof ApiError && e.code === FAILED_PRECONDITION;

type Pending = { change: LayoutChange };

export function useDataTree(ws: string) {
  const qc = useQueryClient(),
    { notify } = useWorkspace();
  const connectors = useQuery(connectorsQuery(ws)),
    kinds = useQuery(connectorKindsQuery(ws)),
    documents = useQuery(documentQueries.workspace(ws)),
    sets = useQuery(documentSetQueries.list(ws)),
    stored = useQuery(dataLayoutQuery(ws));
  // Until the kinds load, the order they come in.
  const treeData = (
    c = connectors.data,
    s = sets.data,
    d = documents.data,
  ): TreeData => ({
    kinds: kinds.data?.kinds?.map((k) => k.kind) || KINDS,
    connectors: c?.connectors || [],
    sets: s || [],
    documents: d || [],
    // A kind's folder; API sources' is "API sources".
    label: (kind) =>
      isEventSource(kind) ? "API sources" : connectorLabel(kind),
  });
  const data = treeData();
  useSetsAfterSyncs(ws, connectors.data?.connectors);
  // A failed refetch keeps the data and isn't a success.
  const ready =
    connectors.isSuccess &&
    sets.isSuccess &&
    documents.isSuccess &&
    stored.isSuccess;
  const parsed = stored.data
    ? parseLayout(stored.data.layout ?? {})
    : undefined;
  const known = parsed?.version !== "unknown";
  const [pending, setPending] = useState<readonly Pending[]>([]);
  const base = parsed?.version === 1 ? parsed.layout : EMPTY_LAYOUT;
  const layout: Layout = ready
    ? pending.reduce((l, p) => {
        const next = applyChange(l, data, p.change);
        return "layout" in next ? next.layout : l;
      }, base)
    : base;
  // What a save that runs later reads: the lists and whether all are read.
  const queue = useRef(Promise.resolve()),
    latest = useRef({ data, ready });
  latest.current = { data, ready };

  const key = dataLayoutQuery(ws).queryKey;
  // `change` made on the layout `read` arranges with `lists`.
  const write = (
    change: LayoutChange,
    read: DataLayoutResponse,
    lists: TreeData,
  ) => {
    const parsed = parseLayout(read.layout ?? {});
    if (parsed.version !== 1)
      throw new Error("a newer version of Foundry arranged it.");
    const next = applyChange(parsed.layout, lists, change);
    if ("problem" in next) throw new Error(next.problem);
    return putDataLayout(ws, {
      layout: toStored(next.layout),
      expectedRevision: read.revision || "0",
    });
  };
  const save = async (entry: Pending) => {
    try {
      let saved: DataLayoutResponse;
      try {
        if (!latest.current.ready)
          throw new Error("a list on the page didn’t load. Try again.");
        saved = await write(
          entry.change,
          qc.getQueryData<DataLayoutResponse>(key) ?? {},
          latest.current.data,
        );
      } catch (e) {
        if (!stale(e)) throw e;
        const fresh = { staleTime: 0 },
          [read, c, s, d] = await Promise.all([
            qc.fetchQuery({ ...dataLayoutQuery(ws), ...fresh }),
            qc.fetchQuery({ ...connectorsQuery(ws), ...fresh }),
            qc.fetchQuery({ ...documentSetQueries.list(ws), ...fresh }),
            qc.fetchQuery({ ...documentQueries.workspace(ws), ...fresh }),
          ]);
        saved = await write(entry.change, read, treeData(c, s, d));
      }
      qc.setQueryData(key, saved);
    } catch (e) {
      notify(
        stale(e)
          ? "Couldn’t save the Connectors tree: someone else changed it at the same time. Try again."
          : `Couldn’t save the Connectors tree: ${(e as Error).message}`,
        true,
      );
      void qc.invalidateQueries({ queryKey: key });
    } finally {
      setPending((list) => list.filter((p) => p !== entry));
    }
  };

  /**
   * Makes `change`, or says in a toast why it can't be made. Does nothing
   * until every list is read, for a layout of a version this site doesn't
   * know, or when nothing would change.
   */
  const change = (change: LayoutChange) => {
    if (!ready || parsed?.version !== 1) return;
    const next = applyChange(layout, data, change);
    if ("problem" in next) return notify(next.problem, true);
    const same = (l: Layout) => JSON.stringify(toStored(l));
    if (same(next.layout) === same(prune(layout, data))) return;
    const entry = { change };
    setPending((list) => [...list, entry]);
    queue.current = queue.current.then(() => save(entry));
  };

  return {
    connectors,
    kinds,
    documents,
    sets,
    stored,
    data,
    layout,
    // Every list is read and the layout is one this site arranges.
    editable: ready && known,
    // The stored layout is of a version this site doesn't know.
    unknown: !known,
    change,
  };
}
