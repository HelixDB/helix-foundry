import { describe, expect, it, vi } from "vitest";
import { localConfig } from "@/server/config";
import { createCsrfToken, verifyCsrfToken } from "@/server/csrf";
import { handleLocalPapiRequest } from "@/server/local";

// /papi in a local build: only the configured loopback host, every request
// the owner's, forwarded with the shared token under the hosted site's checks.

const SITE = "http://localhost:3000";
const config = localConfig({
  HELIX_WFE_ORIGIN: "http://pod:8081",
  FOUNDRY_ORIGIN: SITE,
  FOUNDRY_LOCAL_TOKEN: "local-token-0123456789abcdefghijklmnop",
} as unknown as NodeJS.ProcessEnv)!;

function call(
  path: string,
  init: RequestInit & {
    host?: string;
    origin?: string | null;
    // What the server put in request.url: the standalone server uses the
    // address it listens on.
    listen?: string;
  } = {},
  reply: () => Response = () => Response.json({ ok: true }),
) {
  const { host = "localhost:3000", origin = SITE, listen, ...rest } = init;
  const headers = new Headers(rest.headers);
  headers.set("host", host);
  if (origin) headers.set("origin", origin);
  const upstream = vi.fn(async (_url: URL | RequestInfo, _init?: RequestInit) =>
    reply(),
  );
  const request = new Request(`http://${listen ?? host}/papi${path}`, {
    ...rest,
    headers,
  });
  return handleLocalPapiRequest(
    request,
    path.split("?")[0]!.slice(1).split("/"),
    config,
    upstream as unknown as typeof fetch,
  ).then((res) => ({ res, upstream }));
}

const csrfFor = (token: string) => ({
  "x-helix-csrf": token,
  cookie: `foundry-csrf=${token}`,
});

describe("local /papi", () => {
  it("forwards an allowlisted call with the shared token", async () => {
    const { res, upstream } = await call("/v1/whoami");
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });
    const [url, init] = upstream.mock.calls[0]!;
    expect(String(url)).toBe("http://pod:8081/v1/whoami");
    expect(new Headers(init!.headers).get("authorization")).toBe(
      `Bearer ${config.token}`,
    );
    // Not a JWT: no expiry is reported, and the SPA does not ask.
    expect(res.headers.get("x-foundry-session-expires-in")).toBeNull();
  });

  it.each([
    ["another name for this machine", "127.0.0.1:3000"],
    ["a rebound DNS name", "attacker.example:3000"],
    ["another port", "localhost:3001"],
  ])("refuses %s before anything else", async (_name, host) => {
    const { res, upstream } = await call("/v1/whoami", {
      host,
      origin: `http://${host}`,
    });
    expect(res.status).toBe(403);
    // It names the address it answers, for the SPA to show.
    expect(await res.json()).toMatchObject({
      code: "HOST_REJECTED",
      message: `Open Foundry at ${SITE}: it answers no other address.`,
    });
    expect(upstream).not.toHaveBeenCalled();
  });

  it("still refuses routes off the allowlist", async () => {
    const { res, upstream } = await call("/v1/auth/exchange", {
      method: "POST",
    });
    expect(res.status).toBe(404);
    expect(upstream).not.toHaveBeenCalled();
  });

  it("issues a CSRF token for the owner and requires it on unsafe calls", async () => {
    const issued = await call("/csrf");
    expect(issued.res.status).toBe(200);
    const { token } = await issued.res.json();
    expect(issued.res.headers.get("set-cookie")).toMatch(
      /^foundry-csrf=[^;]+; Path=\/; HttpOnly; SameSite=Lax; Max-Age=\d+$/,
    );
    expect(verifyCsrfToken(token, config.csrfSecret, "local")).toBe(true);

    const body = JSON.stringify({ slug: "acme" });
    const ok = await call("/v1/workspaces", {
      method: "POST",
      body,
      headers: { ...csrfFor(token), "content-type": "application/json" },
    });
    expect(ok.res.status).toBe(200);
    expect(ok.upstream.mock.calls[0]![1]!.method).toBe("POST");

    const missing = await call("/v1/workspaces", { method: "POST", body });
    expect(missing.res.status).toBe(403);
    expect(await missing.res.json()).toMatchObject({ code: "CSRF_REJECTED" });
    expect(missing.upstream).not.toHaveBeenCalled();

    // A token signed with another secret, e.g. the hosted site's.
    const forged = createCsrfToken("x".repeat(32), "local");
    const wrong = await call("/v1/workspaces", {
      method: "POST",
      body,
      headers: csrfFor(forged),
    });
    expect(wrong.res.status).toBe(403);
  });

  it("judges the origin by the Host, not the address it listens on", async () => {
    const issued = await call("/csrf", { listen: "0.0.0.0:3000" });
    const { token } = await issued.res.json();
    const { res, upstream } = await call("/v1/workspaces?page.size=200", {
      listen: "0.0.0.0:3000",
      method: "POST",
      body: JSON.stringify({ slug: "acme" }),
      headers: { ...csrfFor(token), "content-type": "application/json" },
    });
    expect(res.status).toBe(200);
    const [url, init] = upstream.mock.calls[0]!;
    expect(String(url)).toBe("http://pod:8081/v1/workspaces?page.size=200");
    expect(new TextDecoder().decode(init!.body as ArrayBuffer)).toBe(
      '{"slug":"acme"}',
    );
    // A rebound name is still refused, whatever the server listens on.
    const rebound = await call("/v1/whoami", {
      listen: "0.0.0.0:3000",
      host: "attacker.example:3000",
    });
    expect(rebound.res.status).toBe(403);
  });

  it("refuses an unsafe call from another origin", async () => {
    const { res, upstream } = await call("/v1/workspaces", {
      method: "POST",
      body: "{}",
      origin: "http://attacker.example",
    });
    expect(res.status).toBe(403);
    expect(await res.json()).toMatchObject({ code: "ORIGIN_REJECTED" });
    expect(upstream).not.toHaveBeenCalled();
  });

  it("passes on the API refusing the token, without retrying or signing out", async () => {
    const { res, upstream } = await call("/v1/whoami", {}, () =>
      Response.json(
        { code: 16, message: "unauthenticated" },
        { status: 401, headers: { "set-cookie": "x=1" } },
      ),
    );
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ message: "unauthenticated" });
    expect(upstream).toHaveBeenCalledTimes(1);
    expect(res.headers.get("set-cookie")).toBeNull();
  });
});
