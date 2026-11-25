import "./sign-in.css";
import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, Navigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Check } from "lucide-react";
import { signInStarted, trackBeforeLeaving, trackEvent } from "./analytics";
import { signOut } from "./api";
import { Button, ButtonAnchor, useTitle } from "./kit";
import {
  chromeIntent,
  inAppBrowser,
  type InAppBrowser,
} from "./in-app-browser";
import { PasswordSignIn } from "./password";
import { whoamiQuery } from "./workspaces";
import { LOCAL } from "../shared/deployment";
import { parseReturnPath, signInPath } from "../shared/return-path";

// Sign-in, outside the shell, with a provider or an email and password
// (./password.tsx). Each provider is a full-page navigation to /auth/login,
// which starts the WFE-brokered WorkOS flow; WorkOS returns to
// /login/callback, which lands back here with ?error=<reason> if it failed,
// on /login/verify when WorkOS emailed a code to check the address first, or
// goes through the company's single sign-on when it requires it.

type Provider = { id: "github" | "google" | "microsoft"; label: string };
const providers: Provider[] = [
  { id: "google", label: "Google" },
  { id: "github", label: "GitHub" },
  { id: "microsoft", label: "Microsoft" },
];

// Why sign-in came back here, one reason per cause, as sign_in_failed reports
// it: /auth/login's (`request`, `unavailable`), /login/callback's (the
// provider, the callback's link, this browser's cookie and the WFE's code
// exchange) and /auth/verify-email's.
const errors = {
  request: {
    title: "Sign-in couldn’t start.",
    text: "Choose a way to sign in below to try again.",
  },
  unavailable: {
    title: "Sign-in is unavailable right now.",
    text: "Helix couldn’t start sign-in. Try again in a moment.",
  },
  provider_error: {
    title: "Sign-in was cancelled.",
    text: "The provider didn’t sign you in. Choose a way to sign in to try again.",
  },
  missing_code: {
    title: "Sign-in didn’t finish.",
    text: "The sign-in link came back incomplete. Try again.",
  },
  invalid_state: {
    title: "Sign-in didn’t finish.",
    text: "The sign-in link came back incomplete. Try again.",
  },
  missing_flow_cookie: {
    title: "Sign-in didn’t finish.",
    text: "This browser lost the sign-in it started: it took over 10 minutes, or an app’s built-in browser dropped it. Try again in your browser.",
  },
  flow_cookie_invalid: {
    title: "Sign-in ran out of time.",
    text: "Start again: a sign-in lasts 10 minutes.",
  },
  login_session_unknown: {
    title: "Sign-in ran out of time.",
    text: "Helix no longer holds that sign-in. Start again.",
  },
  state_mismatch: {
    title: "Sign-in didn’t finish.",
    text: "The sign-in link isn’t the one this browser started. Try again in one tab.",
  },
  code_exchange_failed: {
    title: "Sign-in didn’t finish.",
    text: "The sign-in link expired or was already used. Try again.",
  },
  exchange_unavailable: {
    title: "Sign-in didn’t finish.",
    text: "Helix couldn’t reach sign-in. Try again in a moment.",
  },
  verify_start_failed: {
    title: "Sign-in didn’t finish.",
    text: "Helix couldn’t start checking your email. Try again, or choose another way to sign in.",
  },
  session_failed: {
    title: "Sign-in didn’t finish.",
    text: "Helix couldn’t open your session. Try again in a moment.",
  },
  denied: {
    title: "This account can’t sign in here.",
    text: "Sign in with your work account, or ask us for access.",
  },
  verify: {
    title: "Your email isn’t verified yet.",
    text: "The code ran out or was tried too many times. Sign in again to get a new one.",
  },
  sso: {
    title: "Your company signs in with single sign-on.",
    text: "Helix couldn’t start your company’s sign-in. Try again in a moment, or ask whoever manages Helix at your company.",
  },
} satisfies Record<string, { title: string; text: string }>;

/** Why sign-in came back here: the ?error= of /auth/login and /login/callback. */
export type SignInError = keyof typeof errors;
const isSignInError = (code: string | null): code is SignInError =>
  !!code && Object.hasOwn(errors, code);

