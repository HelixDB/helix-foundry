import { describe, expect, it } from "vitest";
import type { FoundryAgentAction } from "@/shared/wfe";
import { formatDate } from "@/web/kit/util";
import {
  EMPTY_STREAM,
  NOT_RUN_TEXT,
  cardView,
  connectLink,
  parseCard,
  reduceTurnEvents,
  resultLink,
  reviewEntity,
  statementText,
  valueText,
  type AgentCard,
  type AgentStep,
} from "@/web/areas/agent/model";

// The Assistant's cards (src/web/areas/agent/model.ts): each of the 27
// operations' displays as the backend's display.go writes them (its
// display_test.go's), read without trust, worded by its card, and where a
// ran one opens; a connect step's form; a call a limit kept from running.

const ONT = { id: "ont1", name: "Sales graph" };
const REVIEW = {
  review_key: "r1",
  reason: "unresolved",
  settle: "resolve",
  type: "Customer",
  doc_id: "doc1",
  norm_name: "globexx",
  candidates: ["scus_1"],
  quote: "Globexx",
  entities: [
    { type: "Customer", entity_key: "scus_1", name: "Globex Corporation" },
  ],
};
// Globex Corporation's entity page.
const GLOBEX = "/explore/ont1/entities/Q3VzdG9tZXIAazpzY3VzXzE";

