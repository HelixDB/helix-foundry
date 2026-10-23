import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import {
  ArrowDown,
  ArrowUp,
  Braces,
  Calendar,
  Clock,
  Hash,
  Timer,
  ToggleLeft,
  Type,
} from "lucide-react";
import {
  columnKind,
  compareValues,
  cx,
  formatValue,
  inferKind,
  type ColumnKind,
} from "./util";

export type GridColumn<Row = Record<string, unknown>> = {
  key: string;
  label?: ReactNode;
  // A DuckDB type ("BIGINT", "VARCHAR", "TIMESTAMP"...) or a ColumnKind.
  type?: string;
  width?: number;
  align?: "left" | "right" | "center";
  sortable?: boolean;
  render?: (row: Row, value: unknown) => ReactNode;
  // Added to the column's header and cells.
  className?: string;
  // Value used for sorting when it differs from row[key].
  sortValue?: (row: Row) => unknown;
};
export type GridSort = { key: string; dir: "asc" | "desc" } | null;

const kinds: ColumnKind[] = [
  "string",
  "integer",
  "number",
  "boolean",
  "date",
  "timestamp",
  "interval",
  "json",
];
const kindOf = (type: string | undefined, values: unknown[]): ColumnKind =>
  type
    ? kinds.includes(type as ColumnKind)
      ? (type as ColumnKind)
      : columnKind(type)
    : inferKind(values);
const widths: Record<ColumnKind, number> = {
  string: 180,
  integer: 110,
  number: 120,
  boolean: 90,
  date: 130,
  timestamp: 180,
  interval: 160,
  json: 240,
};
const icons = {
  string: Type,
  integer: Hash,
  number: Hash,
  boolean: ToggleLeft,
  date: Calendar,
  timestamp: Clock,
  interval: Timer,
  json: Braces,
};

// Builds typed grid columns from a dataset profile (DatasetProfile.columns).
export const columnsFromProfile = (
  columns: { name: string; type: string }[],
): GridColumn[] => columns.map((c) => ({ key: c.name, type: c.type }));

const HEADER = 36,
  OVERSCAN = 8;

