import type {
  AgentEnrichmentClues,
  FoundryEnrichment,
  FoundryEnrichmentResponse,
  LoadResponse,
  UpdateFoundryEnrichmentRequest,
} from "../../src/shared/wfe";
import { CREDITS_PER_PROFILE } from "../../src/web/areas/ontology/enrichment-fields";
import {
  normIdentifier,
  type Experience,
  type ProfileRow,
} from "../graph/extract";
import { fail, now, ok, type MockRoute } from "../http";
import { config, isReply, membership, onReset } from "../state";
import { isFoundryPro, proRequired } from "./billing";
import {
  cancelEnrichment,
  ontologyHasProfiles,
  purgeLoaded,
  queuePurgeLoad,
  stopLoadEnrichment,
} from "./loads";
import { isScope, ontologyOf, workspaceOntologies } from "./ontologies";

// People enrichment as the backend serves it,
// in memory: the owner's consent and monthly credit cap, kept as history
// rows; the credits ledger (charges by load, reservations of running loads);
// the results a load published, which the next load of the ontology carries
// forward; and fakeEnrich, the fake context.dev, which decides by the clue's
// text:
// - "bad": no match;
// - "low": a match scoring 72, held (low_score);
// - "dup": a profile another person matches too, held at publish
//   (duplicate_profile);
// - "other": someone else's profile, held (name_mismatch);
// - "timeout": no answer (uncertain, charged);
// - anything else: a match scoring 96, accepted.
// Free-mail and role addresses never reach it: the planner skips them.
//
// Lookups need the operators' flag, consent and Foundry Pro. Knobs, through
// POST /__mock/config: `enrichmentStepMs` (one batch of lookups, and the
// wait for a slot), `enrichmentOutcome`: "cap" (the credit cap stops the
// lookups after the first batch), "provider_credits" and "provider"
// (context.dev stops after the first batch), "time" (the time limit),
// "busy" (no lookup slot: the load applies what was found before); and
// `enrichmentFlag: false`, the operators' flag off: the setting reads
// DISABLED and can't be turned on (off still works), loads look no one up
// (partial(flag), applying what was found before), Revise refuses
// enrichment edits and the agent never proposes it.
//
// Turning consent off cancels live steps, and each ontology with a load that
// applied profiles (current or not, a load still writing included) gets a
// purge: once no job is live, a current load holding them is loaded again
// without profiles, and the others holding them are deleted once a clean
// load is current. A purged load can't be made current.

type Knobs = {
  enrichmentStepMs?: number;
  enrichmentOutcome?: "cap" | "provider_credits" | "provider" | "time" | "busy";
  enrichmentFlag?: boolean;
};
export const enrichmentKnobs = config as typeof config & Knobs;
/** Whether the operators' flag `foundry.enrichment.enabled` is on (by default). */
export const enrichmentFlagOn = () => enrichmentKnobs.enrichmentFlag !== false;
/** The WFE's refusal of what needs the flag while it is off. */
export const enrichmentUnavailable = () =>
  fail("FAILED_PRECONDITION", "People enrichment isn't available");

const CAP_MAX = 100_000;
// The longest notice version a consent row keeps.
const NOTICE_MAX = 32;
// A held or accepted result is carried; no match only for 90 days.
const NOT_FOUND_DAYS = 90;
const FREE_MAIL = new Set([
  "gmail.com",
  "googlemail.com",
  "yahoo.com",
  "hotmail.com",
  "outlook.com",
  "live.com",
  "icloud.com",
  "me.com",
  "aol.com",
  "proton.me",
  "protonmail.com",
  "gmx.com",
]);
const ROLE_MAILBOXES = new Set([
  "sales",
  "info",
  "support",
  "team",
  "billing",
  "admin",
  "hello",
  "contact",
  "noreply",
  "ap",
  "finance",
  "hr",
  "jobs",
  "office",
]);

type Consent = {
  workspaceId: string;
  enabled: boolean;
  cap: number;
  noticeVersion: string;
  userId: string;
  at: string;
};
type Charge = {
  workspaceId: string;
  loadId: string;
  credits: number;
  at: number;
};
// An ontology whose loads applied profiles when consent was turned off.
type Purge = { workspaceId: string; ontologyId: string };