/** One display per case, as the backend writes it. */
const DISPLAYS: [string, string, unknown][] = [
  [
    "an ontology on a database",
    "create_ontology",
    { name: "Renewals", database: { id: "t1", name: "sales" } },
  ],
  [
    "an ontology on a new database in a new project",
    "create_ontology",
    {
      name: "Renewals",
      new_database: {
        name: "Renewals db",
        slug: "renewals",
        new_project: "Contracts",
      },
    },
  ],
  [
    "an ontology on a new database in a project",
    "create_ontology",
    {
      name: "Renewals",
      new_database: { name: "Renewals db", slug: "renewals", project_id: "p1" },
    },
  ],
  [
    "a generation of chosen sources",
    "generate_ontology",
    {
      ontology: ONT,
      goal: "Track renewals",
      connectors_read: "chosen",
      connectors: [
        { id: "c1", name: "Shop database", kind: "postgres" },
        { id: "gone1" },
      ],
      document_sets: [
        { id: "set1", name: "General" },
        { id: "set3", name: "Granola / Notes" },
      ],
      event_sources: [
        {
          connector: { id: "ev1", name: "Notes API", kind: "events" },
          mode: "extraction",
        },
        { connector: { id: "gone2" }, mode: "structured" },
      ],
    },
  ],
  [
    "a generation of every connector",
    "generate_ontology",
    {
      ontology: ONT,
      connectors_read: "every",
      connectors: [{ id: "c2", name: "Stripe", kind: "stripe" }],
      document_sets: [],
    },
  ],
  [
    "a generation of documents alone",
    "generate_ontology",
    {
      ontology: ONT,
      connectors_read: "none",
      connectors: [],
      document_sets: [{ id: "set1", name: "General" }],
    },
  ],
  [
    "the sample's draft",
    "generate_ontology",
    {
      ontology: ONT,
      connectors_read: "chosen",
      connectors: [{ id: "c9", name: "Acme Corp", kind: "postgres" }],
      document_sets: [{ id: "s9", name: "Acme Corp sample" }],
      sample: "acme",
    },
  ],
  [
    "a version made current",
    "set_current_version",
    {
      ontology: ONT,
      version: { id: "ver3", number: 3 },
      current: { id: "ver2", number: 2 },
    },
  ],
  [
    "a first version made current",
    "set_current_version",
    { ontology: ONT, version: { id: "ver1", number: 1 } },
  ],
  ["a load", "create_load", { ontology: ONT, version: { id: "gone" } }],
  [
    "a load made current",
    "set_current_load",
    {
      ontology: ONT,
      load: {
        id: "load5",
        version: { id: "ver2", number: 2 },
        status: "ready",
        created_at: "2026-11-01T09:00:00Z",
      },
      current: {
        id: "load0",
        version: { id: "ver2", number: 2 },
        status: "partial",
        created_at: "2026-11-01T09:00:00Z",
      },
    },
  ],
  [
    "a first load made current",
    "set_current_load",
    { ontology: ONT, load: { id: "load5", version: { id: "" } } },
  ],
  [
    "a preview",
    "revise_version",
    {
      ontology: ONT,
      version: { id: "ver3", number: 3 },
      edits: [
        { kind: "put", item_id: "rule.overdue" },
        { kind: "remove", item_id: "type.Old" },
      ],
      dry_run: true,
    },
  ],
  [
    "a revision saved",
    "revise_version",
    {
      ontology: ONT,
      version: { id: "ver3", number: 3 },
      edits: [{ kind: "remove", item_id: "type.Old" }],
      dry_run: false,
    },
  ],
  [
    "a sync",
    "sync_connector",
    { connector: { id: "c2", name: "Stripe", kind: "stripe" } },
  ],
  ["a sync of a connector gone", "sync_connector", { connector: { id: "c2" } }],
  ["the sample", "add_sample_data", { ontology: ONT }],
  [
    "a set",
    "create_document_set",
    { ontology: ONT, name: "Leases", description: "Signed leases" },
  ],
  [
    "a set without a description",
    "create_document_set",
    { ontology: ONT, name: "Leases" },
  ],
  [
    "a set renamed",
    "update_document_set",
    { ontology: ONT, set: { id: "set1", name: "General" }, name: "Signed" },
  ],
  [
    "a set described",
    "update_document_set",
    { ontology: ONT, set: { id: "set1", name: "General" }, description: "" },
  ],
  [
    "a set renamed and described",
    "update_document_set",
    {
      ontology: ONT,
      set: { id: "set1", name: "General" },
      name: "Signed",
      description: "Signed ones",
    },
  ],
  [
    "a document moved",
    "move_document",
    {
      ontology: ONT,
      document: { id: "doc1", name: "contract.md" },
      from: { id: "set1", name: "General" },
      to: { id: "set2", name: "Contracts" },
    },
  ],
  [
    "a document moved from a set unknown",
    "move_document",
    {
      ontology: ONT,
      document: { id: "doc1" },
      from: { id: "" },
      to: { id: "set2", name: "Contracts" },
    },
  ],
  [
    "a link",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: REVIEW,
      document: { id: "doc1", name: "contract.md" },
      decision: { kind: "link", entity_key: "scus_1", candidate: true },
    },
  ],
  [
    "a link to another entity",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: REVIEW,
      decision: { kind: "link", entity_key: "scus_9" },
    },
  ],
  [
    "no one",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: REVIEW,
      decision: { kind: "not_in_data" },
    },
  ],
  [
    "a value",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: {
        review_key: "r2",
        reason: "prop_conflict",
        settle: "prop",
        type: "Customer",
        entity_key: "scus_1",
        prop: "currency",
        values: [{ value: "GBP", doc_id: "doc1", quote: "billed in GBP" }],
        entities: [
          {
            type: "Customer",
            entity_key: "scus_1",
            name: "Globex Corporation",
          },
        ],
      },
      decision: { kind: "set_value", value: { text: "GBP" } },
    },
  ],
  [
    "no value",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: {
        review_key: "r2",
        reason: "prop_conflict",
        settle: "prop",
        type: "Customer",
        prop: "currency",
      },
      decision: { kind: "remove_value" },
    },
  ],
  [
    "an approval",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: {
        review_key: "r3",
        reason: "low_confidence",
        settle: "release",
        doc_id: "doc1",
        link: "promised_to",
        statement: '{"subject":{"type":"Commitment","name":"support"}}',
        statement_hash: "ab".repeat(32),
        section: 0,
        start: 4,
        end: 9,
        confidence: 0.72,
      },
      document: { id: "doc1", name: "contract.md" },
      decision: { kind: "approve" },
    },
  ],
  [
    "a reject",
    "settle_review",
    {
      ontology: ONT,
      load_id: "load1",
      review: { review_key: "r3", reason: "conflict", settle: "release" },
      decision: { kind: "reject" },
    },
  ],
  [
    "a runtime with its helper running",
    "set_assistant_runtime",
    { runtime: "claude_code", current: "api", online: true },
  ],
  [
    "a runtime whose helper isn't running",
    "set_assistant_runtime",
    { runtime: "claude_code", current: "api", online: false },
  ],
  [
    "back to the workspace's model",
    "set_assistant_runtime",
    { runtime: "api", current: "claude_code", online: true },
  ],
  [
    "a helper revoked",
    "revoke_assistant_helper",
    {
      host: { id: "01HOSTAAAAAAAAAAAAAAAAAAAA", name: "Ada's Mac" },
      runtimes: ["claude_code"],
      online: true,
    },
  ],
  [
    "a helper that runs nothing yet",
    "revoke_assistant_helper",
    { host: { id: "01HOSTAAAAAAAAAAAAAAAAAAAA" }, runtimes: [], online: false },
  ],
  [
    "a dashboard made",
    "create_dashboard",
    {
      name: "Invoices by status",
      description: "Its invoices, counted by status.",
      spec: {
        tiles: [
          { title: "Invoices by status", ontology: ONT, visual: "bar" },
          { title: "Customers", ontology: { id: "gone1" } },
        ],
      },
    },
  ],
  [
    "a dashboard of no tiles yet",
    "create_dashboard",
    { name: "Pipeline", spec: { tiles: [] } },
  ],
  [
    "a dashboard renamed",
    "update_dashboard",
    {
      dashboard: { id: "dash1", name: "Revenue", published: true },
      name: "Revenue by month",
    },
  ],
  [
    "a dashboard's tiles changed",
    "update_dashboard",
    {
      dashboard: { id: "dash1", name: "Revenue" },
      description: "",
      spec: { tiles: [{ title: "Paid", ontology: ONT }] },
    },
  ],
  [
    "a dashboard deleted",
    "delete_dashboard",
    { dashboard: { id: "dash1", name: "Revenue", pinned: true } },
  ],
  [
    "a dashboard pinned",
    "pin_dashboard",
    { dashboard: { id: "dash1", name: "Revenue" }, pinned: true },
  ],
  [
    "a dashboard unpinned",
    "pin_dashboard",
    {
      dashboard: { id: "dash1", name: "Revenue", pinned: true },
      pinned: false,
    },
  ],
  [
    "a dashboard published",
    "publish_dashboard",
    { dashboard: { id: "dash1", name: "Revenue" } },
  ],
  [
    "a published dashboard's snapshot refreshed",
    "publish_dashboard",
    { dashboard: { id: "dash1", name: "Revenue", published: true } },
  ],
  [
    "a dashboard unpublished",
    "unpublish_dashboard",
    { dashboard: { id: "dash1", published: true } },
  ],
  ["an automation set up", "create_automation", { name: "Weekly summary" }],
  [
    "an automation's steps changed",
    "update_automation",
    {
      automation: { id: "aut1", name: "Weekly", status: "active" },
      steps: true,
    },
  ],
  [
    "an automation renamed",
    "update_automation",
    {
      automation: { id: "aut1", name: "Weekly", status: "needs_approval" },
      name: "Mondays",
      steps: false,
    },
  ],
  [
    "an automation paused",
    "pause_automation",
    { automation: { id: "aut1", name: "Weekly", status: "active" } },
  ],
  [
    "an automation turned back on",
    "resume_automation",
    { automation: { id: "aut1", name: "Weekly", status: "paused" } },
  ],
  [
    "an automation deleted",
    "delete_automation",
    { automation: { id: "aut1", name: "Weekly" } },
  ],
  [
    "an automation run now",
    "run_automation",
    { automation: { id: "aut1", name: "Weekly", status: "active" } },
  ],
  [
    "a draft's questions answered",
    "answer_generation_questions",
    {
      ontology: ONT,
      generation: { id: "gen1", name: "Track renewals" },
      round: 1,
      answers: [
        {
          question_id: "Q1",
          header: "Customers",
          question: "Is a customer the company that pays?",
          options: ["Each person"],
        },
        {
          question_id: "Q2",
          question: "Which signals?",
          options: ["Unpaid invoices", "Falling usage"],
          text: "and churned seats",
        },
      ],
      use_defaults: true,
      defaulted: [
        {
          question_id: "Q3",
          header: "Cancelled",
          question: "Keep <b>cancelled</b> orders?",
          options: [],
          text: "No preference: decide from the data and the goal.",
        },
      ],
    },
  ],
  [
    "a draft's questions answered without defaults",
    "answer_generation_questions",
    {
      ontology: ONT,
      generation: { id: "gen1" },
      round: 2,
      answers: [{ question_id: "q1", question: "", options: [], text: "Yes" }],
      use_defaults: false,
    },
  ],
];

