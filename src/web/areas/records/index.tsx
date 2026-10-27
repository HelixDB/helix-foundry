// Records area: an ontology's database as a table, by type, beside the
// Explorer's graph. Lazy-loaded by the Ontology area's Explorer route table
// at /explore/:ontologyId/records; the URL holds the whole view (./model.ts
// readView), the open record included.
import "../explore/explore.css";
import "./records.css";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  CircleAlert,
  Columns3,
  Database,
  Network,
  SearchX,
  Shapes,
  Table2,
} from "lucide-react";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  FilterBar,
  FilterSelect,
  Skeleton,
  cx,
  formatNumber,
  type GridColumn,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { describeFailure } from "../explore/explorer/query/failure";
import { valueText } from "../explore/model";
import { scopeKey, type Scope } from "../explore/load-scope";
import type { TypeNaming } from "../explore/naming";
import { ReadFailed, pausedOf, startingOf } from "../explore/reads";
import { DatabasePaused, DatabaseStarting } from "../explore/starting";
import { useTenantReads } from "../explore/tenant-reads";
import { ExplorerTop } from "../explore/top-bar";
import { useNaming } from "../explore/use-naming";
import { typeHue } from "../ontology/colors";
import {
  LoadNotice,
  NothingLoaded,
  useLoadSelection,
} from "../ontology/load-selection";
import { useOntology } from "../ontology/scope";
import { PanelContent, usePanelSlot } from "../../shell/context";
import { RecordDetail } from "./detail";
import {
  RECORDS_LIMITS,
  RECORD_COLUMN,
  activeFilter,
  columnsOf,
  fieldName,
  filterOf,
  pageSpan,
  readView,
  recordTypes,
  shownColumns,
  viewParams,
  type Column,
  type RecordRow,
  type RecordsView,
} from "./model";
import { recordQueries } from "./queries";

// The grid's kind for a version's prop type: its icon, width and alignment.
const KIND: Record<string, string> = {
  integer: "integer",
  float: "number",
  boolean: "boolean",
  date: "date",
  datetime: "timestamp",
};
// How long typing waits before the URL (and the read) follow.
const TYPING_MS = 300;

type GridRow = { $key: string; $row: RecordRow } & Record<string, unknown>;
// Stands in for the type while the version names none (its read is off).
const NO_TYPE: TypeNaming = { name: "", props: [], key: [], names: [] };
// Stands in for the load while it isn't known (the reads are off).
const NO_SCOPE: Scope = { kind: "load", id: "" };

/** What a failed read says: the explorer's words for the transport's failure. */
function failureText(error: unknown) {
  if (error instanceof ReadFailed) {
    const d = describeFailure(error.failure, "read");
    return { title: d.title, text: error.failure.message };
  }
  return {
    title: "Couldn’t read the records",
    text: error instanceof Error ? error.message : undefined,
  };
}

