import { spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, describe, expect, it } from "vitest";
import { OLLAMA_MODEL } from "@/web/areas/workspace/ai-api";

// local/setup.sh's one question and the .env it writes for each answer:
// Ollama in Docker (its profile, model and digest, the GPU file where Docker
// has NVIDIA's runtime, a warning under 8 GB), Ollama on this computer,
// Claude or OpenAI with a key (the shell's offered), an OpenAI-compatible
// server, Amazon Bedrock with a region, a key and models (the shell's
// AWS_REGION and AWS_BEARER_TOKEN_BEDROCK offered), or none; without a
// terminal, none. It runs on a terminal of its own (util-linux script), with
// a docker and a curl that answer as each case needs.

const tty =
  spawnSync("script", ["--version"], { encoding: "utf8" }).stdout?.includes(
    "util-linux",
  ) ?? false;
const root = mkdtempSync(join(tmpdir(), "foundry-setup-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

const bin = join(root, "bin");
mkdirSync(bin);
for (const [name, body] of Object.entries({
  docker: `case "$*" in
  *MemTotal*) echo "\${FAKE_DOCKER_MEM:-16000000000}" ;;
  *Runtimes*) echo "\${FAKE_DOCKER_RUNTIMES:-io.containerd.runc.v2 runc }" ;;
  *) exit 1 ;;
esac`,
  // Ollama answering at FAKE_OLLAMA_AT (loopback unless set) only.
  curl: `[ -n "\${FAKE_OLLAMA_TAGS:-}" ] || exit 7
case "$*" in *"\${FAKE_OLLAMA_AT:-http://127.0.0.1:11434/}"*) ;; *) exit 7 ;; esac
echo "$FAKE_OLLAMA_TAGS"`,
})) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}

/** Runs setup.sh in a directory of its own, answering `answers`. */
function setup(
  answers: string[],
  env: Record<string, string> = {},
  terminal = true,
) {
  const dir = mkdtempSync(join(root, "run-"));
  copyFileSync("local/setup.sh", join(dir, "setup.sh"));
  const run = terminal
    ? spawnSync("script", ["-qec", `sh ${dir}/setup.sh`, "/dev/null"], {
        input: answers.map((a) => `${a}\n`).join(""),
        encoding: "utf8",
        env: {
          PATH: `${bin}:/usr/bin:/bin`,
          TERM: "dumb",
          ...env,
        } as unknown as NodeJS.ProcessEnv,
      })
    : spawnSync("sh", [join(dir, "setup.sh")], {
        input: "",
        encoding: "utf8",
        env: {
          PATH: `${bin}:/usr/bin:/bin`,
          ...env,
        } as unknown as NodeJS.ProcessEnv,
      });
  const file = join(dir, ".env");
  const text = existsSync(file) ? readFileSync(file, "utf8") : undefined;
  const vars =
    text === undefined
      ? undefined
      : Object.fromEntries(
          text
            .split("\n")
            .filter((l) => /^[A-Z0-9_]+=/.test(l))
            .map((l) => l.split(/=(.*)/).slice(0, 2) as [string, string]),
        );
  return { status: run.status, out: `${run.stdout}${run.stderr}`, vars, text };
}

const AI_KEYS = (vars?: Record<string, string>) =>
  Object.fromEntries(
    Object.entries(vars || {}).filter(([k]) =>
      /^(FOUNDRY_AI_|FOUNDRY_OLLAMA_|COMPOSE_)/.test(k),
    ),
  );
const DIGEST = /^OLLAMA_DIGEST=(.*)$/m.exec(
  readFileSync("local/setup.sh", "utf8"),
)![1];

describe.skipIf(!tty)("local/setup.sh", () => {
  it("writes the secrets and no provider without a terminal", () => {
    const { status, vars } = setup([], {}, false);
    expect(status).toBe(0);
    for (const k of [
      "FOUNDRY_LOCAL_TOKEN",
      "FOUNDRY_S3_SECRET",
      "FOUNDRY_MYSQL_PASSWORD",
    ])
      expect(vars?.[k]).toMatch(/^[0-9a-f]{64}$/);
    expect(vars?.FOUNDRY_CONNECTOR_KEY).toMatch(/^[A-Za-z0-9+/]{43}=$/);
    expect(AI_KEYS(vars)).toEqual({});
  });

  it("runs Ollama in Docker by default: its profile, model and digest", () => {
    const { status, out, vars } = setup([""]);
    expect(status, out).toBe(0);
    expect(out).toContain("Choice [1]:");
    expect(AI_KEYS(vars)).toEqual({
      COMPOSE_PROFILES: "ollama",
      FOUNDRY_OLLAMA_MODEL: OLLAMA_MODEL,
      FOUNDRY_OLLAMA_DIGEST: DIGEST,
      FOUNDRY_AI_PROVIDER: "ollama",
      FOUNDRY_AI_BASE_URL: "http://127.0.0.1:11434",
      FOUNDRY_AI_MODEL: OLLAMA_MODEL,
    });
    expect(out).not.toContain("Warning");
    expect(out).not.toContain("NVIDIA");
    expect(out).toContain("docker compose logs -f ollama-pull");
  });

  it("warns when Docker has under 10 GB for it", () => {
    // qwen3.5:4b takes 5.7 GB with drafting's 65536-token window.
    const { out, vars } = setup(["1"], { FAKE_DOCKER_MEM: "8221675520" });
    expect(out).toContain(
      "Warning: Docker has 8 GB of memory. Ollama with qwen3.5:4b and the rest of Foundry need about 10 GB",
    );
    expect(vars?.COMPOSE_PROFILES).toBe("ollama");
    // Docker Desktop's "10 GB" is a little over 10e9.
    expect(setup(["1"], { FAKE_DOCKER_MEM: "10277094400" }).out).not.toContain(
      "Warning",
    );
  });

  it("offers the NVIDIA GPUs where Docker has their runtime", () => {
    const nvidia = {
      FAKE_DOCKER_RUNTIMES: "io.containerd.runc.v2 nvidia runc ",
    };
    const yes = setup(["1", ""], nvidia);
    expect(yes.out).toContain("Run Ollama on them? [Y/n]");
    expect(yes.vars?.COMPOSE_FILE).toBe("compose.yaml:compose.gpu.yaml");
    const no = setup(["1", "n"], nvidia);
    expect(no.vars?.COMPOSE_FILE).toBeUndefined();
    expect(no.vars?.COMPOSE_PROFILES).toBe("ollama");
  });

  it("runs Ollama on this computer, saying what it lacks", () => {
    const down = setup(["2", ""]);
    expect(AI_KEYS(down.vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "ollama",
      FOUNDRY_AI_BASE_URL: "http://host.docker.internal:11434",
      FOUNDRY_AI_MODEL: OLLAMA_MODEL,
    });
    expect(down.out).toContain("Ollama isn't answering on this computer yet");
    expect(down.out).toContain(
      "On Linux, start Ollama with OLLAMA_HOST=172.17.0.1, Docker's bridge address, so Docker can reach it and your network can't.",
    );
    expect(down.out).not.toContain("0.0.0.0");
    const tags = (name: string) => ({
      FAKE_OLLAMA_TAGS: JSON.stringify({ models: [{ name, digest: "ab" }] }),
    });
    const lacking = setup(["2", "llama4"], tags("qwen3.5:4b"));
    expect(lacking.vars?.FOUNDRY_AI_MODEL).toBe("llama4");
    expect(lacking.out).toContain(
      "Ollama here has no llama4 yet: run ollama pull llama4.",
    );
    // A model without a tag is listed as :latest.
    const has = setup(["2", "llama4"], tags("llama4:latest"));
    expect(has.out).not.toContain("has no");
    expect(has.out).not.toContain("isn't answering");
    // Or on Docker's bridge address alone, as it says to on Linux.
    const bridged = setup(["2", "llama4"], {
      ...tags("llama4:latest"),
      FAKE_OLLAMA_AT: "http://172.17.0.1:11434/",
    });
    expect(bridged.out).not.toContain("isn't answering");
    expect(
      setup(["2", "llama4"], {
        ...tags("llama4:latest"),
        FAKE_OLLAMA_AT: "http://10.0.0.9:11434/",
      }).out,
    ).toContain("isn't answering");
    const bad = setup(["2", "llama 4"]);
    expect(bad.status).toBe(1);
    expect(bad.vars).toBeUndefined();
  });

  it("offers the shell's Anthropic key, and makes Claude the default then", () => {
    const key = "sk-ant-api03-setup-0123456789";
    const { out, vars, text } = setup(["", ""], { ANTHROPIC_API_KEY: key });
    expect(out).toContain("Choice [3]:");
    expect(out).toContain("Use the ANTHROPIC_API_KEY in this shell? [Y/n]");
    expect(AI_KEYS(vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "anthropic",
      FOUNDRY_AI_API_KEY: `'${key}'`,
      FOUNDRY_AI_DRAFT_MODEL: "claude-opus-5-5",
      FOUNDRY_AI_ASK_MODEL: "claude-sonnet-5-5",
      FOUNDRY_AI_EXTRACT_MODEL: "claude-haiku-5-5",
      FOUNDRY_AI_VERIFY_MODEL: "claude-sonnet-5-5",
    });
    // A switch to Bedrock by hand with FOUNDRY_AI_MODEL alone would keep
    // these Claude models, which Bedrock has no price for: .env says so.
    expect(text).toContain(
      "# Each task's model, which wins over FOUNDRY_AI_MODEL: switching provider,\n# set these to its models or delete them.\nFOUNDRY_AI_DRAFT_MODEL=claude-opus-5-5",
    );
    // Or another, typed.
    const typed = setup(["3", "n", "sk-ant-typed-1"], {
      ANTHROPIC_API_KEY: key,
    });
    expect(typed.vars?.FOUNDRY_AI_API_KEY).toBe("'sk-ant-typed-1'");
  });

  it("runs OpenAI on a typed key, and refuses one that isn't", () => {
    const { vars } = setup(["4", "sk-proj-setup-1"]);
    expect(AI_KEYS(vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "openai",
      FOUNDRY_AI_API_KEY: "'sk-proj-setup-1'",
      FOUNDRY_AI_DRAFT_MODEL: "gpt-6.1-sol",
      FOUNDRY_AI_ASK_MODEL: "gpt-6.1-sol",
      FOUNDRY_AI_EXTRACT_MODEL: "gpt-6-luna",
      FOUNDRY_AI_VERIFY_MODEL: "gpt-6.1-sol",
    });
    const bad = setup(["4", "sk proj"]);
    expect(bad.status).toBe(1);
    expect(bad.out).toContain("That isn't an API key; nothing was written.");
    expect(bad.vars).toBeUndefined();
  });

  it("runs an OpenAI-compatible server, its key optional", () => {
    const local = setup(["5", "", "", "qwen/qwen3-8b", ""]);
    expect(local.status, local.out).toBe(0);
    expect(AI_KEYS(local.vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "openai_compatible",
      FOUNDRY_AI_BASE_URL: "'http://host.docker.internal:1234/v1'",
      FOUNDRY_AI_API: "chat_completions",
      FOUNDRY_AI_MODEL: "'qwen/qwen3-8b'",
    });
    const keyed = setup([
      "5",
      "https://llm.example.com/v1",
      "2",
      "qwen3-32b",
      "sk-local-0123",
    ]);
    expect(AI_KEYS(keyed.vars)).toMatchObject({
      FOUNDRY_AI_BASE_URL: "'https://llm.example.com/v1'",
      FOUNDRY_AI_API: "responses",
      FOUNDRY_AI_API_KEY: "'sk-local-0123'",
    });
    const bad = setup(["5", "llm.example.com"]);
    expect(bad.status).toBe(1);
    expect(bad.vars).toBeUndefined();
  });

  const BEDROCK_KEY = "ABSKQmVkcm9ja0FQSUtleS10ZXN0+/0123=";

  it("writes Amazon Bedrock with a region, a key and one model for every task", () => {
    const run = setup([
      "6",
      "eu-west-1",
      BEDROCK_KEY,
      "eu.anthropic.claude-sonnet-5-5",
    ]);
    expect(run.status, run.out).toBe(0);
    expect(AI_KEYS(run.vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "bedrock",
      FOUNDRY_AI_REGION: "eu-west-1",
      FOUNDRY_AI_API_KEY: `'${BEDROCK_KEY}'`,
      FOUNDRY_AI_MODEL: "eu.anthropic.claude-sonnet-5-5",
    });
    expect(run.out).toContain(
      "Wrote .env, with Amazon Bedrock in eu-west-1 as the install's AI.",
    );
  });

  it("writes Claude through Bedrock's global profiles, one per task, without a model", () => {
    const run = setup(["6", "", BEDROCK_KEY, ""]);
    expect(run.status, run.out).toBe(0);
    // It says those may run anywhere commercial, and how a switch of
    // provider by hand must change the tasks' models.
    expect(run.out).toContain(
      "Enter for Claude's models through Bedrock's global inference profiles, one per task",
    );
    expect(run.out).toContain(
      "Wrote .env, with Amazon Bedrock in us-east-1 as the install's AI, through global inference profiles, which may run requests in any AWS commercial region.",
    );
    expect(run.text).toContain(
      "# Claude through Bedrock's global inference profiles, which may run requests\n# in any AWS commercial region; a geography's (eu.anthropic.claude-…) stay\n# within it. Each task's model wins over FOUNDRY_AI_MODEL: switching\n# provider, set these to its models or delete them.\nFOUNDRY_AI_DRAFT_MODEL=",
    );
    expect(AI_KEYS(run.vars)).toEqual({
      FOUNDRY_AI_PROVIDER: "bedrock",
      FOUNDRY_AI_REGION: "us-east-1",
      FOUNDRY_AI_API_KEY: `'${BEDROCK_KEY}'`,
      FOUNDRY_AI_DRAFT_MODEL: "global.anthropic.claude-opus-5-5",
      FOUNDRY_AI_ASK_MODEL: "global.anthropic.claude-sonnet-5-5",
      FOUNDRY_AI_EXTRACT_MODEL: "global.anthropic.claude-haiku-5-5",
      FOUNDRY_AI_VERIFY_MODEL: "global.anthropic.claude-sonnet-5-5",
    });
  });

  it("offers this shell's AWS_REGION and AWS_BEARER_TOKEN_BEDROCK", () => {
    const shell = {
      AWS_REGION: "ap-southeast-2",
      AWS_BEARER_TOKEN_BEDROCK: BEDROCK_KEY,
    };
    const run = setup(["6", "", "", "amazon.nova-pro-v1:0"], shell);
    expect(run.status, run.out).toBe(0);
    expect(run.out).toContain("AWS region [ap-southeast-2]");
    expect(AI_KEYS(run.vars)).toMatchObject({
      FOUNDRY_AI_REGION: "ap-southeast-2",
      FOUNDRY_AI_API_KEY: `'${BEDROCK_KEY}'`,
      FOUNDRY_AI_MODEL: "amazon.nova-pro-v1:0",
    });
    // Declined, the key is asked for.
    const typed = setup(["6", "us-west-2", "n", "ABSKother", ""], shell);
    expect(typed.vars?.FOUNDRY_AI_API_KEY).toBe("'ABSKother'");
  });

  it("writes nothing for a region, key or model Bedrock wouldn't take", () => {
    for (const [lines, says] of [
      [["6", "US-EAST-1"], "That isn't an AWS region"],
      [["6", "us-east-1a"], "That isn't an AWS region"],
      [["6", "us-east-1", ""], "That isn't a Bedrock API key"],
      [["6", "us-east-1", "ABSK key'"], "That isn't a Bedrock API key"],
      [
        ["6", "us-east-1", BEDROCK_KEY, "two words"],
        "That isn't a Bedrock model ID",
      ],
      [
        ["6", "us-gov-west-1", BEDROCK_KEY, ""],
        "Bedrock has no global inference profiles in us-gov-west-1: enter a model ID it offers there",
      ],
      [
        ["6", "cn-north-1", BEDROCK_KEY, ""],
        "Bedrock has no global inference profiles in cn-north-1",
      ],
    ] as const) {
      const run = setup([...lines]);
      expect(run.status, lines.join(" ")).toBe(1);
      expect(run.out, lines.join(" ")).toContain(says);
      expect(run.vars, lines.join(" ")).toBeUndefined();
    }
  });

  it("leaves AI for later, refuses another answer, and never overwrites .env", () => {
    expect(AI_KEYS(setup(["7"]).vars)).toEqual({});
    const other = setup(["9"]);
    expect(other.status).toBe(1);
    expect(other.out).toContain("Choose 1 to 7; nothing was written.");
    expect(other.vars).toBeUndefined();
    const dir = mkdtempSync(join(root, "kept-"));
    copyFileSync("local/setup.sh", join(dir, "setup.sh"));
    writeFileSync(join(dir, ".env"), "KEPT=1\n");
    const again = spawnSync("sh", [join(dir, "setup.sh")], {
      encoding: "utf8",
    });
    expect(again.stdout).toContain(".env exists; leaving it as it is.");
    expect(readFileSync(join(dir, ".env"), "utf8")).toBe("KEPT=1\n");
  });
});
