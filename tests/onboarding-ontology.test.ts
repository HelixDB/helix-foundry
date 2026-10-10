import { beforeEach, afterEach, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
// Lets a test hold sample ingestion to observe setup while samples load, or act
// while a query runs.
const gate = vi.hoisted(() => ({
  wait: undefined as Promise<void> | undefined,
  onQuery: undefined as ((job: any) => Promise<void>) | undefined,
}));
vi.mock("../apps/api/src/executor.js", () => ({
  execute: async (job: any) => {
    if (job.mode === "ingest" && gate.wait) await gate.wait;
    if (job.mode !== "ingest") await gate.onQuery?.(job);
    return localExecute(job);
  },
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace, pipelineId } from "../apps/api/src/assistant.js";
import { onboardingState } from "../apps/api/src/onboarding-state.js";
import { sampleDatasets } from "../apps/api/src/sample-data.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";
import { recordLabel } from "../apps/api/src/routes/ontology.js";
import { referenceReply } from "./fixtures-reference.js";

let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  actor: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const r = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(r.statusCode).toBe(200);
  scope = r.json().workspaces[0].id;
  actor = r.json().user.id;
});
afterEach(async () => {
  gate.wait = undefined;
  gate.onQuery = undefined;
  vi.restoreAllMocks();
  await app.close();
  for (const d of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});
const provider = () =>
  transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "AI",
        { settings: { provider: "local", model: "test-model" } },
        "provider",
      ),
    ),
  );
async function samples() {
  const r = await request("POST", "/demo", { advance: true });
  expect(r.statusCode, r.body).toBe(200);
}

it("moves to Outcome before the samples load and reports their progress", async () => {
  let release!: () => void;
  gate.wait = new Promise((r) => (release = r));
  const demo = Promise.resolve(request("POST", "/demo", { advance: true }));
  await vi.waitFor(async () =>
    expect((await onboardingState(store, scope)).sampleProgress).toBeDefined(),
  );
  const loading = await onboardingState(store, scope);
  expect(loading.step).toBe("outcome");
  expect(loading.sampleProgress).toEqual({
    total: 11,
    loaded: 0,
    pending: sampleDatasets().map((d) => d.name),
  });
  const early = await request("POST", "/onboarding/ontology", {});
  expect(early.statusCode).toBe(409);
  gate.wait = undefined;
  release();
  expect((await demo).statusCode).toBe(200);
  const loaded = await onboardingState(store, scope);
  expect(loaded.sampleProgress).toBeUndefined();
  expect(loaded.inputsReady).toBe(true);
  expect(loaded.step).toBe("outcome");
});

it("suggests an ontology once per data version and lets the AI describe it", async () => {
  await samples();
  const first = await request("POST", "/onboarding/ontology", {});
  expect(first.statusCode, first.body).toBe(200);
  const state = first.json();
  expect(state.ontology.entities[0].name).toBe("Account");
  expect(state.ontology.described).toBe(false);
  // Without a ready provider the computed text stays and no run is queued.
  expect(state.ontologyRun).toBeUndefined();
  await provider();
  const queued = (await request("POST", "/onboarding/ontology", {})).json();
  expect(queued.ontology).toEqual(state.ontology);
  expect(queued.ontologyRun.data).toMatchObject({
    task: "describe_ontology",
    status: "queued",
    ontologyHash: state.ontology.hash,
  });
  const again = (await request("POST", "/onboarding/ontology", {})).json();
  expect(again.ontologyRun.id).toBe(queued.ontologyRun.id);
  expect(await store.list(scope, "run")).toHaveLength(1);
  const ai = vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async (system, context) => ({
      value: referenceReply(system, context),
      tokens: 12,
    }));
  await buildWorkspace(store, scope, queued.ontologyRun.id);
  expect(ai).toHaveBeenCalledTimes(1);
  // The model sees the suggestion's structure, not dataset rows.
  const context = ai.mock.calls[0][1] as any;
  expect(context.datasets).toBeUndefined();
  expect(context.ontology.entities[0].sources).toHaveLength(3);
  const described = await onboardingState(store, scope);
  expect(described.ontologyRun?.data.status).toBe("succeeded");
  expect(described.ontology).toMatchObject({
    described: true,
    hash: state.ontology.hash,
    summary: "Accounts, their people, billing and support in one model.",
  });
  expect(described.ontology!.entities[0].description).toBe(
    "One account per row, from planetscale and workos and stripe.",
  );
  expect(
    described.ontology!.links.find((l) => l.id === "user-account_id-account")
      ?.name,
  ).toBe("is part of");
  expect(described.ontology!.entities.map((e) => e.members)).toEqual(
    state.ontology.entities.map((e: any) => e.members),
  );
  expect(described.ontology!.links.map((l) => l.id)).toEqual(
    state.ontology.links.map((l: any) => l.id),
  );
});

