import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { KeyRound, PlugZap, TriangleAlert } from "lucide-react";
import type {
  Connector,
  ConnectorConfig,
  ConnectorKindName,
  ConnectorSignIn,
} from "../../../shared/wfe";
import { trackBeforeLeavingAt, trackEvent } from "../../analytics";
import { refusalOf, sentence } from "../../api";
import { Button, ButtonAnchor, KeyValue, formatDate, plural } from "../../kit";
import { paths } from "../../paths";
import { Loading, useWorkspace } from "../../ui";
import { Callout } from "../ontology/parts";
import {
  authorizeSignIn,
  connectorQuery,
  signInQuery,
  startSignIn,
} from "./data-api";
import { canManage } from "./model";
import {
  accountLine,
  brokenWhy,
  callbackUrl,
  isProviderUrl,
  memberName,
  needsSignIn,
  providerName,
  signInFailure,
  startAndAuthorize,
  tabStorage,
  writeConnect,
  type ReturnPicks,
} from "./sign-ins";

// Connecting a source by signing in at its provider, in the
// browser: the button's work (useSignInStart), the form an agent's link
// opens (AgentSignIn), and a signed-in connector's notices on its page.
// The callback that finishes it is ./connect-callback.tsx.

/** What one click asks for: a new connector, or one signed in again. */
export type SignInRequest = {
  kind: string;
  // The workspace it is started in; the one open by default.
  workspaceId?: string;
  // A sign-in already started (an agent's link, or Try again): only
  // authorized here.
  signInId?: string;
  // A new connector's name and settings, without a credential.
  name?: string;
  config?: ConnectorConfig;
  // Sign this connector in again.
  connectorId?: string;
  // Makes a retry of the start return the sign-in it started: the hook's
  // `clientToken`, for a new connector's form.
  clientToken?: string;
  // Where the callback returns once connected (ConnectEntry.returnPath),
  // and the page's own picks to hand back to it; by default the page's
  // (SignInReturn).
  returnPath?: string;
  picks?: ReturnPicks;
};

/**
 * Where a sign-in started on a page comes back to once connected, and the
 * page's own picks, kept in this tab's entry (not the URL, which a long
 * goal would outgrow) and handed back as router state: for the pages that
 * choose the connector it made (`connected`), the wizard, Generate and an
 * ontology's setup. A page provides it around everything that may connect.
 */
export const SignInReturn = createContext<{
  path: string;
  picks?: ReturnPicks;
} | null>(null);

/**
 * Sends the browser to the provider to authorize: starts a sign-in unless
 * one is given, authorizes it with the site's callback, keeps it in this
 * tab for the callback, then leaves, for an address it may open only. The
 * button stays pending until the page has gone (or comes back from the
 * back/forward cache).
 *
 * A new connector's form starts its sign-ins under `clientToken`, one per
 * form, so a retry after a refusal finds the sign-in it started; `renew()`
 * makes another once what it starts changes. The page coming back from the
 * back/forward cache makes another too, since the sign-in it started may
 * have ended meanwhile (connected, or run out), and so does a start whose
 * sign-in had ended (startAndAuthorize).
 */
export function useSignInStart() {
  const { id } = useWorkspace(),
    page = useContext(SignInReturn);
  const [pending, setPending] = useState(false),
    [error, setError] = useState<Error | null>(null),
    [clientToken, setClientToken] = useState(() => crypto.randomUUID()),
    renew = useCallback(() => {
      const token = crypto.randomUUID();
      setClientToken(token);
      return token;
    }, []);
  useEffect(() => {
    const back = (e: PageTransitionEvent) => {
      if (!e.persisted) return;
      setPending(false);
      renew();
    };
    window.addEventListener("pageshow", back);
    return () => window.removeEventListener("pageshow", back);
  }, [renew]);
  const go = async (r: SignInRequest) => {
    const workspaceId = r.workspaceId || id;
    setPending(true);
    setError(null);
    try {
      const start = async (token?: string) =>
          (
            await startSignIn(workspaceId, {
              kind: r.kind,
              ...(r.name ? { name: r.name } : {}),
              ...(r.config ? { config: r.config } : {}),
              ...(r.connectorId ? { connectorId: r.connectorId } : {}),
              ...(token ? { clientToken: token } : {}),
            })
          ).signIn.id,
        authorize = (signInId: string) =>
          authorizeSignIn(workspaceId, signInId, callbackUrl());
      const {
        signInId,
        authorized: { url },
      } = r.signInId
        ? { signInId: r.signInId, authorized: await authorize(r.signInId) }
        : await startAndAuthorize(r.clientToken, start, authorize, renew);
      if (!isProviderUrl(url, location.href))
        throw new Error(
          `Foundry was given an address for ${providerName(r.kind)} it won’t open. Try again.`,
        );
      const returnPath = r.returnPath ?? page?.path,
        picks = r.picks ?? page?.picks;
      writeConnect(tabStorage(), {
        workspaceId,
        signInId,
        kind: r.kind,
        ...(returnPath ? { returnPath } : {}),
        ...(picks ? { picks } : {}),
        ...(r.connectorId ? { connectorId: r.connectorId } : {}),
      });
      // Under a fixed path: the button runs on pages whose paths name ids.
      await trackBeforeLeavingAt("/data", "connector_sign_in_started", {
        kind: r.kind as ConnectorKindName,
      });
      location.assign(url);
    } catch (e) {
      trackEvent("connector_sign_in_refused", {
        kind: r.kind as ConnectorKindName,
        reason: refusalOf(e),
      });
      setError(e as Error);
      setPending(false);
    }
  };
  return { pending, error, go, clientToken, renew };
}

