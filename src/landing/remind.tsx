"use client";

import { useState, type FormEvent } from "react";
import { CONSENT, CONSENT_TEXT, REMIND_PATH } from "@/shared/reminders";
import { trackEvent } from "@/web/analytics";
import {
  browserTimeZone,
  FAILURE_TEXT,
  whenText,
  type RemindFailure,
} from "./remind-text";

type State =
  | { kind: "idle" }
  | { kind: "sending" }
  | { kind: "sent"; text: string }
  | { kind: "failed"; reason: RemindFailure };

// The hero's hand-off on a phone: an email, and Foundry's backend reminds
// them to try it on a computer, the first at the next weekday 9am in the
// browser's time zone (src/shared/reminders.ts). The consent shows once
// something is typed. Session replay leaves the form out (data-replay-block),
// and analytics hears only that a reminder was set up, or why it wasn't.
export function Remind() {
  const [email, setEmail] = useState("");
  const [state, setState] = useState<State>({ kind: "idle" });
  const sending = state.kind === "sending";

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (sending) return;
    const timeZone = browserTimeZone();
    setState({ kind: "sending" });
    let reason: RemindFailure = "error";
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
    } catch {
      // The network failed: say so as any other failure.
    }
    trackEvent("reminder_refused", { reason });
    setState({ kind: "failed", reason });
  };

  return (
    <form
      className="remind"
      onSubmit={submit}
      aria-labelledby="remind-title"
      data-replay-block
    >
      <p className="m" id="remind-title">
        Try it on your computer
      </p>
      <p className="remind-lead">
        Foundry is built for a bigger screen. Leave your email and we&#8217;ll
        remind you when you&#8217;re at your desk.
      </p>
      {state.kind !== "sent" && (
        <>
          <div className="remind-row">
            <input
              type="email"
              name="email"
              autoComplete="email"
              inputMode="email"
              autoCapitalize="none"
              spellCheck={false}
              enterKeyHint="send"
              required
              maxLength={254}
              placeholder="you@company.com"
              aria-label="Your email"
              aria-describedby="remind-consent"
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
          <p id="remind-consent" className="remind-note" hidden={!email.trim()}>
            {CONSENT_TEXT}
          </p>
        </>
      )}
      <p
        className="remind-status"
        role="status"
        data-failed={state.kind === "failed" || undefined}
      >
        {state.kind === "sent"
          ? state.text
          : state.kind === "failed"
            ? FAILURE_TEXT[state.reason]
            : ""}
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
