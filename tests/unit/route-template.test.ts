import { describe, expect, it } from "vitest";
import { compileTemplate, matchTemplate } from "@/shared/route-template";

describe("route templates", () => {
  it("captures each parameter from one segment", () => {
    const t = compileTemplate(
      "/v1/workspaces/{workspaceId}/foundry/connectors/{id}:sync",
    );
    expect(
      matchTemplate(t, "/v1/workspaces/ws_1/foundry/connectors/c.2:sync"),
    ).toEqual({ workspaceId: "ws_1", id: "c.2" });
    expect(
      matchTemplate(t, "/v1/workspaces/ws_1/foundry/connectors/c2"),
    ).toBeNull();
    expect(
      matchTemplate(t, "/v1/workspaces/ws_1/foundry/connectors/c2:test"),
    ).toBeNull();
    expect(
      matchTemplate(t, "/v1/workspaces/a/b/foundry/connectors/c:sync"),
    ).toBeNull();
  });

  it("treats a verb and dots as literal text", () => {
    const t = compileTemplate("/v1/databases:query-read");
    expect(t.regex.test("/v1/databases:query-read")).toBe(true);
    expect(t.regex.test("/v1/databases:query-readx")).toBe(false);
    expect(t.regex.test("/v1/databasesXquery-read")).toBe(false);
  });

  it("refuses a parameter that would span a verb or a slash", () => {
    const t = compileTemplate("/v1/workspaces/{id}");
    expect(t.regex.test("/v1/workspaces/ws:1")).toBe(false);
    expect(t.regex.test("/v1/workspaces/")).toBe(false);
    expect(t.regex.test("/v1/workspaces/%2F")).toBe(false);
  });

  it("refuses dot segments as a parameter", () => {
    const t = compileTemplate("/v1/workspaces/{id}");
    expect(matchTemplate(t, "/v1/workspaces/..")).toBeNull();
    expect(matchTemplate(t, "/v1/workspaces/.")).toBeNull();
    expect(matchTemplate(t, "/v1/workspaces/...")).toEqual({ id: "..." });
    expect(matchTemplate(t, "/v1/workspaces/.a")).toEqual({ id: ".a" });
    const verb = compileTemplate("/v1/workspaces/{id}:sync");
    expect(matchTemplate(verb, "/v1/workspaces/..:sync")).toBeNull();
    const nested = compileTemplate("/v1/workspaces/{workspaceId}/members");
    expect(matchTemplate(nested, "/v1/workspaces/../members")).toBeNull();
  });

  it.each([
    "/v1/*",
    "/v1/workspaces/*",
    "/v1/workspaces/**",
    "/v2/whoami",
    "/v1/",
    "/v1/workspaces/{id",
    "/v1/workspaces/(.*)",
  ])("refuses %s", (template) => {
    expect(() => compileTemplate(template as `/v1/${string}`)).toThrow();
  });
});