/**
 * The settings a sign-in was started with, as its form shows them: the
 * name, then the kind's own (GitHub's repositories and review history,
 * Slack's history, PostHog's project and events, Google's events).
 */
function signInSettings(s: ConnectorSignIn) {
  const c = s.config,
    items = [{ label: "Name", value: s.name || providerName(s.kind || "") }];
  if (!c) return items;
  if ("github" in c)
    items.push(
      {
        label: "Repositories",
        value: c.github.repos?.length
          ? c.github.repos.join(", ")
          : "Every repository Helix Foundry is installed on that you can see",
      },
      {
        label: "Review history",
        value: plural(c.github.reviewDays || 90, "day"),
      },
    );
  if ("slack" in c)
    items.push({
      label: "Message history",
      value: plural(c.slack.historyDays || 90, "day"),
    });
  if ("posthog" in c)
    items.push(
      { label: "Region", value: "US Cloud" },
      {
        label: "Project",
        value: c.posthog.projectId || "The one the account sees",
      },
      {
        label: "Event history",
        value: plural(c.posthog.eventDays || 30, "day"),
      },
    );
  if ("google" in c)
    items.push({
      label: "Event history",
      value: plural(c.google.eventDays || 90, "day"),
    });
  return items;
}

/**
 * The form an agent's link opens (/data?add=<kind>&sign_in=<id>): the
 * sign-in its agent started, bound to it, with its workspace, name and
 * settings and one button, which never runs on its own. One that names a
 * connector signs it in again: it says so, and that the connector then
 * reads with the clicker's own access, in place of whose it reads with now
 * when that is someone else's (the WFE makes them a new account). A link
 * made for someone else says so before any click; one that was declined or
 * failed says what happened then; one that ended says what to do.
 * `onUnbind` leaves it for the kind's own form.
 */
export function AgentSignIn({
  signInId,
  returnPath,
  onUnbind,
}: {
  signInId: string;
  returnPath?: string;
  onUnbind: () => void;
}) {
  const { id, name: workspace, user } = useWorkspace();
  const signIn = useQuery(signInQuery(id, signInId)),
    start = useSignInStart();
  // The connector it signs in again, while it may: who it reads as now.
  const named = signIn.data?.signIn,
    againId =
      named?.connectorId &&
      named.status !== "CONNECTOR_SIGN_IN_STATUS_CONNECTED"
        ? named.connectorId
        : "",
    connector = useQuery({
      ...connectorQuery(id, againId),
      enabled: !!againId,
    }).data?.connector;
  if (signIn.isPending) return <Loading />;
  if (!signIn.data)
    return (
      <div className="connect-data">
        <p role="alert" className="error-box">
          Couldn’t read the sign-in your agent started: {signIn.error?.message}
        </p>
        <button type="button" className="text-button" onClick={onUnbind}>
          Connect here instead
        </button>
      </div>
    );
  const s = signIn.data.signIn,
    provider = providerName(s.kind || ""),
    status = s.status;
  const ended =
    status === "CONNECTOR_SIGN_IN_STATUS_EXPIRED" ||
    status === "CONNECTOR_SIGN_IN_STATUS_CONNECTED" ||
    status === "CONNECTOR_SIGN_IN_STATUS_FINISHING";
  // What happened the last time it was sent to the provider.
  const last =
    status === "CONNECTOR_SIGN_IN_STATUS_DECLINED" ||
    status === "CONNECTOR_SIGN_IN_STATUS_FAILED"
      ? signInFailure(s)
      : null;
  const account = connector?.account,
    theirs = account && account.userId !== user.id,
    name = s.name || connector?.name || provider;
  return (
    <div className="connect-data">
      {againId ? (
        <>
          <p className="setup-intro">
            Your agent asked to sign {name} in again, in {workspace}.
          </p>
          <p className="muted small">
            Signing in again makes {name} read with your {provider} access from
            now on
            {theirs
              ? `, in place of ${memberName(account.connectedBy) || "a former member"}’s${account.label ? ` (${account.label})` : ""}`
              : ""}
            .
          </p>
        </>
      ) : (
        <p className="setup-intro">
          Your agent asked to connect {provider} to {workspace}.
        </p>
      )}
      <KeyValue items={signInSettings(s)} />
      {!s.mine ? (
        <p role="alert" className="error-box">
          This link was made for someone else: ask your agent for a new one.
        </p>
      ) : status === "CONNECTOR_SIGN_IN_STATUS_CONNECTED" ? (
        <p role="status" className="muted small">
          This sign-in already connected {provider}.{" "}
          {s.connectorId && (
            <Link to={paths.connection(s.connectorId)}>Open it</Link>
          )}
        </p>
      ) : status === "CONNECTOR_SIGN_IN_STATUS_FINISHING" ? (
        <p role="status" className="muted small">
          This sign-in is finishing in another tab.
        </p>
      ) : status === "CONNECTOR_SIGN_IN_STATUS_EXPIRED" ? (
        <p role="alert" className="error-box">
          This sign-in ran out: ask your agent for a new link, or{" "}
          <button type="button" className="text-button" onClick={onUnbind}>
            connect {provider} here
          </button>
          .
        </p>
      ) : last ? (
        <p role="status" className="error-box">
          <strong>Last time: {last.title}.</strong>{" "}
          {last.retry === "start" ? (
            <>
              Its settings can’t be used: ask your agent for a new link, or{" "}
              <button type="button" className="text-button" onClick={onUnbind}>
                connect {provider} here
              </button>
              .
            </>
          ) : (
            last.text
          )}
        </p>
      ) : null}
      {s.mine && !ended && last?.retry !== "start" && (
        <button
          className="button primary"
          type="button"
          disabled={start.pending}
          onClick={() =>
            void start.go({
              kind: s.kind || "",
              signInId: s.id,
              returnPath,
              // Signing a connector in again: its page, never a new one's.
              ...(againId ? { connectorId: againId } : {}),
            })
          }
        >
          {start.pending
            ? `Opening ${provider}…`
            : againId
              ? `Sign in again with ${provider}`
              : `Connect with ${provider}`}
        </button>
      )}
      {start.error && (
        <p role="alert" className="error-box">
          {start.error.message}
        </p>
      )}
    </div>
  );
}