function ProviderLogo({ id }: { id: Provider["id"] }) {
  if (id === "github")
    return (
      <svg viewBox="0 0 16 16" className="hf-provider-logo" aria-hidden>
        <path
          fill="currentColor"
          d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
        />
      </svg>
    );
  if (id === "google")
    return (
      <svg viewBox="0 0 18 18" className="hf-provider-logo" aria-hidden>
        <path
          fill="#4285F4"
          d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62Z"
        />
        <path
          fill="#34A853"
          d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.33A9 9 0 0 0 9 18Z"
        />
        <path
          fill="#FBBC05"
          d="M3.97 10.72a5.4 5.4 0 0 1 0-3.44V4.95H.96a9 9 0 0 0 0 8.1l3.01-2.33Z"
        />
        <path
          fill="#EA4335"
          d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.58A8.65 8.65 0 0 0 9 0 9 9 0 0 0 .96 4.95l3.01 2.33C4.68 5.16 6.66 3.58 9 3.58Z"
        />
      </svg>
    );
  return (
    <svg viewBox="0 0 16 16" className="hf-provider-logo" aria-hidden>
      <path fill="#F25022" d="M0 0h7.6v7.6H0z" />
      <path fill="#7FBA00" d="M8.4 0H16v7.6H8.4z" />
      <path fill="#00A4EF" d="M0 8.4h7.6V16H0z" />
      <path fill="#FFB900" d="M8.4 8.4H16V16H8.4z" />
    </svg>
  );
}

/**
 * Who is signed in, and the way out, for gate screens a signed-in user can
 * get stuck on (the workspace picker, a failed load): the shell's account
 * menu is not there. Without an email it offers only Sign out.
 */
export function SignedInAs({ email }: { email?: string }) {
  const [leaving, setLeaving] = useState(false);
  // A local build has one owner and nothing to sign out of.
  if (LOCAL) return null;
  return (
    <p className="hf-gate-account">
      {email && (
        <>
          Signed in as <strong data-replay-block>{email}</strong>
          <span aria-hidden> · </span>
        </>
      )}
      <button
        type="button"
        disabled={leaving}
        onClick={() => {
          setLeaving(true);
          void signOut();
        }}
      >
        Sign out
      </button>
    </p>
  );
}

// The narrow page every gate screen shares. `account` is the signed-in
// user's email, for a page they may need to leave by signing out. `aside`
// sits beside the card on a wide screen and under it on a phone.
export function GatePage({
  title,
  lede,
  wide,
  account,
  aside,
  children,
}: {
  title: string;
  lede?: ReactNode;
  wide?: boolean;
  account?: string;
  aside?: ReactNode;
  children?: ReactNode;
}) {
  useTitle(title);
  return (
    <div className="hf-app">
      <main className={"hf-gate" + (aside ? " has-aside" : "")}>
        <div className={"hf-gate-card" + (wide ? " is-wide" : "")}>
          <a href="/" className="hf-gate-brand" aria-label="Helix Foundry">
            <span className="hf-brand-name">
              helix<span>foundry</span>
            </span>
          </a>
          <h1 className="hf-gate-title">{title}</h1>
          {lede && <p className="hf-gate-lede">{lede}</p>}
          {children}
          {account !== undefined && <SignedInAs email={account} />}
        </div>
        {aside}
      </main>
    </div>
  );
}

export function SignInPage() {
  const [params] = useSearchParams(),
    [pending, setPending] = useState<Provider["id"] | null>(null);
  const code = params.get("error"),
    error = isSignInError(code) ? errors[code] : undefined,
    returnTo = parseReturnPath(params.get("return_to"));
  // An app's built-in browser can lose the sign-in on its way back.
  const [inApp] = useState(() =>
    LOCAL ? null : inAppBrowser(navigator.userAgent),
  );
  useEffect(() => {
    if (isSignInError(code)) trackEvent("sign_in_failed", { reason: code });
  }, [code]);
  useEffect(() => {
    if (inApp)
      trackEvent("in_app_browser_shown", {
        app: inApp.app,
        os: inApp.os,
        failed: isSignInError(code),
      });
  }, [inApp, code]);
  // Already signed in: go straight on.
  const me = useQuery(whoamiQuery);
  if (me.data) return <Navigate to={returnTo || "/"} replace />;
  // Someone who came here cold sees what Foundry is: the app sends a
  // signed-out user with return_to (but from `/`, and after signing out), a
  // failed sign-in comes back with ?error=, and a local build has no sign-in.
  const cold = !LOCAL && !returnTo && !code;
  const start = async (id: Provider["id"]) => {
    setPending(id);
    await signInStarted(id);
    const q = new URLSearchParams({ provider: id });
    if (returnTo) q.set("return_to", returnTo);
    window.location.assign(`/auth/login?${q}`);
  };
  return (
    <GatePage
      title="Sign in to Helix Foundry"
      lede="Use your Helix account, or continue to create one."
      aside={cold ? <Pitch /> : undefined}
    >
      {error && (
        <div role="alert" className="hf-gate-alert">
          <strong>{error.title}</strong>
          <span>{error.text}</span>
        </div>
      )}
      {inApp && <InAppNotice inApp={inApp} />}
      <div className="hf-gate-body">
        {providers.map((p) => (
          <Button
            key={p.id}
            size="lg"
            className="hf-provider-button"
            pending={pending === p.id}
            disabled={!!pending}
            onClick={() => void start(p.id)}
          >
            {pending !== p.id && <ProviderLogo id={p.id} />}
            <span>Continue with {p.label}</span>
          </Button>
        ))}
      </div>
      <PasswordSignIn returnTo={returnTo} disabled={!!pending} />
      {DEMO_URL && (
        <p className="hf-gate-fine">
          Not ready to sign in?{" "}
          <a
            href={DEMO_URL}
            onClick={(e) => {
              if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey)
                return trackEvent("demo_clicked");
              e.preventDefault();
              void trackBeforeLeaving("demo_clicked").then(() =>
                window.location.assign(DEMO_URL),
              );
            }}
          >
            See it without an account
          </a>
        </p>
      )}
      <Terms />
    </GatePage>
  );
}

