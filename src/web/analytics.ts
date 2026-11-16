import {
  clearProfile,
  getProfileId,
  getTracker,
  identify,
  isTrackerAvailable,
  setGlobalProperties,
  track,
  trackError,
} from "@databuddy/sdk";
import type {
  AgentRuntime,
  FoundryAgentTurnStatus,
  CannotAnswerKind,
  ConnectorKindName,
  ConnectorSyncStatus,
  DocumentMediaType,
  FoundryAiProviderKind,
  FoundryFileReadState,
  JobStatus,
  LoadStatus,
  OntologyGenerationStatus,
  QuestionStatus,
} from "../shared/wfe";
import { cannotAnswerKind } from "../shared/foundry-ask";
import { LOCAL } from "../shared/deployment";
import type { WizardStepId } from "./areas/ontology/model";
import type { AgentOperation } from "./areas/agent/model";
import type { TemplateId } from "./areas/automations/model";
import type { SectionId } from "./shell/types";
import type { PaidTier, Tier } from "./areas/workspace/plan";
import type { FeedbackKind } from "./feedback";
import type { SignInError } from "./sign-in";
import type { ShellWorkspace, WorkspaceRole } from "./ui";
import type { SignInFailureCode } from "./areas/data/sign-ins";
import * as localUsage from "./local-usage";

// Product analytics, in Databuddy. next.config.ts sets the client id only in
// a build for a stamp's own origin (the hosted site, whose pages load the
// tracker: ./telemetry.tsx), or a local install's image ("Foundry Local",
// whose pages post their events to its site: ./local-usage.ts), so the mock,
// e2e and `pnpm dev` have none: nothing here sends.
export const DATABUDDY_CLIENT_ID =
  process.env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID || "";

// Set once ./telemetry.tsx finds this browser may not send (one of Helix's
// own, or a local install whose owner turned it off): nothing is sent or kept
// from then on. Until it knows, calls wait in the SDK's queue (a local
// build's in ./local-usage.ts).
let stopped = false;
const sending = () => !!DATABUDDY_CLIENT_ID && !stopped;

/** This browser sends nothing: stop sending (./telemetry.tsx). */
export function stopSending() {
  stopped = true;
  localUsage.stop();
}

/** Where a generation's draft comes from: the ontology agent, or the sample's ready-made one. */
export type GenerationSource = "ai" | "sample";

/** A revision of a version's edits: a preview (a dry run), or one that saves them as a version. */
export type RevisionMode = "preview" | "save";

/**
 * Where a sync was queued: a connection's page, the Data explorer, the
 * ontology's source picker, an API source's page, the first sync a new
 * connection starts with, the sample's database, or GitHub's return once
 * Helix Foundry's GitHub App is installed.
 */
export type SyncFrom =
  | "connection"
  | "tree"
  | "setup"
  | "api_source"
  | "created"
  | "sample"
  | "install";

/**
 * The section open when the person used the Assistant (shell/nav.ts), none
 * outside them; an action's operation, "other" for one this page doesn't
 * know.
 */
export type AgentSection = SectionId | "none";
export type AgentOperationName = AgentOperation | "other";

/** A dashboard call that may be refused (src/web/areas/dashboards/queries.ts). */
export type DashboardAction =
  | "create"
  | "edit"
  | "delete"
  | "pin"
  | "add_visual"
  | "publish"
  | "unpublish";

/** A workflow (automation) call that may be refused (src/web/areas/automations/queries.ts). */
export type AutomationCall =
  | "create"
  | "edit"
  | "approve"
  | "pause"
  | "resume"
  | "run"
  | "delete"
  | "cancel";

/**
 * How the WFE refused a call (src/web/api.ts refusalOf): too many at once,
 * refused as asked (a 4xx: a cap, a plan, a precondition), or it failed.
 */
export type Refusal = "rate_limit" | "refused" | "error";

/**
 * Foundry's custom events, in one place so their names stay stable in
 * Databuddy, and named as helix-db.com's are (helix-website-landing's
 * src/lib/analytics.ts) where they mean the same. Goals and funnels match
 * event names only, so every step of the funnel is an event of its own.
 * Properties are a fixed vocabulary: never a person's or resource's name or
 * id, an email, a question, a goal or a file name. Every event also carries
 * the workspace open and the signed-in user's email (`globals`).
 */
