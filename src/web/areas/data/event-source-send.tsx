import { useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Bot, Copy, Terminal } from "lucide-react";
import type { Connector } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { Button, Dialog, SkeletonText } from "../../kit";
import { useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import { connectorQuery, eventBatchesQuery } from "./data-api";
import {
  eventSourceCurl,
  eventSourcePiecesExample,
  eventSourcePrompt,
} from "./event-source-prompt";
import { EventSourceTokens } from "./event-source-tokens";
import {
  eventSourceState,
  eventSourceWaiting,
  eventsConfig,
  eventsTable,
  isEventSource,
} from "./model";

// How pieces reach an API source: its send tokens, then the prompt to copy
// for whoever sends, and, where the stamp takes pieces over HTTP, the curl
// that POSTs them. The same panel shows wherever a source is set up: the
// new-source form's second step, the source's page, the Ontology area's
// source picker and its ?token=new dialog. The site never sends pieces:
// apps and agents do, through the Helix MCP or over HTTP.

// Set per stamp in next.config.ts.
const MCP_URL =
  process.env.NEXT_PUBLIC_HELIX_MCP_URL || "https://mcp.helix-db.com/mcp";
// Empty on a stamp without the HTTP endpoint (prod, local mode): no HTTP.
const EVENTS_URL = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL || "";

/**
 * The send panel of a live API source: its tokens above the prompt, then
 * "Send over HTTP" where the stamp has the endpoint. `focusCreate` focuses
 * "Create a send token" when a dialog opens on it.
 */
export function EventSourceSend({
  source,
  focusCreate = false,
}: {
  source: Connector;
  focusCreate?: boolean;
}) {
  return (
    <div className="dc-event-send">
      <EventSourceTokens source={source} focusCreate={focusCreate} />
      <EventSourcePrompt source={source} />
      {EVENTS_URL && <EventSourceHttp sourceId={source.id} />}
    </div>
  );
}

// The copy-prompt panel, in AgentSetup's copy pattern
// (../ontology/agent-setup.tsx), over eventSourcePrompt(): it names
// HELIX_EVENT_TOKEN and never holds a token or the source's fields.
function EventSourcePrompt({ source }: { source: Connector }) {
  const { id, name, notify } = useWorkspace();
  const copy = () =>
    navigator.clipboard
      .writeText(
        eventSourcePrompt({
          origin: window.location.origin,
          mcpUrl: MCP_URL,
          eventsUrl: EVENTS_URL,
          workspace: { id, name },
          source: {
            id: source.id,
            name: source.name || "API source",
            description: eventsConfig(source).description,
          },
        }),
      )
      .then(() => notify("Copied the prompt: paste it into your agent."))
      .catch(() => notify("Couldn’t copy the prompt.", true));
  return (
    <Callout
      icon={Bot}
      tone="muted"
      title="Send from your agent"
      text={`Copy a prompt for Claude Code, Codex or another agent with the Helix MCP. It says how to connect, with a send token or your own sign-in, and how to send pieces${EVENTS_URL ? ", over the MCP or HTTP" : ""}. It holds no token.`}
      action={
        <Button size="sm" icon={Copy} onClick={() => void copy()}>
          Copy prompt
        </Button>
      }
    />
  );
}

// "Send over HTTP": eventSourceCurl() with Copy, the very text the prompt
// gives, then the shape of the pieces.json it sends, a file to fill in. The
// curl reads the token from HELIX_EVENT_TOKEN, so neither holds one. Only
// where the stamp has the endpoint (EVENTS_URL).
function EventSourceHttp({ sourceId }: { sourceId: string }) {
  const { notify } = useWorkspace(),
    curl = eventSourceCurl({ eventsUrl: EVENTS_URL, sourceId });
  const copy = () =>
    navigator.clipboard
      .writeText(curl)
      .then(() => notify("Copied the curl command."))
      .catch(() => notify("Couldn’t copy the curl command.", true));
  return (
    <section className="dc-event-http" aria-label="Send over HTTP">
      <Callout
        icon={Terminal}
        tone="muted"
        title="Send over HTTP"
        text={
          <>
            For an app or service without an MCP client. Its token goes in{" "}
            <code>HELIX_EVENT_TOKEN</code> on the server; never call this from a
            browser.
          </>
        }
      />
      <figure className="dc-policy" aria-label="curl command">
        <pre>{curl}</pre>
        <Button
          size="sm"
          variant="ghost"
          icon={Copy}
          aria-label="Copy curl command"
          onClick={() => void copy()}
        >
          Copy
        </Button>
      </figure>
      <p className="muted small">
        It sends <code>pieces.json</code>, a file to fill in with real pieces: a
        field keeps the type of its first send.
      </p>
      <figure className="dc-policy" aria-label="pieces.json">
        <pre>{eventSourcePiecesExample()}</pre>
      </figure>
    </section>
  );
}

/**
 * The send panel in the kit's dialog, for a source named by id alone (a
 * ?token=new link), with "Create a send token" focused. An id that is not a
 * live API source of the workspace says it is gone.
 */
export function EventSourceSendDialog({
  connectorId,
  onClose,
}: {
  connectorId: string;
  onClose: () => void;
}) {
  const { id } = useWorkspace();
  const connector = useQuery(connectorQuery(id, connectorId)),
    c = connector.data?.connector,
    gone =
      (c && !isEventSource(c.kind)) ||
      (connector.error instanceof ApiError && connector.error.status === 404);
  return (
    <Dialog
      title={
        c && !gone ? `Send pieces to ${c.name || "API source"}` : "Send pieces"
      }
      size="lg"
      onClose={onClose}
    >
      {gone ? (
        <p role="alert">This API source is gone.</p>
      ) : c ? (
        <EventSourceSend source={c} focusCreate />
      ) : connector.error ? (
        <p role="alert" className="error-box">
          {connector.error.message}
        </p>
      ) : (
        <SkeletonText lines={3} />
      )}
    </Dialog>
  );
}

/**
 * An API source's newest `limit` sends, polled while it waits for its
 * pieces (eventSourceWaiting), with its state (eventSourceState) once they
 * are read. The source and its sends are read apart, and a sync can start
 * and end between two reads of either, so each one's change says the other
 * is stale: when a sync takes the last pending pieces, the connectors are
 * read again (its table's rows and time follow), and when its table's
 * snapshot changes, its sends are (they say what is synced). Its page and
 * the Ontology area's source picker show sources through it; they also
 * poll their connectors while `waiting`.
 */
export function useEventSends(source: Connector, limit = 20) {
  const { id } = useWorkspace(),
    qc = useQueryClient();
  const sends = useQuery(eventBatchesQuery(id, source, limit)),
    pending = Number(sends.data?.pendingPieces || 0),
    syncedAt = eventsTable(source)?.syncedAt || "",
    seen = useRef({ pending, syncedAt });
  useEffect(() => {
    const before = seen.current;
    seen.current = { pending, syncedAt };
    if (before.pending > 0 && pending === 0)
      for (const key of [
        [id, "connectors"],
        [id, "connectors", source.id],
      ])
        void qc.invalidateQueries({ queryKey: key, exact: true });
    if (before.syncedAt !== syncedAt) void sends.refetch();
  }, [pending, syncedAt]);
  return {
    sends,
    state: sends.data && eventSourceState(source, sends.data),
    waiting: eventSourceWaiting(source, sends.data),
  };
}