/**
 * The stamp's public sample demo (next.config.ts): none yet, and never in a
 * local build, which hides the link.
 */
const DEMO_URL = process.env.NEXT_PUBLIC_FOUNDRY_DEMO_URL || "";

/**
 * Asks a reader in an app's built-in browser to sign in from their own: by
 * Chrome's intent on Android, the app's menu on iOS, or the copied link.
 */
function InAppNotice({ inApp }: { inApp: InAppBrowser }) {
  const [copy, setCopy] = useState<"idle" | "copied" | "failed">("idle");
  // This page, without the failure it came back with.
  const here = new URL(location.href);
  here.searchParams.delete("error");
  const left = (via: "chrome" | "copy_link") =>
    trackBeforeLeaving("in_app_browser_left", {
      app: inApp.app,
      os: inApp.os,
      via,
    });
  return (
    <div role="status" className="hf-gate-notice">
      <strong>Open in your browser to sign in</strong>
      <span>
        {inApp.name ? `${inApp.name}’s` : "This app’s"} built-in browser can
        lose your sign-in before it finishes.
        {inApp.os === "ios" &&
          " Tap ··· or the share button, then Open in Safari (or Open in browser)."}
      </span>
      <div className="hf-gate-notice-actions">
        {inApp.os === "android" && (
          <ButtonAnchor
            variant="primary"
            size="sm"
            href={chromeIntent(here)}
            onClick={() => void left("chrome")}
          >
            Open in Chrome
          </ButtonAnchor>
        )}
        <Button
          size="sm"
          onClick={() =>
            // A web view may have no clipboard at all.
            void Promise.resolve()
              .then(() => navigator.clipboard.writeText(here.href))
              .then(
                () => {
                  setCopy("copied");
                  void left("copy_link");
                },
                () => setCopy("failed"),
              )
          }
        >
          {copy === "copied" ? "Link copied" : "Copy link"}
        </Button>
      </div>
      {copy === "failed" && (
        <span>
          Copy this address into your browser: <code>{here.href}</code>
        </span>
      )}
    </div>
  );
}

/** The Ask demo on Foundry's landing page (helix-website's `#foundry-ask`). */
const ASK_DEMO_URL =
  "https://www.helix-db.com/?utm_source=foundry&utm_medium=login&utm_campaign=pitch#foundry-ask";

/** What Foundry is, in a line and three points, beside a cold sign-in. */
function Pitch() {
  return (
    <section className="hf-gate-pitch" aria-labelledby="sign-in-pitch">
      <h2 id="sign-in-pitch" className="hf-gate-pitch-title">
        Your company’s data as one graph your agents can query.
      </h2>
      <ul className="hf-gate-pitch-list">
        <li>
          <Check size={14} aria-hidden />
          <span>Connect Postgres, Stripe, Linear, Slack, Notion and more.</span>
        </li>
        <li>
          <Check size={14} aria-hidden />
          <span>
            Agents query it over MCP, and every answer cites its source.
          </span>
        </li>
        <li>
          <Check size={14} aria-hidden />
          <span>
            Open source: use it hosted, or run <code>docker compose up</code>.
          </span>
        </li>
      </ul>
      <a className="hf-gate-pitch-link" href={ASK_DEMO_URL}>
        See it answer a question
        <ArrowRight size={14} aria-hidden />
      </a>
    </section>
  );
}

