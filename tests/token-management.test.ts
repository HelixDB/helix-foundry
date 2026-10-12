import { afterEach, describe, expect, it } from "vitest";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
import { digest } from "../apps/api/src/security.js";

const apps: Awaited<ReturnType<typeof createApp>>[] = [];
afterEach(async () => {
  for (const app of apps.splice(0)) await app.close();
});
async function installation() {
  const store = new MemoryStore(),
    app = await createApp(store);
  apps.push(app);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  const base = `/api/v1/workspaces/${scope}`;
  const create = async (name: string, readOnly = true, workspace = scope) => {
    const r = await app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${workspace}/tokens`,
      payload: { name, readOnly },
    });
    expect(r.statusCode).toBe(200);
    return r.json() as { id: string; token: string };
  };
  return { app, store, scope, base, create, origin };
}

describe("workspace API token lifecycle", () => {
  it("lists only opaque workspace metadata and revokes through the real loopback SDK", async () => {
    const { app, scope, base, create, origin } = await installation();
    const reader = await create("Notebook reader"),
      writer = await create("Automation", false);
    const other = (
      await app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Other workspace" },
      })
    ).json().id;
    const foreign = await create("Other reader", true, other);
    const inventory = await app.inject(base + "/tokens");
    expect(inventory.statusCode).toBe(200);
    const items = inventory.json().items;
    expect(items).toHaveLength(2);
    for (const item of items) {
      expect(Object.keys(item).sort()).toEqual([
        "createdAt",
        "id",
        "name",
        "readOnly",
      ]);
      expect(item.id).toMatch(/^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/);
    }
    expect(inventory.body).not.toContain(digest(reader.token));
    expect(inventory.body).not.toContain(reader.token);
    expect(inventory.body).not.toContain(foreign.id);
    expect(inventory.body).not.toContain("Other reader");
    const client = new FoundryClient({
      baseUrl: origin,
      workspaceId: scope,
      token: writer.token,
    });
    expect((await client.tokens()).items.map((t) => t.name).sort()).toEqual([
      "Automation",
      "Notebook reader",
    ]);
    const temporary = await client.createToken("SDK temporary");
    expect(
      (await client.tokens()).items.find((t) => t.id === temporary.id)
        ?.readOnly,
    ).toBe(true);
    await client.revokeToken(temporary.id);
    await client.revokeToken(reader.id);
    expect(
      (
        await app.inject({
          url: base + "/overview",
          headers: { authorization: "Bearer " + reader.token },
        })
      ).statusCode,
    ).toBe(401);
    expect((await client.tokens()).items.map((t) => t.name)).toEqual([
      "Automation",
    ]);
    expect(
      (
        await app.inject({
          url: `/api/v1/workspaces/${other}/overview`,
          headers: { authorization: "Bearer " + foreign.token },
        })
      ).statusCode,
    ).toBe(200);
  });
  it("assigns stable opaque handles to tokens created before inventory existed", async () => {
    const { app, store, scope, base } = await installation();
    const raw = "synthetic-legacy-credential-only-for-local-test";
    await transaction(store, "global", async (tx) =>
      tx.put(
        resource(
          "global",
          "token",
          "Legacy notebook",
          { userId: "local-user", workspaceId: scope, readOnly: true },
          digest(raw),
        ),
      ),
    );
    const first = await app.inject(base + "/tokens");
    expect(first.statusCode).toBe(200);
    const entry = first.json().items[0];
    expect(entry.id).toMatch(/^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/);
    expect((await app.inject(base + "/tokens")).json().items[0].id).toBe(
      entry.id,
    );
    expect(first.body).not.toContain(digest(raw));
    expect(
      (
        await app.inject({
          method: "DELETE",
          url: base + "/tokens/" + entry.id,
        })
      ).statusCode,
    ).toBe(200);
    expect(
      (
        await app.inject({
          url: base + "/overview",
          headers: { authorization: "Bearer " + raw },
        })
      ).statusCode,
    ).toBe(401);
  });
  it("keeps legacy revocation IDs working and preserves unrelated credentials", async () => {
    const { app, base, create } = await installation();
    const first = await create("Old integration"),
      second = await create("Keep integration");
    const legacyId = digest(first.token);
    expect(
      (
        await app.inject({
          method: "DELETE",
          url: base + "/tokens/" + legacyId,
        })
      ).statusCode,
    ).toBe(200);
    expect(
      (
        await app.inject({
          url: base + "/overview",
          headers: { authorization: "Bearer " + first.token },
        })
      ).statusCode,
    ).toBe(401);
    expect(
      (
        await app.inject({
          url: base + "/overview",
          headers: { authorization: "Bearer " + second.token },
        })
      ).statusCode,
    ).toBe(200);
  });
  it("refuses inventory to read-only tokens and revocation across workspaces", async () => {
    const { app, scope, base, create, origin } = await installation();
    const reader = await create("Read-only"),
      other = (
        await app.inject({
          method: "POST",
          url: "/api/v1/workspaces",
          payload: { name: "Foreign" },
        })
      ).json().id;
    const foreign = await create("Foreign token", false, other);
    expect(
      (
        await app.inject({
          url: base + "/tokens",
          headers: { authorization: "Bearer " + reader.token },
        })
      ).statusCode,
    ).toBe(403);
    expect(
      (
        await app.inject({
          method: "DELETE",
          url: base + "/tokens/" + foreign.id,
        })
      ).statusCode,
    ).toBe(404);
    expect(
      (
        await app.inject({
          url: `/api/v1/workspaces/${other}/tokens`,
          headers: { authorization: "Bearer " + reader.token },
        })
      ).statusCode,
    ).toBe(404);
  });
});