export type AnalyticsEvents = {
  // Sign-in (src/web/sign-in.tsx, main.tsx).
  sign_in_started: {
    method: "github" | "google" | "microsoft" | "password" | "password_sign_up";
  };
  sign_in_failed: { reason: SignInError };
  // WorkOS emailed a code before it signs the address in (/login/verify):
  // shown, a code refused or not checked, and a right one.
  email_verification_shown: undefined;
  email_verification_failed: { reason: "code" | "unavailable" };
  email_verified: undefined;
  // Email and password (src/web/password.tsx): a form the WFE refused, and a
  // reset link asked for and used.
  password_refused: {
    form: "sign-in" | "sign-up" | "forgot" | "reset";
    reason: "credentials" | "exists" | "denied" | "unavailable" | "refused";
  };
  password_reset_requested: undefined;
  password_reset: undefined;
  login_completed: { destination: "workspace" | "onboarding" };
  workspace_created: undefined;
  // A workspace with no ontology shows only the new-ontology wizard, which
  // first asks how to start: the sample (sample_clicked), the user's own
  // data, or their agent (setup_prompt_copied).
  onboarding_shown: undefined;
  onboarding_own_data_clicked: undefined;
  // The new-ontology wizard's step on screen (a workspace's first ontology
  // of its own, or another), and why creating it failed.
  ontology_step_shown: { step: WizardStepId; first: boolean };
  ontology_create_failed: { reason: "no_subscription" | Refusal };

  // The landing page's copy buttons: the agent's setup prompt, or a local
  // install's (visitors who take them never open sign-in).
  setup_prompt_copied: { setup: "agent" | "local" };
  // The landing page on a phone: a reminder to try Foundry on a computer
  // set up (again: the address already had them), or why it wasn't; then a
  // visit from a reminder's link or the README's (?via=).
  reminder_requested: { again: boolean };
  reminder_refused: { reason: "invalid" | "rate_limit" | "error" };
  landing_returned: { via: "email" | "github" };

  // The sample company: Use the sample, then its tour reaching each step.
  sample_clicked: undefined;
  sample_data_ready: undefined;
  sample_ontology_ready: undefined;
  sample_loaded: undefined;
  sample_ask_opened: undefined;
  // The tour's first question answered, then Connect your own data.
  sample_answered: undefined;
  sample_own_data_clicked: undefined;

  // The workspace's own data.
  connector_test_failed: { kind: ConnectorKindName };
  connector_created: { kind: ConnectorKindName };
  connector_sync_started: { from: SyncFrom };
  connector_synced: { kind: ConnectorKindName | "other" };
  connector_sync_failed: { kind: ConnectorKindName | "other" };
  document_uploaded: { format: DocumentMediaType };
  document_upload_failed: { reason: "refused" | "error" };
  document_read_failed: {
    format?: DocumentMediaType;
    reason: "too_large" | "unsupported" | "error";
  };
  document_moved: undefined;
  document_set_created: undefined;
  // Connecting a source by signing in at the provider: its button, as the
  // page leaves for the provider (to a new connection or signing one in
  // again), or why it couldn't; then how the callback found the sign-in
  // ended, a failure with its code.
  connector_sign_in_started: { kind: ConnectorKindName };
  connector_sign_in_refused: { kind: ConnectorKindName; reason: Refusal };
  connector_sign_in_finished: {
    kind: ConnectorKindName;
    outcome: "connected" | "declined" | "failed" | "expired";
    reason?: SignInFailureCode;
  };

  // Jobs: reported when they start and, by whichever read of them sees it
  // first, when they end (jobStarted, jobSeen); `*_refused` when the WFE
  // won't start one.
  generation_started: { source: GenerationSource };
  generation_succeeded: { source: GenerationSource };
  generation_failed: { source: GenerationSource };
  generation_refused: { reason: Refusal };
  // The agent asked questions only a person answers: the generation waits
  // (seen like an end), then is answered (`with` its answers, some or all
  // of the agent's defaults) or cancelled.
  generation_needs_input: { source: GenerationSource };
  generation_answered: { with: "answers" | "some_defaults" | "defaults" };
  generation_answer_refused: { reason: Refusal };
  generation_cancelled: undefined;
  load_started: undefined;
  load_ready: { status: "ready" | "partial" };
  load_failed: { reason: "budget" | "sources_changed" | "error" };
  load_refused: { reason: Refusal };
  question_asked: undefined;
  question_answered: undefined;
  question_unanswered: {
    status: "cannot_answer" | "failed";
    kind?: CannotAnswerKind;
  };
  question_refused: { reason: Refusal };
  question_cancelled: undefined;
  revision_started: { mode: RevisionMode };
  revision_succeeded: { mode: RevisionMode };
  revision_failed: { mode: RevisionMode };
  revision_refused: { reason: Refusal };

  // The Assistant (src/web/areas/agent): the dock opened, a message's turn
  // started and ended (its runtime, the section it was sent from, how it
  // ended and its tool calls), or why it was refused; an action the agent
  // prepared, then run (how it ended), dismissed or refused, with whether
  // the person named its target. Never the text, a name or a payload.
  agent_dock_opened: { from: "shortcut" | "button" | "palette" };
  agent_turn_started: { section: AgentSection };
  agent_turn_ended: {
    runtime: AgentRuntime;
    section: AgentSection;
    status: Exclude<FoundryAgentTurnStatus, "queued" | "running">;
    steps: number;
  };
  agent_turn_refused: { reason: Refusal };
  agent_action_prepared: {
    operation: AgentOperationName;
    target_named_by_user: boolean;
  };
  agent_action_executed: {
    operation: AgentOperationName;
    outcome: "succeeded" | "failed" | "unknown";
    target_named_by_user: boolean;
  };
  agent_action_dismissed: {
    operation: AgentOperationName;
    target_named_by_user: boolean;
  };
  agent_action_refused: { reason: Refusal };
  // An action that ran at once, without a card's click (a new dashboard the
  // person's message asked for), and how it ended; then the person's Undo.
  agent_action_auto_ran: {
    operation: AgentOperationName;
    outcome: "succeeded" | "failed" | "unknown";
  };
  agent_action_undone: { operation: AgentOperationName };
  // Where a local install's Assistant runs (Settings › AI models): the
  // runtime chosen, a pairing code made, a helper seen paired after one, a
  // helper revoked.
  agent_runtime_set: { runtime: AgentRuntime };
  agent_helper_pairing_made: undefined;
  agent_helper_paired: undefined;
  agent_helper_revoked: undefined;

  // Dashboards (src/web/areas/dashboards): made (from the Dashboards page or
  // an Analyst answer), edited (its tiles, or only its name or
  // description), deleted, pinned and unpinned, given an Analyst answer's
  // visual (reading the current load or the question's), published (the
  // first time, or again for a new snapshot), unpublished, its public link
  // copied, or why a call was refused. Never a name, a title or a query.
  dashboard_created: { from: "page" | "analyst" };
  dashboard_edited: { tiles: boolean };
  dashboard_deleted: undefined;
  dashboard_pinned: undefined;
  dashboard_unpinned: undefined;
  dashboard_visual_added: { load: "current" | "pinned" };
  dashboard_published: { first: boolean };
  dashboard_unpublished: undefined;
  dashboard_link_copied: undefined;
  dashboard_refused: { action: DashboardAction; reason: Refusal };

  // Workflows (src/web/areas/automations; the events keep their automation_
  // names): made (from which template, or
  // the Assistant's card, which agent_action_executed reports), edited (its
  // steps, or only its name or description), approved by its owner,
  // paused, resumed, run by hand, deleted, a run cancelled, or why a call
  // was refused. Never a name, an instruction or a target.
  automation_created: { template: TemplateId };
  automation_edited: { steps: boolean };
  automation_approved: undefined;
  automation_paused: undefined;
  automation_resumed: undefined;
  automation_run_started: undefined;
  automation_deleted: undefined;
  automation_run_cancelled: undefined;
  automation_refused: { action: AutomationCall; reason: Refusal };
  // How much an ontology helped it: its use case opened from the
  // workflow's page, or a run's use case run from the run's.
  automation_ontology_opened: { from: "automation" | "run" };

  // An ontology's versions, loads and reviews.
  version_made_current: undefined;
  load_made_current: undefined;
  review_settled: { kind: "resolve" | "prop" | "release" | "reject" };
  review_decision_removed: undefined;
  graph_exported: undefined;
  // Use cases (the Usage pages; the events keep their workflow_ names): an
  // agent's task, and the prompt that has an agent run it.
  workflow_created: undefined;
  workflow_deleted: undefined;
  workflow_prompt_copied: undefined;

  // Settings and the account.
  ai_provider_saved: { kind: FoundryAiProviderKind | "default" };
  upgrade_clicked: { tier: PaidTier; from: Tier };
  plan_cancelled: undefined;
  plan_resumed: undefined;
  billing_portal_opened: undefined;
  workspace_member_invited: { role: "member" | "admin" };
  people_enrichment_enabled: undefined;
  people_enrichment_disabled: undefined;
  signed_out: undefined;

  // The GitHub star prompt (src/web/shell/star-prompt.tsx): shown, with how
  // its button stars; then starred in place, GitHub opened, or dismissed.
  star_prompt_shown: { via: "in_place" | "on_github" };
  star_prompt_clicked: { outcome: "starred" | "opened_github" };
  star_prompt_dismissed: undefined;

  // Feedback (src/web/feedback.ts): sent from the Feedback button, by its
  // kind, and a connector requested from the Data area. Never its words.
  feedback_sent: { kind: FeedbackKind };
  connector_requested: undefined;

  // Where the app stood in the way: a Pro-only feature (its gate's label,
  // which names the feature, never data), or a page that crashed.
  pro_gate_shown: { feature: string };
  page_crashed: { cause: "stale_build" | "error" };

  // A local install's page views (pageViewed), its path masked: the hosted
  // tracker records its own.
  page_viewed: undefined;
};

