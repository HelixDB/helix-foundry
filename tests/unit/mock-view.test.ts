import { describe, expect, it } from "vitest";
import { joinKey } from "../../mock/graph/extract";
import { compile } from "../../mock/graph/rules";
import {
  MAX_CHECKED_FACTS,
  MAX_CHECKED_ROWS,
  admit,
  nameUnsafe,
  narrows,
  settled,
  settles,
  viewOf,
  widened,
  type Visibility,
} from "../../mock/graph/view";
import { parseAgentOntology } from "@/web/areas/ontology/model";

// The mock's view of a version, as the ontology agent's `mapping::view` works
// it out: who could have written each type, property and link, the ontology
// without what none of a reader's sources writes, and what a query can't ask.

const ONTOLOGY = `version = "v1"
[types.Customer]
description = "A customer."
props = { id = "string", full_name = "string", plan = "string", renewal = "string", region = "string" }
key = ["id"]
names = ["full_name"]
identifiers = { id = "code" }
[types.Invoice]
description = "A bill."
props = { id = "string", amount = "integer" }
key = ["id"]
[types.Contract]
description = "An agreement, named by its title."
[types.Ticket]
description = "A support request, named by its subject."
[links.party_to]
from = "Customer"
to = "Contract"
description = "The customer is a party to the contract."
[links.billed_to]
from = "Invoice"
to = "Customer"
description = "The invoice bills the customer."
cardinality = "one"
required = true
[links.referred_by]
from = "Customer"
to = "Customer"
description = "One customer sent us another."
[links.raised]
from = "Customer"
to = "Ticket"
description = "The customer raised the ticket."
[relations.partners]
description = "The two customers share a contract."
[rules.big]
description = "A large invoice."
datalog = "big(i) :- Invoice(i), i.amount > 100."
[rules.active]
description = "A customer party to a contract."
datalog = "active(c) :- party_to(c, _)."
[rules.quiet]
description = "A customer who raised nothing."
datalog = "quiet(c) :- Customer(c), not raised(c, _)."
[constraints.not_big]
description = "No invoice is large."
severity = "warn"
datalog = "violation(i) :- big(i)."
[constraints.must_be_active]
description = "Every customer has a contract."
severity = "warn"
datalog = "violation(c) :- Customer(c), not active(c)."
`;
const MAPPINGS = `version = "m1"
ontology = "v1"
[mappings.customers]
source = "stripe"
links = [{ link = "referred_by", from = "c", to = "by" }]
[mappings.customers.bind.c]
type = "Customer"
key = { id = "id" }
props = { full_name = "name", plan = "plan" }
missing = "create"
[mappings.customers.bind.by]
type = "Customer"
key = { id = "by" }
missing = "report"
[mappings.lines]
parent = "customers"
[mappings.invoices]
source = "stripe"
links = [{ link = "billed_to", from = "i", to = "c" }]
[mappings.invoices.bind.i]
type = "Invoice"
key = { id = "id" }
props = { amount = "amount" }
missing = "create"
[mappings.invoices.bind.c]
type = "Customer"
key = { id = "customer" }
missing = "report"
[mappings.crm]
source = "crm"
links = [{ link = "referred_by", from = "c", to = "by" }]
[mappings.crm.bind.c]
type = "Customer"
key = { id = "id" }
props = { full_name = "name", plan = "tier", region = "region" }
missing = "create"
[mappings.crm.bind.by]
type = "Customer"
key = { id = "by" }
missing = "report"
[mappings.tickets]
source = "helpdesk"
links = [{ link = "raised", from = "c", to = "t" }]
[mappings.tickets.bind.t]
type = "Ticket"
name = "subject"
missing = "create"
[mappings.tickets.bind.c]
type = "Customer"
key = { id = "customer" }
missing = "report"
[extractions.contracts]
documents = ["set_held", "set_other"]
links = ["party_to"]
[extractions.contracts.types.Contract]
create = true
[extractions.contracts.types.Customer]
props = ["plan", "renewal"]
`;
const held: Visibility = {
  sources: ["stripe"],
  documents: { only: ["held"] },
  sets: ["set_held"],
  enrichment: false,
  withheld: [],
};
const key = (t: string, p: string) => `${t}\u001f${p}`;