it("confirms a reviewed ontology, builds a pipeline-backed view from it and publishes it", async () => {
  // No AI is set up: confirming and building from a confirmed ontology is code.
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const stale = await request("POST", "/onboarding/ontology/confirm", {
    hash: "stale",
  });
  expect(stale.statusCode).toBe(409);
  expect(stale.json().error).toBe(
    "The suggested ontology changed. Review it again.",
  );
  const empty = await request("POST", "/onboarding/ontology/confirm", {
    hash: suggested.hash,
    excluded: suggested.entities
      .filter((e: any) => e.kind === "record")
      .map((e: any) => e.id),
  });
  expect(empty.statusCode).toBe(400);
  const review = {
    hash: suggested.hash,
    excluded: ["campaign"],
    names: { account: "Customer" },
  };
  const confirmed = await request(
    "POST",
    "/onboarding/ontology/confirm",
    review,
  );
  expect(confirmed.statusCode, confirmed.body).toBe(200);
  const state = confirmed.json();
  expect(state).toMatchObject({ step: "review", ontologyConfirmed: true });
  expect(state.buildRun.data.ontology.entities.map((e: any) => e.name)).toEqual(
    [
      "Customer",
      "User",
      "Subscription",
      "Invoice",
      "Support ticket",
      "SSO connection",
      "Event",
    ],
  );
  expect(state.buildRun.data.goal).toBe(
    "Give an overview of customers, users, subscriptions, invoices, support tickets, SSO connections and events, and how they connect.",
  );
  const repeated = await request(
    "POST",
    "/onboarding/ontology/confirm",
    review,
  );
  expect(repeated.json().buildRun.id).toBe(state.buildRun.id);

  const ai = vi
    .spyOn(Provider.prototype, "generate")
    .mockRejectedValue(new Error("The build must not call the model"));
  await buildWorkspace(store, scope, state.buildRun.id);
  expect(ai).not.toHaveBeenCalled();
  // No review step: the build publishes itself as the confirming user.
  const built = await onboardingState(store, scope);
  expect(built.complete, built.buildRun?.data.error).toBe(true);
  expect(built.buildRun?.data).toMatchObject({
    status: "succeeded",
    stage: "published",
  });
  expect(built.buildRun?.data.events.at(-1).message).toBe(
    "Workspace published",
  );
  const p = built.proposal!;
  expect(p.data.status).toBe("published");
  expect(
    p.data.validation.valid,
    JSON.stringify(p.data.validation.checks.filter((c: any) => !c.passed)),
  ).toBe(true);
  expect((await store.list(scope, "publication"))[0].data).toMatchObject({
    proposalId: p.id,
    actor,
  });
  expect((await store.get(scope, built.dashboardId!))?.name).toBe("Overview");
  const bundle = p.data.bundle;
  expect(bundle.pipelines.map((x: any) => x.name)).toEqual([
    "Customers (PlanetScale + WorkOS + Stripe)",
    "Users (PlanetScale + WorkOS)",
    "Support tickets (S3)",
  ]);
  const customer = bundle.ontology.find((t: any) => t.name === "Customer");
  expect(customer.datasetId).toBe(
    pipelineId("Customers (PlanetScale + WorkOS + Stripe)"),
  );
  expect(bundle.ontology.map((t: any) => t.name)).toEqual([
    "Customer",
    "User",
    "Subscription",
    "Invoice",
    "Support ticket",
    "SSO connection",
  ]);
  // Invoices and tickets reach customers in two hops, so no direct shortcut.
  expect(
    bundle.relationships.map((r: any) => [r.fromType, r.name, r.toType]),
  ).toEqual([
    ["User", "User belongs to", "Customer"],
    ["Subscription", "billed to", "Customer"],
    ["Invoice", "for subscription", "Subscription"],
    ["Support ticket", "requested by", "User"],
    ["SSO connection", "SSO connection belongs to", "Customer"],
  ]);
  for (const r of bundle.relationships) {
    const preview = p.data.validation.previews["relation:" + r.name];
    expect(Number(preview.rows[0].matched), r.name).toBeGreaterThan(0);
  }
  expect(bundle.dashboards.map((d: any) => d.name)).toEqual(["Overview"]);

  const types = await store.list(scope, "objectType");
  expect(types.find((t) => t.name === "Customer")?.data).toMatchObject({
    datasetId: customer.datasetId,
    objectCount: 60,
  });
  expect(types.map((t) => t.name)).not.toContain("Event");
  const relations = await store.list(scope, "relation");
  expect(relations.filter((r) => r.name === "requested by")).toHaveLength(114);
  expect(relations.filter((r) => r.name === "billed to")).toHaveLength(46);
  // Setup's progress counted every record and relationship row it read.
  expect(built.buildRun?.data.progress).toEqual({
    records:
      types.reduce((n, t) => n + t.data.objectCount, 0) + relations.length,
  });
  // Revisions keep building from the confirmed ontology. Asking for a new
  // goal is AI work, so it needs AI set up.
  await provider();
  const revision = await request("POST", "/onboarding/build", {
    goal: "Focus on accounts at risk",
    parentId: p.id,
  });
  expect(revision.json().data.ontology.entities[0].name).toBe("Customer");
});

