import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { AutomationNode, AutomationSpec } from "@/shared/wfe";
import { canonicalJson, moveNode } from "@/web/areas/automations/model";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { config, db, resetState } from "../../mock/state";

// The mock's automations behave as the backend's (backend/api/automations.go,
// store/foundry_automations.go, workers/foundry-automation-worker): a new
// one, and one whose steps changed, waits for its owner's approval; only
// its owner approves, runs and resumes it; a run walks its steps a step at a
// time; a schedule fires once when its time came; an event trigger fires
// after the work it listens to ends; three failures in a row pause it; and
// what its steps found is its owner's and managers' to read.

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

const A = "/v1/workspaces/ws_acme/foundry/automations";
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const reason = (r: MockReply & { body: any }) => r.body.details?.[0]?.reason;
let token = 0;

const at = { x: 0, y: 0 };
const node = (id: string, kind: AutomationNode["kind"], c: unknown) =>
  ({ id, kind, config: c, position: at }) as AutomationNode;
const chain = (...nodes: AutomationNode[]): AutomationSpec => ({
  v: 1,
  nodes,
  edges: nodes.slice(1).map((n, i) => ({ from: nodes[i]!.id, to: n.id })),
});
const manual = node("start", "trigger", { kind: "manual" });
const tell = node("tell", "notify", { message: "Done" });

async function create(spec: AutomationSpec, userId = "usr_ada") {
  const r = await call(
    "POST",
    A,
    {
      name: "Test",
      specJson: JSON.stringify(spec),
      clientToken: `c${++token}`,
    },
    userId,
  );
  expect(r.status, JSON.stringify(r.body)).toBe(200);
  return r.body.automation;
}
async function approve(
  a: { id: string; revision: string },
  userId = "usr_ada",
) {
  const r = await call(
    "POST",
    `${A}/${a.id}:approve`,
    { expectedRevision: a.revision },
    userId,
  );
  return r;
}
async function active(spec: AutomationSpec) {
  const r = await approve(await create(spec));
  expect(r.body.automation.status).toBe("active");
  return r.body.automation;
}
const runNow = (id: string, userId = "usr_ada") =>
  call("POST", `${A}/${id}:run`, { clientToken: `r${++token}` }, userId);
const getRun = async (id: string, runId: string, userId = "usr_ada") =>
  (await call("GET", `${A}/${id}/runs/${runId}`, undefined, userId)).body.run;
/** The run once its steps had time to go. */
async function ran(id: string, runId: string, ms = 20_000) {
  for (let i = 0; i < ms / 500; i++) {
    later(500);
    const r = await getRun(id, runId);
    if (r.status !== "queued" && r.status !== "running") return r;
  }
  return getRun(id, runId);
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T08:00:00Z"));
  resetState();
  token = 0;
});
afterEach(() => vi.useRealTimers());

