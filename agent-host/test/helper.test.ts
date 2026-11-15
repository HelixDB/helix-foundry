import assert from "node:assert/strict";
import { mkdtemp, readFile, readdir, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { claudeAccount } from "../src/account.ts";
import { claudeCli, failureOf } from "../src/claude.ts";
import {
  configPath,
  forgetThread,
  listThreads,
  loadPaired,
  savePaired,
  threadDir,
  type Sessions,
} from "../src/config.ts";
import { doctor } from "../src/doctor.ts";
import { findCli } from "../src/find.ts";
import { Foundry, FoundryError } from "../src/foundry.ts";
import {
  command,
  install,
  launchdPlist,
  plan,
  systemdUnit,
} from "../src/install.ts";
import { runTurn, serve, sweep, type RunOptions } from "../src/run.ts";
import { promptText } from "../src/runtime.ts";
import { VERSION, invoke } from "../src/version.ts";
import { CLAIM, FakeFoundry, claimPath, turn } from "./fake-foundry.ts";

const FAKE_CLAUDE = fileURLToPath(
  new URL("./fake-claude.mjs", import.meta.url),
);
const FAKE_CODEX = fileURLToPath(new URL("./fake-codex.mjs", import.meta.url));

// The SDK warns that allowedTools shadows canUseTool, kept on purpose (as the CLI does).
process.removeAllListeners("warning");

let foundry: FakeFoundry;
let dir: string;
let env: NodeJS.ProcessEnv;
let log: string;
/** Claude Code's sessions, as the fake keeps them: by working directory. */
let kept: Map<string, string[]>;
const sessions: Sessions = {
  async list(dir) {
    return kept.get(dir) ?? [];
  },
  async delete(id, dir) {
    const left = (kept.get(dir) ?? []).filter((s) => s !== id);
    if (left.length === (kept.get(dir) ?? []).length)
      throw new Error(`no session ${id}`);
    kept.set(dir, left);
  },
};

before(async () => {
  foundry = await new FakeFoundry().start();
});
after(() => foundry.stop());
beforeEach(async () => {
  foundry.calls = [];
  foundry.queue = [];
  foundry.lost.clear();
  foundry.deleted.clear();
  foundry.overtime.clear();
  foundry.revoked = false;
  kept = new Map();
  dir = await mkdtemp(join(tmpdir(), "agent-host-"));
  log = join(dir, "claude.log");
  env = {
    PATH: process.env.PATH,
    HOME: dir,
    FOUNDRY_AGENT_HOST_CONFIG: join(dir, "config", "agent-host.json"),
    FOUNDRY_AGENT_HOST_STATE: join(dir, "state"),
    FAKE_CLAUDE_LOG: log,
    FAKE_CLAUDE_SCENARIO: "answer",
    // No Codex is found here unless a test names one.
    FOUNDRY_CODEX_PATH: join(dir, "no-codex"),
    // A key in the person's shell never reaches the CLI: it would bill an API account.
    ANTHROPIC_API_KEY: "sk-ant-should-not-pass",
  };
});

/** What the fake CLI logged, entry by entry. */
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

/** The loop's options: on the fake Claude Code, or the CLI named. */
function options(
  over: Partial<RunOptions> & { claude?: string } = {},
): RunOptions {
  const { claude = FAKE_CLAUDE, ...rest } = over;
  return {
    foundry: new Foundry(foundry.url, "hxh_token"),
    clis: [claudeCli(claude, env, { sessions })],
    version: VERSION,
    concurrency: 1,
    env,
    deltaMs: 10,
    ...rest,
  };
}

describe("pairing", () => {
  it("keeps the token where only this user reads it", async () => {
    const path = configPath(env);
    assert.equal(await loadPaired(path), undefined);
    const paired = await new Foundry(foundry.url).pair({
      code: "hxp_good",
      name: "Ada's Mac",
      runtimes: ["claude_code"],
      version: VERSION,
      platform: "darwin-arm64",
    });
    await savePaired(
      {
        foundry: foundry.url,
        hostId: paired.host_id,
        name: "Ada's Mac",
        token: paired.token,
      },
      path,
    );
    assert.equal((await stat(path)).mode & 0o777, 0o600);
    assert.deepEqual(await loadPaired(path), {
      foundry: foundry.url,
      hostId: "host-1",
      name: "Ada's Mac",
      token: "hxh_token",
    });
    assert.deepEqual(foundry.sent("/agent-host/pair")[0], {
      code: "hxp_good",
      name: "Ada's Mac",
      runtimes: ["claude_code"],
      version: VERSION,
      platform: "darwin-arm64",
    });

    await writeFile(
      path,
      JSON.stringify({
        foundry: "x",
        hostId: "h",
        name: "n",
        token: "site-token",
      }),
    );
    await assert.rejects(loadPaired(path), /pair again/);
  });

  it("says why Foundry refused", async () => {
    await assert.rejects(
      new Foundry(foundry.url).pair({
        code: "hxp_used",
        name: "x",
        runtimes: ["claude_code"],
        version: VERSION,
        platform: "linux-x64",
      }),
      (e: unknown) =>
        e instanceof FoundryError &&
        e.code === "pairing_invalid" &&
        e.status === 404,
    );
    await assert.rejects(
      new Foundry("http://127.0.0.1:1").hello(["claude_code"], VERSION, []),
      (e: unknown) => e instanceof FoundryError && e.code === "unreachable",
    );
  });

  it("gives each thread a directory of its own, and nothing else", async () => {
    const t = { workspaceId: "ws-1", threadId: "thread-1" };
    const a = await threadDir(t, env);
    assert.equal(a, join(dir, "state", "threads", "ws-1", "thread-1"));
    assert.equal(
      await threadDir(t, env),
      a,
      "the same every turn, so its session resumes",
    );
    await assert.rejects(
      threadDir({ workspaceId: "ws-1", threadId: "../etc" }, env),
    );
    await assert.rejects(
      threadDir({ workspaceId: "..", threadId: "thread-1" }, env),
    );
    assert.deepEqual(await listThreads(env), [t]);
  });

  it("forgets a thread: Claude Code's sessions of it, then its directory", async () => {
    assert.deepEqual(await listThreads(env), [], "none before the first turn");
    const a = { workspaceId: "ws-1", threadId: "thread-a" };
    const b = { workspaceId: "ws-2", threadId: "thread-b" };
    const dirA = await threadDir(a, env);
    const dirB = await threadDir(b, env);
    kept.set(dirA, ["s1", "s2"]);
    kept.set(dirB, ["s3"]);
    await forgetThread(a, [sessions], env);
    assert.deepEqual(kept.get(dirA), []);
    assert.deepEqual(kept.get(dirB), ["s3"], "another thread's stays");
    assert.deepEqual(await listThreads(env), [b]);
    await forgetThread(a, [sessions], env);
  });

  it("forgets a thread with every CLI one failing or not, and keeps it for the next sweep", async () => {
    const a = { workspaceId: "ws-1", threadId: "thread-a" };
    const dirA = await threadDir(a, env);
    kept.set(dirA, ["s1"]);
    const broken: Sessions = {
      list: () => Promise.reject(new Error("Codex exited (1)")),
      delete: () => Promise.resolve(),
    };
    await assert.rejects(
      forgetThread(a, [broken, sessions], env),
      /Codex exited/,
    );
    assert.deepEqual(kept.get(dirA), [], "the other CLI's sessions go");
    assert.deepEqual(
      await listThreads(env),
      [a],
      "the directory stays, so the thread is tried again",
    );
    await forgetThread(a, [sessions], env);
    assert.deepEqual(await listThreads(env), []);
  });
});

describe("a turn on Claude Code", () => {
  it("runs with Foundry's tools alone and reports what the model wrote", async () => {
    const outcome = await runTurn(turn(), options());
    assert.equal(outcome, "answered");
    const entries = await logged();
    const start = entries.find((e) => e.argv);
    // Built-in tools off, no setting sources, the person's MCP servers left out, only Foundry's allowed.
    for (const flag of [
      "--tools=",
      "--setting-sources=",
      "--strict-mcp-config",
      "--permission-mode=dontAsk",
      "--allowedTools=mcp__foundry__*",
      "--max-turns=20",
    ]) {
      assert.ok(start.argv.includes(flag), `${flag} in ${start.argv}`);
    }
    assert.ok(
      !start.argv.some((a: string) => a.includes("hxr_")),
      "the turn's token is never in the CLI's argv",
    );
    assert.ok(!start.argv.some((a: string) => a.startsWith("--resume")));
    assert.equal(start.env.ANTHROPIC_API_KEY, null);
    assert.equal(start.env.ENABLE_CLAUDEAI_MCP_SERVERS, "false");
    assert.equal(start.cwd, join(dir, "state", "threads", "ws-1", "thread-1"));
    assert.deepEqual(start.cwdEntries, [], "it runs in an empty directory");

    const init = entries.find((e) => e.in?.request?.subtype === "initialize");
    assert.deepEqual(
      init.in.request.systemPrompt,
      ["You are the Assistant in Helix Foundry."],
      "Foundry's system prompt, not Claude Code's",
    );
    const set = entries.find(
      (e) => e.in?.request?.subtype === "mcp_set_servers",
    );
    assert.deepEqual(set.in.request.servers, {
      foundry: {
        type: "http",
        url: `${foundry.url}/mcp`,
        headers: { Authorization: "Bearer hxr_turn" },
      },
    });
    const order =
      entries.findIndex((e) => e.in?.request?.subtype === "mcp_set_servers") <
      entries.findIndex((e) => e.in?.type === "user");
    assert.ok(
      order,
      "Foundry's server is registered before the message is sent",
    );
    const { canUseTool } = entries.find((e) => e.canUseTool);
    assert.equal(
      canUseTool.bash.behavior,
      "deny",
      "anything but Foundry's tools is denied",
    );
    assert.equal(canUseTool.foundry.behavior, "allow");
    assert.equal(entries.find((e) => e.mcp).mcp.status, 200);
    assert.equal(
      foundry.calls.find((c) => c.path === "/mcp")?.auth,
      "Bearer hxr_turn",
      "the CLI calls Foundry with the turn's token",
    );

    const texts = foundry.sent(claimPath("text"));
    assert.ok(texts.length >= 2);
    assert.deepEqual(
      texts.at(-1),
      { step: 1, text: "You have Sales graph.", final: true },
      "the step's final text last",
    );
    assert.ok(texts.slice(0, -1).every((t) => !t.final && t.step === 1));
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "answered", steps: 1, session_id: "sess-new" },
    ]);
  });

  it("resumes the thread's session, and starts afresh told the thread when the CLI lost it", async () => {
    await runTurn(turn({ resume: "sess-old" }), options());
    const resumed = (await logged()).find((e) => e.argv);
    assert.ok(
      resumed.argv.includes("--resume=sess-old") ||
        resumed.argv.includes("--resume"),
      `${resumed.argv}`,
    );
    assert.deepEqual(foundry.sent(claimPath("finish")).at(-1), {
      end: "answered",
      steps: 1,
      session_id: "sess-old",
    });

    foundry.calls = [];
    env.FAKE_CLAUDE_SCENARIO = "lost_session";
    await writeFile(log, "");
    await runTurn(
      turn({ resume: "sess-gone", history: "The person: Hi\n\nYou: Hello." }),
      options(),
    );
    const entries = await logged();
    const starts = entries.filter((e) => e.argv);
    assert.equal(starts.length, 2, "once resumed, once afresh");
    assert.ok(!starts[1].argv.some((a: string) => a.startsWith("--resume")));
    const prompts = entries
      .filter((e) => e.prompt !== undefined)
      .map((e) => e.prompt);
    assert.equal(prompts[0], "Which ontologies do I have?");
    assert.ok(
      prompts[1].startsWith("The person: Hi"),
      "afresh, it is told the thread",
    );
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "answered", steps: 1, session_id: "sess-new" },
    ]);
  });

  for (const [scenario, end] of [
    ["signed_out", { end: "failed", failure: "signed_out" }],
    ["rate_limited", { end: "failed", failure: "rate_limited" }],
    ["max_turns", { end: "stopped", reason: "max_steps" }],
  ] as const) {
    it(`reports ${scenario} as Foundry words it`, async () => {
      env.FAKE_CLAUDE_SCENARIO = scenario;
      await runTurn(turn(), options());
      const [finished] = foundry.sent(claimPath("finish"));
      assert.deepEqual(
        { ...finished, steps: undefined, session_id: undefined },
        { ...end, steps: undefined, session_id: undefined },
      );
    });
  }

  it("reports a CLI that crashed or isn't there", async () => {
    env.FAKE_CLAUDE_SCENARIO = "crash";
    await runTurn(turn(), options());
    const crashed = foundry.sent(claimPath("finish"))[0];
    assert.equal(crashed.end, "failed");
    assert.equal(crashed.failure, "cli_error");

    foundry.calls = [];
    await runTurn(turn(), options({ claude: join(dir, "no-such-claude") }));
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "failed", failure: "not_installed", steps: 0 },
    ]);
  });

  it("stops at once when Foundry takes the turn back, and reports nothing", async () => {
    env.FAKE_CLAUDE_SCENARIO = "slow";
    foundry.lost.add(CLAIM);
    const started = Date.now();
    const outcome = await runTurn(turn(), options({ renewMs: 50 }));
    assert.equal(outcome, "stopped: turn_lost");
    assert.ok(Date.now() - started < 10_000);
    assert.deepEqual(foundry.sent(claimPath("finish")), []);
  });

  it("stops at the turn's wall time, and has Foundry end it then", async () => {
    env.FAKE_CLAUDE_SCENARIO = "slow";
    foundry.overtime.add(CLAIM);
    const outcome = await runTurn(
      turn({ deadline: new Date(Date.now() + 300).toISOString() }),
      options(),
    );
    assert.equal(outcome, "stopped: wall_time");
    assert.deepEqual(
      foundry.sent(claimPath("finish")),
      [],
      "Foundry ends a turn at its wall time",
    );
    assert.equal(
      foundry.sent(claimPath("renew")).length,
      1,
      "a last renew past its time tells Foundry, which ends it at once",
    );
  });

  it("says a step's text before the tool call it goes on to, and each step's as its own", async () => {
    env.FAKE_CLAUDE_SCENARIO = "two_steps";
    const outcome = await runTurn(turn(), options());
    assert.equal(outcome, "answered");
    const texts = foundry.sent(claimPath("text"));
    const finals = texts.filter((t) => t.final);
    assert.deepEqual(finals, [
      { step: 1, text: "Let me look.", final: true },
      { step: 2, text: "You have Sales graph.", final: true },
    ]);
    const second = texts.findIndex((t) => t.step === 2);
    assert.ok(second > 0);
    assert.ok(
      texts.slice(0, second).every((t) => t.step === 1),
      "step 2's text is never shown as step 1's",
    );
    const at = (pred: (c: { path: string; body: any }) => boolean) =>
      foundry.calls.findIndex(pred);
    assert.ok(
      at(
        (c) =>
          c.path === claimPath("text") && c.body.final && c.body.step === 1,
      ) < at((c) => c.path === "/mcp"),
      "step 1's text is said before its tool call runs",
    );
    assert.deepEqual(foundry.sent(claimPath("finish")), [
      { end: "answered", steps: 2, session_id: "sess-new" },
    ]);
  });
});

