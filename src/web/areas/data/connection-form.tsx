import { useRef, useState } from "react";
import type {
  Connector,
  ConnectorDiscovery,
  S3ConnectorSetup,
} from "../../../shared/wfe";
import { Select, Tag, connectorLabel } from "../../kit";
import { useWorkspace } from "../../ui";
import { ProGate, isProRequired } from "../workspace/pro-gate";
import { ConnectionGuide } from "./connection-guides";
import { FileTableDetails, S3Fields, fileSummary } from "./connection-s3";
import {
  createConnector,
  syncConnector,
  testConnector,
  updateConnector,
} from "./data-api";
import {
  emptyForm,
  formFromConfig,
  formatBytes,
  hasCredential,
  hasTableChoice,
  isDatabase,
  keySummary,
  relatedTables,
  suggestedName,
  tableSelection,
  toConfig,
  type ConnectionForm as Form,
  type CredentialKind,
} from "./model";

// One kind's credential form, in helix-foundry's connection-form style:
// fill in the credential, test it, choose what to sync, then connect. The
// credential lives in this form only: it is sent to the WFE, which never
// returns it, and is dropped when the dialog closes.
//
// Editing (`connector` set) starts from the stored settings with the
// credential blank; left blank, the WFE keeps the stored one.
//
// S3 offers an IAM role only where the kind carries its setup (the
// principal and external ID to trust); otherwise a new connection reads with
// a key, and a stored role can be renamed but not tested.
//
// Granola writes each note as a document, and its transcript too when
// Transcripts is on: a choice of what to sync, not of what the key reads,
// so it leaves a test standing.

// Kinds whose credential is one API key: its field's label and placeholder.
const keyFields: Record<
  "stripe" | "workos" | "linear" | "calcom" | "granola",
  [label: string, placeholder: string]
> = {
  stripe: ["Restricted API key", "rk_live_…"],
  workos: ["API key", "sk_…"],
  linear: ["Personal API key", "lin_api_…"],
  calcom: ["API key", "cal_live_…"],
  granola: ["API key", "grn_…"],
};

type Field = { key: string; label: string; placeholder?: string };
const databaseFields = (kind: "postgres" | "mysql"): Field[] => [
  { key: "host", label: "Host", placeholder: "db.example.com" },
  {
    key: "port",
    label: "Port",
    placeholder: kind === "mysql" ? "3306" : "5432",
  },
  { key: "database", label: "Database", placeholder: "app" },
  { key: "user", label: "Username", placeholder: "foundry_readonly" },
  { key: "password", label: "Password" },
  ...(kind === "postgres"
    ? [{ key: "schema", label: "Schema", placeholder: "public" }]
    : []),
];