describe("parseCard", () => {
  it.each(DISPLAYS)("reads %s as the backend writes it", (_, op, display) => {
    const card = parseCard(op, JSON.stringify(display));
    expect(card?.operation).toBe(op);
    // What it keeps is what the backend wrote: a link's false candidate
    // is left out by the backend, and read as false.
    expect(card?.display).toEqual(
      op === "settle_review" &&
        (display as { decision: { kind: string } }).decision.kind === "link"
        ? {
            ...(display as object),
            decision: {
              candidate: false,
              ...(display as { decision: object }).decision,
            },
          }
        : display,
    );
  });

  it.each<[string, string, unknown]>([
    ["an operation it doesn't know", "upload_document", { ontology: ONT }],
    ["JSON that isn't an object", "create_load", "[1]"],
    ["text that isn't JSON", "create_load", "{"],
    [
      "an id that isn't one",
      "create_load",
      { ontology: { id: "../x" }, version: { id: "v" } },
    ],
    ["a version without its id", "create_load", { ontology: ONT, version: {} }],
    ["no ontology name and no database", "create_ontology", { name: "x" }],
    [
      "both databases",
      "create_ontology",
      {
        name: "x",
        database: { id: "t1" },
        new_database: { name: "d", slug: "d", project_id: "p1" },
      },
    ],
    [
      "a new database in two projects",
      "create_ontology",
      {
        name: "x",
        new_database: {
          name: "d",
          slug: "d",
          project_id: "p1",
          new_project: "P",
        },
      },
    ],
    [
      "a new database without its slug",
      "create_ontology",
      { name: "x", new_database: { name: "d", project_id: "p1" } },
    ],
    [
      "a generation reading something else",
      "generate_ontology",
      {
        ontology: ONT,
        connectors_read: "some",
        connectors: [],
        document_sets: [],
      },
    ],
    [
      "a generation with a bad connector",
      "generate_ontology",
      {
        ontology: ONT,
        connectors_read: "chosen",
        connectors: [{ id: 1 }],
        document_sets: [],
      },
    ],
    [
      "an API source without its mode",
      "generate_ontology",
      {
        ontology: ONT,
        connectors_read: "every",
        connectors: [],
        document_sets: [],
        event_sources: [{ connector: { id: "ev1" } }],
      },
    ],
    [
      "a current version that isn't one",
      "set_current_version",
      { ontology: ONT, version: { id: "v" }, current: { number: 2 } },
    ],
    [
      "a current load that isn't one",
      "set_current_load",
      { ontology: ONT, load: { id: "l", version: { id: "v" } }, current: {} },
    ],
    [
      "a revision without edits",
      "revise_version",
      { ontology: ONT, version: { id: "v" }, edits: [], dry_run: true },
    ],
    [
      "an edit of another kind",
      "revise_version",
      {
        ontology: ONT,
        version: { id: "v" },
        edits: [{ kind: "drop", item_id: "x" }],
        dry_run: true,
      },
    ],
    [
      "a revision without its dry run",
      "revise_version",
      {
        ontology: ONT,
        version: { id: "v" },
        edits: [{ kind: "put", item_id: "x" }],
      },
    ],
    ["a sync of no connector", "sync_connector", {}],
    ["the sample of no ontology", "add_sample_data", {}],
    ["a set without a name", "create_document_set", { ontology: ONT }],
    [
      "a set change of nothing",
      "update_document_set",
      { ontology: ONT, set: { id: "s" } },
    ],
    [
      "a set renamed to nothing",
      "update_document_set",
      { ontology: ONT, set: { id: "s" }, name: "" },
    ],
    [
      "a description that isn't text",
      "update_document_set",
      { ontology: ONT, set: { id: "s" }, description: 3 },
    ],
    [
      "a move to nowhere",
      "move_document",
      { ontology: ONT, document: { id: "d" }, from: { id: "s" } },
    ],
    [
      "a decision of another kind",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: REVIEW,
        decision: { kind: "merge" },
      },
    ],
    [
      "a link without its entity",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: REVIEW,
        decision: { kind: "link" },
      },
    ],
    [
      "a value of two kinds",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: REVIEW,
        decision: { kind: "set_value", value: { text: "a", number: 1 } },
      },
    ],
    [
      "an integer that isn't one",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: REVIEW,
        decision: { kind: "set_value", value: { integer: "1.5" } },
      },
    ],
    [
      "a review settled some other way",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: { ...REVIEW, settle: "merge" },
        decision: { kind: "not_in_data" },
      },
    ],
    [
      "a review with candidates that aren't text",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: { ...REVIEW, candidates: [1] },
        decision: { kind: "not_in_data" },
      },
    ],
    ...[
      { type: "Customer" },
      { type: "Customer", entity_key: "k", norm_name: "n" },
      { entity_key: "k" },
      "scus_1",
    ].map((entity): [string, string, unknown] => [
      `a review entity ${JSON.stringify(entity)}`,
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: { ...REVIEW, entities: [entity] },
        decision: { kind: "not_in_data" },
      },
    ]),
    [
      "a document that isn't one",
      "settle_review",
      {
        ontology: ONT,
        load_id: "l",
        review: REVIEW,
        document: {},
        decision: { kind: "not_in_data" },
      },
    ],
  ])("refuses %s", (_, op, display) => {
    expect(
      parseCard(
        op,
        typeof display === "string" ? display : JSON.stringify(display),
      ),
    ).toBeUndefined();
  });

  it("keeps only what each field may hold", () => {
    const card = parseCard("settle_review", {
      ontology: { id: "ont1", name: 7 },
      load_id: "l",
      review: {
        ...REVIEW,
        section: -1,
        start: 1.5,
        end: 3,
        confidence: "high",
        values: [{ value: 3 }],
      },
      decision: { kind: "set_value", value: { integer: "-12" } },
      extra: true,
    });
    expect(card).toEqual({
      operation: "settle_review",
      display: {
        ontology: { id: "ont1" },
        load_id: "l",
        review: { ...REVIEW, end: 3, values: [{ value: 3 }] },
        decision: { kind: "set_value", value: { integer: "-12" } },
      },
    });
  });
});