/** What a lookup came to. */
export type Outcome = "accepted" | "held" | "not_found" | "uncertain";
/** One person's result in an ontology's newest results file. */
export type Result = {
  // The entity's key, for the load that looked it up.
  key: string;
  sent: { email?: string; slug?: string };
  outcome: Outcome;
  reason?: string;
  profile?: ProfileRow;
  checkedAt: string;
};

const state = {
  consents: [] as Consent[],
  charges: [] as Charge[],
  // Credits a running load holds back, by load.
  reserved: new Map<string, { workspaceId: string; credits: number }>(),
  purges: [] as Purge[],
  // The newest results each ontology published.
  results: new Map<string, Result[]>(),
};
onReset(() => {
  state.consents = [];
  state.charges = [];
  state.reserved.clear();
  state.purges = [];
  state.results.clear();
  delete enrichmentKnobs.enrichmentStepMs;
  delete enrichmentKnobs.enrichmentOutcome;
  delete enrichmentKnobs.enrichmentFlag;
});

// ---------------------------------------------------------------- Consent and credits

const consentOf = (workspaceId: string) =>
  state.consents.findLast((c) => c.workspaceId === workspaceId);

/** Whether the workspace's owner allows People enrichment. */
export const enrichmentAllowed = (workspaceId: string) =>
  consentOf(workspaceId)?.enabled === true;

/** Credits charged in the UTC month of `at`. */
export function creditsUsed(workspaceId: string, at = Date.now()) {
  const month = new Date(at).toISOString().slice(0, 7);
  return state.charges
    .filter(
      (c) =>
        c.workspaceId === workspaceId &&
        new Date(c.at).toISOString().slice(0, 7) === month,
    )
    .reduce((n, c) => n + c.credits, 0);
}
const creditsReserved = (workspaceId: string, except = "") =>
  [...state.reserved]
    .filter(([load, r]) => r.workspaceId === workspaceId && load !== except)
    .reduce((n, [, r]) => n + r.credits, 0);

/** Credits the month has left for `loadId`'s lookups. */
export const headroom = (workspaceId: string, loadId = "") =>
  Math.max(
    0,
    (consentOf(workspaceId)?.cap || 0) -
      creditsUsed(workspaceId) -
      creditsReserved(workspaceId, loadId),
  );

/** Holds back credits for a running load, or lets them go (0). */
export function reserve(workspaceId: string, loadId: string, credits: number) {
  if (credits > 0) state.reserved.set(loadId, { workspaceId, credits });
  else state.reserved.delete(loadId);
}

/** Books a batch's charges. */
export function charge(workspaceId: string, loadId: string, credits: number) {
  if (credits > 0)
    state.charges.push({ workspaceId, loadId, credits, at: Date.now() });
}

// ---------------------------------------------------------------- Clues and the planner

/** A person as a load reads them: the entity's key, its name and its clue values. */
export type Person = {
  key: string;
  name: string;
  email?: string | null;
  linkedin?: string | null;
};
/**
 * Who a load looks up: the clues the planner kept to send, and the person's
 * clue values as Clues normalised them, which the profile row carries for
 * Ingest to bind it by.
 */
export type Lookup = {
  key: string;
  name: string;
  email?: string;
  slug?: string;
  clues: { email?: string; linkedin?: string };
};

/**
 * A LinkedIn URL's profile slug, as the planner canonicalises it: the
 * locale or m. subdomain, the query, the fragment and /details/… go;
 * /pub/, /sales/ and ACoAA… URLs aren't profiles.
 *
 * @example
 * linkedinSlug("https://uk.linkedin.com/in/Ada-Lovelace/details/experience?x=1"); // "ada-lovelace"
 * linkedinSlug("https://www.linkedin.com/pub/x/1/2/3"); // undefined
 */
