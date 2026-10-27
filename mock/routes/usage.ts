import type { FoundryUsageResponse } from "../../src/shared/wfe";
import { ok, type MockRoute } from "../http";
import { isReply, membership } from "../state";
import { systemSpend } from "./documents";
import { MONTH_CAP_MICROS, monthSpend } from "./loads";
import { ASK_LIMITS, spend } from "./questions";

// The workspace's AI spend this UTC month by task, as the backend's
// GetFoundryUsage serves it, from the other routes' in-memory state: loads'
// spend (./loads.ts, $100 a month, or prod's $50), API sources' extraction no person
// started (./documents.ts, which spends none unless `eventSetAllowanceSpent`;
// part of loads' spend, so the total counts it once), and questions'
// settled spend and reservations (./questions.ts, $30, or prod's $15). Any member reads it.

/** foundry.events.max_system_usd_per_workspace_month: $20. */
const API_SOURCES_CAP_MICROS = 20_000_000;

export const usageRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/usage",
    handle: ({ params, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const at = new Date(),
        start = new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), 1)),
        resets = new Date(
          Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 1),
        ),
        loads = monthSpend(m.workspaceId),
        questions = spend(m.workspaceId);
      return ok({
        usage: {
          monthStart: start.toISOString(),
          resetsAt: resets.toISOString(),
          totalUsdMicros: String(loads + questions.spent),
          tasks: [
            {
              task: "FOUNDRY_USAGE_TASK_LOADS",
              spentUsdMicros: String(loads),
              reservedUsdMicros: "0",
              capUsdMicros: String(MONTH_CAP_MICROS),
            },
            {
              task: "FOUNDRY_USAGE_TASK_API_SOURCES",
              spentUsdMicros: String(systemSpend(m.workspaceId, "events")),
              reservedUsdMicros: "0",
              capUsdMicros: String(API_SOURCES_CAP_MICROS),
            },
            {
              task: "FOUNDRY_USAGE_TASK_QUESTIONS",
              spentUsdMicros: String(questions.spent),
              reservedUsdMicros: String(questions.reserved),
              capUsdMicros: String(ASK_LIMITS.monthlyUsdMicros),
            },
          ],
        },
      } satisfies FoundryUsageResponse);
    },
  },
];
