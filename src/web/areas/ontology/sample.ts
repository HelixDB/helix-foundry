import type { ConnectorConfig } from "../../../shared/wfe";

// Kestrel's documents: three contracts as PDFs, call notes, a support
// escalation, an incident report and the price list.
const DOCUMENTS = [
  "Kestrel plans and pricing 2026.md",
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
 * Kestrel, a made-up field-service software company, as its own records
 * hold it. scripts/sample-company.mjs writes its files into public/, served
 * at `base`, and its tables into local/kestrel/kestrel.sql.
 *
 * A local install connects its kestrel-db (`database`, local/compose.yaml)
 * as a PostgreSQL database, uploads the `documents` into a set named
 * `set.name`, and creates `ontology` toward `goal` on the sample's
 * ready-made draft (GenerateOntologyRequest's `sample`, `id`;
 * backend/sample/kestrel.json), all on its own page (./sample-tour.tsx).
 * Hosted, the wizard uploads every one of its `files`, the tables as CSV,
 * and drafts from them.
 */
export const SAMPLE = {
  id: "kestrel",
  base: "/samples/kestrel/",
  ontology: "Kestrel customers",
  set: {
    name: "Kestrel sample",
    // The ontology agent reads it.
    description:
      "Kestrel, a made-up field-service scheduling software company, as of 27 October 2026: its customer accounts, contacts, subscriptions, invoices, support tickets and monthly product usage as tables, with three customer contracts, call notes, a support escalation, an incident report and its price list.",
  },
  goal: "Understand Kestrel's customers: each account with its people, plan and subscription, contract terms, invoices, support tickets, usage and what was said on calls, so we can see which renewals are at risk, what we have promised, and what we owe.",
  database: {
    name: "Kestrel",
    // kestrel-db's read-only login; its data is made up.
    config: {
      postgres: {
        host: "kestrel-db",
        port: 5432,
        database: "kestrel",
        user: "kestrel_reader",
        password: "kestrel",
        disableTls: true,
      },
    } satisfies ConnectorConfig,
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
  // Tables first, then documents, as the Data area lists them.
  files: [
    "accounts.csv",
    "contacts.csv",
    "subscriptions.csv",
    "invoices.csv",
    "support_tickets.csv",
    "product_usage.csv",
    ...DOCUMENTS,
  ],
} as const;
