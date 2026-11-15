// The helper on Codex, against test/fake-codex.mjs (a stand-in for its
// app-server) and a fake Foundry: what it starts Codex with, what it
// answers Codex's requests, and how it reports a turn. The isolation itself
// is checked against the real Codex in real-codex.test.ts.

import assert from "node:assert/strict";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import {
  CODEX_SERVER,
  MIN_CODEX,
  TOKEN_ENV,
  answer,
  atLeast,
  checkCodex,
  codexAccount,
  codexCli,
  codexConfig,
  codexFailure,
  codexSessions,
} from "../src/codex.ts";
import {
  configPath,
  forgetThread,
  savePaired,
  threadDir,
} from "../src/config.ts";
import { doctor } from "../src/doctor.ts";
import { Foundry } from "../src/foundry.ts";
import { service } from "../src/install.ts";
import { runTurn, serve, type RunOptions } from "../src/run.ts";
import { VERSION } from "../src/version.ts";
import { FakeFoundry, claimPath, turn } from "./fake-foundry.ts";

const FAKE_CODEX = fileURLToPath(new URL("./fake-codex.mjs", import.meta.url));

let foundry: FakeFoundry;
let dir: string;
let env: NodeJS.ProcessEnv;
let log: string;

before(async () => {
  foundry = await new FakeFoundry().start();
});
after(() => foundry.stop());
beforeEach(async () => {
  foundry.calls = [];
  foundry.queue = [];
  foundry.lost.clear();
  foundry.revoked = false;
  dir = await mkdtemp(join(tmpdir(), "agent-host-codex-"));
  log = join(dir, "codex.log");
  env = {
    PATH: process.env.PATH,
    HOME: dir,
    FOUNDRY_AGENT_HOST_CONFIG: join(dir, "config", "agent-host.json"),
    FOUNDRY_AGENT_HOST_STATE: join(dir, "state"),
    FAKE_CODEX_LOG: log,
    FAKE_CODEX_THREADS: join(dir, "codex-threads.json"),
    FAKE_CODEX_SCENARIO: "answer",
    // A key in the person's shell never reaches Codex: it would bill an API account.
    OPENAI_API_KEY: "sk-should-not-pass",
    // Neither CLI is found unless a test names one: not even the pinned Codex
    // on pnpm's PATH.
    FOUNDRY_CLAUDE_PATH: join(dir, "no-claude"),
    FOUNDRY_CODEX_PATH: join(dir, "no-codex"),
  };
});

async function logged(): Promise<any[]> {
  try {
    return (await readFile(log, "utf8"))
      .trim()
      .split("\n")
      .map((l) => JSON.parse(l));
  } catch {
    return [];
  }
}

function options(
  over: Partial<RunOptions> & { codex?: string } = {},
): RunOptions {
  const { codex = FAKE_CODEX, ...rest } = over;
  return {
    foundry: new Foundry(foundry.url, "hxh_token"),
    clis: [codexCli(codex, env)],
    version: VERSION,
    concurrency: 1,
    env,
    deltaMs: 10,
    ...rest,
  };
}

const codexTurn = (over: Parameters<typeof turn>[0] = {}) =>
  turn({ runtime: "codex", ...over });

