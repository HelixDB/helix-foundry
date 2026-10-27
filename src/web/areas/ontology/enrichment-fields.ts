// People enrichment's built-in catalog (the engine's
// sources/src/builtin.rs): the fields of a profile the `foundry_enrichment`
// source holds, which of them a mappings.toml [enrichments] item may map,
// and the defaults the version editor's Enrich people form offers.

/**
 * The source a load ingests its profiles from, beside its tables: one row
 * per profile, keyed by its profile id ("li:<slug>", or "h:" and 16 hex).
 */
export const ENRICHMENT_SOURCE = "foundry_enrichment";

/** What a profile found costs, accepted or held; no match costs nothing. */
export const CREDITS_PER_PROFILE = 20;

/**
 * Profile fields an item's `props` may map, with their type. The worker
 * keeps `linkedin_url` canonical and dates as strings.
 */
export const PROFILE_FIELDS = {
  full_name: "string",
  first_name: "string",
  last_name: "string",
  current_title: "string",
  current_role_status: "string",
  current_org_name: "string",
  current_org_domain: "string",
  linkedin_url: "string",
  location: "string",
  location_city: "string",
  location_region: "string",
  location_country: "string",
} as const;
export type ProfileField = keyof typeof PROFILE_FIELDS;

/**
 * An `experience` entry's fields `work_history.props` may map: its number
 * in the profile, and the organization's by JSON Pointer. The organization
 * is found by `/organization/domain` (`work_history.match`).
 */
export const EXPERIENCE_FIELDS = {
  id: "integer",
  title: "string",
  "/organization/name": "string",
  "/organization/domain": "string",
  location: "string",
  start_date: "string",
  end_date: "string",
  is_current: "boolean",
} as const;
export type ExperienceField = keyof typeof EXPERIENCE_FIELDS;

/** A profile's ids, clues and lookup metadata: never mappable. */
export const UNMAPPABLE_FIELDS = [
  "profile_id",
  "clue_email",
  "clue_linkedin_url",
  "clue_name",
  "matched_on",
  "score",
  "last_updated",
  "checked_at",
  "fetched_at",
  "load_id",
  "call_id",
  "provider_request_id",
] as const;

/** The profile fields the Enrich people form offers, and the prop each fills by default. */
export const PROFILE_DEFAULTS: readonly {
  field: ProfileField;
  label: string;
  prop: string;
}[] = [
  { field: "current_title", label: "Current title", prop: "current_title" },
  { field: "location", label: "Location", prop: "location" },
  { field: "location_country", label: "Country", prop: "country" },
  { field: "linkedin_url", label: "LinkedIn profile", prop: "profile_url" },
];

/** The link the form adds for work history, and the experience field each of its props holds. */
export const WORK_HISTORY_DEFAULT = {
  link: "worked_at",
  description: "A role the person held there, from their public profile.",
  props: {
    title: "title",
    start_date: "start_date",
    end_date: "end_date",
    is_current: "is_current",
  } satisfies Record<string, ExperienceField>,
} as const;
