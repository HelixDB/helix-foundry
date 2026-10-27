import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import type { FoundryLoad, OntologyVersion } from "../../../shared/wfe";
import { Button, Panel, SkeletonText, StatusBadge } from "../../kit";
import { useWorkspace } from "../../ui";
import { loadVersion } from "../ontology/model";
import { useOntology } from "../ontology/scope";
import {
  COMPARE_LIMITS,
  countRows,
  diffKeys,
  keysRequest,
  parseKeys,
  type CountRow,
  type KeyItem,
} from "./compare";
import { loadCountsQuery } from "./load-counts";
import { readOptions, runRead, startingOf } from "./reads";
import { DatabaseStarting } from "./starting";
import { useTenantReads } from "./tenant-reads";

// The diff under the two canvases of a compare: each type's and link's
// count in both loads (bitmap counts), the ones only one version names
// marked; opening a type compares its entities by key, read only then. A
// new database still starting says so in place of either, and reads again.

const fmt = (n: number | null) => (n === null ? "…" : n.toLocaleString());

function Delta({ row }: { row: CountRow }) {
  if (row.a === null || row.b === null) return <>…</>;
  const d = row.b - row.a;
  return (
    <span className={d > 0 ? "is-up" : d < 0 ? "is-down" : "ex-dim"}>
      {d > 0 ? `+${d.toLocaleString()}` : d.toLocaleString()}
    </span>
  );
}

function KeyList({
  title,
  items,
  total,
  onFocus,
}: {
  title: string;
  items: KeyItem[];
  total: number;
  onFocus: (item: KeyItem) => void;
}) {
  return (
    <div>
      <strong>
        {title} · {total.toLocaleString()}
      </strong>
      {items.length ? (
        <ul>
          {items.map((item) => (
            <li key={`${item.property}:${item.value}`}>
              <Button size="sm" variant="ghost" onClick={() => onFocus(item)}>
                {item.name}
              </Button>
            </li>
          ))}
          {total > items.length && (
            <li className="ex-dim">
              and {(total - items.length).toLocaleString()} more
            </li>
          )}
        </ul>
      ) : (
        <p className="ex-dim">None</p>
      )}
    </div>
  );
}

