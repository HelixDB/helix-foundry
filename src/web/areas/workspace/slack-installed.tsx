import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { LoaderCircle, MessageSquare, TriangleAlert } from "lucide-react";
import { sentence } from "../../api";
import { tabStorage } from "../data/sign-ins";
import {
  ButtonAnchor,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  cancelSlackInstall,
  finishSlackInstall,
  installFailed,
  installReturnTo,
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
// state was for, or, for an install a linked local install started through
// Helix's relay, that install's own Settings › Slack on this computer (the
// WFE's returnTo, read by relayRedirect). A refusal is said in place, in the WFE's words: another
// Foundry workspace has the Slack workspace, Helix is still disconnecting
// it, an org-wide install, a permission left out, an expired state. Slack's
// own ?error= is said in fixed words (slackReturnCopy), never echoed.
// A cancel (?error= with the state) is told to the WFE, which spends the
// state and, for a linked local install's, says where its Settings › Slack
// is: the page goes back there. A refusal says so too (installReturnTo),
// and its way back is that install's, never Helix's own Settings › Slack,
// which would add Helix Foundry to this Helix workspace instead. When the
// WFE can't say whose install it was (no state, an expired one), the page
// tells someone who started from a Foundry on their computer to go back.

/** Where a failure's button goes: Helix's Settings › Slack, or a local install's. */
type WayBack = { kind: "helix" } | { kind: "local"; url: string };

type Phase =
  | { kind: "finishing" }
  // Slack's cancel, told to the WFE: where it goes back to is not known yet.
  | { kind: "cancelling" }
  | { kind: "failed"; title: string; text: string; back: WayBack }
  // For a local install: on the way back to its Settings › Slack.
  | { kind: "leaving"; title: string }
  // This tab holds nothing to finish: opened again, or in another tab.
  | { kind: "lost" };

/** Said under every failure: the way back for a local install's owner. */
const LOCAL_WAY_BACK =
  "Adding it for the Foundry on your computer? Go back there and add it again from its Settings › Slack.";

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
          : kept.state
            ? { kind: "cancelling" }
            : {
                kind: "failed",
                ...slackReturnCopy(kept.error),
                back: { kind: "helix" },
              },
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
      if (!kept.state) return;
      const copy = slackReturnCopy(kept.error);
      void cancelSlackInstall(kept.error, kept.state).then((returnTo) => {
        if (!returnTo) {
          setPhase({ kind: "failed", ...copy, back: { kind: "helix" } });
          return;
        }
        setPhase({ kind: "leaving", title: copy.title });
        window.location.assign(returnTo);
      });
      return;
    }
    void finishSlackInstall(kept.code, kept.state).then(
      async ({ slack, returnPath, returnTo }) => {
        if (returnTo) {
          setPhase({ kind: "leaving", title: "Helix Foundry is in Slack" });
          window.location.assign(returnTo);
          return;
        }
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
      (e: Error) => {
        const local = installReturnTo(e);
        setPhase({
          kind: "failed",
          title: "Helix Foundry wasn’t added to Slack",
          text: local
            ? `${sentence(e.message)} Add it again from Settings › Slack on the Foundry on your computer.`
            : sentence(e.message),
          back: local ? { kind: "local", url: local } : { kind: "helix" },
        });
      },
    );
  }, []);
  const helixBack = (
    <ButtonLink size="sm" variant="primary" to={paths.settings("slack")}>
      Open Settings › Slack
    </ButtonLink>
  );
  return (
    <Page width="narrow">
      <PageHeader title="Slack" />
      {phase.kind === "leaving" ? (
        <EmptyState
          icon={LoaderCircle}
          title={phase.title}
          text="Going back to the Foundry on this computer…"
        />
      ) : phase.kind === "cancelling" ? (
        <EmptyState
          icon={LoaderCircle}
          title="Helix Foundry wasn’t added"
          text="Adding it to Slack was cancelled."
        />
      ) : phase.kind === "finishing" ? (
        <EmptyState
          icon={LoaderCircle}
          title="Adding Helix Foundry to Slack…"
          text="Foundry is saving the install. This takes a few seconds."
        />
      ) : phase.kind === "failed" ? (
        <div role="alert">
          {phase.back.kind === "local" ? (
            <EmptyState
              icon={TriangleAlert}
              title={phase.title}
              text={phase.text}
              action={
                <ButtonAnchor size="sm" variant="primary" href={phase.back.url}>
                  Back to the Foundry on your computer
                </ButtonAnchor>
              }
            />
          ) : (
            <EmptyState
              icon={TriangleAlert}
              title={phase.title}
              text={`${phase.text} ${LOCAL_WAY_BACK}`}
              action={helixBack}
            />
          )}
        </div>
      ) : (
        <EmptyState
          icon={MessageSquare}
          title="Nothing to finish here"
          text={`This tab didn’t add Helix Foundry to Slack, or it was finished already. Add it from Settings › Slack. ${LOCAL_WAY_BACK}`}
          action={helixBack}
        />
      )}
    </Page>
  );
}
