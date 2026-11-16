import { QueryClient } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type {
  AutomationNode,
  AutomationSpec,
  FoundryAutomation,
} from "@/shared/wfe";
import { ApiError, api } from "@/web/api";
import { canonicalJson, parseSpec } from "@/web/areas/automations/model";
import { automationQueries, saveLayout } from "@/web/areas/automations/queries";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { db, resetState } from "../../mock/state";

// Steps moved on an automation's canvas save on their own (queries.ts
// saveLayout), through the mock's update route, which keeps the approval of
// an edit that only moves steps, as the WFE's does: onto the automation as
// saved, never a draft's other changes; changed elsewhere meanwhile, read
// again and placed once more.

vi.mock("@/web/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/web/api")>()),
  api: vi.fn(),
}));

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
/** A call to the mock as Ada, as api() makes it: an ApiError when refused. */
async function mock(path: string, body?: unknown, method?: string) {
  const m = (method ?? (body === undefined ? "GET" : "POST")) as HttpMethod;
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === m && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    const reply = (await r.handle({
      method: m,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get("usr_ada")!, sessionId: "s" },
    })) as MockReply & { body: any };
    if (reply.status >= 400)
      throw new ApiError(
        reply.body.message,
        reply.status,
        reply.body.code,
        reply.body.details,
      );
    return reply.body;
  }
  throw new Error(`no mock route for ${m} ${path}`);
}
const call = vi.mocked(api);

const A = "/v1/workspaces/ws_acme/foundry/automations";
const node = (id: string, kind: AutomationNode["kind"], config: unknown) =>
  ({ id, kind, config, position: { x: 0, y: 0 } }) as AutomationNode;
const spec: AutomationSpec = {
  v: 1,
  nodes: [
    node("start", "trigger", { kind: "manual" }),
    node("tell", "notify", { message: "Done" }),
  ],
  edges: [{ from: "start", to: "tell" }],
};
const placeOf = (a: FoundryAutomation, id: string) => {
  const read = parseSpec(a.specJson || "");
  return read.ok ? read.spec.nodes.find((n) => n.id === id)?.position : null;
};

/** An approved, live automation, read into `qc` as its page reads it. */
async function live(qc: QueryClient) {
  const made = (
    await mock(A, {
      name: "Weekly",
      specJson: canonicalJson(spec),
      clientToken: "t1",
    })
  ).automation as FoundryAutomation;
  const on = (
    await mock(`${A}/${made.id}:approve`, { expectedRevision: made.revision })
  ).automation as FoundryAutomation;
  qc.setQueryData(automationQueries.automation("ws_acme", on.id).queryKey, {
    automation: on,
  });
  return on;
}

beforeEach(() => {
  resetState();
  call.mockReset();
  call.mockImplementation(mock);
});

describe("saving where steps are", () => {
  it("moves only their places, keeping its approval, and caches what it saved", async () => {
    const qc = new QueryClient(),
      a = await live(qc);
    const done = await saveLayout(qc, "ws_acme", a.id, {
      tell: { x: 320.4, y: 10 },
      gone: { x: 5, y: 5 },
    });
    expect(done?.onto).toBe(a.revision);
    expect(done?.automation).toMatchObject({
      status: "active",
      specRevision: a.specRevision,
      approvedSpecRevision: a.approvedSpecRevision,
      revision: String(Number(a.revision) + 1),
    });
    expect(placeOf(done!.automation, "tell")).toEqual({ x: 320, y: 10 });
    expect(
      qc.getQueryData(automationQueries.automation("ws_acme", a.id).queryKey),
    ).toEqual({ automation: done!.automation });
    // One PATCH, at the revision its page had, with the spec as saved but
    // for the places.
    expect(call).toHaveBeenCalledTimes(1);
    expect(call.mock.calls[0]![2]).toBe("PATCH");
    expect(call.mock.calls[0]![1]).toEqual({
      expectedRevision: a.revision,
      specJson: canonicalJson({
        ...spec,
        nodes: [
          spec.nodes[0]!,
          { ...spec.nodes[1]!, position: { x: 320, y: 10 } },
        ],
      }),
    });
  });

  it("sends nothing when they are its places already", async () => {
    const qc = new QueryClient(),
      a = await live(qc);
    expect(
      await saveLayout(qc, "ws_acme", a.id, { tell: { x: 0, y: 0 } }),
    ).toBeUndefined();
    expect(call).not.toHaveBeenCalled();
  });

  it("reads it again once when it changed elsewhere, and places them on that", async () => {
    const qc = new QueryClient(),
      a = await live(qc);
    // Renamed in another tab: the page's revision is old.
    const renamed = (
      await mock(
        `${A}/${a.id}`,
        { expectedRevision: a.revision, name: "Other" },
        "PATCH",
      )
    ).automation as FoundryAutomation;
    call.mockClear();
    const done = await saveLayout(qc, "ws_acme", a.id, {
      tell: { x: 0, y: 400 },
    });
    expect(done?.onto).toBe(renamed.revision);
    expect(done?.automation).toMatchObject({ name: "Other", status: "active" });
    expect(placeOf(done!.automation, "tell")).toEqual({ x: 0, y: 400 });
    // The refused PATCH, the read, the PATCH that went through.
    expect(call.mock.calls.map((c) => c[2] ?? "GET")).toEqual([
      "PATCH",
      "GET",
      "PATCH",
    ]);
  });

  it("reads it first when its page hasn't, and gives up after a second refusal", async () => {
    const qc = new QueryClient(),
      a = await live(qc);
    qc.clear();
    const done = await saveLayout(qc, "ws_acme", a.id, {
      tell: { x: 7, y: 7 },
    });
    expect(call.mock.calls[0]![2]).toBeUndefined();
    expect(placeOf(done!.automation, "tell")).toEqual({ x: 7, y: 7 });
    // Changed again between every read and its PATCH: refused twice.
    call.mockImplementation(async (path, body, method) => {
      if (method === "PATCH")
        throw new ApiError("changed", 400, 9, [
          { reason: "FOUNDRY_AUTOMATION_CHANGED" },
        ]);
      return mock(path, body, method);
    });
    await expect(
      saveLayout(qc, "ws_acme", a.id, { tell: { x: 9, y: 9 } }),
    ).rejects.toThrow("changed");
    // Any other refusal isn't retried.
    call.mockReset();
    call.mockRejectedValueOnce(new ApiError("gone", 404));
    await expect(
      saveLayout(qc, "ws_acme", a.id, { tell: { x: 1, y: 1 } }),
    ).rejects.toThrow("gone");
    expect(call).toHaveBeenCalledTimes(1);
  });
});
