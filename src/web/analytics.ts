import { track } from "@databuddy/sdk";
import type {
  ConnectorKindName,
  DocumentMediaType,
  FoundryAiProviderKind,
  LoadStatus,
  OntologyGenerationStatus,
  QuestionStatus,
} from "../shared/wfe";
import type { PaidTier, Tier } from "./areas/workspace/plan";

// The hosted site's product analytics, in Databuddy (./telemetry.tsx loads
// its tracker). next.config.ts sets the stamp's client id only in a build
// for the stamp's own origin, so a local build, the mock, e2e and `pnpm dev`
// have none: they load no tracker, and nothing here sends.
export const DATABUDDY_CLIENT_ID =
  process.env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID || "";

/** Where a generation's draft comes from: the ontology agent, or the sample's ready-made one. */
export type GenerationSource = "ai" | "sample";

/**
 * Foundry's custom events, in one place so their names stay stable in
 * Databuddy, and named as helix-db.com's are (helix-website-landing's
 * src/lib/analytics.ts) where they mean the same. Goals and funnels match
 * event names only, so every step of the funnel is an event of its own.
 * Properties are a fixed vocabulary: never a person's, workspace's or
 * resource's name or id, an email, a question, a goal or a file name.
 */
export type AnalyticsEvents = {
  // Sign-in (src/web/sign-in.tsx, main.tsx).
  sign_in_started: { method: "github" | "google" | "microsoft" };
  login_completed: { destination: "workspace" | "onboarding" };
  workspace_created: undefined;
  // A workspace with no ontology shows only the new-ontology wizard.
  onboarding_shown: undefined;

  // The sample company: Use the sample, then its tour reaching each step.
  sample_clicked: undefined;
  sample_data_ready: undefined;
  sample_ontology_ready: undefined;
  sample_loaded: undefined;
  sample_ask_opened: undefined;

  // The workspace's own data.
  connector_created: { kind: ConnectorKindName };
  document_uploaded: { format: DocumentMediaType };

  // Jobs: reported when they start and, by whichever read of them sees it
  // first, when they end (jobStarted, jobSeen).
  generation_started: { source: GenerationSource };
  generation_succeeded: { source: GenerationSource };
  generation_failed: { source: GenerationSource };
  load_started: undefined;
  load_ready: undefined;
  question_asked: undefined;
  question_answered: undefined;

  // Settings.
  ai_provider_saved: { kind: FoundryAiProviderKind | "default" };
  upgrade_clicked: { tier: PaidTier; from: Tier };
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
  if (!DATABUDDY_CLIENT_ID) return;
  track(name, properties);
}

// ---------------------------------------------------------------- sign-in

// sessionStorage outlives the sign-in's trip through WorkOS in the same tab.
const SIGNING_IN = "foundry.analytics.signing-in";

/** The sign-in page's provider button: remembered until the user is signed in. */
export function signInStarted(
  method: AnalyticsEvents["sign_in_started"]["method"],
) {
  trackEvent("sign_in_started", { method });
  try {
    sessionStorage.setItem(SIGNING_IN, "1");
  } catch {
    // Storage blocked: login_completed goes unreported.
  }
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

// ---------------------------------------------------------------- jobs

type Job = "generation" | "load" | "question";

type Status = {
  generation: OntologyGenerationStatus;
  load: LoadStatus;
  question: QuestionStatus;
};

/** A job this browser started, until a read of it sees it end. */
type Pending = { job: Job; source?: GenerationSource; at: number };

// Read back from storage, so `source` is checked, not trusted.
const sourceOf = (p: Pending): GenerationSource =>
  p.source === "sample" ? "sample" : "ai";

/**
 * What each job's statuses report once it has ended: an event, or nothing
 * (null). A status not listed is a job still running.
 */
const ENDS: {
  [J in Job]: Partial<Record<Status[J], ((p: Pending) => void) | null>>;
} = {
  generation: {
    ONTOLOGY_GENERATION_STATUS_SUCCEEDED: (p) =>
      trackEvent("generation_succeeded", { source: sourceOf(p) }),
    ONTOLOGY_GENERATION_STATUS_FAILED: (p) =>
      trackEvent("generation_failed", { source: sourceOf(p) }),
  },
  load: {
    ready: () => trackEvent("load_ready"),
    partial: () => trackEvent("load_ready"),
    failed: null,
  },
  question: {
    answered: () => trackEvent("question_answered"),
    cannot_answer: null,
    failed: null,
    cancelled: null,
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
    Object.entries(pending).filter(([, p]) => now - p.at < KEEP_MS),
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
 * A job started here: reports `<job>_started` once per job (a retry with the
 * same client token returns the same one) and remembers it for jobSeen.
 * Nothing is kept without a client id.
 */
export function jobStarted(
  job: "generation",
  id: string | undefined,
  source: GenerationSource,
): void;
export function jobStarted(
  job: "load" | "question",
  id: string | undefined,
): void;
export function jobStarted(
  job: Job,
  id: string | undefined,
  source?: GenerationSource,
) {
  if (!DATABUDDY_CLIENT_ID || !id) return;
  const pending = readPending();
  if (pending[id]) return;
  writePending({ ...pending, [id]: { job, source, at: Date.now() } });
  if (job === "generation")
    trackEvent("generation_started", { source: source! });
  else trackEvent(job === "load" ? "load_started" : "question_asked");
}

/**
 * A read of jobs: reports how each one this browser started has ended, the
 * first time a read sees it, and forgets it.
 */
export function jobSeen<J extends Job>(
  job: J,
  seen: readonly ({ id?: string; status?: Status[J] } | null | undefined)[],
): void {
  if (!DATABUDDY_CLIENT_ID) return;
  const pending = readPending();
  const ends = ENDS[job] as Partial<
    Record<Status[J], ((p: Pending) => void) | null>
  >;
  const ended = seen.filter(
    (s): s is { id: string; status: Status[J] } =>
      !!s?.id && pending[s.id]?.job === job && !!s.status && s.status in ends,
  );
  if (!ended.length) return;
  for (const s of ended) {
    ends[s.status]?.(pending[s.id]);
    delete pending[s.id];
  }
  writePending(pending);
}
