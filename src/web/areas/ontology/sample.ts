/**
 * The sample company the first ontology's wizard offers (./try-sample.tsx):
 * Kestrel, a made-up field-service software company, as its own records
 * hold it. scripts/sample-company.mjs writes its files into public/, served
 * at `base`; uploaded, they go into a set of the workspace's named
 * `set.name`, from which the wizard creates `ontology`, toward `goal`, on
 * the sample's ready-made draft (GenerateOntologyRequest's `sample`, `id`;
 * backend/sample/kestrel.json).
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
  // Tables first, then documents, as the Data area lists them.
  files: [
    "accounts.csv",
    "contacts.csv",
    "subscriptions.csv",
    "invoices.csv",
    "support_tickets.csv",
    "product_usage.csv",
    "Kestrel plans and pricing 2026.md",
    "Pinecrest Home Health order form.pdf",
    "Halvorsen Freight master agreement.pdf",
    "Brightmoor Facilities order form.pdf",
    "Pinecrest QBR 2026-09-15.md",
    "Halvorsen expansion call 2026-10-06.md",
    "Brightmoor renewal call 2026-10-21.md",
    "Sable Solar escalation 2026-10-08.md",
    "Incident report 2026-10-07 dispatch sync.md",
  ],
} as const;