describe("a turn on Codex", () => {
  it("runs on the person's Codex with Foundry's tools, and declines every command, change and other tool", async () => {
    const outcome = await runTurn(codexTurn(), options());
    assert.equal(outcome, "answered");
    const entries = await logged();
    const start = entries.find((e) => e.argv && e.argv[0] === "app-server");
    assert.deepEqual(
      start.argv,
      [
        "app-server",
        ...codexConfig(`${foundry.url}/mcp`).flatMap((c) => ["-c", c]),
      ],
      "its shell and other built-in tools off, and Foundry's MCP server",
    );
    assert.ok(
      !start.argv.some((a: string) => a.includes("hxr_")),
      "the turn's token is never in Codex's argv",
    );
    assert.equal(
      start.env.token,
      "hxr_turn",
      "it reaches Codex by environment",
    );
    assert.equal(start.env.OPENAI_API_KEY, null);
    assert.equal(start.cwd, join(dir, "state", "threads", "ws-1", "thread-1"));
    assert.deepEqual(start.cwdEntries, [], "it runs in an empty directory");

    const thread = entries.find((e) => e.received === "thread/start");
    assert.deepEqual(thread.params, {
      cwd: start.cwd,
      approvalPolicy: "untrusted",
      sandbox: "read-only",
      approvalsReviewer: "user",
      baseInstructions: "You are the Assistant in Helix Foundry.",
    });
    const asked = Object.fromEntries(
      entries
        .filter((e) => e.asked)
        .map((e) => [
          e.asked === "mcpServer/elicitation/request"
            ? `elicitation:${e.answer.action === "accept" ? CODEX_SERVER : "personal"}`
            : e.asked,
          e.answer,
        ]),
    );
    assert.deepEqual(asked["item/commandExecution/requestApproval"], {
      decision: "decline",
    });
    assert.deepEqual(asked["item/fileChange/requestApproval"], {
      decision: "decline",
    });
    assert.deepEqual(asked["item/permissions/requestApproval"], {
      permissions: {},
      scope: "turn",
    });
    assert.equal(
      asked["elicitation:personal"].action,
      "decline",
      "another server's tool call is declined",
    );
    assert.equal(
      asked[`elicitation:${CODEX_SERVER}`].action,
      "accept",
      "Foundry's runs unasked",
    );
    assert.equal(
      asked["item/tool/requestUserInput"].code,
      -32601,
      "no question goes to the person",
    );

    assert.equal(
      foundry.calls.find((c) => c.path === "/mcp")?.auth,
      "Bearer hxr_turn",
      "Codex calls Foundry with the turn's token",
    );
    const texts = foundry.sent(claimPath("text"));
    assert.deepEqual(
      texts.filter((t) => t.final),
      [
        { step: 1, text: "Let me look.", final: true },
        { step: 2, text: "You have one ontology: Sales graph.", final: true },
      ],
    );
    assert.ok(
      texts.every((t) => !t.text.includes("SUBAGENT")),
      "a sub-agent's text isn't the turn's",
    );
    const [finished] = foundry.sent(claimPath("finish"));
    assert.equal(finished.end, "answered");
    assert.equal(finished.steps, 2);
    assert.match(finished.session_id, /^codex-thread-/);
  });

  it("resumes the thread's Codex thread, and starts afresh told the thread when Codex lost it", async () => {
    await runTurn(codexTurn({ resume: "codex-thread-old" }), options());
    const resumed = (await logged()).find(
      (e) => e.received === "thread/resume",
    );
    assert.equal(resumed.params.threadId, "codex-thread-old");
    assert.equal(resumed.params.approvalPolicy, "untrusted");
    assert.equal(
      foundry.sent(claimPath("finish"))[0].session_id,
      "codex-thread-old",
    );
    const said = (await logged()).find((e) => e.received === "turn/start");
    assert.equal(said.params.input[0].text, "Which ontologies do I have?");

    foundry.calls = [];
    env.FAKE_CODEX_SCENARIO = "lost_session";
    const { writeFile } = await import("node:fs/promises");
    await writeFile(log, "");
    await runTurn(
      codexTurn({
        resume: "codex-thread-gone",
        history: "The person: Hi\n\nYou: Hello.",
      }),
      options(),
    );
    const entries = await logged();
    assert.ok(
      entries.some((e) => e.received === "thread/start"),
      "afresh",
    );
    const prompt = entries.find((e) => e.received === "turn/start").params
      .input[0].text;
    assert.ok(
      prompt.startsWith("The person: Hi"),
      "afresh, it is told the thread",
    );
    assert.match(
      foundry.sent(claimPath("finish"))[0].session_id,
      /^codex-thread-\d+$/,
    );
  });

  for (const [scenario, end] of [
    ["signed_out", { end: "failed", failure: "signed_out" }],
    ["rate_limited", { end: "failed", failure: "rate_limited" }],
    [
      "mcp_failed",
      {
        end: "failed",
        failure: "cli_error",
        detail: "Codex couldn't reach Foundry's tools",
      },
    ],
  ] as const) {
    it(`reports ${scenario} as Foundry words it`, async () => {
      env.FAKE_CODEX_SCENARIO = scenario;
      await runTurn(codexTurn(), options());
      const [finished] = foundry.sent(claimPath("finish"));
      assert.deepEqual(
        { ...finished, steps: undefined, session_id: undefined },
        { ...end, steps: undefined, session_id: undefined },
      );
    });
  }

  it("stops at its steps, and says so", async () => {
    env.FAKE_CODEX_SCENARIO = "many_steps";
    const outcome = await runTurn(codexTurn({ max_steps: 3 }), options());
    assert.equal(outcome, "stopped");
    const [finished] = foundry.sent(claimPath("finish"));
    assert.equal(finished.reason, "max_steps");
    assert.equal(finished.steps, 3);
    assert.ok((await logged()).some((e) => e.received === "turn/interrupt"));
  });

  it("reports a Codex that crashed or isn't there", async () => {
    env.FAKE_CODEX_SCENARIO = "crash";
    await runTurn(codexTurn(), options());
    const crashed = foundry.sent(claimPath("finish"))[0];
    assert.equal(crashed.end, "failed");
    assert.equal(crashed.failure, "cli_error");

    foundry.calls = [];
    await runTurn(codexTurn(), options({ codex: join(dir, "no-such-codex") }));
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "failed", failure: "not_installed", steps: 0 },
    ]);
  });

  it("stops Codex at once when Foundry takes the turn back", async () => {
    env.FAKE_CODEX_SCENARIO = "slow";
    foundry.lost.add(turn().claim_id);
    const started = Date.now();
    const outcome = await runTurn(codexTurn(), options({ renewMs: 50 }));
    assert.equal(outcome, "stopped: turn_lost");
    assert.ok(Date.now() - started < 10_000);
    assert.deepEqual(foundry.sent(claimPath("finish")), []);
  });

  it("runs a runtime the helper doesn't as not installed", async () => {
    await runTurn(turn({ runtime: "claude_code" }), options());
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "failed", failure: "not_installed", steps: 0 },
    ]);
  });
});

