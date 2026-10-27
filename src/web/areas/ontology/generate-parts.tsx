import "../data/data.css";
import "../documents/documents.css";
import { useEffect, useState, type Dispatch, type SetStateAction } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Braces,
  Cable,
  ChevronDown,
  Copy,
  FileText,
  Folder,
  KeyRound,
  Plus,
  RotateCcw,
  Send,
  Upload,
} from "lucide-react";
import type {
  Connector,
  FoundryDocumentSet,
  FoundryEventSourceMode,
} from "../../../shared/wfe";
import {
  Button,
  EmptyState,
  IconButton,
  Panel,
  Segmented,
  SkeletonText,
  SourceLogo,
  StatusBadge,
  connectorLabel,
  cx,
  formatNumber,
  formatRelative,
  plural,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { ConnectData } from "../data/connect-data";
import {
  connectorKindsQuery,
  connectorsQuery,
  syncConnector,
  useSetsAfterSyncs,
} from "../data/data-api";
import { EventSourceTextField } from "../data/event-source-page";
import { EventSourceSend, useEventSends } from "../data/event-source-send";
import {
  EVENT_SET_DEFAULTS,
  KINDS,
  canManage,
  eventSourceLine,
  eventsConfig,
  eventsTable,
  isEventSource,
  isLiveSync,
  kindFolders,
  picked as pickedOf,
  piecesSet,
  setFolders,
  setHolding,
  setSpend,
  syncBadge,
} from "../data/model";
import { useDataUpload } from "../data/upload";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { fileReadLabel, formatBytes, setLabel } from "../documents/model";
import { goalPrompt, promptDocuments, promptEventSource } from "./goal-prompt";
import {
  DOCUMENTS_PER_SET,
  MAX_GENERATION_SETS,
  MAX_GOAL,
  eventSourceReadiness,
  isEventSourceSet,
  pickerSets,
  setRoom,
  setSourcesText,
} from "./model";

// What a generation is asked for, shared by Generate (./generate.tsx), the
// new-ontology wizard (./new-ontology.tsx) and an unfinished ontology's
// overview (./setup.tsx): its goal, and what it drafts from, in one step
// (SourcesPicker), any kind: the connectors whose synced tables it reads
// (by kind, as the Data page shows them: a kind with several connectors
// nests them under one choice), the API sources whose pieces it reads, and
// the document sets it reads, each listing its documents, then each
// connection's own sets under the connection, with how many documents each
// holds. GenerateOntology takes connectors, not single tables: every synced
// table of a chosen connector goes in; an API source is read through
// mappings as one table, `events`, or by entity extraction as its own set
// of pieces (one of the sets, never listed among them); of a set, every
// table file Foundry has read is a table of its own.
//
// Neither page sends the user to Data. Add data opens the Data area's
// picker over the page; the new connection's first sync shows in its row
// (the connectors query polls while a sync is live) and it counts as chosen
// once synced. New API source makes one over the page, with its send panel;
// its row moves on as pieces arrive (it polls while it waits for them, as
// the Data area does) and it counts as chosen once it has some. Upload
// documents goes into a set, or the default one (General, made by a
// workspace's first upload), and a set uploaded into is chosen while there
// is room.

// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;
const synced = (c: Connector) => (c.tables || []).length > 0;
// An API source's pieces in its table: it can be chosen once it has some.
const pieces = (c: Connector) => Number(eventsTable(c)?.rowCount || 0);
const lastSynced = (c: Connector) =>
  (c.tables || [])
    .map((t) => t.syncedAt || "")
    .sort()
    .at(-1);
const toggle = (set: ReadonlySet<string>, key: string) => {
  const next = new Set(set);
  if (!next.delete(key)) next.add(key);
  return next;
};

/**
 * "What should it serve?": the goal, counted in characters as the WFE does,
 * and a prompt to copy for the user's own AI agent (./goal-prompt.ts) that
 * describes the ontology's chosen connectors, API sources and sets
 * (`chosen` as ConnectorPicker's, `chosenEvents` as EventSourcePicker's,
 * `chosenSets` as SetPicker's) and has it write the goal.
 */
export function GoalPanel({
  goal,
  setGoal,
  ontology,
  chosen,
  chosenEvents,
  chosenSets,
}: {
  goal: string;
  setGoal: (goal: string) => void;
  // Its name; "" while it has none.
  ontology: string;
  chosen: ReadonlySet<string> | null;
  chosenEvents: ReadonlyMap<string, FoundryEventSourceMode> | null;
  chosenSets: ReadonlySet<string> | null;
}) {
  const { id, name, notify } = useWorkspace();
  const connectors = useQuery(connectorsQuery(id)),
    sets = useQuery(documentSetQueries.list(id)),
    documents = useQuery(documentQueries.workspace(id));
  const chars = [...goal.trim()].length,
    list = connectors.data?.connectors || [];
  const copyPrompt = () =>
    navigator.clipboard
      .writeText(
        goalPrompt({
          workspace: name,
          ontology,
          goal,
          connectors: list
            .filter((c) => !isEventSource(c.kind))
            .map((c) => ({
              name: c.name || connectorLabel(c.kind || ""),
              kind: connectorLabel(c.kind || ""),
              tables: c.tables || [],
              chosen: !chosen || chosen.has(c.id),
            })),
          eventSources: list
            .filter((c) => isEventSource(c.kind))
            .map((c) => ({
              ...promptEventSource(c, chosenEvents?.get(c.id)),
              chosen: !!chosenEvents?.has(c.id),
            })),
          // An API source's own set is its line's, read by extraction.
          sets: setFolders(
            (sets.data || []).filter((s) => !isEventSourceSet(s)),
            documents.data || [],
          ).map((f) => ({
            name: setLabel(f.set),
            description: f.set.description,
            // A connector's set holds text only, none of it in the uploads.
            ...(f.set.connector
              ? { documents: f.set.documents || 0, names: [], files: [] }
              : promptDocuments(f.documents)),
            chosen: !!chosenSets?.has(f.set.id),
          })),
        }),
      )
      .then(() =>
        notify(
          "Copied the prompt: paste it into your agent, and its goal here.",
        ),
      )
      .catch(() => notify("Couldn’t copy the prompt.", true));
  return (
    <Panel
      title="What should it serve?"
      subtitle="The questions people will ask of it, in your words. The agent models what they need. Not sure? Copy a prompt for your own AI agent: it describes what you chose and writes this with you."
      actions={
        <Button
          size="sm"
          icon={Copy}
          disabled={!connectors.data}
          onClick={copyPrompt}
        >
          Copy prompt for your agent
        </Button>
      }
    >
      <label className="hf-field on-goal">
        <span className="hf-sr-only">Goal</span>
        <textarea
          rows={4}
          placeholder="e.g. Trace each order from the customer through its shipments and returns"
          value={goal}
          onChange={(e) => setGoal(e.target.value)}
          aria-describedby="on-goal-hint"
        />
        <small id="on-goal-hint">
          {chars > MAX_GOAL
            ? `${formatNumber(chars - MAX_GOAL)} characters too long.`
            : `Optional. ${formatNumber(MAX_GOAL - chars)} characters left.`}
        </small>
      </label>
    </Panel>
  );
}

/**
 * The connectors to draft from. `chosen` holds their ids, null for every
 * synced one; one connected here joins it, and counts once synced. API
 * sources are EventSourcePicker's: they are left out here, and of every
 * synced one.
 */
export function ConnectorPicker({
  chosen,
  setChosen,
}: {
  chosen: Set<string> | null;
  setChosen: Dispatch<SetStateAction<Set<string> | null>>;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const connectors = useQuery(connectorsQuery(id)),
    kinds = useQuery(connectorKindsQuery(id));
  // Connectors whose tables show, and kinds whose connectors show.
  const [open, setOpen] = useState<Set<string>>(new Set()),
    [openKinds, setOpenKinds] = useState<Set<string>>(new Set());
  const [connecting, setConnecting] = useState(false),
    [syncing, setSyncing] = useState("");
  const list = (connectors.data?.connectors || []).filter(
    (c) => !isEventSource(c.kind),
  );
  const usable = list.filter(synced);
  const selected = chosen ?? new Set(usable.map((c) => c.id));
  const picked = usable.filter((c) => selected.has(c.id));
  const sync = async (c: Connector) => {
    setSyncing(c.id);
    try {
      await syncConnector(id, c.id);
      await qc.invalidateQueries({ queryKey: [id, "connectors"] });
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setSyncing("");
    }
  };
  const addData = (variant: "primary" | "secondary") => (
    <Button
      size="sm"
      variant={variant}
      icon={Plus}
      onClick={() => setConnecting(true)}
    >
      Add data
    </Button>
  );
  // One connector: its choice and, once opened, its synced tables. One not
  // synced yet says how its sync goes, and starts one when none is live.
  const connectorRow = (c: Connector) => {
    const ok = synced(c),
      live = isLiveSync(c.lastSync),
      rows = Number(c.lastSync?.rowsSynced || 0),
      badge = syncBadge(c.lastSync),
      isOpen = open.has(c.id),
      at = lastSynced(c),
      label = connectorLabel(c.kind || "");
    return (
      <li key={c.id} className={cx("on-connector", !ok && "is-off")}>
        <div className="on-connector-row">
          <label className="on-check">
            <input
              type="checkbox"
              checked={ok && selected.has(c.id)}
              disabled={!ok}
              onChange={() => setChosen(toggle(selected, c.id))}
            />
            <SourceLogo source={c.kind || ""} size={28} />
            <span className="on-connector-text">
              <strong>{c.name || label}</strong>
              <small>
                {label} ·{" "}
                {ok
                  ? `${plural(c.tables!.length, "table")}${at ? `, synced ${formatRelative(at)}` : ""}`
                  : live
                    ? rows
                      ? `first sync, ${formatNumber(rows)} rows so far`
                      : "first sync under way"
                    : c.lastSync?.status === "CONNECTOR_SYNC_STATUS_FAILED"
                      ? c.lastSync.error || "its sync failed"
                      : "not synced yet"}
              </small>
            </span>
          </label>
          {!ok && (
            <StatusBadge status={badge.status} label={badge.label} size="sm" />
          )}
          {!ok && !live && (
            <Button
              size="sm"
              icon={RotateCcw}
              pending={syncing === c.id}
              onClick={() => void sync(c)}
            >
              {c.lastSync ? "Sync again" : "Sync"}
            </Button>
          )}
          {ok && (
            <button
              type="button"
              className="on-disclose"
              aria-expanded={isOpen}
              aria-label={`${isOpen ? "Hide" : "Show"} ${c.name}’s tables`}
              onClick={() => setOpen(toggle(open, c.id))}
            >
              <ChevronDown size={16} aria-hidden />
            </button>
          )}
        </div>
        {isOpen && (
          <ul className="on-tables" aria-label={`${c.name || c.id} tables`}>
            {c.tables!.map((t) => (
              <li key={t.name}>
                <span className="hf-mono">{t.name}</span>
                {(t.primaryKey?.length || 0) > 0 && (
                  <span className="on-table-key" title="Primary key">
                    <KeyRound size={11} aria-hidden />
                    {t.primaryKey!.join(", ")}
                  </span>
                )}
                <span className="on-table-rows">
                  {formatNumber(t.rowCount)} rows
                </span>
              </li>
            ))}
          </ul>
        )}
      </li>
    );
  };
  return (
    <Panel
      title="Tables"
      subtitle="Choose connectors: every table they have synced goes in."
      padded={false}
      actions={
        list.length > 0 && (
          <span className="on-select-actions">
            {usable.length > 1 && (
              <>
                <button
                  type="button"
                  className="on-link"
                  disabled={picked.length === usable.length}
                  onClick={() => setChosen(null)}
                >
                  Select all
                </button>
                <button
                  type="button"
                  className="on-link"
                  disabled={!picked.length}
                  onClick={() => setChosen(new Set())}
                >
                  Clear
                </button>
              </>
            )}
            {addData("secondary")}
          </span>
        )
      }
    >
      {connectors.isPending ? (
        <div className="on-pad">
          <SkeletonText lines={3} />
        </div>
      ) : !connectors.data ? (
        <p className="on-pad hf-form-error" role="alert">
          {connectors.error?.message}
        </p>
      ) : !list.length ? (
        <EmptyState
          size="sm"
          icon={Cable}
          title="No connectors yet"
          text="Connect a database or an app. Its first sync starts at once, and it is chosen here when it ends."
          action={addData("primary")}
        />
      ) : (
        <ul className="on-connectors" aria-label="Connectors">
          {kindFolders(
            list,
            // Until the kinds load, the order they come in.
            kinds.data?.kinds?.map((k) => k.kind) || KINDS,
          ).map(({ kind, items }) => {
            const group = items.map((i) => i.connector);
            if (group.length === 1) return connectorRow(group[0]!);
            const label = connectorLabel(kind),
              ids = group.filter(synced).map((c) => c.id),
              state = pickedOf(ids, selected),
              chosenHere = ids.filter((c) => selected.has(c)).length,
              isOpen = openKinds.has(kind);
            return (
              <li
                key={kind}
                className={cx("on-connector", !ids.length && "is-off")}
              >
                <div className="on-connector-row">
                  <label className="on-check">
                    <input
                      type="checkbox"
                      checked={state === "all"}
                      ref={(el) => {
                        if (el) el.indeterminate = state === "some";
                      }}
                      disabled={!ids.length}
                      onChange={() =>
                        setChosen(
                          state === "all"
                            ? new Set(
                                [...selected].filter((c) => !ids.includes(c)),
                              )
                            : new Set([...selected, ...ids]),
                        )
                      }
                    />
                    <SourceLogo source={kind} size={28} />
                    <span className="on-connector-text">
                      <strong>{label}</strong>
                      <small>
                        {plural(group.length, "connector")} ·{" "}
                        {ids.length
                          ? `${formatNumber(chosenHere)} chosen`
                          : "not synced yet"}
                      </small>
                    </span>
                  </label>
                  <button
                    type="button"
                    className="on-disclose"
                    aria-expanded={isOpen}
                    aria-label={`${isOpen ? "Hide" : "Show"} ${label} connectors`}
                    onClick={() => setOpenKinds(toggle(openKinds, kind))}
                  >
                    <ChevronDown size={16} aria-hidden />
                  </button>
                </div>
                {isOpen && (
                  <ul
                    className="on-connectors on-kind-connectors"
                    aria-label={`${label} connectors`}
                  >
                    {group.map(connectorRow)}
                  </ul>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {connecting && (
        <ConnectData
          onClose={() => setConnecting(false)}
          // An API source made here shows in EventSourcePicker, unchosen.
          onConnected={(c) =>
            !isEventSource(c.kind) &&
            setChosen((prev) => prev && new Set([...prev, c.id]))
          }
        />
      )}
    </Panel>
  );
}

/**
 * Connect your data or upload documents: the connectors, the API sources
 * and the document sets a generation drafts from, each picked as
 * ConnectorPicker, EventSourcePicker and SetPicker pick them.
 */
export function SourcesPicker({
  chosen,
  setChosen,
  chosenEvents,
  setChosenEvents,
  chosenSets,
  setChosenSets,
  failed,
}: {
  chosen: Set<string> | null;
  setChosen: Dispatch<SetStateAction<Set<string> | null>>;
  chosenEvents: EventChoices | null;
  setChosenEvents: Dispatch<SetStateAction<EventChoices | null>>;
  chosenSets: Set<string> | null;
  setChosenSets: Dispatch<SetStateAction<Set<string> | null>>;
  failed?: { error: Error; retry: () => void } | null;
}) {
  return (
    <div className="on-stack">
      <ConnectorPicker chosen={chosen} setChosen={setChosen} />
      <EventSourcePicker chosen={chosenEvents} setChosen={setChosenEvents} />
      <SetPicker
        chosen={chosenSets}
        setChosen={setChosenSets}
        chosenEvents={chosenEvents}
        failed={failed}
      />
    </div>
  );
}

/** The API sources chosen, by id, each with how the ontology reads it. */
export type EventChoices = Map<string, FoundryEventSourceMode>;

const STRUCTURED: FoundryEventSourceMode =
    "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
  EXTRACTION: FoundryEventSourceMode = "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";
// The mode an API source's choice starts at: extraction once it has a text
// field, else its table through mappings.
const preferredMode = (c: Connector) =>
  eventsConfig(c).textField ? EXTRACTION : STRUCTURED;

/**
 * The API sources to draft from, each read through mappings as its one
 * table or by entity extraction as its own set of pieces, as its row's
 * choice says: extraction when the source has a text field, else mappings,
 * until changed. `chosen` is null until the sources to start with are
 * known, and nothing can be chosen until then. A source can be chosen once
 * the WFE would take it in its mode (eventSourceReadiness): its table has
 * pieces, or it has a text field and a piece as a document; one chosen
 * without can still be unchosen, and its mode changed. Owners and admins
 * choose a source's text field in place (the Data area's control). Each
 * row says where its pieces are and polls while it waits for them, as the
 * Data area does, so it turns choosable without a reload (SourcesPicker's
 * SetPicker reads the sets again as syncs end); Send pieces opens its send
 * panel. New API source makes one over the page (the Data area's form at
 * `events`), and one made here is chosen once it has pieces. Shown once the
 * WFE offers API sources, or the workspace has one.
 */
export function EventSourcePicker({
  chosen,
  setChosen,
}: {
  chosen: EventChoices | null;
  setChosen: Dispatch<SetStateAction<EventChoices | null>>;
}) {
  const { id } = useWorkspace();
  const connectors = useQuery(connectorsQuery(id)),
    kinds = useQuery(connectorKindsQuery(id)),
    sets = useQuery(documentSetQueries.list(id));
  const [creating, setCreating] = useState(false),
    // Sources made here that have no pieces yet: chosen once they have.
    [made, setMade] = useState<ReadonlySet<string>>(new Set()),
    // The modes chosen on rows, kept while a row is unchosen.
    [modes, setModes] = useState<ReadonlyMap<string, FoundryEventSourceMode>>(
      new Map(),
    );
  const list = (connectors.data?.connectors || []).filter((c) =>
      isEventSource(c.kind),
    ),
    ready = chosen !== null,
    selected = chosen ?? new Map<string, FoundryEventSourceMode>(),
    modeOf = (c: Connector) =>
      selected.get(c.id) ?? modes.get(c.id) ?? preferredMode(c);
  useEffect(() => {
    const arrived = list.filter((c) => made.has(c.id) && pieces(c) > 0);
    if (!ready || !arrived.length) return;
    setMade(
      (prev) =>
        new Set([...prev].filter((m) => !arrived.some((c) => c.id === m))),
    );
    setChosen(
      (prev) =>
        prev &&
        new Map([
          ...arrived.map((c): [string, FoundryEventSourceMode] => [
            c.id,
            modes.get(c.id) ?? preferredMode(c),
          ]),
          ...prev,
        ]),
    );
  }, [connectors.data, made, ready]);
  if (!list.length && !kinds.data?.kinds?.some((k) => isEventSource(k.kind)))
    return null;
  const toggle = (c: Connector) => {
    const mode = modeOf(c);
    setModes((prev) => new Map(prev).set(c.id, mode));
    setChosen((prev) => {
      if (!prev) return prev;
      const next = new Map(prev);
      if (!next.delete(c.id)) next.set(c.id, mode);
      return next;
    });
  };
  const choose = (c: Connector, mode: FoundryEventSourceMode) => {
    setModes((prev) => new Map(prev).set(c.id, mode));
    setChosen((prev) =>
      prev?.has(c.id) ? new Map(prev).set(c.id, mode) : prev,
    );
  };
  const create = (variant: "primary" | "secondary") => (
    <Button
      size="sm"
      variant={variant}
      icon={Plus}
      onClick={() => setCreating(true)}
    >
      New API source
    </Button>
  );
  return (
    <Panel
      title="API sources"
      subtitle="Choose sources your agents send pieces of data to, and how each is read: its fields through mappings, or its text by entity extraction."
      padded={false}
      actions={list.length > 0 && create("secondary")}
    >
      {connectors.isPending ? (
        <div className="on-pad">
          <SkeletonText lines={2} />
        </div>
      ) : !connectors.data ? (
        <p className="on-pad hf-form-error" role="alert">
          {connectors.error?.message}
        </p>
      ) : !list.length ? (
        <EmptyState
          size="sm"
          icon={Braces}
          title="No API sources yet"
          text={
            OVER_HTTP
              ? "Your apps and agents send an API source pieces of data over HTTP or the Helix MCP: JSON objects, each with an id. It can be chosen here once its first pieces have synced."
              : "Your agents send an API source pieces of data over the Helix MCP: JSON objects, each with an id. It can be chosen here once its first pieces have synced."
          }
          action={create("primary")}
        />
      ) : (
        <ul className="on-connectors" aria-label="API sources">
          {list.map((c) => (
            <EventSourceRow
              key={c.id}
              source={c}
              sets={sets.data}
              mode={modeOf(c)}
              checked={selected.has(c.id)}
              ready={ready}
              onToggle={() => toggle(c)}
              onMode={(mode) => choose(c, mode)}
            />
          ))}
        </ul>
      )}
      {creating && (
        <ConnectData
          initialKind="events"
          onClose={() => setCreating(false)}
          // Recorded once made: the modal may be closed without Done.
          onCreated={(c) => setMade((prev) => new Set([...prev, c.id]))}
          // Stay on the page.
          onConnected={() => {}}
        />
      )}
    </Panel>
  );
}

// One API source: its choice, where its pieces are (eventSourceLine), how
// it is read (`mode`, a Segmented choice saying what each costs and keeps)
// and, read by extraction, its text field, chosen in place by owners and
// admins, with its pieces as documents and this month's spend; and its send
// panel behind Send pieces. While it waits for pieces it polls its sends
// (useEventSends) and the connectors.
function EventSourceRow({
  source: c,
  sets,
  mode,
  checked,
  ready,
  onToggle,
  onMode,
}: {
  source: Connector;
  sets: readonly FoundryDocumentSet[] | undefined;
  mode: FoundryEventSourceMode;
  checked: boolean;
  ready: boolean;
  onToggle: () => void;
  onMode: (mode: FoundryEventSourceMode) => void;
}) {
  const { id, role } = useWorkspace();
  const { sends, state, waiting } = useEventSends(c);
  // Another reader of the connectors query, polling while this one waits.
  useQuery(connectorsQuery(id, waiting));
  const [open, setOpen] = useState(false),
    [choosing, setChoosing] = useState(false);
  const readiness = eventSourceReadiness(c, mode, sets),
    own = piecesSet(sets, c.id),
    spend = setSpend(own?.connector),
    { textField } = eventsConfig(c),
    label = connectorLabel(c.kind),
    name = c.name || label;
  // The set's own numbers, else foundry.events' defaults.
  const kept = own?.connector?.maxDocuments || EVENT_SET_DEFAULTS.maxDocuments,
    usd =
      Number(
        own?.connector?.maxMonthSystemUsdMicros ||
          EVENT_SET_DEFAULTS.maxMonthUsdMicros,
      ) / 1e6;
  return (
    <li className={cx("on-connector", readiness !== "ready" && "is-off")}>
      <div className="on-connector-row">
        <label className="on-check">
          <input
            type="checkbox"
            checked={checked}
            // Until the starting sources load, and while the WFE wouldn't
            // take it in its mode (one chosen so can still be unchosen).
            disabled={!ready || (readiness !== "ready" && !checked)}
            onChange={onToggle}
          />
          <SourceLogo source={c.kind} size={28} />
          <span className="on-connector-text">
            <strong>{name}</strong>
            <small>
              {label} ·{" "}
              {state
                ? eventSourceLine(state)
                : sends.isPending
                  ? "Reading its sends…"
                  : "Couldn’t read its sends"}
            </small>
          </span>
        </label>
        <Button
          size="sm"
          variant="ghost"
          icon={Send}
          aria-expanded={open}
          aria-label={`Send pieces to ${name}`}
          onClick={() => setOpen(!open)}
        >
          Send pieces
        </Button>
      </div>
      <div className="on-event-more">
        <div>
          <Segmented
            label={`How the ontology reads ${name}`}
            value={mode}
            onChange={(v) => onMode(v as FoundryEventSourceMode)}
            options={[
              { value: STRUCTURED, label: "Structured mappings" },
              { value: EXTRACTION, label: "Entity extraction" },
            ]}
          />
        </div>
        <p className="on-muted">
          {mode === STRUCTURED
            ? "Fields map to types and properties. Every piece is kept, and arrives when the ontology refreshes."
            : `Foundry’s agent reads each piece’s text as it arrives. The graph keeps the newest ${formatNumber(kept)}. About 1 cent a piece for each ontology that reads it, up to ${Number.isInteger(usd) ? `$${usd}` : `$${usd.toFixed(2)}`} a month for this source; past that, new pieces wait for next month.`}
        </p>
        {mode === EXTRACTION &&
          (readiness === "text-field" ? (
            canManage(role) ? (
              <p className="on-muted">
                It has no text field yet: the field that holds each piece’s
                text.{" "}
                <Button
                  size="sm"
                  variant="ghost"
                  aria-expanded={choosing}
                  onClick={() => setChoosing(!choosing)}
                >
                  Choose its text field
                </Button>
              </p>
            ) : (
              <p className="on-muted">
                It has no text field yet. An owner or admin of the workspace
                chooses the field that holds each piece’s text.
              </p>
            )
          ) : (
            <p className="on-muted">
              Text field <span className="hf-mono">{textField}</span> ·{" "}
              {[
                own?.documents
                  ? `${setHolding(own)} pieces as documents`
                  : spend?.level !== "paused" &&
                    "its pieces become documents at its next sync",
                spend?.level === "paused"
                  ? `${spend.text}: new pieces wait until ${spend.until}`
                  : spend?.level === "warn" && spend.text,
              ]
                .filter(Boolean)
                .join(" · ")}
            </p>
          ))}
        {choosing && readiness === "text-field" && (
          <EventSourceTextField source={c} onSaved={() => setChoosing(false)} />
        )}
        {open && <EventSourceSend source={c} />}
      </div>
    </li>
  );
}

/**
 * The document sets to read, each with what it gives a generation
 * (setSourcesText): the sets people made, then each connection's own (a
 * Granola connection's Notes), grouped under it with how many documents
 * each holds, all text, and one choice for all of them. Reading a
 * connection's set into an ontology draws on the month's budget like any
 * set, but it may hold a thousand documents. An API source's own set is
 * never listed (pickerSets): EventSourcePicker reads it, as the source by
 * extraction, which takes one of the MAX_GENERATION_SETS, so at most
 * setRoom(chosenEvents) can be chosen here. `chosen` is null until the sets
 * to start with are known, and nothing can be chosen until then; `failed`
 * says why they couldn't be read, with how to read them again.
 */
export function SetPicker({
  chosen,
  setChosen,
  chosenEvents,
  failed,
}: {
  chosen: Set<string> | null;
  setChosen: Dispatch<SetStateAction<Set<string> | null>>;
  chosenEvents: EventChoices | null;
  failed?: { error: Error; retry: () => void } | null;
}) {
  const { id } = useWorkspace();
  const sets = useQuery(documentSetQueries.list(id)),
    documents = useQuery(documentQueries.workspace(id)),
    connectors = useQuery(connectorsQuery(id));
  // A connection added here writes its sets as its first sync ends.
  useSetsAfterSyncs(id, connectors.data?.connectors);
  // What the sets chosen here may number: what extraction leaves.
  const cap = Math.max(0, setRoom(chosenEvents, connectors.data?.connectors)),
    extracted = cap < MAX_GENERATION_SETS;
  const files = useDataUpload((setId) =>
    setChosen((prev) =>
      prev && !prev.has(setId) && prev.size < cap
        ? new Set([...prev, setId])
        : prev,
    ),
  );
  // Sets whose documents show.
  const [open, setOpen] = useState<Set<string>>(new Set());
  const ready = chosen !== null,
    selected = chosen ?? new Set<string>();
  // Every set shown: never an API source's own, which EventSourcePicker's
  // extraction reads.
  const {
    made,
    owned,
    ordered: setList,
  } = pickerSets(
    sets.data || [],
    connectors.data?.connectors || [],
    connectorLabel,
  );
  const picked = setList.filter((x) => selected.has(x.id)),
    full = picked.length >= cap;
  // A set's choice: none until the starting sets load, nor past the cap.
  const check = (x: FoundryDocumentSet) => (
    <input
      type="checkbox"
      checked={selected.has(x.id)}
      disabled={!ready || (full && !selected.has(x.id))}
      aria-describedby={full ? "on-sets-cap" : undefined}
      onChange={() => setChosen(toggle(selected, x.id))}
    />
  );
  const error = (!sets.data && sets.error) || (!ready && failed?.error);
  const upload = (
    <Button
      size="sm"
      icon={Upload}
      disabled={!files.ready}
      onClick={() => files.choose()}
    >
      Upload documents
    </Button>
  );
  return (
    <Panel
      title="Documents"
      subtitle={`Choose folders from Connectors: the new version reads their documents, and only theirs. Each table file (CSV, TSV, JSON, JSONL or Parquet) Foundry has read is a table of its own. Of the rest, the agent reads up to ${DOCUMENTS_PER_SET} of the newest in each folder and drafts routes for them, which say what its documents may fill.`}
      padded={false}
      actions={
        setList.length > 0 && (
          <span className="on-select-actions">
            {setList.length > 1 && (
              <>
                <button
                  type="button"
                  className="on-link"
                  disabled={
                    !ready || picked.length === Math.min(setList.length, cap)
                  }
                  onClick={() =>
                    setChosen(new Set(setList.slice(0, cap).map((x) => x.id)))
                  }
                >
                  Select all
                </button>
                <button
                  type="button"
                  className="on-link"
                  disabled={!ready || !picked.length}
                  onClick={() => setChosen(new Set())}
                >
                  Clear
                </button>
              </>
            )}
            {upload}
          </span>
        )
      }
    >
      {error ? (
        <p className="on-pad hf-form-error" role="alert">
          Couldn’t load the document sets: {error.message}{" "}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              if (!sets.data) void sets.refetch();
              failed?.retry();
            }}
          >
            Retry
          </Button>
        </p>
      ) : !sets.data ? (
        <div className="on-pad">
          <SkeletonText lines={2} />
        </div>
      ) : !setList.length ? (
        <EmptyState
          size="sm"
          icon={FileText}
          title="No documents yet"
          text="Upload contracts and notes (Markdown, MDX, text or PDF), or tables (CSV, TSV, JSON, JSONL or Parquet). They go in a new set, General, chosen here."
          action={upload}
        />
      ) : (
        <ul className="on-connectors" aria-label="Document sets">
          {setFolders(made, documents.data || []).map((f) => {
            const x = f.set,
              held = f.documents,
              count = x.documents ?? held.length,
              isOpen = open.has(x.id);
            return (
              <li key={x.id} className="on-connector">
                <div className="on-connector-row">
                  <label className="on-check">
                    {check(x)}
                    <Folder size={20} className="on-set-icon" aria-hidden />
                    <span className="on-connector-text">
                      <strong>{x.name}</strong>
                      <small>
                        {count ? setSourcesText(held) : "No documents yet"}
                        {x.description ? ` · ${x.description}` : ""}
                      </small>
                    </span>
                  </label>
                  <IconButton
                    label={`Upload into ${x.name}`}
                    icon={Upload}
                    size="sm"
                    disabled={!files.ready}
                    onClick={() => files.choose(x.id)}
                  />
                  {held.length > 0 && (
                    <button
                      type="button"
                      className="on-disclose"
                      aria-expanded={isOpen}
                      aria-label={`${isOpen ? "Hide" : "Show"} ${x.name}’s documents`}
                      onClick={() => setOpen(toggle(open, x.id))}
                    >
                      <ChevronDown size={16} aria-hidden />
                    </button>
                  )}
                </div>
                {isOpen && (
                  <ul className="on-tables" aria-label={`${x.name} documents`}>
                    {held.map((d) => (
                      <li key={d.id}>
                        <span>{d.name}</span>
                        <span className="on-table-rows">
                          {[fileReadLabel(d), formatBytes(d.sizeBytes)]
                            .filter(Boolean)
                            .join(" · ")}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </li>
            );
          })}
          {owned.map(({ connectorId, name, kind, sets: own }) => {
            const ids = own.map((x) => x.id),
              state = pickedOf(ids, selected),
              room = cap - picked.length;
            return (
              <li key={connectorId} className="on-connector">
                <div className="on-connector-row">
                  <label className="on-check">
                    <input
                      type="checkbox"
                      checked={state === "all"}
                      ref={(el) => {
                        if (el) el.indeterminate = state === "some";
                      }}
                      disabled={!ready || (state === "none" && room <= 0)}
                      aria-describedby={full ? "on-sets-cap" : undefined}
                      onChange={() =>
                        setChosen(
                          state === "all"
                            ? new Set(
                                [...selected].filter((x) => !ids.includes(x)),
                              )
                            : new Set([
                                ...selected,
                                ...ids
                                  .filter((x) => !selected.has(x))
                                  .slice(0, room),
                              ]),
                        )
                      }
                    />
                    <SourceLogo source={kind} size={28} />
                    <span className="on-connector-text">
                      <strong>{name}</strong>
                      <small>
                        {[
                          kind && connectorLabel(kind),
                          "its syncs write these sets; extraction draws on the month’s budget",
                        ]
                          .filter(Boolean)
                          .join(" · ")}
                      </small>
                    </span>
                  </label>
                </div>
                <ul
                  className="on-connectors on-kind-connectors"
                  aria-label={`${name} sets`}
                >
                  {own.map((x) => {
                    const count = x.documents || 0,
                      max = x.connector?.maxDocuments || 0;
                    return (
                      <li key={x.id} className="on-connector">
                        <div className="on-connector-row">
                          <label className="on-check">
                            {check(x)}
                            <Folder
                              size={20}
                              className="on-set-icon"
                              aria-hidden
                            />
                            <span className="on-connector-text">
                              <strong>{x.name || x.id}</strong>
                              <small>
                                {[
                                  count
                                    ? `${plural(count, "document")}${count > DOCUMENTS_PER_SET ? `; the agent reads the ${DOCUMENTS_PER_SET} newest` : ""}`
                                    : "No documents yet",
                                  max && count >= max
                                    ? `keeps the newest ${formatNumber(max)}`
                                    : "",
                                  x.description,
                                ]
                                  .filter(Boolean)
                                  .join(" · ")}
                              </small>
                            </span>
                          </label>
                        </div>
                      </li>
                    );
                  })}
                </ul>
              </li>
            );
          })}
        </ul>
      )}
      {ready && full && (
        <p id="on-sets-cap" className="on-pad on-muted" role="status">
          A generation reads at most {MAX_GENERATION_SETS} document sets
          {extracted ? ", each API source read by extraction among them" : ""}
          {setList.length > cap ? ": uncheck one to choose another." : "."}
        </p>
      )}
      {files.chooser}
      {files.list && <div className="on-pad">{files.list}</div>}
    </Panel>
  );
}