/** A type's entities in both loads, by key: read when the type is opened. */
function TypeDiff({
  a,
  b,
  type,
  onFocus,
}: {
  a: FoundryLoad;
  b: FoundryLoad;
  type: string;
  onFocus: (item: KeyItem) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    { readLater } = useTenantReads();
  const side = (load: FoundryLoad) => ({
    queryKey: [ws, "ontology", o, "read", load.id, "keys", type],
    queryFn: async ({ signal }: { signal?: AbortSignal }) =>
      parseKeys(await runRead(readLater, keysRequest(load.id, type), signal)),
    ...readOptions,
    staleTime: 30_000,
  });
  const leftRead = side(a),
    rightRead = side(b);
  const left = useQuery(leftRead),
    right = useQuery(rightRead);
  const starting = startingOf(left.error ?? left.failureReason)
    ? leftRead
    : startingOf(right.error ?? right.failureReason)
      ? rightRead
      : null;
  if (starting) return <DatabaseStarting inline queryKey={starting.queryKey} />;
  if (left.error || right.error)
    return (
      <p role="alert" className="on-error-text">
        Couldn’t read its keys: {(left.error || right.error)!.message}
      </p>
    );
  if (!left.data || !right.data) return <SkeletonText lines={3} />;
  const diff = diffKeys(left.data, right.data);
  if (!diff)
    return (
      <p className="ex-dim">
        More than {COMPARE_LIMITS.keysPerSide.toLocaleString()} {type} entities
        in a load: only their counts compare.
      </p>
    );
  return (
    <div className="ex-diff-keys" data-testid={`diff-keys-${type}`}>
      <KeyList
        title={`Added in ${loadVersion(b)}`}
        items={diff.added}
        total={diff.counts.added}
        onFocus={onFocus}
      />
      <KeyList
        title={`Removed since ${loadVersion(a)}`}
        items={diff.removed}
        total={diff.counts.removed}
        onFocus={onFocus}
      />
      <KeyList
        title="Renamed"
        items={diff.changed}
        total={diff.counts.changed}
        onFocus={onFocus}
      />
    </div>
  );
}

export function DiffPanel({
  a,
  b,
  versions,
  onFocus,
}: {
  a: FoundryLoad;
  b: FoundryLoad;
  versions: { a?: OntologyVersion | null; b?: OntologyVersion | null };
  /** Selects the entity on both canvases. */
  onFocus: (type: string, item: KeyItem) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    { readLater } = useTenantReads();
  const leftRead = loadCountsQuery(ws, o, readLater, a, versions.a),
    rightRead = loadCountsQuery(ws, o, readLater, b, versions.b);
  const left = useQuery(leftRead),
    right = useQuery(rightRead);
  const starting = startingOf(left.error ?? left.failureReason)
    ? leftRead
    : startingOf(right.error ?? right.failureReason)
      ? rightRead
      : null;
  const [open, setOpen] = useState<string | null>(null);
  const rows =
    left.data && right.data ? countRows(left.data, right.data) : null;
  const error = left.error || right.error;
  return (
    <Panel
      title={`What changed from ${loadVersion(a)} to ${loadVersion(b)}`}
      subtitle="Counts per type and link, from the database. Open a type to compare its entities by key; a renamed key or type shows as all removed and all added."
    >
      <div className="ex-diff">
        {starting ? (
          <DatabaseStarting inline queryKey={starting.queryKey} />
        ) : error ? (
          <p role="alert" className="on-error-text">
            Couldn’t count the loads: {error.message}
          </p>
        ) : !rows ? (
          <SkeletonText lines={4} />
        ) : (
          <table aria-label="Counts per type and link">
            <thead>
              <tr>
                <th>Type or link</th>
                <th className="is-num">{loadVersion(a)}</th>
                <th className="is-num">{loadVersion(b)}</th>
                <th className="is-num">Change</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const key = `${row.target}:${row.label}`;
                const expandable =
                  row.target === "node" && row.change === "both";
                return (
                  <FragmentRow
                    key={key}
                    row={row}
                    open={open === row.label && expandable}
                    onToggle={
                      expandable
                        ? () => setOpen(open === row.label ? null : row.label)
                        : undefined
                    }
                  >
                    <TypeDiff
                      a={a}
                      b={b}
                      type={row.label}
                      onFocus={(item) => onFocus(row.label, item)}
                    />
                  </FragmentRow>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </Panel>
  );
}

function FragmentRow({
  row,
  open,
  onToggle,
  children,
}: {
  row: CountRow;
  open: boolean;
  onToggle?: () => void;
  children: ReactNode;
}) {
  return (
    <>
      <tr data-testid={`diff-row-${row.label}`}>
        <td>
          {onToggle ? (
            <button
              type="button"
              className="ex-text-btn"
              aria-expanded={open}
              onClick={onToggle}
            >
              {open ? (
                <ChevronDown aria-hidden size={14} />
              ) : (
                <ChevronRight aria-hidden size={14} />
              )}{" "}
              {row.label}
            </button>
          ) : (
            <span>
              {row.label}
              {row.target === "edge" && <span className="ex-dim"> (link)</span>}
            </span>
          )}{" "}
          {row.change !== "both" && (
            <StatusBadge
              size="sm"
              status={row.change === "added" ? "succeeded" : "failed"}
              label={row.change === "added" ? "Added" : "Removed"}
            />
          )}
        </td>
        <td className="is-num">{row.change === "added" ? "—" : fmt(row.a)}</td>
        <td className="is-num">
          {row.change === "removed" ? "—" : fmt(row.b)}
        </td>
        <td className="is-num">
          {row.change === "both" ? <Delta row={row} /> : "—"}
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={4}>{children}</td>
        </tr>
      )}
    </>
  );
}