export function linkedinSlug(url: string): string | undefined {
  let text = url.trim().toLowerCase();
  try {
    text = decodeURIComponent(text);
  } catch {
    return undefined;
  }
  const m =
    /^(?:https?:\/\/)?(?:[a-z]{2,3}\.|www\.|m\.)?linkedin\.com\/in\/([^/?#]+)\/?(?:details\/[^?#]*)?(?:[?#].*)?$/.exec(
      text,
    );
  return m && !m[1]!.startsWith("acoaa") ? m[1] : undefined;
}

const hasFullName = (name: string) =>
  (name.match(/\p{L}+/gu) || []).length >= 2;

/** How many times each value occurs. */
const tally = (values: (string | undefined)[]) => {
  const n = new Map<string, number>();
  for (const v of values) if (v) n.set(v, (n.get(v) || 0) + 1);
  return n;
};

/**
 * Clues, as the engine's Create pass gives them:
 * each person's email and LinkedIn URL normalised as identifiers, a value
 * two people share dropped for both (`shared`). No planner filter applies.
 */
function cluesOf(people: readonly Person[]) {
  const email = (p: Person) =>
      p.email ? normIdentifier("email", p.email) : undefined,
    linkedin = (p: Person) =>
      p.linkedin ? normIdentifier("exact", p.linkedin) : undefined;
  const emails = tally(people.map(email)),
    urls = tally(people.map(linkedin));
  return people.map((p) => {
    const e = email(p),
      u = linkedin(p);
    return {
      person: p,
      email: e && emails.get(e) === 1 ? e : undefined,
      linkedin: u && urls.get(u) === 1 ? u : undefined,
      shared: (!!e && emails.get(e) !== 1) || (!!u && urls.get(u) !== 1),
    };
  });
}

/**
 * Clues and the planner, counted as the backend
 * counts them: a value two people share is dropped for both (`shared`, with
 * a slug two people share); people with no full name are skipped, then the
 * filters drop bad LinkedIn URLs, free mail and role mailboxes, and anyone
 * left with no clue is skipped too (`noClue`; `skipped` is both). Of the
 * rest, the ontology's newest results carry some forward and the others are
 * looked up.
 */
export function planLookups(
  people: readonly Person[],
  previous: readonly Result[],
  at = Date.now(),
) {
  let noName = 0,
    noClue = 0,
    shared = 0;
  const kept: Lookup[] = [];
  for (const c of cluesOf(people)) {
    const p = c.person,
      ownEmail = c.email,
      ownUrl = c.linkedin;
    if (c.shared) shared += 1;
    if (!hasFullName(p.name)) {
      noName += 1;
      continue;
    }
    const slug = ownUrl ? linkedinSlug(ownUrl) : undefined;
    const [local = "", domain = ""] = (ownEmail || "").split("@");
    const usable =
      ownEmail && !FREE_MAIL.has(domain) && !ROLE_MAILBOXES.has(local)
        ? ownEmail
        : undefined;
    kept.push({
      key: p.key,
      name: p.name,
      email: usable,
      slug,
      clues: { email: ownEmail, linkedin: ownUrl },
    });
  }
  // A slug two people share is dropped for both.
  const slugs = tally(kept.map((l) => l.slug));
  const lookups = kept.flatMap((l): Lookup[] => {
    const lost = !!l.slug && slugs.get(l.slug) !== 1,
      slug = lost ? undefined : l.slug;
    if (lost) shared += 1;
    if (l.email || slug) return [{ ...l, slug }];
    noClue += 1;
    return [];
  });
  const carried: Result[] = [],
    calls: Lookup[] = [];
  for (const l of lookups) {
    const sentTo = (r: Result) =>
      (!!l.email && r.sent.email === l.email) ||
      (!!l.slug && r.sent.slug === l.slug);
    const found = previous.find(
      (r) =>
        r.profile &&
        (r.outcome === "accepted" || r.outcome === "held") &&
        sentTo(r) &&
        (!l.slug || r.profile.profile_id === `li:${l.slug}`),
    );
    if (found?.profile) {
      // The checks again, on the current name: a renamed person is held
      // without a call; a held person who now passes is accepted (a
      // duplicate is checked again at publish; a score stays as it was).
      const named = nameMatches(found.profile, l.name);
      const outcome: Outcome =
        !named || found.reason === "low_score" ? "held" : "accepted";
      carried.push({
        key: l.key,
        sent: { email: l.email, slug: l.slug },
        outcome,
        reason: !named
          ? "name_mismatch"
          : outcome === "held"
            ? found.reason
            : undefined,
        profile: {
          ...found.profile,
          clue_email: l.clues.email,
          clue_linkedin_url: l.clues.linkedin,
          clue_name: l.name,
        },
        checkedAt: found.checkedAt,
      });
      continue;
    }
    const missing = previous.find(
      (r) =>
        r.outcome === "not_found" &&
        sentTo(r) &&
        (!l.email || r.sent.email === l.email) &&
        (!l.slug || r.sent.slug === l.slug) &&
        at - Date.parse(r.checkedAt) < NOT_FOUND_DAYS * 86_400_000,
    );
    if (missing) carried.push({ ...missing, key: l.key });
    else calls.push(l);
  }
  return {
    counts: {
      people: people.length,
      noName,
      noClue,
      shared,
      skipped: noName + noClue,
    },
    carried,
    calls,
  };
}

/**
 * The agent's measure of a type's clues (AgentEnrichmentClues), as the
 * engine's `Clues::of` counts Clues: everyone with a full name and a clue
 * is usable, and the email and LinkedIn counts are of everyone holding one;
 * the planner's filters (free mail, role mailboxes, LinkedIn URLs it can't
 * read) don't apply, so a load may look up fewer.
 */
export function measureClues(people: readonly Person[]): AgentEnrichmentClues {
  const clued = cluesOf(people),
    full = (c: (typeof clued)[number]) => hasFullName(c.person.name);
  return {
    people: people.length,
    usable: clued.filter((c) => full(c) && (c.email || c.linkedin)).length,
    email: clued.filter((c) => c.email).length,
    linkedin_url: clued.filter((c) => c.linkedin).length,
    no_name: clued.filter((c) => !full(c)).length,
    shared: clued.filter((c) => c.shared).length,
    // A person holds one value of each clue here.
    conflicting: 0,
  };
}

// ---------------------------------------------------------------- fakeEnrich

// Who the fake finds, by name; anyone else is a partnerships lead in Austin.
const PERSONAS: Record<
  string,
  { title: string; city: string; region: string; country: string }
> = {
  "Ada Lovelace": {
    title: "VP Engineering",
    city: "London",
    region: "England",
    country: "United Kingdom",
  },
  "Grace Hopper": {
    title: "Director of Platform",
    city: "Arlington",
    region: "Virginia",
    country: "United States",
  },
  "Alan Turing": {
    title: "Head of Research",
    city: "Manchester",
    region: "England",
    country: "United Kingdom",
  },
  "Katherine Johnson": {
    title: "Chief Data Officer",
    city: "Hampton",
    region: "Virginia",
    country: "United States",
  },
};
const PERSONA = {
  title: "Head of Partnerships",
  city: "Austin",
  region: "Texas",
  country: "United States",
};

const orgName = (domain: string) =>
  domain.charAt(0).toUpperCase() + domain.split(".")[0]!.slice(1);
const slugOf = (name: string) =>
  name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");

/** Whether a profile's name is the person's, as the accept gate compares them. */
function nameMatches(profile: ProfileRow, name: string) {
  const fold = (s: string) =>
    s
      .normalize("NFKD")
      .replace(/[\u0300-\u036f]/g, "")
      .toLowerCase();
  const tokens: string[] = fold(name).match(/\p{L}+/gu) || [];
  const first = fold(String(profile.fields.first_name || "")),
    last = fold(String(profile.fields.last_name || ""));
  return (
    tokens.includes(last) &&
    tokens.some(
      (t) =>
        t === first ||
        (t.length >= 3 &&
          first.length >= 3 &&
          t.slice(0, 3) === first.slice(0, 3)),
    )
  );
}

/** A profile the fake finds for `name`, at the email's domain. */
function profileOf(l: Lookup, name: string, id?: string): ProfileRow {
  const persona = PERSONAS[name] || PERSONA,
    slug = l.slug || slugOf(name),
    domain = l.email?.split("@")[1] || "acme.io",
    before = domain === "initech.com" ? "globex.example" : "initech.com";
  const [first = "", ...rest] = name.split(" ");
  const experience: Experience[] = [
    {
      id: 0,
      title: persona.title,
      organization: { name: orgName(domain), domain },
      location: persona.city,
      start_date: "2021-03",
      end_date: null,
      is_current: true,
    },
    {
      id: 1,
      title: "Engineer",
      organization: { name: orgName(before), domain: before },
      location: persona.city,
      start_date: "2016-09",
      end_date: "2021-02",
      is_current: false,
    },
  ];
  return {
    profile_id: id || `li:${slug}`,
    clue_email: l.clues.email,
    clue_linkedin_url: l.clues.linkedin,
    clue_name: l.name,
    fields: {
      full_name: name,
      first_name: first,
      last_name: rest.join(" "),
      current_title: persona.title,
      current_role_status: "active",
      current_org_name: orgName(domain),
      current_org_domain: domain,
      linkedin_url: `https://www.linkedin.com/in/${slug}`,
      location: `${persona.city}, ${persona.country}`,
      location_city: persona.city,
      location_region: persona.region,
      location_country: persona.country,
    },
    experience,
  };
}

/**
 * The fake context.dev and the accept gate, for one person: what the
 * lookup came to and what it costs.
 *
 * @example
 * fakeEnrich({ key: "c_22", name: "Bea Badger", email: "bea@badgerworks.io" }); // { outcome: "not_found", credits: 0 }
 */
export function fakeEnrich(l: Lookup): Omit<Result, "checkedAt"> & {
  credits: number;
} {
  const text = `${l.email || ""} ${l.slug || ""}`.toLowerCase(),
    sent = { email: l.email, slug: l.slug },
    paid = CREDITS_PER_PROFILE;
  if (text.includes("bad"))
    return { key: l.key, sent, outcome: "not_found", credits: 0 };
  if (text.includes("timeout"))
    return { key: l.key, sent, outcome: "uncertain", credits: paid };
  const profile = profileOf(
    l,
    text.includes("other") ? "Someone Else" : l.name,
    text.includes("dup") ? "li:dupont-dupre" : undefined,
  );
  const held = text.includes("low")
    ? "low_score"
    : !nameMatches(profile, l.name)
      ? "name_mismatch"
      : undefined;
  return held
    ? {
        key: l.key,
        sent,
        outcome: "held",
        reason: held,
        profile,
        credits: paid,
      }
    : { key: l.key, sent, outcome: "accepted", profile, credits: paid };
}

/**
 * Publishes a load's results for its ontology: a profile two people match
 * holds both (duplicate_profile). Returns the profiles the load applies.
 */
export function publishResults(
  ontologyId: string,
  results: Result[],
): ProfileRow[] {
  const ids = new Map<string, number>();
  for (const r of results)
    if (r.profile && (r.outcome === "accepted" || r.outcome === "held"))
      ids.set(r.profile.profile_id, (ids.get(r.profile.profile_id) || 0) + 1);
  for (const r of results)
    if (r.profile && (ids.get(r.profile.profile_id) || 0) > 1) {
      r.outcome = "held";
      r.reason = "duplicate_profile";
    }
  state.results.set(ontologyId, results);
  return results.flatMap((r) =>
    r.outcome === "accepted" && r.profile ? [r.profile] : [],
  );
}

/** The ontology's newest published results: what its next load carries. */
export const previousResults = (ontologyId: string) =>
  state.results.get(ontologyId) || [];

// ---------------------------------------------------------------- Purges

/** When the workspace's owner last turned People enrichment off, if ever. */
export const lastTurnedOff = (workspaceId: string) =>
  state.consents.findLast((c) => c.workspaceId === workspaceId && !c.enabled)
    ?.at;

const purgesLeft = (workspaceId: string) =>
  state.purges.filter(
    (p) => p.workspaceId === workspaceId && !purgeLoaded(p.ontologyId),
  ).length;

// ---------------------------------------------------------------- Routes

function view(workspaceId: string): FoundryEnrichment {
  const rows = state.consents.filter((c) => c.workspaceId === workspaceId),
    consent = rows.at(-1);
  // The row that last turned it on: who allowed it, and when.
  const on = consent?.enabled
    ? rows.findLast((c, i) => c.enabled && !rows[i - 1]?.enabled)
    : undefined;
  const month = new Date();
  return {
    availability: !enrichmentFlagOn()
      ? "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED"
      : isFoundryPro(workspaceId)
        ? "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE"
        : "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
    enabled: consent?.enabled === true,
    monthlyCreditCap: String(consent?.cap || 0),
    creditsUsed: String(creditsUsed(workspaceId)),
    creditsReserved: String(creditsReserved(workspaceId)),
    capResetsAt: new Date(
      Date.UTC(month.getUTCFullYear(), month.getUTCMonth() + 1, 1),
    ).toISOString(),
    enabledBy: on?.userId || "",
    enabledAt: on?.at || null,
    purgesPending: purgesLeft(workspaceId),
  };
}

const BASE = "/v1/workspaces/{workspaceId}/foundry/enrichment" as const;

export const enrichmentRoutes: MockRoute[] = [
  {
    method: "GET",
    path: BASE,
    handle: ({ params, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      return ok({
        enrichment: view(m.workspaceId),
      } satisfies FoundryEnrichmentResponse);
    },
  },
  {
    method: "PATCH",
    path: BASE,
    handle: ({ params, body, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      // workspace.enrichment.manage: owners only.
      if (m.role !== "WORKSPACE_ROLE_OWNER")
        return fail("PERMISSION_DENIED", "permission denied");
      // As the WFE checks it: every change names its notice; turning it on
      // (or a new cap, which keeps it on) needs the operators' flag, then
      // Foundry Pro.
      const req = (body || {}) as Partial<UpdateFoundryEnrichmentRequest>;
      const enabled = req.enabled === true,
        notice = req.noticeVersion || "",
        cap = Number(req.monthlyCreditCap || 0);
      if (!notice || notice.length > NOTICE_MAX)
        return fail(
          "INVALID_ARGUMENT",
          `notice_version is required, at most ${NOTICE_MAX} characters`,
        );
      if (!Number.isInteger(cap) || cap < 0 || cap > CAP_MAX)
        return fail(
          "INVALID_ARGUMENT",
          `monthly_credit_cap must be 0 to ${CAP_MAX}`,
        );
      if (enabled && !enrichmentFlagOn()) return enrichmentUnavailable();
      if (enabled && !isFoundryPro(m.workspaceId))
        return proRequired(
          m.workspaceId,
          "People enrichment needs Foundry Pro",
        );
      const was = enrichmentAllowed(m.workspaceId),
        at = now();
      state.consents.push({
        workspaceId: m.workspaceId,
        enabled,
        cap,
        noticeVersion: notice,
        userId: user.id,
        at,
      });
      if (was && !enabled) {
        // Live steps stop without publishing; each ontology with a load
        // that applied profiles, current or not, writing or not, is purged
        // of them (one purge per ontology).
        cancelEnrichment(m.workspaceId);
        for (const o of workspaceOntologies(m.workspaceId))
          if (ontologyHasProfiles(o.id)) {
            state.purges = [
              ...state.purges.filter((p) => p.ontologyId !== o.id),
              { workspaceId: m.workspaceId, ontologyId: o.id },
            ];
            queuePurgeLoad(o.id);
          }
        for (const o of workspaceOntologies(m.workspaceId))
          state.results.delete(o.id);
      }
      return ok({
        enrichment: view(m.workspaceId),
      } satisfies FoundryEnrichmentResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{id}:stop-enrichment",
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const load = stopLoadEnrichment(s.o.id, req.params.id!);
      return load
        ? ok({ load } satisfies LoadResponse)
        : fail("NOT_FOUND", "this load looks no one up");
    },
  },
];
