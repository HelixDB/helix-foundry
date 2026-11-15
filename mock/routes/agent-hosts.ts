import { randomBytes } from "node:crypto";
import type {
  AgentHelperRuntime,
  AgentRuntime,
  FoundryAgentHost,
  FoundryAgentHostAccount,
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
// it answers any (./agent.ts), once a helper of its runtime is online, and
// fails it in the backend's words while that helper's CLI is signed out.
// Codex is chosen once the owner confirmed what it runs with (confirmCodex,
// kept), and a helper whose Codex is older than CODEX_MIN runs no Codex turn
// (its account says outdated), as the backend's agent.Runnable. Helpers come
// from two places, until the next reset:
// - `{"agentHosts": [{"name": "Ada's Mac", "runtimes": ["claude_code"],
//   "online": true, "account": {"state": "signed_in", "subscription":
//   "max"}, "codexVersion": "0.169.0"}]}` pairs some (account defaults to a
//   Max subscription for Claude Code and a Plus plan for Codex; null is a
//   helper that hasn't said yet; online false was last seen an hour ago;
//   codexVersion, CODEX_MIN by default, is its Codex's);
// - POST /__mock/agent-host/pair {code, name?, runtimes?, account?,
//   codexVersion?} spends a pairing code as a helper's pair command does
//   (once, within ten minutes): the helper it pairs offers runtimes (Claude
//   Code by default), and isn't running, never seen, until
//   POST /__mock/agent-host/hello {host_id} starts it (its run or install).

type Account = Omit<FoundryAgentHostAccount, "runtime" | "outdated">;
type Knobs = {
  agentHosts?: {
    name: string;
    runtimes: AgentHelperRuntime[];
    online?: boolean;
    account?: Account | null;
    codexVersion?: string;
  }[];
};
const knobs = config as typeof config & Knobs;

/** The oldest Codex a helper runs the Assistant on (backend agent.MinCodexVersion). */
export const CODEX_MIN = "0.169.0";

/** A helper the mock's pair stand-in paired: running once started. */
type Paired = {
  id: string;
  name: string;
  runtimes: AgentHelperRuntime[];
  account: Account | null | undefined;
  codexVersion?: string;
  createdAt: string;
  running: boolean;
};

const state = {
  runtimes: new Map<string, AgentRuntime>(),
  // When each workspace's owner confirmed what codex runs with.
  codexConfirmed: new Map<string, string>(),
  revoked: new Set<string>(),
  // Pairing codes made and not yet spent, with when each stops pairing.
  codes: new Map<string, number>(),
  paired: [] as Paired[],
};
onReset(() => {
  state.runtimes.clear();
  state.codexConfirmed.clear();
  state.revoked.clear();
  state.codes.clear();
  state.paired = [];
  delete knobs.agentHosts;
});

const RUNTIMES: AgentRuntime[] = ["api", "claude_code", "codex"];
const PAIRING_MS = 10 * 60_000;
const ONLINE_MS = 30_000;
const MAX_SUBSCRIPTION = { state: "signed_in", subscription: "max" } as const;
const PLUS_PLAN = { state: "signed_in", subscription: "plus" } as const;
// When the mock's helpers paired: they have been paired a day.
const PAIRED_AT = () => new Date(Date.now() - 86_400_000).toISOString();

/**
 * Whether version (x.y.z, maybe a prerelease) is at least min, as the
 * backend's agent.VersionAtLeast.
 */
export function versionAtLeast(version: string, min: string): boolean {
  const parse = (v: string) => {
    const m = /^(\d+)\.(\d+)\.(\d+)(-.*)?$/.exec(v);
    return m ? { n: [+m[1]!, +m[2]!, +m[3]!], pre: !!m[4] } : undefined;
  };
  const a = parse(version),
    b = parse(min);
  if (!a || !b) return false;
  for (let i = 0; i < 3; i++) if (a.n[i] !== b.n[i]) return a.n[i]! > b.n[i]!;
  return !a.pre || b.pre;
}

/**
 * What a helper's CLIs say they are signed in to (account, else each
 * runtime's default; null for none yet), Codex with its version.
 */
function accountsOf(
  runtimes: AgentHelperRuntime[],
  a: Account | null | undefined,
  codexVersion = CODEX_MIN,
): FoundryAgentHostAccount[] {
  if (a === null) return [];
  return runtimes.map((runtime) =>
    runtime === "codex"
      ? {
          runtime,
          ...(a ?? PLUS_PLAN),
          version: codexVersion,
          outdated: !versionAtLeast(codexVersion, CODEX_MIN),
        }
      : { runtime, ...(a ?? MAX_SUBSCRIPTION) },
  );
}

/** What a helper runs of what it offers: not an outdated Codex, nor one it said nothing of. */
const runnable = (
  runtimes: AgentHelperRuntime[],
  accounts: FoundryAgentHostAccount[],
) =>
  runtimes.filter(
    (r) =>
      r !== "codex" || accounts.some((a) => a.runtime === r && !a.outdated),
  );

/** The helpers paired and not revoked, newest first, with stable ids. */
function hosts(): FoundryAgentHost[] {
  const seen = (online: boolean) =>
    new Date(Date.now() - (online ? 1_000 : 3_600_000)).toISOString();
  return [
    ...state.paired.map((p) => {
      const accounts = accountsOf(p.runtimes, p.account, p.codexVersion);
      return {
        id: p.id,
        name: p.name,
        runtimes: runnable(p.runtimes, accounts),
        accounts,
        version: "0.1.0",
        platform: "darwin-arm64",
        createdAt: p.createdAt,
        // Pairing starts nothing: seen once it runs.
        ...(p.running && { lastSeenAt: seen(true) }),
        online: p.running,
        revokedAt: null,
      };
    }),
    ...(knobs.agentHosts ?? []).map((h, i) => {
      const accounts = accountsOf(h.runtimes, h.account, h.codexVersion);
      return {
        id: `01JHOST${String(i).padStart(19, "0")}`,
        name: h.name,
        runtimes: runnable(h.runtimes, accounts),
        accounts,
        version: "0.1.0",
        platform: "darwin-arm64",
        createdAt: PAIRED_AT(),
        lastSeenAt: seen(h.online !== false),
        online: h.online !== false,
        revokedAt: null,
      };
    }),
  ].filter((h) => !state.revoked.has(h.id));
}

/** The helpers online now that run runtime. */
const running = (runtime: AgentHelperRuntime) =>
  hosts().filter(
    (h) =>
      h.online &&
      Date.now() - Date.parse(h.lastSeenAt!) < ONLINE_MS &&
      h.runtimes!.includes(runtime),
  );

/** Whether a helper that runs runtime is online. */
export const helperOnline = (runtime: AgentHelperRuntime) =>
  running(runtime).length > 0;

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
  const old = hosts().find(
    (h) =>
      h.online && h.accounts!.some((a) => a.runtime === runtime && a.outdated),
  );
  return refused(
    ws,
    "FOUNDRY_AGENT_HOST_OFFLINE",
    !LOCAL_TOKEN
      ? "no helper runs this thread here"
      : old
        ? `the Codex on ${old.name} is older than ${CODEX_MIN}, the oldest Foundry runs the Assistant on: update it there, then send again`
        : `no helper running ${runtime} has been seen in the last 30 seconds: start it on your computer, then send again`,
  );
}

