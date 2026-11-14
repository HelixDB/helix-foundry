import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type * as wfe from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import { contractErrors } from "../../mock/agent-contract";
import { routes } from "../../mock/routes";
import { handleSharedDashboard } from "../../mock/routes/dashboards";
import { handleDocumentUpload } from "../../mock/routes/documents";
import { config, db, resetState } from "../../mock/state";

// The mock's dashboards behave as the backend's (backend/api/dashboards.go,
// backend/dashboard): every member sees them, a member changes only their
// own unpinned ones, owners and admins pin and publish, edits name the
// revision read, tiles are read through the load's :read as the caller, and
// a published snapshot names no id and is served with no session.

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

const D = "/v1/workspaces/ws_acme/foundry/dashboards",
  O = "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts";
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const reason = (r: MockReply & { body: any }) => r.body.details?.[0]?.reason;

const tile = (id: string, query: unknown, extra: object = {}) => ({
  id,
  title: `Tile ${id}`,
  ontology_id: "ont_acme_contracts",
  load: { kind: "current" },
  query,
  visual: { kind: "bar" },
  ...extra,
});
const customers = { start: { type: "Customer" }, return: { count: {} } };
const byStatus = {
  start: { type: "Invoice" },
  return: { group_count: { at: 0, prop: "status" } },
};
const totalDue = {
  start: { type: "Invoice" },
  return: { aggregate: { fn: "sum", at: 0, prop: "amount_due" } },
};
const named = {
  start: { type: "Customer" },
  return: {
    rows: { columns: [{ at: 0 }, { at: 0, prop: "currency" }], limit: 5 },
  },
};
const spec = (tiles: object[], rows: string[][]) =>
  JSON.stringify({ v: 1, tiles, layout: { rows } });

async function create(
  specJson = "",
  userId = "usr_ada",
): Promise<wfe.FoundryDashboard> {
  const r = await call(
    "POST",
    D,
    { name: "Revenue", specJson, clientToken: `c-${Math.random()}` },
    userId,
  );
  expect(r.status, JSON.stringify(r.body)).toBe(200);
  return r.body.dashboard;
}

const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);

/** Uploads the seeded contract into General through its presigned PUT. */
async function uploadContract() {
  const { document, upload } = (
    await call("POST", "/v1/workspaces/ws_acme/foundry/documents", {
      name: "contract.md",
      mediaType: "text/markdown",
      sizeBytes: String(CONTRACT.length),
      sha256: createHash("sha256").update(CONTRACT).digest("hex"),
      clientToken: "c",
      setId: "set_acme_general",
    })
  ).body;
  const req = Object.assign(Readable.from([CONTRACT]), {
    method: "PUT",
    headers: Object.fromEntries(
      Object.entries(upload.headers as Record<string, string>).map(([k, v]) => [
        k.toLowerCase(),
        v,
      ]),
    ),
  }) as unknown as IncomingMessage;
  const res = {
    setHeader: () => res,
    writeHead: () => res,
    end: () => undefined,
  } as unknown as ServerResponse;
  await handleDocumentUpload(req, res, new URL(upload.url));
  await call(
    "POST",
    `/v1/workspaces/ws_acme/foundry/documents/${document.id}:complete`,
    {},
  );
}

/** Customer contracts' version 3 loaded with the contract, current. */
async function loaded() {
  Object.assign(config, { loadStepMs: 50 });
  await uploadContract();
  const load = (
    await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "l",
    })
  ).body.load;
  later(10_000);
  expect((await call("GET", `${O}/loads/${load.id}`)).body.load).toMatchObject({
    status: "ready",
    current: true,
  });
  return load.id as string;
}

