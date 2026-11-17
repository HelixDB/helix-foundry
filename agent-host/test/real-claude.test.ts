// The isolation the helper promises, checked against the real Claude Code
// it pins (the Agent SDK's bundled CLI, or any `claude` REAL_CLAUDE names),
// not a stand-in: what the model is offered and what the CLI runs. The CLI
// talks to a stand-in for Anthropic's API (ANTHROPIC_BASE_URL, with a key
// a wrapper adds, since the helper drops the person's), which records each
// request and plays the model: it asks for Bash, Read and Foundry's tool at
// once, then answers. The person's own configuration is planted in HOME (an
// MCP server, hooks, permissions that allow Bash) to show none of it
// reaches a turn. It runs with REAL_CLAUDE set, and skips otherwise.
//
// Claude Code 2.1.321 also sends a DeferredToolPlaceholder: a tool with no
// input that the API holds back (defer_loading) and that the CLI refuses to
// run, sent only to keep its deferred tool loading on. The test holds it to
// that.

import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, describe, it } from "node:test";

import { claudeCli } from "../src/claude.ts";
import { Foundry } from "../src/foundry.ts";
import { runTurn } from "../src/run.ts";
import { VERSION } from "../src/version.ts";
import { claimPath, turn } from "./fake-foundry.ts";

const REAL = process.env.REAL_CLAUDE;
const MARKER = "You are the Assistant in Helix Foundry, under test.";

type Req = { path: string; auth: string; body: any };

async function read(req: IncomingMessage): Promise<any> {
  let text = "";
  for await (const chunk of req) text += chunk;
  try {
    return text ? JSON.parse(text) : undefined;
  } catch {
    return text;
  }
}

