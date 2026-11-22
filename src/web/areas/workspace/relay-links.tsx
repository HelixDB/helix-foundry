import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { FoundryRelayLink } from "../../../shared/wfe";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  Panel,
  SkeletonText,
  formatDate,
  formatNumber,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { relayLinksKey, relayLinksQuery, revokeRelayLink } from "./relay-api";

// Settings › Slack's linked Foundry installs (hosted, owners and admins):
// Foundry installs on people's computers that reach Slack through this
// workspace, each with who linked it, when Helix last heard from it, the
// Slack workspaces it serves and what it did today, and Revoke. Nothing
// shows where Helix's relay is off.

/** The workspace's linked installs, with Revoke. */
export function RelayLinks() {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const links = useQuery(relayLinksQuery(id));
  const [revoking, setRevoking] = useState<FoundryRelayLink | null>(null),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  if (links.data === null) return null;
  const revoke = async () => {
    if (!revoking?.id) return;
    setPending(true);
    setError("");
    try {
      await revokeRelayLink(id, revoking.id);
      await qc.invalidateQueries({ queryKey: relayLinksKey(id) });
      notify(`${revoking.name || "The install"} is revoked`);
      setRevoking(null);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  return (
    <Panel
      title="Linked Foundry installs"
      subtitle="Foundry installs on people’s computers that reach Slack through this workspace. Helix holds Slack’s messages for them until they take them."
      padded={false}
    >
      {links.isPending ? (
        <div className="wk-pad">
          <SkeletonText lines={2} />
        </div>
      ) : links.error ? (
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read the linked installs. {links.error.message}
          </p>
          <Button size="sm" onClick={() => void links.refetch()}>
            Retry
          </Button>
        </div>
      ) : links.data.length ? (
        <ul
          className="wk-members"
          aria-label="Linked Foundry installs"
          data-replay-block
        >
          {links.data.map((l) => {
            const name = l.name || "A Foundry install",
              teams = (l.installs || [])
                .map((i) => i.teamName || i.teamId)
                .filter(Boolean);
            return (
              <li key={l.id} className="wk-member">
                <div className="wk-member-text">
                  <strong>{name}</strong>
                  <small>
                    Linked by {l.createdByName || "a member"}
                    {l.createdAt ? ` on ${formatDate(l.createdAt, false)}` : ""}
                    {teams.length
                      ? `. Answers in ${teams.join(", ")}.`
                      : ". Not in Slack yet."}
                  </small>
                  <small>
                    {l.lastSeenAt
                      ? `Last heard from ${formatDate(l.lastSeenAt)}`
                      : "Not heard from yet"}
                    {`. Today: ${formatNumber(Number(l.deliveriesToday || 0))} messages held, ${formatNumber(Number(l.callsToday || 0))} Slack calls.`}
                  </small>
                </div>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Revoke ${name}`}
                  onClick={() => setRevoking(l)}
                >
                  Revoke
                </Button>
              </li>
            );
          })}
        </ul>
      ) : (
        <EmptyState
          size="sm"
          title="No Foundry install is linked"
          text="The owner of a Foundry on their computer links it from its Settings › Slack, choosing this workspace on Helix."
        />
      )}
      {revoking && (
        <ConfirmDialog
          title={`Revoke ${revoking.name || "this install"}?`}
          description="Its link stops working, and Helix Foundry leaves the Slack workspaces it serves through it. Its owner can link it again."
          confirmLabel="Revoke"
          tone="danger"
          pending={pending}
          onConfirm={revoke}
          onCancel={() => {
            setRevoking(null);
            setError("");
          }}
        >
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </ConfirmDialog>
      )}
    </Panel>
  );
}
