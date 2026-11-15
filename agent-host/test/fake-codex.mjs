#!/usr/bin/env node
// A stand-in for the `codex` executable's app-server, speaking its stdio
// JSON-RPC as Codex 0.169.0 does, for the helper's tests: no login exists
// on the test machine. It logs what it was started with, sent and told
// (FAKE_CODEX_LOG), keeps its threads by working directory in
// FAKE_CODEX_THREADS, and plays FAKE_CODEX_SCENARIO:
//   answer        says a sentence, calls Foundry's tool at the MCP server it was configured with, asks to run a
//                 command, change a file and call the person's own tool and Foundry's, then answers in a second step
//   signed_out    fails as a Codex that isn't signed in
//   rate_limited  fails as a plan past its usage limit
//   lost_session  can't resume (answers when started afresh)
//   mcp_failed    can't start Foundry's MCP server
//   crash         exits after initialize
//   slow          writes forever, until stopped
//   many_steps    calls a tool forever, until interrupted
// account/read answers FAKE_CODEX_ACCOUNT (JSON), --version FAKE_CODEX_VERSION.
import {
  appendFileSync,
  readFileSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import process from "node:process";
import { createInterface } from "node:readline";
import { setTimeout } from "node:timers";

const scenario = process.env.FAKE_CODEX_SCENARIO || "answer";
const version = process.env.FAKE_CODEX_VERSION || "0.169.0";
const log = (x) =>
  process.env.FAKE_CODEX_LOG &&
  appendFileSync(process.env.FAKE_CODEX_LOG, JSON.stringify(x) + "\n");
const argv = process.argv.slice(2);
if (argv[0] === "--version") {
  process.stdout.write(`codex-cli ${version}\n`);
  process.exit(0);
}
const config = argv.filter((_, i) => argv[i - 1] === "-c");
const server = config.find((c) => c.startsWith("mcp_servers."));
const url = server && /url="([^"]+)"/.exec(server)?.[1];
const tokenEnv = server && /bearer_token_env_var="([^"]+)"/.exec(server)?.[1];
log({
  argv,
  cwd: process.cwd(),
  cwdEntries: readdirSync(process.cwd()),
  env: {
    OPENAI_API_KEY: process.env.OPENAI_API_KEY ?? null,
    token: tokenEnv ? (process.env[tokenEnv] ?? null) : null,
    HELIX_FOUNDRY_TURN_TOKEN: process.env.HELIX_FOUNDRY_TURN_TOKEN ?? null,
  },
});
const threads = () => {
  try {
    return JSON.parse(readFileSync(process.env.FAKE_CODEX_THREADS, "utf8"));
  } catch {
    return {};
  }
};
const keep = (t) =>
  process.env.FAKE_CODEX_THREADS &&
  writeFileSync(process.env.FAKE_CODEX_THREADS, JSON.stringify(t));

const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const note = (method, params) => out({ method, params });
const delay = (ms) => new Promise((r) => setTimeout(r, ms));
let next = 0;
const waiting = new Map();
const ask = (method, params) =>
  new Promise((resolve) => {
    const id = `srv-${++next}`;
    waiting.set(id, (m) => {
      log({ asked: method, answer: m.result ?? m.error });
      resolve(m);
    });
    out({ id, method, params });
  });
let interrupted = false;

async function mcpCall(name) {
  const res = await globalThis.fetch(url, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      authorization: `Bearer ${process.env[tokenEnv]}`,
    },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name, arguments: {} },
    }),
  });
  return res.json();
}

