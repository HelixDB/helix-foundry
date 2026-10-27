import { stringify } from "smol-toml";
import type {
  AgentBundleDefinition,
  AgentDraftItem,
  AgentEnrichmentClues,
} from "../../src/shared/wfe";

// Acme's "Sales graph": the people of the Shop database's public.people
// table and the accounts (web domains) their rows name, as two agent drafts.
// Version 1 maps the table; version 2, the current one, also looks the
// people up with People enrichment, as the ontology agent suggested.
//
// The rows exercise every path a lookup can take (mock/routes/enrichment.ts
// fakeEnrich decides by the clue's text): matches, a low score ("low"), no
// match ("bad"), two people sharing a profile ("dup"), a profile of someone
// else ("other"), free mail and a role mailbox (skipped), no full name, no
// clue, and a LinkedIn URL that isn't a profile's. No two people share an
// address, so the agent proposes enrichment for them.

/** One row of public.people. */
export type PersonRow = {
  contact_id: string;
  full_name: string | null;
  email: string | null;
  linkedin_url: string | null;
  // The account's web domain.
  company: string | null;
};

const row = (
  contact_id: string,
  full_name: string | null,
  email: string | null,
  company: string | null,
  linkedin_url: string | null = null,
): PersonRow => ({ contact_id, full_name, email, linkedin_url, company });

export const PEOPLE: readonly PersonRow[] = [
  row("c_17", "Ada Lovelace", "ada@acme.io", "acme.io"),
  row("c_18", "Grace Hopper", "grace.hopper@initech.com", "initech.com"),
  row(
    "c_19",
    "Alan Turing",
    null,
    "acme.io",
    "https://www.linkedin.com/in/alan-turing/",
  ),
  row(
    "c_20",
    "Katherine Johnson",
    "katherine@globex.example",
    "globex.example",
  ),
  row("c_21", "Rob Lowell", "rob.lowell@acme.io", "acme.io"),
  row("c_22", "Bea Badger", "bea@badgerworks.io", null),
  row("c_23", "Dana Dupont", "dana@dupont.io", null),
  row("c_24", "Dan Dupre", "dan@dupre.io", null),
  row("c_25", "Otto Mann", "otto@another.io", null),
  row("c_26", "Fran Free", "fran.free@gmail.com", null),
  row("c_27", "Sales Team", "sales@acme.io", "acme.io"),
  row("c_28", "Cher", "cher@initech.com", "initech.com"),
  row("c_29", "Nobody Known", null, null),
  row(
    "c_30",
    "Lin Kedin",
    null,
    null,
    "https://www.linkedin.com/pub/lin-kedin/1/2/3",
  ),
];

/** public.people's columns, as the Shop database reports them. */
export const PEOPLE_COLUMNS: [
  name: string,
  postgres: string,
  mysql: string,
  nullable?: true,
][] = [
  ["contact_id", "text", "varchar(64)"],
  ["full_name", "text", "varchar(255)", true],
  ["email", "text", "varchar(255)", true],
  ["linkedin_url", "text", "varchar(255)", true],
  ["company", "text", "varchar(255)", true],
];

const toml = (value: Record<string, unknown>) => stringify(value) + "\n";

/**
 * A Sales graph draft reading public.people (`source`, its agent source id).
 * With `clues`, it enriches Person: the props and link the item fills, the
 * clues as identifiers, and the item, with the agent's measure.
 */
