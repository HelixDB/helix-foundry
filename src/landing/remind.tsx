"use client";

import { useId, useState, type FormEvent } from "react";
import { CONSENT, CONSENT_TEXT, REMIND_PATH } from "@/shared/reminders";
import { trackEvent } from "@/web/analytics";
import {
  browserTimeZone,
  failureText,
  whenText,
  type RemindFailure,
} from "./remind-text";

type State =
  | { kind: "idle" }
  | { kind: "sending" }
  | { kind: "sent"; text: string }
  | { kind: "failed"; reason: RemindFailure; text: string };

// The hero's hand-off on a phone: an email, and Foundry's backend reminds
// them to try it on a computer, the first at the next weekday 9am in the
// browser's time zone (src/shared/reminders.ts). The consent shows once
// something is typed. Session replay leaves the form out (data-replay-block),
// and analytics hears only that a reminder was set up, or why it wasn't.
// The page shows it twice on a phone (the hero, and again above the footer),
// so its ids are its own.
export function Remind() {
  const id = useId();
  const [email, setEmail] = useState("");
  const [state, setState] = useState<State>({ kind: "idle" });
  const sending = state.kind === "sending";

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (sending) return;
    const timeZone = browserTimeZone();
    setState({ kind: "sending" });
    let reason: RemindFailure = "error";
    let retryAfter: string | null = null;
    try {
      const res = await fetch(REMIND_PATH, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          email: email.trim(),
          timeZone,
          consent: CONSENT,
        }),
      });
      const body: unknown = await res.json().catch(() => null);
      const answer = (body ?? {}) as Record<string, unknown>;
      const at =
        res.ok && typeof answer.nextSendAt === "string"
          ? new Date(answer.nextSendAt)
          : null;
      if (at && !Number.isNaN(at.getTime())) {
        trackEvent("reminder_requested", { again: answer.again === true });
        setState({
          kind: "sent",
          text: `We’ll email you ${whenText(at, timeZone, new Date())}. Open it on your computer to start.`,
        });
        return;
      }
      if (answer.error === "invalid" || answer.error === "rate_limit")
        reason = answer.error;
      retryAfter = res.headers.get("Retry-After");
    } catch {
      // The network failed: say so as any other failure.
    }
    trackEvent("reminder_refused", { reason });
    setState({ kind: "failed", reason, text: failureText(reason, retryAfter) });
  };

  return (
    <form
      className="remind"
      onSubmit={submit}
      aria-labelledby={`${id}-title`}
      data-replay-block
    >
      <p className="m" id={`${id}-title`}>
        Try it on your computer
      </p>
      <p className="remind-lead">
        Foundry is built for a bigger screen. Leave your email and we&#8217;ll
        remind you when you&#8217;re at your desk.
      </p>
      {state.kind !== "sent" && (
        <>
          {/* What every platform's autofill reads: the type, name and
              autocomplete token, which iOS Safari's suggestions, Android's
              autofill (Chrome, Samsung Internet and the apps' in-app
              browsers) and password managers fill from, and a label tied to
              the field by its id, which managers that guess from labels
              read. The keyboard is the email one, with no capital or
              correction. */}
          <label className="visually-hidden" htmlFor={`${id}-email`}>
            Your email
          </label>
          <div className="remind-row">
            <input
              id={`${id}-email`}
              type="email"
              name="email"
              autoComplete="email"
              inputMode="email"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              enterKeyHint="send"
              required
              maxLength={254}
              placeholder="you@company.com"
              aria-describedby={`${id}-consent`}
              value={email}
              onChange={(event) => {
                setEmail(event.target.value);
                if (state.kind === "failed") setState({ kind: "idle" });
              }}
            />
            <button className="btn btn-solid" type="submit" disabled={sending}>
              {sending ? "Sending" : "Remind me"}
            </button>
          </div>
          <p
            id={`${id}-consent`}
            className="remind-note"
            hidden={!email.trim()}
          >
            {CONSENT_TEXT}
          </p>
        </>
      )}
      <p
        className="remind-status"
        role="status"
        data-failed={state.kind === "failed" || undefined}
      >
        {state.kind === "sent" || state.kind === "failed" ? state.text : ""}
      </p>
      {state.kind === "sent" && (
        <button
          type="button"
          className="remind-again"
          onClick={() => {
            setEmail("");
            setState({ kind: "idle" });
          }}
        >
          Use another address
        </button>
      )}
    </form>
  );
}
