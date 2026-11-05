import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The Data area's WFE calls: the paths they reach through /papi, their
// methods, and when their queries poll.

type Call = { url: string; method: string; body?: unknown };
let calls: Call[];

beforeEach(() => {
  calls = [];
  vi.resetModules();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (url === "/papi/csrf")
        return Response.json({ token: "t", expiresInSeconds: 3600 });
      calls.push({
        url,
        method: init.method || "GET",
        body: init.body ? JSON.parse(String(init.body)) : undefined,
      });
      // A sync is answered with the sync it queued, as the WFE does.
      return Response.json(
        url.endsWith(":sync") ? { sync: { id: "sync_1" } } : { ok: true },
      );
    }),
  );
});
afterEach(() => vi.unstubAllGlobals());

const load = () => import("@/web/areas/data/data-api");
const F = "/papi/v1/workspaces/ws_1/foundry";

describe("data api", () => {
  it("reads kinds, connectors, one connector, its syncs and one sync", async () => {
    const d = await load();
    await d.connectorKindsQuery("ws_1").queryFn();
    await d.connectorsQuery("ws_1").queryFn();
    await d.connectorQuery("ws_1", "c 1").queryFn();
    await d.connectorSyncsQuery("ws_1", "c1").queryFn();
    await d.connectorSyncQuery("ws_1", "s1").queryFn();
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      `GET ${F}/connector-kinds`,
      `GET ${F}/connectors`,
      `GET ${F}/connectors/c%201`,
      `GET ${F}/connectors/c1/syncs?limit=50`,
      `GET ${F}/connector-syncs/s1`,
    ]);
  });

  it("tests, creates, updates, syncs and deletes", async () => {
    const d = await load();
    const config = { stripe: { apiKey: "sk_test_1" } };
    await d.testConnector("ws_1", config);
    await d.createConnector("ws_1", { name: "S", config });
    await d.updateConnector("ws_1", "c1", { name: "T" });
    await d.syncConnector("ws_1", "c1", "connection");
    await d.deleteConnector("ws_1", "c1");
    expect(calls).toEqual([
      { method: "POST", url: `${F}/connectors:test`, body: { config } },
      { method: "POST", url: `${F}/connectors`, body: { name: "S", config } },
      { method: "PATCH", url: `${F}/connectors/c1`, body: { name: "T" } },
      { method: "POST", url: `${F}/connectors/c1:sync`, body: {} },
      { method: "DELETE", url: `${F}/connectors/c1`, body: undefined },
    ]);
  });

  it("starts, reads, authorizes and finishes a sign-in at the provider", async () => {
    const d = await load();
    const config = { github: { repos: ["acme/api"], reviewDays: 30 } };
    await d.startSignIn("ws_1", {
      kind: "github",
      name: "GitHub",
      config,
      clientToken: "t1",
    });
    await d.getSignIn("ws_1", "csi 1");
    await d.signInQuery("ws_1", "csi_1").queryFn();
    await d.authorizeSignIn(
      "ws_1",
      "csi_1",
      "https://foundry.helix-db.com/connect/callback",
    );
    await d.finishSignIn("ws_1", "csi_1", { connectCode: "abc" });
    await d.finishSignIn("ws_1", "csi_1", { error: "access_denied" });
    expect(calls).toEqual([
      {
        method: "POST",
        url: `${F}/sign-ins`,
        body: { kind: "github", name: "GitHub", config, clientToken: "t1" },
      },
      { method: "GET", url: `${F}/sign-ins/csi%201`, body: undefined },
      { method: "GET", url: `${F}/sign-ins/csi_1`, body: undefined },
      {
        method: "POST",
        url: `${F}/sign-ins/csi_1:authorize`,
        body: { returnTo: "https://foundry.helix-db.com/connect/callback" },
      },
      {
        method: "POST",
        url: `${F}/sign-ins/csi_1:finish`,
        body: { connectCode: "abc" },
      },
      {
        method: "POST",
        url: `${F}/sign-ins/csi_1:finish`,
        body: { error: "access_denied" },
      },
    ]);
  });

  it("reads only the connectors that need signing in again, for the shell", async () => {
    const d = await load();
    const q = d.needsSignInQuery("ws_1");
    await q.queryFn();
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      `GET ${F}/connectors?needsSignIn=true`,
    ]);
    // Under the connectors' key: whatever refreshes them refreshes it.
    expect(q.queryKey.slice(0, 2)).toEqual(["ws_1", "connectors"]);
    expect(q.staleTime).toBeGreaterThanOrEqual(60_000);
  });

  it("sets and clears an API source's text field, sending it alone", async () => {
    const d = await load();
    await d.setEventTextField("ws_1", "c 1", "reply");
    await d.setEventTextField("ws_1", "c 1", "");
    // No name and no description: the WFE leaves both as they are.
    expect(calls).toEqual([
      {
        method: "PATCH",
        url: `${F}/connectors/c%201`,
        body: { config: { events: { textField: "reply" } } },
      },
      {
        method: "PATCH",
        url: `${F}/connectors/c%201`,
        body: { config: { events: { textField: "" } } },
      },
    ]);
  });

  it("polls the list while any connector syncs, and a sync until it ends", async () => {
    const d = await load();
    const list = d.connectorsQuery("ws_1").refetchInterval,
      one = d.connectorSyncQuery("ws_1", "s1").refetchInterval;
    const state = <T>(data?: T) => ({ state: { data } });
    expect(list(state())).toBe(false);
    expect(list(state({ connectors: [{ id: "a", kind: "stripe" }] }))).toBe(
      false,
    );
    expect(
      list(
        state({
          connectors: [
            {
              id: "a",
              kind: "stripe",
              lastSync: { id: "s", status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" },
            },
            {
              id: "b",
              kind: "mysql",
              lastSync: { id: "t", status: "CONNECTOR_SYNC_STATUS_RUNNING" },
            },
          ],
        }),
      ),
    ).toBe(d.SYNC_POLL_MS);
    expect(one(state())).toBe(d.SYNC_POLL_MS);
    expect(
      one(state({ sync: { id: "s", status: "CONNECTOR_SYNC_STATUS_QUEUED" } })),
    ).toBe(d.SYNC_POLL_MS);
    expect(
      one(state({ sync: { id: "s", status: "CONNECTOR_SYNC_STATUS_FAILED" } })),
    ).toBe(false);
  });

  it("makes, lists and revokes an API source's send tokens, and reads its sends", async () => {
    const d = await load();
    const source = { id: "c 1", kind: "events" };
    await d.eventTokensQuery("ws_1", "c 1").queryFn();
    await d.createEventToken("ws_1", "c 1");
    await d.revokeEventToken("ws_1", "c 1", "t/1");
    await d.eventBatchesQuery("ws_1", source).queryFn();
    await d.eventBatchesQuery("ws_1", source, 100).queryFn();
    expect(calls).toEqual([
      { method: "GET", url: `${F}/connectors/c%201/tokens`, body: undefined },
      { method: "POST", url: `${F}/connectors/c%201/tokens`, body: {} },
      {
        method: "POST",
        url: `${F}/connectors/c%201/tokens/t%2F1:revoke`,
        body: {},
      },
      {
        method: "GET",
        url: `${F}/connectors/c%201/event-batches?limit=20`,
        body: undefined,
      },
      {
        method: "GET",
        url: `${F}/connectors/c%201/event-batches?limit=100`,
        body: undefined,
      },
    ]);
    // Under the connector, so its invalidation refreshes them.
    expect(d.eventTokensQuery("ws_1", "c1").queryKey).toEqual([
      "ws_1",
      "connectors",
      "c1",
      "tokens",
    ]);
    expect(d.eventBatchesQuery("ws_1", source, 5).queryKey).toEqual([
      "ws_1",
      "connectors",
      "c 1",
      "event-batches",
      5,
    ]);
  });

  it("polls an API source's sends, and its connectors when asked, while it waits", async () => {
    const d = await load();
    const state = <T>(data?: T) => ({ state: { data } });
    const empty = { id: "c1", kind: "events" },
      synced = {
        ...empty,
        tables: [{ name: "events", rowCount: "3" }],
      };
    const sends = (s: object) =>
      d.eventBatchesQuery("ws_1", s as never).refetchInterval;
    // No rows: waiting, whatever was sent.
    expect(sends(empty)(state())).toBe(d.EVENT_POLL_MS);
    expect(sends(empty)(state({ batches: [], pendingPieces: "0" }))).toBe(
      d.EVENT_POLL_MS,
    );
    // Rows: waiting only while pieces are pending.
    expect(sends(synced)(state())).toBe(false);
    expect(sends(synced)(state({ pendingPieces: "0" }))).toBe(false);
    expect(sends(synced)(state({ pendingPieces: "4" }))).toBe(d.EVENT_POLL_MS);

    const list = (waiting?: boolean) =>
      d.connectorsQuery("ws_1", waiting).refetchInterval;
    expect(list()(state({ connectors: [synced] }))).toBe(false);
    expect(list(true)(state({ connectors: [synced] }))).toBe(d.EVENT_POLL_MS);
    expect(list(true)(state())).toBe(d.EVENT_POLL_MS);
    // A live sync polls faster.
    expect(
      list(true)(
        state({
          connectors: [
            {
              ...synced,
              lastSync: { id: "s", status: "CONNECTOR_SYNC_STATUS_RUNNING" },
            },
          ],
        }),
      ),
    ).toBe(d.SYNC_POLL_MS);
    expect(d.connectorQuery("ws_1", "c1").refetchInterval).toBe(false);
    expect(d.connectorQuery("ws_1", "c1", true).refetchInterval).toBe(
      d.EVENT_POLL_MS,
    );
  });
});
