import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link2Off, MessageSquare } from "lucide-react";
import { sentence } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  Select,
  SkeletonText,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  shellWorkspaces,
  whoamiQuery,
  workspacesQuery,
} from "../../workspaces";
import { tabStorage } from "../data/sign-ins";
import {
  authorizeRelayLink,
  relayCancelUrl,
  relayLinkRequest,
} from "./relay-api";
import { clearSlackReturn, readSlackReturn } from "./slack-return";

// /relay/link (hosted): a Foundry install on someone's computer asks to be
// linked to a Helix workspace, so Slack reaches it through Helix's relay.
// Its request (state, PKCE challenge, its Settings › Slack and its name)
// was moved into this tab, and the URL made bare, before the app ran (the
// root layout's SLACK_RETURN_BOOT), so a sign-in on the way keeps it. The
// page names the install and the workspace, among those the caller owns or
// administers, and Link sends the browser back to the install's own
// Settings › Slack on this computer with a one-time code; Cancel sends it
// back saying so. An install whose Settings › Slack isn't on this computer
// (http://localhost or 127.0.0.1) is never sent anything.

export function RelayLinkPage() {
  const { id: current } = useWorkspace();
  useTitle("Link a Foundry to Helix");
  const [kept] = useState(() => readSlackReturn(tabStorage(), "relay_link")),
    request = kept && relayLinkRequest(kept);
  const me = useQuery(whoamiQuery),
    list = useQuery(workspacesQuery);
  const choices =
    me.data && list.data
      ? shellWorkspaces(me.data, list.data.workspaces || []).filter(
          (w) => w.role === "owner" || w.role === "admin",
        )
      : [];
  const [picked, setPicked] = useState(""),
    chosen =
      choices.find((w) => w.id === picked) ||
      choices.find((w) => w.id === current) ||
      choices[0];
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const link = async () => {
    if (!request || !chosen) return;
    setPending(true);
    setError("");
    try {
      const back = await authorizeRelayLink(chosen.id, request);
      clearSlackReturn(tabStorage());
      window.location.assign(back);
    } catch (e) {
      setError(sentence((e as Error).message));
      setPending(false);
    }
  };
  const cancel = () => {
    if (!request) return;
    clearSlackReturn(tabStorage());
    window.location.assign(relayCancelUrl(request));
  };
  const settings = (
    <ButtonLink size="sm" to={paths.settings("slack")}>
      Open Settings › Slack
    </ButtonLink>
  );
  const body = !kept ? (
    <EmptyState
      icon={MessageSquare}
      title="Nothing to link here"
      text="Open Link to Helix again in Settings › Slack of the Foundry on your computer, in this tab."
      action={settings}
    />
  ) : !request ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="This link can’t be used"
        text="The Foundry asking to be linked didn’t name its own Settings › Slack on this computer, or a name Helix keeps. Link to Helix again from its Settings › Slack."
        action={settings}
      />
    </div>
  ) : me.isPending || list.isPending ? (
    <Panel title="Link a Foundry to Helix">
      <SkeletonText lines={2} />
    </Panel>
  ) : !chosen ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title="You can’t link a Foundry here"
        text="Only a workspace’s owners and admins link a Foundry to it. Ask one of them to open this link, or sign in as one."
        action={settings}
      />
    </div>
  ) : (
    <Panel title="Link a Foundry to Helix">
      <div className="wk-pad">
        <p data-replay-block>
          Link the Foundry on <strong>{request.name}</strong> to{" "}
          <strong>{chosen.name}</strong>. Slack messages for that install will
          pass through Helix, and Helix vouches for who clicked in Slack.
        </p>
        {choices.length > 1 && (
          <label className="hf-field">
            <span>Workspace</span>
            <Select
              label="Workspace"
              value={chosen.id}
              disabled={pending}
              onChange={setPicked}
              options={choices.map((w) => ({ value: w.id, label: w.name }))}
            />
          </label>
        )}
        <p className="wk-note">
          Link goes back to that Foundry at{" "}
          <span className="hf-mono">{request.origin}</span> on this computer,
          which finishes linking. Revoke it any time in this workspace’s
          Settings › Slack.
        </p>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button
            variant="primary"
            pending={pending}
            onClick={() => void link()}
          >
            Link
          </Button>
          <Button disabled={pending} onClick={cancel}>
            Cancel
          </Button>
        </div>
      </div>
    </Panel>
  );
  return (
    <Page width="narrow">
      <PageHeader title="Slack" />
      {body}
    </Page>
  );
}
