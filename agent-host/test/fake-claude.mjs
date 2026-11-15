#!/usr/bin/env node
// A stand-in for the `claude` executable, speaking the Agent SDK's
// stream-json control protocol as the CLI does, for the helper's tests: no
// login exists on the test machine. It logs what it was started with and
// sent (FAKE_CLAUDE_LOG), and plays FAKE_CLAUDE_SCENARIO:
//   answer        calls Foundry's tool at the MCP server it was given, then answers
//   two_steps     says a sentence, calls Foundry's tool, then answers in a second step
//   signed_out    fails as a CLI that isn't signed in
//   rate_limited  fails as a subscription past its usage limit
//   max_turns     stops at its turns
//   lost_session  can't resume (answers when started afresh)
//   crash         exits with an error
//   slow          writes forever, until stopped
//   no_account    starts with no subscription (doctor)
import { appendFileSync, readdirSync } from "node:fs";
import process from "node:process";
import { createInterface } from "node:readline";
import { setTimeout } from "node:timers";

const scenario = process.env.FAKE_CLAUDE_SCENARIO || "answer";
const log = (x) =>
  process.env.FAKE_CLAUDE_LOG &&
  appendFileSync(process.env.FAKE_CLAUDE_LOG, JSON.stringify(x) + "\n");
const argv = process.argv.slice(2);
if (argv[0] === "--version") {
  process.stdout.write("9.9.9 (Claude Code)\n");
  process.exit(0);
}
log({
  argv,
  cwd: process.cwd(),
  cwdEntries: readdirSync(process.cwd()),
  env: {
    ANTHROPIC_API_KEY: process.env.ANTHROPIC_API_KEY ?? null,
    ENABLE_CLAUDEAI_MCP_SERVERS:
      process.env.ENABLE_CLAUDEAI_MCP_SERVERS ?? null,
  },
});
const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const resumed = argv.find((a) => a.startsWith("--resume"));
const session = resumed ? resumed.split("=")[1] : "sess-new";
let server;
let next = 0;
const waiting = new Map();
const ask = (request) =>
  new Promise((resolve) => {
    const request_id = `cli-${++next}`;
    waiting.set(request_id, resolve);
    out({ type: "control_request", request_id, request });
  });
const delay = (ms) => new Promise((r) => setTimeout(r, ms));
// A stream event, as the CLI forwards the model's (includePartialMessages).
const event = (e) =>
  out({
    type: "stream_event",
    event: e,
    parent_tool_use_id: null,
    session_id: session,
    uuid: "u-e",
  });
// One model call writing text, streamed as the API streams it: its
// message's start, the text's block, then the whole message (as the CLI
// sends it once a block ends) and its stop.
async function say(id, chunks, more = async () => {}) {
  event({ type: "message_start", message: { id, role: "assistant" } });
  event({
    type: "content_block_start",
    index: 0,
    content_block: { type: "text", text: "" },
  });
  for (const text of chunks) {
    event({
      type: "content_block_delta",
      index: 0,
      delta: { type: "text_delta", text },
    });
    await delay(30);
  }
  event({ type: "content_block_stop", index: 0 });
  out({
    type: "assistant",
    message: {
      id,
      role: "assistant",
      content: [{ type: "text", text: chunks.join("") }],
    },
    parent_tool_use_id: null,
    session_id: session,
    uuid: "u-a",
  });
  await more();
  event({ type: "message_delta", delta: { stop_reason: "end_turn" } });
  event({ type: "message_stop" });
}
// Foundry's tool, called at the MCP server the helper registered.
async function callFoundry() {
  const res = await globalThis.fetch(server.url, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      ...server.headers,
    },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "helix_foundry_list_ontologies", arguments: {} },
    }),
  });
  log({ mcp: { status: res.status, body: await res.json() } });
}

