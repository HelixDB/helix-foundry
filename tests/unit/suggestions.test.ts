import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { FoundryHealFinding } from "@/shared/wfe";
import {
  auditPending,
  emptyText,
  lastRunText,
  suggestion,
} from "@/web/areas/suggestions/model";
import type { MockReply } from "../../mock/http";
import { contractErrors } from "../../mock/agent-contract";
import { routes } from "../../mock/routes";
import { SUGGESTION_FINDINGS } from "../../mock/routes/suggestions";
import { config, db, resetState } from "../../mock/state";

// The Suggestions page reads the engine's Finding JSON without trust, and
// the mock serves the backend's list and dismissal with findings the
// vendored audit_result.json holds to the engine's shapes.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    return (await r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}

const O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
const H = `${O}/heal-findings`;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

const stored = (
  finding: object,
  rest: Partial<FoundryHealFinding> = {},
): FoundryHealFinding => ({
  fingerprint: "f".repeat(64),
  kind: "unmatched",
  status: "open",
  findingJson: JSON.stringify(finding),
  ...rest,
});

describe("a suggestion's words", () => {
  it("reads every kind the contract has", () => {
    const [unmatched, unresolved, rows] = SUGGESTION_FINDINGS.map((f) =>
      suggestion(
        stored(f.finding, {
          support: String(f.support),
          sources: String(f.sources),
        }),
      ),
    );
    expect(unmatched).toMatchObject({
      kind: "unmatched",
      kindLabel: "Relationship no link covers",
      title: "Documents state “supplier of”, which no link covers",
      detail: "In document set set_acme_general",
      support: 12,
      documents: 5,
      proposal: "Let documents of set_acme_general state “supplies”",
      spellings: [],
    });
    expect(unmatched!.examples[0]).toEqual({
      docId: "doc_acme_msa",
      quote: "Globex supplies Acme with steel under this agreement.",
    });
    expect(unresolved).toMatchObject({
      title: "Documents name a Customer “globex corp” that resolved to no one",
      detail: "No entity has the name",
      proposal: undefined,
    });
    expect(rows).toMatchObject({
      title: "Rows of “customers” didn't load",
      support: 3,
      documents: 0,
      examples: [{ docId: "", quote: "row 14: customer_id is empty" }],
      proposal: "Map “customers” again",
    });
    const clash = (c: object) =>
      suggestion(stored({ signal: { kind: "clash", clash: c } }));
    expect(
      clash({
        clash: "ambiguous",
        entities: [
          { type: "Customer", norm_name: "acme" },
          { type: "Customer", key: "cus_1" },
        ],
      }),
    ).toMatchObject({
      title: "Entities share a name documents use",
      detail: "Customer “acme” and Customer “cus_1”",
    });
    expect(clash({ clash: "identifier_conflict", entities: [] }).title).toBe(
      "A document's identifiers point at different entities",
    );
    expect(
      clash({ clash: "collision", entity: { type: "Customer", key: "cus_1" } })
        .title,
    ).toBe("Two names a document uses resolve to Customer “cus_1”");
    expect(clash({ clash: "later" }).title).toBe("Entities clash");
    expect(
      suggestion(
        stored({
          signal: {
            kind: "source_drift",
            mapping: "people",
            drift: { added: ["email"], removed: ["fax", "telex"] },
          },
        }),
      ),
    ).toMatchObject({
      title: "The source “people” reads changed",
      detail: "Gained email; lost fax and telex",
      support: 0,
    });
    const now = (n: object) =>
      suggestion(
        stored({
          signal: {
            kind: "unresolved",
            type: "Customer",
            norm_name: "x",
            now: n,
          },
        }),
      ).detail;
    expect(
      now({ resolves: "one", entity: { type: "Customer", key: "cus_1" } }),
    ).toBe("It resolves to Customer “cus_1” now");
    expect(now({ resolves: "many", entities: [{}, {}] })).toBe(
      "It resolves to 2 entities now",
    );
  });

  it("words every proposal, and a draft's brief", () => {
    const proposal = (p: object) =>
      suggestion(
        stored({ signal: { kind: "unmatched", canonical: "x" }, proposal: p }),
      ).proposal;
    expect(
      proposal({
        proposal: "draft",
        brief: { brief: "description", link: "supplies" },
      }),
    ).toBe("Describe “supplies” so documents use it");
    expect(
      proposal({
        proposal: "draft",
        brief: { brief: "link", names: ["supplier_of", "vendor_of"] },
      }),
    ).toBe("Add a link for “supplier of” and “vendor of”");
    expect(proposal({ proposal: "draft", brief: { brief: "later" } })).toBe(
      "Draft a change",
    );
    expect(
      proposal({
        proposal: "ask",
        question: { question: "cardinality", link: "x" },
      }),
    ).toBe("A person decides what to change");
    expect(proposal({ proposal: "later" })).toBeUndefined();
  });

  it("names a document set by its name, and by its id when it's unknown", () => {
    const f = stored({
      signal: { kind: "unmatched", canonical: "supplier_of", set: "set_1" },
      proposal: { proposal: "route", set: "set_1", links: ["supplies"] },
    });
    const named = suggestion(f, [
      { id: "set_0", name: "Invoices" },
      { id: "set_1", name: "Contracts" },
    ]);
    expect(named.detail).toBe("In document set “Contracts”");
    expect(named.proposal).toBe(
      "Let documents of “Contracts” state “supplies”",
    );
    // A set the list doesn't hold (deleted, or not loaded yet), or one with
    // no name, is its id.
    for (const sets of [
      [],
      [{ id: "set_1" }],
      [{ id: "set_2", name: "Other" }],
    ]) {
      expect(suggestion(f, sets).detail).toBe("In document set set_1");
    }
  });

  it("reads a later contract's fields and kinds as what they are", () => {
    // 4.8.0: a gathered finding's spellings, and a route's gathered flag.
    const gathered = suggestion(
      stored({
        signal: {
          kind: "unmatched",
          canonical: "date_of_death",
          set: "s",
          spellings: ["date_of_death", "date_of_death_no_link"],
          backing: { count: 3, sources: 2, examples: [] },
        },
        proposal: {
          proposal: "route",
          set: "s",
          links: ["died_on"],
          gathered: true,
          target: { count: "unmatched", canonicals: ["date_of_death"] },
        },
      }),
    );
    expect(gathered.spellings).toEqual([
      "date_of_death",
      "date_of_death_no_link",
    ]);
    expect(gathered.support).toBe(3);
    expect(
      suggestion(
        stored({ signal: { kind: "held_facts" } }, { kind: "held_facts" }),
      ),
    ).toMatchObject({
      kindLabel: "held facts",
      title: "held facts",
    });
  });

  it("never throws on JSON that isn't a finding", () => {
    for (const findingJson of [
      "",
      "{",
      "3",
      "null",
      '{"signal":3}',
      '{"signal":{"kind":"unmatched","backing":{"examples":[3,{"quote":2}]}}}',
    ])
      expect(() =>
        suggestion({
          fingerprint: "f",
          kind: "unmatched",
          status: "open",
          findingJson,
        }),
      ).not.toThrow();
    expect(
      suggestion({
        fingerprint: "f",
        kind: "",
        status: "open",
        findingJson: "{",
      }),
    ).toMatchObject({
      kindLabel: "Finding",
      support: 0,
      examples: [],
    });
    expect(
      suggestion(
        stored(
          { signal: { kind: "unmatched" } },
          { support: "9007199254740993" },
        ),
      ).support,
    ).toBe(0);
  });

  it("says how the last audit went", () => {
    const at = Date.parse("2026-10-19T10:00:00Z"),
      now = at + 2 * 3600_000;
    expect(lastRunText(undefined)).toBeUndefined();
    expect(
      lastRunText(
        {
          id: "r",
          loadId: "l1",
          status: "complete",
          pages: 3,
          rowsMeasured: true,
          finishedAt: "2026-10-19T10:00:00Z",
        },
        { current: "l1", now },
      ),
    ).toBe("Last audited 2 h ago, 3 pages of documents.");
    expect(
      lastRunText(
        {
          id: "r",
          status: "complete",
          pages: 1,
          finishedAt: "2026-10-19T10:00:00Z",
        },
        { now: at },
      ),
    ).toBe(
      "Last audited just now, 1 page of documents. Rows weren’t checked on this load.",
    );
    expect(
      lastRunText(
        { id: "r", status: "running", startedAt: "2026-10-19T10:00:00Z" },
        { now: at + 5 * 60_000 },
      ),
    ).toBe("Auditing the current load now (started 5 min ago).");
    const incomplete = {
      id: "r",
      status: "incomplete",
      finishedAt: "2026-10-16T10:00:00Z",
    };
    // Only suggestions shown are from the audit before it.
    expect(lastRunText(incomplete, { now, shown: true })).toBe(
      "The last audit 3 days ago didn’t finish, so these are from the audit before it; it will audit again soon.",
    );
    expect(lastRunText(incomplete, { now })).toBe(
      "The last audit 3 days ago didn’t finish; it will audit again soon.",
    );
    expect(lastRunText({ id: "r", status: "failed" }, { now })).toBe(
      "The last audit failed; it will audit again soon.",
    );
    expect(
      lastRunText({ id: "r", status: "failed" }, { now, shown: true }),
    ).toBe(
      "The last audit failed, so these are from the audit before it; it will audit again soon.",
    );
    expect(lastRunText({ id: "r", status: "later" }, { now })).toBeUndefined();
    // An audit of another load, whatever its status, says so.
    for (const status of ["complete", "running", "failed"])
      expect(
        lastRunText({ id: "r", loadId: "l1", status }, { current: "l2", now }),
      ).toBe("The last audit was of another load.");
  });

  it("says what the newest complete audit of the current load left open", () => {
    expect(emptyText(undefined, undefined)).toMatch(/^Load the ontology/);
    expect(emptyText("l1", { id: "r", loadId: "l1", status: "complete" })).toBe(
      "No open suggestions from the last audit of the current load.",
    );
    for (const complete of [
      undefined,
      { id: "r", loadId: "l0", status: "complete" },
    ])
      expect(emptyText("l1", complete)).toBe(
        "The current load hasn’t been fully audited yet.",
      );
  });

  it("asks again until an audit of the current load completes", () => {
    const complete = { id: "r1", loadId: "l1", status: "complete" };
    expect(auditPending(undefined, "l1")).toBe(false);
    expect(auditPending({ enabled: false }, "l1")).toBe(false);
    expect(auditPending({ enabled: true }, undefined)).toBe(false);
    expect(auditPending({ enabled: true }, "l1")).toBe(true);
    expect(
      auditPending({ enabled: true, lastCompleteRun: complete }, "l2"),
    ).toBe(true);
    expect(
      auditPending(
        { enabled: true, lastRun: complete, lastCompleteRun: complete },
        "l1",
      ),
    ).toBe(false);
    // A nightly audit running: its findings will change.
    expect(
      auditPending(
        {
          enabled: true,
          lastRun: { id: "r2", loadId: "l1", status: "running" },
          lastCompleteRun: complete,
        },
        "l1",
      ),
    ).toBe(true);
  });

  it("keeps the load a finding was counted over", () => {
    expect(
      suggestion(stored({ signal: { kind: "unresolved" } }, { loadId: "l1" }))
        .loadId,
    ).toBe("l1");
    expect(suggestion(stored({ signal: { kind: "unresolved" } })).loadId).toBe(
      "",
    );
  });
});

