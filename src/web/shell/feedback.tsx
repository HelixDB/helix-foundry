import { useState } from "react";
import { useLocation } from "react-router-dom";
import { MessageSquareText } from "lucide-react";
import { LOCAL } from "../../shared/deployment";
import { trackEvent } from "../analytics";
import {
  FEEDBACK_KINDS,
  MAX_FEEDBACK_BYTES,
  feedbackBytes,
  feedbackPage,
  firstLine,
  sendFeedback,
  sendable,
  type FeedbackKind,
} from "../feedback";
import { Button, Dialog, Segmented, formatNumber } from "../kit";
import { useWorkspace } from "../ui";

// The sidebar's Feedback button, at its foot on every page (onboarding
// too): a dialog that takes what kind it is and what happened, with the
// page it was sent from (../feedback.ts says where it goes).

export function FeedbackButton({ compact = false }: { compact?: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        className={
          compact
            ? "hf-btn hf-btn--ghost hf-btn--md hf-btn--icon hf-feedback-button"
            : "hf-btn hf-btn--ghost hf-btn--sm hf-feedback-button"
        }
        aria-label={compact ? "Send feedback" : undefined}
        title="Send feedback"
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
      >
        <MessageSquareText size={compact ? 17 : 15} aria-hidden />
        {!compact && "Feedback"}
      </button>
      {open && <FeedbackDialog onClose={() => setOpen(false)} />}
    </>
  );
}

function FeedbackDialog({ onClose }: { onClose: () => void }) {
  const { id, notify } = useWorkspace(),
    { pathname } = useLocation();
  const [kind, setKind] = useState<FeedbackKind>("bug"),
    [message, setMessage] = useState(""),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const chosen = FEEDBACK_KINDS.find((k) => k.kind === kind)!,
    bytes = feedbackBytes(message),
    over = bytes > MAX_FEEDBACK_BYTES;
  return (
    <Dialog
      title="Send feedback"
      description={
        LOCAL
          ? "This opens an issue on GitHub with what you write, for you to send."
          : "Tell the Helix team what’s broken, confusing or missing. We read every message."
      }
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (!sendable(message) || pending) return;
          setPending(true);
          setError("");
          try {
            const how = await sendFeedback(
              {
                category: chosen.category,
                message: message.trim(),
                workspaceId: id,
                page: feedbackPage(pathname),
              },
              `${chosen.label}: ${firstLine(message)}`,
            );
            trackEvent("feedback_sent", { kind });
            if (how === "sent")
              notify("Thanks. Your feedback is with the Helix team");
            onClose();
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <Segmented
          label="Kind of feedback"
          value={kind}
          onChange={(v) => setKind(v as FeedbackKind)}
          options={FEEDBACK_KINDS.map((k) => ({
            value: k.kind,
            label: k.label,
          }))}
        />
        <label className="hf-field">
          <span>{chosen.prompt}</span>
          <textarea
            data-autofocus
            rows={6}
            value={message}
            aria-invalid={over || undefined}
            onChange={(e) => setMessage(e.target.value)}
          />
          {over && (
            <small className="hf-form-error">
              Too long: {formatNumber(bytes)} of{" "}
              {formatNumber(MAX_FEEDBACK_BYTES)} bytes.
            </small>
          )}
          {!LOCAL && !over && (
            <small>
              Sent with the page you’re on. Leave out passwords, keys and
              customer data.
            </small>
          )}
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!sendable(message)}
          >
            {LOCAL ? "Open a GitHub issue" : "Send feedback"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
