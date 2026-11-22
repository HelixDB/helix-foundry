import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { LoaderCircle, MessageSquare, TriangleAlert } from "lucide-react";
import { sentence } from "../../api";
import { tabStorage } from "../data/sign-ins";
import { ButtonLink, EmptyState, Page, PageHeader, useTitle } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  finishSlackInstall,
  installFailed,
  slackKey,
  slackReturnCopy,
  slackReturnFailure,
} from "./slack-api";
import { clearSlackReturn, readSlackReturn } from "./slack-return";

// /slack/installed: where Slack returns once an owner or admin allowed
// Helix Foundry (?code=&state=), or cancelled (?error=). What it carried was
// moved into this tab, and the URL made bare, before the app made any call
// (the root layout's SLACK_RETURN_BOOT), so a sign-in on the way keeps it. Here it
// is finished once (a ref keeps <StrictMode>'s second effect from spending
// the state again), then the page goes where the install was started from
// (Settings › Slack, the Data area's Slack form or a Slack connection's
// page: the WFE's returnPath, read by slackReturnTo) in the workspace the
// state was for. A refusal is said in place, in the WFE's words: another
// Foundry workspace has the Slack workspace, Helix is still disconnecting
// it, an org-wide install, a permission left out, an expired state. Slack's
// own ?error= is said in fixed words (slackReturnCopy), never echoed.

type Phase =
  | { kind: "finishing" }
  | { kind: "failed"; title: string; text: string }
  // This tab holds nothing to finish: opened again, or in another tab.
  | { kind: "lost" };

export function SlackInstalledPage() {
  const navigate = useNavigate(),
    qc = useQueryClient(),
    { notify } = useWorkspace();
  const [kept] = useState(() =>
      readSlackReturn(tabStorage(), ["installed", "install_error"]),
    ),
    [phase, setPhase] = useState<Phase>(
      !kept
        ? { kind: "lost" }
        : kept.kind === "installed"
          ? { kind: "finishing" }
          : { kind: "failed", ...slackReturnCopy(kept.error) },
    ),
    started = useRef(false);
  useTitle("Adding Helix Foundry to Slack");
  useEffect(() => {
    if (!kept || started.current) return;
    started.current = true;
    // Spent either way: a reload says there is nothing to finish.
    clearSlackReturn(tabStorage());
    if (kept.kind === "install_error") {
      installFailed(slackReturnFailure(kept.error));
      return;
    }
    void finishSlackInstall(kept.code, kept.state).then(
      async ({ slack, returnPath }) => {
        const ws = slack.workspaceId;
        // The connector kinds carry the install too (the Slack form's setup).
        if (ws)
          await Promise.all(
            [slackKey(ws), [ws, "connector-kinds"], [ws, "connectors"]].map(
              (queryKey) => qc.invalidateQueries({ queryKey }),
            ),
          );
        const team = slack.install?.teamName || "Slack";
        notify(
          returnPath === paths.settings("slack")
            ? `Helix Foundry is in ${team}. Message it there to link your Slack account.`
            : `Helix Foundry is in ${team}.`,
        );
        navigate(
          returnPath +
            (ws
              ? (returnPath.includes("?") ? "&" : "?") +
                "workspace=" +
                encodeURIComponent(ws)
              : ""),
          { replace: true },
        );
      },
      (e: Error) =>
        setPhase({
          kind: "failed",
          title: "Helix Foundry wasn’t added to Slack",
          text: sentence(e.message),
        }),
    );
  }, []);
  const back = (
    <ButtonLink size="sm" variant="primary" to={paths.settings("slack")}>
      Open Settings › Slack
    </ButtonLink>
  );
  return (
    <Page width="narrow">
      <PageHeader title="Slack" />
      {phase.kind === "finishing" ? (
        <EmptyState
          icon={LoaderCircle}
          title="Adding Helix Foundry to Slack…"
          text="Foundry is saving the install. This takes a few seconds."
        />
      ) : phase.kind === "failed" ? (
        <div role="alert">
          <EmptyState
            icon={TriangleAlert}
            title={phase.title}
            text={phase.text}
            action={back}
          />
        </div>
      ) : (
        <EmptyState
          icon={MessageSquare}
          title="Nothing to finish here"
          text="This tab didn’t add Helix Foundry to Slack, or it was finished already. Add it from Settings › Slack."
          action={back}
        />
      )}
    </Page>
  );
}