export function RecordsPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    [params, setParams] = useSearchParams();
  const view = useMemo(() => readView(params), [params]);
  // The load Records reads, and the version that names its types.
  const selection = useLoadSelection();
  const scope = selection.scope ?? NO_SCOPE;
  const {
    naming,
    ready,
    error: versionError,
    refetch,
  } = useNaming(selection.versionId);
  const { read } = useTenantReads();
  const tenant = ontology.tenantId || "";
  const types = useMemo(() => recordTypes(naming), [naming]);
  const countsRead = recordQueries.counts(ws, o, read, scope, naming);
  const counts = useQuery({
    ...countsRead,
    enabled: ready && !!tenant && !!selection.scope && types.length > 0,
  });
  // The URL's type, else the first that has records, else the first.
  const type =
    types.find((t) => t.name === view.type) ||
    types.find((t) => (counts.data?.get(t.name) || 0) > 0) ||
    types[0];
  const pageQuery = recordQueries.page(
    ws,
    o,
    read,
    scope,
    naming,
    type || NO_TYPE,
    view,
  );
  const page = useQuery({
    ...pageQuery,
    enabled:
      pageQuery.enabled &&
      ready &&
      !!tenant &&
      !!selection.scope &&
      !!type &&
      !counts.isPending,
    // Keep the rows on screen while the next page, sort or search loads,
    // within one load and type.
    placeholderData: (previous, query) =>
      query?.queryKey[4] === scopeKey(scope) &&
      query?.queryKey[7] === type?.name
        ? previous
        : undefined,
  });
  // The view's params, with the page's load kept.
  const load = params.get("load");
  const withView = (v: RecordsView) => ({
    ...viewParams(v),
    ...(load ? { load } : {}),
  });

  // The view changes through the URL: a new type starts over, a new search,
  // filter or sort goes back to the first page. Typing replaces the entry.
  const viewRef = useRef(view);
  viewRef.current = view;
  const update = (patch: Partial<RecordsView>, replace = false) =>
    setParams(withView({ ...viewRef.current, ...patch }), { replace });

  // The search and filter boxes follow the URL, and the URL follows typing.
  // What typing last pushed is remembered until the URL catches up with it,
  // so that never takes back keys typed since; a later visit to the same
  // URL (Back) sets the boxes again.
  const boxes = (v: RecordsView) => ({
    search: v.search,
    field: v.filter?.field || "",
    value: v.filter?.value || "",
  });
  const [draft, setDraft] = useState(() => boxes(view));
  // The columns chosen per type, for this visit; unchosen, a type shows its
  // defaults.
  const [chosen, setChosen] = useState<Record<string, string[]>>({});
  const pushed = useRef("");
  const urlBoxes = JSON.stringify(boxes(view));
  useEffect(() => {
    if (urlBoxes === pushed.current) {
      pushed.current = "";
      return;
    }
    setDraft(JSON.parse(urlBoxes));
  }, [urlBoxes]);
  useEffect(() => {
    if (JSON.stringify(draft) === JSON.stringify(boxes(viewRef.current)))
      return;
    const t = window.setTimeout(() => {
      pushed.current = JSON.stringify(draft);
      setParams(
        withView({
          ...viewRef.current,
          search: draft.search,
          filter: draft.field
            ? { field: draft.field, value: draft.value }
            : null,
          page: 0,
        }),
        { replace: true },
      );
    }, TYPING_MS);
    return () => window.clearTimeout(t);
  }, [draft, setParams]);

  const header = (
    <ExplorerTop view="records" selection={tenant ? selection : undefined} />
  );
  // The type list is the section panel's while it shows (beside the sidebar, or
  // in a phone's open drawer), else it sits above the table.
  const slot = usePanelSlot();
  const frame = (content: ReactNode) => (
    <div className="ex-view">
      {header}
      <div className="ex-frame">{content}</div>
    </div>
  );
  if (!tenant)
    return frame(
      <>
        <EmptyState
          icon={Database}
          title="This ontology has no database"
          text="Create an ontology on a database to see its records."
          action={
            <ButtonLink to={paths.ontologies()}>Go to Ontologies</ButtonLink>
          }
        />
      </>,
    );
  if (
    (selection.choice.kind === "missing" && !selection.pending) ||
    selection.failed
  )
    return frame(
      <>
        <LoadNotice selection={selection} />
      </>,
    );
  if (selection.choice.kind === "none" && !selection.pending)
    return frame(<NothingLoaded purpose="see its records" />);
  if (startingOf(counts.error ?? counts.failureReason))
    return frame(
      <>
        <DatabaseStarting queryKey={countsRead.queryKey} />
      </>,
    );
  if (
    !ready ||
    selection.pending ||
    !selection.scope ||
    (types.length > 0 && counts.isPending)
  )
    return frame(
      <>
        <Skeleton height={420} />
      </>,
    );
  // Records lists the version's types: without the version it has none.
  if (versionError)
    return frame(
      <>
        <EmptyState
          icon={CircleAlert}
          title={
            selection.choice.kind === "load"
              ? "Couldn’t read the load’s version"
              : "Couldn’t read the current version"
          }
          text={versionError.message}
          action={<Button onClick={refetch}>Retry</Button>}
        />
      </>,
    );
  if (!type)
    return frame(
      <>
        <EmptyState
          icon={Shapes}
          title={
            ontology.currentVersionId
              ? "The current version names no types"
              : "No version is current yet"
          }
          text="Records lists the types of the version a load wrote. The graph shows everything the database holds."
          action={
            <>
              <ButtonLink to={paths.versions(o)}>Schema versions</ButtonLink>
              <ButtonLink
                variant="ghost"
                icon={Network}
                to={selection.link(paths.explore(o))}
              >
                Open the graph
              </ButtonLink>
            </>
          }
        />
      </>,
    );
  if (counts.error) {
    const paused = pausedOf(counts.error);
    if (paused) return frame(<DatabasePaused failure={paused} />);
    const f = failureText(counts.error);
    return frame(
      <>
        <EmptyState
          icon={CircleAlert}
          title={f.title}
          text={f.text}
          action={<Button onClick={() => void counts.refetch()}>Retry</Button>}
        />
      </>,
    );
  }
  const count = (t: TypeNaming) => counts.data?.get(t.name) ?? null;
  if (types.every((t) => count(t) === 0))
    return frame(
      <>
        <EmptyState
          icon={Table2}
          title="Nothing loaded yet"
          text={
            selection.choice.kind === "other" && selection.loads.length
              ? "Everything under these types was written by a load. Pick a load to see its records."
              : `Load the current version to fill ${ontology.databaseName || "the ontology’s database"} with its records.`
          }
          action={
            <>
              <ButtonLink to={paths.ontology(o)}>Load data</ButtonLink>
              <ButtonLink
                variant="ghost"
                icon={Network}
                to={selection.link(paths.explore(o))}
              >
                Open the graph
              </ButtonLink>
            </>
          }
        />
      </>,
    );

  // The types' colours: every type with records, alphabetically, as the graph's.
  const hues = types.filter((t) => (count(t) || 0) > 0).map((t) => t.name);
  const columns = columnsOf(type),
    shown = shownColumns(type, chosen[type.name]),
    isShown = (c: Column) => shown.some((x) => x.key === c.key);
  // A field picked with nothing typed yet filters nothing.
  const applied = activeFilter(view);
  const filter = applied ? filterOf(type, applied.field, applied.value) : null;
  const data = page.data;
  const searching = !!view.search.trim() || !!applied;
  const total = searching ? (data?.matched?.count ?? null) : count(type);
  const span = pageSpan(
    view.page,
    data?.rows.length || 0,
    total,
    !!data?.matched?.capped,
    !!data?.more,
  );
  // A value the field can't hold reads nothing: it says so in the table's
  // place, or under the box while the last rows stay up.
  const refused = filter && !filter.ok ? filter.error : "";
  const pagePaused = pausedOf(page.error);
  const rows: GridRow[] = (data?.rows || []).map((r) => ({
    ...r.props,
    $key: r.entity?.id || `#${r.dbId}`,
    $row: r,
  }));
  const gridColumns = shown.map((c) => gridColumn(type, c));
  const open = (r: RecordRow) =>
    r.entity
      ? update({ record: r.entity.id })
      : navigate(selection.link(paths.explore(o, r.dbId)));
  const typeList = (
    <ul className="og-legend" aria-label="Types">
      {types.map((t) => (
        <li key={t.name}>
          <button
            type="button"
            aria-pressed={t.name === type.name}
            className={cx("og-type", t.name === type.name && "is-current")}
            onClick={() => {
              slot?.onNavigate?.();
              update({
                type: t.name,
                search: "",
                filter: null,
                sort: null,
                page: 0,
                record: "",
              });
            }}
          >
            <span
              className="og-dot"
              style={{ background: typeHue(hues, t.name) || undefined }}
              aria-hidden
            />
            {t.name}
            <span className="og-count">
              {count(t) === null ? "—" : formatNumber(count(t)!)}
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
  return frame(
    <>
      <LoadNotice selection={selection} />
      <div className="rx">
        {slot ? (
          <PanelContent label="Types">
            <div className="ex is-panel">
              <div className="ex-panel">
                <div className="ex-panel-head">
                  <h2 className="ex-section-title">Types</h2>
                </div>
                <div className="ex-panel-body rx-types">{typeList}</div>
              </div>
            </div>
          </PanelContent>
        ) : (
          typeList
        )}
        <FilterBar
          search={{
            value: draft.search,
            onChange: (search) => setDraft((d) => ({ ...d, search })),
            placeholder: `Search ${type.name} records`,
          }}
          count={span.text || undefined}
          actions={
            <details className="rx-columns">
              <summary className="hf-btn hf-btn--secondary hf-btn--sm">
                <Columns3 size={14} aria-hidden /> Columns
              </summary>
              <div className="rx-columns-menu">
                {columns
                  .filter((c) => c.role !== "record")
                  .map((c) => (
                    <label key={c.key}>
                      <input
                        type="checkbox"
                        checked={isShown(c)}
                        onChange={(e) =>
                          setChosen((was) => ({
                            ...was,
                            [type.name]: columns
                              .filter((x) =>
                                x.key === c.key
                                  ? e.target.checked
                                  : x.role !== "record" && isShown(x),
                              )
                              .map((x) => x.key),
                          }))
                        }
                      />
                      {fieldName(c.key)}
                    </label>
                  ))}
                <button
                  type="button"
                  className="og-link"
                  onClick={() =>
                    setChosen((was) => {
                      const next = { ...was };
                      delete next[type.name];
                      return next;
                    })
                  }
                >
                  Reset columns
                </button>
              </div>
            </details>
          }
        >
          <FilterSelect
            className="rx-where"
            label="Filter by field"
            value={draft.field}
            onChange={(field) => setDraft((d) => ({ ...d, field }))}
            options={[
              { value: "", label: "Filter by field…" },
              ...columns
                .filter((c) => c.key !== RECORD_COLUMN)
                .map((c) => ({ value: c.key, label: fieldName(c.key) })),
            ]}
          />
          {draft.field && (
            <>
              <input
                className="rx-value"
                aria-label={`${fieldName(draft.field)} equals`}
                placeholder="equals…"
                value={draft.value}
                onChange={(e) =>
                  setDraft((d) => ({ ...d, value: e.target.value }))
                }
              />
              <Button
                size="sm"
                variant="ghost"
                onClick={() =>
                  setDraft((d) => ({ ...d, field: "", value: "" }))
                }
              >
                Clear
              </Button>
            </>
          )}
        </FilterBar>
        {refused && rows.length > 0 && (
          <p className="rx-hint" role="status">
            {refused}
          </p>
        )}
        {startingOf(page.error ?? page.failureReason) ? (
          <DatabaseStarting queryKey={pageQuery.queryKey} />
        ) : pagePaused ? (
          <DatabasePaused failure={pagePaused} />
        ) : page.error ? (
          <EmptyState
            size="sm"
            icon={CircleAlert}
            title={failureText(page.error).title}
            text={failureText(page.error).text}
            action={<Button onClick={() => void page.refetch()}>Retry</Button>}
          />
        ) : !rows.length && (refused || (data && !page.isPlaceholderData)) ? (
          // Nothing to show: said in view at any width, not in a cell as
          // wide as every column.
          refused ? (
            <EmptyState size="sm" icon={CircleAlert} title={refused} />
          ) : view.page > 0 ? (
            <EmptyState
              size="sm"
              icon={Table2}
              title="No records on this page."
              text={
                searching
                  ? "The matches end on an earlier page."
                  : "The records end on an earlier page."
              }
              action={
                <Button
                  onClick={() =>
                    update({
                      // Back to the last page with rows, when it's known.
                      page:
                        total === null
                          ? view.page - 1
                          : Math.min(
                              view.page - 1,
                              Math.max(
                                0,
                                Math.ceil(total / RECORDS_LIMITS.page) - 1,
                              ),
                            ),
                    })
                  }
                >
                  Previous
                </Button>
              }
            />
          ) : searching ? (
            <EmptyState size="sm" icon={SearchX} title="No records match." />
          ) : (
            <EmptyState
              size="sm"
              icon={Table2}
              title={`No ${type.name} records in the database.`}
            />
          )
        ) : (
          <DataGrid
            label={`${type.name} records`}
            rows={rows}
            columns={gridColumns}
            getRowKey={(r) => r.$key}
            selectedKey={view.record || undefined}
            onRowClick={(r) => open(r.$row)}
            sort={
              view.sort ? { key: view.sort.column, dir: view.sort.dir } : null
            }
            onSortChange={(s) =>
              update({
                sort: s ? { column: s.key, dir: s.dir } : null,
                page: 0,
              })
            }
            loading={page.isPending || (page.isPlaceholderData && !rows.length)}
            maxHeight={620}
            rowHeight={40}
            footer={
              (span.prev || span.next || span.deep) && (
                <div className="rx-pages">
                  {span.deep && (
                    <span>
                      Search or filter to reach records past the first{" "}
                      {formatNumber(span.last)}.
                    </span>
                  )}
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={!span.prev}
                    onClick={() => update({ page: view.page - 1 })}
                  >
                    Previous
                  </Button>
                  <span>Page {formatNumber(view.page + 1)}</span>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={!span.next}
                    onClick={() => update({ page: view.page + 1 })}
                  >
                    Next
                  </Button>
                </div>
              )
            }
          />
        )}
      </div>
      {view.record && (
        <RecordDetail
          key={view.record}
          entityId={view.record}
          naming={naming}
          scope={scope}
          link={selection.link}
          onClose={() => update({ record: "" })}
          onOpen={(other) =>
            update({
              type: other.type,
              search: "",
              filter: null,
              sort: null,
              page: 0,
              record: other.id,
            })
          }
        />
      )}
    </>,
  );
}

function gridColumn(t: TypeNaming, c: Column): GridColumn<GridRow> {
  if (c.role === "record")
    return {
      key: c.key,
      label: "Record",
      type: "string",
      width: 260,
      render: (r) => {
        const key = t.key
          .map((k) => r.$row.props[k])
          .filter((v) => v !== undefined && v !== null && v !== "")
          .map(String)
          .join(" · ");
        return (
          <span className="rx-record">
            <strong>{r.$row.name}</strong>
            {r.$row.by !== "key" && key && <small>{key}</small>}
          </span>
        );
      },
    };
  return {
    key: c.key,
    label: fieldName(c.key),
    type: KIND[c.type] || "string",
    render: (r) => {
      const v = r.$row.props[c.key];
      const text =
        c.role === "key" && v !== undefined ? String(v) : valueText(v);
      return text ? (
        <span title={text}>{text}</span>
      ) : (
        <span className="rx-blank">—</span>
      );
    },
  };
}