describe("approval", () => {
  it("runs nothing until its owner approves the revision they read", async () => {
    const a = await create(chain(manual, tell));
    expect(a).toMatchObject({
      status: "needs_approval",
      revision: "1",
      canApprove: true,
      canRun: false,
      canEdit: true,
    });
    expect(reason(await runNow(a.id))).toBe("FOUNDRY_AUTOMATION_UNAPPROVED");
    expect((await approve(a, "usr_alan")).body.code).toBe(7);
    expect(reason(await approve({ ...a, revision: "9" }))).toBe(
      "FOUNDRY_AUTOMATION_CHANGED",
    );
    const on = (await approve(a)).body.automation;
    expect(on).toMatchObject({
      status: "active",
      approvedSpecRevision: "1",
      canRun: true,
    });
    // Approving it again changes nothing.
    expect((await approve(on)).body.automation.revision).toBe(on.revision);
    // Another member sees it, and may do nothing to it.
    const seen = (await call("GET", `${A}/${a.id}`, undefined, "usr_alan")).body
      .automation;
    expect(seen).toMatchObject({
      canEdit: false,
      canRun: false,
      canPause: false,
      canDelete: false,
    });
  });

  it("keeps its approval when only its steps' places move, and waits again otherwise", async () => {
    const a = await active(chain(manual, tell));
    const read = (await call("GET", `${A}/${a.id}`)).body.automation;
    const spec = JSON.parse(read.specJson) as AutomationSpec;
    const moved = await call("PATCH", `${A}/${a.id}`, {
      expectedRevision: read.revision,
      specJson: canonicalJson(moveNode(spec, "tell", { x: 200, y: 300 })),
    });
    expect(moved.body.automation).toMatchObject({
      status: "active",
      specRevision: "2",
    });
    const changed = await call("PATCH", `${A}/${a.id}`, {
      expectedRevision: moved.body.automation.revision,
      specJson: canonicalJson(
        chain(manual, node("tell", "notify", { message: "Other" })),
      ),
    });
    expect(changed.body.automation).toMatchObject({
      status: "needs_approval",
      nextRunAt: null,
    });
    // Renamed by someone else, or at a revision passed: refused.
    expect(
      (
        await call(
          "PATCH",
          `${A}/${a.id}`,
          { expectedRevision: changed.body.automation.revision, name: "x" },
          "usr_alan",
        )
      ).body.code,
    ).toBe(7);
    expect(
      reason(
        await call("PATCH", `${A}/${a.id}`, {
          expectedRevision: "1",
          name: "x",
        }),
      ),
    ).toBe("FOUNDRY_AUTOMATION_CHANGED");
    expect(
      (await call("PATCH", `${A}/${a.id}`, { expectedRevision: "3" })).status,
    ).toBe(400);
  });

  it("refuses a spec that doesn't read, or names what the workspace hasn't", async () => {
    const bad = await call("POST", A, {
      name: "x",
      specJson: "{}",
      clientToken: "b",
    });
    expect(bad.body.message).toMatch(/^spec_json: /);
    const other = await call("POST", A, {
      name: "x",
      clientToken: "o",
      specJson: JSON.stringify(
        chain(
          manual,
          node("sync", "action", {
            operation: "sync_connector",
            connector_id: "conn_other",
          }),
        ),
      ),
    });
    expect(other.body.message).toBe(
      "spec_json names the connector conn_other, which this workspace hasn't",
    );
    expect(
      (await call("POST", A, { name: " ", specJson: "{}", clientToken: "n" }))
        .body.message,
    ).toBe("name is 1 to 120 characters");
    // The same client token returns the one made.
    const a = await create(chain(manual, tell));
    const again = await call("POST", A, {
      name: "Test",
      specJson: "{}",
      clientToken: `c${token}`,
    });
    expect(again.body.automation.id).toBe(a.id);
  });

  it("keeps 50 automations a workspace", async () => {
    for (let i = 0; i < 50; i++) await create(chain(manual, tell));
    const r = await call("POST", A, {
      name: "One more",
      specJson: JSON.stringify(chain(manual, tell)),
      clientToken: "last",
    });
    expect(reason(r)).toBe("FOUNDRY_AUTOMATION_LIMIT");
  });
});