describe("cardView", () => {
  const view = (op: string, display: unknown) =>
    cardView(parseCard(op, display) as AgentCard);
  const of = (name: string) => {
    const [, op, display] = DISPLAYS.find(([n]) => n === name)!;
    return view(op, display);
  };

  it("words every operation, naming what Foundry resolved", () => {
    const shown = DISPLAYS.map(([name, op, display]) => {
      const v = view(op, display);
      return [name, v.title, v.run, v.target];
    });
    expect(shown).toEqual([
      [
        "an ontology on a database",
        "Create the ontology “Renewals”",
        "Create",
        "sales",
      ],
      [
        "an ontology on a new database in a new project",
        "Create the ontology “Renewals”",
        "Create",
        "Renewals db",
      ],
      [
        "an ontology on a new database in a project",
        "Create the ontology “Renewals”",
        "Create",
        "Renewals db",
      ],
      [
        "a generation of chosen sources",
        "Draft a new version of “Sales graph”",
        "Generate",
        "Sales graph",
      ],
      [
        "a generation of every connector",
        "Draft a new version of “Sales graph”",
        "Generate",
        "Sales graph",
      ],
      [
        "a generation of documents alone",
        "Draft a new version of “Sales graph”",
        "Generate",
        "Sales graph",
      ],
      [
        "the sample's draft",
        "Draft the sample company’s version of “Sales graph”",
        "Generate",
        "Sales graph",
      ],
      [
        "a version made current",
        "Make v3 current for “Sales graph”",
        "Make current",
        "Sales graph",
      ],
      [
        "a first version made current",
        "Make v1 current for “Sales graph”",
        "Make current",
        "Sales graph",
      ],
      ["a load", "Load a version of “Sales graph”", "Load", "Sales graph"],
      [
        "a load made current",
        "Make the v2 load current for “Sales graph”",
        "Make current",
        "Sales graph",
      ],
      [
        "a first load made current",
        "Make a load current for “Sales graph”",
        "Make current",
        "Sales graph",
      ],
      [
        "a preview",
        "Preview 2 edits to v3 of “Sales graph”",
        "Preview",
        "Sales graph",
      ],
      [
        "a revision saved",
        "Save 1 edit to v3 of “Sales graph” as a new version",
        "Save version",
        "Sales graph",
      ],
      ["a sync", "Sync “Stripe”", "Sync", "Stripe"],
      ["a sync of a connector gone", "Sync c2", "Sync", "c2"],
      [
        "the sample",
        "Add the sample company’s data for “Sales graph”",
        "Add sample data",
        "Sales graph",
      ],
      [
        "a set",
        "Create the document set “Leases”",
        "Create set",
        "Sales graph",
      ],
      [
        "a set without a description",
        "Create the document set “Leases”",
        "Create set",
        "Sales graph",
      ],
      [
        "a set renamed",
        "Rename the set “General” to “Signed”",
        "Save",
        "General",
      ],
      [
        "a set described",
        "Change the description of the set “General”",
        "Save",
        "General",
      ],
      [
        "a set renamed and described",
        "Rename and describe the set “General”",
        "Save",
        "General",
      ],
      [
        "a document moved",
        "Move “contract.md” to “Contracts”",
        "Move",
        "contract.md",
      ],
      [
        "a document moved from a set unknown",
        "Move doc1 to “Contracts”",
        "Move",
        "doc1",
      ],
      ["a link", "Link “globexx” to Globex Corporation", "Link", "Sales graph"],
      [
        "a link to another entity",
        "Link “globexx” to scus_9",
        "Link",
        "Sales graph",
      ],
      [
        "no one",
        "Mark “globexx” as not in the data",
        "Mark not in data",
        "Sales graph",
      ],
      [
        "a value",
        "Set Globex Corporation’s currency to “GBP”",
        "Set value",
        "Sales graph",
      ],
      ["no value", "Remove Customer’s currency", "Remove value", "Sales graph"],
      ["an approval", "Approve the held statement", "Approve", "Sales graph"],
      ["a reject", "Reject the held statement", "Reject", "Sales graph"],
      [
        "a runtime with its helper running",
        "Run new Assistant threads on your Claude subscription",
        "Switch",
        "this workspace",
      ],
      [
        "a runtime whose helper isn't running",
        "Run new Assistant threads on your Claude subscription",
        "Switch",
        "this workspace",
      ],
      [
        "back to the workspace's model",
        "Run new Assistant threads on the workspace’s AI model",
        "Switch",
        "this workspace",
      ],
      [
        "a helper revoked",
        "Revoke the helper “Ada's Mac”",
        "Revoke",
        "Ada's Mac",
      ],
      [
        "a helper that runs nothing yet",
        "Revoke the helper 01HOSTAAAAAAAAAAAAAAAAAAAA",
        "Revoke",
        "01HOSTAAAAAAAAAAAAAAAAAAAA",
      ],
      [
        "a dashboard made",
        "Make the dashboard “Invoices by status”",
        "Create",
        "Invoices by status",
      ],
      [
        "a dashboard of no tiles yet",
        "Make the dashboard “Pipeline”",
        "Create",
        "Pipeline",
      ],
      [
        "a dashboard renamed",
        "Change the dashboard “Revenue”",
        "Save",
        "Revenue",
      ],
      [
        "a dashboard's tiles changed",
        "Change the dashboard “Revenue”",
        "Save",
        "Revenue",
      ],
      [
        "a dashboard deleted",
        "Delete the dashboard “Revenue”",
        "Delete",
        "Revenue",
      ],
      [
        "a dashboard pinned",
        "Pin the dashboard “Revenue” for everyone",
        "Pin",
        "Revenue",
      ],
      [
        "a dashboard unpinned",
        "Unpin the dashboard “Revenue” for everyone",
        "Unpin",
        "Revenue",
      ],
      [
        "a dashboard published",
        "Publish the dashboard “Revenue” at a public link",
        "Publish",
        "Revenue",
      ],
      [
        "a published dashboard's snapshot refreshed",
        "Refresh the public snapshot of “Revenue”",
        "Publish",
        "Revenue",
      ],
      [
        "a dashboard unpublished",
        "Unpublish the dashboard dash1",
        "Unpublish",
        "dash1",
      ],
      [
        "an automation set up",
        "Set up the automation “Weekly summary”",
        "Create and turn on",
        "Weekly summary",
      ],
      [
        "an automation's steps changed",
        "Change the automation “Weekly”",
        "Save",
        "Weekly",
      ],
      [
        "an automation renamed",
        "Change the automation “Weekly”",
        "Save",
        "Weekly",
      ],
      [
        "an automation paused",
        "Pause the automation “Weekly”",
        "Pause",
        "Weekly",
      ],
      [
        "an automation turned back on",
        "Turn the automation “Weekly” back on",
        "Turn on",
        "Weekly",
      ],
      [
        "an automation deleted",
        "Delete the automation “Weekly”",
        "Delete",
        "Weekly",
      ],
      [
        "an automation run now",
        "Run the automation “Weekly” now",
        "Run now",
        "Weekly",
      ],
      [
        "a draft's questions answered",
        "Answer the draft’s questions for “Sales graph”",
        "Send answers",
        "Sales graph",
      ],
      [
        "a draft's questions answered without defaults",
        "Answer the draft’s questions for “Sales graph”",
        "Send answers",
        "Sales graph",
      ],
    ]);
  });

  it("shows each question beside its answer, and links the draft's own questions", () => {
    const v = of("a draft's questions answered");
    expect(v.facts).toEqual([
      { label: "Ontology", value: "Sales graph" },
      {
        label: "Draft",
        entities: [
          { text: "Track renewals", to: "/ontology/ont1/generations/gen1" },
        ],
      },
    ]);
    expect(v.answers).toEqual([
      {
        name: "Customers",
        question: "Is a customer the company that pays?",
        answer: "Each person",
      },
      {
        name: "Question 2",
        question: "Which signals?",
        answer: "Unpaid invoices, Falling usage, and “and churned seats”",
      },
      {
        name: "Cancelled",
        question: "Keep <b>cancelled</b> orders?",
        answer:
          "Left to the agent: “No preference: decide from the data and the goal.”",
        left: true,
      },
    ]);
    expect(v.does).toContain("take the agent’s recommended answer");
    const bare = of("a draft's questions answered without defaults");
    expect(bare.does).not.toContain("recommended");
    expect(bare.answers).toEqual([
      { name: "Question 1", question: "", answer: "“Yes”" },
    ]);
    expect(bare.facts[1]).toEqual({
      label: "Draft",
      entities: [{ text: "The draft", to: "/ontology/ont1/generations/gen1" }],
    });
    // An answer it can't read makes no card.
    for (const display of [
      {
        ...(DISPLAYS.find(
          ([n]) => n === "a draft's questions answered",
        )![2] as object),
        use_defaults: "yes",
      },
      {
        ontology: ONT,
        generation: { id: "gen1" },
        round: 1,
        answers: [{ question_id: "q1", question: "Q", options: [] }],
        use_defaults: false,
      },
      {
        ontology: ONT,
        generation: { id: "gen1" },
        round: 1,
        answers: [{ question_id: "q1", question: "Q", options: "A" }],
        use_defaults: false,
      },
      {
        ontology: ONT,
        generation: { id: "gen1" },
        answers: [],
        use_defaults: true,
      },
      {
        ontology: ONT,
        generation: { id: "../x" },
        round: 1,
        answers: [],
        use_defaults: true,
      },
    ])
      expect(parseCard("answer_generation_questions", display)).toBeUndefined();
  });

  it("names an automation's state, and whether an edit approves its steps", () => {
    const value = (name: string, label: string) =>
      of(name).facts.find((f) => f.label === label);
    expect(value("an automation paused", "Automation")).toEqual({
      label: "Automation",
      value: "Weekly · on",
    });
    expect(value("an automation renamed", "Automation")).toEqual({
      label: "Automation",
      value: "Weekly · waits for approval",
    });
    expect(value("an automation renamed", "New name")).toEqual({
      label: "New name",
      value: "“Mondays”",
    });
    expect(of("an automation renamed").does).toContain("approves nothing");
    expect(of("an automation's steps changed").does).toContain(
      "your click approves its steps",
    );
    expect(of("an automation set up").cost).toBe(
      "Its Assistant steps spend the workspace’s automations budget.",
    );
    // A status this page doesn't know is left out.
    expect(
      parseCard(
        "pause_automation",
        '{"automation":{"id":"aut1","status":"broken"}}',
      )?.display,
    ).toEqual({ automation: { id: "aut1" } });
    expect(parseCard("update_automation", '{"automation":{"id":"aut1"}}')).toBe(
      undefined,
    );
  });

  it("names a dashboard's state and its tiles' ontologies", () => {
    const facts = (name: string) =>
      Object.fromEntries(
        of(name).facts.map((f) => [f.label, "value" in f ? f.value : ""]),
      );
    expect(facts("a dashboard made")).toEqual({
      Name: "“Invoices by status” (as the Assistant wrote it)",
      Tiles: "Invoices by status (Sales graph), Customers (gone1)",
    });
    expect(facts("a dashboard of no tiles yet").Tiles).toBe("None");
    expect(facts("a dashboard renamed")).toEqual({
      Dashboard: "Revenue · at a public link",
      "New name": "“Revenue by month”",
    });
    expect(facts("a dashboard's tiles changed")).toEqual({
      Dashboard: "Revenue",
      "New description": "None",
      Tiles: "Paid (Sales graph)",
    });
    expect(facts("a dashboard deleted").Dashboard).toBe(
      "Revenue · pinned for everyone",
    );
    expect(of("a dashboard published").caution).toBe(
      "Anyone who has the link sees this data.",
    );
  });

  it("says where new threads run, and that a helper must run first", () => {
    const facts = (name: string) =>
      Object.fromEntries(
        of(name).facts.map((f) => [f.label, "value" in f ? f.value : ""]),
      );
    expect(facts("a runtime with its helper running")).toEqual({
      Now: "Foundry’s AI model",
      "From then on": "My Claude subscription",
      Helper: "Running",
    });
    expect(of("a runtime with its helper running").caution).toBeUndefined();
    expect(of("a runtime whose helper isn't running").caution).toBe(
      "No helper running Claude Code is online: start it on your computer before you send.",
    );
    expect(facts("back to the workspace's model")).toEqual({
      Now: "My Claude subscription",
      "From then on": "Foundry’s AI model",
    });
    expect(of("back to the workspace's model").does).toMatch(
      /count toward its Assistant budget/,
    );
    expect(facts("a helper revoked")).toEqual({
      Helper: "Ada's Mac",
      Runs: "Claude Code",
      Status: "Running",
    });
    expect(facts("a helper that runs nothing yet")).toMatchObject({
      Runs: "Nothing yet",
      Status: "Not running",
    });
  });

  it("refuses a runtime or helper it doesn't know", () => {
    for (const display of [
      { runtime: "gemini", current: "api", online: true },
      { runtime: "claude_code", current: "api" },
      { runtime: "claude_code", current: "", online: true },
    ])
      expect(parseCard("set_assistant_runtime", display)).toBeUndefined();
    for (const display of [
      { host: { id: "../x" }, runtimes: [], online: true },
      { host: { id: "h" }, runtimes: ["api"], online: true },
      { host: { id: "h" }, online: true },
      { host: { id: "h" }, runtimes: [], online: "yes" },
    ])
      expect(parseCard("revoke_assistant_helper", display)).toBeUndefined();
  });

  it("lists what it acts on", () => {
    const facts = (name: string) =>
      Object.fromEntries(
        of(name).facts.map((f) => [
          f.label,
          "value" in f ? f.value : f.entities.map((e) => e.text).join(", "),
        ]),
      );
    expect(facts("an ontology on a database")).toEqual({
      Name: "Renewals",
      Database: "sales",
    });
    expect(facts("an ontology on a new database in a new project")).toEqual({
      Name: "Renewals",
      Database: "New: Renewals db (renewals)",
      Project: "New: Contracts",
    });
    expect(facts("an ontology on a new database in a project").Project).toBe(
      "p1",
    );
    expect(facts("a generation of chosen sources")).toEqual({
      Ontology: "Sales graph",
      Goal: "Track renewals",
      Connections: "Shop database, gone1",
      "Document sets": "General, Granola / Notes",
      "API sources": "Notes API as documents, gone2 as rows",
    });
    expect(facts("a generation of every connector")).toMatchObject({
      Goal: "None: the agent decides",
      Connections: "Every connection: Stripe",
      "Document sets": "None",
    });
    expect(
      view("generate_ontology", {
        ontology: ONT,
        connectors_read: "every",
        connectors: [],
        document_sets: [],
        event_sources: [{ connector: { id: "e" }, mode: "LATER" }],
      }).facts.map((f) => ("value" in f ? f.value : "")),
    ).toEqual([
      "Sales graph",
      "None: the agent decides",
      "Every connection (none yet)",
      "None",
      "e (LATER)",
    ]);
    expect(facts("a generation of documents alone").Connections).toBe(
      "None: documents only",
    );
    expect(facts("a version made current")["Current now"]).toBe("v2");
    expect(facts("a first version made current")["Current now"]).toBe(
      "None yet",
    );
    expect(facts("a load made current")).toEqual({
      Ontology: "Sales graph",
      Load: `v2 load, ready, started ${formatDate("2026-11-01T09:00:00Z")}`,
      "Current now": `v2 load, partial, started ${formatDate("2026-11-01T09:00:00Z")}`,
    });
    expect(facts("a first load made current")).toMatchObject({
      Load: "A load",
      "Current now": "None yet",
    });
    expect(facts("a preview").Edits).toBe(
      "Change rule.overdue; Remove type.Old",
    );
    expect(facts("a sync")).toEqual({ Connection: "Stripe", Kind: "Stripe" });
    expect(facts("a sync of a connector gone")).toEqual({ Connection: "c2" });
    expect(facts("a set without a description").Description).toBe("None");
    expect(facts("a set described").Description).toBe("Cleared");
    expect(facts("a document moved from a set unknown").From).toBe("Unknown");
    expect(facts("a link")).toEqual({
      Ontology: "Sales graph",
      Document: "contract.md",
      Review: "Names something that isn’t in your data",
      "Links to": "Globex Corporation",
      Suggested: "Globex Corporation",
    });
    expect(facts("a link to another entity")).toMatchObject({
      "Links to": "scus_9",
      Suggested: "Globex Corporation",
    });
    expect(facts("a value").Entity).toBe("Globex Corporation");
    expect(facts("an approval")).toMatchObject({
      Statement: "Commitment “support” promised_to",
    });
  });

  it("names a review's entities, each opening its page", () => {
    const facts = of("a link").facts;
    const entities = (label: string) => {
      const f = facts.find((x) => x.label === label);
      return f && "entities" in f ? f.entities : undefined;
    };
    expect(entities("Links to")).toEqual([
      { text: "Globex Corporation", to: GLOBEX },
    ]);
    expect(entities("Suggested")).toEqual([
      { text: "Globex Corporation", to: GLOBEX },
    ]);
    // One the review doesn't name is its identity alone, with no page.
    expect(reviewEntity("ont1", REVIEW as never, "scus_9")).toEqual({
      text: "scus_9",
    });
    // A type without a key is named by its normalized name.
    expect(
      reviewEntity(
        "ont1",
        {
          ...REVIEW,
          settle: "resolve",
          type: "Person",
          entities: [{ type: "Person", norm_name: "ada" }],
        },
        "ada",
      ),
    ).toEqual({ text: "ada", to: "/explore/ont1/entities/UGVyc29uAG46YWRh" });
    // Another type's entity of the same identity is not this one.
    expect(
      reviewEntity(
        "ont1",
        {
          ...REVIEW,
          settle: "resolve",
          entities: [{ type: "Supplier", entity_key: "scus_1", name: "X" }],
        },
        "scus_1",
      ),
    ).toEqual({ text: "scus_1" });
  });

  it("warns when a draft would read the sample's made-up data", () => {
    const sample = {
      id: "c9",
      name: "Acme Corp",
      kind: "postgres",
      sample: true,
    };
    expect(
      view("generate_ontology", {
        ontology: ONT,
        connectors_read: "chosen",
        connectors: [{ id: "c1", name: "Shop database" }, sample],
        document_sets: [],
      }).caution,
    ).toBe(
      "Acme Corp is the made-up sample company’s data: this draft would hold its records beside yours.",
    );
    expect(
      view("generate_ontology", {
        ontology: ONT,
        connectors_read: "chosen",
        connectors: [sample],
        document_sets: [{ id: "s9", name: "Acme Corp sample" }],
        sample: "acme",
      }).caution,
    ).toBeUndefined();
    expect(of("a generation of chosen sources").caution).toBeUndefined();
    expect(
      parseCard(
        "generate_ontology",
        JSON.stringify({
          ontology: ONT,
          connectors_read: "chosen",
          connectors: [{ ...sample, sample: "yes" }],
          document_sets: [],
        }),
      )?.display,
    ).toMatchObject({ connectors: [{ id: "c9", name: "Acme Corp" }] });
  });

  it("says what running it costs, and what to check", () => {
    expect(of("a load").cost).toMatch(/at most \$\d+ for this load/);
    expect(of("a sync").cost).toBeUndefined();
    expect(of("a link").caution).toBeUndefined();
    expect(of("a link to another entity").caution).toBe(
      "This entity isn’t one the review suggested.",
    );
    expect(of("a value").caution).toMatch(/every later load/);
    expect(of("a link").quote).toBe("Globexx");
    expect(of("an approval").does).toBe(
      "Writes the statement into the current load now.",
    );
    expect(of("a reject").does).toMatch(/Writes nothing/);
    expect(of("a value").does).toMatch(/Replaces the property’s value/);
    expect(of("an ontology on a new database in a project").does).toMatch(
      /new Helix database/,
    );
    expect(of("a preview").does).toMatch(/saves nothing/);
    expect(of("the sample's draft").does).toMatch(/ready-made/);
  });

  it("says values and statements in words", () => {
    expect(valueText({ text: "GBP" })).toBe("“GBP”");
    expect(valueText({ integer: "12" })).toBe("12");
    expect(valueText({ number: 1.5 })).toBe("1.5");
    expect(valueText({ boolean: false })).toBe("false");
    expect(
      statementText(
        '{"subject":{"type":"Contract","name":"MSA"},"object":{"type":"Customer"}}',
        "signed_with",
      ),
    ).toBe("Contract “MSA” signed_with Customer");
    expect(statementText('{"subject":{}}')).toBeUndefined();
    expect(statementText("[")).toBeUndefined();
    expect(statementText()).toBeUndefined();
  });
});

