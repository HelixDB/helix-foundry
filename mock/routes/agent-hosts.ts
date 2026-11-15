import { randomBytes } from "node:crypto";
import type {
  AgentRuntime,
  FoundryAgentHost,
  FoundryAgentHostPairingResponse,
  FoundryAgentHostResponse,
  FoundryAgentRuntimeResponse,
  UpdateFoundryAgentRuntimeRequest,
} from "../../src/shared/wfe";
import { fail, now, ok, type MockReply, type MockRoute } from "../http";
import { config, isReply, LOCAL_TOKEN, membership, onReset } from "../state";
import { foundryPrecondition as refused } from "./ai";

// Where a local install's Assistant runs, and the helpers paired with it
// (backend api/agent_hosts.go): the owner chooses each workspace's runtime
// for its new threads (api, claude_code or codex), makes one-time pairing
// codes, and revokes helpers. Hosted (no MOCK_LOCAL_TOKEN) every route is
// FAILED_PRECONDITION, FOUNDRY_AGENT_HOSTS_LOCAL_ONLY, and every thread runs
// on the worker.
//
// The mock runs no helper: its scripted agent answers a helper's thread as
// it answers any (./agent.ts), once a helper of its runtime is online.
// `{"agentHosts": [{"name": "Ada's Mac", "runtimes": ["claude_code"],
// "online": true}]}` pairs helpers until the next reset; a pairing code
// pairs nothing by itself.

type Helper = Exclude<AgentRuntime, "api">;
type Knobs = {
  agentHosts?: { name: string; runtimes: Helper[]; online?: boolean }[];
};
const knobs = config as typeof config & Knobs;

const state = {
  runtimes: new Map<string, AgentRuntime>(),
  revoked: new Set<string>(),
};
onReset(() => {
  state.runtimes.clear();
  state.revoked.clear();
  delete knobs.agentHosts;
});

const RUNTIMES: AgentRuntime[] = ["api", "claude_code", "codex"];
const PAIRING_MS = 10 * 60_000;
const ONLINE_MS = 30_000;

/** The helpers paired by agentHosts and not revoked, with stable ids. */
function hosts(): FoundryAgentHost[] {
  return (knobs.agentHosts ?? [])
    .map((h, i) => ({
      id: `01JHOST${String(i).padStart(19, "0")}`,
      name: h.name,
      runtimes: h.runtimes,
      version: "0.1.0",
      platform: "darwin-arm64",
      createdAt: now(),
      lastSeenAt: new Date(
        Date.now() - (h.online === false ? 3_600_000 : 1_000),
      ).toISOString(),
      online: h.online !== false,
      revokedAt: null,
    }))
    .filter((h) => !state.revoked.has(h.id));
}

/** Whether a helper that runs runtime is online. */
export function helperOnline(runtime: Helper): boolean {
  return hosts().some(
    (h) =>
      h.online &&
      Date.now() - Date.parse(h.lastSeenAt!) < ONLINE_MS &&
      h.runtimes!.includes(runtime),
  );
}

/** Where a workspace's new threads run: always api hosted. */
export function workspaceRuntime(ws: string): AgentRuntime {
  return (LOCAL_TOKEN && state.runtimes.get(ws)) || "api";
}

/**
 * Why a message to a thread of runtime can't be sent: a helper's thread
 * while none that runs it is online, as the backend's agentThreadRuntime.
 */
export function hostOffline(
  ws: string,
  runtime: AgentRuntime,
): MockReply | undefined {
  if (runtime === "api") return undefined;
  if (LOCAL_TOKEN && helperOnline(runtime)) return undefined;
  return refused(
    ws,
    "FOUNDRY_AGENT_HOST_OFFLINE",
    LOCAL_TOKEN
      ? `no helper running ${runtime} has been seen in the last 30 seconds: start it on your computer, then send again`
      : "no helper runs this thread here",
  );
}

/** The owner of a local install's workspace, or why not. */
function owner(ws: string, userId: string): MockReply | undefined {
  const m = membership(ws, userId);
  if (isReply(m)) return m;
  if (!LOCAL_TOKEN)
    return refused(
      ws,
      "FOUNDRY_AGENT_HOSTS_LOCAL_ONLY",
      "the Assistant runs on a subscription only on a local install",
    );
  if (m.role !== "WORKSPACE_ROLE_OWNER")
    return fail(
      "PERMISSION_DENIED",
      "only the workspace's owner chooses where the Assistant runs",
    );
  return undefined;
}

function runtimeOf(ws: string): FoundryAgentRuntimeResponse {
  const listed = hosts();
  const online = [
    ...new Set(listed.filter((h) => h.online).flatMap((h) => h.runtimes!)),
  ].sort();
  return {
    runtime: { runtime: workspaceRuntime(ws), hosts: listed, online },
  };
}

const BASE = "/v1/workspaces/{workspaceId}/foundry/agent" as const;

export const agentHostRoutes: MockRoute[] = [
  {
    method: "GET",
    path: `${BASE}/runtime`,
    handle: ({ params, principal: { user } }) => {
      const ws = params.workspaceId!;
      return owner(ws, user.id) ?? ok(runtimeOf(ws));
    },
  },
  {
    method: "PATCH",
    path: `${BASE}/runtime`,
    handle: ({ params, body, principal: { user } }) => {
      const ws = params.workspaceId!;
      const refusal = owner(ws, user.id);
      if (refusal) return refusal;
      const runtime = (body as Partial<UpdateFoundryAgentRuntimeRequest>)
        ?.runtime;
      if (!runtime || !RUNTIMES.includes(runtime))
        return fail("INVALID_ARGUMENT", "runtime is api, claude_code or codex");
      state.runtimes.set(ws, runtime);
      return ok(runtimeOf(ws));
    },
  },
  {
    method: "POST",
    path: `${BASE}/host-pairings`,
    handle: ({ params, principal: { user } }) => {
      const refusal = owner(params.workspaceId!, user.id);
      if (refusal) return refusal;
      const code = "hxp_" + randomBytes(32).toString("base64url");
      return ok({
        code,
        expiresAt: new Date(Date.now() + PAIRING_MS).toISOString(),
        command: `npx -y @helixdb/foundry-agent-host@0.1.0 pair ${code}`,
      } satisfies FoundryAgentHostPairingResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/hosts/{id}:revoke`,
    handle: ({ params, principal: { user } }) => {
      const refusal = owner(params.workspaceId!, user.id);
      if (refusal) return refusal;
      if (!/^[0-9A-Z]{26}$/.test(params.id!))
        return fail("INVALID_ARGUMENT", "id is a helper's");
      const host = hosts().find((h) => h.id === params.id);
      if (!host) return fail("NOT_FOUND", "helper not found");
      state.revoked.add(host.id);
      return ok({
        host: { ...host, online: false, revokedAt: now() },
      } satisfies FoundryAgentHostResponse);
    },
  },
];
