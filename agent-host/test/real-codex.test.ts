// The isolation the helper promises, checked against the real Codex it
// pins (`codex app-server`, the binary REAL_CODEX names: 0.169.0 when this
// was written), not a stand-in. Codex talks to a stand-in for OpenAI's
// Responses API (the person's config.toml sets openai_base_url to it), and
// is signed in by `codex login --with-api-key` with a fake key in a
// throwaway CODEX_HOME: no ChatGPT login exists on the test machine, so a
// ChatGPT plan's account/read is the fake app-server's (codex.test.ts).
// The stand-in records each request and plays the model.
//
// The person's own configuration is planted in CODEX_HOME: approval_policy
// never, a full-access sandbox, an auto reviewer, the shell and exec tools
// (and, for the sub-agent case, sub-agents) on, an exec rule that allows
// `touch`, an MCP server of their own with a tool that writes (and one that
// reads), and a server named `foundry`. What the helper's overrides and
// answers hold, turn by turn:
//   - no shell or exec tool is offered (so nothing, "known safe" or not,
//     runs unasked), nor an image viewer, web search or sub-agent tool;
//   - Foundry's tools, whatever Foundry's MCP server lists, are called with
//     the turn's token, unasked, and their results reach the model;
//   - apply_patch asks, is declined, and writes nothing, in the thread's
//     directory or out of it; the person's writing tool asks, is declined,
//     and isn't called (their reading tool, which asks nothing, runs: their
//     own setup, which Settings › AI models discloses);
//   - the turn's token is in no argument Codex is started with, nor in
//     anything Codex writes under CODEX_HOME;
//   - a thread's next turn resumes its Codex thread, and the helper deletes
//     the thread's Codex threads;
//   - a managed configuration that outranks the helper's (where /etc/codex
//     is writable) makes the turn fail before the model is called.
// It runs with REAL_CODEX set, and skips otherwise.

