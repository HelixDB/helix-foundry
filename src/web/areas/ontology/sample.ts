import { LOCAL } from "../../../shared/deployment";
import type {
  Connector,
  ConnectorConfig,
  FoundryDocumentSet,
} from "../../../shared/wfe";
import { ontologyPaths } from "./paths";

// Acme Corp's documents: three contracts as PDFs, call notes, a support
// escalation, an incident report and the price list.
const DOCUMENTS = [
  "Acme Corp plans and pricing 2026.md",
  "Pinecrest Home Health order form.pdf",
  "Halvorsen Freight master agreement.pdf",
  "Brightmoor Facilities order form.pdf",
  "Pinecrest QBR 2026-09-15.md",
  "Halvorsen expansion call 2026-10-06.md",
  "Brightmoor renewal call 2026-10-21.md",
  "Sable Solar escalation 2026-10-08.md",
  "Incident report 2026-10-07 dispatch sync.md",
] as const;

/**
 * The sample company the first ontology's wizard offers (./try-sample.tsx):
 * Acme Corp, a made-up field-service software company, as its own records
 * hold it. scripts/sample-company.mjs writes its documents into public/,
 * served at `base`, and its tables into local/acme/acme.sql.
 *
 * Its own page (./sample-tour.tsx) connects its database (`database.host`:
 * a local install's acme-db, local/compose.yaml, or the stamp's own) as a
 * PostgreSQL database, uploads the `documents` into a set named
 * `set.name`, and creates `ontology` toward `goal` on the sample's
 * ready-made draft (GenerateOntologyRequest's `sample`, `id`;
 * backend/sample/acme.json).
 */
export const SAMPLE = {
  id: "acme",
  base: "/samples/acme/",
  ontology: "Acme Corp customers",
  set: {
    name: "Acme Corp sample",
    // The ontology agent reads it.
    description:
      "Acme Corp, a made-up field-service scheduling software company, as of 27 October 2026: its customer accounts, contacts, subscriptions, invoices, support tickets and monthly product usage as tables, with three customer contracts, call notes, a support escalation, an incident report and its price list.",
  },
  goal: "Understand Acme Corp's customers: each account with its people, plan and subscription, contract terms, invoices, support tickets, usage and what was said on calls, so we can see which renewals are at risk, what we have promised, and what we owe.",
  database: {
    name: "Acme Corp",
    // Per build: next.config.ts.
    host: process.env.NEXT_PUBLIC_SAMPLE_DB_HOST ?? "",
    // Its read-only login, public: the data is made up, and a stamp's
    // database lets in only the stamp's own connectors.
    login: {
      port: 5432,
      database: "acme",
      user: "acme_reader",
      password: "acme",
    },
    tables: [
      "accounts",
      "contacts",
      "subscriptions",
      "invoices",
      "support_tickets",
      "product_usage",
    ],
  },
  documents: DOCUMENTS,
} as const;

// The ontology a page of the app has open: its own pages, the Explorer's and
// Ask's.
const OPEN_ONTOLOGY = /^\/(?:ontology|explore|ask)\/([^/]+)/;

/**
 * Whether what `pathname` shows is the sample's, `sampleId` being its
 * ontology and `ontologies` how many the workspace has: anything while it is
 * the only one, else its own pages and the sample's page. Never the page making
 * a new ontology, which is the workspace's own.
 *
 * @example
 * sampleInView("/data", "o1", 1); // true
 * sampleInView("/data", "o1", 2); // false
 * sampleInView("/explore/o1/records", "o1", 2); // true
 * sampleInView("/ontology/new", "o1", 1); // false
 */
export const sampleInView = (
  pathname: string,
  sampleId: string,
  ontologies: number,
) =>
  pathname !== ontologyPaths.newOntology() &&
  (ontologies === 1 ||
    pathname === ontologyPaths.sampleTour() ||
    OPEN_ONTOLOGY.exec(pathname)?.[1] === encodeURIComponent(sampleId));

/**
 * Whether a connector is the sample's database, by what it connects to: it can
 * be renamed, and someone's own database may be called Acme Corp. A build with
 * no sample database (`database.host` empty) has none. With the set
 * (isSampleSet), it is left out of what an ontology of the user's own can draft
 * from; only the sample's own ontology (useSampleOntology) draws on them.
 *
 * @example
 * // With NEXT_PUBLIC_SAMPLE_DB_HOST=db.example:
 * const postgres = { host: "db.example", port: 5432, database: "acme", user: "acme_reader" };
 * isSampleConnector({ id: "c1", kind: "postgres", name: "Renamed", config: { postgres } }); // true
 * isSampleConnector({ id: "c2", kind: "postgres", name: "Acme Corp", config: { postgres: { ...postgres, host: "mine.example" } } }); // false
 */
export function isSampleConnector(c: Connector) {
  const { host, login } = SAMPLE.database,
    db = c.config && "postgres" in c.config ? c.config.postgres : undefined;
  return (
    !!host &&
    db?.host === host &&
    db.database === login.database &&
    db.user === login.user
  );
}

/**
 * Whether a document set is the one the sample's documents are uploaded into:
 * one people made (a connector's never is), by its name.
 *
 * @example
 * isSampleSet({ id: "s1", name: "Acme Corp sample" }); // true
 * isSampleSet({ id: "s2", name: "General" }); // false
 * isSampleSet({ id: "s3", name: "Acme Corp sample", connector: { connectorId: "c1" } }); // false
 */
export const isSampleSet = (s: FoundryDocumentSet) =>
  !s.connector && s.name === SAMPLE.set.name;

/**
 * The sample's database as a PostgreSQL connector's config. A local
 * acme-db has no TLS; a stamp's is an RDS whose certificate chains to
 * Amazon's RDS root for its region, which no system store holds, so the
 * connector trusts public/samples/rds-ca.pem (both stamps' regions) too.
 */
export async function sampleDatabaseConfig(): Promise<ConnectorConfig> {
  const { host, login } = SAMPLE.database;
  if (LOCAL) return { postgres: { host, ...login, disableTls: true } };
  const res = await fetch("/samples/rds-ca.pem");
  if (!res.ok)
    throw new Error("couldn’t read the sample database’s certificate");
  return { postgres: { host, ...login, ca: await res.text() } };
}
