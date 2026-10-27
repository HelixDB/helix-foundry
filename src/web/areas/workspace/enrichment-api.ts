import type {
  FoundryEnrichment,
  FoundryEnrichmentResponse,
  UpdateFoundryEnrichmentRequest,
} from "../../../shared/wfe";
import { api } from "../../api";
import { formatNumber } from "../../kit/util";
import { CREDITS_PER_PROFILE } from "../ontology/enrichment-fields";
import { shortDate, utcDay } from "../ontology/model";

// Settings › People enrichment's WFE calls and words: the owner's consent and monthly credit cap, this month's credits,
// and what a load's dialog says the load will do with them.

const path = (ws: string) =>
  `/v1/workspaces/${encodeURIComponent(ws)}/foundry/enrichment`;

/** The notice an owner is shown, recorded with every change they make. */
export const ENRICHMENT_NOTICE_VERSION = "2026-10-20";
export const ENRICHMENT_NOTICE =
  "Helix sends the work emails and LinkedIn URLs of people in your tables to context.dev to find public professional profiles, and stores the matches.";
/** The most a workspace may allow a month, and what is offered at first. */
export const CAP_MAX = 100_000;
export const DEFAULT_CAP = 50_000;

export const enrichmentKey = (ws: string) => [ws, "foundry-enrichment"];
/** The setting; read again every few seconds while values are being removed. */
export const enrichmentQuery = (ws: string) => ({
  queryKey: enrichmentKey(ws),
  queryFn: async () =>
    (await api<FoundryEnrichmentResponse>(path(ws))).enrichment || {},
  refetchInterval: (q: { state: { data?: FoundryEnrichment } }) =>
    q.state.data?.purgesPending ? 5000 : false,
});

/** Saves the owner's choice; turning it off deletes the stored profiles. */
export const updateEnrichment = async (
  ws: string,
  body: UpdateFoundryEnrichmentRequest,
) =>
  (await api<FoundryEnrichmentResponse>(path(ws), body, "PATCH")).enrichment ||
  {};

/**
 * This month's credits: the cap, what was charged, what a running load
 * holds back, and what is left (never below 0).
 *
 * @example
 * enrichmentCredits({ monthlyCreditCap: "50000", creditsUsed: "8460", creditsReserved: "12240" }); // { cap: 50000, used: 8460, reserved: 12240, left: 29300 }
 */
export function enrichmentCredits(e: FoundryEnrichment) {
  const cap = Number(e.monthlyCreditCap || 0),
    used = Number(e.creditsUsed || 0),
    reserved = Number(e.creditsReserved || 0);
  return { cap, used, reserved, left: Math.max(0, cap - used - reserved) };
}

const resetDay = (e: FoundryEnrichment) => {
  const d = new Date(e.capResetsAt || "");
  return Number.isNaN(d.getTime()) ? "" : utcDay(d);
};

/**
 * Whether Helix couldn't say if the workspace's loads may look people up:
 * its plan couldn't be read (UNSPECIFIED, which the WFE may leave unset).
 * Turning People enrichment off still works; turning it on or changing the
 * cap waits.
 *
 * @example
 * planUnknown({ availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED" }); // true
 */
export const planUnknown = (e: FoundryEnrichment) =>
  !e.availability ||
  e.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED";

/**
 * What a load of an enriching version will do, for its dialog: off without
 * consent, carry-only off Pro or while Helix pauses lookups, unknown while
 * Helix can't read the plan, carry-only with no credits left, else what it
 * may spend.
 *
 * @example
 * enrichmentLoadText({ enabled: false }); // "Off for this workspace: this load applies no profiles."
 */
export function enrichmentLoadText(e: FoundryEnrichment): string {
  if (!e.enabled)
    return "Off for this workspace: this load applies no profiles.";
  if (e.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED")
    return "Needs Foundry Pro for new lookups; profiles found before still apply.";
  if (e.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED")
    return "Lookups are paused by Helix; profiles found before still apply.";
  if (planUnknown(e))
    return "Helix can’t read this workspace’s plan just now, so it can’t say whether this load looks people up; profiles found before still apply.";
  const { cap, left } = enrichmentCredits(e),
    reset = resetDay(e);
  if (left < CREDITS_PER_PROFILE)
    return `0 credits left this month: applies profiles found before.${reset ? ` The cap resets on ${reset}.` : ""}`;
  return `Looks up people earlier loads haven’t: ${CREDITS_PER_PROFILE} credits per profile found, ${formatNumber(left)} of ${formatNumber(cap)} left this month. The first load takes longer and re-reads documents that mention people.`;
}

/**
 * The setting in a line: for the owner, this month's credits and who
 * allowed it (`allowedBy`, a name, when known); for anyone else, whether it
 * is on and that only owners change it.
 *
 * @example
 * enrichmentStatusText({ enabled: true, monthlyCreditCap: "50000", creditsUsed: "8460" }, false); // "On · cap 50,000 · 8,460 used. Only owners can change this."
 */
export function enrichmentStatusText(
  e: FoundryEnrichment,
  owner: boolean,
  allowedBy?: string,
): string {
  const { cap, used, reserved } = enrichmentCredits(e);
  if (!owner)
    return e.enabled
      ? `On · cap ${formatNumber(cap)} · ${formatNumber(used)} used. Only owners can change this.`
      : "Off. Only owners can change this.";
  if (!e.enabled) return "";
  const reset = resetDay(e),
    day = shortDate(e.enabledAt);
  return [
    `${formatNumber(used)} of ${formatNumber(cap)} credits used this month`,
    ...(reserved
      ? [`${formatNumber(reserved)} reserved by a running load`]
      : []),
    ...(reset ? [`resets ${reset}`] : []),
    ...(allowedBy || day
      ? [
          `Allowed${allowedBy ? ` by ${allowedBy}` : ""}${day ? ` on ${day}` : ""}`,
        ]
      : []),
  ].join(" · ");
}
