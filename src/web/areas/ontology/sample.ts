import { LOCAL } from "../../../shared/deployment";
import type { ConnectorConfig } from "../../../shared/wfe";

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