it("recomputes the suggestion when the selected data changes", async () => {
  await samples();
  const first = (await request("POST", "/onboarding/ontology", {})).json();
  const events = first.datasets.find((d: any) => d.name === "PostHog · events");
  const patched = (
    await request("PATCH", "/onboarding", { excludedIds: [events.id] })
  ).json();
  expect(patched.ontology).toBeUndefined();
  expect(
    (await store.get(scope, "workspace"))?.data.onboarding.ontologySuggestion,
  ).toBeNull();
  const next = (await request("POST", "/onboarding/ontology", {})).json();
  expect(next.ontology.signature).not.toBe(first.ontology.signature);
  expect(next.ontology.entities.map((e: any) => e.name)).not.toContain("Event");
});

it("describes a restored suggestion again and keeps it across a reordered selection", async () => {
  await provider();
  await samples();
  const queued = (await request("POST", "/onboarding/ontology", {})).json();
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system, context) => ({
      value: referenceReply(system, context),
      tokens: 12,
    }),
  );
  await buildWorkspace(store, scope, queued.ontologyRun.id);
  const events = queued.datasets.find(
    (d: any) => d.name === "PostHog · events",
  );
  const campaigns = queued.datasets.find(
    (d: any) => d.name === "S3 · marketing_campaigns.csv",
  );
  await request("PATCH", "/onboarding", { excludedIds: [events.id] });
  await request("PATCH", "/onboarding", { excludedIds: [] });
  const restored = (await request("POST", "/onboarding/ontology", {})).json();
  expect(restored.ontology.described).toBe(false);
  expect(restored.ontologyRun.id).not.toBe(queued.ontologyRun.id);
  expect(restored.ontologyRun.data.status).toBe("queued");
  const again = (await request("POST", "/onboarding/ontology", {})).json();
  expect(again.ontologyRun.id).toBe(restored.ontologyRun.id);
  await buildWorkspace(store, scope, restored.ontologyRun.id);
  expect((await onboardingState(store, scope)).ontology?.described).toBe(true);

  await request("PATCH", "/onboarding", {
    excludedIds: [events.id, campaigns.id],
  });
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const review = {
    hash: suggested.hash,
    excluded: ["support_ticket"],
    names: { account: "Customer" },
  };
  const confirmed = (
    await request("POST", "/onboarding/ontology/confirm", review)
  ).json();
  expect(confirmed.ontologyEdits).toEqual({
    excluded: review.excluded,
    names: review.names,
  });
  // The same set in another order changes nothing.
  const reordered = (
    await request("PATCH", "/onboarding", {
      excludedIds: [campaigns.id, events.id],
    })
  ).json();
  expect(reordered.ontologyConfirmed).toBe(true);
  expect(reordered.buildRun.id).toBe(confirmed.buildRun.id);
  expect(reordered.ontology.hash).toBe(suggested.hash);
  expect(reordered.ontologyEdits).toEqual(confirmed.ontologyEdits);
});

