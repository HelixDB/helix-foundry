import { afterEach, describe, expect, it, vi } from "vitest";
import type { WhoamiResponse } from "@/shared/wfe";
import {
  createWorkspace,
  shellUser,
  shellWorkspaces,
  slugify,
  workspacesQuery,
} from "@/web/workspaces";

afterEach(() => vi.unstubAllGlobals());

describe("workspaces", () => {
  it.each([
    ["Acme", "acme"],
    ["Acme Inc.", "acme-inc"],
    ["  Café   Ünïcode  ", "cafe-unicode"],
    ["--x--", "x"],
    ["日本", ""],
    ["a".repeat(80), "a".repeat(64)],
    ["a".repeat(63) + " b", "a".repeat(63)],
  ])("slugify(%j) is %j", (name, slug) => {
    expect(slugify(name)).toBe(slug);
    if (slug) expect(slug).toMatch(/^[a-z0-9][a-z0-9_-]{0,63}$/);
  });

  const me: WhoamiResponse = {
    user: { id: "u1", email: "ada@example.com", displayName: "Ada" },
    workspaces: [
      { workspaceId: "w1", role: "WORKSPACE_ROLE_OWNER" },
      { workspaceId: "w2", role: "WORKSPACE_ROLE_ADMIN" },
      { workspaceId: "w3" },
    ],
  };

  it("joins memberships with workspace names", () => {
    expect(
      shellWorkspaces(me, [
        { id: "w1", slug: "acme", displayName: "Acme" },
        { id: "w2", slug: "beta" },
        { id: "w3" },
        { id: "w4", slug: "gone", status: "RESOURCE_STATUS_DELETED" },
      ]),
    ).toEqual([
      { id: "w1", slug: "acme", name: "Acme", role: "owner" },
      { id: "w2", slug: "beta", name: "beta", role: "admin" },
      { id: "w3", slug: "w3", name: "w3", role: "member" },
    ]);
  });

  it("names the user", () => {
    expect(shellUser(me)).toEqual({
      id: "u1",
      email: "ada@example.com",
      name: "Ada",
    });
    expect(shellUser({ user: { id: "u", email: "e@x.test" } }).name).toBe(
      "e@x.test",
    );
  });

  it("reads every page of the user's workspaces", async () => {
    const urls: string[] = [];
    const pages = [
      { workspaces: [{ id: "w1" }, { id: "w2" }], nextPageToken: "t2" },
      { workspaces: [{ id: "w3" }], nextPageToken: "t3" },
      { workspaces: [] },
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        urls.push(url);
        return Response.json(pages.shift());
      }),
    );
    expect(await workspacesQuery.queryFn()).toEqual({
      workspaces: [{ id: "w1" }, { id: "w2" }, { id: "w3" }],
    });
    expect(urls).toEqual([
      "/papi/v1/workspaces?page.size=200",
      "/papi/v1/workspaces?page.size=200&page.token=t2",
      "/papi/v1/workspaces?page.size=200&page.token=t3",
    ]);
  });

  describe("createWorkspace", () => {
    // The WFE's answers in order; the slugs it was sent.
    const replies = (...answers: Response[]) => {
      const slugs: string[] = [];
      vi.stubGlobal(
        "fetch",
        vi.fn(async (url: string, init?: RequestInit) => {
          if (url === "/papi/csrf")
            return Response.json({ token: "t", expiresInSeconds: 3600 });
          slugs.push(JSON.parse(String(init?.body)).slug);
          return answers.shift()!;
        }),
      );
      return slugs;
    };
    const refusal = (status: number, code: number, message: string) =>
      Response.json({ code, message, details: [] }, { status });

    it("sends the name's slug", async () => {
      const slugs = replies(Response.json({ id: "ws_1", slug: "acme-inc" }));
      expect(await createWorkspace(" Acme Inc. ")).toMatchObject({
        id: "ws_1",
      });
      expect(slugs).toEqual(["acme-inc"]);
    });

    it.each([
      [refusal(409, 6, 'workspace "acme" already exists'), "Acme"],
      [refusal(400, 3, 'slug: "personal" is reserved'), "Personal"],
    ])("adds a suffix once when the slug is unusable", async (first, name) => {
      const slugs = replies(first, Response.json({ id: "ws_2" }));
      expect(await createWorkspace(name)).toMatchObject({ id: "ws_2" });
      expect(slugs).toHaveLength(2);
      expect(slugs[1]).toMatch(new RegExp(`^${slugify(name)}-[a-z0-9]{4}$`));
    });

    it("keeps a long slug within 64 characters", async () => {
      const slugs = replies(
        refusal(409, 6, "already exists"),
        Response.json({ id: "ws_3" }),
      );
      await createWorkspace("a".repeat(58) + "-b" + "c".repeat(10));
      // Cut at 59, where a hyphen would end it: dropped, not doubled.
      expect(slugs[1]).toMatch(/^a{58}-[a-z0-9]{4}$/);
      expect(slugs[1]!.length).toBeLessThanOrEqual(64);
    });

    it("gives up after one retry, and on any other refusal", async () => {
      const taken = refusal(409, 6, "already exists");
      replies(taken, refusal(409, 6, "already exists"));
      await expect(createWorkspace("Acme")).rejects.toMatchObject({
        status: 409,
      });
      const slugs = replies(
        refusal(400, 3, 'region "us-east-2" is not enabled'),
      );
      await expect(createWorkspace("Acme")).rejects.toMatchObject({
        message: 'region "us-east-2" is not enabled',
      });
      expect(slugs).toEqual(["acme"]);
    });
  });
});