/** The terms a new account agrees to, under the sign-in and sign-up forms. */
export function Terms() {
  return (
    <p className="hf-gate-fine">
      By continuing, you agree to the HelixDB{" "}
      <a href="https://www.helix-db.com/terms">Terms</a> and acknowledge the{" "}
      <a href="https://www.helix-db.com/privacy">Privacy Policy</a>.
    </p>
  );
}

/** What /auth/verify-email answers (src/app/auth/verify-email/route.ts). */
export type VerifyEmailAnswer =
  // GET: the sign-in waits for the code sent to `email`.
  | { email: string; returnTo: string }
  // Where to go next: signed in, the company's single sign-on, or the
  // sign-in page saying why it ended.
  | { redirectTo: string }
  // POST: a wrong code, or the check couldn't run; try again.
  | { error: "code" | "unavailable" };

const codeErrors = {
  code: "That code isn’t right. Check the email and try again.",
  unavailable: "Helix couldn’t check the code. Try again in a moment.",
} as const;

/**
 * /login/verify?flow=: WorkOS hasn't seen this address verified (a GitHub
 * or Microsoft account, often) and emailed it a code; the sign-in the
 * callback kept waits for it. Once it's right, the page the sign-in started
 * from opens.
 */
export function VerifyEmailPage() {
  const [params] = useSearchParams();
  const flow = params.get("flow") ?? "";
  const [code, setCode] = useState(""),
    [pending, setPending] = useState(false),
    [error, setError] = useState<keyof typeof codeErrors | null>(null);
  const waiting = useQuery({
    queryKey: ["sign-in", "verify-email", flow],
    queryFn: async () => {
      const res = await fetch(
        `/auth/verify-email?${new URLSearchParams({ flow })}`,
        { cache: "no-store" },
      );
      return (await res.json()) as VerifyEmailAnswer;
    },
    staleTime: Infinity,
    retry: false,
  });
  const answer = waiting.data,
    email = answer && "email" in answer ? answer.email : "",
    returnTo = answer && "returnTo" in answer ? answer.returnTo : null;
  const shown = !!answer && "email" in answer;
  useEffect(() => {
    if (shown) trackEvent("email_verification_shown");
  }, [shown]);
  if (answer && "redirectTo" in answer)
    return <Navigate to={answer.redirectTo} replace />;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setPending(true);
    setError(null);
    let next: VerifyEmailAnswer;
    try {
      const res = await fetch("/auth/verify-email", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ flow, code }),
      });
      next = (await res.json()) as VerifyEmailAnswer;
    } catch {
      next = { error: "unavailable" };
    }
    if ("redirectTo" in next && typeof next.redirectTo === "string") {
      // The sign-in page says itself why a sign-in ended (sign_in_failed).
      if (!next.redirectTo.startsWith("/login"))
        await trackBeforeLeaving("email_verified");
      window.location.assign(next.redirectTo);
      return;
    }
    const reason =
      "error" in next && next.error === "code" ? "code" : "unavailable";
    trackEvent("email_verification_failed", { reason });
    setError(reason);
    setPending(false);
  };

  return (
    <GatePage
      title="Check your email"
      lede={
        email ? (
          <>
            Enter the code we sent to <strong data-replay-block>{email}</strong>{" "}
            to finish signing in.
          </>
        ) : (
          "Enter the code we emailed you to finish signing in."
        )
      }
    >
      <form className="hf-gate-body" onSubmit={(e) => void submit(e)}>
        <label className="hf-field">
          <span>Verification code</span>
          <input
            name="code"
            autoFocus
            required
            autoComplete="one-time-code"
            inputMode="numeric"
            spellCheck={false}
            maxLength={32}
            value={code}
            aria-invalid={error === "code" || undefined}
            aria-describedby={error ? "verify-email-error" : undefined}
            onChange={(e) => {
              setCode(e.target.value);
              setError(null);
            }}
          />
        </label>
        {error && (
          <p id="verify-email-error" role="alert" className="hf-form-error">
            {codeErrors[error]}
          </p>
        )}
        <Button
          type="submit"
          variant="primary"
          size="lg"
          pending={pending}
          disabled={!code.trim()}
        >
          Verify email
        </Button>
      </form>
      <p className="hf-gate-fine">
        No email? Check your spam folder, or{" "}
        <Link to={signInPath(returnTo)}>sign in again</Link> to get a new code.
      </p>
    </GatePage>
  );
}