/**
 * Why a helper's turn fails as it starts, in the backend's words
 * (hostFailureText): its CLI is signed out on every helper online that
 * runs it. Undefined for a turn that runs.
 */
export function helperFailure(runtime: AgentRuntime): string | undefined {
  if (runtime === "api") return undefined;
  const online = running(runtime);
  if (
    !online.length ||
    !online.every((h) =>
      h.accounts!.some(
        (a) => a.runtime === runtime && a.state === "signed_out",
      ),
    )
  )
    return undefined;
  return runtime === "codex"
    ? "Codex isn't signed in on the computer running the helper. Run codex login there, then send your message again."
    : "Claude Code isn't signed in on the computer running the helper. Run claude, then /login there, then send your message again.";
}

/** Where the workspace's Assistant runs, as GET runtime answers it. */
export function runtimeOf(ws: string): FoundryAgentRuntimeResponse {
  const listed = hosts();
  const online = [
    ...new Set(listed.filter((h) => h.online).flatMap((h) => h.runtimes!)),
  ].sort();
  return {
    runtime: {
      runtime: workspaceRuntime(ws),
      hosts: listed,
      online,
      codexConfirmedAt: state.codexConfirmed.get(ws) ?? null,
      codexMinVersion: CODEX_MIN,
    },
  };
}