describe("a reader's view", () => {
  const view = viewOf(held, ONTOLOGY, MAPPINGS);

  it("says who could have written each thing", () => {
    expect(view.whole).toBe(false);
    expect(view.types.get("Customer")).toBe("partial");
    expect(view.types.get("Invoice")).toBe("full");
    expect(view.types.get("Contract")).toBe("partial");
    expect(view.types.get("Ticket")).toBeUndefined();
    expect(view.props.get(key("Customer", "plan"))).toBe("partial");
    expect(view.props.get(key("Customer", "id"))).toBe("partial");
    expect(view.props.get(key("Customer", "region"))).toBeUndefined();
    expect(view.props.get(key("Invoice", "amount"))).toBe("full");
    expect(view.links.get("billed_to")).toBe("full");
    expect(view.links.get("referred_by")).toBe("partial");
    expect(view.links.get("raised")).toBeUndefined();
    // A keyed type it doesn't hold whole reads by id; another keeps its name.
    expect(nameUnsafe(view, "Customer")).toBe(true);
    expect(nameUnsafe(view, "Invoice")).toBe(false);
    expect(nameUnsafe(view, "Contract")).toBe(false);
    expect(view.partial).toEqual(
      expect.arrayContaining(["Customer", "Customer.plan", "referred_by"]),
    );
    expect(view.holdsWhole("stripe")).toBe(true);
    expect(view.holdsWhole("crm")).toBe(false);
    expect(view.holdsWhole("foundry_enrichment")).toBe(false);
    expect(view.holdsRow("stripe", "customers", "c1")).toBe(true);
    expect(view.holdsRow("crm", "crm", "c1")).toBe(false);
    expect(view.holdsDocument("held")).toBe(true);
    expect(view.holdsDocument("other")).toBe(false);
  });

  it("projects an ontology that still reads, with nothing it can't name", () => {
    const parsed = parseAgentOntology(view.ontologyToml);
    if (!parsed.ok) throw new Error(parsed.error);
    expect(parsed.value.types.map((t) => t.name).sort()).toEqual([
      "Contract",
      "Customer",
      "Invoice",
    ]);
    const customer = parsed.value.types.find((t) => t.name === "Customer")!;
    expect(customer.props.map((p) => p.name)).not.toContain("region");
    expect(customer.names).toEqual([]);
    expect(view.ontologyToml).not.toContain('"code"');
    expect(parsed.value.links.map((l) => l.name).sort()).toEqual([
      "billed_to",
      "party_to",
      "referred_by",
    ]);
    // Only the rule over what is whole, and the constraint over it.
    expect(view.ontologyToml).toContain("rules.big");
    expect(view.ontologyToml).toContain("constraints.not_big");
    for (const gone of ["rules.active", "rules.quiet", "must_be_active"])
      expect(view.ontologyToml).not.toContain(gone);
    // The link's checks read a type that isn't whole.
    expect(view.ontologyToml).not.toContain("required");
    expect(view.ontologyToml).not.toContain("cardinality");
    expect(() => compile(view.ontologyToml)).not.toThrow();
    // Only what names the files: which mappings it holds would say which
    // sources it does.
    expect(view.mappingsToml).toContain('version = "m1"');
    expect(view.mappingsToml).toContain('ontology = "v1"');
    expect(view.mappingsToml).not.toContain("mappings.");
    expect(view.mappingsToml).not.toContain("extractions.");
  });

  it("is the version itself for a reader who holds it all", () => {
    const all = viewOf(
      {
        sources: ["stripe", "crm", "helpdesk"],
        documents: "all",
        sets: [],
        enrichment: false,
        withheld: [],
      },
      ONTOLOGY,
      MAPPINGS,
    );
    expect(all.whole).toBe(true);
    expect(all.ontologyToml).toBe(ONTOLOGY);
    expect(all.mappingsToml).toBe(MAPPINGS);
    expect(all.partial).toEqual([]);
    expect(all.holdsDocument("any")).toBe(true);
    expect(
      admit(all, { start: { type: "Customer" }, return: { count: {} } }),
    ).toBeUndefined();
  });

  it("leaves out what nothing it holds writes", () => {
    const nothing = viewOf(
      { ...held, sources: [], documents: { only: [] }, sets: [] },
      ONTOLOGY,
      MAPPINGS,
    );
    expect(nothing.types.size).toBe(0);
    expect(parseAgentOntology(nothing.ontologyToml).ok).toBe(true);
    expect(nothing.mappingsToml).not.toContain("mappings.");
    expect(nothing.mappingsToml).not.toContain("extractions.");
    // A route of sets it holds nothing of gives it nothing.
    const other = viewOf(
      { ...held, documents: { only: ["elsewhere"] }, sets: ["set_elsewhere"] },
      ONTOLOGY,
      MAPPINGS,
    );
    expect(other.types.get("Contract")).toBeUndefined();
    expect(other.props.get(key("Customer", "renewal"))).toBeUndefined();
    // A property decided from documents it doesn't hold, and what reads it.
    const withheld = viewOf(
      { ...held, withheld: [{ type: "Invoice", prop: "amount" }] },
      ONTOLOGY,
      MAPPINGS,
    );
    expect(withheld.props.get(key("Invoice", "amount"))).toBeUndefined();
    expect(withheld.ontologyToml).not.toContain("rules.big");
    expect(withheld.ontologyToml).not.toContain("constraints.not_big");
  });

  it("keeps a relation only while it holds every member link whole", () => {
    const ontology = `version = "v"
[types.A]
description = "An A."
props = { id = "string" }
key = ["id"]
[types.B]
description = "A B."
props = { id = "string" }
key = ["id"]
[links.whole]
from = "A"
to = "A"
description = "Whole."
implies = ["kept", "both", "hidden_end"]
[links.part]
from = "A"
to = "A"
description = "Part."
implies = ["both"]
[relations.kept]
description = "Over a link it holds whole."
[relations.both]
description = "Over one it holds whole and one it doesn't."
[relations.hidden_end]
description = "Naming a type it can't see."
from = "A"
to = "B"
`;
    const mappings = `version = "v"
ontology = "v"
[mappings.a]
source = "stripe"
links = [{ link = "whole", from = "a", to = "a" }, { link = "part", from = "a", to = "a" }]
[mappings.a.bind.a]
type = "A"
key = { id = "id" }
missing = "create"
[mappings.c]
source = "crm"
links = [{ link = "part", from = "a", to = "a" }]
[mappings.c.bind.a]
type = "A"
key = { id = "id" }
missing = "create"
[mappings.b]
source = "crm"
[mappings.b.bind.b]
type = "B"
key = { id = "id" }
missing = "create"
`;
    const seen = viewOf(held, ontology, mappings).ontologyToml;
    expect(seen).toContain("relations.kept");
    expect(seen).not.toContain("relations.both");
    expect(seen).not.toContain("relations.hidden_end");
    // What named them no longer does.
    expect(parseAgentOntology(seen).ok).toBe(true);
    expect(seen).not.toContain('"both"');
    expect(seen).not.toContain('"hidden_end"');
  });

  it("counts People enrichment's profiles as a source", () => {
    const mappings = `version = "v"
ontology = "v"
[mappings.people]
source = "shop"
[mappings.people.bind.p]
type = "Person"
key = { id = "id" }
missing = "create"
[enrichments.people]
type = "Person"
clues = { email = "email" }
props = { title = "current_title" }
work_history = { link = "worked_at", match = "domain" }
`;
    const ontology = `version = "v"
[types.Person]
description = "A person."
props = { id = "string", title = "string" }
key = ["id"]
[links.worked_at]
from = "Person"
to = "Person"
description = "Worked with."
`;
    const without = viewOf(
      { ...held, sources: ["shop"], documents: "all" },
      ontology,
      mappings,
    );
    expect(without.props.get(key("Person", "title"))).toBeUndefined();
    expect(without.links.get("worked_at")).toBeUndefined();
    expect(without.mappingsToml).not.toContain("enrichments");
    const withIt = viewOf(
      { ...held, sources: ["shop"], documents: "all", enrichment: true },
      ontology,
      mappings,
    );
    expect(withIt.whole).toBe(true);
    expect(withIt.holdsWhole("foundry_enrichment")).toBe(true);
  });

  it("hides a link rules derive whose facts it holds only some of", () => {
    const derived = viewOf(
      held,
      `version = "v"
[types.A]
description = "An A."
props = { id = "string" }
key = ["id"]
[links.next]
from = "A"
to = "A"
description = "Follows."
transitive = true
`,
      `version = "v"
ontology = "v"
[mappings.a]
source = "stripe"
links = [{ link = "next", from = "a", to = "a" }]
[mappings.a.bind.a]
type = "A"
key = { id = "id" }
missing = "create"
[mappings.b]
source = "crm"
links = [{ link = "next", from = "a", to = "a" }]
[mappings.b.bind.a]
type = "A"
key = { id = "id" }
missing = "report"
`,
    );
    expect(derived.links.get("next")).toBeUndefined();
  });
});

