import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleCheck, Link2Off, MessageSquare } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { sentence } from "../../api";
import { tabStorage } from "../data/sign-ins";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  createSlackLink,
  linkRefusalText,
  previewSlackLink,
  slackKey,
  slackReason,
} from "./slack-api";
import { clearSlackReturn, readSlackReturn } from "./slack-return";

// /slack/link: a person links their Slack account to their Foundry account
// from the one-time link Helix Foundry sent them in Slack
// (/slack/link?workspace=<id>#t=<token>). The token was moved into this tab,
// and the fragment cleared, before any of the site's scripts ran (the root
// layout's SLACK_RETURN_BOOT), so a sign-in on the way keeps it and it never
// reaches a server's logs or the tracker. The page says who would act as
// whom, from a preview that spends nothing, and Link is offered only when
// the Slack account's confirmed email is the caller's. On a local install
// the link makes the Slack account that added the app this Foundry's owner
// in Slack, with no email to compare (Helix keeps Slack's emails).

export function SlackLinkPage() {
  const { id, name, user, notify } = useWorkspace(),
    qc = useQueryClient();
  useTitle("Link your Slack account");
  const [kept] = useState(() => readSlackReturn(tabStorage(), "link")),
    token = kept?.token || "";
  const preview = useQuery({
    queryKey: [id, "foundry-slack-link-preview", token],
    queryFn: () => previewSlackLink(id, token),
    enabled: !!token,
    retry: false,
    staleTime: Infinity,
  });
  const [linking, setLinking] = useState(false),
    [linked, setLinked] = useState(false),
    [error, setError] = useState("");
  const link = async () => {
    setLinking(true);
    setError("");
    try {
      await createSlackLink(id, token);
      clearSlackReturn(tabStorage());
      await qc.invalidateQueries({ queryKey: slackKey(id) });
      setLinked(true);
      notify("Your Slack account is linked");
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setLinking(false);
    }
  };
  const settings = (
    <ButtonLink size="sm" to={paths.settings("slack")}>
      Open Settings › Slack
    </ButtonLink>
  );
  const body = linked ? (
    <EmptyState
      icon={CircleCheck}
      title="Your Slack account is linked"
      text={
        LOCAL
          ? "Helix Foundry now answers you in Slack as this Foundry’s owner. Go back to Slack and ask again."
          : "Helix Foundry now answers you in Slack as you. Go back to Slack and ask again."
      }
      action={settings}
    />
  ) : !token ? (
    <EmptyState
      icon={MessageSquare}
      title="This link has no Slack account to link"
      text="Open the link Helix Foundry sent you in Slack in this tab, or message Helix Foundry in Slack for a new one."
      action={settings}
    />
  ) : preview.isPending ? (
    <Panel title="Link your Slack account">
      <SkeletonText lines={2} />
    </Panel>
  ) : preview.error ? (
    <div role="alert">
      <EmptyState
        icon={Link2Off}
        title={
          slackReason(preview.error) === "FOUNDRY_SLACK_DISABLED"
            ? "Helix Foundry for Slack isn’t available here"
            : "This link can’t be used"
        }
        text={sentence(preview.error.message)}
        action={settings}
      />
    </div>
  ) : (
    (() => {
      const p = preview.data,
        refusal = linkRefusalText(p),
        slackUser = p.slackHandle ? `@${p.slackHandle}` : p.slackName;
      return (
        <Panel title="Link your Slack account">
          <div className="wk-pad">
            <p data-replay-block>
              Slack user <strong>{slackUser || p.slackUserId}</strong>
              {p.slackEmailMasked ? ` (${p.slackEmailMasked})` : ""} in{" "}
              <strong>{p.teamName || "Slack"}</strong> will act as{" "}
              {LOCAL ? (
                "this Foundry’s owner"
              ) : (
                <>
                  you (<strong>{user.email}</strong>)
                </>
              )}{" "}
              in <strong>{name}</strong>.
            </p>
            <p className="wk-note">
              {LOCAL
                ? "Helix Foundry then answers that Slack account as you, and it may approve changes from Slack while Settings › Slack allows it."
                : "Helix Foundry then answers that Slack account as you, with what you can see and do in Foundry."}
            </p>
            {refusal && (
              <p role="alert" className="hf-form-error">
                {refusal}
              </p>
            )}
            {error && (
              <p role="alert" className="hf-form-error">
                {error}
              </p>
            )}
            <div className="hf-form-actions">
              <Button
                variant="primary"
                pending={linking}
                disabled={!p.matchesYou}
                onClick={() => void link()}
              >
                Link
              </Button>
              <ButtonLink
                to={paths.settings("slack")}
                onClick={() => clearSlackReturn(tabStorage())}
              >
                Cancel
              </ButtonLink>
            </div>
          </div>
        </Panel>
      );
    })()
  );
  return (
    <Page width="narrow">
      <PageHeader title="Slack" />
      {body}
    </Page>
  );
}
