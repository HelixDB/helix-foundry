import { useRef, useState } from "react";
import { api } from "./api";
import { useWorkspace, DataTable } from "./ui";
import { useSetupAction } from "./setup-components";
import { ConnectionGuide } from "./connection-guides";
import type { Resource, HostedConnection } from "../../../packages/shared/src";
export function ConnectionForm({
  kind,
  onImported,
  correctSource,
  initialConnection,
}: {
  kind: string;
  onImported?: () => void | Promise<void>;
  correctSource?: Resource;
  initialConnection?: HostedConnection;
}) {
  const { id, role } = useWorkspace(),
    action = useSetupAction();
  const [form, setForm] = useState<Record<string, any>>({
      name: initialConnection?.name || correctSource?.name || "",
      port: kind === "mysql" ? 3306 : 5432,
    }),
    [discovered, setDiscovered] = useState<any>(
      initialConnection?.discovery || null,
    ),
    [selected, setSelected] = useState<string[]>([]),
    [preview, setPreview] = useState<any>(null),
    [apiToken, setApiToken] = useState("");
  const [manualPostgres, setManualPostgres] = useState(false);
  // A SQLite database is a file: it is uploaded when the connection is tested.
  const [sqliteFile, setSqliteFile] = useState<File | null>(null);
  // Only REST asks for a name; it has no table or object to name its data.
  const stripeMode = /_(live|test)_/.exec(form.apiKey || "")?.[1];
  let suggestedName =
    {
      postgres: form.database || "PostgreSQL",
      mysql: form.database || "MySQL",
      neo4j: form.database || "Neo4j",
      sqlite: sqliteFile?.name.replace(/\.[^.]+$/, "") || "SQLite",
      s3: form.bucket || "Blob storage",
      stripe: stripeMode ? `Stripe (${stripeMode})` : "Stripe",
      workos: "WorkOS",
      posthog: "PostHog",
    }[kind] || "";
  if (kind === "postgres" && !manualPostgres && form.connectionString) {
    try {
      suggestedName =
        decodeURIComponent(new URL(form.connectionString).pathname.slice(1)) ||
        "PostgreSQL";
    } catch {
      /* Validate the URL when testing the connection. */
    }
  }
  const connectionName = form.name?.trim() || suggestedName;
  const { connectionString, ...individualConfig } = form;
  const connectionConfig = initialConnection
    ? initialConnection.config
    : kind === "sqlite"
      ? { file: form.file }
      : kind === "postgres" && !manualPostgres
        ? {
            connectionString: (connectionString || "").trim(),
            tls: form.tls,
            ca: form.ca,
          }
        : individualConfig;
  const operation = useRef(crypto.randomUUID());
  const uploadKeys = useRef(new Map<string, string>());
  const update = (key: string, value: any) => {
    setForm((f) => ({ ...f, [key]: value }));
    operation.current = crypto.randomUUID();
    if (key !== "schedule") {
      setDiscovered(null);
      setSelected([]);
      setPreview(null);
    }
  };
  const finish = () => onImported?.();
  const selection = (t: any) =>
    kind === "s3"
      ? { key: t.name }
      : { table: t.name, ...(t.schema ? { schema: t.schema } : {}) };
  const keyFor = (t: any) => JSON.stringify(selection(t));
  const related = new Set<string>();
  for (const k of discovered?.foreignKeys || discovered?.keys || []) {
    const from = keyFor({
        name: k.tableName || k.table_name,
        schema: k.table_schema,
      }),
      to = keyFor({
        name: k.referencedTable || k.referenced_table,
        schema: k.referenced_schema,
      });
    if (!(k.referencedTable || k.referenced_table)) continue;
    if (selected.includes(from)) related.add(to);
    if (selected.includes(to)) related.add(from);
  }
  if (role === "viewer")
    return <p>An editor can add data to this workspace.</p>;
  return (
    <div className="connect-data">
      {kind === "upload" && (
        <form
          className="form-grid"
          onSubmit={(e) => {
            e.preventDefault();
            const files = Array.from(
              (e.currentTarget.elements.namedItem("files") as HTMLInputElement)
                .files || [],
            );
            action.run(async () => {
              for (const file of files) {
                const key = file.name + file.size + file.lastModified;
                if (!uploadKeys.current.has(key))
                  uploadKeys.current.set(key, crypto.randomUUID());
                const body = new FormData();
                body.set("file", file);
                await api(
                  `/workspaces/${id}/onboarding/uploads`,
                  body,
                  "POST",
                  { "idempotency-key": uploadKeys.current.get(key)! },
                );
              }
              await finish();
            });
          }}
        >
          <label className="span2">
            Choose files
            <input
              name="files"
              type="file"
              multiple
              required
              accept=".csv,.json,.jsonl,.parquet"
            />
          </label>
          <p className="muted span2">
            Up to 256 MB per file. Complete files are imported before preparing
            your view.
          </p>
          <button className="button primary" disabled={action.pending}>
            {action.pending ? "Uploading files…" : "Import files"}
          </button>
        </form>
      )}
      {kind === "api" && (
        <div>
          <p>
            Send up to 10,000 records in each request. New datasets appear here
            automatically.
          </p>
          {role === "owner" && !apiToken && (
            <button
              className="button"
              disabled={action.pending}
              onClick={() =>
                action.run(async () => {
                  const r = await api(`/workspaces/${id}/tokens`, {
                    name: "Data ingestion",
                    readOnly: false,
                  });
                  setApiToken(r.token);
                })
              }
            >
              Create ingestion token
            </button>
          )}
          {role !== "owner" && (
            <p>Ask an owner for a workspace ingestion token.</p>
          )}
          {apiToken && (
            <label>
              Save this token — it is shown once
              <input readOnly type="password" value={apiToken} />
              <button
                className="text-button"
                onClick={() => navigator.clipboard.writeText(apiToken)}
              >
                Copy token
              </button>
            </label>
          )}
          <pre>{`POST ${location.origin}/api/v1/workspaces/${id}/ingest
Authorization: Bearer YOUR_TOKEN
Content-Type: application/json

{"name":"Orders","rows":[{"order_id":"o1","amount":42}]}`}</pre>
          <p className="muted">
            Store the returned dataset ID and send it as datasetId when
            replacing that snapshot. Each ingest request creates a complete
            snapshot; it does not append.
          </p>
          <a href="/api/docs" target="_blank" rel="noreferrer">
            API reference
          </a>
        </div>
      )}
      {kind && !["upload", "api"].includes(kind) && (
        <form
          className="form-grid"
          onSubmit={(e) => {
            e.preventDefault();
            action.run(async () => {
              const selections =
                kind === "rest" ? [{}] : selected.map((key) => JSON.parse(key));
              if (correctSource) {
                await api(
                  `/workspaces/${id}/onboarding/sources/${correctSource.id}`,
                  {
                    kind,
                    name: connectionName,
                    config: { ...connectionConfig, ...selections[0] },
                    schedule: form.schedule || null,
                  },
                  "PUT",
                );
              } else {
                await api(`/workspaces/${id}/onboarding/sources`, {
                  kind,
                  name: connectionName,
                  config: connectionConfig,
                  selections,
                  schedule: form.schedule || null,
                  operationId: operation.current,
                });
              }
              await finish();
            });
          }}
        >
          {initialConnection && (
            <p className="span2 muted small">
              Connected to {initialConnection.name}. Choose the tables you want
              to use.
            </p>
          )}
          {!initialConnection && kind === "rest" && (
            <label className="span2">
              Connection name
              <input
                required
                value={form.name || ""}
                onChange={(e) => update("name", e.target.value)}
              />
            </label>
          )}
          {!initialConnection && kind === "postgres" && !manualPostgres && (
            <label className="span2">
              Connection URL
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required
                placeholder="postgresql://user:password@host/database"
                value={form.connectionString || ""}
                onChange={(e) => update("connectionString", e.target.value)}
              />
            </label>
          )}
          {!initialConnection &&
            ((kind === "postgres" && manualPostgres) || kind === "mysql") && (
              <>
                {[
                  ["host", "Host", "localhost"],
                  ["port", "Port", kind === "mysql" ? "3306" : "5432"],
                  ["database", "Database", "app"],
                  ["user", "Username", "readonly"],
                  ["password", "Password", ""],
                  ...(kind === "postgres"
                    ? [["schema", "Schema", "public"]]
                    : []),
                ].map(([key, label, placeholder]) => (
                  <label key={key}>
                    {label}
                    <input
                      type={key === "password" ? "password" : "text"}
                      required={key !== "password" && key !== "schema"}
                      placeholder={placeholder}
                      value={form[key] || ""}
                      onChange={(e) => update(key, e.target.value)}
                    />
                  </label>
                ))}
                {kind === "mysql" && (
                  <>
                    <label className="checkbox span2">
                      <input
                        type="checkbox"
                        checked={!!form.tls}
                        onChange={(e) => update("tls", e.target.checked)}
                      />{" "}
                      Use TLS with certificate verification
                    </label>
                    {form.tls && (
                      <label className="span2">
                        CA certificate (optional)
                        <textarea
                          value={form.ca || ""}
                          onChange={(e) => update("ca", e.target.value)}
                        />
                      </label>
                    )}
                  </>
                )}
              </>
            )}
          {!initialConnection && kind === "sqlite" && (
            <label className="span2">
              SQLite database file
              <input
                type="file"
                required
                accept=".db,.sqlite,.sqlite3,.db3"
                onChange={(e) => {
                  setSqliteFile(e.target.files?.[0] || null);
                  update("file", undefined);
                }}
              />
            </label>
          )}
          {!initialConnection && kind === "neo4j" && (
            <>
              {[
                ["url", "HTTP URL", "http://localhost:7474"],
                ["database", "Database", "neo4j"],
                ["user", "Username", "neo4j"],
                ["password", "Password", ""],
              ].map(([key, label, placeholder]) => (
                <label
                  key={key}
                  className={key === "url" ? "span2" : undefined}
                >
                  {label}
                  <input
                    type={key === "password" ? "password" : "text"}
                    autoComplete="off"
                    required={key === "url"}
                    placeholder={placeholder}
                    value={form[key] || ""}
                    onChange={(e) => update(key, e.target.value)}
                  />
                </label>
              ))}
            </>
          )}
          {!initialConnection && kind === "postgres" && (
            <>
              <button
                className="text-button span2"
                type="button"
                onClick={() => {
                  setManualPostgres(!manualPostgres);
                  setDiscovered(null);
                  setSelected([]);
                  setPreview(null);
                  operation.current = crypto.randomUUID();
                }}
              >
                {manualPostgres
                  ? "Use a connection URL"
                  : "Enter details individually"}
              </button>
              <details className="span2 disclosure">
                <summary>Connection options</summary>
                <div className="form-grid">
                  <label className="span2">
                    Connection name (optional)
                    <input
                      value={form.name || ""}
                      placeholder={suggestedName}
                      onChange={(e) => update("name", e.target.value)}
                    />
                  </label>
                  <label className="span2">
                    Encryption
                    <select
                      value={
                        form.tls === undefined
                          ? "default"
                          : form.tls
                            ? "require"
                            : "disable"
                      }
                      onChange={(e) =>
                        update(
                          "tls",
                          e.target.value === "default"
                            ? undefined
                            : e.target.value === "require",
                        )
                      }
                    >
                      <option value="default">
                        {manualPostgres
                          ? "Default"
                          : "Use URL settings (TLS by default)"}
                      </option>
                      <option value="require">
                        Require TLS with certificate verification
                      </option>
                      <option value="disable">No TLS</option>
                    </select>
                  </label>
                  <label className="span2">
                    CA certificate (optional)
                    <textarea
                      value={form.ca || ""}
                      onChange={(e) => update("ca", e.target.value)}
                    />
                  </label>
                </div>
              </details>
            </>
          )}
          {kind === "rest" && (
            <>
              {[
                ["url", "Endpoint URL"],
                ["token", "Bearer token / API key"],
                ["header", "API key header (optional)"],
                ["itemsPath", "Items path, e.g. data.items"],
                ["nextPath", "Next cursor or link path"],
              ].map(([k, l]) => (
                <label className="span2" key={k}>
                  {l}
                  <input
                    required={k === "url"}
                    type={k === "token" ? "password" : "text"}
                    value={form[k] || ""}
                    onChange={(e) => update(k, e.target.value)}
                  />
                </label>
              ))}
              <label>
                Pagination
                <select
                  aria-label="Pagination"
                  onChange={(e) => update("pagination", e.target.value)}
                  value={form.pagination || "none"}
                >
                  {["none", "next", "cursor", "offset"].map((x) => (
                    <option key={x}>{x}</option>
                  ))}
                </select>
              </label>
            </>
          )}
          {kind === "rest" && form.pagination && form.pagination !== "none" && (
            <details className="span2 disclosure">
              <summary>Pagination settings</summary>
              <div className="form-grid">
                {[
                  ["pageSize", "Page size", "100"],
                  ["offsetParam", "Offset parameter", "offset"],
                  ["limitParam", "Limit parameter", "limit"],
                  ["cursorParam", "Cursor parameter", "cursor"],
                ].map(([key, title, placeholder]) => (
                  <label key={key}>
                    {title}
                    <input
                      value={form[key] || ""}
                      placeholder={placeholder}
                      onChange={(e) => update(key, e.target.value)}
                    />
                  </label>
                ))}
              </div>
            </details>
          )}
          {(kind === "stripe" || kind === "workos") && (
            <label className="span2">
              {kind === "stripe" ? "Restricted API key" : "API key"}
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required
                placeholder={kind === "stripe" ? "rk_live_…" : "sk_…"}
                value={form.apiKey || ""}
                onChange={(e) => update("apiKey", e.target.value)}
              />
            </label>
          )}
          {kind === "posthog" && (
            <>
              <label className="span2">
                Personal API key
                <input
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  required
                  placeholder="phx_…"
                  value={form.apiKey || ""}
                  onChange={(e) => update("apiKey", e.target.value)}
                />
              </label>
              <label>
                Region
                <select
                  value={form.region || "us"}
                  onChange={(e) => update("region", e.target.value)}
                >
                  <option value="us">US Cloud</option>
                  <option value="eu">EU Cloud</option>
                  <option value="custom">Self-hosted</option>
                </select>
              </label>
              <label>
                Project ID (optional)
                <input
                  inputMode="numeric"
                  placeholder="Found in project settings"
                  value={form.projectId || ""}
                  onChange={(e) => update("projectId", e.target.value)}
                />
              </label>
              {form.region === "custom" && (
                <label className="span2">
                  PostHog URL
                  <input
                    required
                    placeholder="https://posthog.example.com"
                    value={form.host || ""}
                    onChange={(e) => update("host", e.target.value)}
                  />
                </label>
              )}
              <label>
                Event history (days)
                <input
                  type="number"
                  min={1}
                  max={365}
                  value={form.eventDays || 30}
                  onChange={(e) => update("eventDays", e.target.value)}
                />
              </label>
            </>
          )}
          {kind === "s3" &&
            [
              ["bucket", "Bucket"],
              ["region", "Region"],
              ["prefix", "Prefix"],
              ["endpoint", "Custom endpoint (optional)"],
              ["accessKeyId", "Access key"],
              ["secretAccessKey", "Secret key"],
            ].map(([k, l]) => (
              <label key={k}>
                {l}
                <input
                  type={k === "secretAccessKey" ? "password" : "text"}
                  value={form[k] || ""}
                  onChange={(e) => update(k, e.target.value)}
                />
              </label>
            ))}

          {!initialConnection && <ConnectionGuide kind={kind} form={form} />}
          {!initialConnection && (
            <div className="span2">
              <button
                className={discovered ? "button" : "button primary"}
                type="button"
                disabled={
                  action.pending ||
                  !connectionName ||
                  (kind === "postgres" &&
                    !manualPostgres &&
                    !form.connectionString?.trim()) ||
                  (kind === "sqlite" && !sqliteFile)
                }
                onClick={() =>
                  action.run(async () => {
                    let config = connectionConfig;
                    if (kind === "sqlite" && sqliteFile) {
                      const body = new FormData();
                      body.set("file", sqliteFile);
                      const { file } = await api(
                        `/workspaces/${id}/sources/sqlite`,
                        body,
                        "POST",
                      );
                      config = { file };
                      setForm((f) => ({ ...f, file }));
                    }
                    const result = await api(`/workspaces/${id}/sources/test`, {
                      kind,
                      name: connectionName,
                      config,
                    });
                    // Pin imports to the project that was tested.
                    if (result.projectId)
                      setForm((f) => ({ ...f, projectId: result.projectId }));
                    setDiscovered(result);
                    setSelected([]);
                  })
                }
              >
                {kind === "sqlite"
                  ? action.pending
                    ? "Reading file…"
                    : "Read file"
                  : action.pending
                    ? "Testing connection…"
                    : "Test connection"}
              </button>
            </div>
          )}
          {discovered && kind !== "rest" && (
            <fieldset className="span2 plain-fieldset">
              <legend>
                {kind === "s3"
                  ? "Choose files to import"
                  : kind === "stripe"
                    ? `Choose Stripe objects to import (${discovered.mode} mode)`
                    : kind === "workos" || kind === "posthog"
                      ? `Choose ${kind === "workos" ? "WorkOS" : "PostHog"} objects to import`
                      : kind === "neo4j"
                        ? "Choose labels and relationship types to import"
                        : "Choose tables to import"}
              </legend>
              {!correctSource && discovered.tables.length > 1 && (
                <div className="selection-actions">
                  <button
                    type="button"
                    className="text-button"
                    disabled={
                      selected.length >= Math.min(discovered.tables.length, 100)
                    }
                    onClick={() => {
                      operation.current = crypto.randomUUID();
                      setSelected(discovered.tables.slice(0, 100).map(keyFor));
                    }}
                  >
                    {discovered.tables.length > 100
                      ? "Select the first 100"
                      : "Select all"}
                  </button>
                  <button
                    type="button"
                    className="text-button"
                    disabled={selected.length === 0}
                    onClick={() => {
                      operation.current = crypto.randomUUID();
                      setSelected([]);
                    }}
                  >
                    Clear
                  </button>
                </div>
              )}
              <div className="table-selection">
                {discovered.tables.map((t: any) => {
                  const key = keyFor(t);
                  return (
                    <div className="selection-row" key={key}>
                      <label className="checkbox">
                        <input
                          type="checkbox"
                          checked={selected.includes(key)}
                          onChange={(e) => {
                            operation.current = crypto.randomUUID();
                            setSelected((s) =>
                              e.target.checked
                                ? correctSource
                                  ? [key]
                                  : [...s, key]
                                : s.filter((k) => k !== key),
                            );
                          }}
                        />
                        <span>
                          {kind === "neo4j"
                            ? t.name
                            : (t.schema ? t.schema + "." : "") + t.name}
                          {kind === "neo4j" && (
                            <small>
                              {t.schema === "relationship"
                                ? `relationship type${t.from && t.to ? `, ${t.from} to ${t.to}` : ""}`
                                : "label"}
                            </small>
                          )}
                          {related.has(key) && !selected.includes(key) && (
                            <small>Related to your selection</small>
                          )}
                        </span>
                      </label>
                      {kind !== "s3" && (
                        <button
                          type="button"
                          className="text-button"
                          onClick={() =>
                            action.run(async () =>
                              setPreview(
                                await api(`/workspaces/${id}/sources/test`, {
                                  kind,
                                  name: connectionName,
                                  config: {
                                    ...connectionConfig,
                                    ...selection(t),
                                  },
                                }),
                              ),
                            )
                          }
                        >
                          Preview
                        </button>
                      )}
                    </div>
                  );
                })}
              </div>
              <p className="muted small">
                Only selected{" "}
                {kind === "s3"
                  ? "files"
                  : kind === "stripe"
                    ? "objects"
                    : kind === "neo4j"
                      ? "labels and relationship types"
                      : "tables"}{" "}
                will be imported.
                {discovered.unavailable?.length > 0 &&
                  ` This key cannot read: ${discovered.unavailable.join(", ")}.`}
              </p>
            </fieldset>
          )}
          {discovered?.sample?.length > 0 && kind === "rest" && (
            <div className="span2">
              <DataTable rows={discovered.sample} />
            </div>
          )}
          {preview && (
            <details open className="span2 disclosure">
              <summary>Sample and schema</summary>
              <DataTable rows={preview.sample || []} />
              <pre>{JSON.stringify(preview.columns, null, 2)}</pre>
            </details>
          )}
          {discovered && (
            <button
              className="button primary"
              disabled={action.pending || (kind !== "rest" && !selected.length)}
            >
              {action.pending
                ? "Saving connection…"
                : correctSource
                  ? "Save and retry import"
                  : `Import ${kind === "rest" ? "data" : selected.length + " selected"}`}
            </button>
          )}
        </form>
      )}
      {action.error && (
        <p role="alert" className="error-box">
          {action.error}
        </p>
      )}
    </div>
  );
}