export type AnalyticsEventName = keyof AnalyticsEvents;

type EventArgs<Name extends AnalyticsEventName> =
  AnalyticsEvents[Name] extends undefined
    ? []
    : [properties: AnalyticsEvents[Name]];

/**
 * Sends a custom event. Databuddy queues calls made before its tracker loads,
 * so this is safe from any handler; without a client id it does nothing.
 */
export function trackEvent<Name extends AnalyticsEventName>(
  name: Name,
  ...[properties]: EventArgs<Name>
): void {
  // Without a client id the tracker never loads, and queued calls would only
  // expire into console warnings.
  if (!sending()) return;
  // A local page loads no tracker: its site forwards the event, with the
  // global properties the tracker would add.
  if (LOCAL)
    return localUsage.report(
      name,
      { ...globals, ...properties },
      location.pathname,
    );
  track(name, properties);
}

/**
 * The SPA's page changed (main.tsx): a local install's page view, which no
 * tracker records there. The hosted tracker records its own.
 */
export function pageViewed() {
  if (LOCAL) trackEvent("page_viewed");
}

// How long a page about to leave waits for the tracker to load.
const LEAVE_WAIT_MS = 1000;
// Databuddy's custom events API (www.databuddy.cc/docs/api/events).
const TRACK_URL = "https://basket.databuddy.cc/track";