describe("resultLink", () => {
  const ran = (
    operation: string,
    display: unknown,
    result: unknown,
    target = "ontology:ws1/ont1",
  ): FoundryAgentAction => ({
    id: "a",
    operation,
    target,
    displayJson: JSON.stringify(display),
    status: "succeeded",
    resultJson: JSON.stringify({ content_trust: "untrusted", result }),
  });
  const d = (name: string) => DISPLAYS.find(([n]) => n === name)![2];

  it.each<[string, FoundryAgentAction, unknown]>([
    [
      "a created ontology",
      ran(
        "create_ontology",
        d("an ontology on a database"),
        { id: "ont9" },
        "tenant:t1",
      ),
      { to: "/ontology/ont9", label: "Open the ontology" },
    ],
    [
      "a draft",
      ran("generate_ontology", d("a generation of every connector"), {
        id: "gen1",
      }),
      { to: "/ontology/ont1/generations/gen1", label: "Open the draft" },
    ],
    [
      "a preview",
      ran("revise_version", d("a preview"), { id: "gen2" }),
      { to: "/ontology/ont1/generations/gen2", label: "Open the preview" },
    ],
    [
      "a revision",
      ran("revise_version", d("a revision saved"), { id: "gen2" }),
      { to: "/ontology/ont1/generations/gen2", label: "Open the revision" },
    ],
    [
      "a version made current",
      ran("set_current_version", d("a version made current"), { id: "ver3" }),
      { to: "/ontology/ont1/versions/ver3", label: "Open the version" },
    ],
    [
      "a load made current",
      ran("set_current_load", d("a load made current"), { id: "load5" }),
      { to: "/ontology/ont1/loads/load5", label: "Open the load" },
    ],
    [
      "the sample",
      ran("add_sample_data", d("the sample"), { connector_id: "c" }),
      { to: "/ontology/ont1", label: "Open the ontology" },
    ],
    [
      "a sync",
      ran("sync_connector", d("a sync"), { id: "sync1" }, "connector:ws1/c2"),
      { to: "/data/connections/c2", label: "Open the connection" },
    ],
    [
      "a set",
      ran("create_document_set", d("a set"), { id: "set9" }),
      { to: "/data", label: "Open Data" },
    ],
    [
      "a set changed",
      ran("update_document_set", d("a set renamed"), { id: "set1" }),
      { to: "/data", label: "Open Data" },
    ],
    [
      "a moved document",
      ran("move_document", d("a document moved"), { document: { id: "doc1" } }),
      { to: "/data/documents/doc1", label: "Open the document" },
    ],
    [
      "a moved document it can't name",
      ran("move_document", d("a document moved"), { document: "doc1" }),
      undefined,
    ],
    [
      "a settled review",
      ran("settle_review", d("a link"), { id: "dec1" }),
      { to: "/ontology/ont1/documents/doc1", label: "Open the document" },
    ],
    [
      "a settled review of no document",
      ran("settle_review", d("a reject"), { id: "dec1" }),
      undefined,
    ],
    [
      "a created ontology it can't name",
      ran(
        "create_ontology",
        d("an ontology on a database"),
        { id: "../x" },
        "tenant:t1",
      ),
      undefined,
    ],
    [
      "a runtime chosen",
      ran(
        "set_assistant_runtime",
        d("a runtime with its helper running"),
        { runtime: "claude_code", online: ["claude_code"] },
        "workspace:ws1",
      ),
      { to: "/settings/ai", label: "Open Settings › AI models" },
    ],
    [
      "a helper revoked",
      ran(
        "revoke_assistant_helper",
        d("a helper revoked"),
        { id: "01HOSTAAAAAAAAAAAAAAAAAAAA" },
        "workspace:ws1",
      ),
      { to: "/settings/ai", label: "Open Settings › AI models" },
    ],
    [
      "a dashboard made",
      ran(
        "create_dashboard",
        d("a dashboard made"),
        { id: "dash9" },
        "workspace:ws1",
      ),
      { to: "/dashboards/dash9", label: "Open the dashboard" },
    ],
    ...(
      [
        ["update_dashboard", "a dashboard renamed"],
        ["pin_dashboard", "a dashboard pinned"],
        ["publish_dashboard", "a dashboard published"],
        ["unpublish_dashboard", "a dashboard unpublished"],
      ] as const
    ).map(([op, name]): [string, FoundryAgentAction, unknown] => [
      name,
      ran(op, d(name), { id: "dash1" }, "dashboard:ws1/dash1"),
      { to: "/dashboards/dash1", label: "Open the dashboard" },
    ]),
    [
      "an automation set up",
      ran(
        "create_automation",
        d("an automation set up"),
        { id: "aut9" },
        "workspace:ws1",
      ),
      { to: "/automations/aut9", label: "Open the automation" },
    ],
    ...(
      [
        ["update_automation", "an automation's steps changed"],
        ["pause_automation", "an automation paused"],
        ["resume_automation", "an automation turned back on"],
      ] as const
    ).map(([op, name]): [string, FoundryAgentAction, unknown] => [
      name,
      ran(op, d(name), { id: "aut1" }, "automation:ws1/aut1"),
      { to: "/automations/aut1", label: "Open the automation" },
    ]),
    [
      "an automation run now",
      ran(
        "run_automation",
        d("an automation run now"),
        { id: "run3", automation_id: "aut1" },
        "automation:ws1/aut1",
      ),
      { to: "/automations/aut1/runs/run3", label: "Open the run" },
    ],
    [
      "a draft answered",
      ran("answer_generation_questions", d("a draft's questions answered"), {
        id: "gen1",
      }),
      { to: "/ontology/ont1/generations/gen1", label: "Open the draft" },
    ],
    [
      "a deleted automation",
      ran(
        "delete_automation",
        d("an automation deleted"),
        { deleted: true, automation_id: "aut1" },
        "automation:ws1/aut1",
      ),
      undefined,
    ],
    [
      "a deleted dashboard",
      ran(
        "delete_dashboard",
        d("a dashboard deleted"),
        { deleted: true, dashboard_id: "dash1" },
        "dashboard:ws1/dash1",
      ),
      undefined,
    ],
    [
      "a card it can't read",
      ran("generate_ontology", {}, { id: "gen1" }),
      undefined,
    ],
  ])("opens %s", (_, action, link) => {
    expect(resultLink(action)).toEqual(link);
  });
});