// Virtualised, sortable table for up to tens of thousands of rows.
export function DataGrid<Row extends Record<string, any>>({
  rows,
  columns,
  label,
  getRowKey,
  onRowClick,
  selectedKey,
  maxHeight = 520,
  rowHeight = 34,
  sort: controlledSort,
  onSortChange,
  emptyText = "No rows to display.",
  loading = false,
  footer,
  className,
}: {
  rows: Row[];
  // Inferred from the first rows when omitted.
  columns?: GridColumn<Row>[];
  // Accessible name of the grid.
  label: string;
  getRowKey?: (row: Row, index: number) => string | number;
  onRowClick?: (row: Row, index: number) => void;
  selectedKey?: string | number;
  maxHeight?: number;
  rowHeight?: number;
  // Controlled sort: rows are shown as given (e.g. sorted by the server).
  sort?: GridSort;
  onSortChange?: (sort: GridSort) => void;
  emptyText?: ReactNode;
  loading?: boolean;
  footer?: ReactNode;
  className?: string;
}) {
  const sample = rows.slice(0, 50);
  const cols = useMemo(() => {
    const list: GridColumn<Row>[] =
      columns ||
      [...new Set(sample.flatMap((r) => Object.keys(r)))].map((key) => ({
        key,
      }));
    return list.map((c) => {
      const kind = kindOf(
        c.type,
        sample.map((r) => r[c.key]),
      );
      return {
        ...c,
        kind,
        width: c.width ?? widths[kind],
        align:
          c.align ??
          (kind === "integer" || kind === "number" ? "right" : "left"),
      };
    });
    // Inferred columns depend on the row shape, not on every new rows array.
  }, [columns, rows.length ? Object.keys(rows[0]).join("\u0000") : ""]);
  const [localSort, setLocalSort] = useState<GridSort>(null);
  const sort = controlledSort !== undefined ? controlledSort : localSort;
  const sorted = useMemo(() => {
    if (!sort || controlledSort !== undefined) return rows;
    const col = cols.find((c) => c.key === sort.key);
    if (!col) return rows;
    const value = (r: Row) => (col.sortValue ? col.sortValue(r) : r[col.key]);
    const blank = (v: unknown) => v === null || v === undefined;
    const dir = sort.dir === "desc" ? -1 : 1;
    // Nulls stay last in both directions.
    return [...rows].sort((a, b) => {
      const va = value(a),
        vb = value(b);
      return blank(va) || blank(vb)
        ? compareValues(va, vb, col.kind)
        : compareValues(va, vb, col.kind) * dir;
    });
  }, [rows, sort, cols, controlledSort]);
  const toggle = (key: string) => {
    const next: GridSort =
      sort?.key !== key
        ? { key, dir: "asc" }
        : sort.dir === "asc"
          ? { key, dir: "desc" }
          : null;
    if (onSortChange) onSortChange(next);
    if (controlledSort === undefined) setLocalSort(next);
  };

  const scroller = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0),
    [viewport, setViewport] = useState(maxHeight);
  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const observer = new ResizeObserver(() =>
      setViewport(el.clientHeight || maxHeight),
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [maxHeight]);
  const frame = useRef(0);
  const onScroll = () => {
    cancelAnimationFrame(frame.current);
    frame.current = requestAnimationFrame(() =>
      setScrollTop(scroller.current?.scrollTop || 0),
    );
  };
  const start = Math.max(
      0,
      Math.floor(Math.max(0, scrollTop - HEADER) / rowHeight) - OVERSCAN,
    ),
    end = Math.min(
      sorted.length,
      start + Math.ceil(viewport / rowHeight) + OVERSCAN * 2,
    );
  const template = cols
    .map((c) => `minmax(${c.width}px, ${c.kind === "json" ? 2 : 1}fr)`)
    .join(" ");
  const minWidth = cols.reduce((n, c) => n + (c.width || 0), 0);
  const keyOf = (r: Row, i: number) => (getRowKey ? getRowKey(r, i) : i);
  const activate = (e: KeyboardEvent, r: Row, i: number) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onRowClick?.(r, i);
    }
  };

  return (
    <div className={cx("hf-grid-wrap", className)}>
      <div
        ref={scroller}
        className="hf-grid"
        role="grid"
        aria-label={label}
        aria-rowcount={sorted.length + 1}
        aria-colcount={cols.length}
        aria-busy={loading || undefined}
        style={
          {
            maxHeight,
            "--hf-grid-template": template,
            "--hf-grid-row": rowHeight + "px",
          } as CSSProperties
        }
        onScroll={onScroll}
      >
        <div className="hf-grid-inner" style={{ minWidth }}>
          <div
            className="hf-grid-row hf-grid-head"
            role="row"
            aria-rowindex={1}
          >
            {cols.map((c, ci) => {
              const Icon = icons[c.kind];
              const active = sort?.key === c.key;
              const sortable = c.sortable !== false;
              return (
                <div
                  key={c.key}
                  role="columnheader"
                  aria-colindex={ci + 1}
                  aria-sort={
                    active
                      ? sort!.dir === "asc"
                        ? "ascending"
                        : "descending"
                      : undefined
                  }
                  className={cx("hf-grid-th", `is-${c.align}`, c.className)}
                >
                  <button
                    type="button"
                    disabled={!sortable}
                    onClick={() => toggle(c.key)}
                    title={c.type || c.kind}
                  >
                    <Icon size={12} className="hf-grid-type" aria-hidden />
                    <span>{c.label ?? c.key}</span>
                    {active &&
                      (sort!.dir === "asc" ? (
                        <ArrowUp size={12} aria-hidden />
                      ) : (
                        <ArrowDown size={12} aria-hidden />
                      ))}
                  </button>
                </div>
              );
            })}
          </div>
          {loading ? (
            Array.from({ length: 6 }, (_, i) => (
              <div key={i} className="hf-grid-row" role="row">
                {cols.map((c) => (
                  <div key={c.key} className="hf-grid-td" role="gridcell">
                    <span className="hf-skeleton" style={{ width: "70%" }} />
                  </div>
                ))}
              </div>
            ))
          ) : !sorted.length ? (
            <div className="hf-grid-empty" role="row">
              <div role="gridcell">{emptyText}</div>
            </div>
          ) : (
            <>
              <div style={{ height: start * rowHeight }} aria-hidden />
              {sorted.slice(start, end).map((r, offset) => {
                const i = start + offset,
                  key = keyOf(r, i);
                return (
                  <div
                    key={key}
                    role="row"
                    aria-rowindex={i + 2}
                    aria-selected={
                      selectedKey !== undefined
                        ? selectedKey === key
                        : undefined
                    }
                    tabIndex={onRowClick ? 0 : undefined}
                    className={cx(
                      "hf-grid-row",
                      onRowClick && "is-clickable",
                      selectedKey === key && "is-selected",
                    )}
                    onClick={onRowClick ? () => onRowClick(r, i) : undefined}
                    onKeyDown={
                      onRowClick ? (e) => activate(e, r, i) : undefined
                    }
                  >
                    {cols.map((c, ci) => {
                      const value = r[c.key];
                      const text = c.render ? null : formatValue(value, c.kind);
                      return (
                        <div
                          key={c.key}
                          role="gridcell"
                          aria-colindex={ci + 1}
                          className={cx(
                            "hf-grid-td",
                            `is-${c.align}`,
                            `is-${c.kind}`,
                            c.className,
                          )}
                          title={text ?? undefined}
                        >
                          {c.render ? (
                            c.render(r, value)
                          ) : text === null ? (
                            <span className="hf-null">null</span>
                          ) : (
                            text
                          )}
                        </div>
                      );
                    })}
                  </div>
                );
              })}
              <div
                style={{ height: (sorted.length - end) * rowHeight }}
                aria-hidden
              />
            </>
          )}
        </div>
      </div>
      {footer && <div className="hf-grid-foot">{footer}</div>}
    </div>
  );
}
