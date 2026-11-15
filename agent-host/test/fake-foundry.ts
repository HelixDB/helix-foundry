// A stand-in for a local install's integration listener: the helper's
// routes (backend/local/agent_host.go) and /mcp, as they answer, recording
// what the helper and its CLI send.

import { createServer, type IncomingMessage, type Server } from "node:http";

import type { Turn } from "../src/foundry.ts";

export type Recorded = { path: string; auth: string; body: any };

export class FakeFoundry {
  server: Server;
  url = "";
  calls: Recorded[] = [];
  /** Turns the next claims take, in order. */
  queue: Turn[] = [];
  /** Turns whose lease Foundry took back: renew answers turn_lost. */
  lost = new Set<string>();
  /** A revoked helper's token is refused everywhere. */
  revoked = false;

  constructor() {
    this.server = createServer(async (req, res) => {
      const body = await read(req);
      const auth = req.headers.authorization ?? "";
      const path = req.url ?? "";
      this.calls.push({ path, auth, body });
      const send = (status: number, out?: unknown) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(out === undefined ? "" : JSON.stringify(out));
      };
      const refuse = (status: number, code: string) =>
        send(status, { error: { code, message: code } });
      if (path === "/agent-host/pair")
        return body.code === "hxp_good"
          ? send(200, { host_id: "host-1", token: "hxh_token" })
          : refuse(404, "pairing_invalid");
      if (path === "/mcp") {
        return auth.startsWith("Bearer hxr_")
          ? send(200, {
              jsonrpc: "2.0",
              id: body.id,
              result: { content: [{ type: "text", text: "Sales graph" }] },
            })
          : refuse(401, "unauthenticated");
      }
      if (auth !== "Bearer hxh_token" || this.revoked)
        return refuse(401, "unauthenticated");
      if (path === "/agent-host/hello")
        return send(200, { host_id: "host-1", name: "Ada's Mac" });
      if (path === "/agent-host/claim") {
        const turn = this.queue.shift();
        if (turn) return send(200, { turn });
        await new Promise((r) => setTimeout(r, 30));
        return send(204);
      }
      const m = /^\/agent-host\/turns\/([^/]+)\/(renew|text|finish)$/.exec(
        path,
      );
      if (!m) return refuse(404, "not_found");
      if (this.lost.has(m[1]!)) return refuse(410, "turn_lost");
      if (m[2] === "renew")
        return send(200, {
          lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
        });
      return send(204);
    });
  }

  async start(): Promise<this> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    const a = this.server.address();
    this.url = `http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}`;
    return this;
  }

  stop(): Promise<void> {
    this.server.closeAllConnections();
    return new Promise((r) => this.server.close(() => r()));
  }

  /** What the helper sent to path, in order. */
  sent(path: string): any[] {
    return this.calls.filter((c) => c.path === path).map((c) => c.body);
  }
}

async function read(req: IncomingMessage): Promise<any> {
  let text = "";
  for await (const chunk of req) text += chunk;
  return text ? JSON.parse(text) : undefined;
}

/** A turn as Foundry hands it to a helper. */
export function turn(over: Partial<Turn> = {}): Turn {
  return {
    id: "turn-1",
    workspace_id: "ws-1",
    thread_id: "thread-1",
    runtime: "claude_code",
    attempt: 1,
    lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
    deadline: new Date(Date.now() + 240_000).toISOString(),
    max_steps: 20,
    mcp_token: "hxr_turn",
    system: "You are the Assistant in Helix Foundry.",
    prompt: "Which ontologies do I have?",
    ...over,
  };
}