describe("what a query can ask", () => {
  const view = viewOf(held, ONTOLOGY, MAPPINGS);
  const rows = (columns: { at: number; prop?: string }[], order?: string) => ({
    rows: {
      columns,
      ...(order ? { order_by: { at: 0, prop: order } } : {}),
    },
  });

  it("refuses to test, order, group or count what it holds only some of", () => {
    const pointer = (q: Parameters<typeof admit>[1]) => admit(view, q)?.pointer;
    expect(
      pointer({ start: { type: "Customer" }, return: { count: {} } }),
    ).toBe("/start/type");
    expect(
      pointer({
        start: {
          type: "Customer",
          where: [{ prop: "plan", op: "=", value: "x" }],
        },
        return: rows([{ at: 0 }]),
      }),
    ).toBe("/start/where/0/prop");
    expect(
      pointer({
        start: { type: "Customer" },
        return: rows([{ at: 0 }], "plan"),
      }),
    ).toBe("/return/rows/order_by/prop");
    expect(
      pointer({
        start: { type: "Customer" },
        return: rows([{ at: 0 }], "name"),
      }),
    ).toBe("/return/rows/order_by/prop");
    // Its name is its type's: only a type held whole.
    expect(
      pointer({
        start: { type: "Invoice" },
        return: rows([{ at: 0 }], "name"),
      }),
    ).toBeUndefined();
    expect(
      pointer({
        start: { type: "Customer" },
        return: { group_count: { at: 0, prop: "plan" } },
      }),
    ).toBe("/start/type");
    expect(
      pointer({
        start: { type: "Invoice" },
        return: { group_count: { at: 0, prop: "id" } },
      }),
    ).toBeUndefined();
    expect(
      pointer({
        start: { type: "Invoice" },
        steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
        return: { aggregate: { fn: "sum", at: 0, prop: "amount" } },
      }),
    ).toBe("/steps/0/to");
    expect(
      pointer({
        start: { type: "Invoice" },
        return: {
          aggregate: {
            fn: "sum",
            at: 0,
            prop: "amount",
            group_by: { at: 0, prop: "name" },
          },
        },
      }),
    ).toBeUndefined();
    expect(
      pointer({
        start: { type: "Customer" },
        steps: [
          {
            via: "referred_by",
            dir: "out",
            to: "Customer",
            where: [{ prop: "plan", op: "=", value: "x" }],
          },
        ],
        return: rows([{ at: 0 }, { at: 1 }]),
      }),
    ).toBe("/steps/0/where/0/prop");
  });

  it("needs every position it must check among the columns", () => {
    expect(
      admit(view, {
        start: { type: "Invoice" },
        steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
        return: rows([{ at: 0 }]),
      })?.pointer,
    ).toBe("/return/rows/columns");
    expect(
      admit(view, {
        start: { type: "Customer" },
        steps: [{ via: "referred_by", dir: "out", to: "Customer" }],
        return: rows([{ at: 1 }]),
      })?.pointer,
    ).toBe("/return/rows/columns");
    expect(
      admit(view, {
        start: { type: "Invoice" },
        steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
        return: rows([{ at: 0 }, { at: 1 }]),
      }),
    ).toBeUndefined();
    // Its whole types read as anyone's.
    expect(
      admit(view, {
        start: {
          type: "Invoice",
          where: [{ prop: "amount", op: ">", value: 1 }],
        },
        return: rows([{ at: 0, prop: "amount" }], "amount"),
      }),
    ).toBeUndefined();
    expect(
      admit(view, { start: { type: "Invoice" }, return: { count: {} } }),
    ).toBeUndefined();
  });

  it("narrows what touches anything partly held, and doesn't page it", () => {
    const start = (type: string, offset?: number) => ({
      start: { type },
      return: { rows: { columns: [{ at: 0 }], ...(offset ? { offset } : {}) } },
    });
    expect(narrows(view, start("Customer"))).toBe(true);
    expect(narrows(view, start("Invoice"))).toBe(false);
    // A whole type with a column it holds only some of.
    const held2 = viewOf(
      { ...held, sources: ["stripe", "helpdesk"] },
      ONTOLOGY,
      MAPPINGS,
    );
    expect(held2.types.get("Invoice")).toBe("full");
    const column = (prop: string) => ({
      start: { type: "Invoice" },
      steps: [{ via: "billed_to", dir: "out" as const, to: "Customer" }],
      return: { rows: { columns: [{ at: 0 }, { at: 1, prop }] } },
    });
    expect(narrows(view, column("plan"))).toBe(true);
    expect(settles(view, "Customer", "plan")).toBe(true);
    expect(settles(view, "Customer", "name")).toBe(false);
    expect(settles(view, "Invoice", "amount")).toBe(false);
    // A count of what is whole, or a reader with it all, isn't narrowed.
    expect(
      narrows(view, { start: { type: "Invoice" }, return: { count: {} } }),
    ).toBe(false);
    // Where a page ended would say how much it can't see.
    expect(admit(view, start("Customer", 100))).toMatchObject({
      code: "not_visible",
      pointer: "/return/rows/offset",
    });
    expect(admit(view, start("Invoice", 100))).toBeUndefined();
    // Reading on past what it asked, less far when each fact takes a read.
    expect(widened(view, start("Customer"))).toBe(MAX_CHECKED_ROWS);
    expect(
      widened(view, {
        start: { type: "Customer" },
        steps: [{ via: "referred_by", dir: "out", to: "Customer" }],
        return: { rows: { columns: [{ at: 0 }, { at: 1 }] } },
      }),
    ).toBe(MAX_CHECKED_FACTS);
    // At most so many ids of what is only partly held.
    const ids = (n: number, type = "Customer") => ({
      start: { type, ids: Array.from({ length: n }, (_, i) => `id_${i}`) },
      return: { rows: { columns: [{ at: 0 }] } },
    });
    expect(admit(view, ids(100))).toBeUndefined();
    expect(admit(view, ids(101))).toMatchObject({
      code: "not_visible",
      pointer: "/start/ids",
    });
    expect(admit(view, ids(101, "Invoice"))).toBeUndefined();
  });

  it("settles a value from what it holds", () => {
    const quote = (documentId: string, value: string) => ({
      documentId,
      value,
    });
    const row = (source: string) => ({ source, mapping: source, rowKey: "c1" });
    expect(settled(view, "pro", [row("stripe")], [])).toEqual({
      from: "stored",
    });
    // A row it holds says it, whatever other rows do.
    expect(settled(view, "pro", [row("stripe"), row("crm")], [])).toEqual({
      from: "stored",
    });
    expect(
      settled(view, "pro", [row("crm"), row("stripe")], [quote("held", "pro")]),
    ).toEqual({ from: "stored" });
    expect(settled(view, "x", [row("crm")], [quote("held", "pro")])).toEqual({
      from: "quoted",
      value: "pro",
    });
    expect(
      settled(view, "x", [], [quote("held", "pro"), quote("other", "free")]),
    ).toEqual({ from: "quoted", value: "pro" });
    expect(
      settled(view, "x", [], [quote("held", "pro"), quote("held", "free")]),
    ).toEqual({ from: "none" });
    expect(settled(view, "x", [], [quote("other", "pro")])).toEqual({
      from: "none",
    });
    expect(settled(view, "x", [], [])).toEqual({ from: "none" });
  });
});