describe("the helper", () => {
  it("claims and runs turns until stopped, and stops when revoked", async () => {
    foundry.queue = [turn()];
    const stop = new AbortController();
    const lines: string[] = [];
    const serving = serve({
      ...options(),
      signal: stop.signal,
      log: (l) => lines.push(l),
    });
    for (
      let i = 0;
      i < 200 && foundry.sent(claimPath("finish")).length === 0;
      i++
    )
      await new Promise((r) => setTimeout(r, 25));
    stop.abort();
    await serving;
    const max = [
      { runtime: "claude_code", state: "signed_in", subscription: "max" },
    ];
    assert.deepEqual(foundry.sent("/agent-host/hello"), [
      { runtimes: ["claude_code"], version: VERSION, accounts: max },
    ]);
    assert.deepEqual(foundry.sent("/agent-host/claim")[0], {
      runtimes: ["claude_code"],
      version: VERSION,
      accounts: max,
    });
    assert.equal(foundry.sent(claimPath("finish")).length, 1);
    assert.ok(lines.some((l) => l.includes("Paired as Ada's Mac")));

    // Revoked, it deletes what Claude Code kept of every thread.
    const ran = await threadDir(
      { workspaceId: "ws-1", threadId: "thread-1" },
      env,
    );
    kept.set(ran, ["sess-new"]);
    foundry.revoked = true;
    const told: string[] = [];
    await assert.rejects(
      serve({ ...options(), log: (l) => told.push(l) }),
      (e: unknown) => e instanceof FoundryError && e.code === "unauthenticated",
    );
    assert.deepEqual(kept.get(ran), []);
    assert.deepEqual(await listThreads(env), []);
    assert.ok(
      told.some((l) => l.includes("revoked") && l.includes("1 thread")),
    );
  });

  it("deletes the sessions of threads Foundry says are gone", async () => {
    const live = { workspaceId: "ws-1", threadId: "thread-live" };
    const gone = { workspaceId: "ws-1", threadId: "thread-gone" };
    for (const t of [live, gone])
      kept.set(await threadDir(t, env), ["s-" + t.threadId]);
    foundry.deleted.add("thread-gone");
    const lines: string[] = [];
    await sweep(new Foundry(foundry.url, "hxh_token"), [sessions], env, (l) =>
      lines.push(l),
    );
    assert.deepEqual(await listThreads(env), [live]);
    assert.deepEqual(foundry.sent("/agent-host/threads/gone"), [
      {
        threads: [
          { workspace_id: "ws-1", thread_id: "thread-gone" },
          { workspace_id: "ws-1", thread_id: "thread-live" },
        ],
      },
    ]);
    assert.equal(lines.length, 0);

    // Foundry unreachable: nothing is deleted, and the next sweep tries again.
    await sweep(
      new Foundry("http://127.0.0.1:1", "hxh_token"),
      [sessions],
      env,
      (l) => lines.push(l),
    );
    assert.deepEqual(await listThreads(env), [live]);
    assert.ok(lines.some((l) => l.includes("trying again later")));
    // Revoked: the sweep says so, for serve to forget every thread.
    foundry.revoked = true;
    await assert.rejects(
      sweep(new Foundry(foundry.url, "hxh_token"), [sessions], env, () => {}),
      (e: unknown) => e instanceof FoundryError && e.code === "unauthenticated",
    );
  });

  it("goes on past a thread one CLI can't delete, at a sweep and when revoked", async () => {
    const bad = { workspaceId: "ws-1", threadId: "thread-bad" };
    const good = { workspaceId: "ws-1", threadId: "thread-good" };
    const badDir = await threadDir(bad, env);
    kept.set(await threadDir(good, env), ["s-good"]);
    // One CLI's list fails for one thread (its app-server didn't start).
    const flaky: Sessions = {
      list: (d) =>
        d === badDir
          ? Promise.reject(new Error("Codex exited (1)"))
          : sessions.list(d),
      delete: (id, d) => sessions.delete(id, d),
    };
    foundry.deleted.add("thread-bad");
    foundry.deleted.add("thread-good");
    const lines: string[] = [];
    await sweep(new Foundry(foundry.url, "hxh_token"), [flaky], env, (l) =>
      lines.push(l),
    );
    assert.deepEqual(await listThreads(env), [bad], "the good one went");
    assert.deepEqual(
      kept.get(join(dir, "state", "threads", "ws-1", "thread-good")),
      [],
    );
    assert.ok(
      lines.some(
        (l) => l.includes("thread-bad") && l.includes("trying again later"),
      ),
    );

    kept.set(await threadDir(good, env), ["s-good"]);
    foundry.revoked = true;
    const told: string[] = [];
    const cli = {
      ...claudeCli(FAKE_CLAUDE, env, { sessions }),
      sessions: flaky,
    };
    await assert.rejects(
      serve({ ...options(), clis: [cli], log: (l) => told.push(l) }),
      (e: unknown) => e instanceof FoundryError && e.code === "unauthenticated",
      "the revoke is what's thrown, not the CLI's failure",
    );
    assert.deepEqual(await listThreads(env), [bad]);
    assert.ok(
      told.some(
        (l) =>
          l.includes("revoked") &&
          l.includes("of 1 thread;") &&
          l.includes("1 couldn't be deleted"),
      ),
      told.join("\n"),
    );
  });

  it("asks after at most a hundred threads a call", async () => {
    for (let i = 0; i < 150; i++)
      await threadDir(
        { workspaceId: "ws-1", threadId: `t-${String(i).padStart(3, "0")}` },
        env,
      );
    foundry.deleted.add("t-149");
    await sweep(
      new Foundry(foundry.url, "hxh_token"),
      [sessions],
      env,
      () => {},
    );
    assert.deepEqual(
      foundry.sent("/agent-host/threads/gone").map((b) => b.threads.length),
      [100, 50],
    );
    assert.equal((await listThreads(env)).length, 149);
  });

  it("prints its own commands as the person runs them, through npx", () => {
    assert.equal(
      invoke("run"),
      `npx -y @helixdb/foundry-agent-host@${VERSION} run`,
    );
  });

  it("says what Claude Code is signed in to, and that a turn found it signed out", async () => {
    env.FAKE_CLAUDE_SCENARIO = "signed_out";
    foundry.queue = [turn()];
    const stop = new AbortController();
    const serving = serve({ ...options(), signal: stop.signal });
    const after = () => {
      const i = foundry.calls.findIndex((c) => c.path === claimPath("finish"));
      return i < 0
        ? []
        : foundry.calls
            .slice(i + 1)
            .filter((c) => c.path === "/agent-host/claim");
    };
    for (let i = 0; i < 200 && !after().length; i++)
      await new Promise((r) => setTimeout(r, 25));
    stop.abort();
    await serving;
    assert.equal(
      foundry.sent("/agent-host/claim")[0].accounts[0].state,
      "signed_in",
      "it starts signed in",
    );
    assert.deepEqual(after()[0]?.body.accounts, [
      { runtime: "claude_code", state: "signed_out" },
    ]);
  });

  it("says Claude Code is signed out, and asks it again on a schedule", async () => {
    env.FAKE_CLAUDE_SCENARIO = "no_account";
    const stop = new AbortController();
    const lines: string[] = [];
    const serving = serve({
      ...options(),
      signal: stop.signal,
      checkMs: 50,
      log: (l) => lines.push(l),
    });
    for (
      let i = 0;
      i < 200 && foundry.sent("/agent-host/claim").length === 0;
      i++
    )
      await new Promise((r) => setTimeout(r, 25));
    assert.deepEqual(foundry.sent("/agent-host/hello")[0].accounts, [
      { runtime: "claude_code", state: "signed_out" },
    ]);
    assert.ok(lines.some((l) => l.includes("isn't signed in")));
    // The person signs in: the next check says so.
    env.FAKE_CLAUDE_SCENARIO = "answer";
    const signedIn = () =>
      foundry
        .sent("/agent-host/claim")
        .some((b) => b.accounts[0]?.subscription === "max");
    for (let i = 0; i < 200 && !signedIn(); i++)
      await new Promise((r) => setTimeout(r, 25));
    stop.abort();
    await serving;
    assert.ok(signedIn());
  });

  it("tells a subscription from another sign-in, or none", () => {
    assert.deepEqual(
      claudeAccount({ subscriptionType: "pro", email: "ada@example.com" }),
      { runtime: "claude_code", state: "signed_in", subscription: "pro" },
    );
    assert.deepEqual(
      claudeAccount({ subscriptionType: "x\n".repeat(40) }).state,
      "signed_in",
    );
    assert.equal(
      (
        claudeAccount({ subscriptionType: "x\n".repeat(40) }) as {
          subscription: string;
        }
      ).subscription,
      "x".repeat(32),
      "printable, at most 32 bytes",
    );
    assert.deepEqual(claudeAccount({ email: "ada@example.com" }), {
      runtime: "claude_code",
      state: "signed_in",
    });
    assert.deepEqual(claudeAccount({ tokenSource: "none" }), {
      runtime: "claude_code",
      state: "signed_out",
    });
    assert.deepEqual(claudeAccount({}), {
      runtime: "claude_code",
      state: "signed_out",
    });
  });

  it("words a CLI's failures", () => {
    assert.deepEqual(failureOf("Invalid API key · Please run /login"), {
      end: "failed",
      failure: "signed_out",
    });
    assert.deepEqual(failureOf("OAuth token has expired"), {
      end: "failed",
      failure: "signed_out",
    });
    assert.deepEqual(failureOf("Claude AI usage limit reached|1900000000"), {
      end: "failed",
      failure: "rate_limited",
    });
    assert.deepEqual(failureOf("boom"), {
      end: "failed",
      failure: "cli_error",
      detail: "boom",
    });
    assert.equal(
      promptText(turn({ history: "earlier" }), false),
      "earlier\n\n---\n\nWhich ontologies do I have?",
    );
    assert.equal(
      promptText(turn({ history: "earlier" }), true),
      "Which ontologies do I have?",
    );
  });

  it("finds the person's claude and codex", async () => {
    assert.equal(await findCli("claude", FAKE_CLAUDE, env), FAKE_CLAUDE);
    assert.equal(await findCli("claude", join(dir, "nope"), env), undefined);
    assert.equal(
      await findCli("claude", undefined, {
        ...env,
        FOUNDRY_CLAUDE_PATH: FAKE_CLAUDE,
      }),
      FAKE_CLAUDE,
    );
    assert.equal(
      await findCli("claude", undefined, { PATH: dir, HOME: dir }),
      undefined,
    );
    assert.equal(
      await findCli("codex", undefined, {
        ...env,
        FOUNDRY_CODEX_PATH: FAKE_CODEX,
      }),
      FAKE_CODEX,
    );
    assert.equal(
      await findCli("codex", undefined, { PATH: dir, HOME: dir }),
      undefined,
    );
  });

  it("is the version its package says", async () => {
    const pkg = JSON.parse(
      await readFile(new URL("../package.json", import.meta.url), "utf8"),
    );
    assert.equal(VERSION, pkg.version);
  });
});

