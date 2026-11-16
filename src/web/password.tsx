import { useState, type FormEvent } from "react";
import { Link, Navigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { signInStarted, trackEvent } from "./analytics";
import { Button, ButtonLink } from "./kit";
import { GatePage, Terms } from "./sign-in";
import { whoamiQuery } from "./workspaces";
import { parseReturnPath } from "../shared/return-path";

// Signing in with an email and password: the sign-in page's form, and the
// pages to create an account (/login/sign-up), ask for a reset link
// (/login/forgot) and choose a new password from it (/reset-password?token=,
// where the WFE has WorkOS's email link to). Each POSTs to
// /auth/password/<action> (src/server/password.ts), which calls the WFE.

/** What /auth/password/<action> answers (src/server/password.ts). */
export type PasswordAnswer =
  // sign-in and sign-up: signed in (the page it started from), the code
  // WorkOS emailed (/login/verify), the company's single sign-on, or the
  // sign-in page saying why it ended.
  | { redirectTo: string }
  // forgot: WorkOS emails a link, if the address has an account.
  | { sent: true }
  // reset: the new password is set.
  | { reset: true }
  | {
      error: "credentials" | "exists" | "denied" | "unavailable";
    }
  // The WFE's own reason, as a sentence: WorkOS's password rules, a reset
  // link that ran out.
  | { error: "refused"; message: string };

type Action = "sign-in" | "sign-up" | "forgot" | "reset";
type Refusal = Extract<PasswordAnswer, { error: string }>;

const refusals: Record<Exclude<Refusal["error"], "refused">, string> = {
  credentials:
    "That email and password don’t match. Try again, or reset your password.",
  exists:
    "An account already uses this email. Sign in instead, or reset its password.",
  denied:
    "This account can’t sign in here. Sign in with your work account, or ask us for access.",
  unavailable: "Helix couldn’t reach sign-in. Try again in a moment.",
};
const refusalText = (r: Refusal) =>
  r.error === "refused" ? r.message : refusals[r.error];

async function post(action: Action, body: object): Promise<PasswordAnswer> {
  try {
    const res = await fetch(`/auth/password/${action}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const answer = (await res.json()) as PasswordAnswer;
    return answer && typeof answer === "object"
      ? answer
      : { error: "unavailable" };
  } catch {
    return { error: "unavailable" };
  }
}

/**
 * A form's state: what it sends, pending while it does, and what the WFE
 * refused. `send` follows a `redirectTo` itself and answers null then.
 */
function usePasswordForm(action: Action) {
  const [pending, setPending] = useState(false),
    [refused, setRefused] = useState<Refusal | null>(null);
  const send = async (body: object): Promise<PasswordAnswer | null> => {
    setPending(true);
    setRefused(null);
    const answer = await post(action, body);
    if ("redirectTo" in answer && typeof answer.redirectTo === "string") {
      // A full page load: the new session cookie, or the company's IdP.
      window.location.assign(answer.redirectTo);
      return null;
    }
    setPending(false);
    if ("error" in answer) {
      trackEvent("password_refused", { form: action, reason: answer.error });
      setRefused(answer);
      return null;
    }
    return answer;
  };
  return { pending, refused, clear: () => setRefused(null), send };
}

function RefusalText({ refused }: { refused: Refusal | null }) {
  if (!refused) return null;
  return (
    <p id="password-error" role="alert" className="hf-form-error">
      {refusalText(refused)}
    </p>
  );
}

/** The sign-in page's email and password form, under the providers. */
export function PasswordSignIn({
  returnTo,
  disabled,
}: {
  returnTo: string | null;
  disabled: boolean;
}) {
  const [email, setEmail] = useState(""),
    [password, setPassword] = useState("");
  const form = usePasswordForm("sign-in");
  const describedBy = form.refused ? "password-error" : undefined;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    await signInStarted("password");
    await form.send({ email, password, returnTo });
  };
  const q = new URLSearchParams();
  if (returnTo) q.set("return_to", returnTo);
  const forgot = new URLSearchParams(email.includes("@") ? { email } : {});
  return (
    <>
      <p className="hf-gate-or" aria-hidden>
        <span>or</span>
      </p>
      <form
        className="hf-gate-body hf-gate-form"
        aria-label="Sign in with email"
        onSubmit={(e) => void submit(e)}
      >
        <label className="hf-field">
          <span>Email</span>
          <input
            name="email"
            type="email"
            required
            autoComplete="username"
            spellCheck={false}
            maxLength={320}
            value={email}
            aria-describedby={describedBy}
            onChange={(e) => {
              setEmail(e.target.value);
              form.clear();
            }}
          />
        </label>
        <label className="hf-field">
          <span>Password</span>
          <input
            name="password"
            type="password"
            required
            autoComplete="current-password"
            maxLength={1024}
            value={password}
            aria-invalid={form.refused?.error === "credentials" || undefined}
            aria-describedby={describedBy}
            onChange={(e) => {
              setPassword(e.target.value);
              form.clear();
            }}
          />
        </label>
        <RefusalText refused={form.refused} />
        <Button
          type="submit"
          variant="primary"
          size="lg"
          pending={form.pending}
          disabled={disabled || !email.trim() || !password}
        >
          Sign in
        </Button>
      </form>
      <p className="hf-gate-fine">
        No account? <Link to={`/login/sign-up?${q}`}>Create one</Link>.{" "}
        <Link to={`/login/forgot?${forgot}`}>Forgot your password?</Link>
      </p>
    </>
  );
}

/** /login/sign-up: a new account with an email and password. */
export function SignUpPage() {
  const [params] = useSearchParams(),
    returnTo = parseReturnPath(params.get("return_to"));
  const [name, setName] = useState(""),
    [email, setEmail] = useState(""),
    [password, setPassword] = useState("");
  const form = usePasswordForm("sign-up");
  const me = useQuery(whoamiQuery);
  if (me.data) return <Navigate to={returnTo || "/"} replace />;
  const describedBy = form.refused ? "password-error" : undefined;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    await signInStarted("password_sign_up");
    await form.send({ name, email, password, returnTo });
  };
  const q = new URLSearchParams();
  if (returnTo) q.set("return_to", returnTo);
  return (
    <GatePage
      title="Create your Helix account"
      lede="One account for Helix Foundry and the rest of Helix."
    >
      <form className="hf-gate-body" onSubmit={(e) => void submit(e)}>
        <label className="hf-field">
          <span>Name</span>
          <input
            name="name"
            required
            autoFocus
            autoComplete="name"
            maxLength={128}
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              form.clear();
            }}
          />
        </label>
        <label className="hf-field">
          <span>Email</span>
          <input
            name="email"
            type="email"
            required
            autoComplete="email"
            spellCheck={false}
            maxLength={320}
            value={email}
            aria-invalid={form.refused?.error === "exists" || undefined}
            aria-describedby={describedBy}
            onChange={(e) => {
              setEmail(e.target.value);
              form.clear();
            }}
          />
        </label>
        <label className="hf-field">
          <span>Password</span>
          <input
            name="password"
            type="password"
            required
            autoComplete="new-password"
            maxLength={1024}
            value={password}
            aria-invalid={form.refused?.error === "refused" || undefined}
            aria-describedby={describedBy}
            onChange={(e) => {
              setPassword(e.target.value);
              form.clear();
            }}
          />
        </label>
        <RefusalText refused={form.refused} />
        <Button
          type="submit"
          variant="primary"
          size="lg"
          pending={form.pending}
          disabled={!name.trim() || !email.trim() || !password}
        >
          Create account
        </Button>
      </form>
      <p className="hf-gate-fine">
        Have an account? <Link to={`/login?${q}`}>Sign in</Link>, or continue
        with Google, GitHub or Microsoft there.
      </p>
      <Terms />
    </GatePage>
  );
}

/** /login/forgot?email=: asks WorkOS to email a link to /reset-password. */
export function ForgotPasswordPage() {
  const [params] = useSearchParams();
  const [email, setEmail] = useState(params.get("email") ?? ""),
    [sentTo, setSentTo] = useState<string | null>(null);
  const form = usePasswordForm("forgot");
  if (sentTo !== null)
    return (
      <GatePage
        title="Check your email"
        lede={
          <>
            If an account uses <strong data-replay-block>{sentTo}</strong>,
            we’ve emailed it a link to choose a new password.
          </>
        }
      >
        <p className="hf-gate-fine">
          No email? Check your spam folder, or{" "}
          <button
            type="button"
            className="hf-gate-link"
            onClick={() => setSentTo(null)}
          >
            send another
          </button>
          . <Link to="/login">Back to sign in</Link>
        </p>
      </GatePage>
    );
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const answer = await form.send({ email });
    if (answer && "sent" in answer) {
      trackEvent("password_reset_requested");
      setSentTo(email.trim());
    }
  };
  return (
    <GatePage
      title="Reset your password"
      lede="We’ll email you a link to choose a new one."
    >
      <form className="hf-gate-body" onSubmit={(e) => void submit(e)}>
        <label className="hf-field">
          <span>Email</span>
          <input
            name="email"
            type="email"
            required
            autoFocus
            autoComplete="email"
            spellCheck={false}
            maxLength={320}
            value={email}
            aria-describedby={form.refused ? "password-error" : undefined}
            onChange={(e) => {
              setEmail(e.target.value);
              form.clear();
            }}
          />
        </label>
        <RefusalText refused={form.refused} />
        <Button
          type="submit"
          variant="primary"
          size="lg"
          pending={form.pending}
          disabled={!email.trim()}
        >
          Email me a link
        </Button>
      </form>
      <p className="hf-gate-fine">
        <Link to="/login">Back to sign in</Link>
      </p>
    </GatePage>
  );
}

/** /reset-password?token=: the link WorkOS emailed, to choose a new password. */
export function ResetPasswordPage() {
  const [params] = useSearchParams(),
    token = params.get("token") ?? "";
  const [password, setPassword] = useState(""),
    [done, setDone] = useState(false);
  const form = usePasswordForm("reset");
  if (!token)
    return (
      <GatePage
        title="This link is incomplete"
        lede="Open the link from the email again, or ask for a new one."
      >
        <div className="hf-gate-body">
          <ButtonLink to="/login/forgot" variant="primary" size="lg">
            Email me a new link
          </ButtonLink>
        </div>
      </GatePage>
    );
  if (done)
    return (
      <GatePage
        title="Password changed"
        lede="Sign in with your email and new password."
      >
        <div className="hf-gate-body">
          <ButtonLink to="/login" variant="primary" size="lg">
            Sign in
          </ButtonLink>
        </div>
      </GatePage>
    );
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const answer = await form.send({ token, password });
    if (answer && "reset" in answer) {
      trackEvent("password_reset");
      setDone(true);
    }
  };
  return (
    <GatePage title="Choose a new password">
      <form className="hf-gate-body" onSubmit={(e) => void submit(e)}>
        <label className="hf-field">
          <span>New password</span>
          <input
            name="password"
            type="password"
            required
            autoFocus
            autoComplete="new-password"
            maxLength={1024}
            value={password}
            aria-invalid={!!form.refused || undefined}
            aria-describedby={form.refused ? "password-error" : undefined}
            onChange={(e) => {
              setPassword(e.target.value);
              form.clear();
            }}
          />
        </label>
        <RefusalText refused={form.refused} />
        <Button
          type="submit"
          variant="primary"
          size="lg"
          pending={form.pending}
          disabled={!password}
        >
          Change password
        </Button>
      </form>
      <p className="hf-gate-fine">
        Link ran out? <Link to="/login/forgot">Email me a new one</Link>.
      </p>
    </GatePage>
  );
}
