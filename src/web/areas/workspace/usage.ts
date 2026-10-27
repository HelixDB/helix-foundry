import type {
  FoundryUsage,
  FoundryUsageResponse,
  FoundryUsageTask,
} from "../../../shared/wfe";
import { api } from "../../api";
import { formatUsd } from "../documents/model";

// The workspace's AI spend this month by task, for the account menu
// (the backend's GetFoundryUsage): the UTC calendar month the caps
// count, each task's spend against its cap, spend on the workspace's own
// model key included. Drafts record tokens, not dollars, and titles no
// spend, so neither has a row. Read when the menu opens, kept briefly: work spends while it is open.

export const usageKey = (workspaceId: string) => [workspaceId, "foundry-usage"];
export const usageQuery = (workspaceId: string) => ({
  queryKey: usageKey(workspaceId),
  queryFn: async () =>
    (
      await api<FoundryUsageResponse>(
        `/v1/workspaces/${encodeURIComponent(workspaceId)}/foundry/usage`,
      )
    ).usage || {},
  staleTime: 15_000,
});

/**
 * The tasks the menu shows, in order, with what each one's spend is. API
 * sources' spend is part of loads'.
 */
const TASKS: readonly {
  task: FoundryUsageTask;
  label: string;
  hint: string;
  within: boolean;
}[] = [
  {
    task: "FOUNDRY_USAGE_TASK_LOADS",
    label: "Loads",
    hint: "Reading documents into your ontologies",
    within: false,
  },
  {
    task: "FOUNDRY_USAGE_TASK_API_SOURCES",
    label: "API sources",
    hint: "Part of Loads: reading API sources’ pieces no one asked for",
    within: true,
  },
  {
    task: "FOUNDRY_USAGE_TASK_QUESTIONS",
    label: "Questions",
    hint: "Answering questions in Ask",
    within: false,
  },
];

/** From this share of a cap on, its bar warns. */
export const USAGE_WARNING = 0.8;

/** How full a cap is: under USAGE_WARNING, from it, or spent. */
type Level = "room" | "warn" | "full";

/** A task's row: what it spent and, with a cap, how much of it is used. */
export type UsageRow = {
  task: FoundryUsageTask;
  label: string;
  // What the spend is, for the row's tooltip.
  hint: string;
  // Part of the row above's spend (API sources' within loads').
  within: boolean;
  spent: string;
  // "of $50.00" and the bar: how full the cap is, 0 to 1, counting what
  // running work holds back; none for a task without a cap.
  cap?: { of: string; fill: number; level: Level };
  // What running questions hold back, when they hold anything.
  held?: string;
};

/** The menu's Usage section: the month's total, its reset and the rows. */
export type UsageView = { total: string; resets: string; rows: UsageRow[] };

const PLACEHOLDER = "—";

/**
 * The Usage section of `usage`. Until it is read (or when it can't be),
 * every task shows a dash and an empty bar, so the open menu keeps its
 * height. A task the WFE doesn't send, or one this page doesn't know, has
 * no row.
 *
 * @example
 * usageView({
 *   resetsAt: "2026-11-01T00:00:00Z",
 *   totalUsdMicros: "4250000",
 *   tasks: [{ task: "FOUNDRY_USAGE_TASK_QUESTIONS", spentUsdMicros: "250000",
 *     reservedUsdMicros: "500000", capUsdMicros: "15000000" }],
 * });
 * // { total: "$4.25", resets: "November 1", rows: [{ task: "FOUNDRY_USAGE_TASK_QUESTIONS",
 * //   label: "Questions", hint: "Answering questions in Ask", within: false, spent: "$0.25",
 * //   cap: { of: "of $15.00", fill: 0.05, level: "room" }, held: "$0.50" }] }
 */
export function usageView(usage: FoundryUsage | undefined): UsageView {
  if (!usage)
    return {
      total: PLACEHOLDER,
      resets: "",
      rows: TASKS.map((t) => ({
        ...t,
        spent: PLACEHOLDER,
        cap: { of: "", fill: 0, level: "room" },
      })),
    };
  const resets = usage.resetsAt ? new Date(usage.resetsAt) : null;
  return {
    total: formatUsd(usage.totalUsdMicros),
    resets:
      resets && !Number.isNaN(resets.getTime())
        ? resets.toLocaleDateString("en-US", {
            month: "long",
            day: "numeric",
            timeZone: "UTC",
          })
        : "",
    rows: TASKS.flatMap((t): UsageRow[] => {
      const e = usage.tasks?.find((x) => x.task === t.task);
      if (!e) return [];
      const spent = Math.max(0, Number(e.spentUsdMicros) || 0),
        reserved = Math.max(0, Number(e.reservedUsdMicros) || 0),
        cap = e.capUsdMicros == null ? 0 : Number(e.capUsdMicros),
        fill = cap > 0 ? Math.min(1, (spent + reserved) / cap) : 0,
        level: Level =
          fill >= 1 ? "full" : fill >= USAGE_WARNING ? "warn" : "room";
      return [
        {
          ...t,
          spent: formatUsd(spent),
          ...(cap > 0
            ? { cap: { of: `of ${formatUsd(cap)}`, fill, level } }
            : {}),
          ...(reserved > 0 ? { held: formatUsd(reserved) } : {}),
        },
      ];
    }),
  };
}