it("builds from dashboards alone when no table has a usable key", async () => {
  await provider();
  await request("POST", "/ingest", {
    name: "readings",
    rows: [1, 2, 3, 4].map(() => ({ sensor: "A", value: 1 })),
  });
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  expect(suggested.entities).toEqual([]);
  const confirmed = await request("POST", "/onboarding/ontology/confirm", {
    hash: suggested.hash,
  });
  expect(confirmed.statusCode, confirmed.body).toBe(200);
  expect(confirmed.json().buildRun.data.ontology.entities).toEqual([]);
  await buildWorkspace(store, scope, confirmed.json().buildRun.id);
  const built = await onboardingState(store, scope);
  expect(built.complete, built.buildRun?.data.error).toBe(true);
  // Without records the overview counts every table's rows.
  const dashboard = await store.get(scope, built.dashboardId!);
  expect(dashboard?.data.widgets.map((w: any) => w.title)).toEqual(["Records"]);
  expect(built.proposal!.data.validation.previews["chart:Overview:0"]).toEqual([
    { records: "4" },
  ]);
});

it("publishes a computed overview without calling the model", async () => {
  await provider();
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const state = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: suggested.hash,
      names: { account: "Customer" },
    })
  ).json();
  // AI is set up, but only describes the suggestion; the build doesn't use it.
  const ai = vi.spyOn(Provider.prototype, "generate");
  await buildWorkspace(store, scope, state.buildRun.id);
  expect(ai).not.toHaveBeenCalled();
  const built = await onboardingState(store, scope);
  expect(built.complete, built.buildRun?.data.error).toBe(true);
  expect(built.buildRun?.data.status).toBe("succeeded");
  const dashboard = await store.get(scope, built.dashboardId!);
  expect(dashboard?.name).toBe("Overview");
  expect(
    dashboard?.data.widgets.map((w: any) => [w.title, w.type, w.inputs]),
  ).toEqual([
    ["Customers", "metric", [suggested.entities[0].anchorDatasetId]],
    [
      "Records by type",
      "bar",
      expect.arrayContaining([suggested.entities[0].anchorDatasetId]),
    ],
    [
      "New customers per month",
      "line",
      [suggested.entities[0].anchorDatasetId],
    ],
  ]);
  const previews = built.proposal!.data.validation.previews as any;
  expect(previews["chart:Overview:0"]).toEqual([{ records: "60" }]);
  expect(previews["chart:Overview:1"][0]).toEqual({
    type: "Users",
    records: "337",
  });
  expect(previews["chart:Overview:1"].map((r: any) => r.type).sort()).toEqual(
    [
      "Campaigns",
      "Customers",
      "Invoices",
      "SSO connections",
      "Subscriptions",
      "Support tickets",
      "Users",
    ].sort(),
  );
  expect(
    previews["chart:Overview:2"].reduce(
      (n: number, r: any) => n + Number(r.records),
      0,
    ),
  ).toBe(60);
  expect(previews["chart:Overview:2"][0].month).toMatch(/^\d{4}-\d{2}$/);
  // Records and relationships come from the confirmed ontology as before.
  expect(
    (await store.list(scope, "objectType")).find((t) => t.name === "Customer")
      ?.data.objectCount,
  ).toBe(60);
});

