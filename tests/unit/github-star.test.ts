import { afterEach, describe, expect, it, vi } from "vitest";
import {
  checkStar,
  type GitHubStarConfig,
  githubToken,
  starRepository,
} from "@/server/github-star";

// /github-star in a local build (src/server/github-star.ts): the star
// prompt asks whether the install's GitHub token has starred
// helixdb/helix-foundry, and has the site star it with that token. Only the
// site's own address answers, only its own pages may star, and without a
// token or an answer from GitHub the prompt links to the repository.

const ORIGIN = "http://localhost:3000";
const CONFIG: GitHubStarConfig = { origin: ORIGIN, token: "ghp_test" };
const STARRED = "https://api.github.com/user/starred/helixdb/helix-foundry";

/** A request to the site's /github-star, from its own page unless `headers` say otherwise. */
const request = (
  method: "GET" | "POST",
  headers: Record<string, string> = {},
  without?: string,
) => {
  const r = new Request(`${ORIGIN}/github-star`, {
    method,
    headers: { host: "localhost:3000", origin: ORIGIN, ...headers },
  });
  if (without) r.headers.delete(without);
  return r;
};

/** GitHub, answering `status` (or failing); what it was asked. */
const github = (status: number | "fail") => {
  const calls: { url: string; init: RequestInit }[] = [];
  const upstream = vi.fn(
    async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(url), init: init ?? {} });
      if (status === "fail") throw new TypeError("fetch failed");
      return new Response(null, { status });
    },
  ) as unknown as typeof fetch;
  return { upstream, calls };
};

describe("githubToken", () => {
  it.each<[Record<string, string>, string]>([
    [{ GH_TOKEN: " ghp_a ", GITHUB_TOKEN: "ghp_b" }, "ghp_a"],
    [{ GH_TOKEN: "", GITHUB_TOKEN: "ghp_b" }, "ghp_b"],
    [{ GH_TOKEN: "  ", GITHUB_TOKEN: " ghp_b " }, "ghp_b"],
    [{ GITHUB_TOKEN: "" }, ""],
    [{}, ""],
  ])("reads %o as %j", (env, token) => {
    expect(githubToken(env)).toBe(token);
  });
});

describe("checkStar", () => {
  it.each<[number | "fail", string]>([
    [204, "starred"],
    [404, "in_place"],
    // A token GitHub refuses, or one without star access.
    [401, "on_github"],
    [403, "on_github"],
    [500, "on_github"],
    ["fail", "on_github"],
  ])("reads GitHub's %s as %s, never cached", async (status, star) => {
    const { upstream, calls } = github(status);
    const res = await checkStar(request("GET"), CONFIG, upstream);
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({ star });
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(STARRED);
    expect(calls[0].init.method).toBe("GET");
    expect(calls[0].init.headers).toEqual({
      accept: "application/vnd.github+json",
      authorization: "Bearer ghp_test",
      "user-agent": "helix-foundry",
      "x-github-api-version": "2022-11-28",
    });
    expect(calls[0].init.signal).toBeInstanceOf(AbortSignal);
  });

  it("links to GitHub without asking it when the install has no token", async () => {
    const { upstream, calls } = github(204);
    const res = await checkStar(
      request("GET"),
      { ...CONFIG, token: "" },
      upstream,
    );
    expect(await res.json()).toEqual({ star: "on_github" });
    expect(calls).toEqual([]);
  });

  // A page whose DNS name was rebound to 127.0.0.1 arrives under its own Host.
  it("answers only the site's own address", async () => {
    const { upstream, calls } = github(204);
    for (const host of ["127.0.0.1:3000", "evil.example:3000"])
      expect(
        (await checkStar(request("GET", { host }), CONFIG, upstream)).status,
      ).toBe(403);
    expect(calls).toEqual([]);
  });
});