/**
 * Sends an event before the page may leave (for WorkOS, Stripe, a
 * provider's sign-in, or the app from the landing page). The tracker holds custom events for up to 5 s and
 * loses what it still holds when the page unloads (its build of Oct 2026),
 * and a call made before it loads waits in the SDK's queue, which the
 * navigation drops. So this waits for the tracker, at most a second, then
 * sends the event at once with a beacon to the events API, under the
 * running tracker's visitor and session so it joins the same funnel. The
 * tracker has none while the visitor opts out (Do Not Track, Global Privacy
 * Control, databuddyOptOut()), and the SDK would read a stored one before
 * the tracker loads: without the tracker's, or a beacon, the event goes
 * through the tracker, which honours the opt-out. The event's path is the page's,
 * unmasked: only for pages whose path names nothing (else
 * trackBeforeLeavingAt).
 */
export function trackBeforeLeaving<Name extends AnalyticsEventName>(
  name: Name,
  ...properties: EventArgs<Name>
): Promise<void> {
  return trackBeforeLeavingAt(
    typeof location === "undefined" ? "/" : location.pathname,
    name,
    ...properties,
  );
}

/**
 * trackBeforeLeaving, under a fixed `path` in place of the page's: for a
 * button on pages whose paths name ids (a connector's, an ontology's), which
 * a beacon's path wouldn't mask.
 */