it("queues a build left waiting for the former review page again on confirm", async () => {
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const state = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: suggested.hash,
    })
  ).json();
  await transaction(store, scope, async (tx) => {
    const r = (await tx.get(state.buildRun.id))!;
    tx.update(r, { ...r.data, status: "awaiting_review" });
  });
  const again = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: suggested.hash,
    })
  ).json();
  expect(again.buildRun).toMatchObject({
    id: state.buildRun.id,
    data: { status: "queued" },
  });
  await buildWorkspace(store, scope, state.buildRun.id);
  const built = await onboardingState(store, scope);
  expect(built.complete, built.buildRun?.data.error).toBe(true);
  expect((await store.get(scope, built.dashboardId!))?.name).toBe("Overview");
});

it("finishes publishing once started and refuses Stop meanwhile", async () => {
  await provider();
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const state = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: suggested.hash,
    })
  ).json();
  let stop: any;
  gate.onQuery = async (job) => {
    // Publication reads each type's records with one query.
    if (!stop && job.mode === "materialize")
      stop = await request(
        "POST",
        `/assistant/runs/${state.buildRun.id}/cancel`,
        {},
      );
  };
  await buildWorkspace(store, scope, state.buildRun.id);
  expect(stop.statusCode).toBe(409);
  expect(stop.json().error).toBe("Your workspace is already being published.");
  const built = await onboardingState(store, scope);
  expect(built.complete).toBe(true);
  expect(built.buildRun?.data.status).toBe("succeeded");
});

it("restarts a failed build on Retry or on confirming again, as the acting owner", async () => {
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const confirm = () =>
    request("POST", "/onboarding/ontology/confirm", { hash: suggested.hash });
  const id = (await confirm()).json().buildRun.id;
  // The query engine is down, so nothing can be checked or published.
  gate.onQuery = async () => {
    throw new Error("Dataset execution failed");
  };
  await buildWorkspace(store, scope, id);
  expect((await store.get(scope, id))?.data.status).toBe("failed");
  const resumed = await request("POST", `/assistant/runs/${id}/resume`, {});
  expect(resumed.json().data).toMatchObject({
    status: "queued",
    stage: "queued",
  });
  await buildWorkspace(store, scope, id);
  expect((await store.get(scope, id))?.data.status).toBe("failed");
  // The run still carries the authority of a user who is no longer a member
  // (e.g. from an earlier install); confirming again hands it to the local owner.
  await transaction(store, scope, async (tx) => {
    const r = (await tx.get(id))!;
    tx.update(r, {
      ...r.data,
      authority: { userId: "stale-user", readOnly: false },
    });
  });
  await request("PATCH", "/onboarding", { step: "outcome" });
  const again = (await confirm()).json();
  expect(again.buildRun).toMatchObject({
    id,
    data: {
      status: "queued",
      error: null,
      authority: { userId: actor },
    },
  });
  expect(again.buildRun.data.events.at(-1).message).toBe(
    "Building your workspace again",
  );
  gate.onQuery = undefined;
  await buildWorkspace(store, scope, id);
  const built = await onboardingState(store, scope);
  expect(built.complete, built.buildRun?.data.error).toBe(true);
  expect((await store.list(scope, "publication"))[0].data.actor).toBe(actor);
});

it("reports a malformed AI reply in plain words", async () => {
  await provider();
  await samples();
  const run = (await request("POST", "/onboarding/ontology", {})).json()
    .ontologyRun;
  vi.spyOn(Provider.prototype, "generate").mockResolvedValue({
    value: { summary: 1 },
    tokens: 12,
  });
  await buildWorkspace(store, scope, run.id);
  expect((await store.get(scope, run.id))?.data).toMatchObject({
    status: "failed",
    error:
      "The AI’s reply didn’t match the expected format. Retry, or choose a more capable model.",
  });
});