/**
 * Sign in again, for a connector that signs in: as the person clicking,
 * who becomes the member it reads as. Back on the connector's page.
 */
export function SignInAgainButton({
  connector: c,
  variant = "primary",
  size = "sm",
}: {
  connector: Connector;
  variant?: "primary" | "secondary";
  size?: "sm" | "md";
}) {
  const start = useSignInStart(),
    { notify } = useWorkspace();
  useEffect(() => {
    if (start.error) notify(start.error.message, true);
  }, [start.error]);
  return (
    <Button
      size={size}
      variant={variant}
      icon={KeyRound}
      pending={start.pending}
      onClick={() => void start.go({ kind: c.kind, connectorId: c.id })}
    >
      Sign in again
    </Button>
  );
}

/** What a connector page's arrival from the callback carries. */
export type SignedInArrival = {
  // Signing in again changed the provider account it reads with.
  identityChanged?: boolean;
  // The connector was made but can't read all it names.
  discoveryProblem?: string;
};

/**
 * A signed-in connector's notices on its page: that it needs signing in
 * again (why, since when, and Sign in again for owners and admins), what
 * the sign-in that just made it couldn't read, with GitHub's install link,
 * and whose access it reads with now when that changed.
 */
export function AccountNotices({
  connector: c,
  arrival,
  installUrl,
}: {
  connector: Connector;
  arrival: SignedInArrival;
  installUrl?: string;
}) {
  const { role } = useWorkspace(),
    navigate = useNavigate(),
    manage = canManage(role);
  const dismiss = (key: keyof SignedInArrival) =>
    navigate(".", { replace: true, state: { ...arrival, [key]: undefined } });
  if (!c.account) return null;
  const at = c.account.brokenAt,
    provider = providerName(c.kind);
  return (
    <>
      {needsSignIn(c) && (
        <Callout
          icon={TriangleAlert}
          tone="danger"
          title={`${c.name || provider} needs signing in again`}
          text={`${brokenWhy(c)} It stopped syncing${at ? ` on ${formatDate(at)}` : ""}: its tables keep what its last sync read, and loads and questions still read them. ${manage ? `Sign it in again with your ${provider} account to sync again.` : "Ask an owner or admin to sign it in again."}`}
          action={manage && <SignInAgainButton connector={c} />}
        />
      )}
      {arrival.discoveryProblem && (
        <Callout
          icon={PlugZap}
          tone="muted"
          title={`Connected, but ${provider} can’t read everything it names`}
          text={sentence(arrival.discoveryProblem)}
          action={
            <span className="on-card-actions">
              {installUrl && (
                <ButtonAnchor
                  size="sm"
                  href={installUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  Install Helix Foundry
                </ButtonAnchor>
              )}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => dismiss("discoveryProblem")}
              >
                Dismiss
              </Button>
            </span>
          }
        />
      )}
      {arrival.identityChanged && (
        <Callout
          icon={KeyRound}
          tone="muted"
          title={`Now reads with ${c.account.label || "another account"}’s access`}
          text={`It was signed in again as another ${provider} account, so its next sync reads what that account can see. ${accountLine(c)}.`}
          action={
            <Button
              size="sm"
              variant="ghost"
              onClick={() => dismiss("identityChanged")}
            >
              Dismiss
            </Button>
          }
        />
      )}
    </>
  );
}
