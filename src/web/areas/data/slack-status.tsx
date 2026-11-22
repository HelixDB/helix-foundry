import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { CloudOff, History, RefreshCw, TriangleAlert } from "lucide-react";
import type {
  Connector,
  ConnectorSync,
  SlackConnectorConfig,
  SlackConnectorSetup,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import { Button, formatDate, plural } from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import {
  installFailed,
  installFailure,
  startSlackInstall,
} from "../workspace/slack-api";
import { updateConnector } from "./data-api";
import { slackStatusLine } from "./model";

// A Slack connection through Helix's Slack app, on its page: what keeps it
// from reading (permissions to add, the app removed from Slack and when
// its data goes, the workspace's Slack changed), Slack holding back the
// team's events, days that may miss messages, and a switch from its token
// that its token can no longer sync, with Switch now. Owners and admins
// get the actions; everyone gets the words.

/** The connection's Slack line, for its header: how it reads now. */
export function SlackStatusLine({ connector: c }: { connector: Connector }) {
  const backfill = !!c.slackStatus?.backfill;
  // Once a page shows a backfill, it says so.
  useEffect(() => {
    if (backfill) trackEvent("slack_backfill_shown");
  }, [backfill]);
  return c.slackStatus ? <span>{slackStatusLine(c.slackStatus)}</span> : null;
}

export function SlackNotices({
  connector: c,
  current,
  setup,
}: {
  connector: Connector;
  // The newest sync, as the page polls it.
  current: ConnectorSync | null | undefined;
  // The Slack kind's setup: whether the caller may add the app again.
  setup?: SlackConnectorSetup | null;
}) {
  const { id, role, notify } = useWorkspace(),
    qc = useQueryClient(),
    manage = canManage(role);
  const [pending, setPending] = useState<"install" | "switch" | null>(null),
    [error, setError] = useState("");
  const s = c.slackStatus;
  if (!s) return null;
  const team = s.teamName || s.teamId || "Slack";
  const reinstall = async () => {
    setPending("install");
    setError("");
    try {
      window.location.assign(
        await startSlackInstall(id, paths.connection(c.id)),
      );
    } catch (e) {
      installFailed(installFailure(e));
      setError(sentence((e as Error).message));
      setPending(null);
    }
  };
  // The flip at once: the days the app hasn't read yet lose their
  // documents until it has.
  const switchNow = async () => {
    setPending("switch");
    setError("");
    const stored: SlackConnectorConfig =
      c.config && "slack" in c.config ? c.config.slack : {};
    try {
      await updateConnector(id, c.id, {
        config: {
          slack: {
            historyDays: stored.historyDays || 0,
            helixApp: {
              noticeVersion: stored.helixApp?.noticeVersion || 0,
              now: true,
            },
          },
        },
      });
      trackEvent("slack_switch_now");
      await qc.invalidateQueries({ queryKey: [id, "connectors"] });
      notify("Switched to Helix Foundry’s Slack app");
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(null);
    }
  };
  const failure = error && (
    <p role="alert" className="small error-text">
      {error}
    </p>
  );
  return (
    <>
      {s.state === "SLACK_FEED_STATE_NEEDS_REINSTALL" && (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title="Helix Foundry needs new Slack permissions to sync channels"
          text={`${team} hasn’t granted what syncing its channels needs${s.missingScopes?.length ? ` (${s.missingScopes.join(", ")})` : ""}. ${manage && setup?.canInstall ? "Add Helix Foundry to Slack again to grant it; it keeps answering in Slack meanwhile." : "An owner or admin adds it to Slack again from Settings › Slack."}`}
          action={
            manage &&
            setup?.canInstall && (
              <Button
                size="sm"
                variant="primary"
                pending={pending === "install"}
                onClick={() => void reinstall()}
              >
                Reinstall
              </Button>
            )
          }
        >
          {failure}
        </Callout>
      )}
      {s.state === "SLACK_FEED_STATE_INSTALL_GONE" && (
        <Callout
          icon={CloudOff}
          tone="danger"
          title={`Helix Foundry was removed from ${team}`}
          text={
            <>
              Its data is frozen as of its last sync
              {s.deletedAt
                ? ` and is deleted on ${formatDate(s.deletedAt, false)}`
                : ""}
              .{" "}
              {manage
                ? "Add it again in "
                : "An owner or admin adds it again in "}
              <Link to={paths.settings("slack")}>Settings › Slack</Link>, or
              switch this connection to a bot token.
            </>
          }
        />
      )}
      {s.state === "SLACK_FEED_STATE_TEAM_CHANGED" && (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title={`This connection reads ${team}, which is no longer this workspace’s Slack`}
          text="Edit the connection to read the Slack workspace connected now, or delete it."
        />
      )}
      {s.switching && current?.status === "CONNECTOR_SYNC_STATUS_FAILED" && (
        <Callout
          icon={RefreshCw}
          tone="danger"
          title="Your token can’t sync while Helix Foundry reads the history"
          text="Switch to Helix Foundry’s Slack app now to keep this connection current. Days it hasn’t read yet lose their documents until it has."
          action={
            manage && (
              <Button
                size="sm"
                variant="primary"
                pending={pending === "switch"}
                onClick={() => void switchNow()}
              >
                Switch now
              </Button>
            )
          }
        >
          {failure}
        </Callout>
      )}
      {s.eventsLimitedAt && (
        <Callout
          icon={TriangleAlert}
          tone="muted"
          title={`Slack held back some of ${team}’s messages in the last day`}
          text="Slack delivers at most 30,000 messages an hour per workspace to an app, so some may be missing. To read everything now, use your own app’s token."
        />
      )}
      {!!s.lossyDays && (
        <Callout
          icon={History}
          tone="muted"
          title={`${plural(s.lossyDays, "day")} may be missing messages`}
          text="Slack sent more than Helix Foundry could read again in time. Each says so in its document."
        />
      )}
    </>
  );
}
