import { useState } from "react";
import { useLocation } from "react-router-dom";
import { LOCAL } from "../../../shared/deployment";
import { trackEvent } from "../../analytics";
import {
  MAX_FEEDBACK_BYTES,
  connectorRequestMessage,
  feedbackBytes,
  feedbackPage,
  sendFeedback,
} from "../../feedback";
import { Button, Dialog } from "../../kit";
import { useWorkspace } from "../../ui";

// Request a connector: the tool or database someone wants Foundry to read,
// and what they'd want from it, sent as feedback of its own kind
// (src/web/feedback.ts). In the picker (./connect-data.tsx) it takes the
// picker's place; on the Connectors page (./overview.tsx) it is a dialog.
// Anyone in the workspace may ask.

const MAX_NAME = 100;

export function RequestConnectorForm({
  onDone,
  onCancel,
}: {
  // Sent (or, in a local build, opened on GitHub).
  onDone: () => void;
  onCancel: () => void;
}) {
  const { id, notify } = useWorkspace(),
    { pathname } = useLocation();
  const [name, setName] = useState(""),
    [details, setDetails] = useState(""),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const message = connectorRequestMessage(name, details),
    over = feedbackBytes(message) > MAX_FEEDBACK_BYTES,
    ready = !!name.trim() && !over;
  return (
    <form
      className="dc-request"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!ready || pending) return;
        setPending(true);
        setError("");
        try {
          const how = await sendFeedback(
            {
              category: "FEEDBACK_CATEGORY_CONNECTOR_REQUEST",
              message,
              workspaceId: id,
              page: feedbackPage(pathname),
            },
            `Connector request: ${name.trim()}`,
          );
          trackEvent("connector_requested");
          if (how === "sent")
            notify(`Thanks. We’ve passed on your request for ${name.trim()}`);
          onDone();
        } catch (err) {
          setError((err as Error).message);
        } finally {
          setPending(false);
        }
      }}
    >
      <p className="dc-request-intro">
        Tell us which tool or database you want Foundry to read.
        {LOCAL
          ? " This opens an issue on GitHub for you to send."
          : " The requests we get decide which connectors we build next."}
      </p>
      <label className="hf-field">
        <span>Tool or database</span>
        <input
          data-autofocus
          autoFocus
          required
          maxLength={MAX_NAME}
          placeholder="HubSpot, Salesforce, Snowflake…"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </label>
      <label className="hf-field">
        <span>What would you want from it? (optional)</span>
        <textarea
          rows={4}
          placeholder="The objects or tables you need, and what you’d ask of them."
          value={details}
          aria-invalid={over || undefined}
          onChange={(e) => setDetails(e.target.value)}
        />
        {over && <small className="hf-form-error">That’s too long.</small>}
      </label>
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      <div className="hf-form-actions">
        <Button onClick={onCancel}>Cancel</Button>
        <Button
          type="submit"
          variant="primary"
          pending={pending}
          disabled={!ready}
        >
          {LOCAL ? "Open a GitHub issue" : "Send request"}
        </Button>
      </div>
    </form>
  );
}

export function RequestConnectorDialog({ onClose }: { onClose: () => void }) {
  return (
    <Dialog title="Request a connector" size="sm" onClose={onClose}>
      <RequestConnectorForm onDone={onClose} onCancel={onClose} />
    </Dialog>
  );
}