it("loads each sample once when a second /demo arrives mid-load", async () => {
  let release!: () => void;
  gate.wait = new Promise((r) => (release = r));
  const first = Promise.resolve(request("POST", "/demo", { advance: true }));
  await vi.waitFor(async () =>
    expect((await onboardingState(store, scope)).sampleProgress).toBeDefined(),
  );
  const second = Promise.resolve(request("POST", "/demo", { advance: true }));
  gate.wait = undefined;
  release();
  expect((await first).statusCode).toBe(200);
  expect((await second).statusCode).toBe(200);
  const datasets = await store.list(scope, "dataset");
  expect(datasets).toHaveLength(sampleDatasets().length);
  expect(datasets.every((d) => d.data.versions.length === 1)).toBe(true);
});

it("serves the published records as a graph, whole or around one record", async () => {
  await samples();
  const suggested = (await request("POST", "/onboarding/ontology", {})).json()
    .ontology;
  const state = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: suggested.hash,
    })
  ).json();
  await buildWorkspace(store, scope, state.buildRun.id);
  const all = (await request("GET", "/graph")).json();
  expect(all.nodes).toHaveLength(all.total);
  expect(all.edges.length).toBeGreaterThan(0);
  const ids = new Set(all.nodes.map((n: any) => n.id));
  expect(all.edges.every((e: any) => ids.has(e.from) && ids.has(e.to))).toBe(
    true,
  );
  // Records are named by their own fields where they have one.
  const account = all.nodes
    .filter((n: any) => n.type === "Account")
    .sort((a: any, b: any) => b.degree - a.degree)[0];
  expect(account.label).not.toMatch(/^Account /);
  expect(
    all.nodes.some((n: any) => n.type === "User" && !/^User /.test(n.label)),
  ).toBe(true);
  expect(
    all.nodes
      .filter((n: any) => n.type === "Support ticket")
      .every((n: any) => n.label.startsWith("Support ticket ")),
  ).toBe(true);
  // Around one record: it comes first, and everything else links to it.
  const near = (
    await request("GET", `/graph?focus=${encodeURIComponent(account.id)}`)
  ).json();
  expect(near.nodes[0].id).toBe(account.id);
  expect(near.nodes.length).toBeGreaterThan(1);
  for (const n of near.nodes.slice(1))
    expect(
      near.edges.some(
        (e: any) =>
          (e.from === account.id && e.to === n.id) ||
          (e.to === account.id && e.from === n.id),
      ),
    ).toBe(true);
  const wider = (
    await request(
      "GET",
      `/graph?focus=${encodeURIComponent(account.id)}&depth=2`,
    )
  ).json();
  expect(wider.nodes.length).toBeGreaterThan(near.nodes.length);
  // Too big to draw: the best-connected records.
  const top = (await request("GET", "/graph?limit=10")).json();
  expect(top.nodes).toHaveLength(10);
  expect(top.total).toBe(all.total);
  expect(top.nodes[0].degree).toBeGreaterThanOrEqual(top.nodes[9].degree);
  expect((await request("GET", "/graph?focus=missing")).statusCode).toBe(404);
  // The table lists the same records with their names and link counts, and
  // sorts by links across every page.
  const listed = (
    await request(
      "GET",
      "/resources/object?type=Account&sort=links&dir=desc&limit=5",
    )
  ).json();
  expect(listed.items[0].data).toMatchObject({
    label: account.label,
    links: account.degree,
  });
  const counts = listed.items.map((o: any) => o.data.links);
  expect(counts).toEqual([...counts].sort((a, b) => b - a));
});

it("names records readably", () => {
  expect(
    recordLabel("User usr_1", "User", {
      workos_first_name: "Omar",
      workos_last_name: "Walsh",
      email: "omar@x.example",
    }),
  ).toBe("Omar Walsh");
  expect(
    recordLabel("Account acct_1", "Account", { name: "Marigold", plan: "x" }),
  ).toBe("Marigold");
  expect(
    recordLabel("Support ticket 4101", "Support ticket", {
      requester_email: "someone@x.example",
      name: "None",
    }),
  ).toBe("Support ticket 4101");
  expect(
    recordLabel("Invoice in_1aFTyPqAEQQCd1LrWgyIgIyF", "Invoice", {}),
  ).toBe("Invoice in_1aFTyPqAE…");
});
