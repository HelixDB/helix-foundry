import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction, resource } from "../apps/api/src/store.js";
import { config } from "../apps/api/src/config.js";
const store = new MemoryStore();
let app: Awaited<ReturnType<typeof createApp>>, scope: string;
beforeAll(async () => {
  app = await createApp(store);
  await app.ready();
  const r = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(r.statusCode).toBe(200);
  scope = r.json().workspaces[0].id;
});
afterAll(() => app.close());
const request = (
  method: any,
  path: string,
  payload?: unknown,
  headers: Record<string, string> = {},
) =>
  app.inject({
    method,
    url: "/api/v1" + path,
    payload: payload as any,
    headers,
  });
// A separate installation whose store can be seeded before its first request.
async function freshApp(seed?: (s: MemoryStore) => Promise<void>) {
  const s = new MemoryStore();
  await seed?.(s);
  const a = await createApp(s);
  await a.ready();
  return { store: s, app: a };
}
describe("workspace boundaries and actions", () => {
  it("sets up one local owner and workspace on first run and has no sign-in routes", async () => {
    const fresh = await freshApp();
    try {
      const me = async () =>
        (await fresh.app.inject({ method: "GET", url: "/api/v1/me" })).json();
      const first = await Promise.all([me(), me(), me(), me(), me()]);
      const again = await me();
      for (const r of [...first, again]) {
        expect(r.user).toEqual({ id: "local-user", name: "Local user" });
        expect(r.workspaces).toEqual([
          {
            id: first[0].workspaces[0].id,
            name: "My workspace",
            role: "owner",
          },
        ]);
      }
      expect(await fresh.store.list("global", "user")).toHaveLength(1);
      expect(await fresh.store.list("global", "workspaceRef")).toHaveLength(1);
      expect(await fresh.store.list("global", "member")).toHaveLength(1);
      expect(
        (await fresh.store.get("global", "installation"))?.data.ownerId,
      ).toBe("local-user");
      expect((await fresh.store.get("global", "local-session"))?.data).toEqual({
        userId: "local-user",
        expiresAt: Number.MAX_SAFE_INTEGER,
      });
    } finally {
      await fresh.app.close();
    }
    for (const [method, path] of [
      ["POST", "/auth/bootstrap"],
      ["POST", "/auth/login"],
      ["POST", "/auth/logout"],
      ["GET", "/auth/status"],
      ["POST", "/invitations/accept"],
      ["GET", `/workspaces/${scope}/members`],
      ["POST", `/workspaces/${scope}/invitations`],
    ])
      expect(
        (await request(method, path, method === "POST" ? {} : undefined))
          .statusCode,
      ).toBe(404);
  });
  it("adopts an existing installation's owner and workspaces", async () => {
    const fresh = await freshApp(async (s) => {
      for (const [id, name] of [
        ["ws-a", "Acme"],
        ["ws-b", "Beta"],
      ])
        await transaction(s, id, async (tx) =>
          tx.put(
            resource(
              id,
              "workspace",
              name,
              { activeGeneration: null, previousGeneration: null },
              "workspace",
            ),
          ),
        );
      await transaction(s, "global", async (tx) => {
        tx.put(
          resource(
            "global",
            "user",
            "Existing owner",
            { email: "owner@test.example" },
            "owner-1",
          ),
        );
        tx.put(
          resource(
            "global",
            "installation",
            "Helix Foundry",
            { ownerId: "owner-1" },
            "installation",
          ),
        );
        tx.put(resource("global", "workspaceRef", "Acme", {}, "ws-a"));
        tx.put(resource("global", "workspaceRef", "Beta", {}, "ws-b"));
        tx.put(
          resource(
            "global",
            "member",
            "Existing owner",
            { userId: "owner-1", role: "owner", workspaceId: "ws-a" },
            "owner-1:ws-a",
          ),
        );
      });
    });
    try {
      const me = (
        await fresh.app.inject({ method: "GET", url: "/api/v1/me" })
      ).json();
      expect(me.user).toEqual({
        id: "owner-1",
        name: "Existing owner",
        email: "owner@test.example",
      });
      expect(
        me.workspaces.sort((x: any, y: any) => x.id.localeCompare(y.id)),
      ).toEqual([
        { id: "ws-a", name: "Acme", role: "owner" },
        { id: "ws-b", name: "Beta", role: "owner" },
      ]);
      expect(await fresh.store.list("global", "user")).toHaveLength(1);
      expect(await fresh.store.list("global", "workspaceRef")).toHaveLength(2);
      expect((await fresh.store.get("global", "owner-1:ws-b"))?.data).toEqual({
        userId: "owner-1",
        role: "owner",
        workspaceId: "ws-b",
      });
      expect(
        (await fresh.store.get("global", "local-session"))?.data.userId,
      ).toBe("owner-1");
    } finally {
      await fresh.app.close();
    }
  });
  it("denies unknown workspaces and raw secret kinds", async () => {
    expect(
      (await request("GET", "/workspaces/foreign/overview")).statusCode,
    ).toBe(404);
    expect(
      (await request("GET", `/workspaces/${scope}/resources/provider`))
        .statusCode,
    ).toBe(404);
    expect(
      (
        await request("POST", `/workspaces/${scope}/assistant/runs`, {
          goal: "Explain this",
          intent: "auto",
          context: { resourceId: "foreign" },
        })
      ).statusCode,
    ).toBe(404);
  });
  it("lets the local owner run and cancel the assistant", async () => {
    const auto = await request("POST", `/workspaces/${scope}/assistant/runs`, {
      goal: "How many customers?",
      intent: "auto",
    });
    expect(auto.statusCode).toBe(200);
    expect(auto.json().data.authority).toEqual({
      userId: "local-user",
      readOnly: false,
    });
    expect(
      (
        await request(
          "POST",
          `/workspaces/${scope}/assistant/runs/${auto.json().id}/cancel`,
          {},
        )
      ).statusCode,
    ).toBe(200);
  });
  it("applies idempotent overlays, rejects stale edits and preserves imported values", async () => {
    await transaction(store, scope, async (tx) => {
      const w = await tx.get("workspace");
      tx.update(w!, { activeGeneration: "gen" });
      tx.put(
        resource(
          scope,
          "objectType",
          "Customer",
          { generation: "gen", primaryKey: "id", properties: ["id", "status"] },
          "type",
        ),
      );
      tx.put(
        resource(
          scope,
          "object",
          "Customer A",
          {
            generation: "gen",
            logicalId: "a",
            type: "Customer",
            base: { id: "a", status: "new" },
          },
          "obj",
        ),
      );
    });
    const body = {
      objectId: "obj",
      values: { status: "active" },
      revision: 0,
      idempotencyKey: "action-key-123",
    };
    const first = await request(
      "POST",
      `/workspaces/${scope}/actions/execute`,
      body,
    );
    expect(first.statusCode).toBe(200);
    const replay = await request(
      "POST",
      `/workspaces/${scope}/actions/execute`,
      body,
    );
    expect(replay.json().id).toBe(first.json().id);
    expect(
      (
        await request("POST", `/workspaces/${scope}/actions/execute`, {
          ...body,
          idempotencyKey: "another-key",
        })
      ).statusCode,
    ).toBe(409);
    expect(
      (
        await request("POST", `/workspaces/${scope}/actions/execute`, {
          ...body,
          values: { id: "changed" },
          revision: 1,
          idempotencyKey: "another-key",
        })
      ).statusCode,
    ).toBe(400);
    expect((await store.get(scope, "obj"))?.data.base.status).toBe("new");
    expect((await store.get(scope, "overlay_a"))?.data.values.status).toBe(
      "active",
    );
  });
  it("rejects mutations from another origin and requests for another host", async () => {
    const foreign = await request(
      "POST",
      `/workspaces/${scope}/assistant/runs`,
      { goal: "Unauthorized build" },
      { origin: "https://evil.example" },
    );
    expect(foreign.statusCode).toBe(403);
    expect(foreign.json().error).toBe("Origin is not allowed");
    const loopback = config.origin.replace("localhost", "127.0.0.1");
    expect(loopback).not.toBe(config.origin);
    expect(
      (
        await request(
          "POST",
          `/workspaces/${scope}/tokens`,
          { name: "Loopback", readOnly: true },
          { origin: loopback },
        )
      ).statusCode,
    ).toBe(200);
    const rebound = await request("GET", "/me", undefined, {
      host: "evil.example:3001",
    });
    expect(rebound.statusCode).toBe(403);
    expect(rebound.json().error).toBe("Host is not allowed");
  });
  it("never treats a bad Authorization header as the local owner", async () => {
    expect(
      (await request("GET", `/workspaces/${scope}/overview`)).statusCode,
    ).toBe(200);
    for (const authorization of ["Bearer not-a-real-token", "Basic b3duZXI="]) {
      const me = await request("GET", "/me", undefined, { authorization });
      expect(me.statusCode).toBe(401);
      expect(me.json().user).toBeUndefined();
      expect(
        (
          await request("GET", `/workspaces/${scope}/overview`, undefined, {
            authorization,
          })
        ).statusCode,
      ).toBe(401);
    }
  });
  it("runs a requested sync now when a scheduled one is waiting", async () => {
    await transaction(store, scope, async (tx) => {
      tx.put(resource(scope, "source", "Waiting", {}, "waiting"));
      tx.put(
        resource(
          scope,
          "job",
          "Sync Waiting",
          {
            type: "sync",
            sourceId: "waiting",
            status: "queued",
            background: true,
            trigger: "schedule",
          },
          "waiting-sync",
        ),
      );
    });
    const r = await request(
      "POST",
      `/workspaces/${scope}/sources/waiting/sync`,
    );
    expect(r.statusCode).toBe(200);
    expect(r.json()).toMatchObject({
      id: "waiting-sync",
      data: { status: "queued", background: false },
    });
    expect(
      (await store.list(scope, "job")).filter(
        (j) => j.data.sourceId === "waiting",
      ),
    ).toHaveLength(1);
  });
  it("scopes API tokens and limits read-only tokens", async () => {
    const r = await request("POST", `/workspaces/${scope}/tokens`, {
      name: "CI",
      readOnly: true,
    });
    const auth = { authorization: "Bearer " + r.json().token };
    expect(
      (await request("GET", `/workspaces/${scope}/overview`, undefined, auth))
        .statusCode,
    ).toBe(200);
    expect(
      (
        await request(
          "POST",
          `/workspaces/${scope}/assistant/runs`,
          { goal: "Build dashboard" },
          auth,
        )
      ).statusCode,
    ).toBe(403);
    const other = await request("POST", "/workspaces", { name: "Other" });
    expect(other.statusCode).toBe(200);
    expect(
      (
        await request(
          "GET",
          `/workspaces/${other.json().id}/overview`,
          undefined,
          auth,
        )
      ).statusCode,
    ).toBe(404);
    const writer = await request("POST", `/workspaces/${scope}/tokens`, {
      name: "Script",
      readOnly: false,
    });
    expect(
      (
        await request(
          "POST",
          "/workspaces",
          { name: "From a token" },
          { authorization: "Bearer " + writer.json().token },
        )
      ).statusCode,
    ).toBe(403);
  });
});

