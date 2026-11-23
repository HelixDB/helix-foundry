import {
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type DragEvent,
  type ReactNode,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Plus, X } from "lucide-react";
import type { Connector, ConnectorKindName } from "../../../shared/wfe";
import { Button, SourceLogo, connectorLabel, sourceMark } from "../../kit";
import { paths } from "../../paths";
import { useCurrentTheme, type Theme } from "../../shell/theme";
import { Loading, canManage, useWorkspace } from "../../ui";
import { ProGate } from "../workspace/pro-gate";
import { ConnectionForm } from "./connection-form";
import { connectorKindsQuery, connectorsQuery, signInQuery } from "./data-api";
import { EventSourceForm } from "./event-source-form";
import { RequestConnectorForm } from "./request-connector";
import { isEventSource, isKnownKind } from "./model";
import { AgentSignIn, SignInReturn } from "./sign-in";

// "Add data": helix-foundry's connector picker and per-kind form in its
// modal, offering the kinds the WFE's catalog holds. Kinds the workspace's
// plan does not include open the Workspace area's Pro gate instead of a form.
// A link may open one kind's form (`initialKind`); a kind the catalog doesn't
// offer shows the picker. An API source (events), helix-foundry's Ingestion
// API card, has a form of its own (./event-source-form.tsx), whose send
// panel shows in the modal once the source is made; `onCreated` hears of a
// source as soon as it is made, before Done, since the modal may be closed
// without it. A new connection ends on what was made (`Connected`), with Add
// another source and Open, unless the caller takes it (`onConnected`) to
// stay where it is. Under the kinds, Request a connector takes the
// picker's place (./request-connector.tsx), for members too.
//
// A kind may connect by signing in at its provider (the kind's
// `signIn`), a round trip that leaves the page: the callback comes back to
// `returnPath` with `connected=<id>` (else the page's SignInReturn; a
// caller that stays where it is gets its own page by default), else to the
// new connection's page. An agent's
// link names its sign-in (`signIn`, the link's sign_in): its own kind's
// form opens bound to it (./sign-in.tsx AgentSignIn), whatever kind the
// link's add= named.

const appChoices: [ConnectorKindName, string, string][] = [
  ["stripe", "Stripe", "Customers and payments"],
  ["workos", "WorkOS", "Users and organizations"],
  ["posthog", "PostHog", "Product usage and events"],
  ["github", "GitHub", "Issues and pull requests"],
  ["linear", "Linear", "Issues, projects and cycles"],
  ["calcom", "Cal.com", "Bookings and event types"],
  ["granola", "Granola", "Meeting notes and attendees"],
  ["slack", "Slack", "Channels, messages and people"],
  ["notion", "Notion", "Pages and databases"],
  ["google", "Google Workspace", "Drive docs, calendars and people"],
  ["databuddy", "Databuddy", "Web analytics: sessions, pages and visitors"],
  ["attio", "Attio", "CRM: people, companies, deals, lists, notes and tasks"],
];
const databaseChoices: [ConnectorKindName, string, string][] = [
  ["postgres", "PostgreSQL", "Import tables from your database."],
  ["mysql", "MySQL", "Import tables from your database."],
];
const fileChoices: [ConnectorKindName, string, string][] = [
  ["s3", "Amazon S3", "Parquet, JSON and CSV files in a bucket."],
];
// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;
const agentChoices: [ConnectorKindName, string, string][] = [
  [
    "events",
    "API source",
    OVER_HTTP
      ? "Send pieces of data from your app or agent, over HTTP or the Helix MCP. Foundry keeps every piece."
      : "Send pieces of data from your agent, over the Helix MCP. Foundry keeps every piece.",
  ],
];
const titles = Object.fromEntries(
  [...appChoices, ...databaseChoices, ...fileChoices, ...agentChoices].map(
    ([kind, title]) => [kind, title],
  ),
) as Record<ConnectorKindName, string>;
function logo(kind: ConnectorKindName, theme: Theme) {
  const { url, wordmark, Icon } = sourceMark(kind, theme);
  return url ? (
    <img
      className={wordmark ? "connection-wordmark" : "connection-icon"}
      src={url}
      alt=""
    />
  ) : (
    <Icon />
  );
}

// A drag of files over the picker: kept from the page, and refused.
const refuseFiles = (e: DragEvent<HTMLElement>) => {
  e.stopPropagation();
  if (!e.dataTransfer.types.includes("Files")) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = "none";
};

