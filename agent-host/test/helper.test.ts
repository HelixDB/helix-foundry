import assert from "node:assert/strict";
import { mkdtemp, readFile, readdir, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { claudeAccount } from "../src/account.ts";
import { failureOf, promptText } from "../src/claude.ts";
import {
  configPath,
  loadPaired,
  savePaired,
  threadDir,
} from "../src/config.ts";
import { doctor } from "../src/doctor.ts";
import { findClaude } from "../src/find.ts";
import { Foundry, FoundryError } from "../src/foundry.ts";
import {
  command,
  install,
  launchdPlist,
  plan,
  systemdUnit,
} from "../src/install.ts";
import { runTurn, serve } from "../src/run.ts";
import { VERSION } from "../src/version.ts";
import { FakeFoundry, turn } from "./fake-foundry.ts";

const FAKE_CLAUDE = fileURLToPath(
  new URL("./fake-claude.mjs", import.meta.url),
);

// The SDK warns that allowedTools shadows canUseTool, kept on purpose (as the CLI does).
process.removeAllListeners("warning");

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
  dir = await mkdtemp(join(tmpdir(), "agent-host-"));
  log = join(dir, "claude.log");
  env = {
    PATH: process.env.PATH,
    HOME: dir,
    FOUNDRY_AGENT_HOST_CONFIG: join(dir, "config", "agent-host.json"),
    FOUNDRY_AGENT_HOST_STATE: join(dir, "state"),
    FAKE_CLAUDE_LOG: log,
    FAKE_CLAUDE_SCENARIO: "answer",
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

function options(over: Partial<Parameters<typeof runTurn>[1]> = {}) {
  return {
    foundry: new Foundry(foundry.url, "hxh_token"),
    claude: FAKE_CLAUDE,
    version: VERSION,
    concurrency: 1,
    env,
    deltaMs: 10,
    ...over,
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
    const a = await threadDir("thread-1", env);
    assert.equal(a, join(dir, "state", "threads", "thread-1"));
    assert.equal(
      await threadDir("thread-1", env),
      a,
      "the same every turn, so its session resumes",
    );
    await assert.rejects(threadDir("../etc", env));
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
    assert.equal(start.cwd, join(dir, "state", "threads", "thread-1"));
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

    const texts = foundry.sent("/agent-host/turns/turn-1/text");
    assert.ok(texts.length >= 2);
    assert.deepEqual(
      texts.at(-1),
      { step: 1, text: "You have Sales graph.", final: true },
      "the step's final text last",
    );
    assert.ok(texts.slice(0, -1).every((t) => !t.final && t.step === 1));
    assert.deepEqual(foundry.sent("/agent-host/turns/turn-1/finish"), [
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
    assert.deepEqual(foundry.sent("/agent-host/turns/turn-1/finish").at(-1), {
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
    assert.deepEqual(foundry.sent("/agent-host/turns/turn-1/finish"), [
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
      const [finished] = foundry.sent("/agent-host/turns/turn-1/finish");
      assert.deepEqual(
        { ...finished, steps: undefined, session_id: undefined },
        { ...end, steps: undefined, session_id: undefined },
      );
    });
  }

  it("reports a CLI that crashed or isn't there", async () => {
    env.FAKE_CLAUDE_SCENARIO = "crash";
    await runTurn(turn(), options());
    const crashed = foundry.sent("/agent-host/turns/turn-1/finish")[0];
    assert.equal(crashed.end, "failed");
    assert.equal(crashed.failure, "cli_error");

    foundry.calls = [];
    await runTurn(turn(), options({ claude: join(dir, "no-such-claude") }));
    assert.deepEqual(foundry.sent("/agent-host/turns/turn-1/finish"), [
      { end: "failed", failure: "not_installed", steps: 0 },
    ]);
  });

  it("stops at once when Foundry takes the turn back, and reports nothing", async () => {
    env.FAKE_CLAUDE_SCENARIO = "slow";
    foundry.lost.add("turn-1");
    const started = Date.now();
    const outcome = await runTurn(turn(), options({ renewMs: 50 }));
    assert.equal(outcome, "stopped: turn_lost");
    assert.ok(Date.now() - started < 10_000);
    assert.deepEqual(foundry.sent("/agent-host/turns/turn-1/finish"), []);
  });

  it("stops at the turn's wall time", async () => {
    env.FAKE_CLAUDE_SCENARIO = "slow";
    const outcome = await runTurn(
      turn({ deadline: new Date(Date.now() + 300).toISOString() }),
      options(),
    );
    assert.equal(outcome, "stopped: wall_time");
    assert.deepEqual(
      foundry.sent("/agent-host/turns/turn-1/finish"),
      [],
      "Foundry ends a turn at its wall time",
    );
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
      i < 200 && foundry.sent("/agent-host/turns/turn-1/finish").length === 0;
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
    assert.equal(foundry.sent("/agent-host/turns/turn-1/finish").length, 1);
    assert.ok(lines.some((l) => l.includes("Paired as Ada's Mac")));

    foundry.revoked = true;
    await assert.rejects(
      serve(options()),
      (e: unknown) => e instanceof FoundryError && e.code === "unauthenticated",
    );
  });

  it("says what Claude Code is signed in to, and that a turn found it signed out", async () => {
    env.FAKE_CLAUDE_SCENARIO = "signed_out";
    foundry.queue = [turn()];
    const stop = new AbortController();
    const serving = serve({ ...options(), signal: stop.signal });
    const after = () => {
      const i = foundry.calls.findIndex(
        (c) => c.path === "/agent-host/turns/turn-1/finish",
      );
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

  it("finds the person's claude", async () => {
    assert.equal(await findClaude(FAKE_CLAUDE, env), FAKE_CLAUDE);
    assert.equal(await findClaude(join(dir, "nope"), env), undefined);
    assert.equal(
      await findClaude(undefined, { ...env, FOUNDRY_CLAUDE_PATH: FAKE_CLAUDE }),
      FAKE_CLAUDE,
    );
    assert.equal(
      await findClaude(undefined, { PATH: dir, HOME: dir }),
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
