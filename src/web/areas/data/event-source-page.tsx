import { useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Braces,
  Folder,
  MoreHorizontal,
  Pencil,
  Table2,
  Trash2,
} from "lucide-react";
import type { Connector, FoundryEventBatch } from "../../../shared/wfe";
import { paths } from "../../paths";
import { RefreshFailed, useAction, useWorkspace } from "../../ui";
import {
  Button,
  ConfirmDialog,
  DataGrid,
  DropdownMenu,
  EmptyState,
  KeyValue,
  MenuItem,
  Page,
  PageHeader,
  Panel,
  Section,
  Select,
  SourceLogo,
  Tag,
  connectorLabel,
  formatDate,
  formatNumber,
  formatRelative,
  plural,
} from "../../kit";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { readersText } from "../documents/model";
import { useOntologyNames } from "../documents/sets";
import { Modal } from "./connect-data";
import {
  connectorQuery,
  deleteConnector,
  setEventTextField,
  syncConnector,
  useSetsAfterSyncs,
} from "./data-api";
import { EventSourceForm } from "./event-source-form";
import {
  EventSourceSend,
  EventSourceSendDialog,
  useEventSends,
} from "./event-source-send";
import {
  canManage,
  eventCapacity,
  eventSourceLine,
  eventsConfig,
  eventsTable,
  formatBytes,
  isLiveSync,
  piecesSet,
  setHolding,
  setSpend,
  textFieldChoices,
  textFieldConfirm,
  textFieldProblem,
} from "./model";

// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;

const crumbs = [{ label: "Connectors", to: paths.data() }];
// As the connection page's sync history: four columns of one width.
const COL = 170;

/**
 * /data/connections/:connectorId of an API source: what it is (its name and
 * description, which owners and admins edit), the fields its pieces have
 * carried with their types (read-only: a field keeps its first type), its
 * table, its state, capacity and recent sends, and its send panel. It has
 * no credential, test, schedule or Sync: pieces sync as they arrive. A sync
 * that failed shows why, with Resume. While it waits for pieces, or a sync
 * of it is live (a new text field's), it polls its sends and itself, so its
 * state moves on without a reload, and its set is read again as each sync
 * ends. ?token=new (the Helix MCP's token link) opens the send panel in a
 * dialog. Its text field (owners and admins choose it; members read it) makes
 * its newest pieces documents of its own set (Documents), which shows how
 * many it holds of how many, with a link into its folder of the Data
 * explorer, and Details what extracting them has spent of its allowance
 * this month. `source` is the page's connector query's, re-read with it.
 */