describe("runs", () => {
  it("walks a run a step at a time, its findings its owner's to read", async () => {
    const a = await active(
      chain(
        manual,
        node("ask", "assistant", {
          instruction: "How many ontologies? Answer yes or no.",
        }),
        tell,
      ),
    );
    const queued = await runNow(a.id);
    expect(queued.body.run).toMatchObject({
      status: "queued",
      canCancel: true,
    });
    expect(reason(await runNow(a.id))).toBe("FOUNDRY_AUTOMATION_BUSY");
    expect((await runNow(a.id, "usr_alan")).body.code).toBe(7);
    const done = await ran(a.id, queued.body.run.id);
    expect(done.status).toBe("succeeded");
    const steps = Object.fromEntries(done.nodes.map((n: any) => [n.nodeId, n]));
    expect(JSON.parse(steps.start.outputJson)).toEqual({
      summary: "Run by hand.",
    });
    expect(JSON.parse(steps.ask.outputJson)).toMatchObject({
      answer: true,
      count: 3,
    });
    expect(steps.ask.turnId).toBeTruthy();
    expect(JSON.parse(steps.tell.outputJson)).toEqual({
      summary: "Emailed its owner.",
    });
    expect(done.threadId).toBeTruthy();
    // Another member sees how far it went, not what it found.
    const theirs = await getRun(a.id, done.id, "usr_alan");
    expect(theirs.nodes.every((n: any) => !n.outputJson && !n.turnId)).toBe(
      true,
    );
    expect(theirs.threadId).toBeUndefined();
    // The run's thread is out of its owner's list of threads.
    const threads = await call(
      "GET",
      "/v1/workspaces/ws_acme/foundry/agent/threads",
    );
    expect(threads.body.threads).toEqual([]);
    const list = (await call("GET", A)).body;
    expect(list).toMatchObject({ canCreate: true, email: true });
    expect(list.automations[0].lastRun).toMatchObject({
      id: done.id,
      status: "succeeded",
    });
  });

  it("takes a condition's branch, and skips the other", async () => {
    Object.assign(config, { automationAnswer: "no" });
    const spec: AutomationSpec = {
      v: 1,
      nodes: [
        manual,
        node("ask", "assistant", {
          instruction: "Is anything overdue? Answer yes or no.",
        }),
        node("if", "condition", { check: "answer", equals: "yes" }),
        tell,
        node("other", "notify", { message: "Nothing" }),
      ],
      edges: [
        { from: "start", to: "ask" },
        { from: "ask", to: "if" },
        { from: "if", to: "tell", branch: "true" },
        { from: "if", to: "other", branch: "false" },
      ],
    };
    const a = await active(spec);
    const done = await ran(a.id, (await runNow(a.id)).body.run.id);
    const status = Object.fromEntries(
      done.nodes.map((n: any) => [n.nodeId, n.status]),
    );
    expect(status).toEqual({
      start: "succeeded",
      ask: "succeeded",
      if: "succeeded",
      tell: "skipped",
      other: "succeeded",
    });
  });

  it("runs an Assistant step's approved load at once, and leaves another as a card", async () => {
    const load = {
      operation: "create_load",
      ontology_id: "ont_acme_contracts",
    };
    const allowed = await active(
      chain(
        manual,
        node("ask", "assistant", {
          instruction: "Load Customer contracts.",
          allow: [load],
        }),
      ),
    );
    const done = await ran(allowed.id, (await runNow(allowed.id)).body.run.id);
    expect(done.status).toBe("succeeded");
    const thread = await call(
      "GET",
      `/v1/workspaces/ws_acme/foundry/agent/threads/${done.threadId}`,
    );
    expect(thread.body.actions.map((x: any) => x.status)).toEqual([
      "succeeded",
    ]);
    const asked = await active(
      chain(
        manual,
        node("ask", "assistant", { instruction: "Load Customer contracts." }),
      ),
    );
    const waits = await ran(asked.id, (await runNow(asked.id)).body.run.id);
    expect(JSON.parse(waits.nodes[1].outputJson)).toMatchObject({ pending: 1 });
    const cards = await call(
      "GET",
      `/v1/workspaces/ws_acme/foundry/agent/threads/${waits.threadId}`,
    );
    expect(cards.body.actions.map((x: any) => x.status)).toEqual(["prepared"]);
  });

  it("stops at a failed step, holds the next run back, and pauses at the third", async () => {
    Object.assign(config, { agentOutcome: "fail" });
    const a = await active(
      chain(
        manual,
        node("ask", "assistant", { instruction: "Summarise." }),
        tell,
      ),
    );
    for (let i = 1; i <= 3; i++) {
      const done = await ran(a.id, (await runNow(a.id)).body.run.id);
      expect(done.status).toBe("failed");
      expect(done.error).toBe(
        'The assistant step "ask" failed: The model failed: overloaded.',
      );
      expect(done.nodes.find((n: any) => n.nodeId === "tell").status).toBe(
        "blocked",
      );
      const after = (await call("GET", `${A}/${a.id}`)).body.automation;
      expect(after.failures).toBe(i);
      if (i < 3)
        expect(after).toMatchObject({
          status: "active",
          backoffUntil: expect.any(String),
        });
      else
        expect(after).toMatchObject({
          status: "paused",
          pausedReason: "failures",
          canResume: true,
        });
    }
    // Another member doesn't read why.
    const runs = (await call("GET", `${A}/${a.id}/runs`, undefined, "usr_alan"))
      .body.runs;
    expect(runs).toHaveLength(3);
    expect(runs[0].error).toBeUndefined();
    // Turned on again, it starts afresh.
    const on = (await call("POST", `${A}/${a.id}:resume`, {})).body.automation;
    expect(on).toMatchObject({ status: "active", failures: 0 });
  });

  it("cancels a live run, its owner's or a manager's call", async () => {
    const a = await active(
      chain(manual, node("ask", "assistant", { instruction: "Summarise." })),
    );
    const run = (await runNow(a.id)).body.run;
    expect(
      (await call("POST", `${A}/${a.id}/runs/${run.id}:cancel`, {}, "usr_alan"))
        .body.code,
    ).toBe(7);
    const cancelled = await call(
      "POST",
      `${A}/${a.id}/runs/${run.id}:cancel`,
      {},
    );
    expect(cancelled.body.run).toMatchObject({
      status: "cancelled",
      error: "Cancelled by a person.",
    });
    expect(
      (await call("POST", `${A}/${a.id}/runs/${run.id}:cancel`, {})).status,
    ).toBe(200);
    const other = (await runNow(a.id)).body.run;
    await ran(a.id, other.id);
    expect(
      reason(await call("POST", `${A}/${a.id}/runs/${other.id}:cancel`, {})),
    ).toBe("FOUNDRY_AUTOMATION_RUN_ENDED");
  });

  it("refuses past the workspace's runs of the day", async () => {
    Object.assign(config, { automationRunsPerDay: 1 });
    const a = await active(chain(manual, tell));
    await ran(a.id, (await runNow(a.id)).body.run.id);
    expect((await runNow(a.id)).status).toBe(429);
  });
});

