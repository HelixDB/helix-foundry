import { useState, useEffect, lazy, Suspense, useDeferredValue } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Link,
  useParams,
  useSearchParams,
  useNavigate,
} from "react-router-dom";
import { Columns3, Plus } from "lucide-react";
import { api, number } from "./api";
import { paths } from "./paths";
import {
  useWorkspace,
  useResources,
  useAction,
  Loading,
  Modal,
  Menu,
  AskButton,
  DataTable,
  fieldName,
} from "./ui";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  FilterBar,
  FilterSelect,
  PageHeader,
  Skeleton,
  cx,
  type GridColumn,
  type GridSort,
} from "./kit";
import { typeHue } from "./areas/ontology/colors";
import type { Resource } from "../../../packages/shared/src";
import { ExplorerSwitch } from "./areas/ontology/views";
const ObjectActionForm = lazy(() => import("./object-action-form"));
const blank = (v: unknown) =>
  v == null || ["", "None", "null", "NaN"].includes(String(v).trim());
const display = (v: any) =>
  blank(v) ? "—" : typeof v === "object" ? JSON.stringify(v) : String(v);
const PAGE = 50;
// Columns worth showing first: not keys, IDs or names, which the record's
// name and links already stand for.
const idLike = (f: string, key?: string) =>
  f === key || /(^|_)(id|key|uuid)$/i.test(f);
// Fields that make up the record's name are already in its first column.
const nameLike = (f: string) =>
  /(^|_)(name|title|subject)$/i.test(f) || /(first|last)_?name$/i.test(f);
const defaultColumns = (fields: string[], key?: string) => {
  const useful = fields.filter((f) => !idLike(f, key) && !nameLike(f));
  return (useful.length ? useful : fields).slice(0, 4);
};
const numeric = (values: unknown[]) => {
  const present = values.filter((v) => !blank(v));
  return (
    present.length > 0 &&
    present.every((v) => typeof v !== "object" && Number.isFinite(Number(v)))
  );
};

