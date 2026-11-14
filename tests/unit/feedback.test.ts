import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import {
  FEEDBACK_KINDS,
  MAX_FEEDBACK_BYTES,
  connectorRequestMessage,
  feedbackBytes,
  feedbackPage,
  firstLine,
  githubIssueUrl,
  sendable,
} from "@/web/feedback";
import type { MockReply, MockRequest } from "../../mock/http";
import { routes } from "../../mock/routes";
import { mockFeedback } from "../../mock/routes/feedback";
import { db, resetState } from "../../mock/state";

// Feedback (src/web/feedback.ts): what the Feedback button and Request a
// connector send, and the mock WFE's POST /v1/feedback, which keeps the
// WFE's rules so the UI meets the same refusals.

process.env.MOCK_ORIGIN = "http://127.0.0.1:4999";
process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI =
  "http://localhost:3999/login/callback";

describe("feedback's payload", () => {
  it("offers each kind once, each its own category", () => {
    const categories = FEEDBACK_KINDS.map((k) => k.category);
    expect(new Set(categories).size).toBe(categories.length);
    expect(FEEDBACK_KINDS.map((k) => k.kind)).toEqual([
      "bug",
      "idea",
      "confusing",
      "other",
    ]);
  });

  it("names the page by its path alone, cut to whole characters within 255 bytes", () => {
    expect(feedbackPage("/data/connections/conn_1")).toBe(
      "/data/connections/conn_1",
    );
    expect(feedbackPage("data")).toBe("/data");
    expect(feedbackPage("/a\u0000b\nc\u007f\u0085d")).toBe("/abcd");
    const long = feedbackPage("/" + "é".repeat(200));
    expect(feedbackBytes(long)).toBeLessThanOrEqual(255);
    expect(long).toBe("/" + "é".repeat(127));
    expect(feedbackPage("/" + "a".repeat(254))).toHaveLength(255);
  });

  it("names the connector on the first line, then what is wanted of it", () => {
    expect(connectorRequestMessage(" HubSpot ", "Deals and contacts")).toBe(
      "Connector: HubSpot\n\nDeals and contacts",
    );
    expect(connectorRequestMessage("Snowflake", "  ")).toBe(
      "Connector: Snowflake",
    );
  });

  it("takes a message of 1 to 4096 bytes that isn't blank", () => {
    expect(sendable("")).toBe(false);
    expect(sendable(" \n ")).toBe(false);
    expect(sendable("Broken")).toBe(true);
    expect(sendable("a".repeat(MAX_FEEDBACK_BYTES))).toBe(true);
    expect(sendable("a".repeat(MAX_FEEDBACK_BYTES + 1))).toBe(false);
    // Bytes, not characters: 2,049 two-byte characters are too many.
    expect(sendable("é".repeat(2049))).toBe(false);
  });

  it("titles by the first line, cut with an ellipsis", () => {
    expect(firstLine("  The picker hangs\nwhen I add Stripe")).toBe(
      "The picker hangs",
    );
    expect(firstLine("x".repeat(100), 10)).toBe("xxxxxxxxx…");
    expect(firstLine("short", 10)).toBe("short");
  });

  it("fills in GitHub's new-issue page for a local build", () => {
    const url = new URL(githubIssueUrl("Bug: it broke", "What\n& why"));
    expect(url.origin + url.pathname).toBe(
      "https://github.com/helixdb/helix-foundry/issues/new",
    );
    expect(url.searchParams.get("title")).toBe("Bug: it broke");
    expect(url.searchParams.get("body")).toBe("What\n& why");
  });
});