// helix-foundry's Modal (ui.tsx): a native dialog with its header.
export function Modal({
  title,
  children,
  onClose,
  className = "",
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  className?: string;
}) {
  const dialog = useRef<HTMLDialogElement>(null),
    close = useRef(onClose),
    // Where the last press began: a press that starts in the card (a text
    // selection in a field) and ends over the backdrop keeps the dialog.
    pressed = useRef<EventTarget | null>(null);
  close.current = onClose;
  useEffect(() => {
    const el = dialog.current!,
      previous = document.activeElement as HTMLElement | null;
    el.showModal();
    const cancel = (e: Event) => {
      e.preventDefault();
      close.current();
    };
    el.addEventListener("cancel", cancel);
    return () => {
      el.removeEventListener("cancel", cancel);
      el.close();
      previous?.focus();
    };
  }, []);
  return (
    <dialog
      ref={dialog}
      className={"modal " + className}
      aria-label={title}
      onMouseDown={(e) => (pressed.current = e.target)}
      onClick={(e) => {
        if (e.target === e.currentTarget && pressed.current === e.target)
          onClose();
      }}
      // A page that takes dropped files as documents (the Data page) never
      // gets one dropped on the dialog, and the browser doesn't open it: no
      // field here takes a file.
      onDragEnter={(e) => e.stopPropagation()}
      onDragOver={refuseFiles}
      onDragLeave={(e) => e.stopPropagation()}
      onDrop={refuseFiles}
    >
      <div className="modal-head">
        <h2>{title}</h2>
        <button className="icon-btn" onClick={onClose} aria-label="Close">
          <X size={18} />
        </button>
      </div>
      {children}
    </dialog>
  );
}

// helix-foundry's ConnectionCard (choice-card.tsx).
function ConnectionCard({
  title,
  description,
  icon,
  count,
  disabled,
  onClick,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  count: number;
  disabled?: boolean;
  onClick: () => void;
}) {
  const descriptionId = useId();
  return (
    <button
      type="button"
      className={`choice-card connection-card${count ? " added" : ""}`}
      disabled={disabled}
      onClick={onClick}
      aria-label={title}
      aria-haspopup="dialog"
      aria-describedby={descriptionId}
    >
      <span className="choice-card-icon" aria-hidden="true">
        {icon}
      </span>
      <span className="choice-card-heading">
        <span
          className={`connection-indicator${count ? " added" : ""}`}
          aria-hidden="true"
        >
          {count > 1 ? (
            count
          ) : count === 1 ? (
            <Check size={12} strokeWidth={2.5} />
          ) : null}
        </span>
        <strong>{title}</strong>
      </span>
      <span id={descriptionId} className="choice-card-description">
        {description}
        <span className="sr-only">
          {count ? ` · ${count} added` : " · None added"}
        </span>
      </span>
    </button>
  );
}

// What a connect from the picker ends on: the connection made, and where to
// go next, so adding several sources never leaves the picker.
function Connected({
  connector,
  message,
  onAnother,
  onOpen,
}: {
  connector: Connector;
  message: string;
  onAnother: () => void;
  onOpen: () => void;
}) {
  const name = connector.name || connectorLabel(connector.kind);
  return (
    <div className="dc-connected" role="status">
      <SourceLogo source={connector.kind} size={28} />
      <p className="dc-connected-text">
        <strong>{name}</strong>
        <span>{message}.</span>
      </p>
      <div className="dc-connected-actions">
        <Button icon={Plus} onClick={onAnother} autoFocus>
          Add another source
        </Button>
        <Button variant="primary" onClick={onOpen}>
          Open {name}
        </Button>
      </div>
    </div>
  );
}

