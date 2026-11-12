// The landing page's reminders, shared by its form (src/landing/remind.tsx)
// and the route that takes it (src/app/remind, src/server/reminders.ts): a
// visitor on a phone leaves an email, and Foundry's backend emails them to
// try Foundry on a computer (the foundry-events door's POST /v1/reminders).

/** Where the form posts: `{email, timeZone, consent}`. */
export const REMIND_PATH = "/remind";

/**
 * The consent the form shows, by version: the backend keeps the version an
 * address agreed to. A change to the words is a new version.
 */
export const CONSENT = "remind-v1";
export const CONSENT_TEXT =
  "By sending, you’re agreeing for us to use your email to remind you about Foundry.";

/** What the route answers: when the first reminder goes, or why it won't. */
export type RemindAnswer =
  | { nextSendAt: string; again: boolean }
  | { error: "invalid" | "rate_limit" | "error" };