describe("connectLink", () => {
  const step = (
    output: unknown,
    name = "helix_foundry_connect",
  ): AgentStep => ({
    id: "t",
    name,
    input: {},
    result: { kind: "returned", output, truncated: false },
  });

  it("opens the kind's form in this app, whatever site the backend names", () => {
    expect(
      connectLink(
        step({
          url: "https://foundry.helix-db.com/data?add=slack&workspace=ws_1",
          kind: "slack",
          display_name: "Slack",
        }),
      ),
    ).toEqual({ to: "/data?add=slack", name: "Slack" });
    expect(
      connectLink(step({ url: "http://localhost:3000/data?add=postgres" })),
    ).toEqual({ to: "/data?add=postgres", name: "PostgreSQL" });
  });

  it.each<[string, AgentStep]>([
    [
      "another tool's step",
      step(
        { url: "https://f.example/data?add=slack" },
        "helix_foundry_list_connectors",
      ),
    ],
    ["another page", step({ url: "https://f.example/settings?add=slack" })],
    ["a kind that isn't one", step({ url: "https://f.example/data?add=../x" })],
    ["no kind", step({ url: "https://f.example/data" })],
    ["no address", step({})],
    [
      "an address of another scheme",
      step({ url: "javascript:alert(1)//data?add=slack" }),
    ],
    ["output that isn't an object", step("https://f.example/data?add=slack")],
    [
      "a step that failed",
      {
        id: "t",
        name: "helix_foundry_connect",
        input: {},
        result: { kind: "failed", error: "no" },
      },
    ],
    [
      "a step still running",
      { id: "t", name: "helix_foundry_connect", input: {} },
    ],
  ])("gives no link for %s", (_, s) => {
    expect(connectLink(s)).toBeUndefined();
  });
});