export async function trackBeforeLeavingAt<Name extends AnalyticsEventName>(
  path: string,
  name: Name,
  ...properties: EventArgs<Name>
): Promise<void> {
  if (!sending()) return;
  // A local page's event goes to its own site at once, which outlives the
  // page leaving (keepalive).
  if (LOCAL) {
    localUsage.report(name, { ...globals, ...properties[0] }, path);
    return localUsage.flush();
  }
  for (
    const end = Date.now() + LEAVE_WAIT_MS;
    !isTrackerAvailable() && Date.now() < end;

  )
    await new Promise((resolve) => setTimeout(resolve, 50));
  const ids = getTracker()?.getTrackingIds?.();
  const anonymousId = ids?.anonId,
    event = {
      eventId: crypto.randomUUID(),
      name,
      // The tracker merges its global properties (the workspace and the
      // user) into its own events' properties; a beacon has to.
      properties: { ...globals, ...properties[0] },
      anonymousId,
      sessionId: ids?.sessionId,
      // As the tracker's own events: the signed-in user (identifyUser).
      profileId: getProfileId() ?? undefined,
      // As the tracker sends its own: the visitor id is hashed on arrival.
      anonymizeVisitorIds: true,
      path: location.origin + path,
      source: "browser",
      websiteId: DATABUDDY_CLIENT_ID,
    };
  const sent =
    !!anonymousId &&
    typeof navigator.sendBeacon === "function" &&
    navigator.sendBeacon(
      `${TRACK_URL}?website_id=${encodeURIComponent(DATABUDDY_CLIENT_ID)}`,
      // text/plain, as the tracker's beacons: a beacon of JSON would need a
      // preflight it can't make.
      new Blob([JSON.stringify(event)], { type: "text/plain;charset=UTF-8" }),
    );
  if (!sent) trackEvent(name, ...properties);
}

// ---------------------------------------------------------------- sign-in

// sessionStorage outlives the sign-in's trip through WorkOS in the same tab.
const SIGNING_IN = "foundry.analytics.signing-in";

/**
 * The sign-in page's provider button, before the page leaves for WorkOS:
 * remembered until the user is signed in. Resolves once it may leave.
 */
export async function signInStarted(
  method: AnalyticsEvents["sign_in_started"]["method"],
): Promise<void> {
  if (!sending()) return;
  try {
    sessionStorage.setItem(SIGNING_IN, "1");
  } catch {
    // Storage blocked: login_completed goes unreported.
  }
  await trackBeforeLeaving("sign_in_started", { method });
}

/** The app signed in, with its workspaces read: reports a sign-in this tab started, once. */
export function signedIn(
  destination: AnalyticsEvents["login_completed"]["destination"],
) {
  try {
    if (!sessionStorage.getItem(SIGNING_IN)) return;
    sessionStorage.removeItem(SIGNING_IN);
  } catch {
    return;
  }
  trackEvent("login_completed", { destination });
}

// Helix's own people: a browser signed in with a company email, or marked
// by opening any page with ?internal=1 (?internal=0 undoes it), sends
// nothing, so funnels, replays and reports count customers only.
const INTERNAL = "foundry.analytics.internal";
const INTERNAL_DOMAINS = ["helix-db.com"];

/**
 * Whether this browser may load the tracker (./telemetry.tsx): not one of
 * Helix's own. Keeps a ?internal= mark first; undoing one also undoes the
 * tracker's own opt-out, which marking it set (markInternal).
 *
 * @example
 * trackingAllowed("?internal=1"); // false, and on every later page
 * trackingAllowed("?internal=0"); // true
 */
export function trackingAllowed(search: string): boolean {
  const mark = new URLSearchParams(search).get("internal");
  try {
    if (mark === "1") localStorage.setItem(INTERNAL, "1");
    if (mark === "0")
      for (const key of [INTERNAL, "databuddy_opt_out", "databuddy_disabled"])
        localStorage.removeItem(key);
    return localStorage.getItem(INTERNAL) !== "1";
  } catch {
    return true;
  }
}

/** Marks this browser as Helix's own, and stops the tracker and replay running now. */
function markInternal() {
  try {
    localStorage.setItem(INTERNAL, "1");
  } catch {
    // Storage blocked: the next page loads the tracker, and is marked again.
  }
  window.databuddyOptOut?.();
}

