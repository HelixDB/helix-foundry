import { beforeEach, describe, expect, it } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { DataLayoutResponse } from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import {
  MAX_DATA_LAYOUT_BYTES,
  dataLayoutRoutes,
} from "../../mock/routes/data-layout";
import { config, resetState } from "../../mock/state";

// The mock's data layout behaves as the WFE's GetFoundryDataLayout and
// PutFoundryDataLayout do: opaque JSON with a revision, a stale revision
// refused (a racing first write included), and writes for owners and admins.

const compiled = dataLayoutRoutes.map((r) => ({
  ...r,
  c: compileTemplate(r.path),
}));
const PATH = "/v1/workspaces/ws_acme/foundry/data-layout";

function call(
  method: HttpMethod,
  body?: unknown,
  userId = "usr_ada",
  path = PATH,
): MockReply {
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, path);
    if (!params) continue;
    const reply = r.handle({
      method,
      path,
      params,
      query: new URLSearchParams(),
      body,
      principal: {
        user: {
          id: userId,
          workosId: "w",
          email: "e",
          firstName: "",
          lastName: "",
          createdAt: "",
        },
        sessionId: "s",
      },
    });
    if (reply instanceof Promise) throw new Error("async mock route");
    return reply;
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const body = (r: MockReply) => r.body as DataLayoutResponse;
const code = (r: MockReply) => (r.body as { code: number }).code;

beforeEach(() => resetState());

describe("the data layout mock", () => {
  it("reads {} at revision 0 before the first write", () => {
    expect(body(call("GET"))).toEqual({ layout: {}, revision: "0" });
  });

  it("stores a layout as it is and counts writes", () => {
    const first = call("PUT", {
      layout: { v: 1, x: [1] },
      expectedRevision: "0",
    });
    expect(body(first)).toEqual({ layout: { v: 1, x: [1] }, revision: "1" });
    const second = call("PUT", { layout: { v: 1 }, expectedRevision: "1" });
    expect(body(second).revision).toBe("2");
    expect(body(call("GET"))).toEqual({ layout: { v: 1 }, revision: "2" });
  });

  it("refuses a stale revision, a racing first write included", () => {
    call("PUT", { layout: { a: 1 }, expectedRevision: "0" });
    const race = call("PUT", { layout: { b: 1 }, expectedRevision: "0" });
    expect([race.status, code(race)]).toEqual([400, 9]);
    const ahead = call("PUT", { layout: { b: 1 }, expectedRevision: "5" });
    expect(code(ahead)).toBe(9);
    expect(body(call("GET")).layout).toEqual({ a: 1 });
  });

  it("lets another writer come first on the next n writes", () => {
    Object.assign(config, { dataLayoutConflicts: 2 });
    const first = call("PUT", { layout: { a: 1 }, expectedRevision: "0" });
    expect(code(first)).toBe(9);
    // The other write moved the revision on and left the layout.
    expect(body(call("GET"))).toEqual({ layout: {}, revision: "1" });
    expect(code(call("PUT", { layout: { a: 1 }, expectedRevision: "1" }))).toBe(
      9,
    );
    expect(
      body(call("PUT", { layout: { a: 1 }, expectedRevision: "2" })).revision,
    ).toBe("3");
    resetState();
    expect(config).not.toHaveProperty("dataLayoutConflicts");
    expect(body(call("GET")).revision).toBe("0");
  });

  it("needs an object of at most 64 KiB of compact JSON", () => {
    for (const layout of [undefined, null, [], "x"])
      expect(code(call("PUT", { layout, expectedRevision: "0" }))).toBe(3);
    expect(code(call("PUT", { layout: {}, expectedRevision: "-1" }))).toBe(3);
    // 64 KiB of compact JSON, counted in UTF-8 bytes, fits; a byte more doesn't.
    const at = (bytes: number) => {
      const base = JSON.stringify({ items: ["é"], pad: "" }).length + 1;
      return { items: ["é"], pad: "x".repeat(bytes - base) };
    };
    const full = at(MAX_DATA_LAYOUT_BYTES);
    expect(new TextEncoder().encode(JSON.stringify(full)).length).toBe(
      MAX_DATA_LAYOUT_BYTES,
    );
    expect(
      code(
        call("PUT", {
          layout: at(MAX_DATA_LAYOUT_BYTES + 1),
          expectedRevision: "0",
        }),
      ),
    ).toBe(3);
    expect(
      body(call("PUT", { layout: full, expectedRevision: "0" })).revision,
    ).toBe("1");
  });

  it("lets members read and only owners and admins write", () => {
    expect(call("GET", undefined, "usr_alan").status).toBe(200);
    const member = call(
      "PUT",
      { layout: {}, expectedRevision: "0" },
      "usr_alan",
    );
    expect(code(member)).toBe(7);
    const outsider = call(
      "GET",
      undefined,
      "usr_ada",
      "/v1/workspaces/ws_nope/foundry/data-layout",
    );
    expect(code(outsider)).toBe(7);
  });
});