describe("install", () => {
  const s = {
    npx: "/usr/local/bin/npx",
    claude: "/Users/ada/.local/bin/claude",
    path: "/usr/local/bin:/usr/bin:/bin",
    home: "/Users/ada",
  };
  it("keeps the pinned helper running with the person's claude", async () => {
    assert.deepEqual(command(s), [
      "/usr/local/bin/npx",
      "-y",
      `@helixdb/foundry-agent-host@${VERSION}`,
      "run",
      "--claude",
      "/Users/ada/.local/bin/claude",
    ]);
    const plist = launchdPlist({ ...s, claude: "/Users/a&b/claude" });
    assert.match(plist, /<string>\/Users\/a&amp;b\/claude<\/string>/);
    assert.match(plist, /<key>KeepAlive<\/key>\s*<true\/>/);
    const unit = systemdUnit(s);
    assert.match(
      unit,
      /^ExecStart="\/usr\/local\/bin\/npx" "-y" "@helixdb\/foundry-agent-host@0\.1\.0" "run" "--claude" "\/Users\/ada\/\.local\/bin\/claude"$/m,
    );
    assert.match(unit, /^Restart=always$/m);
    assert.match(
      unit,
      /^Environment="PATH=\/usr\/local\/bin:\/usr\/bin:\/bin"$/m,
    );
    // With Codex too: both named, and the person's CODEX_HOME kept.
    const both = {
      ...s,
      codex: "/Users/ada/.npm-global/bin/codex",
      codexHome: "/Users/ada/.codex-work",
    };
    assert.deepEqual(command(both).slice(4), [
      "--claude",
      "/Users/ada/.local/bin/claude",
      "--codex",
      "/Users/ada/.npm-global/bin/codex",
    ]);
    assert.match(
      systemdUnit(both),
      /^Environment="CODEX_HOME=\/Users\/ada\/\.codex-work"$/m,
    );
    assert.match(
      launchdPlist(both),
      /<key>CODEX_HOME<\/key>\s*<string>\/Users\/ada\/\.codex-work<\/string>/,
    );
    const codexOnly = { ...both, claude: undefined };
    assert.deepEqual(command(codexOnly).slice(4), [
      "--codex",
      "/Users/ada/.npm-global/bin/codex",
    ]);
    assert.deepEqual(plan("darwin", s, 501).commands, [
      ["launchctl", "bootout", "gui/501/com.helix-db.foundry-agent-host"],
      [
        "launchctl",
        "bootstrap",
        "gui/501",
        "/Users/ada/Library/LaunchAgents/com.helix-db.foundry-agent-host.plist",
      ],
    ]);
    assert.equal(
      plan("linux", s, 1000).file,
      "/Users/ada/.config/systemd/user/foundry-agent-host.service",
    );
    assert.throws(() => plan("win32", s, 0), /run it yourself/);
  });

  it("writes the unit and starts it, a bootout of nothing being no error", async () => {
    const ran: string[][] = [];
    const done = await install(
      { ...s, home: dir },
      {
        platform: "darwin",
        uid: 501,
        exec: async (cmd, args) => {
          ran.push([cmd, ...args]);
          if (args[0] === "bootout") throw new Error("not loaded");
        },
      },
    );
    assert.equal(await readFile(done.file, "utf8"), done.body);
    assert.equal(ran.length, 2);
    await assert.rejects(
      install(
        { ...s, home: dir },
        {
          platform: "linux",
          uid: 1000,
          exec: async () => {
            throw new Error("no systemd");
          },
        },
      ),
      /no systemd/,
    );
    const dry = await install(
      { ...s, home: join(dir, "dry") },
      { platform: "linux", dryRun: true },
    );
    await assert.rejects(readdir(join(dir, "dry")), "a dry run writes nothing");
    assert.match(dry.body, /ExecStart=/);
  });
});