// The Explorer's table: one type's records, searchable, filterable and
// sortable on the server, with their readable names and link counts.
export default function RecordsPage() {
  const { id, role, ask } = useWorkspace(),
    types = useResources("objectType"),
    { logicalId } = useParams(),
    navigate = useNavigate(),
    [params, setParams] = useSearchParams();
  const list: Resource[] = [...(types.data?.items || [])].sort((a, b) =>
      a.name.localeCompare(b.name),
    ),
    type = list.find((t) => t.name === params.get("type")) || list[0],
    // The graph's colours: every type with records, alphabetically.
    hues = list.filter((t) => t.data.objectCount > 0).map((t) => t.name);
  const [search, setSearch] = useState(params.get("search") || ""),
    deferred = useDeferredValue(search.trim()),
    [chosen, setChosen] = useState<Record<string, string[]>>({}),
    [field, setField] = useState(""),
    [value, setValue] = useState(""),
    [sort, setSort] = useState<GridSort>(null),
    [page, setPage] = useState(0),
    [creating, setCreating] = useState(false);
  const fields: string[] = type?.data.properties || [],
    shown =
      (type && chosen[type.name]) ||
      defaultColumns(fields, type?.data.primaryKey),
    filtering = !!field && !!value.trim();
  const objects = useQuery({
    queryKey: [
      id,
      "records",
      type?.name,
      deferred,
      filtering ? field : "",
      filtering ? value.trim() : "",
      sort?.key,
      sort?.dir,
      page,
    ],
    enabled: !!type,
    // Keep the rows on screen while the next page or sort loads.
    placeholderData: (previous, query) =>
      query?.queryKey[2] === type?.name ? previous : undefined,
    queryFn: () =>
      api(
        `/workspaces/${id}/resources/object?${new URLSearchParams({
          type: type.name,
          search: deferred,
          offset: String(page * PAGE),
          limit: String(PAGE),
          ...(filtering ? { field, value: value.trim() } : {}),
          ...(sort ? { sort: sort.key, dir: sort.dir } : {}),
        })}`,
      ),
  });
  useEffect(() => {
    setPage(0);
  }, [type?.name, deferred, field, value, sort?.key, sort?.dir]);
  const pick = (name: string) => {
    setParams({ type: name });
    setField("");
    setValue("");
    setSort(null);
  };
  if (types.isPending) return <Skeleton height={420} />;
  if (types.error) return <p role="alert">{types.error.message}</p>;
  const rows: Resource[] = objects.data?.items || [],
    total: number = objects.data?.total ?? 0,
    first = total ? page * PAGE + 1 : 0,
    last = Math.min(total, (page + 1) * PAGE);
  const columns: GridColumn<Resource>[] = [
    {
      key: "record",
      label: "Record",
      width: 260,
      render: (r) => (
        <span className="rx-record">
          <strong>{r.data.label || r.name}</strong>
          {r.data.label && r.data.label !== r.name && (
            <small>{r.data.key}</small>
          )}
        </span>
      ),
    },
    {
      key: "links",
      label: "Links",
      width: 90,
      align: "right",
      render: (r) => (
        <span className={r.data.links ? "" : "rx-blank"}>
          {r.data.links ? number(r.data.links) : "—"}
        </span>
      ),
    },
    ...shown.map(
      (f): GridColumn<Resource> => ({
        key: f,
        label: fieldName(f),
        align: numeric(rows.map((r) => r.data.effective?.[f]))
          ? "right"
          : "left",
        render: (r) => (
          <span className={blank(r.data.effective?.[f]) ? "rx-blank" : ""}>
            {display(r.data.effective?.[f])}
            {Object.hasOwn(r.data.edits || {}, f) && (
              <span className="edit-mark" title="Edited in this workspace">
                {" "}
                •
              </span>
            )}
          </span>
        ),
      }),
    ),
  ];
  return (
    <>
      <PageHeader
        title="Explorer"
        subtitle="Your records and how they connect."
        actions={
          <>
            <ExplorerSwitch />
            <ButtonLink to={paths.recordCollections()}>Collections</ButtonLink>
            {type && (
              <AskButton
                resourceId={type.data.datasetId}
                title={type.name}
                prompt={"Help me understand " + type.name + " records"}
              />
            )}
            {type && role !== "viewer" && (
              <Button
                variant="primary"
                icon={Plus}
                onClick={() => setCreating(true)}
              >
                Create record
              </Button>
            )}
          </>
        }
      />
      {!type ? (
        <EmptyState
          title="Your business records belong here"
          text="Use connected data to create a model of the things your team works with."
          action={
            <>
              {role !== "viewer" && (
                <Button
                  onClick={() =>
                    ask({
                      title: "your business model",
                      prompt: "Create a business model from my connected data",
                    })
                  }
                >
                  Prepare a business model
                </Button>
              )}
              <ButtonLink variant="ghost" to="/data">
                View connected data
              </ButtonLink>
            </>
          }
        />
      ) : (
        <div className="rx">
          <ul className="og-legend" aria-label="Object types">
            {list.map((t) => (
              <li key={t.id}>
                <button
                  type="button"
                  aria-pressed={t.id === type.id}
                  className={cx("og-type", t.id === type.id && "is-current")}
                  onClick={() => pick(t.name)}
                >
                  <span
                    className="og-dot"
                    style={{ background: typeHue(hues, t.name) || undefined }}
                    aria-hidden
                  />
                  {t.name}
                  <span className="og-count">{number(t.data.objectCount)}</span>
                </button>
              </li>
            ))}
          </ul>
          <FilterBar
            search={{
              value: search,
              onChange: setSearch,
              placeholder: `Search ${type.name.toLowerCase()} records`,
            }}
            count={
              total
                ? `${number(first)}–${number(last)} of ${number(total)}`
                : undefined
            }
            actions={
              <details className="rx-columns">
                <summary className="hf-btn hf-btn--secondary hf-btn--sm">
                  <Columns3 size={14} aria-hidden /> Columns
                </summary>
                <div className="rx-columns-menu">
                  {fields.map((f) => (
                    <label key={f}>
                      <input
                        type="checkbox"
                        checked={shown.includes(f)}
                        onChange={(e) =>
                          setChosen((c) => ({
                            ...c,
                            [type.name]: e.target.checked
                              ? fields.filter(
                                  (x) => x === f || shown.includes(x),
                                )
                              : shown.filter((x) => x !== f),
                          }))
                        }
                      />
                      {fieldName(f)}
                    </label>
                  ))}
                  <button
                    type="button"
                    className="og-link"
                    onClick={() =>
                      setChosen((c) => {
                        const next = { ...c };
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
              value={field}
              onChange={setField}
              options={[
                { value: "", label: "Filter by field…" },
                ...fields.map((f) => ({ value: f, label: fieldName(f) })),
              ]}
            />
            {field && (
              <>
                <input
                  className="rx-value"
                  aria-label={`${fieldName(field)} equals`}
                  placeholder="equals…"
                  value={value}
                  onChange={(e) => setValue(e.target.value)}
                />
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setField("");
                    setValue("");
                  }}
                >
                  Clear
                </Button>
              </>
            )}
          </FilterBar>
          {objects.error ? (
            <p role="alert">{(objects.error as Error).message}</p>
          ) : (
            <DataGrid
              label={`${type.name} records`}
              rows={rows}
              columns={columns}
              getRowKey={(r) => r.data.logicalId}
              selectedKey={logicalId}
              onRowClick={(r) =>
                navigate(paths.object(r.data.logicalId, type.name))
              }
              sort={sort}
              onSortChange={setSort}
              loading={objects.isPending}
              maxHeight={620}
              emptyText={
                deferred || filtering
                  ? "No records match."
                  : "No records of this type."
              }
              footer={
                total > PAGE && (
                  <div className="rx-pages">
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={page === 0}
                      onClick={() => setPage((p) => p - 1)}
                    >
                      Previous
                    </Button>
                    <span>
                      Page {page + 1} of {Math.ceil(total / PAGE)}
                    </span>
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={last >= total}
                      onClick={() => setPage((p) => p + 1)}
                    >
                      Next
                    </Button>
                  </div>
                )
              }
            />
          )}
        </div>
      )}
      {logicalId && (
        <RecordDetail
          key={logicalId}
          logicalId={logicalId}
          types={list}
          onClose={() => navigate(paths.explorer(type?.name))}
        />
      )}
      {creating && (
        <Modal title={"Create " + type.name} onClose={() => setCreating(false)}>
          <Suspense fallback={<Loading />}>
            <ObjectActionForm
              types={[type]}
              onDone={() => setCreating(false)}
            />
          </Suspense>
        </Modal>
      )}
    </>
  );
}
export function RecordDetail({
  logicalId,
  types,
  onClose,
}: {
  logicalId: string;
  types: Resource[];
  onClose: () => void;
}) {
  const { id, role } = useWorkspace(),
    action = useAction(),
    [editing, setEditing] = useState(false),
    [linking, setLinking] = useState(false),
    [values, setValues] = useState<Record<string, any>>({}),
    [preview, setPreview] = useState<any>(null);
  const q = useQuery({
      queryKey: [id, "record", logicalId],
      queryFn: () => api(`/workspaces/${id}/records/${logicalId}`),
    }),
    datasets = useResources("dataset");
  const r = q.data?.record,
    type = types.find((t) => t.name === r?.data.type),
    ds = datasets.data?.items.find(
      (d: Resource) => d.id === type?.data.datasetId,
    );
  const editable = (type?.data.properties || []).filter(
    (f: string) => f !== type?.data.primaryKey,
  );
  return (
    <Modal
      title={r?.name || "Record"}
      className="record-drawer"
      onClose={onClose}
    >
      {q.isPending ? (
        <Loading />
      ) : q.error ? (
        <p className="pad" role="alert">
          {q.error.message}
        </p>
      ) : (
        <div className="record-detail">
          <div className="record-actions">
            <AskButton resourceId={r.id} title={r.name} />
            {role !== "viewer" && (
              <>
                <button
                  className="button"
                  onClick={() => {
                    setEditing(true);
                    setValues({ ...r.data.effective });
                    setPreview(null);
                  }}
                >
                  Edit record
                </button>
                <button
                  className="text-button"
                  onClick={() => setLinking((v) => !v)}
                >
                  Relationships
                </button>
              </>
            )}
          </div>
          {linking && (
            <details open className="disclosure">
              <summary>Change relationships</summary>
              <Suspense fallback={<Loading />}>
                <ObjectActionForm
                  types={types}
                  initialRecord={r}
                  onDone={() => setLinking(false)}
                />
              </Suspense>
            </details>
          )}
          {editing ? (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const changed = Object.fromEntries(
                  editable
                    .filter(
                      (f: string) =>
                        JSON.stringify(values[f]) !==
                        JSON.stringify(r.data.effective[f]),
                    )
                    .map((f: string) => [f, values[f]]),
                );
                action.run(async () => {
                  setPreview({
                    ...(await api(`/workspaces/${id}/actions/preview`, {
                      objectId: r.id,
                      values: changed,
                    })),
                    values: changed,
                  });
                });
              }}
            >
              <div className="form-grid">
                {editable.map((f: string) => (
                  <label key={f}>
                    {fieldName(f)}
                    {typeof r.data.effective[f] === "boolean" ? (
                      <select
                        value={String(values[f])}
                        onChange={(e) => {
                          setValues((v) => ({
                            ...v,
                            [f]: e.target.value === "true",
                          }));
                          setPreview(null);
                        }}
                      >
                        <option value="true">True</option>
                        <option value="false">False</option>
                      </select>
                    ) : (
                      <input
                        value={values[f] ?? ""}
                        type={
                          typeof r.data.effective[f] === "number"
                            ? "number"
                            : "text"
                        }
                        step="any"
                        onChange={(e) => {
                          setValues((v) => ({
                            ...v,
                            [f]:
                              e.target.value === ""
                                ? null
                                : typeof r.data.effective[f] === "number"
                                  ? Number(e.target.value)
                                  : e.target.value,
                          }));
                          setPreview(null);
                        }}
                      />
                    )}
                  </label>
                ))}
              </div>
              <p className="muted">
                Changes stay in this workspace. Source snapshots are unchanged.
              </p>
              {preview && (
                <div className="change-preview">
                  <h3>Review changes</h3>
                  <DataTable
                    rows={
                      Object.keys(preview.values || {}).length
                        ? Object.keys(preview.values).map((f) => ({
                            field: fieldName(f),
                            before: display(preview.before[f]),
                            after: display(preview.after[f]),
                          }))
                        : editable
                            .filter(
                              (f: string) =>
                                JSON.stringify(preview.before[f]) !==
                                JSON.stringify(preview.after[f]),
                            )
                            .map((f: string) => ({
                              field: fieldName(f),
                              before: display(preview.before[f]),
                              after: display(preview.after[f]),
                            }))
                    }
                  />
                </div>
              )}
              <div className="buttons">
                {preview ? (
                  <button
                    type="button"
                    className="button primary"
                    disabled={action.pending}
                    onClick={() =>
                      action.run(async () => {
                        await api(`/workspaces/${id}/actions/execute`, {
                          objectId: r.id,
                          values: preview.values,
                          revision: preview.revision,
                          idempotencyKey: crypto.randomUUID(),
                        });
                        setEditing(false);
                        setPreview(null);
                      }, "Record updated")
                    }
                  >
                    Apply changes
                  </button>
                ) : (
                  <button className="button primary" disabled={action.pending}>
                    Preview changes
                  </button>
                )}
                <button
                  type="button"
                  className="text-button"
                  onClick={() => {
                    setEditing(false);
                    setPreview(null);
                  }}
                >
                  Cancel
                </button>
              </div>
            </form>
          ) : (
            <dl className="record-properties">
              {Object.entries(r.data.effective).map(([f, v]) => (
                <div key={f}>
                  <dt>{fieldName(f)}</dt>
                  <dd>
                    {display(v)}
                    {Object.hasOwn(r.data.edits || {}, f) && (
                      <small className="edit-label">Workspace edit</small>
                    )}
                  </dd>
                </div>
              ))}
            </dl>
          )}
          <section className="related-records">
            <h3>Related records</h3>
            {q.data.related.length ? (
              q.data.related.map((o: Resource) => (
                <Link
                  className="simple-row"
                  key={o.id}
                  to={paths.object(o.data.logicalId, o.data.type)}
                >
                  <strong>{o.name}</strong>
                  <span>{o.data.type}</span>
                </Link>
              ))
            ) : (
              <p className="muted">No related records.</p>
            )}
          </section>
          <details className="disclosure">
            <summary>Source and history</summary>
            {ds ? (
              <Link to={paths.dataset(ds.id)}>{ds.name}</Link>
            ) : (
              <p>Created in this workspace</p>
            )}
            <p className="muted">
              {r.data.sourceVersion
                ? "Imported snapshot " + r.data.sourceVersion.slice(0, 8)
                : "Workspace record"}
            </p>
            <DataTable rows={[r.data.base]} />
            <h3>Workspace changes</h3>
            {q.data.history?.length ? (
              q.data.history.map((h: Resource) => (
                <div key={h.id}>
                  <p>
                    {h.name} · {new Date(h.createdAt).toLocaleString()}
                  </p>
                  <details>
                    <summary>Changes</summary>
                    <pre>
                      {JSON.stringify(h.data.values || h.data.action, null, 2)}
                    </pre>
                  </details>
                </div>
              ))
            ) : (
              <p className="muted">No workspace changes recorded.</p>
            )}
          </details>
        </div>
      )}
    </Modal>
  );
}