export function ConnectData({
  onClose,
  initialKind,
  signIn,
  returnPath,
  onCreated,
  onConnected,
}: {
  onClose: () => void;
  initialKind?: string | null;
  // The sign-in an agent's link names, for `initialKind`'s form.
  signIn?: string | null;
  // Where a sign-in's callback returns once connected.
  returnPath?: string;
  onCreated?: (connector: Connector) => void;
  onConnected?: (connector: Connector) => void;
}) {
  const { id, role, notify } = useWorkspace(),
    navigate = useNavigate(),
    location = useLocation(),
    qc = useQueryClient(),
    theme = useCurrentTheme();
  // Bound to the link's sign-in until left for the kind's own form.
  const [bound, setBound] = useState(signIn || null);
  const page = useContext(SignInReturn),
    back =
      returnPath ??
      page?.path ??
      (onConnected ? location.pathname + location.search : undefined);
  const kinds = useQuery(connectorKindsQuery(id)),
    connectors = useQuery(connectorsQuery(id));
  const [kind, setKind] = useState<ConnectorKindName | null>(
    initialKind && isKnownKind(initialKind) ? initialKind : null,
  );
  // A link's sign-in opens its own kind's form, whatever kind the link
  // named, and shows under no other.
  const boundRead = useQuery({
      ...signInQuery(id, bound || ""),
      enabled: !!bound,
    }),
    boundKind = boundRead.data?.signIn.kind;
  useEffect(() => {
    if (bound && boundKind && isKnownKind(boundKind) && boundKind !== kind)
      setKind(boundKind);
  }, [bound, boundKind]);
  const offered = new Map(
    (kinds.data?.kinds || [])
      .filter((k) => isKnownKind(k.kind))
      .map((k) => [k.kind as ConnectorKindName, k]),
  );
  const count = (k: ConnectorKindName) =>
    (connectors.data?.connectors || []).filter((c) => c.kind === k).length;
  // What was just connected, while the dialog says so: Add another source
  // goes back to the picker, Open to its page.
  // Request a connector, in the picker's place.
  const [requesting, setRequesting] = useState(false);
  const requestLine = (
    <>
      <hr className="connection-divider" />
      <p className="dc-request-line">
        Don’t see your tool or database?{" "}
        <button
          type="button"
          className="text-button"
          onClick={() => setRequesting(true)}
        >
          Request a connector
        </button>
      </p>
    </>
  );
  const [connected, setConnected] = useState<{
    connector: Connector;
    message: string;
  } | null>(null);
  const done = (connector: Connector, message: string) => {
    void qc.invalidateQueries({ queryKey: [id, "connectors"] });
    if (!onConnected) return setConnected({ connector, message });
    onClose();
    notify(message);
    onConnected(connector);
  };
  const cards = (choices: typeof appChoices) =>
    choices
      .filter(([value]) => offered.has(value))
      .map(([value, title, description]) => (
        <ConnectionCard
          key={value}
          title={title}
          description={
            offered.get(value)!.available === false
              ? "Needs Foundry Pro"
              : description
          }
          count={count(value)}
          onClick={() => setKind(value)}
          icon={logo(value, theme)}
        />
      ));
  const chosen = kind ? offered.get(kind) : undefined;
  return (
    <Modal
      title={
        connected
          ? "Connected"
          : requesting
            ? "Request a connector"
            : kind && chosen
              ? titles[kind]
              : "Where is your data?"
      }
      onClose={onClose}
    >
      <div className="pad connection-dialog">
        {connected ? (
          <Connected
            connector={connected.connector}
            message={connected.message}
            onAnother={() => {
              setConnected(null);
              setBound(null);
              setKind(null);
              // The picker's first kind takes the focus the panel had.
              requestAnimationFrame(() =>
                document
                  .querySelector<HTMLElement>("dialog[open] .connection-card")
                  ?.focus(),
              );
            }}
            // The connection's page takes the picker's entry: Back from it
            // is where the picker was opened.
            onOpen={() =>
              navigate(paths.connection(connected.connector.id), {
                replace: true,
              })
            }
          />
        ) : requesting ? (
          <RequestConnectorForm
            onDone={() => setRequesting(false)}
            onCancel={() => setRequesting(false)}
          />
        ) : !canManage(role) ? (
          <>
            <p>An owner or admin can add data to this workspace.</p>
            {requestLine}
          </>
        ) : kinds.isPending ? (
          <Loading />
        ) : kinds.error ? (
          <p role="alert" className="error-box">
            {kinds.error.message}
          </p>
        ) : !kind || !chosen ? (
          <>
            <div
              className="connection-options choice-options"
              role="group"
              aria-label="Choose an app"
            >
              {cards(appChoices)}
            </div>
            <hr className="connection-divider" />
            <p className="setup-intro" id="database-sources-label">
              Or connect a database
            </p>
            <div
              className="connection-options choice-options"
              role="group"
              aria-labelledby="database-sources-label"
            >
              {cards(databaseChoices)}
            </div>
            <hr className="connection-divider" />
            <p className="setup-intro" id="file-sources-label">
              Or read files
            </p>
            <div
              className="connection-options choice-options"
              role="group"
              aria-labelledby="file-sources-label"
            >
              {cards(fileChoices)}
            </div>
            {agentChoices.some(([value]) => offered.has(value)) && (
              <>
                <hr className="connection-divider" />
                <p className="setup-intro" id="agent-sources-label">
                  {OVER_HTTP ? "From your app or agent" : "From your agent"}
                </p>
                <div
                  className="connection-options choice-options"
                  role="group"
                  aria-labelledby="agent-sources-label"
                >
                  {cards(agentChoices)}
                </div>
              </>
            )}
            {requestLine}
          </>
        ) : (
          <>
            <button
              className="text-button back-link"
              onClick={() => {
                setBound(null);
                setKind(null);
              }}
            >
              ← All sources
            </button>
            {bound &&
            !boundRead.error &&
            (!boundKind || (isKnownKind(boundKind) && boundKind !== kind)) ? (
              // Its kind isn't known yet, or the form moves to it.
              <Loading />
            ) : bound ? (
              <AgentSignIn
                signInId={bound}
                returnPath={back}
                onUnbind={() => setBound(null)}
              />
            ) : chosen.available === false ? (
              <ProGate
                feature={
                  isEventSource(kind)
                    ? "API sources"
                    : `${titles[kind]} connections`
                }
              />
            ) : isEventSource(kind) ? (
              <EventSourceForm onCreated={onCreated} onDone={done} />
            ) : (
              <ConnectionForm
                key={kind}
                kind={kind}
                setup={chosen.s3}
                slack={chosen.slack}
                // Where Helix's Slack app reads Slack, it is how Slack
                // connects: Pipes' sign-in isn't offered beside it.
                signIn={!!chosen.signIn && !chosen.slack}
                installUrl={chosen.github?.installUrl}
                returnPath={back}
                onDone={done}
              />
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