describe("the mock's POST /v1/feedback", () => {
  const send = (body: unknown, as = "usr_ada"): MockReply => {
    const route = routes.find(
      (r) =>
        r.method === "POST" &&
        matchTemplate(compileTemplate(r.path), "/v1/feedback"),
    )!;
    const req: MockRequest = {
      method: "POST",
      path: "/v1/feedback",
      params: {},
      query: new URLSearchParams(),
      body,
      principal: { user: db.users.get(as)!, sessionId: "s" },
    };
    return route.handle(req) as MockReply;
  };
  const valid = {
    category: "FEEDBACK_CATEGORY_BUG",
    message: "The picker closed on me",
    workspaceId: "ws_acme",
    page: "/data",
  };

  beforeEach(() => resetState());

  it("keeps what it took, with the page and workspace", () => {
    const reply = send(valid);
    expect(reply.status).toBe(200);
    expect((reply.body as { id: string }).id).toMatch(/^fb_/);
    expect(mockFeedback()).toMatchObject([
      {
        category: "FEEDBACK_CATEGORY_BUG",
        message: "The picker closed on me",
        workspaceId: "ws_acme",
        page: "/data",
        userId: "usr_ada",
      },
    ]);
    expect(send({ ...valid, workspaceId: "", page: "" }).status).toBe(200);
    expect(mockFeedback()[1]).toMatchObject({ workspaceId: null, page: "" });
  });

  it("takes a connector request", () => {
    expect(
      send({
        ...valid,
        category: "FEEDBACK_CATEGORY_CONNECTOR_REQUEST",
        message: "Connector: HubSpot",
      }).status,
    ).toBe(200);
  });

  it("refuses what the WFE refuses", () => {
    const refused = (body: unknown) => {
      const reply = send(body);
      expect(reply.status).toBe(400);
      return (reply.body as { message: string }).message;
    };
    expect(
      refused({ ...valid, category: "FEEDBACK_CATEGORY_UNSPECIFIED" }),
    ).toMatch(/^category is/);
    expect(refused({ ...valid, message: "  " })).toMatch(/^message is/);
    expect(refused({ ...valid, message: "a".repeat(4097) })).toMatch(
      /^message is/,
    );
    expect(refused({ ...valid, page: "data" })).toMatch(/^page is/);
    expect(refused({ ...valid, page: "/" + "a".repeat(255) })).toMatch(
      /^page is/,
    );
    expect(refused({ ...valid, page: "/a\nb" })).toMatch(/^page is/);
    expect(mockFeedback()).toHaveLength(0);
  });

  it("hides a workspace the caller isn't in", () => {
    expect(send({ ...valid, workspaceId: "ws_elsewhere" }).status).toBe(404);
  });

  it("takes 20 an hour from one person", () => {
    for (let n = 0; n < 20; n++) expect(send(valid).status).toBe(200);
    expect(send(valid).status).toBe(429);
    expect(mockFeedback()).toHaveLength(20);
  });
});

describe("sendFeedback", () => {
  const request = {
    category: "FEEDBACK_CATEGORY_OTHER" as const,
    message: "  Hello\n",
    workspaceId: "ws_acme",
    page: "/ask",
  };
  afterEach(() => {
    vi.doUnmock("@/web/api");
    vi.doUnmock("@/shared/deployment");
    vi.unstubAllGlobals();
    vi.resetModules();
  });
  const load = async (local: boolean) => {
    vi.resetModules();
    const api = vi.fn(async () => ({ id: "fb_1" }));
    vi.doMock("@/web/api", () => ({ api }));
    vi.doMock("@/shared/deployment", () => ({ LOCAL: local }));
    return { api, ...(await import("@/web/feedback")) };
  };

  it("posts to the WFE on the hosted site", async () => {
    const { api, sendFeedback } = await load(false);
    expect(await sendFeedback(request, "Other: Hello")).toBe("sent");
    expect(api).toHaveBeenCalledWith("/v1/feedback", request);
  });

  it("opens a filled-in GitHub issue in a local build, and sends nothing", async () => {
    const open = vi.fn();
    vi.stubGlobal("window", { open });
    const { api, sendFeedback } = await load(true);
    expect(await sendFeedback(request, "Other: Hello")).toBe("opened");
    expect(api).not.toHaveBeenCalled();
    const [url, target, features] = open.mock.calls[0]!;
    const issue = new URL(url);
    expect(issue.searchParams.get("title")).toBe("Other: Hello");
    expect(issue.searchParams.get("body")).toBe(
      "Hello\n\n---\nPage: /ask\nFoundry, local build",
    );
    expect([target, features]).toEqual(["_blank", "noopener,noreferrer"]);
  });
});