/** One helper paired and not revoked, by id. */
export const helperById = (id: string) => hosts().find((h) => h.id === id);

/**
 * A helper's pair command, standing in for /agent-host/pair: spends a code
 * the owner made (once, within ten minutes) on a new helper of runtimes
 * (Claude Code by default), not running until started (startHelper). Refused as the backend's
 * errPairingInvalid otherwise.
 */
export function pairHelper(body: {
  code?: string;
  name?: string;
  runtimes?: AgentHelperRuntime[];
  account?: Account | null;
  codexVersion?: string;
}): MockReply {
  const expires = state.codes.get(body.code || "");
  if (!expires || expires <= Date.now())
    return {
      status: 404,
      body: {
        error: {
          code: "pairing_invalid",
          message:
            "the pairing code is unknown, used or expired: make another in Foundry",
        },
      },
    };
  state.codes.delete(body.code!);
  const id = `01JPAIR${String(state.paired.length).padStart(19, "0")}`;
  state.paired.unshift({
    id,
    name: body.name || "Ada's Mac",
    runtimes: body.runtimes?.length ? body.runtimes : ["claude_code"],
    account: body.account,
    codexVersion: body.codexVersion,
    createdAt: now(),
    running: false,
  });
  return ok({ host_id: id });
}

/**
 * A paired helper's run or install, standing in for its hello: it runs, and
 * is online from then on. Refused as the backend's errHostUnauthenticated
 * for a helper unknown or revoked.
 */
export function startHelper(body: { host_id?: string }): MockReply {
  const p = state.paired.find((h) => h.id === body.host_id);
  if (!p || state.revoked.has(p.id))
    return {
      status: 401,
      body: {
        error: {
          code: "unauthenticated",
          message: "the helper's token is unknown or revoked: pair it again",
        },
      },
    };
  p.running = true;
  return ok({ host_id: p.id, name: p.name });
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
      const req = body as Partial<UpdateFoundryAgentRuntimeRequest>;
      const runtime = req?.runtime;
      if (!runtime || !RUNTIMES.includes(runtime))
        return fail("INVALID_ARGUMENT", "runtime is api, claude_code or codex");
      if (req.confirmCodex && !state.codexConfirmed.has(ws))
        state.codexConfirmed.set(ws, now());
      if (runtime === "codex" && !state.codexConfirmed.has(ws))
        return refused(
          ws,
          "FOUNDRY_AGENT_CODEX_UNCONFIRMED",
          "Codex runs with your own Codex setup, its MCP servers and plugins included, on your ChatGPT plan: choose it in Settings › AI models, where you confirm that once",
        );
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
      const expires = Date.now() + PAIRING_MS;
      state.codes.set(code, expires);
      return ok({
        code,
        expiresAt: new Date(expires).toISOString(),
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
      const host = helperById(params.id!);
      if (!host) return fail("NOT_FOUND", "helper not found");
      state.revoked.add(host.id);
      return ok({
        host: { ...host, online: false, revokedAt: now() },
      } satisfies FoundryAgentHostResponse);
    },
  },
];