export function EventSourcePage({ source: c }: { source: Connector }) {
  const { id, role, notify } = useWorkspace(),
    manage = canManage(role),
    action = useAction(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    [params, setParams] = useSearchParams();
  const { sends, state, waiting } = useEventSends(c);
  // Another reader of the page's connector query, polling while it waits or
  // a sync of it is live.
  useQuery(connectorQuery(id, c.id, waiting || isLiveSync(c.lastSync)));
  useSetsAfterSyncs(id, [c]);
  const sets = useQuery(documentSetQueries.list(id)),
    ontologyName = useOntologyNames(),
    pieces = piecesSet(sets.data, c.id),
    piecesLabel = pieces?.name || pieces?.id || "",
    readers = pieces?.readBy || [],
    spend = setSpend(pieces?.connector);
  // Its newest piece's page opens the set's folder in the Data explorer. An
  // emptied set keeps no link to a piece it held.
  const listed = useInfiniteQuery({
      ...documentQueries.set(id, pieces?.id || ""),
      enabled: !!pieces?.documents,
    }),
    newest = pieces?.documents
      ? listed.data?.pages[0]?.documents?.[0]
      : undefined;
  const [editing, setEditing] = useState(false),
    [deleting, setDeleting] = useState<"confirm" | "pending" | null>(null);
  const tokenDialog = params.get("token") === "new",
    closeTokenDialog = () =>
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          next.delete("token");
          return next;
        },
        { replace: true },
      );
  const name = c.name || connectorLabel(c.kind),
    { description, fields = [], textField } = eventsConfig(c),
    table = eventsTable(c),
    batches = sends.data?.batches || [],
    last = batches[0]?.createdAt,
    capacity = eventCapacity(sends.data?.capacity),
    failed =
      c.lastSync?.status === "CONNECTOR_SYNC_STATUS_FAILED" && c.lastSync;
  // Leaves the page before the deleted source is refetched as missing.
  const remove = async () => {
    setDeleting("pending");
    try {
      await deleteConnector(id, c.id);
      navigate(paths.data());
      qc.removeQueries({ queryKey: [id, "connectors", c.id] });
      // Its set and its documents went with it.
      await Promise.all(
        ["connectors", "document-sets", "documents"].map((key) =>
          qc.invalidateQueries({ queryKey: [id, key] }),
        ),
      );
      notify("API source deleted");
    } catch (e) {
      setDeleting("confirm");
      notify((e as Error).message, true);
    }
  };
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: name }]}
        icon={<SourceLogo source={c.kind} size={28} />}
        title={name}
        subtitle={connectorLabel(c.kind)}
        meta={
          <>
            <span>
              {state
                ? eventSourceLine(state)
                : sends.isPending
                  ? "Reading its sends…"
                  : "Couldn’t read its sends"}
            </span>
            {last && <span>Last piece received {formatRelative(last)}</span>}
          </>
        }
        actions={
          manage && (
            <DropdownMenu
              label="API source actions"
              align="end"
              trigger={<MoreHorizontal size={16} aria-hidden />}
            >
              <MenuItem icon={Pencil} onSelect={() => setEditing(true)}>
                Edit API source
              </MenuItem>
              <MenuItem
                icon={Trash2}
                danger
                onSelect={() => setDeleting("confirm")}
              >
                Delete API source
              </MenuItem>
            </DropdownMenu>
          )
        }
      />
      <RefreshFailed query={sends} />
      {failed && (
        <div role="alert" className="error-box dc-sync-error dc-event-paused">
          <p>
            <strong>The last sync failed.</strong> {failed.error}
          </p>
          {manage && (
            <Button
              size="sm"
              pending={action.pending}
              onClick={() =>
                void action.run(() => syncConnector(id, c.id), "Sync queued")
              }
            >
              Resume
            </Button>
          )}
        </div>
      )}
      <div className="dc-connection-grid">
        <Panel title="Recent sends" padded={false}>
          <DataGrid<FoundryEventBatch>
            label="Recent sends"
            loading={sends.isPending}
            rows={batches}
            getRowKey={(b) => b.id}
            maxHeight={360}
            emptyText={
              sends.error && !sends.data
                ? `Couldn’t load its sends: ${sends.error.message}`
                : "Nothing sent yet."
            }
            columns={[
              {
                key: "createdAt",
                label: "Received",
                type: "timestamp",
                width: COL,
                render: (b) => (
                  <span className="dc-clip" title={formatDate(b.createdAt)}>
                    {b.createdAt ? formatRelative(b.createdAt) : "—"}
                  </span>
                ),
              },
              {
                key: "pieces",
                label: "Pieces",
                type: "integer",
                width: COL,
                render: (b) => formatNumber(b.pieces || 0),
              },
              {
                key: "bytes",
                label: "Size",
                width: COL,
                sortValue: (b) => Number(b.bytes || 0),
                render: (b) => formatBytes(b.bytes),
              },
              {
                key: "synced",
                label: "Table",
                width: COL,
                render: (b) => (b.synced ? "Synced" : "Arriving"),
              },
            ]}
          />
        </Panel>
        <Panel title="Details">
          <KeyValue
            items={[
              { label: "Connector", value: connectorLabel(c.kind) },
              { label: "Description", value: description },
              {
                label: "Text field",
                value: textField ? (
                  <span className="hf-mono">{textField}</span>
                ) : (
                  "None"
                ),
              },
              ...(spend
                ? [
                    {
                      label: "Extraction",
                      value: (
                        <span
                          className={
                            spend.level === "room"
                              ? undefined
                              : "dc-capacity-warn"
                          }
                        >
                          {spend.text}
                        </span>
                      ),
                      hint:
                        spend.level === "paused"
                          ? `Paused until ${spend.until}: its syncs add no new pieces as documents until then, so ontologies keep the pieces they have.`
                          : spend.level === "warn"
                            ? "Once it is spent, its syncs add no new pieces as documents until next month."
                            : "Extracting its pieces as they arrive and at refreshes, for every ontology that reads them. A load a person starts spends its own budget.",
                    },
                  ]
                : []),
              {
                label: "Capacity",
                value: capacity && (
                  <span
                    className={capacity.warn ? "dc-capacity-warn" : undefined}
                  >
                    {capacity.text}
                  </span>
                ),
                hint: capacity?.warn
                  ? "Once it is full, sends are refused: send new pieces to a new API source before then."
                  : undefined,
              },
              {
                label: "Created",
                value: c.createdAt && formatDate(c.createdAt),
              },
            ]}
          />
        </Panel>
      </div>
      <Section
        title="Fields"
        id="es-fields"
        description="The top-level keys its pieces have carried, each with the JSON type it first had. A field keeps its first type."
      >
        {fields.length ? (
          <DataGrid
            label="Fields"
            rows={fields}
            getRowKey={(f) => f.name}
            maxHeight={320}
            columns={[
              {
                key: "name",
                label: "Field",
                width: 260,
                render: (f) => <span className="hf-mono">{f.name}</span>,
              },
              {
                key: "type",
                label: "Type",
                width: 200,
                render: (f) =>
                  !f.type || f.type === "null" ? "null so far" : f.type,
              },
            ]}
          />
        ) : (
          <EmptyState
            size="sm"
            icon={Braces}
            title="No fields yet"
            text="Its fields are learned from the pieces sent to it."
          />
        )}
      </Section>
      <Section
        title="Table"
        id="es-table"
        description="Its pieces, one row per id: a later piece with the same id replaces the earlier one."
      >
        {table ? (
          <ul className="dc-dataset-list" aria-label="Synced tables">
            <li>
              <Link to={paths.syncedTable(c.id, table.name)}>
                <Table2 size={15} aria-hidden />
                <strong className="ds-name" title={table.name}>
                  {table.name}
                </strong>
                <span className="dc-dataset-meta">
                  {formatNumber(table.rowCount)} rows ·{" "}
                  {plural((table.columns || []).length, "column")}
                </span>
                <span className="dc-dataset-meta">Key id</span>
              </Link>
            </li>
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Table2}
            title="No table yet"
            text="Its table appears with the first sync, about a minute after the first pieces arrive."
          />
        )}
      </Section>
      <Section
        title="Documents"
        id="es-documents"
        description="With a text field, its newest pieces are Markdown documents of a set of its own, which ontologies read by entity extraction. Extracting them as they arrive is about 1 cent a piece for each ontology, within the API source’s allowance for the month."
      >
        <div className="dc-event-documents">
          {manage && <EventSourceTextField source={c} />}
          {pieces ? (
            <ul className="dt-sets" aria-label="Document sets">
              <li aria-label={piecesLabel}>
                <Folder size={16} className="dt-icon" aria-hidden />
                <span className="dt-set-text">
                  <strong>
                    {newest ? (
                      <Link
                        to={paths.dataDocument(newest.id)}
                        title="Its newest piece, in its folder of the Data explorer"
                      >
                        {piecesLabel}
                      </Link>
                    ) : (
                      piecesLabel
                    )}
                  </strong>
                  <span>
                    {setHolding(pieces)} pieces ·{" "}
                    {readersText(piecesLabel, readers, ontologyName)}
                  </span>
                </span>
                <Tag tone={readers.length ? "success" : "neutral"}>
                  {readers.length
                    ? `Read by ${readers.length === 1 ? ontologyName(readers[0]!.ontologyId) : plural(readers.length, "ontology", "ontologies")}`
                    : "Not read"}
                </Tag>
              </li>
            </ul>
          ) : (
            <EmptyState
              size="sm"
              icon={Folder}
              title={textField ? "No documents yet" : "No text field"}
              text={
                textField
                  ? "Its pieces become documents at its next sync."
                  : manage
                    ? "Choose the field that holds each piece’s text, and its newest pieces become documents."
                    : "An owner or admin of the workspace chooses the field that holds each piece’s text."
              }
            />
          )}
        </div>
      </Section>
      <Section
        title="Send pieces"
        id="es-send"
        description={
          OVER_HTTP
            ? "Apps and agents send pieces over HTTP or the Helix MCP, with a send token as their bearer, or over the MCP signed in as an owner or admin. Anyone who can send can replace a piece by its id."
            : "Apps and agents send pieces over the Helix MCP, with a send token as their bearer or signed in as an owner or admin. Anyone who can send can replace a piece by its id."
        }
      >
        <EventSourceSend source={c} />
      </Section>
      {editing && (
        <Modal title={`Edit ${name}`} onClose={() => setEditing(false)}>
          <div className="pad connection-dialog">
            <EventSourceForm
              connector={c}
              onDone={async (_, message) => {
                await qc.invalidateQueries({ queryKey: [id, "connectors"] });
                setEditing(false);
                notify(message);
              }}
            />
          </div>
        </Modal>
      )}
      {tokenDialog && (
        <EventSourceSendDialog connectorId={c.id} onClose={closeTokenDialog} />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          description="Deletes its pieces and their documents, and revokes its tokens."
          confirmLabel="Delete API source"
          tone="danger"
          pending={deleting === "pending"}
          onCancel={() => setDeleting(null)}
          onConfirm={remove}
        />
      )}
    </Page>
  );
}