describe("Codex's account and sessions", () => {
  it("says the ChatGPT plan Codex is signed in on, and its version", async () => {
    assert.deepEqual(await checkCodex(FAKE_CODEX, env), {
      runtime: "codex",
      state: "signed_in",
      subscription: "plus",
      version: "0.169.0",
    });
    env.FAKE_CODEX_ACCOUNT = JSON.stringify({
      account: null,
      requiresOpenaiAuth: true,
    });
    env.FAKE_CODEX_VERSION = "0.150.2";
    assert.deepEqual(await checkCodex(FAKE_CODEX, env), {
      runtime: "codex",
      state: "signed_out",
      version: "0.150.2",
    });
    assert.deepEqual(await checkCodex(join(dir, "none"), env), {
      runtime: "codex",
      state: "unknown",
    });
    env.FAKE_CODEX_SCENARIO = "crash";
    env.FAKE_CODEX_ACCOUNT = "";
    // It starts, says its version, then dies before account/read.
    const crashed = await checkCodex(FAKE_CODEX, env);
    assert.equal(crashed.runtime, "codex");
  });

  it("tells a ChatGPT plan from another sign-in, or none", () => {
    assert.deepEqual(
      codexAccount(
        { account: { type: "chatgpt", planType: "pro" } },
        "0.170.0",
      ),
      {
        runtime: "codex",
        state: "signed_in",
        subscription: "pro",
        version: "0.170.0",
      },
    );
    assert.deepEqual(
      codexAccount({ account: { type: "chatgpt", planType: "unknown" } }),
      { runtime: "codex", state: "signed_in" },
    );
    assert.deepEqual(codexAccount({ account: { type: "apiKey" } }), {
      runtime: "codex",
      state: "signed_in",
    });
    assert.deepEqual(
      codexAccount({
        account: { type: "chatgpt", planType: "x\n".repeat(40) },
      }),
      {
        runtime: "codex",
        state: "signed_in",
        subscription: "x".repeat(32),
      },
    );
    assert.deepEqual(
      codexAccount({ account: null, requiresOpenaiAuth: true }),
      { runtime: "codex", state: "signed_out" },
    );
    assert.deepEqual(
      codexAccount({ account: null, requiresOpenaiAuth: false }),
      {
        runtime: "codex",
        state: "signed_in",
      },
      "a provider of the person's that needs no OpenAI sign-in",
    );
  });

  it("deletes Codex's threads of a thread once it is gone", async () => {
    const t = { workspaceId: "ws-1", threadId: "thread-1" };
    await runTurn(codexTurn(), options());
    const sessions = codexSessions(FAKE_CODEX, env);
    const kept = await sessions.list(await threadDir(t, env));
    assert.equal(kept.length, 1);
    await forgetThread(t, [sessions], env);
    assert.deepEqual(
      await sessions.list(join(dir, "state", "threads", "ws-1", "thread-1")),
      [],
    );
  });

  it("offers Codex with its account when it runs", async () => {
    foundry.queue = [codexTurn()];
    const stop = new AbortController();
    const serving = serve({ ...options(), signal: stop.signal });
    for (
      let i = 0;
      i < 200 && foundry.sent(claimPath("finish")).length === 0;
      i++
    )
      await new Promise((r) => setTimeout(r, 25));
    stop.abort();
    await serving;
    assert.deepEqual(foundry.sent("/agent-host/hello"), [
      {
        runtimes: ["codex"],
        version: VERSION,
        accounts: [
          {
            runtime: "codex",
            state: "signed_in",
            subscription: "plus",
            version: "0.169.0",
          },
        ],
      },
    ]);
  });

  it("says what stands in the way of Codex", async () => {
    await savePaired(
      {
        foundry: foundry.url,
        hostId: "host-1",
        name: "Ada's Mac",
        token: "hxh_token",
      },
      configPath(env),
    );
    let checks = await doctor({ codex: FAKE_CODEX, env });
    assert.deepEqual(
      checks.filter((c) => !c.ok),
      [],
      JSON.stringify(checks),
    );
    assert.ok(checks.some((c) => c.what === `Codex 0.169.0 (${FAKE_CODEX})`));
    assert.ok(
      checks.some(
        (c) => c.what === "Signed in to Codex on a ChatGPT plus plan",
      ),
    );
    assert.deepEqual(foundry.sent("/agent-host/hello").at(-1)?.runtimes, [
      "codex",
    ]);

    env.FAKE_CODEX_VERSION = "0.150.0";
    env.FAKE_CODEX_ACCOUNT = JSON.stringify({
      account: { type: "apiKey" },
      requiresOpenaiAuth: true,
    });
    checks = await doctor({ codex: FAKE_CODEX, env });
    assert.deepEqual(
      checks.filter((c) => !c.ok).map((c) => c.what),
      [
        `Codex 0.150.0 is older than ${MIN_CODEX}, the oldest Foundry runs (${FAKE_CODEX})`,
        "Signed in to Codex, but not with a ChatGPT plan",
      ],
    );
    checks = await doctor({ env });
    assert.ok(
      checks.some((c) => !c.ok && c.what === "Claude Code or Codex installed"),
    );
  });
});

