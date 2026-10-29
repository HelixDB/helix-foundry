import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { Connector } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { useWorkspace } from "../../ui";
import { ProGate, isProRequired } from "../workspace/pro-gate";
import { createConnector, updateConnector } from "./data-api";
import { EventSourceSend } from "./event-source-send";
import { eventsConfig } from "./model";

// A new API source, in the connection form's style (connection-form.tsx):
// its name and what each piece is, then, once made, its send panel in place
// of the form, so a token and the prompt are a click away; Done hands the
// source on. `onCreated` hears of it as soon as it is made, since the form
// may be closed without Done. Editing (`connector` set) changes the name and
// description only: its fields are learned from the pieces sent, never
// written here.

/** The longest description the WFE stores, in characters. */
const MAX_DESCRIPTION = 1000;
// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;

export function EventSourceForm({
  connector,
  onCreated,
  onDone,
}: {
  // Editing this source rather than making one.
  connector?: Connector;
  onCreated?: (connector: Connector) => void;
  onDone: (connector: Connector, message: string) => void | Promise<void>;
}) {
  const { id } = useWorkspace(),
    qc = useQueryClient();
  const [name, setName] = useState(connector?.name || ""),
    [description, setDescription] = useState(
      connector ? eventsConfig(connector).description || "" : "",
    ),
    [pending, setPending] = useState(false),
    [error, setError] = useState<Error | null>(null),
    // The source this form made, whose send panel shows in its place.
    [made, setMade] = useState<Connector>();
  const config = { events: { description: description.trim() } };
  const changed =
    !connector ||
    name.trim() !== connector.name ||
    description.trim() !== (eventsConfig(connector).description || "");
  const submit = async () => {
    setPending(true);
    setError(null);
    try {
      if (connector) {
        const renamed = name.trim() !== connector.name,
          described =
            description.trim() !== (eventsConfig(connector).description || "");
        const saved = await updateConnector(id, connector.id, {
          ...(renamed ? { name: name.trim() } : {}),
          ...(described ? { config } : {}),
        });
        await onDone(saved.connector, "API source saved");
        return;
      }
      const created = await createConnector(id, { name: name.trim(), config });
      trackEvent("connector_created", { kind: "events" });
      onCreated?.(created.connector);
      await qc.invalidateQueries({ queryKey: [id, "connectors"] });
      setMade(created.connector);
    } catch (e) {
      setError(e as Error);
    } finally {
      setPending(false);
    }
  };
  if (made)
    return (
      <div className="connect-data dc-event-made">
        <p role="status">
          <strong>{made.name}</strong> is ready. Make a send token for an app or
          agent that sends on its own, or copy the prompt for the agent you work
          with. Pieces sync about a minute after they arrive.
        </p>
        <EventSourceSend source={made} />
        <button
          className="button primary"
          onClick={() => void onDone(made, "API source created")}
        >
          Done
        </button>
      </div>
    );
  return (
    <div className="connect-data">
      <form
        className="form-grid"
        onSubmit={(e) => {
          e.preventDefault();
          if (!pending && changed && name.trim()) void submit();
        }}
      >
        {!connector && (
          <p className="span2">
            {OVER_HTTP
              ? "Your apps and agents send it pieces of data over HTTP or the Helix MCP: JSON objects, each with an id. Foundry keeps every piece, merges them by id into one table, and learns its fields from what arrives."
              : "Your agents send it pieces of data over the Helix MCP: JSON objects, each with an id. Foundry keeps every piece, merges them by id into one table, and learns its fields from what arrives."}
          </p>
        )}
        <label className="span2">
          Name
          <input
            required
            maxLength={120}
            placeholder="e.g. Order lines"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <label className="span2">
          Description (optional)
          <textarea
            maxLength={MAX_DESCRIPTION}
            placeholder="One line of a customer’s order: what was bought, how many and for how much."
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
          <small className="muted">
            What each piece is: Foundry’s agent reads this.
          </small>
        </label>
        <button
          className="button primary"
          disabled={pending || !changed || !name.trim()}
        >
          {pending
            ? connector
              ? "Saving…"
              : "Creating…"
            : connector
              ? "Save changes"
              : "Create API source"}
        </button>
      </form>
      {error &&
        (isProRequired(error) ? (
          <ProGate feature="API sources" />
        ) : (
          <p role="alert" className="error-box">
            {error.message}
          </p>
        ))}
    </div>
  );
}
