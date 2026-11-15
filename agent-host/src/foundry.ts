// Foundry's side of a helper (backend/local/agent_host.go): the routes it
// calls on the install's integration listener, all JSON, each but pair with
// the helper's hxh_ token. Nothing else reaches Foundry from here: the CLI
// calls Foundry's tools itself, at /mcp, with the turn's hxr_ token.

/** A runtime a helper runs: the CLI it drives. */
export type Runtime = "claude_code" | "codex";

/**
 * What a runtime's CLI is signed in to, as the helper last found it
 * (backend agent.HostAccount): on a subscription when it names one, or
 * some other way; signed out; or unknown, when its CLI didn't start. Codex
 * says its version too: Foundry runs none older than its floor.
 */
export type Account =
  | {
      runtime: Runtime;
      state: "signed_in";
      subscription?: string;
      version?: string;
    }
  | { runtime: Runtime; state: "signed_out" | "unknown"; version?: string };

/** A turn Foundry leased to this helper, and what its CLI is told. */
export type Turn = {
  id: string;
  /** This claim of it: renew, text and finish name it, so a run whose lease lapsed can't write to the turn once another claim took it. */
  claim_id: string;
  workspace_id: string;
  thread_id: string;
  runtime: Runtime;
  attempt: number;
  lease_expires_at: string;
  /** When the turn's wall time runs out: Foundry ends it then. */
  deadline: string;
  max_steps: number;
  /** The bearer its CLI calls Foundry's tools at /mcp with. */
  mcp_token: string;
  system: string;
  prompt: string;
  /** The CLI session the thread's last turn ended in. */
  resume?: string;
  /** The thread's earlier turns in words, for a CLI that lost its session. */
  history?: string;
};

/** How a turn ended, as Foundry takes it: a closed set. */
export type End =
  | { end: "answered" }
  | {
      end: "stopped";
      reason: "max_steps" | "max_tokens" | "refusal" | "context";
    }
  | {
      end: "failed";
      failure: "signed_out" | "not_installed" | "rate_limited" | "cli_error";
      detail?: string;
    };

/** A refusal's code, as Foundry words it. */
export type Refusal =
  | "unauthenticated"
  | "pairing_invalid"
  | "turn_lost"
  | "wall_time"
  | "invalid_arguments"
  | "internal";

/** A call Foundry refused, with its code. */
export class FoundryError extends Error {
  readonly code: Refusal | "unreachable";
  readonly status: number;
  constructor(code: Refusal | "unreachable", status: number, message: string) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

/** Foundry's integration listener, as one helper calls it. */
export class Foundry {
  readonly base: string;
  readonly token: string;
  constructor(base: string, token = "") {
    this.base = base.replace(/\/+$/, "");
    this.token = token;
  }

  /** The URL the CLI calls Foundry's tools at. */
  get mcpURL(): string {
    return `${this.base}/mcp`;
  }

  private async call<T>(
    path: string,
    body: unknown,
    signal?: AbortSignal,
  ): Promise<T | undefined> {
    let res: Response;
    try {
      res = await fetch(this.base + path, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          ...(this.token && { authorization: `Bearer ${this.token}` }),
        },
        body: JSON.stringify(body),
        signal,
      });
    } catch (e) {
      if (signal?.aborted) throw e;
      throw new FoundryError(
        "unreachable",
        0,
        `Foundry isn't reachable at ${this.base}: ${(e as Error).message}`,
      );
    }
    if (res.status === 204) return undefined;
    const text = await res.text();
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
    if (!res.ok) {
      const error = (
        parsed as { error?: { code?: string; message?: string } } | undefined
      )?.error;
      throw new FoundryError(
        (error?.code as Refusal) ?? "internal",
        res.status,
        error?.message ?? `Foundry answered ${res.status}`,
      );
    }
    return parsed as T;
  }

  /** Spends a pairing code on this computer: its id and token. */
  async pair(hello: {
    code: string;
    name: string;
    runtimes: Runtime[];
    version: string;
    platform: string;
  }) {
    const out = await this.call<{ host_id: string; token: string }>(
      "/agent-host/pair",
      hello,
    );
    if (!out?.token?.startsWith("hxh_"))
      throw new FoundryError(
        "internal",
        200,
        "Foundry answered the pairing without a token",
      );
    return out;
  }

  /** Says this helper runs, what it offers and what its CLIs are signed in to: who it is to Foundry. */
  async hello(runtimes: Runtime[], version: string, accounts: Account[]) {
    const out = await this.call<{ host_id: string; name: string }>(
      "/agent-host/hello",
      { runtimes, version, accounts },
    );
    if (!out)
      throw new FoundryError(
        "internal",
        204,
        "Foundry answered hello with nothing",
      );
    return out;
  }

  /** Waits up to 25 s for a turn: undefined when none came. */
  async claim(
    runtimes: Runtime[],
    version: string,
    accounts: Account[],
    signal?: AbortSignal,
  ): Promise<Turn | undefined> {
    return (
      await this.call<{ turn: Turn }>(
        "/agent-host/claim",
        { runtimes, version, accounts },
        signal,
      )
    )?.turn;
  }

  /** Extends the claim's lease: when it lapses next. Past the turn's wall time, Foundry ends it (wall_time). */
  async renew(claimId: string): Promise<string> {
    const out = await this.call<{ lease_expires_at: string }>(
      `/agent-host/claims/${encodeURIComponent(claimId)}/renew`,
      {},
    );
    return out?.lease_expires_at ?? "";
  }

  /** Shows the person what the model writes in step: final once it ends. */
  async text(
    claimId: string,
    step: number,
    text: string,
    final: boolean,
  ): Promise<void> {
    await this.call(`/agent-host/claims/${encodeURIComponent(claimId)}/text`, {
      step,
      text,
      final,
    });
  }

  /** Ends the claim's turn, keeping the CLI's session for the thread's next. */
  async finish(
    claimId: string,
    end: End,
    steps: number,
    sessionId: string,
  ): Promise<void> {
    await this.call(
      `/agent-host/claims/${encodeURIComponent(claimId)}/finish`,
      {
        ...end,
        steps,
        ...(sessionId && { session_id: sessionId }),
      },
    );
  }

  /** Which of these threads (at most GONE_BATCH) are no one's now: deleted, or never were. */
  async gone(threads: ThreadRef[]): Promise<string[]> {
    const out = await this.call<{ gone?: string[] }>(
      "/agent-host/threads/gone",
      {
        threads: threads.map((t) => ({
          workspace_id: t.workspaceId,
          thread_id: t.threadId,
        })),
      },
    );
    return out?.gone ?? [];
  }
}

/** A thread the helper keeps a CLI session of. */
export type ThreadRef = { workspaceId: string; threadId: string };

/** How many threads one gone() asks after. */
export const GONE_BATCH = 100;