describe("calls a limit kept from running", () => {
  it("are steps that weren't run, not failures", () => {
    const e = (seq: number, x: object) => ({
      seq,
      eventJson: JSON.stringify(x),
    });
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        e(1, {
          type: "tool_call",
          tool_use_id: "t9",
          name: "helix_foundry_ask",
          title: "",
          // A call cut at the reply's length limit: its raw text.
          input: '{"ontology_id":"ont1","quest',
        }),
        e(2, {
          type: "tool_result",
          tool_use_id: "t9",
          name: "helix_foundry_ask",
          is_error: true,
          not_run: "asks",
        }),
        e(3, {
          type: "tool_result",
          tool_use_id: "t10",
          name: "helix_foundry_ask",
          is_error: true,
          not_run: "input",
        }),
        // A reason this page doesn't know is no reason: the call failed.
        e(4, {
          type: "tool_result",
          tool_use_id: "t11",
          name: "helix_foundry_ask",
          is_error: true,
          error: "it stopped",
          not_run: "later",
        }),
      ],
      status: "running",
      lastSeq: 4,
    });
    expect(s.blocks).toEqual([
      {
        kind: "step",
        step: {
          id: "t9",
          name: "helix_foundry_ask",
          input: '{"ontology_id":"ont1","quest',
          result: { kind: "not_run", reason: "asks" },
        },
      },
      {
        kind: "step",
        step: {
          id: "t10",
          name: "helix_foundry_ask",
          input: null,
          result: { kind: "not_run", reason: "input" },
        },
      },
      {
        kind: "step",
        step: {
          id: "t11",
          name: "helix_foundry_ask",
          input: null,
          result: { kind: "failed", error: "it stopped" },
        },
      },
    ]);
  });

  it("words each reason for the person, never as the model was told", () => {
    expect(Object.keys(NOT_RUN_TEXT).sort()).toEqual(
      ["asked", "asks", "calls", "input", "length", "time"].sort(),
    );
    for (const text of Object.values(NOT_RUN_TEXT)) {
      expect(text).toMatch(/\.$/);
      expect(text).not.toMatch(/\b(you|Call it again|Tell the person)\b/);
    }
    expect(NOT_RUN_TEXT.input).not.toMatch(/limit/);
  });
});
