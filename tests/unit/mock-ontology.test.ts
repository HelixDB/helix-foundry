import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's ontology routes behave as the backend's do: the e2e
// suite and dev:mock rely on it. Versions and generations belong to an
// ontology: Acme's seeded "Customer contracts".

const ws = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  for (const r of routes) {
    if (r.method !== method) continue;
    const names: string[] = [];
    const pattern = r.path.replace(/\{(\w+)\}/g, (_, n) => {
      names.push(n);
      return "([^/:]+)";
    });
    const m = new RegExp(`^${pattern}$`).exec(path.split("?")[0]!);
    if (!m) continue;
    return (await r.handle({
      method,
      path,
      params: Object.fromEntries(names.map((n, i) => [n, m[i + 1]!])),
      query: new URLSearchParams(path.split("?")[1] || ""),
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no route for ${method} ${path}`);
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-18T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("mock ontology versions", () => {
  it("serves the seeded current version, in both formats", async () => {
    const ontology = await call("GET", ws);
    expect(ontology.body.ontology).toMatchObject({
      id: "ont_acme_contracts",
      name: "Customer contracts",
      tenantId: "ten_acme_contracts",
      databaseName: "contracts",
      currentVersionId: "ver_seed_1",
      lastLoad: null,
      counts: { documents: 0 },
    });
    const current = await call("GET", `${ws}/versions/ver_seed_1`);
    expect(current.body.version).toMatchObject({
      id: "ver_seed_1",
      ontologyId: "ont_acme_contracts",
      version: 1,
      isCurrent: true,
      format: "ONTOLOGY_FORMAT_FOUNDRY_V1",
    });
    expect(current.body.version.definition.entities).toHaveLength(4);
    const list = await call("GET", `${ws}/versions`);
    expect(
      list.body.versions.map((v: any) => [v.version, v.isCurrent]),
    ).toEqual([
      [3, false],
      [2, false],
      [1, true],
    ]);
    // Lists leave the definition out, as null (the gateway emits it).
    expect(list.body.versions[0].definition).toBeNull();
    const agent = await call("GET", `${ws}/versions/ver_seed_2`);
    expect(agent.body.version.definition.format).toBe("agent_bundle");
    // Version 3 drafts contracts with the Stripe customers: what a load reads.
    const contracts = await call("GET", `${ws}/versions/ver_seed_3`);
    expect(contracts.body.version.definition).toMatchObject({
      format: "agent_bundle",
      status: "done",
    });
    expect(contracts.body.version.definition.ontology_toml).toContain(
      'names = ["company_name"]',
    );
    expect((await call("GET", `${ws}/versions/nope`)).status).toBe(404);
    expect(
      (await call("GET", `${ws}/versions?limit=1`)).body.versions,
    ).toHaveLength(1);
    // An ontology of another workspace isn't found.
    const globex = {
      id: "ws_globex",
      slug: "globex",
      displayName: "Globex",
      region: "us-east-2",
    };
    db.workspaces.set(globex.id, globex);
    db.memberships.push({
      id: "mem_globex",
      workspaceId: globex.id,
      userId: "usr_ada",
      role: "WORKSPACE_ROLE_OWNER",
      createdAt: "2026-10-18T12:00:00Z",
    });
    expect(
      (
        await call(
          "GET",
          "/v1/workspaces/ws_globex/foundry/ontologies/ont_acme_contracts/versions",
        )
      ).status,
    ).toBe(404);
  });

  it("moves the current pointer only from the version the caller saw", async () => {
    const stale = await call("POST", `${ws}:set-current`, {
      versionId: "ver_seed_2",
    });
    expect(stale.status).toBe(400);
    expect(stale.body.message).toMatch(/changed since you loaded it/);
    const moved = await call("POST", `${ws}:set-current`, {
      versionId: "ver_seed_2",
      expectedCurrentVersionId: "ver_seed_1",
    });
    expect(moved.body.version).toMatchObject({
      id: "ver_seed_2",
      isCurrent: true,
    });
    // Choosing the current version again is a no-op; rolling back works.
    const again = await call("POST", `${ws}:set-current`, {
      versionId: "ver_seed_2",
      expectedCurrentVersionId: "ver_seed_2",
    });
    expect(again.status).toBe(200);
    const back = await call("POST", `${ws}:set-current`, {
      versionId: "ver_seed_1",
      expectedCurrentVersionId: "ver_seed_2",
    });
    expect(back.body.version.isCurrent).toBe(true);
    expect(
      (
        await call("POST", `${ws}:set-current`, {
          versionId: "missing",
          expectedCurrentVersionId: "ver_seed_1",
        })
      ).status,
    ).toBe(404);
    expect((await call("POST", `${ws}:set-current`, {})).status).toBe(400);
  });

  it("refuses members and strangers", async () => {
    db.memberships.find((m) => m.userId === "usr_ada")!.role =
      "WORKSPACE_ROLE_MEMBER";
    expect(
      (
        await call("POST", `${ws}:set-current`, {
          versionId: "ver_seed_2",
          expectedCurrentVersionId: "ver_seed_1",
        })
      ).status,
    ).toBe(403);
    expect((await call("POST", `${ws}:generate`, {})).status).toBe(403);
    expect((await call("GET", ws)).status).toBe(200);
    expect(
      (
        await call(
          "GET",
          "/v1/workspaces/ws_other/foundry/ontologies/ont_acme_contracts",
        )
      ).status,
    ).toBe(403);
  });
});

describe("mock ontology generations", () => {
  it("drafts a new version: queued, running, then succeeded", async () => {
    const started = await call("POST", `${ws}:generate`, {
      goal: "  Billing health  ",
      connectorIds: ["conn_acme_stripe"],
    });
    const g = started.body.generation;
    expect(g).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
      goal: "Billing health",
      title: "Billing health",
      baseVersionId: "ver_seed_1",
      attempts: 0,
    });
    // Every table Stripe has synced.
    expect(g.tables).toHaveLength(15);
    const get = async () =>
      (await call("GET", `${ws}/generations/${g.id}`)).body.generation;
    expect((await get()).status).toBe("ONTOLOGY_GENERATION_STATUS_QUEUED");
    vi.setSystemTime(Date.now() + 1500);
    expect(await get()).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
      attempts: 1,
    });
    // One live generation per workspace.
    const second = await call("POST", `${ws}:generate`, {});
    expect(second.status).toBe(409);
    vi.setSystemTime(Date.now() + 1500);
    const done = await get();
    expect(done).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      model: "claude-opus-5-5",
    });
    const version = (
      await call("GET", `${ws}/versions/${done.resultVersionId}`)
    ).body.version;
    expect(version).toMatchObject({
      version: 4,
      isCurrent: false,
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      source: "ONTOLOGY_SOURCE_GENERATED",
      generationId: g.id,
    });
    expect(version.definition.ontology_toml).toContain("[types.Invoice]");
    // The draft never moves the current pointer.
    expect((await call("GET", ws)).body.ontology.currentVersionId).toBe(
      "ver_seed_1",
    );
    const list = await call("GET", `${ws}/generations`);
    expect(list.body.generations[0].id).toBe(g.id);
  });

  it("titles a generation from its goal when it is created", async () => {
    const titled = async (goal?: string) => {
      const g = (await call("POST", `${ws}:generate`, { goal })).body
        .generation;
      resetState();
      return g.title;
    };
    expect(await titled("Trace orders through shipments and returns.")).toBe(
      "Orders through shipments and returns",
    );
    expect(await titled("which orders ship late, and why? By carrier")).toBe(
      "Orders ship late",
    );
    expect(await titled("Help us")).toBe("Us");
    expect(
      await titled(
        "Contracts with our customers: who we signed with and what they are billed.",
      ),
    ).toBe("Contracts with our customers");
    expect(
      await titled(
        "Help our customer service and operations teams trace every order from the customer who placed it",
      ),
    ).toBe("Customer service and operations teams trace every order");
    expect(await titled("x".repeat(80))).toBe("X" + "x".repeat(59));
    // No goal, no title: the gateway sends it as "".
    expect(await titled()).toBe("");
    // Seeded ones as the title model wrote them; the oldest is from before.
    const seeded = (await call("GET", `${ws}/generations`)).body.generations;
    expect(
      seeded.map((g: { id: string; title: string }) => [g.id, g.title]),
    ).toEqual([
      ["gen_seed_4", "Customer contracts and billing"],
      ["gen_seed_3", "Order tracing through shipments and returns"],
      ["gen_seed_2", "Late order shipments"],
      ["gen_seed_1", ""],
    ]);
  });

  it("fails as the knobs say, with a message for the customer", async () => {
    Object.assign(config, { ontologyStepMs: 10, ontologyOutcome: "fail" });
    const g = (await call("POST", `${ws}:generate`, {})).body.generation;
    // Every synced table of every connector: Stripe's and the shop's.
    expect(g.tables).toHaveLength(21);
    vi.setSystemTime(Date.now() + 20);
    const done = (await call("GET", `${ws}/generations`)).body.generations[0];
    expect(done).toMatchObject({
      id: g.id,
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      error: "Claude declined to draft this ontology",
    });
    // A reset clears the knobs.
    resetState();
    expect(config).not.toHaveProperty("ontologyOutcome");
  });

  it("retries a failed attempt as the worker does, then succeeds", async () => {
    Object.assign(config, { ontologyStepMs: 100, ontologyOutcome: "retry" });
    const g = (await call("POST", `${ws}:generate`, {})).body.generation;
    const t0 = Date.parse(g.createdAt);
    const get = async () =>
      (await call("GET", `${ws}/generations/${g.id}`)).body.generation;
    vi.setSystemTime(t0 + 150);
    expect(await get()).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
      attempts: 1,
    });
    vi.setSystemTime(t0 + 250);
    const waiting = await get();
    expect(waiting).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
      attempts: 1,
      error: "the ontology agent is unavailable; Helix will retry",
      startedAt: new Date(t0 + 100).toISOString(),
      nextAttemptAt: new Date(t0 + 300).toISOString(),
      finishedAt: null,
    });
    vi.setSystemTime(t0 + 350);
    expect(await get()).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
      attempts: 2,
      startedAt: new Date(t0 + 100).toISOString(),
    });
    vi.setSystemTime(t0 + 450);
    const done = await get();
    expect(done).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      attempts: 2,
      error: "",
      finishedAt: new Date(t0 + 400).toISOString(),
    });
    expect(done.resultVersionId).toBeTruthy();
  });

  it("goes straight from a retry to done when polled late", async () => {
    Object.assign(config, { ontologyStepMs: 100, ontologyOutcome: "retry" });
    const g = (await call("POST", `${ws}:generate`, {})).body.generation;
    vi.setSystemTime(Date.parse(g.createdAt) + 1000);
    const done = (await call("GET", `${ws}/generations`)).body.generations[0];
    expect(done).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      attempts: 2,
    });
  });

  it("fails a generation whose tables are no longer synced", async () => {
    const g = (
      await call("POST", `${ws}:generate`, {
        connectorIds: ["conn_acme_stripe"],
      })
    ).body.generation;
    expect(
      (
        await call(
          "DELETE",
          "/v1/workspaces/ws_acme/foundry/connectors/conn_acme_stripe",
        )
      ).status,
    ).toBe(200);
    vi.setSystemTime(Date.now() + 3000);
    expect(
      (await call("GET", `${ws}/generations/${g.id}`)).body.generation,
    ).toMatchObject({
      status: "ONTOLOGY_GENERATION_STATUS_FAILED",
      error:
        "none of the tables to generate from are synced any more; sync a connector and try again",
      resultVersionId: "",
    });
  });

  it("checks the request as GenerateOntology does", async () => {
    const tooLong = await call("POST", `${ws}:generate`, {
      goal: "x".repeat(2001),
    });
    expect(tooLong.body.message).toBe("goal is longer than 2000 characters");
    const unknown = await call("POST", `${ws}:generate`, {
      connectorIds: ["con_nope"],
    });
    expect(unknown).toMatchObject({ status: 404 });
    expect(unknown.body.message).toBe("connector con_nope not found");
    expect((await call("GET", `${ws}/generations/gen_nope`)).status).toBe(404);
  });
});
