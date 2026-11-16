import type {
  AgentActionResponse,
  AgentRuntime,
  FoundryAgentHostPairingResponse,
  FoundryAgentHostResponse,
  FoundryAgentRuntimeResponse,
  UpdateFoundryAgentRuntimeRequest,
  AgentThreadResponse,
  AgentTurnResponse,
  FoundryAgentAction,
  ListAgentThreadsResponse,
  ListAgentTurnEventsResponse,
  SendAgentMessageRequest,
} from "../../../shared/wfe";
import { api, refusalOf } from "../../api";
import { validZone } from "../automations/model";
import {
  jobSeen,
  jobStarted,
  trackEvent,
  type AgentOperationName,
  type AgentSection,
} from "../../analytics";
import {
  isAgentOperation,
  isLiveTurn,
  parseAgentEvent,
  reduceTurnEvents,
  type TurnStream,
} from "./model";

// The Assistant's WFE calls (src/shared/wfe.ts "Agent"), and the analytics
// each reports: never the text, a name or a payload.

const enc = encodeURIComponent;
const at = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry/agent`;

/** A live turn's events, as Ask's are polled. */
export const EVENTS_POLL_MS = 500;
/** The thread, while a turn of it is live or an action of it runs. */
export const THREAD_POLL_MS = 1500;

const operationOf = (a: FoundryAgentAction): AgentOperationName =>
  isAgentOperation(a.operation || "")
    ? (a.operation as AgentOperationName)
    : "other";

/** Whether the thread changes on its own: a live turn, or an action running. */
export const threadBusy = (t?: AgentThreadResponse) =>
  !!t &&
  ((t.turns || []).some(isLiveTurn) ||
    (t.actions || []).some((a) => a.status === "executing"));

/** The runtime and its helpers, while a helper is awaited or offline. */
export const RUNTIME_POLL_MS = 3000;

export const agentQueries = {
  // Where the workspace's new threads run, and the install's helpers: a
  // local install's owner's only (hosted refuses it).
  runtime: (ws: string) => ({
    queryKey: [ws, "agent", "runtime"],
    queryFn: async () =>
      (await api<FoundryAgentRuntimeResponse>(`${at(ws)}/runtime`)).runtime ??
      {},
  }),
  // The caller's threads, newest first: the first page (50).
  threads: (ws: string) => ({
    queryKey: [ws, "agent", "threads"],
    queryFn: () => api<ListAgentThreadsResponse>(`${at(ws)}/threads`),
  }),
  // One thread, its turns and actions, polled while it changes on its own.
  thread: (ws: string, id: string) => ({
    queryKey: [ws, "agent", "thread", id],
    queryFn: async () => {
      const r = await api<AgentThreadResponse>(`${at(ws)}/threads/${enc(id)}`);
      jobSeen("agent_turn", r.turns || []);
      return r;
    },
    refetchInterval: (q: { state: { data?: AgentThreadResponse } }) =>
      threadBusy(q.state.data) ? THREAD_POLL_MS : false,
  }),
};

/**
 * A turn's events after `prev`, folded in. A card that shows while the turn
 * runs reports its preparing; a turn read once it ended reports nothing.
 */
export async function turnEvents(
  ws: string,
  turnId: string,
  prev: TurnStream,
): Promise<TurnStream> {
  const page = await api<ListAgentTurnEventsResponse>(
    `${at(ws)}/turns/${enc(turnId)}/events?afterSeq=${prev.lastSeq}`,
  );
  if (prev.lastSeq > 0 || isLiveTurn(page))
    for (const e of page.events || []) {
      const ev =
        e.seq > prev.lastSeq ? parseAgentEvent(e.eventJson) : undefined;
      if (ev?.type === "action_card")
        trackEvent("agent_action_prepared", {
          operation: isAgentOperation(ev.operation) ? ev.operation : "other",
          target_named_by_user: ev.target_named_by_user,
        });
    }
  return reduceTurnEvents(prev, page);
}

/**
 * Sends a message: a new thread's first without `threadId`. The same client
 * token is the same turn. Every turn carries the browser's time zone, when
 * it names one, for the Assistant's schedules.
 */
export async function sendAgentMessage(
  ws: string,
  body: SendAgentMessageRequest,
  section: AgentSection,
) {
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const sent: SendAgentMessageRequest = validZone(zone)
    ? { ...body, pageContext: { ...body.pageContext, timeZone: zone } }
    : body;
  const r = await api<AgentTurnResponse>(`${at(ws)}/turns`, sent).catch(
    (e: unknown) => {
      trackEvent("agent_turn_refused", { reason: refusalOf(e) });
      throw e;
    },
  );
  jobStarted(
    { job: "agent_turn", section, runtime: r.thread.runtime ?? "api" },
    r.turn.id,
  );
  return r;
}

/** Stops a queued or running turn. */
export const cancelAgentTurn = (ws: string, turnId: string) =>
  api<AgentTurnResponse>(`${at(ws)}/turns/${enc(turnId)}:cancel`, {});

/** Deletes one of the caller's threads; a live turn of it stops. */
export const deleteAgentThread = (ws: string, id: string) =>
  api(`${at(ws)}/threads/${enc(id)}`, undefined, "DELETE");

/** Runs the action as shown: its payload's hash. The agent goes on from it in `continuation`. */
export async function executeAgentAction(ws: string, a: FoundryAgentAction) {
  const r = await api<AgentActionResponse>(
    `${at(ws)}/actions/${enc(a.id)}:execute`,
    { payloadHash: a.payloadHash || "" },
  ).catch((e: unknown) => {
    trackEvent("agent_action_refused", { reason: refusalOf(e) });
    throw e;
  });
  const status = r.action.status;
  if (status === "succeeded" || status === "failed" || status === "unknown")
    trackEvent("agent_action_executed", {
      operation: operationOf(a),
      outcome: status,
      target_named_by_user: !!a.targetNamedByUser,
    });
  return r;
}

/** Sets where the workspace's new threads run: a helper need not be online. */
/**
 * Sets where the workspace's new threads run: codex with confirmCodex the
 * first time (the owner confirmed what it runs with).
 */
export async function setAgentRuntime(
  ws: string,
  runtime: AgentRuntime,
  confirmCodex = false,
) {
  const body: UpdateFoundryAgentRuntimeRequest = {
    runtime,
    ...(confirmCodex && { confirmCodex }),
  };
  const r = await api<FoundryAgentRuntimeResponse>(
    `${at(ws)}/runtime`,
    body,
    "PATCH",
  );
  trackEvent("agent_runtime_set", { runtime });
  return r.runtime ?? {};
}

/** A one-time code, ten minutes, and the command that pairs a helper with it. */
export async function pairAgentHelper(ws: string) {
  const r = await api<FoundryAgentHostPairingResponse>(
    `${at(ws)}/host-pairings`,
    {},
  );
  trackEvent("agent_helper_pairing_made");
  return r;
}

/** Unpairs a helper: its token and its turns' stop working at once. */
export async function revokeAgentHelper(ws: string, id: string) {
  const r = await api<FoundryAgentHostResponse>(
    `${at(ws)}/hosts/${enc(id)}:revoke`,
    {},
  );
  trackEvent("agent_helper_revoked");
  return r;
}

/** Prepares an expired action again, as the person is now, with no model call. */
export const refreshAgentAction = (ws: string, id: string) =>
  api<AgentActionResponse>(`${at(ws)}/actions/${enc(id)}:refresh`, {});

/** The person's no: nothing runs and no turn starts. */
export async function dismissAgentAction(ws: string, a: FoundryAgentAction) {
  const r = await api<AgentActionResponse>(
    `${at(ws)}/actions/${enc(a.id)}:dismiss`,
    {},
  );
  trackEvent("agent_action_dismissed", {
    operation: operationOf(a),
    target_named_by_user: !!a.targetNamedByUser,
  });
  return r;
}