export function salesGraphBundle(
  source: string,
  clues?: AgentEnrichmentClues,
): AgentBundleDefinition {
  const enriched = !!clues;
  const person = {
    description: "A person at one of our customer accounts.",
    props: {
      contact_id: "string",
      full_name: "string",
      email: "string",
      linkedin_url: "string",
      ...(enriched
        ? {
            current_title: "string",
            location: "string",
            country: "string",
            profile_url: "string",
          }
        : {}),
    },
    key: ["contact_id"],
    names: ["full_name"],
    identifiers: enriched
      ? { email: "email", linkedin_url: "exact" }
      : { email: "email" },
  };
  const company = {
    description: "A customer account, known by its web domain.",
    props: { domain: "string" },
    key: ["domain"],
  };
  // As the engine renders a link: its definition, and its cardinality as
  // an item of its own.
  const worksAtDefinition = {
      from: "Person",
      to: "Company",
      description: "The account the person's row names.",
    },
    worksAt = { ...worksAtDefinition, cardinality: "one" };
  const workedAt = {
    from: "Person",
    to: "Company",
    description: "A role the person held there, from their public profile.",
    props: {
      title: "string",
      start_date: "string",
      end_date: "string",
      is_current: "boolean",
    },
  };
  const mapping = {
    source,
    links: [{ link: "works_at", from: "person", to: "company" }],
    bind: {
      person: {
        type: "Person",
        key: { contact_id: "contact_id" },
        props: {
          full_name: "full_name",
          email: "email",
          linkedin_url: "linkedin_url",
        },
        missing: "create",
      },
      company: {
        type: "Company",
        key: { domain: { field: "company", transform: ["lower"] } },
        missing: "create",
        optional: true,
      },
    },
  };
  const enrichment = {
    type: "Person",
    clues: { email: "email", linkedin_url: "linkedin_url" },
    props: {
      current_title: "current_title",
      location: "location",
      country: "location_country",
      profile_url: "linkedin_url",
    },
    work_history: {
      link: "worked_at",
      match: "domain",
      props: {
        title: "title",
        start_date: "start_date",
        end_date: "end_date",
        is_current: "is_current",
      },
    },
  };
  const types = { Person: person, Company: company },
    links = enriched
      ? { works_at: worksAt, worked_at: workedAt }
      : { works_at: worksAt };
  const ontologyToml = toml({ version: "draft", types, links }),
    mappingsToml = toml({
      version: "draft",
      ontology: "draft",
      mappings: { people: mapping },
      ...(enriched ? { enrichments: { people: enrichment } } : {}),
    });
  const item = (
    snippet: Record<string, unknown>,
    rationale: string,
    serves: string[],
  ): AgentDraftItem => ({
    toml: toml(snippet),
    status: "proposed",
    rationale,
    serves,
  });
  const items: Record<string, AgentDraftItem> = {
    "type.Person": item(
      { types: { Person: person } },
      "Each row of people is one contact, keyed by contact_id and named by full_name.",
      ["cq.CQ1"],
    ),
    "type.Company": item(
      { types: { Company: company } },
      "people.company names each contact's account by its web domain.",
      ["cq.CQ1"],
    ),
    "link.works_at": item(
      { links: { works_at: worksAtDefinition } },
      "people.company is the account a contact works at.",
      ["cq.CQ1"],
    ),
    "link.works_at.cardinality": item(
      { links: { works_at: { cardinality: "one" } } },
      "Each row names one account.",
      ["cq.CQ1"],
    ),
    "mapping.people": item(
      { mappings: { people: mapping } },
      "Loads every row of people as a Person, and the account it names.",
      ["cq.CQ1"],
    ),
    "cq.CQ1": item(
      {
        cq: {
          CQ1: {
            question:
              "What is each contact's current title, and where did they work before?",
            path: ["Person", "worked_at>", "Company"],
          },
        },
      },
      "",
      [],
    ),
    ...(enriched
      ? {
          "link.worked_at": item(
            { links: { worked_at: workedAt } },
            "Work history from each contact's public profile.",
            ["cq.CQ1"],
          ),
          "enrichment.people": {
            ...item(
              { enrichments: { people: enrichment } },
              "CQ1 asks each contact's current title; people.email fills Person.email.",
              ["cq.CQ1"],
            ),
            clues,
          },
        }
      : {}),
  };
  const summary = enriched
    ? "Contacts from the Shop database's people table and the accounts they work at, with their current titles and work history looked up by People enrichment."
    : "Contacts from the Shop database's people table and the accounts they work at.";
  return {
    format: "agent_bundle",
    rules: { count: 0, evaluated: 0 },
    status: "done",
    ontology_toml: ontologyToml,
    mappings_toml: mappingsToml,
    draft: {
      ontology_toml: ontologyToml,
      mappings_toml: mappingsToml,
      draft: { rev: 1, base_version: null, items, left_out: [], questions: [] },
    },
    questions: [],
    summary,
    usage: {
      input: 12,
      cache_read: 33000,
      cache_write: 5500,
      output: 1400,
      usd: 0.066,
    },
  };
}
