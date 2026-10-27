import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type * as wfe from "@/shared/wfe";
import {
  USAGE_WARNING,
  usageQuery,
  usageView,
} from "@/web/areas/workspace/usage";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { config, db, resetState } from "../../mock/state";

// The account menu's Usage section: the month's AI spend by task against
// each cap, read from the WFE's GetFoundryUsage, and the mock's route.

const entry = (
  task: wfe.FoundryUsageTask,
  spent: number,
  capMicros?: number,
  reserved = 0,
): wfe.FoundryUsageEntry => ({
  task,
  spentUsdMicros: String(spent),
  reservedUsdMicros: String(reserved),
  ...(capMicros === undefined ? {} : { capUsdMicros: String(capMicros) }),
});

describe("usageView", () => {
  it("shows every task's spend of its cap, API sources within loads", () => {
    expect(
      usageView({
        monthStart: "2026-10-01T00:00:00Z",
        resetsAt: "2026-11-01T00:00:00Z",
        totalUsdMicros: "17200000",
        tasks: [
          entry("FOUNDRY_USAGE_TASK_LOADS", 12_400_000, 50_000_000),
          entry("FOUNDRY_USAGE_TASK_API_SOURCES", 1_200_000, 20_000_000),
          entry("FOUNDRY_USAGE_TASK_QUESTIONS", 4_800_000, 15_000_000),
        ],
      }),
    ).toEqual({
      total: "$17.20",
      resets: "November 1",
      rows: [
        {
          task: "FOUNDRY_USAGE_TASK_LOADS",
          label: "Loads",
          hint: "Reading documents into your ontologies",
          within: false,
          spent: "$12.40",
          cap: { of: "of $50.00", fill: 0.248, level: "room" },
        },
        {
          task: "FOUNDRY_USAGE_TASK_API_SOURCES",
          label: "API sources",
          hint: "Part of Loads: reading API sources’ pieces no one asked for",
          within: true,
          spent: "$1.20",
          cap: { of: "of $20.00", fill: 0.06, level: "room" },
        },
        {
          task: "FOUNDRY_USAGE_TASK_QUESTIONS",
          label: "Questions",
          hint: "Answering questions in Ask",
          within: false,
          spent: "$4.80",
          cap: { of: "of $15.00", fill: 0.32, level: "room" },
        },
      ],
    });
  });

  it("fills a question's bar with what running ones hold, and says how much", () => {
    const [row] = usageView({
      tasks: [
        entry("FOUNDRY_USAGE_TASK_QUESTIONS", 250_000, 15_000_000, 500_000),
      ],
    }).rows;
    expect(row).toMatchObject({
      spent: "$0.25",
      held: "$0.50",
      cap: { of: "of $15.00", fill: 0.05, level: "room" },
    });
  });

  it("warns from 80% of a cap and is full once spent, never past it", () => {
    const at = (spent: number, reserved = 0) =>
      usageView({
        tasks: [entry("FOUNDRY_USAGE_TASK_LOADS", spent, 10_000_000, reserved)],
      }).rows[0]!.cap;
    expect(USAGE_WARNING).toBe(0.8);
    expect(at(7_999_999)?.level).toBe("room");
    expect(at(8_000_000)).toEqual({
      of: "of $10.00",
      fill: 0.8,
      level: "warn",
    });
    expect(at(6_000_000, 2_000_000)?.level).toBe("warn");
    expect(at(10_000_000)).toEqual({ of: "of $10.00", fill: 1, level: "full" });
    expect(at(12_000_000)?.fill).toBe(1);
  });

  it("leaves the bar out for a task without a cap", () => {
    for (const capUsdMicros of [undefined, null, "0", "nope"]) {
      const [row] = usageView({
        tasks: [
          {
            task: "FOUNDRY_USAGE_TASK_LOADS",
            spentUsdMicros: "2000000",
            capUsdMicros,
          },
        ],
      }).rows;
      expect(row, String(capUsdMicros)).toEqual({
        task: "FOUNDRY_USAGE_TASK_LOADS",
        label: "Loads",
        hint: "Reading documents into your ontologies",
        within: false,
        spent: "$2.00",
      });
    }
  });

  it("reads an empty month, and fields at their defaults, as nothing spent", () => {
    const view = usageView({
      tasks: [
        { task: "FOUNDRY_USAGE_TASK_LOADS", capUsdMicros: "50000000" },
        { task: "FOUNDRY_USAGE_TASK_QUESTIONS", spentUsdMicros: "-5" },
      ],
    });
    expect(view.total).toBe("$0");
    expect(view.resets).toBe("");
    expect(view.rows.map((r) => [r.spent, r.cap?.fill, r.held])).toEqual([
      ["$0", 0, undefined],
      ["$0", undefined, undefined],
    ]);
  });

  it("keeps the WFE's tasks it knows, in its own order", () => {
    const view = usageView({
      resetsAt: "not a date",
      tasks: [
        entry("FOUNDRY_USAGE_TASK_QUESTIONS", 1, 15_000_000),
        entry("FOUNDRY_USAGE_TASK_UNSPECIFIED", 1, 1),
        entry("FOUNDRY_USAGE_TASK_DRAFTS" as wfe.FoundryUsageTask, 1, 1),
        entry("FOUNDRY_USAGE_TASK_LOADS", 1, 50_000_000),
      ],
    });
    expect(view.resets).toBe("");
    expect(view.rows.map((r) => r.label)).toEqual(["Loads", "Questions"]);
    expect(usageView({}).rows).toEqual([]);
  });

  it("shows every task with a dash and an empty bar until it is read", () => {
    const view = usageView(undefined);
    expect(view.total).toBe("—");
    expect(view.resets).toBe("");
    expect(
      view.rows.map((r) => [r.task, r.label, r.within, r.spent, r.cap]),
    ).toEqual(
      [
        ["FOUNDRY_USAGE_TASK_LOADS", "Loads", false],
        ["FOUNDRY_USAGE_TASK_API_SOURCES", "API sources", true],
        ["FOUNDRY_USAGE_TASK_QUESTIONS", "Questions", false],
      ].map((r) => [...r, "—", { of: "", fill: 0, level: "room" }]),
    );
  });
});

