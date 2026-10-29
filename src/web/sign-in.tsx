import "./sign-in.css";
import { useState, type ReactNode } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { signInStarted } from "./analytics";
import { signOut } from "./api";
import { Button, useTitle } from "./kit";
import { whoamiQuery } from "./workspaces";
import { LOCAL } from "../shared/deployment";
import { parseReturnPath } from "../shared/return-path";

// Sign-in, outside the shell. Each provider is a full-page navigation to
// /auth/login, which starts the WFE-brokered WorkOS flow; WorkOS returns to
// /login/callback, which lands back here with ?error=<reason> if it failed.

type Provider = { id: "github" | "google" | "microsoft"; label: string };
const providers: Provider[] = [
  { id: "google", label: "Google" },
  { id: "github", label: "GitHub" },
  { id: "microsoft", label: "Microsoft" },
];

const errors: Record<string, { title: string; text: string }> = {
  request: {
    title: "Sign-in couldn’t start.",
    text: "Choose a provider below to try again.",
  },
  unavailable: {
    title: "Sign-in is unavailable right now.",
    text: "Helix couldn’t start sign-in. Try again in a moment.",
  },
  callback: {
    title: "Sign-in didn’t finish.",
    text: "The sign-in link expired or was already used. Try again.",
  },
  denied: {
    title: "This account can’t sign in here.",
    text: "Sign in with your work account, or ask us for access.",
  },
  verify: {
    title: "Your email isn’t verified yet.",
    text: "Verify it with your provider, or sign in with another provider.",
  },
  sso: {
    title: "Your company signs in with single sign-on.",
    text: "Single sign-on isn’t available on Helix Foundry yet. Sign in to the Helix dashboard instead.",
  },
};

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
          Signed in as <strong>{email}</strong>
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
// user's email, for a page they may need to leave by signing out.
export function GatePage({
  title,
  lede,
  wide,
  account,
  children,
}: {
  title: string;
  lede?: ReactNode;
  wide?: boolean;
  account?: string;
  children?: ReactNode;
}) {
  useTitle(title);
  return (
    <div className="hf-app">
      <main className="hf-gate">
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
      </main>
    </div>
  );
}

export function SignInPage() {
  const [params] = useSearchParams(),
    [pending, setPending] = useState<Provider["id"] | null>(null);
  const error = errors[params.get("error") || ""],
    returnTo = parseReturnPath(params.get("return_to"));
  // Already signed in: go straight on.
  const me = useQuery(whoamiQuery);
  if (me.data) return <Navigate to={returnTo || "/"} replace />;
  const start = (id: Provider["id"]) => {
    setPending(id);
    signInStarted(id);
    const q = new URLSearchParams({ provider: id });
    if (returnTo) q.set("return_to", returnTo);
    window.location.assign(`/auth/login?${q}`);
  };
  return (
    <GatePage
      title="Sign in to Helix Foundry"
      lede="Use the account you use for Helix. Your workspaces come with it."
    >
      {error && (
        <div role="alert" className="hf-gate-alert">
          <strong>{error.title}</strong>
          <span>{error.text}</span>
        </div>
      )}
      <div className="hf-gate-body">
        {providers.map((p) => (
          <Button
            key={p.id}
            size="lg"
            className="hf-provider-button"
            pending={pending === p.id}
            disabled={!!pending}
            onClick={() => start(p.id)}
          >
            {pending !== p.id && <ProviderLogo id={p.id} />}
            <span>Continue with {p.label}</span>
          </Button>
        ))}
      </div>
      <p className="hf-gate-fine">
        By continuing, you agree to the HelixDB{" "}
        <a href="https://www.helix-db.com/terms">Terms</a> and acknowledge the{" "}
        <a href="https://www.helix-db.com/privacy">Privacy Policy</a>.
      </p>
    </GatePage>
  );
}