/**
 * The signed-in user, by the backend's user id, with their email and name on
 * the profile (Databuddy's own fields, so replays and profiles can be found
 * by email): every event and replay from here on, in any session of this
 * browser, is theirs. Safe on every page load; with details, the tracker
 * sends it on each. One of Helix's own is never identified: their browser
 * is marked and stops sending.
 */
export function identifyUser(
  id: string,
  { email, name }: { email?: string; name?: string } = {},
) {
  // A local install's owner stays anonymous.
  if (!sending() || !id || LOCAL) return;
  const domain = email?.split("@").pop()?.toLowerCase();
  if (domain && INTERNAL_DOMAINS.includes(domain)) return markInternal();
  identify(id, { ...(email && { email }), ...(name && { name }) });
  // Databuddy's queries don't read a profile's email: events carry it, so
  // the team's dashboard can say who did what.
  if (email) setGlobals({ user_email: email });
}

/**
 * What every event and page view carries besides its own properties (the
 * tracker's global properties): the workspace open and the signed-in user's
 * email.
 */
let globals: {
  workspace_id?: string;
  workspace_slug?: string;
  workspace_name?: string;
  workspace_role?: WorkspaceRole;
  // "<slug> <email>" while the open workspace's owner is the one signed in:
  // Databuddy's queries can't pair two properties, so the team's dashboard
  // reads each workspace's owner from this one.
  workspace_owner?: string;
  user_email?: string;
} = {};
const setGlobals = (more: Omit<typeof globals, "workspace_owner">) => {
  const next = { ...globals, ...more };
  next.workspace_owner =
    next.workspace_role === "owner" && next.workspace_slug && next.user_email
      ? `${next.workspace_slug} ${next.user_email}`
      : undefined;
  globals = next;
  // A local page loads no tracker: trackEvent adds them itself.
  if (!LOCAL)
    setGlobalProperties({ ...more, workspace_owner: next.workspace_owner });
};

/**
 * The workspace open (and again on a switch): every event and page view
 * from here on carries its id, slug, name and the user's role in it (the
 * tracker's global properties), so each is the workspace's, and
 * per-account retention counts workspaces (`workspace_id`).
 */
export function workspaceOpened({ id, slug, name, role }: ShellWorkspace) {
  if (!sending()) return;
  // A local install's workspace is its owner's: its id only.
  setGlobals(
    LOCAL
      ? { workspace_id: id }
      : {
          workspace_id: id,
          workspace_slug: slug,
          workspace_name: name,
          workspace_role: role,
        },
  );
}

/** Sign-out, before the page leaves: reports it as the user, then forgets them and their workspace. */
export async function signedOut(): Promise<void> {
  if (!sending()) return;
  await trackBeforeLeaving("signed_out");
  clearProfile();
  globals = {};
  setGlobalProperties({
    workspace_id: undefined,
    workspace_slug: undefined,
    workspace_name: undefined,
    workspace_role: undefined,
    workspace_owner: undefined,
    user_email: undefined,
  });
}

/**
 * A page that crashed (src/web/shell/ErrorBoundary.tsx): the tracker reports
 * uncaught errors itself, but not those a boundary catches.
 */
export function pageCrashed(error: Error, staleBuild: boolean) {
  if (!sending()) return;
  trackEvent("page_crashed", { cause: staleBuild ? "stale_build" : "error" });
  // A local install sends no errors: a message may quote its data.
  if (!LOCAL)
    trackError(error.message, { error_type: error.name, stack: error.stack });
}

// ---------------------------------------------------------------- jobs

/** A job this browser started, by kind, with what its events say of it. */
export type Started =
  | { job: "generation"; source: GenerationSource }
  | { job: "revision"; mode: RevisionMode }
  | { job: "sync"; from: SyncFrom }
  // An uploaded table file or PDF that Foundry reads.
  | { job: "read" }
  | { job: "load" }
  | { job: "question" }
  // A message to the Assistant, from the section it was sent in, and where
  // its thread runs.
  | { job: "agent_turn"; section: AgentSection; runtime: AgentRuntime };
type Job = Started["job"];

