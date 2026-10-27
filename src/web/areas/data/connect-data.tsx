import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Braces, Check, X } from "lucide-react";
import type { Connector, ConnectorKindName } from "../../../shared/wfe";
import { logoUrl } from "../../kit";
import { paths } from "../../paths";
import { useCurrentTheme, type Theme } from "../../shell/theme";
import { Loading, useWorkspace } from "../../ui";
import { ProGate } from "../workspace/pro-gate";
import { ConnectionForm } from "./connection-form";
import { connectorKindsQuery, connectorsQuery } from "./data-api";
import { EventSourceForm } from "./event-source-form";
import { canManage, isEventSource, isKnownKind } from "./model";

// "Add data": helix-foundry's connector picker and per-kind form in its
// modal, offering the kinds the WFE's catalog holds. Kinds the workspace's
// plan does not include open the Workspace area's Pro gate instead of a form.
// A link may open one kind's form (`initialKind`); a kind the catalog doesn't
// offer shows the picker. An API source (events), helix-foundry's Ingestion
// API card, has a form of its own (./event-source-form.tsx), whose send
// panel shows in the modal once the source is made; `onCreated` hears of a
// source as soon as it is made, before Done, since the modal may be closed
// without it. A new connection opens its page, unless the caller takes it
// (`onConnected`) to stay where it is.

const appChoices: [ConnectorKindName, string, string][] = [
  ["stripe", "Stripe", "Customers and payments"],
  ["workos", "WorkOS", "Users and organizations"],
  ["posthog", "PostHog", "Product usage and events"],
  ["github", "GitHub", "Issues and pull requests"],
  ["linear", "Linear", "Issues, projects and cycles"],
  ["calcom", "Cal.com", "Bookings and event types"],
  ["granola", "Granola", "Meeting notes and attendees"],
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
const logo = (kind: ConnectorKindName, theme: Theme) =>
  isEventSource(kind) ? (
    <Braces />
  ) : kind === "mysql" ? (
    <img
      className="connection-wordmark"
      src={logoUrl("mysql-wordmark", theme)}
      alt=""
    />
  ) : (
    <img
      className="connection-icon"
      src={logoUrl(kind === "postgres" ? "postgresql" : kind, theme)}
      alt=""
    />
  );

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
    close = useRef(onClose);
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
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
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

export function ConnectData({
  onClose,
  initialKind,
  onCreated,
  onConnected,
}: {
  onClose: () => void;
  initialKind?: string | null;
  onCreated?: (connector: Connector) => void;
  onConnected?: (connector: Connector) => void;
}) {
  const { id, role, notify } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    theme = useCurrentTheme();
  const kinds = useQuery(connectorKindsQuery(id)),
    connectors = useQuery(connectorsQuery(id));
  const [kind, setKind] = useState<ConnectorKindName | null>(
    initialKind && isKnownKind(initialKind) ? initialKind : null,
  );
  const offered = new Map(
    (kinds.data?.kinds || [])
      .filter((k) => isKnownKind(k.kind))
      .map((k) => [k.kind as ConnectorKindName, k]),
  );
  const count = (k: ConnectorKindName) =>
    (connectors.data?.connectors || []).filter((c) => c.kind === k).length;
  const done = async (connector: Connector, message: string) => {
    await qc.invalidateQueries({ queryKey: [id, "connectors"] });
    onClose();
    notify(message);
    if (onConnected) onConnected(connector);
    else navigate(paths.connection(connector.id));
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
      title={kind ? titles[kind] : "Where is your data?"}
      onClose={onClose}
    >
      <div className="pad connection-dialog">
        {!canManage(role) ? (
          <p>An owner or admin can add data to this workspace.</p>
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
          </>
        ) : (
          <>
            <button
              className="text-button back-link"
              onClick={() => setKind(null)}
            >
              ← All sources
            </button>
            {chosen.available === false ? (
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
                onDone={done}
              />
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