describe("doctor", () => {
  it("says what stands in the way", async () => {
    let checks = await doctor({ claude: FAKE_CLAUDE, env });
    assert.ok(
      checks.some((c) => !c.ok && c.what.startsWith("Paired with Foundry")),
    );
    await savePaired(
      {
        foundry: foundry.url,
        hostId: "host-1",
        name: "Ada's Mac",
        token: "hxh_token",
      },
      configPath(env),
    );
    checks = await doctor({ claude: FAKE_CLAUDE, env });
    assert.deepEqual(
      checks.filter((c) => !c.ok),
      [],
      JSON.stringify(checks),
    );
    assert.ok(
      checks.some((c) => c.what.includes("knows this computer as Ada's Mac")),
    );
    assert.ok(
      checks.some(
        (c) =>
          c.what === "Claude Code 9.9.9 (Claude Code) (" + FAKE_CLAUDE + ")",
      ),
    );
    assert.deepEqual(foundry.sent("/agent-host/hello").at(-1)?.accounts, [
      { runtime: "claude_code", state: "signed_in", subscription: "max" },
    ]);
    assert.ok(
      checks.some((c) =>
        c.what.startsWith(
          "Signed in to a Claude max subscription as ada@example.com",
        ),
      ),
    );

    env.FAKE_CLAUDE_SCENARIO = "no_account";
    foundry.revoked = true;
    checks = await doctor({ claude: FAKE_CLAUDE, env });
    assert.deepEqual(
      checks.filter((c) => !c.ok).map((c) => c.what),
      [`Foundry at ${foundry.url}`, "Signed in to a Claude subscription"],
    );
    checks = await doctor({ claude: join(dir, "none"), env });
    assert.equal(checks.at(-1)?.what, "Claude Code installed");
  });
});