/** A model's reply, streamed as Anthropic's API streams it. */
function stream(blocks: any[], stop: string): string {
  const ev = (type: string, data: object) =>
    `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;
  let out = ev("message_start", {
    message: {
      id: `msg_${Math.random().toString(36).slice(2)}`,
      type: "message",
      role: "assistant",
      model: "claude-test",
      content: [],
      stop_reason: null,
      stop_sequence: null,
      usage: { input_tokens: 10, output_tokens: 1 },
    },
  });
  blocks.forEach((b, index) => {
    if (b.type === "text") {
      out += ev("content_block_start", {
        index,
        content_block: { type: "text", text: "" },
      });
      out += ev("content_block_delta", {
        index,
        delta: { type: "text_delta", text: b.text },
      });
    } else {
      out += ev("content_block_start", {
        index,
        content_block: { type: "tool_use", id: b.id, name: b.name, input: {} },
      });
      out += ev("content_block_delta", {
        index,
        delta: {
          type: "input_json_delta",
          partial_json: JSON.stringify(b.input),
        },
      });
    }
    out += ev("content_block_stop", { index });
  });
  out += ev("message_delta", {
    delta: { stop_reason: stop, stop_sequence: null },
    usage: { output_tokens: 5 },
  });
  return out + ev("message_stop", {});
}

/** Anthropic's API, Foundry's integration listener and the person's own MCP server, in one stand-in. */
class World {
  server: Server;
  url = "";
  calls: Req[] = [];
  /** The main conversation's requests to the model. */
  turns: any[] = [];

  constructor() {
    this.server = createServer(async (req, res) => {
      const body = await read(req);
      const path = (req.url ?? "").split("?")[0]!;
      this.calls.push({ path, auth: req.headers.authorization ?? "", body });
      const json = (status: number, out: unknown) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify(out));
      };
      if (path === "/v1/messages") return this.model(body, res, json);
      if (path === "/mcp" || path === "/person-mcp") {
        if (path === "/mcp" && req.headers.authorization !== "Bearer hxr_turn")
          return json(401, { error: { code: "unauthenticated" } });
        if (body?.id === undefined) {
          res.writeHead(202);
          return res.end();
        }
        const result =
          body.method === "initialize"
            ? {
                protocolVersion: body.params?.protocolVersion ?? "2025-06-18",
                capabilities: { tools: {} },
                serverInfo: { name: path.slice(1), version: "1" },
              }
            : body.method === "tools/list"
              ? {
                  tools: [
                    {
                      name:
                        path === "/mcp"
                          ? "helix_foundry_list_ontologies"
                          : "person_secret_tool",
                      description: "Lists things.",
                      inputSchema: { type: "object", properties: {} },
                    },
                  ],
                }
              : body.method === "tools/call"
                ? { content: [{ type: "text", text: "Sales graph" }] }
                : {};
        return json(200, { jsonrpc: "2.0", id: body.id, result });
      }
      if (path === claimPath("renew"))
        return json(200, {
          lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
        });
      if (path.startsWith("/agent-host/")) {
        res.writeHead(204);
        return res.end();
      }
      json(404, { type: "error", error: { type: "not_found_error" } });
    });
  }

  /** Plays the model: in the turn, Bash, Read and Foundry's tool at once, then an answer. */
  model(
    body: any,
    res: import("node:http").ServerResponse,
    json: (s: number, o: unknown) => void,
  ) {
    const system = JSON.stringify(body?.system ?? "");
    const main = system.includes(MARKER);
    if (main) this.turns.push(body);
    const blocks =
      main && this.turns.length === 1
        ? [
            { type: "text", text: "Let me look." },
            {
              type: "tool_use",
              id: "toolu_bash",
              name: "Bash",
              input: { command: "cat /etc/passwd" },
            },
            {
              type: "tool_use",
              id: "toolu_read",
              name: "Read",
              input: { file_path: "/etc/passwd" },
            },
            {
              type: "tool_use",
              id: "toolu_placeholder",
              name: "DeferredToolPlaceholder",
              input: {},
            },
            {
              type: "tool_use",
              id: "toolu_foundry",
              name: "mcp__foundry__helix_foundry_list_ontologies",
              input: {},
            },
          ]
        : [{ type: "text", text: main ? "You have Sales graph." : "ok" }];
    const stop = blocks.some((b) => b.type === "tool_use")
      ? "tool_use"
      : "end_turn";
    if (!body?.stream)
      return json(200, {
        id: "msg_plain",
        type: "message",
        role: "assistant",
        model: "claude-test",
        content: blocks,
        stop_reason: stop,
        stop_sequence: null,
        usage: { input_tokens: 10, output_tokens: 5 },
      });
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.end(stream(blocks, stop));
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
}

describe("the real Claude Code", { skip: !REAL && "set REAL_CLAUDE" }, () => {
  let world: World;
  let dir: string;
  let outcome: string;

  before(async () => {
    world = await new World().start();
    dir = await mkdtemp(join(tmpdir(), "agent-host-real-"));
    // The person's own Claude Code: an MCP server of theirs, a hook, and
    // permissions that would allow Bash and Read.
    await mkdir(join(dir, ".claude"), { recursive: true });
    const theirs = {
      mcpServers: {
        person: { type: "http", url: `${world.url}/person-mcp` },
      },
    };
    await writeFile(join(dir, ".claude.json"), JSON.stringify(theirs));
    await writeFile(
      join(dir, ".claude", "settings.json"),
      JSON.stringify({
        permissions: { allow: ["Bash", "Read", "mcp__person"] },
        hooks: {
          PreToolUse: [
            {
              matcher: "*",
              hooks: [{ type: "command", command: `touch ${dir}/hooked` }],
            },
          ],
        },
      }),
    );
    // The key goes in a wrapper: the helper never passes the person's on.
    const wrapper = join(dir, "claude");
    await writeFile(
      wrapper,
      `#!/bin/sh\nANTHROPIC_API_KEY=sk-ant-test exec "${REAL}" "$@"\n`,
    );
    await chmod(wrapper, 0o755);
    const env = {
      PATH: process.env.PATH,
      HOME: dir,
      FOUNDRY_AGENT_HOST_STATE: join(dir, "state"),
      ANTHROPIC_BASE_URL: world.url,
      DISABLE_TELEMETRY: "1",
      DISABLE_ERROR_REPORTING: "1",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
    };
    outcome = await runTurn(turn({ system: MARKER }), {
      foundry: new Foundry(world.url, "hxh_token"),
      clis: [claudeCli(wrapper, env)],
      version: VERSION,
      concurrency: 1,
      env,
    });
  });
  after(() => world?.stop());

  it("answers the turn", () => {
    assert.equal(
      outcome,
      "answered",
      JSON.stringify(world.calls.map((c) => c.path)),
    );
    assert.ok(world.turns.length >= 2, "the model was asked twice");
  });

  it("offers the model Foundry's tools alone", () => {
    for (const body of world.turns) {
      const tools = (body.tools ?? []) as {
        name: string;
        defer_loading?: boolean;
        input_schema?: { properties?: object };
      }[];
      assert.deepEqual(
        tools.map((t) => t.name).filter((n) => n !== "DeferredToolPlaceholder"),
        ["mcp__foundry__helix_foundry_list_ontologies"],
        "no built-in tool, and none of the person's MCP servers",
      );
      // The CLI's placeholder, if sent, is held back and takes nothing.
      for (const t of tools.filter((t) => t.name === "DeferredToolPlaceholder"))
        assert.deepEqual(
          { defer: t.defer_loading, input: t.input_schema?.properties },
          { defer: true, input: {} },
        );
    }
  });

  it("runs neither Bash, Read nor its placeholder, and Foundry's tool with the turn's token", () => {
    const results = new Map<string, any>();
    for (const m of world.turns[1].messages)
      for (const b of Array.isArray(m.content) ? m.content : [])
        if (b.type === "tool_result") results.set(b.tool_use_id, b);
    for (const id of ["toolu_bash", "toolu_read", "toolu_placeholder"]) {
      const r = results.get(id);
      assert.ok(r, `${id} was answered`);
      assert.equal(r.is_error, true, `${id} was refused: ${JSON.stringify(r)}`);
      assert.doesNotMatch(JSON.stringify(r.content), /root:/);
    }
    assert.equal(results.get("toolu_foundry")?.is_error ?? false, false);
    const called = world.calls.filter(
      (c) => c.path === "/mcp" && c.body?.method === "tools/call",
    );
    assert.equal(called.length, 1);
    assert.equal(called[0]!.auth, "Bearer hxr_turn");
  });

  it("reaches none of the person's MCP servers or hooks", async () => {
    assert.deepEqual(
      world.calls.filter((c) => c.path === "/person-mcp"),
      [],
    );
    const { access } = await import("node:fs/promises");
    await assert.rejects(access(join(dir, "hooked")), "no hook of theirs ran");
  });

  it("is told Foundry's system prompt, and sends the model's text", () => {
    assert.ok(JSON.stringify(world.turns[0].system).includes(MARKER));
    const finals = world.calls
      .filter((c) => c.path === claimPath("text") && c.body.final)
      .map((c) => c.body);
    assert.deepEqual(finals, [
      { step: 1, text: "Let me look.", final: true },
      { step: 2, text: "You have Sales graph.", final: true },
    ]);
  });
});