/** What a read of each job gives: its status, and what its end reports. */
type Seen = {
  generation: { id?: string; status?: OntologyGenerationStatus };
  revision: { id?: string; status?: OntologyGenerationStatus };
  // A connector's newest sync, with the connector's kind.
  sync: {
    id?: string;
    status?: ConnectorSyncStatus;
    kind: ConnectorKindName | "other";
  };
  // An uploaded file's read, with its format and the code it failed with.
  read: {
    id?: string;
    status?: FoundryFileReadState;
    format?: DocumentMediaType;
    code?: string;
  };
  load: {
    id?: string;
    status?: LoadStatus;
    jobs?: readonly { status?: JobStatus; error?: string }[];
  };
  question: { id?: string; status?: QuestionStatus; resultJson?: string };
  agent_turn: { id?: string; status?: FoundryAgentTurnStatus; steps?: number };
};
type Status = { [J in Job]: NonNullable<Seen[J]["status"]> };

/** A job this browser started, until a read of it sees it end. */
type Pending = Started & { at: number };

// Read back from storage, so `source` and `mode` are checked, not trusted.
const sourceOf = (p: Pending): GenerationSource =>
  "source" in p && p.source === "sample" ? "sample" : "ai";
const modeOf = (p: Pending): RevisionMode =>
  "mode" in p && p.mode === "preview" ? "preview" : "save";
const SECTIONS: readonly AgentSection[] = [
  "data",
  "ontology",
  "explore",
  "ask",
  "settings",
  "none",
];
const sectionOf = (p: Pending): AgentSection =>
  SECTIONS.find((s) => "section" in p && p.section === s) ?? "none";
const RUNTIMES: readonly AgentRuntime[] = ["api", "claude_code", "codex"];
const runtimeOf = (p: Pending): AgentRuntime =>
  RUNTIMES.find((r) => "runtime" in p && p.runtime === r) ?? "api";

/** What a job reports when it starts; a read's upload reports itself. */
function reportStart(s: Started) {
  switch (s.job) {
    case "generation":
      return trackEvent("generation_started", { source: s.source });
    case "revision":
      return trackEvent("revision_started", { mode: s.mode });
    case "sync":
      return trackEvent("connector_sync_started", { from: s.from });
    case "read":
      return;
    case "load":
      return trackEvent("load_started");
    case "question":
      return trackEvent("question_asked");
    case "agent_turn":
      return trackEvent("agent_turn_started", { section: s.section });
  }
}

const agentTurnEnded = (
  p: Pending,
  t: Seen["agent_turn"],
  status: AnalyticsEvents["agent_turn_ended"]["status"],
) =>
  trackEvent("agent_turn_ended", {
    runtime: runtimeOf(p),
    section: sectionOf(p),
    status,
    steps: t.steps || 0,
  });

/**
 * What each job's statuses report once it has ended: an event, or nothing
 * (null). A status not listed is a job still running.
 */
