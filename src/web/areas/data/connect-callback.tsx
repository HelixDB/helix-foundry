import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, LoaderCircle, TriangleAlert } from "lucide-react";
import type { ConnectorKindName, ConnectorSignIn } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { ApiError, sentence } from "../../api";
import {
  Button,
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import { connectorKindsQuery, finishSignIn, getSignIn } from "./data-api";
import { GitHubInstallStep } from "./github-install-step";
import { needsGitHubInstall, type PendingInstall } from "./github-install";
import { useSignInStart, type SignedInArrival } from "./sign-in";
import {
  codeStale,
  connectedPath,
  failureCode,
  forgetConnect,
  providerName,
  readConnect,
  signInFailure,
  startAgainPath,
  tabStorage,
  withConnected,
  writeConnect,
  type ConnectEntry,
  type ReturnPicks,
} from "./sign-ins";

// /connect/callback: where the provider (through WorkOS Pipes) returns after
// the user authorized Helix Foundry, or declined. Its code was moved into
// this tab's sign-in, and the URL made bare, before the app made any call
// (main.tsx, captureCallback). Here it is finished once, in the workspace
// the sign-in was started in (a ref keeps <StrictMode>'s second effect from
// finishing it again; the backend returns a repeat as it stands). After a
// transport error it reads the sign-in until it ends rather than starting
// again. Connected, it goes back where the sign-in was started: the wizard
// or Generate with the connector chosen (`connected`), else the connector's
// page. Otherwise it says why, in Foundry's words for the failure's code,
// with Try again on the same sign-in, so an agent waiting on it follows.
// A GitHub sign-in going back to the wizard or Generate that reads nothing
// for want of Helix Foundry's GitHub App stops here at the install step
// first, since those pages don't show what a connector can't read; the
// connector's own page shows the step itself.

// How often a sign-in being finished is read, and for how long: the
// backend gives a finish 60 s of its own.
const POLL_MS = 1500,
  POLL_FOR_MS = 90_000;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// A refusal is the WFE's answer; anything else (no answer, a 5xx, a gateway
// timeout) may have finished it all the same.
const refused = (e: unknown) => e instanceof ApiError && e.status < 500;
const ENDED = new Set<ConnectorSignIn["status"]>([
  "CONNECTOR_SIGN_IN_STATUS_CONNECTED",
  "CONNECTOR_SIGN_IN_STATUS_DECLINED",
  "CONNECTOR_SIGN_IN_STATUS_FAILED",
  "CONNECTOR_SIGN_IN_STATUS_EXPIRED",
]);
const OUTCOMES = {
  CONNECTOR_SIGN_IN_STATUS_CONNECTED: "connected",
  CONNECTOR_SIGN_IN_STATUS_DECLINED: "declined",
  CONNECTOR_SIGN_IN_STATUS_EXPIRED: "expired",
} as const;

type Phase =
  | { kind: "finishing" }
  // `stale`: its code had outlived a code's 90 s when it came back.
  | { kind: "ended"; signIn: ConnectorSignIn; stale: boolean }
  | { kind: "refused"; message: string }
  // Connected, but GitHub's app isn't installed where it reads: install it,
  // or skip to `to`, where the sign-in goes back to.
  | {
      kind: "install";
      problem: string;
      installUrl: string;
      install: Omit<PendingInstall, "at">;
      skip: () => void;
    }
  // This tab started no sign-in, or holds none any more.
  | { kind: "lost" };

/**
 * The sign-in once no finish of it is under way: read until it isn't
 * FINISHING (through transport errors), for as long as the backend's
 * finish would take.
 */
async function settled(entry: ConnectEntry): Promise<ConnectorSignIn> {
  const until = Date.now() + POLL_FOR_MS;
  let last: ConnectorSignIn | undefined;
  for (;;) {
    try {
      last = (await getSignIn(entry.workspaceId, entry.signInId)).signIn;
      if (last.status !== "CONNECTOR_SIGN_IN_STATUS_FINISHING") return last;
    } catch (e) {
      if (refused(e)) throw e;
    }
    if (Date.now() > until)
      return last || { id: entry.signInId, kind: entry.kind };
    await sleep(POLL_MS);
  }
}

export function ConnectCallbackPage() {
  const navigate = useNavigate(),
    qc = useQueryClient(),
    { notify } = useWorkspace();
  const [entry] = useState(() => readConnect(tabStorage())),
    [phase, setPhase] = useState<Phase>(
      entry ? { kind: "finishing" } : { kind: "lost" },
    ),
    retry = useSignInStart(),
    started = useRef(false);
  const provider = providerName(entry?.kind || "");
  useTitle(`Connecting ${provider}`);
  useEffect(() => {
    if (!entry || started.current) return;
    started.current = true;
    const stale = codeStale(entry);
    // What came back from the provider, spent once.
    const answer = entry.code
      ? { connectCode: entry.code }
      : entry.error
        ? { error: entry.error }
        : null;
    void (async () => {
      let s: ConnectorSignIn;
      try {
        s = answer
          ? await finishSignIn(entry.workspaceId, entry.signInId, answer).then(
              (r) => r.signIn,
              (e) => {
                if (refused(e)) throw e;
                return settled(entry);
              },
            )
          : await settled(entry);
      } catch (e) {
        setPhase({ kind: "refused", message: sentence((e as Error).message) });
        return;
      }
      if (s.status === "CONNECTOR_SIGN_IN_STATUS_FINISHING")
        s = await settled(entry).catch(() => s);
      // Its answer is spent: a reload reads the sign-in as it stands.
      if (answer)
        writeConnect(tabStorage(), {
          ...entry,
          code: undefined,
          error: undefined,
          at: undefined,
        });
      if (answer && ENDED.has(s.status))
        trackEvent("connector_sign_in_finished", {
          kind: entry.kind as ConnectorKindName,
          outcome:
            OUTCOMES[s.status as keyof typeof OUTCOMES] || ("failed" as const),
          // Why it dropped off: a failure's code (a stale code is one).
          ...(s.status === "CONNECTOR_SIGN_IN_STATUS_FAILED"
            ? {
                reason: stale
                  ? ("code_expired" as const)
                  : failureCode(s.failure),
              }
            : {}),
        });
      if (s.status !== "CONNECTOR_SIGN_IN_STATUS_CONNECTED" || !s.connectorId) {
        setPhase({
          kind: "ended",
          signIn: { ...s, kind: s.kind || entry.kind },
          stale,
        });
        return;
      }
      if (answer && !entry.connectorId)
        trackEvent("connector_created", {
          kind: entry.kind as ConnectorKindName,
        });
      forgetConnect(tabStorage());
      await Promise.all(
        ["connectors", "document-sets"].map((key) =>
          qc.invalidateQueries({ queryKey: [entry.workspaceId, key] }),
        ),
      );
      notify(
        entry.connectorId
          ? `Signed in again. ${s.name || provider} syncs now`
          : `Connected ${s.name || provider}. Its first sync is queued`,
      );
      // The page's own picks go back to it, the new connector among them.
      const arrival: SignedInArrival & { picks?: ReturnPicks } = {
        ...(s.discoveryProblem ? { discoveryProblem: s.discoveryProblem } : {}),
        ...(s.identityChanged ? { identityChanged: true } : {}),
        ...(entry.picks
          ? { picks: withConnected(entry.picks, s.connectorId) }
          : {}),
      };
      const to = connectedPath(entry, s.connectorId, paths.connection),
        go = () => navigate(to, { replace: true, state: arrival });
      const toConnection =
        new URL(to, "https://foundry.invalid").pathname ===
        paths.connection(s.connectorId);
      if (
        entry.kind === "github" &&
        !toConnection &&
        needsGitHubInstall(s.discoveryProblem)
      ) {
        const kinds = await qc
          .fetchQuery(connectorKindsQuery(entry.workspaceId))
          .catch(() => undefined);
        const installUrl = kinds?.kinds?.find((k) => k.kind === "github")
          ?.github?.installUrl;
        if (installUrl) {
          // GitHub's return goes back with the page's picks, the new
          // connector among them; the problem is the install's to fix.
          const { discoveryProblem: _, ...state } = arrival;
          return setPhase({
            kind: "install",
            problem: s.discoveryProblem!,
            installUrl,
            install: {
              workspaceId: entry.workspaceId,
              connectorId: s.connectorId,
              to,
              state,
            },
            skip: go,
          });
        }
      }
      go();
    })();
  }, []);

  if (phase.kind === "lost")
    return (
      <Page width="narrow">
        <PageHeader title="Connect a source" />
        <EmptyState
          icon={KeyRound}
          title="This tab didn’t start a sign-in"
          text="Open the link in the tab that started it, or start again from Data."
          action={<ButtonLink to={paths.data()}>Open Data</ButtonLink>}
        />
      </Page>
    );
  const again = entry && startAgainPath(entry, paths);
  return (
    <Page width="narrow">
      <PageHeader title={`Connecting ${provider}`} />
      {phase.kind === "finishing" ? (
        <Callout
          icon={LoaderCircle}
          live
          title={`Finishing the sign-in with ${provider}…`}
          text="Foundry is saving the account and making the connection. This takes a few seconds."
        />
      ) : phase.kind === "install" ? (
        <GitHubInstallStep
          problem={phase.problem}
          installUrl={phase.installUrl}
          install={phase.install}
          onSkip={phase.skip}
        />
      ) : phase.kind === "refused" ? (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title="Foundry couldn’t finish the sign-in"
          text={phase.message}
          action={
            again && (
              <ButtonLink size="sm" to={again}>
                Start again
              </ButtonLink>
            )
          }
        />
      ) : (
        (() => {
          const f = signInFailure(phase.signIn, phase.stale);
          return (
            <Callout
              icon={TriangleAlert}
              tone="danger"
              title={f.title}
              text={retry.error ? retry.error.message : f.text}
              action={
                f.retry === "again" ? (
                  <Button
                    size="sm"
                    variant="primary"
                    pending={retry.pending}
                    onClick={() =>
                      void retry.go({
                        kind: entry!.kind,
                        workspaceId: entry!.workspaceId,
                        signInId: entry!.signInId,
                        returnPath: entry!.returnPath,
                        picks: entry!.picks,
                        connectorId: entry!.connectorId,
                      })
                    }
                  >
                    Try again
                  </Button>
                ) : (
                  again && (
                    <ButtonLink size="sm" variant="primary" to={again}>
                      Start again
                    </ButtonLink>
                  )
                )
              }
            />
          );
        })()
      )}
    </Page>
  );
}