export function ConnectionForm({
  kind,
  connector,
  setup,
  onDone,
}: {
  kind: CredentialKind;
  // Editing this connector rather than creating one.
  connector?: Connector;
  // The kind's S3 role setup, where role access is configured.
  setup?: S3ConnectorSetup | null;
  onDone: (connector: Connector, message: string) => void | Promise<void>;
}) {
  const { id, notify } = useWorkspace();
  const editing = !!connector,
    stored = connector?.config,
    storedTables =
      (stored &&
        ("postgres" in stored
          ? stored.postgres.tables
          : "mysql" in stored
            ? stored.mysql.tables
            : "s3" in stored
              ? stored.s3.tables
              : undefined)) ||
      [],
    storedKey = !!stored && "s3" in stored && "keys" in stored.s3;
  const roleOff = !setup?.principalArn;
  const [form, setForm] = useState<Form>(() => {
      // An edit keeps the stored access, so a stored role stays one.
      if (editing) return formFromConfig(kind, stored);
      const f = emptyForm(kind);
      return f.kind === "s3" && roleOff ? { ...f, access: "keys" } : f;
    }),
    [name, setName] = useState(connector?.name || ""),
    [dirty, setDirty] = useState(false),
    [discovered, setDiscovered] = useState<ConnectorDiscovery | null>(null),
    [chosen, setChosen] = useState<Set<string>>(new Set()),
    // What is in flight: a test or a save.
    [pending, setPending] = useState<"test" | "save" | null>(null),
    [error, setError] = useState<Error | null>(null);
  const label = connectorLabel(kind),
    database = isDatabase(kind),
    choice = hasTableChoice(kind),
    secretHint = connector?.secretHint;
  const value = (key: string) => String((form as Record<string, unknown>)[key]);
  // Counts changes to the settings, so a test can tell they changed while
  // it ran (an S3 test can take most of a minute).
  const edits = useRef(0);
  // Any change to the settings or credential invalidates the last test.
  const change = (patch: Partial<Form>) => {
    edits.current++;
    setForm((f) => ({ ...f, ...patch }) as Form);
    setDirty(true);
    setDiscovered(null);
    setChosen(new Set());
  };
  const update = (key: string, v: string) => change({ [key]: v });
  const run = async (what: "test" | "save", fn: () => Promise<void>) => {
    setPending(what);
    setError(null);
    try {
      await fn();
    } catch (e) {
      setError(e as Error);
    } finally {
      setPending(null);
    }
  };
  const tables = discovered?.tables || [],
    names = tables.map((t) => t.name),
    related = relatedTables(tables, chosen),
    unavailable = discovered?.unavailable || [];
  const selection = choice
    ? discovered
      ? tableSelection(names, chosen)
      : storedTables
    : [];
  // File sources cap the stored bytes a connection syncs.
  const maxBytes = Number(discovered?.maxBytes || 0),
    chosenBytes = tables
      .filter((t) => chosen.has(t.name))
      .reduce((n, t) => n + Number(t.bytes || 0), 0),
    overLimit = maxBytes > 0 && chosenBytes > maxBytes;
  const test = () =>
    run("test", async () => {
      const at = edits.current;
      // A test of settings changed since reports nothing, pass or fail: its
      // tables and its error describe the old ones.
      const result = await testConnector(id, toConfig(form, [])).catch(
        (e: unknown) => {
          if (edits.current === at) throw e;
        },
      );
      if (!result || edits.current !== at) return;
      const found = result.discovery || { tables: [] };
      // Pin syncs to the PostHog project the test resolved, and keep the
      // bucket's region it detected so syncs need not.
      const project =
          result.config && "posthog" in result.config
            ? result.config.posthog.projectId
            : "",
        region =
          result.config && "s3" in result.config ? result.config.s3.region : "";
      setForm((f) =>
        f.kind === "posthog" && project && !f.projectId
          ? { ...f, projectId: project }
          : f.kind === "s3" && region && region !== f.region
            ? { ...f, region }
            : f,
      );
      setDiscovered(found);
      const all = (found.tables || []).map((t) => t.name);
      // A new connection syncs every table until some are cleared; an edit
      // keeps its stored choice.
      setChosen(
        new Set(
          editing && storedTables.length
            ? all.filter((t) => storedTables.includes(t))
            : all,
        ),
      );
    });
  const submit = () =>
    run("save", async () => {
      if (selection === "none") return;
      const config = toConfig(form, selection);
      if (connector) {
        const renamed = name.trim() !== connector.name,
          reconfigured = dirty || !!discovered;
        const saved = await updateConnector(id, connector.id, {
          ...(renamed ? { name: name.trim() } : {}),
          ...(reconfigured ? { config } : {}),
        });
        await onDone(saved.connector, "Connection saved");
        return;
      }
      const created = await createConnector(id, {
        name: name.trim() || suggestedName(form),
        config,
      });
      // The first sync starts with the connection; a refusal is reported but
      // the connection is kept.
      await syncConnector(id, created.connector.id).catch((e) =>
        notify((e as Error).message, true),
      );
      await onDone(created.connector, "Connected. The first sync is queued");
    });
  const canSubmit =
    selection !== "none" &&
    !overLimit &&
    (editing
      ? name.trim() !== "" &&
        (name.trim() !== connector!.name || dirty || !!discovered)
      : !!discovered);
  const credentialRequired = !editing,
    // A role can't be tested where role access is off.
    untestable = form.kind === "s3" && form.access === "role" && roleOff;
  return (
    <div className="connect-data">
      <form
        className="form-grid"
        onSubmit={(e) => {
          e.preventDefault();
          if (canSubmit && !pending) void submit();
        }}
      >
        {/* The steps end "paste it below": they come before the fields. */}
        {!editing && <ConnectionGuide form={form} />}
        {editing && (
          <label className="span2">
            Connection name
            <input
              required
              maxLength={120}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
        )}
        {(form.kind === "stripe" ||
          form.kind === "workos" ||
          form.kind === "linear" ||
          form.kind === "calcom" ||
          form.kind === "granola") && (
          <label className="span2">
            {keyFields[form.kind][0]}
            <input
              type="password"
              autoComplete="off"
              spellCheck={false}
              required={credentialRequired}
              placeholder={
                secretHint
                  ? `Leave blank to keep ${secretHint}`
                  : keyFields[form.kind][1]
              }
              value={form.apiKey}
              onChange={(e) => update("apiKey", e.target.value)}
            />
          </label>
        )}
        {form.kind === "granola" && (
          <div className="span2 selection-row">
            <label className="checkbox">
              <input
                type="checkbox"
                checked={form.transcripts}
                onChange={(e) => {
                  const transcripts = e.target.checked;
                  setForm((f) => ({ ...f, transcripts }) as Form);
                  setDirty(true);
                }}
              />
              <span>
                Transcripts
                <small>
                  Each note’s transcript as a document too, in a Transcripts set
                  beside Notes. Transcripts are long: reading one into an
                  ontology costs several times what a note does.
                </small>
              </span>
            </label>
          </div>
        )}
        {form.kind === "github" && (
          <>
            <label className="span2">
              Personal access token
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required={credentialRequired}
                placeholder={
                  secretHint
                    ? `Leave blank to keep ${secretHint}`
                    : "github_pat_…"
                }
                value={form.token}
                onChange={(e) => update("token", e.target.value)}
              />
            </label>
            <label className="span2">
              Repositories (optional)
              <textarea
                spellCheck={false}
                placeholder={"acme/api\nacme/web"}
                value={form.repos}
                onChange={(e) => update("repos", e.target.value)}
              />
              <small className="muted">
                One owner/name per line. Leave blank to sync every repository
                the token can read, up to 100.
              </small>
            </label>
            <label>
              Review history (days)
              <input
                type="number"
                min={1}
                max={365}
                value={form.reviewDays}
                onChange={(e) => update("reviewDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "posthog" && (
          <>
            <label className="span2">
              Personal API key
              <input
                type="password"
                autoComplete="off"
                spellCheck={false}
                required={credentialRequired}
                placeholder={
                  secretHint ? `Leave blank to keep ${secretHint}` : "phx_…"
                }
                value={form.apiKey}
                onChange={(e) => update("apiKey", e.target.value)}
              />
            </label>
            <label>
              Region
              <Select
                label="Region"
                value={form.region}
                onChange={(v) => update("region", v)}
                options={[
                  { value: "us", label: "US Cloud" },
                  { value: "eu", label: "EU Cloud" },
                  { value: "custom", label: "Self-hosted" },
                ]}
              />
            </label>
            <label>
              Project ID (optional)
              <input
                inputMode="numeric"
                placeholder="Found in project settings"
                value={form.projectId}
                onChange={(e) => update("projectId", e.target.value)}
              />
            </label>
            {form.region === "custom" && (
              <label className="span2">
                PostHog URL
                <input
                  required
                  type="url"
                  placeholder="https://posthog.example.com"
                  value={form.host}
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
                value={form.eventDays}
                onChange={(e) => update("eventDays", e.target.value)}
              />
            </label>
          </>
        )}
        {form.kind === "s3" && (
          <S3Fields
            form={form}
            setup={setup}
            editing={editing}
            storedKey={storedKey}
            onChange={change}
          />
        )}
        {(form.kind === "postgres" || form.kind === "mysql") &&
          databaseFields(form.kind).map(({ key, label, placeholder }) => (
            <label key={key}>
              {label}
              <input
                type={
                  key === "password"
                    ? "password"
                    : key === "port"
                      ? "number"
                      : "text"
                }
                min={key === "port" ? 1 : undefined}
                max={key === "port" ? 65535 : undefined}
                autoComplete="off"
                spellCheck={false}
                required={
                  key === "password"
                    ? credentialRequired
                    : !["port", "schema"].includes(key)
                }
                placeholder={
                  key === "password" && editing
                    ? "Leave blank to keep the stored password"
                    : placeholder
                }
                value={value(key)}
                onChange={(e) => update(key, e.target.value)}
              />
            </label>
          ))}
        {(!editing || database) && (
          <details className="span2 disclosure">
            <summary>Connection options</summary>
            <div className="form-grid">
              {!editing && (
                <label className="span2">
                  Connection name (optional)
                  <input
                    maxLength={120}
                    value={name}
                    placeholder={suggestedName(form)}
                    onChange={(e) => setName(e.target.value)}
                  />
                </label>
              )}
              {(form.kind === "postgres" || form.kind === "mysql") && (
                <>
                  <label className="span2">
                    Encryption
                    <Select
                      label="Encryption"
                      value={form.tls}
                      onChange={(v) => update("tls", v)}
                      options={[
                        {
                          value: "verify",
                          label: "Require TLS with certificate verification",
                        },
                        { value: "disable", label: "No TLS" },
                      ]}
                    />
                  </label>
                  {form.tls === "verify" && (
                    <label className="span2">
                      CA certificate (optional)
                      <textarea
                        placeholder="-----BEGIN CERTIFICATE-----"
                        value={form.ca}
                        onChange={(e) => update("ca", e.target.value)}
                      />
                    </label>
                  )}
                </>
              )}
            </div>
          </details>
        )}
        {editing && choice && !discovered && (
          <p className="span2 muted small">
            {storedTables.length
              ? `Syncs ${storedTables.join(", ")}.`
              : "Syncs every table."}{" "}
            To change which tables sync,{" "}
            {form.kind === "s3"
              ? form.access === "keys"
                ? "enter the secret access key and test the connection."
                : untestable
                  ? "use an access key and test the connection."
                  : "test the connection."
              : "enter the password and test the connection."}
          </p>
        )}
        {(!editing || hasCredential(form)) && !untestable && (
          <div className="span2">
            <button
              className={discovered ? "button" : "button primary"}
              type="button"
              disabled={!!pending}
              onClick={() => void test()}
            >
              {pending === "test" ? "Testing connection…" : "Test connection"}
            </button>
          </div>
        )}
        {discovered && choice && (
          <fieldset className="span2 plain-fieldset">
            <legend>Choose tables to sync</legend>
            {form.kind === "s3" && form.region && (
              <p className="muted small">The bucket is in {form.region}.</p>
            )}
            {names.length > 1 && (
              <div className="selection-actions">
                <button
                  type="button"
                  className="text-button"
                  disabled={chosen.size === names.length}
                  onClick={() => setChosen(new Set(names))}
                >
                  Select all
                </button>
                <button
                  type="button"
                  className="text-button"
                  disabled={chosen.size === 0}
                  onClick={() => setChosen(new Set())}
                >
                  Clear
                </button>
              </div>
            )}
            <div className="table-selection">
              {tables.map((t) => (
                <div
                  className={"selection-row" + (t.format ? " dc-file-row" : "")}
                  key={t.name}
                  role={t.format ? "group" : undefined}
                  aria-label={t.format ? t.name : undefined}
                >
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={chosen.has(t.name)}
                      onChange={(e) =>
                        setChosen((s) => {
                          const next = new Set(s);
                          if (e.target.checked) next.add(t.name);
                          else next.delete(t.name);
                          return next;
                        })
                      }
                    />
                    <span>
                      {t.name}
                      {t.format && (
                        <Tag className="dc-format">
                          {t.format.toUpperCase()}
                        </Tag>
                      )}
                      <small>
                        {t.format ? fileSummary(t) : keySummary(t)}
                        {related.has(t.name) && " · Related to your selection"}
                      </small>
                      {t.problem && (
                        <small className="dc-problem">
                          {t.problem}. The sync will fail until this is fixed.
                        </small>
                      )}
                    </span>
                  </label>
                  {t.format && <FileTableDetails table={t} />}
                </div>
              ))}
            </div>
            {maxBytes > 0 && (
              <p
                className={"small " + (overLimit ? "error-text" : "muted")}
                role="status"
              >
                Selected {formatBytes(String(chosenBytes))} of{" "}
                {formatBytes(discovered.maxBytes)}
                {overLimit &&
                  ". Choose fewer tables or a narrower folder to connect"}
                .
              </p>
            )}
            <p className="muted small">
              {names.length
                ? "Only selected tables are synced. Selecting every table also syncs tables added later."
                : "No tables found. Check the schema and the user’s read permissions."}
            </p>
            {!!unavailable.length && (
              <div className="muted small">
                Helix can’t read:
                <ul className="dc-unavailable">
                  {unavailable.map((u) => (
                    <li key={u}>{u}</li>
                  ))}
                </ul>
              </div>
            )}
          </fieldset>
        )}
        {discovered && !choice && (
          <fieldset className="span2 plain-fieldset">
            <legend>{label} objects this key can read</legend>
            <div className="table-selection" role="list">
              {tables.map((t) => (
                <div className="selection-row" key={t.name} role="listitem">
                  <span className="checkbox">
                    <span>
                      {t.name}
                      <small>{keySummary(t)}</small>
                    </span>
                  </span>
                </div>
              ))}
            </div>
            <p className="muted small">
              Every object listed is synced.
              {!!discovered.unavailable?.length &&
                ` This key cannot read: ${discovered.unavailable.join(", ")}.`}
            </p>
          </fieldset>
        )}
        {!!discovered?.warnings?.length && (
          <ul className="span2 small dc-warnings" aria-label="Warnings">
            {discovered.warnings.map((w) => (
              <li key={w}>{w.charAt(0).toUpperCase() + w.slice(1)}.</li>
            ))}
          </ul>
        )}
        {(editing || discovered) && (
          <button className="button primary" disabled={!!pending || !canSubmit}>
            {pending === "save"
              ? "Saving connection…"
              : editing
                ? "Save changes"
                : choice
                  ? `Connect and sync ${
                      selection === "none"
                        ? "tables"
                        : selection.length
                          ? `${selection.length} ${selection.length === 1 ? "table" : "tables"}`
                          : "every table"
                    }`
                  : "Connect and sync"}
          </button>
        )}
      </form>
      {/* The plan can change under an open form: the WFE's Pro refusal
          becomes the gate, with its way to upgrade. */}
      {error &&
        (isProRequired(error) ? (
          <ProGate feature={`${label} connections`} />
        ) : (
          <p role="alert" className="error-box">
            {error.message}
          </p>
        ))}
    </div>
  );
}