async function play(threadId, turnId) {
  const item = (type, id, extra = {}) => ({ type, id, ...extra });
  const started = (i) => note("item/started", { threadId, turnId, item: i });
  const completed = (i) =>
    note("item/completed", { threadId, turnId, item: i });
  const text = async (id, words) => {
    started(item("agentMessage", id, { text: "" }));
    for (const w of words) {
      note("item/agentMessage/delta", {
        threadId,
        turnId,
        itemId: id,
        delta: w,
      });
      await delay(15);
    }
    completed(item("agentMessage", id, { text: words.join("") }));
  };
  const done = (status, error = null) =>
    note("turn/completed", {
      threadId,
      turn: { id: turnId, status, error, items: [] },
    });
  switch (scenario) {
    case "signed_out":
      return done("failed", {
        message: "unexpected status 401 Unauthorized",
        codexErrorInfo: "unauthorized",
      });
    case "rate_limited":
      return done("failed", {
        message: "You've hit your usage limit.",
        codexErrorInfo: "usageLimitExceeded",
      });
    case "slow":
      started(item("agentMessage", "m1"));
      for (;;) {
        note("item/agentMessage/delta", {
          threadId,
          turnId,
          itemId: "m1",
          delta: "more ",
        });
        await delay(20);
      }
    case "many_steps":
      for (let i = 0; !interrupted; i++) {
        const call = item("mcpToolCall", `t${i}`, {
          server: "foundry_assistant_turn",
          tool: "get_context",
        });
        started(call);
        completed({ ...call, status: "completed" });
        await delay(5);
      }
      return done("interrupted");
  }
  await text("m1", ["Let me ", "look."]);
  const call = item("mcpToolCall", "t1", {
    server: "foundry_assistant_turn",
    tool: "helix_foundry_list_ontologies",
  });
  started(call);
  const result = await mcpCall("helix_foundry_list_ontologies");
  log({ mcp: result });
  completed({ ...call, status: "completed", result });
  // What the model may ask for, and the helper must refuse (but Foundry's own tool).
  await ask("item/commandExecution/requestApproval", {
    threadId,
    turnId,
    itemId: "c1",
    command: "touch /tmp/x",
  });
  await ask("item/fileChange/requestApproval", {
    threadId,
    turnId,
    itemId: "f1",
  });
  await ask("item/permissions/requestApproval", {
    threadId,
    turnId,
    itemId: "p1",
    permissions: {},
  });
  await ask("mcpServer/elicitation/request", {
    threadId,
    turnId,
    serverName: "personal",
    mode: "form",
    _meta: { codex_approval_kind: "mcp_tool_call" },
    message: "Allow?",
    requestedSchema: { type: "object", properties: {} },
  });
  await ask("mcpServer/elicitation/request", {
    threadId,
    turnId,
    serverName: "foundry_assistant_turn",
    mode: "form",
    _meta: { codex_approval_kind: "mcp_tool_call" },
    message: "Allow?",
    requestedSchema: { type: "object", properties: {} },
  });
  await ask("item/tool/requestUserInput", {
    threadId,
    turnId,
    itemId: "q1",
    questions: [],
  });
  // A sub-agent's text is its own: the helper shows the turn's thread only.
  note("item/agentMessage/delta", {
    threadId: "sub-agent",
    turnId: "sub",
    itemId: "s1",
    delta: "SUBAGENT",
  });
  await text("m2", ["You have ", "one ontology: ", "Sales graph."]);
  done("completed");
}

const rl = createInterface({ input: process.stdin });
rl.on("line", async (line) => {
  const m = JSON.parse(line);
  if (m.id !== undefined && !m.method) {
    waiting.get(m.id)?.(m);
    waiting.delete(m.id);
    return;
  }
  log({ received: m.method, params: m.params });
  const reply = (result) => out({ id: m.id, result });
  const fail = (message) => out({ id: m.id, error: { code: -32600, message } });
  switch (m.method) {
    case "initialize":
      if (scenario === "crash") setTimeout(() => process.exit(3), 20);
      return reply({
        userAgent: `helix_foundry_agent_host/${version} (Linux; x86_64) node`,
        codexHome: "/home/x/.codex",
        platformFamily: "unix",
        platformOs: "linux",
      });
    case "initialized":
      return;
    case "account/read":
      return reply(
        JSON.parse(
          process.env.FAKE_CODEX_ACCOUNT ||
            '{"account":{"type":"chatgpt","email":"ada@example.com","planType":"plus"},"requiresOpenaiAuth":true}',
        ),
      );
    case "thread/resume":
      if (scenario === "lost_session")
        return fail(`no rollout found for thread id ${m.params.threadId}`);
    // falls through
    case "thread/start": {
      const id = m.params.threadId ?? `codex-thread-${process.pid}`;
      const t = threads();
      t[m.params.cwd] = [...new Set([...(t[m.params.cwd] ?? []), id])];
      keep(t);
      reply({ thread: { id, cwd: m.params.cwd } });
      await delay(10);
      note("mcpServer/startupStatus/updated", {
        threadId: id,
        name: "personal",
        status: "ready",
      });
      note("mcpServer/startupStatus/updated", {
        threadId: id,
        name: "foundry_assistant_turn",
        status: scenario === "mcp_failed" ? "failed" : "ready",
      });
      return;
    }
    case "turn/start": {
      const turnId = `turn-${process.pid}`;
      reply({ turn: { id: turnId, status: "inProgress", items: [] } });
      await delay(10);
      return play(m.params.threadId, turnId);
    }
    case "turn/interrupt":
      interrupted = true;
      return reply({});
    case "thread/list":
      return reply({
        data: (threads()[m.params.cwd] ?? []).map((id) => ({ id })),
        nextCursor: null,
      });
    case "thread/delete": {
      const t = threads();
      for (const k of Object.keys(t))
        t[k] = t[k].filter((id) => id !== m.params.threadId);
      keep(t);
      return reply({});
    }
  }
  fail(`unknown method ${m.method}`);
});
