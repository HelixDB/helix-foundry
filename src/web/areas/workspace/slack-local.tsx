import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { FoundrySlackLink } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  KeyValue,
  Panel,
  SkeletonText,
  StatusBadge,
  Tag,
  formatDate,
  formatRelative,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { tabStorage } from "../data/sign-ins";
import {
  completeRelayLink,
  relayContact,
  relayKey,
  relayQuery,
  setSlackAccess,
  setSlackApprovals,
  startRelayLink,
  unlinkRelay,
  type RelayView,
} from "./relay-api";
import { slackKey, slackLinksQuery } from "./slack-api";
import { clearSlackReturn, readSlackReturn } from "./slack-return";

// A local install's Settings › Slack, beside the app's own panels
// (./slack.tsx): Helix's Slack app reaches this Foundry only through
// Helix's relay. Its owner links it to a Helix workspace (Link to Helix:
// Helix's /relay/link, which hands a code back to this page, kept and the
// URL cleaned before the app ran, slack-return.ts), sees when Helix last
// answered and unlinks it; once Helix Foundry is in Slack, chooses whether
// their own Slack account approves changes through Slack, and which Slack
// users who asked may read and ask as them.

/** The install's link to Helix: Link to Helix, or who it is linked as, and Unlink. */
export function RelayPanel() {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const relay = useQuery(relayQuery(id));
  const [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [confirm, setConfirm] = useState(false),
    finished = useRef(false);
  const now = useNow(30_000);
  // Helix's answer, kept from the URL before the app ran: finished once
  // (a ref keeps <StrictMode>'s second effect from spending it again).
  useEffect(() => {
    if (finished.current) return;
    finished.current = true;
    const kept = readSlackReturn(tabStorage(), ["relay_return", "relay_error"]);
    if (!kept) return;
    clearSlackReturn(tabStorage());
    if (kept.kind === "relay_error") {
      trackEvent("relay_link_failed", { reason: "cancelled" });
      setError("Linking to Helix was cancelled.");
      return;
    }
    setPending(true);
    void completeRelayLink(id, kept.code, kept.state)
      .then(
        (view) => {
          qc.setQueryData(relayKey(id), view);
          void qc.invalidateQueries({ queryKey: slackKey(id) });
          notify(
            view.kind === "linked" && view.accountEmail
              ? `This Foundry is linked to Helix as ${view.accountEmail}`
              : "This Foundry is linked to Helix",
          );
        },
        (e: Error) => setError(sentence(e.message)),
      )
      .finally(() => setPending(false));
  }, []);
  const link = async () => {
    setPending(true);
    setError("");
    try {
      window.location.assign(await startRelayLink(id));
    } catch (e) {
      setError(sentence((e as Error).message));
      setPending(false);
    }
  };
  const unlink = async () => {
    setPending(true);
    setError("");
    try {
      qc.setQueryData(relayKey(id), await unlinkRelay(id));
      await qc.invalidateQueries({ queryKey: slackKey(id) });
      setConfirm(false);
      notify("This Foundry is unlinked from Helix");
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  const view: RelayView | undefined = relay.data;
  const failed = error && !confirm && (
    <p role="alert" className="hf-form-error">
      {error}
    </p>
  );
  if (relay.isPending)
    return (
      <Panel title="Helix">
        <SkeletonText lines={2} />
      </Panel>
    );
  if (!view)
    return (
      <Panel title="Helix">
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read this Foundry’s link to Helix. {relay.error?.message}
          </p>
          <Button size="sm" onClick={() => void relay.refetch()}>
            Retry
          </Button>
        </div>
      </Panel>
    );
  if (view.kind === "unlinked")
    return (
      <Panel
        title="Helix"
        subtitle="Slack reaches this Foundry through Helix. Nothing leaves this computer until you link it."
      >
        <div className="wk-pad">
          <p className="wk-note">
            Link this Foundry to a Helix workspace you own or administer.
            Slack’s messages for it then pass through Helix, which holds them
            until this Foundry takes them (at most 10 minutes), and Helix
            Foundry’s replies, built from your data here, go to Slack through
            Helix.
          </p>
          <Button
            variant="primary"
            pending={pending}
            onClick={() => void link()}
          >
            Link to Helix
          </Button>
          {failed}
        </div>
      </Panel>
    );
  const contact = relayContact(view.lastOkAt, now);
  return (
    <Panel
      title="Helix"
      subtitle="Slack reaches this Foundry through Helix."
      actions={
        <Button size="sm" variant="danger" onClick={() => setConfirm(true)}>
          Unlink
        </Button>
      }
    >
      <KeyValue
        columns={2}
        items={[
          {
            label: "Helix account",
            value: view.accountEmail && (
              <span data-replay-block>{view.accountEmail}</span>
            ),
          },
          {
            label: "Linked",
            value: view.linkedAt && formatDate(view.linkedAt, false),
          },
          {
            label: "Last contact",
            value:
              contact.kind === "online" ? (
                <StatusBadge status="healthy" label="In contact" size="sm" />
              ) : contact.kind === "never" ? (
                <StatusBadge
                  status="queued"
                  label="Waiting for Helix"
                  size="sm"
                />
              ) : (
                <>
                  <StatusBadge status="stale" label="Not answering" size="sm" />{" "}
                  {formatRelative(contact.since, now)}
                </>
              ),
          },
        ]}
      />
      {(contact.kind === "offline" || failed) && (
        <div className="wk-pad">
          {contact.kind === "offline" && (
            <p className="wk-note">
              Helix hasn’t answered this Foundry since{" "}
              {formatDate(contact.since)}. Slack messages wait at Helix for 10
              minutes; check this computer can reach the internet.
            </p>
          )}
          {failed}
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title="Unlink this Foundry from Helix?"
          description="Helix revokes its link, and Helix Foundry leaves the Slack workspaces it was added to through it. Slack’s links and requests here are deleted. Link it again to use Slack."
          confirmLabel="Unlink"
          tone="danger"
          pending={pending}
          onConfirm={unlink}
          onCancel={() => {
            setConfirm(false);
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

/**
 * Approve changes from Slack: on, the owner's Slack account runs the
 * changes Helix Foundry prepares once they approve them in Slack; off,
 * Slack only reads and asks, for the owner too.
 */
export function ApproveFromSlack({
  relay,
}: {
  relay: Extract<RelayView, { kind: "linked" }>;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const flip = async (on: boolean) => {
    setPending(true);
    setError("");
    try {
      qc.setQueryData(relayKey(id), await setSlackApprovals(id, on));
      notify(
        on
          ? "Your Slack account approves changes again"
          : "Helix Foundry only reads and asks in Slack now",
      );
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  return (
    <Panel title="Changes from Slack">
      <div className="wk-pad">
        <label className="checkbox">
          <input
            type="checkbox"
            role="switch"
            checked={relay.approvals}
            disabled={pending}
            onChange={(e) => void flip(e.target.checked)}
          />
          Approve changes from Slack
        </label>
        <p className="wk-note">
          {relay.approvals
            ? "Your Slack account runs the changes Helix Foundry prepares when you approve them in Slack. Helix’s relay vouches for who clicked in Slack: turn this off to make Slack read and ask only."
            : "Helix Foundry only reads and asks in Slack, for you too. Make changes here in Foundry, or turn this on to approve them from Slack."}
        </p>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
    </Panel>
  );
}

/**
 * Who may use this Foundry in Slack: its owner's account, the Slack users
 * the owner allowed, each with Remove, and those who asked, each with Allow
 * and Dismiss.
 */
export function AllowedPeople() {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const people = useQuery(slackLinksQuery(id));
  const [busy, setBusy] = useState(""),
    [removing, setRemoving] = useState<FoundrySlackLink | null>(null),
    [error, setError] = useState("");
  const change = async (
    slackUserId: string,
    access: "read" | "none",
    done: string,
  ) => {
    setBusy(slackUserId);
    setError("");
    try {
      await setSlackAccess(id, slackUserId, access);
      await qc.invalidateQueries({ queryKey: slackKey(id) });
      setRemoving(null);
      notify(done);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setBusy("");
    }
  };
  const links = people.data?.links || [],
    requests = people.data?.requests || [];
  return (
    <Panel
      title="People in Slack"
      subtitle="Who may use this Foundry in Slack. People you allow read and ask as you; only you make changes."
      padded={false}
    >
      {people.isPending ? (
        <div className="wk-pad">
          <SkeletonText lines={2} />
        </div>
      ) : people.error ? (
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read who may use it. {people.error.message}
          </p>
          <Button size="sm" onClick={() => void people.refetch()}>
            Retry
          </Button>
        </div>
      ) : links.length || requests.length ? (
        <ul
          className="wk-members"
          aria-label="People in Slack"
          data-replay-block
        >
          {links.map((l) => {
            const owner = l.access !== "FOUNDRY_SLACK_LINK_ACCESS_READ";
            return (
              <li key={l.slackUserId} className="wk-member">
                <div className="wk-member-text">
                  <strong>
                    Slack user <span className="hf-mono">{l.slackUserId}</span>
                    {owner ? <Tag>You</Tag> : <Tag>Reads and asks</Tag>}
                  </strong>
                  <small>
                    {owner
                      ? "Your own Slack account, which added Helix Foundry."
                      : "Reads and asks as you, changing nothing."}
                  </small>
                </div>
                {l.createdAt && (
                  <small className="wk-member-joined">
                    {owner ? "Linked" : "Allowed"}{" "}
                    {formatDate(l.createdAt, false)}
                  </small>
                )}
                {!owner && (
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={`Remove ${l.slackUserId}`}
                    onClick={() => setRemoving(l)}
                  >
                    Remove
                  </Button>
                )}
              </li>
            );
          })}
          {requests.map((r) => {
            const who = r.slackName || r.slackUserId || "Someone";
            return (
              <li key={r.slackUserId} className="wk-member">
                <div className="wk-member-text">
                  <strong>
                    {who}
                    <Tag>Asked</Tag>
                  </strong>
                  <small>
                    Slack user <span className="hf-mono">{r.slackUserId}</span>{" "}
                    asked to use this Foundry
                    {r.createdAt ? ` on ${formatDate(r.createdAt, false)}` : ""}
                    .
                  </small>
                </div>
                <Button
                  size="sm"
                  variant="primary"
                  aria-label={`Allow ${who}`}
                  pending={busy === r.slackUserId}
                  disabled={!!busy}
                  onClick={() =>
                    void change(
                      r.slackUserId!,
                      "read",
                      `${who} may read and ask in Slack`,
                    )
                  }
                >
                  Allow
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Dismiss ${who}`}
                  disabled={!!busy}
                  onClick={() =>
                    void change(
                      r.slackUserId!,
                      "none",
                      `${who}’s request is dismissed`,
                    )
                  }
                >
                  Dismiss
                </Button>
              </li>
            );
          })}
        </ul>
      ) : (
        <EmptyState
          size="sm"
          title="No one yet"
          text="Message @Helix Foundry in Slack from the account that added it to link it as you. Anyone else who messages it asks you, here."
        />
      )}
      {error && !removing && (
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        </div>
      )}
      {removing?.slackUserId && (
        <ConfirmDialog
          title={`Remove ${removing.slackUserId}?`}
          description="Helix Foundry stops answering them in Slack. They can ask again by messaging it."
          confirmLabel="Remove"
          tone="danger"
          pending={busy === removing.slackUserId}
          onConfirm={() =>
            change(
              removing.slackUserId!,
              "none",
              `${removing.slackUserId} can’t use this Foundry in Slack now`,
            )
          }
          onCancel={() => {
            setRemoving(null);
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