describe("usageQuery", () => {
  const calls: string[] = [];
  beforeEach(() => {
    calls.length = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        calls.push(url);
        return new Response(
          JSON.stringify(
            calls.length === 1 ? { usage: { totalUsdMicros: "5" } } : {},
          ),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }),
    );
    vi.stubGlobal("window", {
      location: { pathname: "/", search: "", hash: "", assign: () => {} },
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("reads the workspace's month, briefly cached", async () => {
    const q = usageQuery("ws 1");
    expect(q.queryKey).toEqual(["ws 1", "foundry-usage"]);
    expect(q.staleTime).toBe(15_000);
    expect(await q.queryFn()).toEqual({ totalUsdMicros: "5" });
    expect(calls[0]).toBe("/papi/v1/workspaces/ws%201/foundry/usage");
    expect(await q.queryFn()).toEqual({});
  });
});

describe("the mock's usage route", () => {
  const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
  async function call(
    method: HttpMethod,
    path: string,
    userId = "usr_ada",
  ): Promise<MockReply & { body: any }> {
    const url = new URL(path, "http://mock");
    for (const r of compiled) {
      const params = r.method === method && matchTemplate(r.c, url.pathname);
      if (!params) continue;
      return (await r.handle({
        method,
        path: url.pathname,
        params,
        query: url.searchParams,
        body: undefined,
        principal: { user: db.users.get(userId)!, sessionId: "s" },
      })) as MockReply & { body: any };
    }
    throw new Error(`no mock route for ${method} ${path}`);
  }
  beforeEach(() => {
    vi.useFakeTimers({ now: new Date("2026-10-25T12:00:00Z") });
    resetState();
  });
  afterEach(() => {
    vi.useRealTimers();
    resetState();
  });

  it("serves the month with every task's cap, the total counting loads and questions", async () => {
    (config as { askSpentUsdMicros?: number }).askSpentUsdMicros = 2_500_000;
    const r = await call("GET", "/v1/workspaces/ws_acme/foundry/usage");
    expect(r.status).toBe(200);
    const u = r.body.usage as wfe.FoundryUsage;
    expect(u.monthStart).toBe("2026-10-01T00:00:00.000Z");
    expect(u.resetsAt).toBe("2026-11-01T00:00:00.000Z");
    expect(u.tasks?.map((t) => [t.task, t.capUsdMicros])).toEqual([
      ["FOUNDRY_USAGE_TASK_LOADS", "100000000"],
      ["FOUNDRY_USAGE_TASK_API_SOURCES", "20000000"],
      ["FOUNDRY_USAGE_TASK_QUESTIONS", "30000000"],
    ]);
    const spent = Object.fromEntries(
      u.tasks!.map((t) => [t.task, Number(t.spentUsdMicros)]),
    );
    expect(spent.FOUNDRY_USAGE_TASK_QUESTIONS).toBeGreaterThanOrEqual(
      2_500_000,
    );
    expect(Number(u.totalUsdMicros)).toBe(
      spent.FOUNDRY_USAGE_TASK_LOADS! + spent.FOUNDRY_USAGE_TASK_QUESTIONS!,
    );
  });

  it("refuses someone outside the workspace", async () => {
    db.users.set("usr_eve", {
      id: "usr_eve",
      workosId: "user_01EVE",
      email: "eve@example.com",
      firstName: "Eve",
      lastName: "",
      createdAt: "2026-10-01T09:00:00.000Z",
    });
    const r = await call(
      "GET",
      "/v1/workspaces/ws_acme/foundry/usage",
      "usr_eve",
    );
    expect(r.status).toBe(403);
  });
});