it("serves stable record links across generations with effective edits and workspace boundaries", async () => {
  await transaction(store, scope, async (tx) => {
    const ws = await tx.get("workspace");
    tx.update(ws!, { activeGeneration: "g1" });
    tx.put(
      resource(
        scope,
        "object",
        "Order A",
        {
          generation: "g1",
          logicalId: "stable",
          type: "Order",
          base: { status: "new" },
        },
        "g1_stable",
      ),
    );
    tx.put(
      resource(
        scope,
        "overlay",
        "Edit",
        { values: { status: "edited" } },
        "overlay_stable",
      ),
    );
  });
  let r = await request("GET", `/workspaces/${scope}/records/stable`);
  expect(r.statusCode).toBe(200);
  expect(r.json().record.data.effective.status).toBe("edited");
  const filtered = await request(
    "GET",
    `/workspaces/${scope}/resources/object?type=Order&field=status&value=edited`,
  );
  expect(filtered.json().total).toBe(1);
  await transaction(store, scope, async (tx) => {
    const ws = await tx.get("workspace");
    tx.update(ws!, { activeGeneration: "g2" });
    tx.put(
      resource(
        scope,
        "object",
        "Order A",
        {
          generation: "g2",
          logicalId: "stable",
          type: "Order",
          base: { status: "refreshed" },
        },
        "g2_stable",
      ),
    );
  });
  r = await request("GET", `/workspaces/${scope}/records/stable`);
  expect(r.json().record.id).toBe("g2_stable");
  expect(r.json().record.data.effective.status).toBe("edited");
  expect(
    (await request("GET", "/workspaces/foreign/records/stable")).statusCode,
  ).toBe(404);
});