// A reader may hold some rows of a source (contract 4.32.0), told by one field
// of their stored row key, as the engine's `HeldRows`: a Slack grantee holds
// the rows of her channels.
describe("rows held by their key", () => {
  const ontology = `version = "v1"
[types.Person]
description = "Someone in Slack."
props = { user_id = "string", real_name = "string" }
key = ["user_id"]
names = ["real_name"]
[types.Channel]
description = "A channel."
props = { channel_id = "string", name = "string" }
key = ["channel_id"]
[types.Message]
description = "A message."
props = { message_id = "string", text = "string" }
key = ["message_id"]
[types.Reply]
description = "A reply in a message's thread."
props = { reply_id = "string" }
key = ["reply_id"]
[types.Day]
description = "A channel's day."
props = { day_id = "string" }
key = ["day_id"]
[links.posted_in]
from = "Message"
to = "Channel"
description = "The message was posted in the channel."
[links.member_of]
from = "Person"
to = "Channel"
description = "The person is in the channel."
`;
  const mappings = `version = "m1"
ontology = "v1"
[mappings.users]
source = "slack_users"
row_key = ["user_id"]
[mappings.users.bind.p]
type = "Person"
key = { user_id = "user_id" }
props = { real_name = "real_name" }
missing = "create"
[mappings.channels]
source = "slack_channels"
row_key = ["channel_id"]
[mappings.channels.bind.c]
type = "Channel"
key = { channel_id = "channel_id" }
props = { name = "name" }
missing = "create"
[mappings.messages]
source = "slack_messages"
row_key = ["message_id"]
links = [{ link = "posted_in", from = "m", to = "c" }]
[mappings.messages.bind.m]
type = "Message"
key = { message_id = "message_id" }
props = { text = "text" }
missing = "create"
[mappings.messages.bind.c]
type = "Channel"
key = { channel_id = "channel_id" }
missing = "report"
[mappings.replies]
parent = "messages"
records = "/replies"
row_key = ["/ts"]
[mappings.replies.bind.r]
type = "Reply"
key = { reply_id = "/ts" }
missing = "create"
[mappings.members]
source = "slack_members"
row_key = ["channel_id", "user_id"]
links = [{ link = "member_of", from = "p", to = "c" }]
[mappings.members.bind.p]
type = "Person"
key = { user_id = "user_id" }
missing = "report"
[mappings.members.bind.c]
type = "Channel"
key = { channel_id = "channel_id" }
missing = "report"
[mappings.days]
source = "slack_days"
[mappings.days.bind.d]
type = "Day"
key = { day_id = "channel_day_id" }
missing = "create"
`;
  const hers: Visibility = {
    sources: ["slack_users"],
    rows: [
      {
        source: "slack_channels",
        field: "channel_id",
        values: [" C1 ", "a|b"],
      },
      { source: "slack_messages", field: "message_id", prefixes: ["C1:"] },
      { source: "slack_members", field: "channel_id", values: ["C1"] },
      // Its mapping names no row_key: the table's primary key, which only the
      // catalog knows, so none of its rows.
      { source: "slack_days", field: "channel_day_id", prefixes: ["C1:"] },
    ],
    documents: "all",
    sets: [],
    enrichment: false,
    withheld: [],
  };
  const view = viewOf(hers, ontology, mappings);

  it("holds the rows its items' key components name, and what they write is partial", () => {
    expect(view.whole).toBe(false);
    expect(view.types.get("Person")).toBe("full");
    expect(view.types.get("Channel")).toBe("partial");
    expect(view.types.get("Message")).toBe("partial");
    expect(view.types.get("Reply")).toBe("partial");
    expect(view.types.get("Day")).toBeUndefined();
    expect(view.links.get("posted_in")).toBe("partial");
    expect(view.links.get("member_of")).toBe("partial");
    expect(nameUnsafe(view, "Message")).toBe(true);
    expect(view.holdsWhole("slack_messages")).toBe(false);
    for (const [source, mapping, rowKey, held] of [
      ["slack_messages", "messages", "C1:1700000000.000100", true],
      ["slack_messages", "messages", "C2:1700000000.000100", false],
      // The prefix carries its separator: C10 is another channel.
      ["slack_messages", "messages", "C10:1700000000.000100", false],
      // A nested mapping's key is its root's part, a /, then its own.
      [
        "slack_messages",
        "replies",
        "C1:1700000000.000100/1700000001.000200",
        true,
      ],
      [
        "slack_messages",
        "replies",
        "C2:1700000000.000100/1700000001.000200",
        false,
      ],
      ["slack_messages", "replies", "C1:1700000000.000100", false],
      ["slack_channels", "channels", "C1", true],
      ["slack_channels", "channels", "C2", false],
      // Escaped as the key stores it: the item a|b, not a and b.
      ["slack_channels", "channels", "a\\|b", true],
      ["slack_channels", "channels", "a|b", false],
      ["slack_channels", "channels", "C1\\", false],
      ["slack_members", "members", "C1|U1", true],
      ["slack_members", "members", "C2|U1", false],
      ["slack_members", "members", "C1", false],
      ["slack_days", "days", "C1:2026-11-01", false],
      ["slack_users", "users", "U1", true],
      ["slack_channels", "messages", "C1", false],
      ["crm", "crm", "C1", false],
    ] as const)
      expect(
        view.holdsRow(source, mapping, rowKey),
        `${mapping} ${rowKey}`,
      ).toBe(held);
  });

  it("reads the keys loads write, escaped as the engine joins them", () => {
    expect(joinKey(["C1"])).toBe("C1");
    expect(joinKey(["C1", "U1"])).toBe("C1|U1");
    expect(joinKey(["a|b", "c/d", "e\\f"])).toBe("a\\|b|c\\/d|e\\\\f");
    expect(view.holdsRow("slack_channels", "channels", joinKey(["a|b"]))).toBe(
      true,
    );
    expect(
      view.holdsRow("slack_members", "members", joinKey(["C1", "U|1"])),
    ).toBe(true);
    expect(
      view.holdsRow("slack_members", "members", joinKey(["C1|U1", "x"])),
    ).toBe(false);
  });

  it("holds nothing of a source with no items", () => {
    const none = viewOf(
      {
        ...hers,
        rows: [{ source: "slack_messages", field: "message_id", prefixes: [] }],
      },
      ontology,
      mappings,
    );
    expect(none.types.get("Message")).toBeUndefined();
    expect(none.holdsRow("slack_messages", "messages", "C1:1")).toBe(false);
    const blank = viewOf(
      {
        ...hers,
        rows: [
          { source: "slack_channels", field: "channel_id", values: [" "] },
        ],
      },
      ontology,
      mappings,
    );
    expect(blank.types.get("Channel")).toBeUndefined();
  });

  it("refuses what the engine refuses", () => {
    const rows = (r: Visibility["rows"]) => () =>
      viewOf({ ...hers, rows: r }, ontology, mappings);
    expect(
      rows([{ source: "slack_users", field: "user_id", values: ["U1"] }]),
    ).toThrow(/in sources too/);
    expect(
      rows([
        { source: "slack_channels", field: "channel_id", values: ["C1"] },
        { source: "slack_channels", field: "channel_id", values: ["C2"] },
      ]),
    ).toThrow(/named twice/);
    expect(
      rows([{ source: "foundry_enrichment", field: "x", values: ["C1"] }]),
    ).toThrow(/enrichment/);
    // What read_request.json refuses.
    expect(
      rows([{ source: "slack_messages", field: "message_id", prefixes: [""] }]),
    ).toThrow(/visibility: .*fewer than 1 characters/);
    expect(
      rows([
        {
          source: "slack_messages",
          field: "message_id",
          values: ["C1"],
          prefixes: ["C1:"],
        } as never,
      ]),
    ).toThrow(/visibility: /);
    expect(
      rows([{ source: "slack_messages", values: ["C1"] } as never]),
    ).toThrow(/visibility: .*field/);
  });

  it("settles a value from the rows it holds of a source held by its rows", () => {
    const row = (rowKey: string, value?: string) => ({
      source: "slack_channels",
      mapping: "channels",
      rowKey,
      ...(value === undefined ? {} : { value }),
    });
    expect(settled(view, "general", [row("C1", "general")], [])).toEqual({
      from: "stored",
    });
    // Another row, of a channel she doesn't hold, wrote the stored value.
    expect(
      settled(view, "exec", [row("C1", "general"), row("C2", "exec")], []),
    ).toEqual({
      from: "given",
      value: "general",
    });
    expect(settled(view, "x", [row("C1", "a"), row("a\\|b", "b")], [])).toEqual(
      {
        from: "none",
      },
    );
    // A row that kept no value gives none: it stands for the stored one,
    // which another row may have written.
    expect(settled(view, "exec", [row("C1")], [])).toEqual({ from: "none" });
    expect(settled(view, "exec", [row("C2", "exec")], [])).toEqual({
      from: "none",
    });
  });
});