describe("Codex's rules", () => {
  it("answers no to everything Codex asks but Foundry's own tool calls", () => {
    const toolCall = (serverName: string) => ({
      serverName,
      _meta: { codex_approval_kind: "mcp_tool_call" },
    });
    assert.deepEqual(answer("item/commandExecution/requestApproval", {}), {
      result: { decision: "decline" },
    });
    assert.deepEqual(answer("item/fileChange/requestApproval", {}), {
      result: { decision: "decline" },
    });
    assert.deepEqual(answer("item/permissions/requestApproval", {}), {
      result: { permissions: {}, scope: "turn" },
    });
    for (const m of ["execCommandApproval", "applyPatchApproval"])
      assert.ok(
        "denied" in
          (answer(m, {}) as { result: { decision: object } }).result.decision,
      );
    assert.equal(
      (answer("mcpServer/elicitation/request", toolCall(CODEX_SERVER)) as any)
        .result.action,
      "accept",
    );
    assert.equal(
      (answer("mcpServer/elicitation/request", toolCall("personal")) as any)
        .result.action,
      "decline",
    );
    assert.equal(
      (
        answer("mcpServer/elicitation/request", {
          serverName: CODEX_SERVER,
          mode: "url",
          _meta: null,
        }) as any
      ).result.action,
      "decline",
      "Foundry's server asks nothing else of the person",
    );
    for (const m of [
      "item/tool/requestUserInput",
      "item/tool/call",
      "account/chatgptAuthTokens/refresh",
      "attestation/generate",
    ])
      assert.equal((answer(m, {}) as any).error.code, -32601, m);
  });

  it("compares versions, a prerelease below its release", () => {
    assert.ok(atLeast("0.169.0", "0.169.0"));
    assert.ok(atLeast("0.170.1", "0.169.0"));
    assert.ok(atLeast("1.0.0", "0.169.0"));
    assert.ok(!atLeast("0.168.9", "0.169.0"));
    assert.ok(!atLeast("0.169.0-alpha.3", "0.169.0"));
    assert.ok(atLeast("0.170.0-alpha.3", "0.169.0"));
    assert.ok(!atLeast("garbage", "0.169.0"));
  });

  it("words Codex's failures", () => {
    assert.deepEqual(codexFailure({ codexErrorInfo: "usageLimitExceeded" }), {
      end: "failed",
      failure: "rate_limited",
    });
    assert.deepEqual(codexFailure({ codexErrorInfo: "unauthorized" }), {
      end: "failed",
      failure: "signed_out",
    });
    assert.deepEqual(
      codexFailure({ codexErrorInfo: "contextWindowExceeded" }),
      { end: "stopped", reason: "context" },
    );
    assert.deepEqual(
      codexFailure({
        codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 401 } },
        message: "401 Unauthorized",
      }),
      {
        end: "failed",
        failure: "signed_out",
      },
    );
    assert.deepEqual(codexFailure({ message: "boom" }), {
      end: "failed",
      failure: "cli_error",
      detail: "boom",
    });
  });

  it("names the token's variable and Foundry's server as Foundry does", async () => {
    const host = await readFile(
      new URL("../../backend/agent/host.go", import.meta.url),
      "utf8",
    );
    assert.match(host, new RegExp(`CodexMCPServer\\s*=\\s*"${CODEX_SERVER}"`));
    assert.match(
      host,
      new RegExp(
        `MinCodexVersion\\s*=\\s*"${MIN_CODEX.replaceAll(".", "\\.")}"`,
      ),
    );
    assert.ok(
      codexConfig("http://x/mcp").some((c) =>
        c.includes(`bearer_token_env_var="${TOKEN_ENV}"`),
      ),
    );
  });

  it("installs with the person's Codex and CODEX_HOME", () => {
    const s = service(
      { codex: "/opt/codex" },
      { PATH: "/bin", HOME: "/home/ada", CODEX_HOME: "/home/ada/.cx" },
    );
    assert.equal(s.codex, "/opt/codex");
    assert.equal(s.codexHome, "/home/ada/.cx");
    assert.equal(s.claude, undefined);
    assert.equal(
      service(
        { claude: "/opt/claude" },
        { PATH: "/bin", HOME: "/h", CODEX_HOME: "/x" },
      ).codexHome,
      undefined,
    );
  });
});
