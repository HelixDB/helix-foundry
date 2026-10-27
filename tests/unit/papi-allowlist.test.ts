import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  allowedMethods,
  findPapiRoute,
  papiRoutes,
} from "@/server/papi-allowlist";
import { routes as mockRoutes } from "../../mock/routes";

type Paths = Record<string, Record<string, unknown>>;
const read = (name: string) =>
  (
    JSON.parse(
      readFileSync(new URL(`../../contract/${name}`, import.meta.url), "utf8"),
    ) as { paths: Paths }
  ).paths;
// Foundry's own routes are FoundryService's; the rest are the WFE's. A path
// is in one contract or the other, never both.
const foundry = read("foundry.swagger.json");
const wfe = read("wfe.swagger.json");
const swagger = { paths: { ...wfe, ...foundry } as Paths };

describe("papi allowlist", () => {
  it.each(papiRoutes.map((r) => [r.method, r.path] as const))(
    "%s %s is a WFE or Foundry route",
    (method, path) => {
      expect(swagger.paths[path], path).toBeDefined();
      expect(Object.keys(swagger.paths[path]!)).toContain(method.toLowerCase());
    },
  );

  it("finds each path in one contract", () => {
    const both = Object.keys(foundry).filter((p) => p in wfe);
    expect(both).toEqual([]);
    for (const p of Object.keys(foundry))
      expect(p, p).toMatch(
        /^\/v1\/(foundry\/|foundry$|workspaces\/\{workspaceId\}\/foundry(\/|$))/,
      );
  });

  // scripts/trim-wfe-contract.mjs keeps the rest of hosted Helix's API out.
  it("vendors only the WFE routes and methods the site calls", () => {
    const allowed = new Set(
      [...papiRoutes, ...mockRoutes].map((r) => `${r.method} ${r.path}`),
    );
    for (const [path, methods] of Object.entries(wfe))
      for (const m of Object.keys(methods))
        expect(allowed, `${m} ${path}`).toContain(`${m.toUpperCase()} ${path}`);
  });

  it("lists each route once", () => {
    const keys = papiRoutes.map((r) => `${r.method} ${r.path}`);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("never lets the browser reach sign-in, SSO, roles or member removal", () => {
    for (const r of papiRoutes) {
      expect(r.path).not.toMatch(/^\/v1\/auth\//);
      expect(r.path).not.toMatch(/\/sso/);
      expect(r.path).not.toMatch(/\/service-credentials|\/report-keys/);
      expect(`${r.method} ${r.path}`).not.toMatch(
        /^(PATCH|DELETE) .*\/members\//,
      );
    }
  });

  it("matches one segment per parameter and nothing else", () => {
    expect(findPapiRoute("GET", "/v1/whoami")?.path).toBe("/v1/whoami");
    expect(findPapiRoute("get", "/v1/workspaces/ws_1")?.path).toBe(
      "/v1/workspaces/{id}",
    );
    for (const path of [
      "/v1/whoami/",
      "/v1/whoamix",
      "/v1/workspaces/ws_1/members/u_1",
      "/v1/workspaces/a/b",
      "/v1/workspaces/..",
      "/v1/workspaces/.",
      "/v1/workspaces/ws_1:delete",
      "/v1/clusters",
      "/v1/databases:query-write",
      "/v1/auth/login:start",
      "/api/workspaces",
    ])
      expect(findPapiRoute("GET", path), path).toBeUndefined();
    expect(findPapiRoute("DELETE", "/v1/workspaces/ws_1")).toBeUndefined();
  });

  it("names the methods a path allows", () => {
    expect(allowedMethods("/v1/workspaces")).toEqual(["GET", "POST"]);
    expect(allowedMethods("/v1/workspaces/ws_1")).toEqual(["GET"]);
    expect(allowedMethods("/v1/clusters")).toEqual([]);
  });

  it("is served by the mock, route for route", () => {
    const mock = new Set(mockRoutes.map((r) => `${r.method} ${r.path}`));
    for (const r of papiRoutes)
      expect(mock.has(`${r.method} ${r.path}`), `${r.method} ${r.path}`).toBe(
        true,
      );
  });

  it("mocks only WFE and Foundry routes", () => {
    for (const r of mockRoutes)
      expect(
        Object.keys(swagger.paths[r.path] || {}),
        `${r.method} ${r.path}`,
      ).toContain(r.method.toLowerCase());
  });
});