import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { constants } from "node:fs";
import {
  access,
  chmod,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { createServer, type IncomingMessage, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, describe, it } from "node:test";
import { promisify } from "node:util";
import { zstdDecompressSync } from "node:zlib";

import {
  CODEX_SERVER,
  checkCodex,
  checkLocked,
  codexCli,
  codexSessions,
} from "../src/codex.ts";
import { forgetThread } from "../src/config.ts";
import { Foundry } from "../src/foundry.ts";
import { runTurn } from "../src/run.ts";
import { VERSION } from "../src/version.ts";
import { claimPath, turn } from "./fake-foundry.ts";

const REAL = process.env.REAL_CODEX;
const MARKER = "You are the Assistant in Helix Foundry, under test.";
const TOKEN = "hxr_turn";

/** The tools Foundry's MCP server lists: whatever the Assistant has (the helper names none). */
const FOUNDRY_TOOLS = [
  {
    name: "helix_foundry_list_ontologies",
    annotations: { readOnlyHint: true },
  },
  // One that prepares a change: still Foundry's, still unasked (the person runs its card).
  {
    name: "helix_foundry_prepare_operation",
    annotations: { readOnlyHint: false, destructiveHint: true },
  },
];

async function read(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  let raw = Buffer.concat(chunks);
  if (req.headers["content-encoding"] === "zstd")
    raw = Buffer.from(zstdDecompressSync(raw));
  return raw.toString();
}

/** Every tool name a model request offers, nested ones (code mode's exec declarations) included. */
function offered(body: any): string[] {
  const names = (ts: any[] = [], pre = ""): string[] =>
    ts.flatMap((t) => {
      if (Array.isArray(t.tools)) return names(t.tools, `${pre}${t.name}/`);
      const nested =
        t.name === "exec" && typeof t.description === "string"
          ? [...t.description.matchAll(/^### `([^`]+)`/gm)].map(
              (m) => `exec:${m[1]}`,
            )
          : [];
      return [`${pre}${t.name ?? t.type}`, ...nested];
    });
  return [
    ...names(body.tools),
    ...(body.input ?? [])
      .filter((x: any) => x.type === "additional_tools")
      .flatMap((x: any) => names(x.tools)),
  ];
}

/** A reply as the Responses API streams it. */
function sse(i: number, item: object): string {
  const ev = (o: any) => `event: ${o.type}\ndata: ${JSON.stringify(o)}\n\n`;
  return (
    ev({ type: "response.created", response: { id: `resp_${i}` } }) +
    ev({ type: "response.output_item.done", item, output_index: 0 }) +
    ev({
      type: "response.completed",
      response: {
        id: `resp_${i}`,
        usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 },
      },
    })
  );
}

/** OpenAI's Responses API, Foundry's integration listener and its MCP server, in one stand-in. */
class World {
  server: Server;
  url = "";
  /** The model's requests, in order. */
  requests: any[] = [];
  /** Foundry's MCP calls, with their bearer. */
  mcp: { auth: string; method: string; params: any }[] = [];
  /** What the helper told Foundry, by path. */
  told: { path: string; body: any }[] = [];
  /** The model's replies, in order: a reply past the last answers "Done." */
  script: object[] = [];
  /** Plays the model instead of script, when set: its reply to a request, in its own time. */
  reply?: (body: any, i: number) => Promise<object>;

  constructor() {
    this.server = createServer(async (req, res) => {
      const text = await read(req);
      const body = text ? JSON.parse(text) : undefined;
      const path = (req.url ?? "").split("?")[0]!;
      const json = (status: number, out: unknown) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify(out));
      };
      if (path === "/v1/responses") {
        // Codex tries a websocket first: refused, it streams over HTTP.
        if (!body) return (res.writeHead(426), res.end());
        const i = this.requests.push(body) - 1;
        const item = this.reply
          ? await this.reply(body, i)
          : (this.script[i] ?? {
              type: "message",
              id: `m_${i}`,
              role: "assistant",
              content: [{ type: "output_text", text: "Done." }],
            });
        res.writeHead(200, { "content-type": "text/event-stream" });
        return res.end(sse(i, item));
      }
      if (path === "/mcp") {
        const auth = req.headers.authorization ?? "";
        this.mcp.push({ auth, method: body?.method, params: body?.params });
        if (auth !== `Bearer ${TOKEN}`)
          return json(401, { error: { code: "unauthenticated" } });
        if (body?.id === undefined) return (res.writeHead(202), res.end());
        const result =
          body.method === "initialize"
            ? {
                protocolVersion: body.params.protocolVersion,
                capabilities: { tools: {} },
                serverInfo: { name: "foundry", version: "1" },
              }
            : body.method === "tools/list"
              ? {
                  tools: FOUNDRY_TOOLS.map((t) => ({
                    ...t,
                    description: `Foundry's ${t.name}.`,
                    inputSchema: { type: "object", properties: {} },
                  })),
                }
              : body.method === "tools/call"
                ? {
                    content: [
                      {
                        type: "text",
                        text: `FOUNDRY ANSWERED ${body.params.name}`,
                      },
                    ],
                  }
                : {};
        return json(200, { jsonrpc: "2.0", id: body.id, result });
      }
      if (path.startsWith("/agent-host/")) {
        this.told.push({ path, body });
        if (path === claimPath("renew"))
          return json(200, {
            lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
          });
        return (res.writeHead(204), res.end());
      }
      json(404, {});
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

  /** The output a call's id got, as the model was next told it. */
  output(callId: string): string {
    for (const r of this.requests)
      for (const x of r.input ?? [])
        if (x.call_id === callId && /output/.test(x.type))
          return typeof x.output === "string"
            ? x.output
            : (x.output as { text?: string }[])
                .map((o) => o.text ?? "")
                .join("\n");
    return "";
  }
}

const exec = promisify(execFile);

/** Every file under dir, as text, with its path. */
async function everything(
  dir: string,
): Promise<{ path: string; text: string }[]> {
  const out: { path: string; text: string }[] = [];
  for (const e of await readdir(dir, {
    withFileTypes: true,
    recursive: true,
  })) {
    if (!e.isFile()) continue;
    const path = join(e.parentPath, e.name);
    out.push({ path, text: (await readFile(path)).toString("latin1") });
  }
  return out;
}

const exists = (p: string) =>
  stat(p).then(
    () => true,
    () => false,
  );

/**
 * A person's computer: a throwaway HOME and CODEX_HOME signed in with a
 * fake key, their own configuration (model as given) and MCP server, and a
 * wrapper that records the arguments Codex is started with.
 */
async function computer(world: World, model?: string) {
  const home = await mkdtemp(join(tmpdir(), "real-codex-"));
  const codexHome = join(home, ".codex");
  await mkdir(join(codexHome, "rules"), { recursive: true });
  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH,
    HOME: home,
    CODEX_HOME: codexHome,
    FOUNDRY_AGENT_HOST_STATE: join(home, "state"),
  };
  const login = exec(REAL!, ["login", "--with-api-key"], { env });
  login.child.stdin!.end("sk-test-not-a-real-key\n");
  await login;
  const person = join(home, "person-mcp.mjs");
  await writeFile(
    person,
    `import { writeFileSync } from "node:fs";
import { createInterface } from "node:readline";
const out = (m) => process.stdout.write(JSON.stringify(m) + "\\n");
writeFileSync(${JSON.stringify(join(home, "person-env.json"))}, JSON.stringify(process.env));
createInterface({ input: process.stdin }).on("line", (l) => {
  const m = JSON.parse(l);
  if (m.id === undefined) return;
  const result =
    m.method === "initialize" ? { protocolVersion: m.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: "personal", version: "1" } }
    : m.method === "tools/list" ? { tools: [
        { name: "write_note", description: "Writes a note.", inputSchema: { type: "object", properties: {} }, annotations: { readOnlyHint: false, destructiveHint: true } },
        { name: "read_note", description: "Reads a note.", inputSchema: { type: "object", properties: {} }, annotations: { readOnlyHint: true } } ] }
    : m.method === "tools/call" ? (writeFileSync(${JSON.stringify(home)} + "/person-called-" + m.params.name, "1"), { content: [{ type: "text", text: "ok" }] })
    : {};
  out({ jsonrpc: "2.0", id: m.id, result });
});
`,
  );
  await writeFile(
    join(codexHome, "config.toml"),
    `openai_base_url = "${world.url}/v1"
${model ? `model = "${model}"\n` : ""}approval_policy = "never"
sandbox_mode = "danger-full-access"
approvals_reviewer = "auto_review"

[features]
shell_tool = true
unified_exec = true

[mcp_servers.personal]
command = "node"
args = [${JSON.stringify(person)}]

[mcp_servers.foundry]
command = "node"
args = [${JSON.stringify(person)}]
`,
  );
  await writeFile(
    join(codexHome, "rules", "default.rules"),
    `prefix_rule(pattern = ["touch"], decision = "allow")\n`,
  );
  const argv = join(home, "argv.log");
  const wrapper = join(home, "codex");
  await writeFile(
    wrapper,
    `#!/bin/sh\nprintf '%s\\n' "$@" >> ${JSON.stringify(argv)}\nexec ${JSON.stringify(REAL)} "$@"\n`,
  );
  await chmod(wrapper, 0o755);
  return { home, codexHome, env, argv, wrapper };
}

describe("the helper on the real Codex (code mode)", { skip: !REAL }, () => {
  let world: World;
  let pc: Awaited<ReturnType<typeof computer>>;
  const outcomes: string[] = [];
  const lines: string[] = [];
  let threadDirPath = "";
  before(async () => {
    world = await new World().start();
    pc = await computer(world);
    threadDirPath = join(pc.home, "state", "threads", "ws-1", "thread-1");
    const outside = join(pc.home, "pwned-outside");
    world.script = [
      {
        type: "custom_tool_call",
        id: "ct_0",
        call_id: "call_try",
        name: "exec",
        input: `const out = {};
const t = async (k, f) => { try { out[k] = await f(); } catch (e) { out[k] = "threw: " + e; } };
await t("foundry", () => tools.mcp__${CODEX_SERVER}__helix_foundry_list_ontologies({}));
await t("prepare", () => tools.mcp__${CODEX_SERVER}__helix_foundry_prepare_operation({}));
out.exec_command = typeof tools.exec_command;
out.write_stdin = typeof tools.write_stdin;
out.view_image = typeof tools.view_image;
await t("patchOutside", () => tools.apply_patch("*** Begin Patch\\n*** Add File: ${outside}\\n+pwned\\n*** End Patch\\n"));
await t("patchInside", () => tools.apply_patch("*** Begin Patch\\n*** Add File: inside.txt\\n+pwned\\n*** End Patch\\n"));
await t("personWrite", () => tools.mcp__personal__write_note({}));
await t("personRead", () => tools.mcp__personal__read_note({}));
text(JSON.stringify(out));`,
      },
      {
        type: "message",
        id: "m_1",
        role: "assistant",
        content: [{ type: "output_text", text: "You have Sales graph." }],
      },
    ];
    const foundry = new Foundry(world.url, "hxh_token");
    const run = (t: ReturnType<typeof turn>) =>
      runTurn(t, {
        foundry,
        clis: [codexCli(pc.wrapper, pc.env)],
        version: VERSION,
        concurrency: 1,
        env: pc.env,
        log: (l) => lines.push(l),
      });
    outcomes.push(
      await run(turn({ runtime: "codex", system: MARKER, mcp_token: TOKEN })),
    );
    const first = world.told.find((t) => t.path === claimPath("finish"))!.body;
    world.script.push({
      type: "message",
      id: "m_2",
      role: "assistant",
      content: [{ type: "output_text", text: "Still Sales graph." }],
    });
    outcomes.push(
      await run(
        turn({
          runtime: "codex",
          system: MARKER,
          mcp_token: TOKEN,
          resume: first.session_id,
          prompt: "And again?",
        }),
      ),
    );
  });
  after(() => world?.stop());

  it("answers both turns, the second on the first's Codex thread", () => {
    assert.deepEqual(outcomes, ["answered", "answered"]);
    const finishes = world.told
      .filter((t) => t.path === claimPath("finish"))
      .map((t) => t.body);
    assert.equal(finishes.length, 2);
    assert.equal(
      finishes[1].session_id,
      finishes[0].session_id,
      "resumed, not started afresh",
    );
    const last = JSON.stringify(world.requests.at(-1).input);
    assert.ok(
      last.includes("Which ontologies do I have?") &&
        last.includes("And again?"),
      "the model reads the thread so far",
    );
    const finals = world.told
      .filter((t) => t.path === claimPath("text") && t.body.final)
      .map((t) => t.body.text);
    assert.deepEqual(finals, ["You have Sales graph.", "Still Sales graph."]);
  });

  it("runs on Foundry's system prompt", () => {
    const first = JSON.stringify(world.requests[0]);
    assert.ok(first.includes(MARKER));
    assert.ok(
      !first.includes("You are Codex"),
      "Codex's own base prompt is replaced",
    );
  });

  it("offers no shell, exec, image viewer or web search, whatever the person's config", () => {
    for (const r of world.requests) {
      const tools = offered(r);
      for (const banned of [
        "exec_command",
        "write_stdin",
        "shell",
        "unified_exec",
        "local_shell",
        "view_image",
        "web_search",
        "image_generation",
      ])
        assert.ok(
          !tools.some(
            (t) =>
              t === banned ||
              t.endsWith(`/${banned}`) ||
              t === `exec:${banned}`,
          ),
          `${banned} offered: ${tools}`,
        );
    }
    const out = JSON.parse(
      world.output("call_try").match(/\{"foundry".*\}/s)![0],
    );
    assert.equal(out.exec_command, "undefined");
    assert.equal(out.write_stdin, "undefined");
    assert.equal(out.view_image, "undefined");
  });

  it("calls Foundry's tools, whatever it lists, with the turn's token, unasked", () => {
    const calls = world.mcp.filter((c) => c.method === "tools/call");
    assert.deepEqual(
      calls.map((c) => c.params.name),
      FOUNDRY_TOOLS.map((t) => t.name),
    );
    assert.ok(world.mcp.every((c) => c.auth === `Bearer ${TOKEN}`));
    const out = world.output("call_try");
    assert.match(out, /FOUNDRY ANSWERED helix_foundry_list_ontologies/);
    assert.match(out, /FOUNDRY ANSWERED helix_foundry_prepare_operation/);
  });

  it("declines apply_patch and the person's writing tool, and nothing is written", async () => {
    const out = JSON.parse(
      world.output("call_try").match(/\{"foundry".*\}/s)![0],
    );
    assert.match(out.patchOutside, /rejected/);
    assert.match(out.patchInside, /rejected/);
    assert.match(JSON.stringify(out.personWrite), /rejected/);
    assert.ok(!(await exists(join(pc.home, "pwned-outside"))));
    assert.deepEqual(
      await readdir(threadDirPath),
      [],
      "the thread's directory stays empty",
    );
    assert.ok(
      !(await exists(join(pc.home, "person-called-write_note"))),
      "the person's writing tool never ran",
    );
    // Their reading tool asks nothing, so it runs: their own setup, disclosed in Settings.
    assert.ok(await exists(join(pc.home, "person-called-read_note")));
  });

  it("never puts the turn's token in an argument, a log or anything Codex keeps", async () => {
    const argv = await readFile(pc.argv, "utf8");
    assert.ok(argv.includes("app-server"));
    assert.ok(!argv.includes(TOKEN), "Codex's arguments");
    assert.ok(!lines.some((l) => l.includes(TOKEN)), "the helper's log");
    const kept = await everything(pc.codexHome);
    assert.ok(kept.length > 0);
    const leaks = kept.filter((f) => f.text.includes(TOKEN)).map((f) => f.path);
    assert.deepEqual(leaks, [], "anything under CODEX_HOME");
    // The person's own MCP server gets Codex's default environment, not the token.
    const personEnv = await readFile(join(pc.home, "person-env.json"), "utf8");
    assert.ok(!personEnv.includes(TOKEN));
  });

  it("says Codex's account and version, and deletes the thread's Codex threads", async () => {
    const account = await checkCodex(pc.wrapper, pc.env);
    assert.deepEqual(account, {
      runtime: "codex",
      state: "signed_in",
      version: account.version,
    });
    assert.match(account.version ?? "", /^\d+\.\d+\.\d+/);
    assert.deepEqual(
      await checkLocked(pc.wrapper, pc.env),
      [],
      "over the person's own config.toml, everything the helper sets holds",
    );
    const sessions = codexSessions(pc.wrapper, pc.env);
    const kept = await sessions.list(threadDirPath);
    assert.equal(kept.length, 1);
    await forgetThread(
      { workspaceId: "ws-1", threadId: "thread-1" },
      [sessions],
      pc.env,
    );
    assert.deepEqual(await sessions.list(threadDirPath), []);
    assert.ok(!(await exists(threadDirPath)));
  });
});

describe("the helper on the real Codex (direct tools)", { skip: !REAL }, () => {
  let world: World;
  let pc: Awaited<ReturnType<typeof computer>>;
  let outcome = "";
  before(async () => {
    world = await new World().start();
    // A model of the catalog's without code mode: tools are offered directly.
    pc = await computer(world, "gpt-5.5");
    world.script = [
      {
        type: "custom_tool_call",
        id: "ct_0",
        call_id: "call_patch",
        name: "apply_patch",
        input: `*** Begin Patch\n*** Add File: ${join(pc.home, "pwned-direct")}\n+x\n*** End Patch\n`,
      },
      {
        type: "function_call",
        id: "fc_1",
        call_id: "call_shell",
        name: "shell",
        arguments: JSON.stringify({
          command: ["touch", join(pc.home, "pwned-shell")],
        }),
      },
      {
        type: "function_call",
        id: "fc_2",
        call_id: "call_exec",
        name: "exec_command",
        arguments: JSON.stringify({
          cmd: `touch ${join(pc.home, "pwned-exec")}`,
        }),
      },
      {
        type: "message",
        id: "m_3",
        role: "assistant",
        content: [{ type: "output_text", text: "Done." }],
      },
    ];
    outcome = await runTurn(
      turn({ runtime: "codex", system: MARKER, mcp_token: TOKEN }),
      {
        foundry: new Foundry(world.url, "hxh_token"),
        clis: [codexCli(pc.wrapper, pc.env)],
        version: VERSION,
        concurrency: 1,
        env: pc.env,
      },
    );
  });
  after(() => world?.stop());

  it("offers no shell, and declines apply_patch", async () => {
    assert.equal(outcome, "answered");
    for (const r of world.requests) {
      const tools = offered(r);
      for (const banned of [
        "shell",
        "exec_command",
        "write_stdin",
        "view_image",
        "web_search",
      ])
        assert.ok(!tools.includes(banned), `${banned} offered: ${tools}`);
    }
    assert.match(world.output("call_patch"), /rejected/);
    assert.match(world.output("call_shell"), /unsupported/);
    assert.match(world.output("call_exec"), /unsupported/);
    for (const f of ["pwned-direct", "pwned-shell", "pwned-exec"])
      assert.ok(!(await exists(join(pc.home, f))), f);
  });
});

describe("the helper on the real Codex (sub-agents)", { skip: !REAL }, () => {
  for (const model of [undefined, "gpt-5.5"]) {
    describe(model ? "direct tools" : "code mode", () => {
      let world: World;
      let pc: Awaited<ReturnType<typeof computer>>;
      let outcome = "";
      before(async () => {
        world = await new World().start();
        pc = await computer(world, model);
        // The person's own config turns sub-agents on: the helper's overrides win.
        await writeFile(
          join(pc.codexHome, "config.toml"),
          (await readFile(join(pc.codexHome, "config.toml"), "utf8")).replace(
            "[features]\n",
            "[features]\nmulti_agent = true\nmulti_agent_v2 = true\n",
          ) + "\n[agents]\nenabled = true\n",
        );
        world.script = [
          {
            type: "function_call",
            id: "fc_0",
            call_id: "call_spawn",
            name: "spawn_agent",
            namespace: "collaboration",
            arguments: JSON.stringify({
              task_name: "pwn",
              message: "SUBAGENT TASK: write a file",
            }),
          },
          {
            type: "message",
            id: "m_1",
            role: "assistant",
            content: [{ type: "output_text", text: "Done." }],
          },
        ];
        outcome = await runTurn(
          turn({ runtime: "codex", system: MARKER, mcp_token: TOKEN }),
          {
            foundry: new Foundry(world.url, "hxh_token"),
            clis: [codexCli(pc.wrapper, pc.env)],
            version: VERSION,
            concurrency: 1,
            env: pc.env,
          },
        );
      });
      after(() => world?.stop());

      it("offers no sub-agent tool, and starts none when the model asks", () => {
        assert.equal(outcome, "answered");
        for (const r of world.requests) {
          const tools = offered(r);
          assert.ok(
            !tools.some((t) =>
              /spawn_agent|send_input|wait_agent|close_agent|resume_agent|collaboration/.test(
                t,
              ),
            ),
            `a sub-agent tool offered: ${tools}`,
          );
        }
        assert.match(world.output("call_spawn"), /unsupported/);
        assert.equal(
          world.requests.filter(
            (r) =>
              JSON.stringify(r.input ?? []).includes("SUBAGENT TASK") &&
              (r.input ?? []).some((x: any) => x.type === "agent_message"),
          ).length,
          0,
          "no sub-agent ran",
        );
      });
    });
  }
});

// A managed configuration (/etc/codex/managed_config.toml) outranks the
// helper's -c overrides and its thread settings alike. The helper reads
// back what Codex will run with and runs nothing unless it holds. Writing
// /etc/codex takes a machine of the test's own (CI's, or the EC2 box's):
// it runs where /etc/codex is writable, and skips otherwise.
const MANAGED = "/etc/codex/managed_config.toml";
const managedWritable = await access("/etc/codex", constants.W_OK).then(
  () => true,
  () => false,
);

describe(
  "the helper on the real Codex (a managed configuration)",
  { skip: !REAL || !managedWritable },
  () => {
    let world: World;
    let pc: Awaited<ReturnType<typeof computer>>;
    let before_: string | undefined;
    const run = () =>
      runTurn(turn({ runtime: "codex", system: MARKER, mcp_token: TOKEN }), {
        foundry: new Foundry(world.url, "hxh_token"),
        clis: [codexCli(pc.wrapper, pc.env)],
        version: VERSION,
        concurrency: 1,
        env: pc.env,
      });
    const finish = () =>
      world.told.filter((t) => t.path === claimPath("finish")).at(-1)!.body;
    before(async () => {
      before_ = await readFile(MANAGED, "utf8").catch(() => undefined);
      world = await new World().start();
      pc = await computer(world);
    });
    after(async () => {
      await (before_ === undefined
        ? rm(MANAGED, { force: true })
        : writeFile(MANAGED, before_));
      await world?.stop();
    });

    it("runs nothing when it turns the shell or sub-agents back on", async () => {
      await writeFile(
        MANAGED,
        "[features]\nshell_tool = true\nunified_exec = true\n[agents]\nenabled = true\n",
      );
      assert.equal(await run(), "failed: cli_error");
      const end = finish();
      for (const k of [
        "features.shell_tool",
        "features.unified_exec",
        "agents.enabled",
      ])
        assert.ok(end.detail.includes(k), end.detail);
      assert.equal(world.requests.length, 0, "the model is never called");
      assert.deepEqual(await checkLocked(pc.wrapper, pc.env), [
        "features.shell_tool",
        "features.unified_exec",
        "agents.enabled",
      ]);
    });

    it("runs nothing when it sets the approval policy over the thread's", async () => {
      await writeFile(MANAGED, 'approval_policy = "never"\n');
      assert.equal(await run(), "failed: cli_error");
      assert.match(finish().detail, /approval_policy/);
      assert.equal(world.requests.length, 0, "the model is never called");
      assert.deepEqual(await checkLocked(pc.wrapper, pc.env), [
        "approval_policy",
      ]);
    });
  },
);