describe("starRepository", () => {
  it("stars with the install's token, saying GitHub did", async () => {
    const { upstream, calls } = github(204);
    const res = await starRepository(request("POST"), CONFIG, upstream);
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({ starred: true });
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(STARRED);
    expect(calls[0].init.method).toBe("PUT");
    expect(calls[0].init.body).toBeUndefined();
    expect(calls[0].init.headers).toMatchObject({
      authorization: "Bearer ghp_test",
    });
  });

  it.each<number | "fail">([401, 403, 404, 422, 500, "fail"])(
    "says it didn't when GitHub answers %s",
    async (status) => {
      const { upstream } = github(status);
      const res = await starRepository(request("POST"), CONFIG, upstream);
      expect(await res.json()).toEqual({ starred: false });
    },
  );

  it("says it didn't, without asking GitHub, when the install has no token", async () => {
    const { upstream, calls } = github(204);
    const res = await starRepository(
      request("POST"),
      { ...CONFIG, token: "" },
      upstream,
    );
    expect(await res.json()).toEqual({ starred: false });
    expect(calls).toEqual([]);
  });

  it.each<[string, Record<string, string>, string?]>([
    ["another host", { host: "127.0.0.1:3000" }],
    ["another site's page", { origin: "https://evil.example" }],
    ["a page on another port", { origin: "http://localhost:3001" }],
    ["no origin", {}, "origin"],
  ])("refuses a post from %s", async (_, headers, without) => {
    const { upstream, calls } = github(204);
    expect(
      (
        await starRepository(
          request("POST", headers, without),
          CONFIG,
          upstream,
        )
      ).status,
    ).toBe(403);
    expect(calls).toEqual([]);
  });
});

describe("/github-star", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  /** The route as `mode`'s build serves it, with the site's environment. */
  const route = async (
    mode: "" | "local",
    env: Record<string, string> = {},
  ) => {
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", mode);
    vi.stubEnv("HELIX_WFE_ORIGIN", "http://pod:8081");
    vi.stubEnv("FOUNDRY_ORIGIN", ORIGIN);
    vi.stubEnv("FOUNDRY_LOCAL_TOKEN", "t".repeat(32));
    vi.stubEnv("GH_TOKEN", "");
    vi.stubEnv("GITHUB_TOKEN", "");
    for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
    vi.resetModules();
    return import("@/app/github-star/route");
  };

  // The hosted site holds no GitHub token of the user's: its prompt links.
  it("is not in the hosted build", async () => {
    const { upstream, calls } = github(204);
    vi.stubGlobal("fetch", upstream);
    const { GET, POST } = await route("", { GH_TOKEN: "ghp_shell" });
    expect((await GET(request("GET"))).status).toBe(404);
    expect((await POST(request("POST"))).status).toBe(404);
    expect(calls).toEqual([]);
  });

  it("stars with the shell's token in a local build", async () => {
    const { upstream, calls } = github(204);
    vi.stubGlobal("fetch", upstream);
    const { GET, POST } = await route("local", { GITHUB_TOKEN: "ghp_shell" });
    expect(await (await GET(request("GET"))).json()).toEqual({
      star: "starred",
    });
    expect(await (await POST(request("POST"))).json()).toEqual({
      starred: true,
    });
    expect(calls.map((c) => c.init.headers)).toEqual([
      expect.objectContaining({ authorization: "Bearer ghp_shell" }),
      expect.objectContaining({ authorization: "Bearer ghp_shell" }),
    ]);
  });

  it("links without one", async () => {
    const { upstream, calls } = github(204);
    vi.stubGlobal("fetch", upstream);
    const { GET, POST } = await route("local");
    expect(await (await GET(request("GET"))).json()).toEqual({
      star: "on_github",
    });
    expect(await (await POST(request("POST"))).json()).toEqual({
      starred: false,
    });
    expect(calls).toEqual([]);
  });

  // localConfig's checks: a site that isn't on a loopback origin serves none.
  it("is not there in a local build set up unsafely", async () => {
    const { GET, POST } = await route("local", {
      FOUNDRY_ORIGIN: "http://foundry.example.com",
      GH_TOKEN: "ghp_shell",
    });
    expect((await GET(request("GET"))).status).toBe(404);
    expect((await POST(request("POST"))).status).toBe(404);
  });
});