async function turn(message) {
  log({ prompt: message.message.content });
  out({
    type: "system",
    subtype: "init",
    session_id: session,
    tools: [],
    mcp_servers: [{ name: "foundry", status: "connected" }],
    model: "claude-x",
    permissionMode: "dontAsk",
    apiKeySource: "none",
    cwd: process.cwd(),
    slash_commands: [],
    uuid: "u-init",
  });
  const result = (r) =>
    out({
      type: "result",
      duration_ms: 1,
      duration_api_ms: 1,
      num_turns: 1,
      total_cost_usd: 0,
      usage: {},
      session_id: session,
      uuid: "u-r",
      ...r,
    });
  switch (scenario) {
    case "signed_out":
      return result({
        subtype: "success",
        is_error: true,
        result: "Invalid API key · Please run /login",
      });
    case "rate_limited":
      return result({
        subtype: "success",
        is_error: true,
        result: "Claude AI usage limit reached|1900000000",
      });
    case "crash":
      process.stderr.write("segfault\n");
      process.exit(3);
      return;
    case "lost_session":
      if (resumed)
        return result({
          subtype: "error_during_execution",
          is_error: true,
          errors: [`No conversation found with session ID: ${session}`],
        });
      break;
    case "two_steps":
      // Step 1 writes a sentence, then goes on to a tool call: its input
      // streams a while before the CLI calls it.
      await say("msg_1", ["Let me ", "look."], async () => {
        event({
          type: "content_block_start",
          index: 1,
          content_block: {
            type: "tool_use",
            id: "t1",
            name: "mcp__foundry__helix_foundry_list_ontologies",
          },
        });
        await delay(150);
        event({ type: "content_block_stop", index: 1 });
        out({
          type: "assistant",
          message: {
            id: "msg_1",
            role: "assistant",
            content: [
              {
                type: "tool_use",
                id: "t1",
                name: "mcp__foundry__helix_foundry_list_ontologies",
                input: {},
              },
            ],
          },
          parent_tool_use_id: null,
          session_id: session,
          uuid: "u-a2",
        });
      });
      await callFoundry();
      out({
        type: "user",
        message: {
          role: "user",
          content: [
            { type: "tool_result", tool_use_id: "t1", content: "Sales graph" },
          ],
        },
        parent_tool_use_id: null,
        session_id: session,
        uuid: "u-u",
      });
      await say("msg_2", ["You have ", "Sales graph."]);
      return result({
        subtype: "success",
        is_error: false,
        result: "You have Sales graph.",
        num_turns: 2,
      });
    case "slow":
      for (;;) {
        out({
          type: "stream_event",
          event: {
            type: "content_block_delta",
            index: 0,
            delta: { type: "text_delta", text: "." },
          },
          parent_tool_use_id: null,
          session_id: session,
          uuid: "u-d",
        });
        await delay(20);
      }
  }
  // What a tool call outside Foundry's would be told, and one of Foundry's.
  const bash = await ask({
    subtype: "can_use_tool",
    tool_name: "Bash",
    input: { command: "cat ~/.ssh/id_rsa" },
    tool_use_id: "t1",
  });
  const foundry = await ask({
    subtype: "can_use_tool",
    tool_name: "mcp__foundry__helix_foundry_list_ontologies",
    input: {},
    tool_use_id: "t2",
  });
  log({ canUseTool: { bash, foundry } });
  await callFoundry();
  await say("msg_1", ["You have ", "Sales graph."]);
  if (scenario === "max_turns")
    return result({ subtype: "error_max_turns", is_error: true, errors: [] });
  result({
    subtype: "success",
    is_error: false,
    result: "You have Sales graph.",
  });
}

createInterface({ input: process.stdin }).on("line", (line) => {
  const m = JSON.parse(line);
  if (m.type === "control_response") {
    waiting.get(m.response.request_id)?.(m.response.response ?? m.response);
    return;
  }
  log({ in: m });
  if (m.type === "control_request") {
    let response = {};
    if (m.request.subtype === "initialize")
      response = {
        commands: [],
        models: [],
        account:
          scenario === "no_account"
            ? {}
            : { subscriptionType: "max", email: "ada@example.com" },
      };
    if (m.request.subtype === "mcp_set_servers") {
      server = m.request.servers.foundry;
      response = {
        added: Object.keys(m.request.servers),
        removed: [],
        errors: {},
      };
    }
    out({
      type: "control_response",
      response: { subtype: "success", request_id: m.request_id, response },
    });
  } else if (m.type === "user") {
    turn(m).catch((e) => {
      log({ error: e.message });
      process.exit(4);
    });
  }
});