describe("triggers", () => {
  it("fires a schedule once when its time came, and once again at the next", async () => {
    const a = await active(
      chain(
        node("start", "trigger", {
          kind: "schedule",
          every: "day",
          at: "09:00",
          time_zone: "UTC",
        }),
        tell,
      ),
    );
    expect(a.nextRunAt).toBe("2026-11-16T09:00:00.000Z");
    expect((await call("GET", `${A}/${a.id}/runs`)).body.runs).toEqual([]);
    // Two days on, it fires once, not once a missed day, and next at the
    // first time after now.
    Object.assign(config, { automationClockMs: 2 * 24 * 3_600_000 });
    const read = (await call("GET", `${A}/${a.id}`)).body.automation;
    expect(read.nextRunAt).toBe("2026-11-18T09:00:00.000Z");
    const runs = (await call("GET", `${A}/${a.id}/runs`)).body.runs;
    expect(runs).toHaveLength(1);
    expect(JSON.parse(runs[0].triggerEventJson)).toMatchObject({
      kind: "schedule",
      at: "2026-11-16T09:00:00.000Z",
    });
    expect((await call("GET", `${A}/${a.id}/runs`)).body.runs).toHaveLength(1);
  });

  it("records a skipped run when its schedule fires while one is live", async () => {
    const a = await active(
      chain(
        node("start", "trigger", {
          kind: "schedule",
          every: "hour",
          minute: 30,
          time_zone: "UTC",
        }),
        node("ask", "assistant", { instruction: "Summarise." }),
      ),
    );
    await runNow(a.id);
    later(1_000);
    Object.assign(config, { automationClockMs: 3_600_000 });
    const runs = (await call("GET", `${A}/${a.id}/runs`)).body.runs;
    expect(runs.map((r: any) => r.status)).toEqual(["skipped", "running"]);
    expect(runs[0].error).toBe("Skipped: the run before it was still going.");
  });

  it("fires after a sync of its connector ends as it listens for", async () => {
    const a = await active(
      chain(
        node("start", "trigger", {
          kind: "sync_finished",
          connector_id: "conn_acme_stripe",
          outcome: "succeeded",
        }),
        node("if", "condition", { check: "count", op: "at_least", value: 1 }),
      ),
    );
    // A sync that ended before it was on fires nothing.
    expect((await call("GET", `${A}/${a.id}/runs`)).body.runs).toEqual([]);
    const sync = await call(
      "POST",
      "/v1/workspaces/ws_acme/foundry/connectors/conn_acme_stripe:sync",
      {},
    );
    expect(sync.status).toBe(200);
    later(5_000);
    const runs = (await call("GET", `${A}/${a.id}/runs`)).body.runs;
    expect(runs).toHaveLength(1);
    const event = JSON.parse(runs[0].triggerEventJson);
    expect(event).toMatchObject({
      kind: "sync_finished",
      outcome: "succeeded",
      sync_id: sync.body.sync.id,
    });
    const done = await ran(a.id, runs[0].id);
    expect(done.status).toBe("succeeded");
    expect(
      JSON.parse(done.nodes.find((n: any) => n.nodeId === "if").outputJson),
    ).toEqual({ branch: true });
    // Read again, the same sync fires nothing more.
    expect((await call("GET", `${A}/${a.id}/runs`)).body.runs).toHaveLength(1);
  });

  it("lets an owner or admin pause and delete another's", async () => {
    const mine = await create(chain(manual, tell), "usr_alan");
    expect((await approve(mine, "usr_alan")).body.automation.status).toBe(
      "active",
    );
    expect(
      (await call("POST", `${A}/${mine.id}:pause`, {})).body.automation,
    ).toMatchObject({
      status: "paused",
      pausedReason: "person",
      pausedByUserId: "usr_ada",
    });
    expect(
      reason(
        await call(
          "POST",
          `${A}/${(await create(chain(manual, tell))).id}:pause`,
          {},
        ),
      ),
    ).toBe("FOUNDRY_AUTOMATION_NOT_ACTIVE");
    expect((await call("DELETE", `${A}/${mine.id}`)).status).toBe(200);
    expect((await call("GET", `${A}/${mine.id}`)).status).toBe(404);
  });

  it("is off where automations are", async () => {
    Object.assign(config, { automationsDisabled: true });
    expect(reason(await call("GET", A))).toBe("FOUNDRY_AUTOMATIONS_DISABLED");
  });
});