/** The door's answer for token. */
function shared(token: string) {
  let status = 0,
    headers: Record<string, string> = {},
    text = "";
  const res = {
    writeHead: (s: number, h: Record<string, string>) => {
      status = s;
      headers = h;
      return res;
    },
    end: (b: string) => {
      text = b;
    },
  } as unknown as ServerResponse;
  const url = new URL(`http://door/v1/shared-dashboards/${token}`);
  expect(
    handleSharedDashboard({ method: "GET" } as IncomingMessage, res, url),
  ).toBe(true);
  return { status, headers, body: JSON.parse(text) };
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-14T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("mock dashboards", () => {
  it("creates, lists and reads a dashboard, the same click once", async () => {
    const body = {
      name: "  Revenue ",
      specJson: spec([tile("t1", customers)], [["t1"]]),
      clientToken: "c1",
    };
    const made = (await call("POST", D, body)).body.dashboard;
    expect(made).toMatchObject({
      name: "Revenue",
      revision: "1",
      pinned: false,
      canEdit: true,
      canPin: true,
      canPublish: true,
    });
    expect(JSON.parse(made.specJson).tiles[0].query).toEqual({
      return: { count: {} },
      start: { type: "Customer" },
    });
    expect((await call("POST", D, body)).body.dashboard.id).toBe(made.id);
    const list = (await call("GET", D, undefined, "usr_alan")).body;
    expect(list.canCreate).toBe(true);
    expect(list.dashboards).toHaveLength(1);
    expect(list.dashboards[0]).toMatchObject({ specJson: "", canEdit: false });
  });

  it("refuses what the backend refuses", async () => {
    const refusal = async (specJson: string) =>
      (await call("POST", D, { name: "x", specJson, clientToken: "x" })).body
        .message as string;
    expect(await refusal(`{"v":2}`)).toContain("reads v 1");
    expect(await refusal(spec([tile("T", customers)], [["T"]]))).toContain(
      "lowercase",
    );
    expect(
      await refusal(spec([tile("t1", { start: { type: "X" } })], [["t1"]])),
    ).toContain("not a Query");
    expect(await refusal(spec([tile("t1", customers)], []))).toContain(
      "leaves out tile t1",
    );
    expect(
      await refusal(
        spec([tile("t1", customers, { ontology_id: "ont_other" })], [["t1"]]),
      ),
    ).toContain("doesn't have");
    const noName = await call("POST", D, { name: " ", clientToken: "x" });
    expect(noName.status).toBe(400);
  });

  it("lets a member change only their own unpinned dashboards", async () => {
    const mine = await create("", "usr_alan");
    const edit = (userId: string, revision: string, name: string) =>
      call(
        "PATCH",
        `${D}/${mine.id}`,
        { expectedRevision: revision, name },
        userId,
      );
    expect((await edit("usr_alan", "1", "Mine")).body.dashboard.revision).toBe(
      "2",
    );
    expect(reason(await edit("usr_alan", "1", "Stale"))).toBe(
      "FOUNDRY_DASHBOARD_CHANGED",
    );
    const adas = await create();
    expect(
      (
        await call(
          "PATCH",
          `${D}/${adas.id}`,
          { expectedRevision: "1", name: "x" },
          "usr_alan",
        )
      ).status,
    ).toBe(403);
    expect(
      (await call("POST", `${D}/${mine.id}:pin`, { pinned: true }, "usr_alan"))
        .status,
    ).toBe(403);
    const pinned = await call("POST", `${D}/${mine.id}:pin`, { pinned: true });
    expect(pinned.body.dashboard).toMatchObject({
      pinned: true,
      pinnedByUserId: "usr_ada",
    });
    expect((await edit("usr_alan", "2", "Pinned")).status).toBe(403);
    expect(
      (await call("DELETE", `${D}/${mine.id}`, undefined, "usr_alan")).status,
    ).toBe(403);
    expect((await edit("usr_ada", "2", "By an owner")).status).toBe(200);
    expect((await call("DELETE", `${D}/${mine.id}`)).status).toBe(200);
    expect((await call("GET", `${D}/${mine.id}`)).status).toBe(404);
  });

  it("previews a tile as the caller, through the load's read", async () => {
    const t = JSON.stringify(tile("t1", customers));
    const none = await call("POST", `${D}:preview-tile`, { tileJson: t });
    expect(reason(none)).toBe("FOUNDRY_DASHBOARD_NOT_LOADED");
    const loadId = await loaded();
    const r = (await call("POST", `${D}:preview-tile`, { tileJson: t })).body;
    expect(r.loadId).toBe(loadId);
    const outcome = JSON.parse(r.outcomeJson);
    expect(outcome.ok.kind).toBe("query");
    expect(outcome.ok.result.kind).toBe("count");
    expect(contractErrors("read_result#/$defs/ReadOutcome", outcome)).toEqual(
      [],
    );
    const bad = await call("POST", `${D}:preview-tile`, { tileJson: "{" });
    expect(bad.status).toBe(400);
  });

  it("publishes a snapshot that names no id, and ends it", async () => {
    const loadId = await loaded();
    const d = await create(
      spec(
        [
          tile("t1", customers),
          tile("t2", named, { load: { kind: "pinned", load_id: loadId } }),
        ],
        [["t1", "t2"]],
      ),
    );
    const member = await call(
      "POST",
      `${D}/${d.id}:publish`,
      { expectedRevision: "1" },
      "usr_alan",
    );
    expect(member.status).toBe(403);
    const published = (
      await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "1" })
    ).body.dashboard;
    const token = published.share.token as string;
    expect(token).toMatch(/^hxd_[A-Za-z0-9_-]{43}$/);
    const page = shared(token);
    expect(page.status).toBe(200);
    expect(page.headers["Access-Control-Allow-Origin"]).toBe("*");
    expect(page.headers["Cache-Control"]).toBe("no-store");
    const snapshot = page.body as wfe.DashboardSnapshot;
    expect(snapshot.tiles.map((t) => t.output.kind)).toEqual(["count", "rows"]);
    const rows = snapshot.tiles[1]!.output as Extract<
      wfe.QueryOutput,
      { kind: "rows" }
    >;
    expect(rows.rows[0]!.cells[0]).toEqual({
      type: "Customer",
      name: expect.any(String),
    });
    expect(rows.rows[0]!.edges).toEqual([]);
    const text = JSON.stringify(snapshot);
    for (const id of ["ws_acme", "ont_acme_contracts", loadId, "usr_ada", d.id])
      expect(text).not.toContain(id);
    expect(
      (await call("GET", `${D}/${d.id}`, undefined, "usr_alan")).body.dashboard
        .share.token,
    ).toBe("");

    const again = (
      await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "1" })
    ).body.dashboard;
    expect(again.share.token).toBe(token);
    await call("POST", `${D}/${d.id}:unpublish`, {});
    expect(shared(token).status).toBe(404);
  });

  it("reads and publishes counts by value and aggregates", async () => {
    await loaded();
    const d = await create(
      spec(
        [tile("t1", byStatus), tile("t2", totalDue, { visual: {} })],
        [["t1", "t2"]],
      ),
    );
    for (const t of [tile("t1", byStatus), tile("t2", totalDue)]) {
      const r = (
        await call("POST", `${D}:preview-tile`, { tileJson: JSON.stringify(t) })
      ).body;
      const outcome = JSON.parse(r.outcomeJson);
      expect(contractErrors("read_result#/$defs/ReadOutcome", outcome)).toEqual(
        [],
      );
    }
    const published = (
      await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "1" })
    ).body.dashboard;
    const snapshot = shared(published.share.token)
      .body as wfe.DashboardSnapshot;
    const [groups, sum] = snapshot.tiles.map((t) => t.output);
    expect(groups).toMatchObject({ kind: "group_count" });
    expect(
      (groups as Extract<wfe.QueryOutput, { kind: "group_count" }>).groups
        .map((g) => g.count)
        .reduce((a, b) => a + b, 0),
    ).toBe(65);
    expect(sum).toMatchObject({ kind: "aggregate" });
    expect(
      (sum as Extract<wfe.QueryOutput, { kind: "aggregate" }>).values,
    ).toBeGreaterThan(0);
  });

  it("refuses a publish it can't make", async () => {
    const empty = await create();
    expect(
      reason(
        await call("POST", `${D}/${empty.id}:publish`, {
          expectedRevision: "1",
        }),
      ),
    ).toBe("FOUNDRY_DASHBOARD_EMPTY");
    const d = await create(spec([tile("t1", customers)], [["t1"]]));
    expect(
      reason(
        await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "1" }),
      ),
    ).toBe("FOUNDRY_DASHBOARD_NOT_LOADED");
    await loaded();
    expect(
      reason(
        await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "9" }),
      ),
    ).toBe("FOUNDRY_DASHBOARD_CHANGED");
    const broken = await create(
      spec(
        [tile("t1", { start: { type: "Nothing" }, return: { count: {} } })],
        [["t1"]],
      ),
    );
    expect(
      reason(
        await call("POST", `${D}/${broken.id}:publish`, {
          expectedRevision: "1",
        }),
      ),
    ).toBe("FOUNDRY_DASHBOARD_TILE_FAILED");
    Object.assign(config, { dashboardsPublic: false });
    expect(
      reason(
        await call("POST", `${D}/${d.id}:publish`, { expectedRevision: "1" }),
      ),
    ).toBe("FOUNDRY_DASHBOARDS_PUBLIC_OFF");
    expect((await call("GET", `${D}/${d.id}`)).body.dashboard.canPublish).toBe(
      false,
    );
    expect(shared("hxd_" + "a".repeat(43)).status).toBe(404);
  });

  it("adds an Analyst answer's visual as a tile", async () => {
    Object.assign(config, { askStepMs: 10 });
    await loaded();
    const asked = (
      await call("POST", `${O}/questions`, {
        question: "How many invoices are there by status?",
        clientToken: "q",
      })
    ).body.question;
    later(60_000);
    const q = (await call("GET", `${O}/questions/${asked.id}`)).body.question;
    const result = JSON.parse(q.resultJson);
    const evidence = result.queries[0].evidence_id as string;
    const d = await create();
    const added = (
      await call("POST", `${D}/${d.id}:add-question-visual`, {
        expectedRevision: "1",
        ontologyId: "ont_acme_contracts",
        questionId: asked.id,
        evidenceId: evidence,
      })
    ).body.dashboard;
    const s = JSON.parse(added.specJson) as wfe.DashboardSpec;
    expect(s.tiles[0]).toMatchObject({ id: "t1", load: { kind: "current" } });
    expect(s.tiles[0]!.query).toEqual(
      JSON.parse(JSON.stringify(result.queries[0].query)),
    );
    expect(s.layout.rows).toEqual([["t1"]]);
    const missing = await call("POST", `${D}/${d.id}:add-question-visual`, {
      expectedRevision: "2",
      ontologyId: "ont_acme_contracts",
      questionId: asked.id,
      evidenceId: "e99",
    });
    expect(missing.status).toBe(400);
  });
});

describe("a restricted member's dashboards", () => {
  it("lists the pinned ones only", async () => {
    const mine = await create();
    const pinned = await create();
    await call("POST", `${D}/${pinned.id}:pin`, { pinned: true });
    const list = (await call("GET", D, undefined, "usr_eve")).body;
    expect(list.canCreate).toBe(false);
    expect(list.dashboards.map((d: wfe.FoundryDashboard) => d.id)).toEqual([
      pinned.id,
    ]);
    expect(list.dashboards[0]).toMatchObject({
      canEdit: false,
      canPin: false,
      canPublish: false,
    });
    expect(
      (await call("GET", `${D}/${mine.id}`, undefined, "usr_eve")).status,
    ).toBe(404);
    const made = await call(
      "POST",
      D,
      { name: "x", clientToken: "e" },
      "usr_eve",
    );
    expect(made.status).toBe(403);
  });
});