const ENDS: {
  [J in Job]: Partial<
    Record<Status[J], ((p: Pending, seen: Seen[J]) => void) | null>
  >;
} = {
  generation: {
    ONTOLOGY_GENERATION_STATUS_SUCCEEDED: (p) =>
      trackEvent("generation_succeeded", { source: sourceOf(p) }),
    ONTOLOGY_GENERATION_STATUS_FAILED: (p) =>
      trackEvent("generation_failed", { source: sourceOf(p) }),
    // Answering it starts reporting it again (answerGeneration).
    ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT: (p) =>
      trackEvent("generation_needs_input", { source: sourceOf(p) }),
    ONTOLOGY_GENERATION_STATUS_CANCELLED: null,
  },
  revision: {
    ONTOLOGY_GENERATION_STATUS_SUCCEEDED: (p) =>
      trackEvent("revision_succeeded", { mode: modeOf(p) }),
    ONTOLOGY_GENERATION_STATUS_FAILED: (p) =>
      trackEvent("revision_failed", { mode: modeOf(p) }),
    ONTOLOGY_GENERATION_STATUS_CANCELLED: null,
  },
  sync: {
    CONNECTOR_SYNC_STATUS_SUCCEEDED: (_, s) =>
      trackEvent("connector_synced", { kind: s.kind }),
    CONNECTOR_SYNC_STATUS_FAILED: (_, s) =>
      trackEvent("connector_sync_failed", { kind: s.kind }),
  },
  read: {
    FOUNDRY_FILE_READ_STATE_READY: null,
    FOUNDRY_FILE_READ_STATE_FAILED: (_, s) =>
      trackEvent("document_read_failed", {
        ...(s.format && { format: s.format }),
        reason:
          s.code === "too_large" || s.code === "unsupported" ? s.code : "error",
      }),
  },
  load: {
    ready: () => trackEvent("load_ready", { status: "ready" }),
    partial: () => trackEvent("load_ready", { status: "partial" }),
    // Its newest job says why: a spending cap, its tables synced again
    // while it ran (the WFE's words, as loads.tsx reads them), or else.
    failed: (_, l) => {
      const job = l.jobs?.[0];
      trackEvent("load_failed", {
        reason:
          job?.status === "budget_exhausted"
            ? "budget"
            : /synced again while this load was written/i.test(job?.error || "")
              ? "sources_changed"
              : "error",
      });
    },
  },
  question: {
    answered: () => trackEvent("question_answered"),
    cannot_answer: (_, q) => {
      let kind: CannotAnswerKind | undefined;
      try {
        const result: unknown = JSON.parse(q.resultJson || "");
        if (
          result &&
          typeof result === "object" &&
          "status" in result &&
          result.status === "cannot_answer" &&
          "kind" in result
        )
          kind = cannotAnswerKind(result.kind);
      } catch {
        // Older or unreadable results carry no classification.
      }
      trackEvent("question_unanswered", {
        status: "cannot_answer",
        ...(kind ? { kind } : {}),
      });
    },
    failed: () => trackEvent("question_unanswered", { status: "failed" }),
    cancelled: null,
  },
  agent_turn: {
    done: (p, t) => agentTurnEnded(p, t, "done"),
    failed: (p, t) => agentTurnEnded(p, t, "failed"),
    cancelled: (p, t) => agentTurnEnded(p, t, "cancelled"),
  },
};

// localStorage, so a job that ends after a reload, or in another tab, is
// still reported once. Entries a week old are dropped unreported: a job
// never seen to end (deleted, or never opened again) mustn't pile up.
const PENDING = "foundry.analytics.pending.v1";
const KEEP_MS = 7 * 24 * 60 * 60 * 1000;

function readPending(): Record<string, Pending> {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(PENDING) || "{}");
    return value && typeof value === "object"
      ? (value as Record<string, Pending>)
      : {};
  } catch {
    return {};
  }
}

function writePending(pending: Record<string, Pending>) {
  const now = Date.now();
  const kept = Object.fromEntries(
    Object.entries(pending).filter(
      ([, p]) => typeof p?.at === "number" && now - p.at < KEEP_MS,
    ),
  );
  try {
    if (Object.keys(kept).length)
      localStorage.setItem(PENDING, JSON.stringify(kept));
    else localStorage.removeItem(PENDING);
  } catch {
    // Storage blocked or full: its end goes unreported.
  }
}

/**
 * A job started here: reports its start once per job (a retry with the same
 * client token returns the same one) and remembers it for jobSeen. A job
 * carried on (`report` false: a generation answered) is only remembered.
 * Nothing is kept without a client id.
 */
export function jobStarted(
  started: Started,
  id: string | undefined,
  report = true,
) {
  if (!sending() || !id) return;
  const pending = readPending();
  if (pending[id]) return;
  writePending({ ...pending, [id]: { ...started, at: Date.now() } });
  if (report) reportStart(started);
}

/**
 * A read of jobs: reports how each one this browser started has ended, the
 * first time a read sees it, and forgets it.
 */
export function jobSeen<J extends Job>(
  job: J,
  seen: readonly (Seen[J] | null | undefined)[],
): void {
  if (!sending()) return;
  const pending = readPending();
  const ends = ENDS[job] as Partial<
    Record<Status[J], ((p: Pending, seen: Seen[J]) => void) | null>
  >;
  const ended = seen.filter(
    (s): s is Seen[J] & { id: string; status: Status[J] } =>
      !!s?.id && pending[s.id]?.job === job && !!s.status && s.status in ends,
  );
  if (!ended.length) return;
  for (const s of ended) {
    ends[s.status]?.(pending[s.id]!, s);
    delete pending[s.id];
  }
  writePending(pending);
}