// "Another field…" among the text field's choices: a backtick, which no
// field's name may hold, so it is never a field.
const OTHER = "`";

/**
 * An API source's text field, for owners and admins: none, a field its
 * pieces have carried as a string or only as null (textFieldChoices), or
 * another typed by name, checked as the WFE checks it (textFieldProblem).
 * Saving asks first, saying what it costs (textFieldConfirm), then sends the
 * text field alone (setEventTextField), so its description stays; the WFE
 * queues the one full sync that re-renders its newest pieces, and refuses
 * the change while a sync of the source is live, whose message shows here.
 * It reads the source's set and its readers itself, so it works wherever a
 * source shows: its page, and the Ontology area's source picker, in place.
 * `onSaved` hears of the source as saved.
 */
export function EventSourceTextField({
  source,
  onSaved,
}: {
  source: Connector;
  onSaved?: (source: Connector) => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    sets = useQuery(documentSetQueries.list(id)),
    ontologyName = useOntologyNames();
  const config = eventsConfig(source),
    held = config.textField || "",
    fields = config.fields || [];
  // "" for none, OTHER while a name is typed, else a field's name.
  const [choice, setChoice] = useState(held),
    [typed, setTyped] = useState(""),
    // The text field the choice started from: once the source has another
    // (saved here or elsewhere), the choice starts again from that one.
    [from, setFrom] = useState(held),
    [confirming, setConfirming] = useState<"confirm" | "pending" | null>(null),
    [error, setError] = useState("");
  if (from !== held) {
    setFrom(held);
    setChoice(held);
    setTyped("");
  }
  const next = choice === OTHER ? typed : choice,
    problem = next ? textFieldProblem(next, fields) : undefined,
    ready = next !== held && !problem && (choice !== OTHER || !!typed),
    set = piecesSet(sets.data, source.id),
    confirm = textFieldConfirm(
      held,
      next,
      set,
      (set?.readBy || []).map((r) => ontologyName(r.ontologyId)),
    );
  const save = async () => {
    setConfirming("pending");
    try {
      const saved = await setEventTextField(id, source.id, next);
      await Promise.all(
        ["connectors", "document-sets"].map((key) =>
          qc.invalidateQueries({ queryKey: [id, key] }),
        ),
      );
      setConfirming(null);
      notify(next ? "Text field saved" : "Text field cleared");
      onSaved?.(saved.connector);
    } catch (e) {
      setConfirming(null);
      setError((e as Error).message);
    }
  };
  return (
    <div className="connect-data">
      <form
        className="form-grid"
        onSubmit={(e) => {
          e.preventDefault();
          if (!ready) return;
          setError("");
          setConfirming("confirm");
        }}
      >
        <label>
          Text field
          <Select
            label="Text field"
            value={choice}
            onChange={(value) => {
              setChoice(value);
              setError("");
            }}
            options={[
              { value: "", label: "None" },
              ...textFieldChoices(config).map((name) => {
                const field = fields.find((f) => f.name === name);
                return {
                  value: name,
                  label: name,
                  hint: !field
                    ? undefined
                    : field.type === "string"
                      ? "string"
                      : "null so far",
                };
              }),
              { value: OTHER, label: "Another field…" },
            ]}
          />
          <small className="muted">
            The top-level field holding each piece’s text. Once chosen, later
            pieces must send it as a string.
          </small>
        </label>
        {choice === OTHER && (
          <label>
            Field name
            <input
              autoFocus
              placeholder="e.g. reply"
              value={typed}
              onChange={(e) => {
                setTyped(e.target.value);
                setError("");
              }}
            />
            <small className="muted">
              {(typed && problem) ||
                "A field its pieces haven’t carried yet, which they must then send as a string."}
            </small>
          </label>
        )}
        <div className="span2">
          <Button type="submit" variant="primary" disabled={!ready}>
            Save text field
          </Button>
        </div>
      </form>
      {error && (
        <p role="alert" className="error-box">
          {error}
        </p>
      )}
      {confirming && (
        <ConfirmDialog
          title={confirm.title}
          description={confirm.text}
          confirmLabel={confirm.action}
          tone={next ? "default" : "danger"}
          pending={confirming === "pending"}
          onCancel={() => setConfirming(null)}
          onConfirm={save}
        />
      )}
    </div>
  );
}