describe("mock suggestions", () => {
  const load = async (clientToken = "l") => {
    Object.assign(config, { loadStepMs: 50 });
    const made = (
      await call("POST", `${O}/loads`, { versionId: "ver_seed_3", clientToken })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", `${O}/loads/${made.id}`)).body.load.status).toBe(
      "ready",
    );
    return made.id as string;
  };

  it("fit the engine's Finding", () => {
    for (const { finding } of SUGGESTION_FINDINGS)
      expect(contractErrors("audit_result#/$defs/Finding", finding)).toEqual(
        [],
      );
  });

  it("list nothing before a load, then the load's findings by support", async () => {
    const before = await call("GET", H);
    expect(before.status).toBe(200);
    expect(before.body.findings).toBeUndefined();
    const loadId = await load();
    const listed = await call("GET", H);
    expect(listed.body.findings.map((f: FoundryHealFinding) => f.kind)).toEqual(
      ["unmatched", "unresolved", "row_errors"],
    );
    for (const f of listed.body.findings) {
      expect(f.loadId).toBe(loadId);
      expect(
        contractErrors(
          "audit_result#/$defs/Finding",
          JSON.parse(f.findingJson),
        ),
      ).toEqual([]);
    }
    expect(listed.body.lastRun).toMatchObject({
      status: "complete",
      loadId,
      rowsMeasured: true,
    });
    // A load whose report kept no rows: the audit didn't check them.
    Object.assign(config, { healRowsMeasured: false });
    expect((await call("GET", H)).body.lastRun.rowsMeasured).toBe(false);
  });

  it("follow the heal knobs: off, an audit not complete, a dismissal superseded", async () => {
    const first = await load();
    expect((await call("GET", H)).body.enabled).toBe(true);
    Object.assign(config, { healEnabled: false });
    expect((await call("GET", H)).body).toEqual({ enabled: false });
    Object.assign(config, { healEnabled: true, healRun: "failed" });
    const second = await load("l2");
    const held = (await call("GET", H)).body;
    expect(held.lastRun).toMatchObject({ status: "failed", loadId: second });
    expect(held.lastCompleteRun).toMatchObject({
      status: "complete",
      loadId: first,
    });
    expect(
      held.findings.every((f: FoundryHealFinding) => f.loadId === first),
    ).toBe(true);
    Object.assign(config, { healDismiss: "superseded" });
    const fp = held.findings[0].fingerprint;
    expect((await call("POST", `${H}/${fp}:dismiss`, {})).status).toBe(400);
    expect(
      (await call("GET", H)).body.findings.map(
        (f: FoundryHealFinding) => f.fingerprint,
      ),
    ).not.toContain(fp);
    // A backend that fails the dismissal leaves the finding open.
    Object.assign(config, { healDismiss: "fails" });
    const other = held.findings[1].fingerprint;
    expect((await call("POST", `${H}/${other}:dismiss`, {})).status).toBe(500);
    expect(
      (await call("GET", H)).body.findings.map(
        (f: FoundryHealFinding) => f.fingerprint,
      ),
    ).toContain(other);
  });

  it("dismiss an open finding for good, idempotently, and refuse the rest", async () => {
    await load();
    const fp = "a".repeat(64);
    const dismissed = await call("POST", `${H}/${fp}:dismiss`, {});
    expect(dismissed.status).toBe(200);
    expect(dismissed.body.finding).toMatchObject({
      status: "dismissed",
      dismissedByUserId: "usr_ada",
      supportAtDismissal: "12",
    });
    expect(
      (await call("GET", H)).body.findings.map(
        (f: FoundryHealFinding) => f.fingerprint,
      ),
    ).not.toContain(fp);
    // Again, it is the same dismissal, and no error.
    const twice = await call("POST", `${H}/${fp}:dismiss`, {});
    expect(twice.status).toBe(200);
    expect(twice.body.finding).toMatchObject({
      status: "dismissed",
      supportAtDismissal: "12",
    });
    expect(
      (await call("POST", `${H}/${"9".repeat(64)}:dismiss`, {})).status,
    ).toBe(404);
    // A new load is audited again; the dismissal holds.
    const next = await load("l2");
    const again = await call("GET", H);
    expect(again.body.lastRun.loadId).toBe(next);
    expect(
      again.body.findings.map((f: FoundryHealFinding) => f.fingerprint),
    ).not.toContain(fp);
  });
});
