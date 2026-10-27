import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, describe, expect, it } from "vitest";

// local/compose.yaml's AI default, as Compose reads it from an .env: foundry
// refuses default settings without a provider ("foundry.ai.default has
// settings but no provider"), so an empty FOUNDRY_AI_PROVIDER must pass none
// of the others, whatever .env still sets (setup.sh's Claude models, a key).
// tests/stack/run.sh and the README switch AI off by emptying it alone, and
// run.sh's Claude empties every other kind's field, which foundry refuses
// for Claude ("foundry.ai.default.base_url is not used by anthropic").
// Needs Docker Compose, which reads the file without a daemon.

const compose =
  spawnSync("docker", ["compose", "version"], { encoding: "utf8" }).status ===
  0;
const dir = mkdtempSync(join(tmpdir(), "foundry-compose-"));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

// The secrets setup.sh writes, which every service requires.
const SECRETS = {
  FOUNDRY_LOCAL_TOKEN: "t",
  FOUNDRY_S3_SECRET: "s",
  FOUNDRY_MYSQL_PASSWORD: "m",
  FOUNDRY_CONNECTOR_KEY: "c",
};
// What setup.sh writes for Claude.
const CLAUDE = {
  FOUNDRY_AI_PROVIDER: "anthropic",
  FOUNDRY_AI_API_KEY: "sk-ant-api03-test",
  FOUNDRY_AI_DRAFT_MODEL: "claude-opus-5-5",
  FOUNDRY_AI_ASK_MODEL: "claude-sonnet-5-5",
  FOUNDRY_AI_EXTRACT_MODEL: "claude-haiku-5-5",
  FOUNDRY_AI_VERIFY_MODEL: "claude-sonnet-5-5",
};
// What setup.sh writes for an OpenAI-compatible server without a key.
const COMPATIBLE = {
  FOUNDRY_AI_PROVIDER: "openai_compatible",
  FOUNDRY_AI_BASE_URL: "'http://host.docker.internal:1234/v1'",
  FOUNDRY_AI_API: "chat_completions",
  FOUNDRY_AI_MODEL: "'qwen/qwen3-8b'",
};

/**
 * foundry's HELIX_FOUNDRY__AI__DEFAULT__* for an .env of `vars`, run from a
 * shell exporting `shell`, whose values win over the file's.
 */
function aiDefault(
  vars: Record<string, string>,
  shell: Record<string, string> = {},
) {
  const environment = composed(vars, undefined, shell).services.foundry
    .environment as Record<string, string>;
  return Object.fromEntries(
    Object.entries(environment)
      .filter(([k]) => k.startsWith("HELIX_FOUNDRY__AI__DEFAULT__"))
      .map(([k, v]) => [k.slice("HELIX_FOUNDRY__AI__DEFAULT__".length), v]),
  );
}

/**
 * The project Compose reads from `files` and an .env of `vars`, run from a
 * shell exporting `shell`.
 */
function composed(
  vars: Record<string, string>,
  files = ["local/compose.yaml"],
  shell: Record<string, string> = {},
): { services: Record<string, any>; volumes?: Record<string, unknown> } {
  const env = join(dir, `${Math.random().toString(36).slice(2)}.env`);
  writeFileSync(
    env,
    Object.entries({ ...SECRETS, ...vars })
      .map(([k, v]) => `${k}=${v}`)
      .join("\n"),
  );
  // Only this file and `shell`: neither local/.env nor this process's
  // FOUNDRY_* count.
  const run = spawnSync(
    "docker",
    [
      "compose",
      ...files.flatMap((f) => ["-f", f]),
      "--env-file",
      env,
      "config",
      "--format",
      "json",
    ],
    {
      encoding: "utf8",
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        // Where the compose plugin may be.
        ...(process.env.DOCKER_CONFIG && {
          DOCKER_CONFIG: process.env.DOCKER_CONFIG,
        }),
        ...shell,
      } as unknown as NodeJS.ProcessEnv,
    },
  );
  expect(run.status, run.stderr).toBe(0);
  return JSON.parse(run.stdout);
}

/** The FOUNDRY_AI_* tests/stack/run.sh exports, with or without models. */
function stackShell(models: boolean) {
  const block =
    /^if \[ -n "\$\{FOUNDRY_STACK_MODELS:-\}" \]; then$[\s\S]*?^fi$/m.exec(
      readFileSync("tests/stack/run.sh", "utf8"),
    )?.[0];
  expect(block, "run.sh's FOUNDRY_STACK_MODELS block").toBeDefined();
  const run = spawnSync("sh", ["-c", `${block}\nenv`], {
    encoding: "utf8",
    env: {
      PATH: process.env.PATH,
      FOUNDRY_STACK_MODELS: models ? "1" : "",
      ANTHROPIC_API_KEY: "sk-ant-api03-stack",
    } as unknown as NodeJS.ProcessEnv,
  });
  expect(run.status, run.stderr).toBe(0);
  return Object.fromEntries(
    run.stdout
      .split("\n")
      .filter((line) => line.startsWith("FOUNDRY_AI_"))
      .map((line) => line.split(/=(.*)/).slice(0, 2) as [string, string]),
  );
}

const NONE = {
  PROVIDER: "",
  API_KEY: "",
  BASE_URL: "",
  REGION: "",
  ENDPOINT: "",
  API: "",
  CONTEXT_TOKENS: "",
  MODELS__DRAFT: "",
  MODELS__ASK: "",
  MODELS__EXTRACT: "",
  MODELS__VERIFY: "",
};

describe.skipIf(!compose)("local/compose.yaml's AI default", () => {
  it("passes setup.sh's Claude as written", () => {
    expect(aiDefault(CLAUDE)).toEqual({
      ...NONE,
      PROVIDER: "anthropic",
      API_KEY: "sk-ant-api03-test",
      MODELS__DRAFT: "claude-opus-5-5",
      MODELS__ASK: "claude-sonnet-5-5",
      MODELS__EXTRACT: "claude-haiku-5-5",
      MODELS__VERIFY: "claude-sonnet-5-5",
    });
  });

  it("passes setup.sh's OpenAI-compatible server as written", () => {
    expect(aiDefault(COMPATIBLE)).toEqual({
      ...NONE,
      PROVIDER: "openai_compatible",
      BASE_URL: "http://host.docker.internal:1234/v1",
      API: "chat_completions",
      MODELS__DRAFT: "qwen/qwen3-8b",
      MODELS__ASK: "qwen/qwen3-8b",
      MODELS__EXTRACT: "qwen/qwen3-8b",
      MODELS__VERIFY: "qwen/qwen3-8b",
    });
    expect(
      aiDefault({
        FOUNDRY_AI_PROVIDER: "openai_compatible",
        FOUNDRY_AI_API_KEY: "'sk-local-0123'",
        FOUNDRY_AI_BASE_URL: "'https://llm.example.com/v1'",
        FOUNDRY_AI_API: "responses",
        FOUNDRY_AI_MODEL: "'qwen3-32b'",
      }),
    ).toMatchObject({
      API_KEY: "sk-local-0123",
      API: "responses",
    });
  });

  it("passes setup.sh's Amazon Bedrock as written", () => {
    expect(
      aiDefault({
        FOUNDRY_AI_PROVIDER: "bedrock",
        FOUNDRY_AI_REGION: "eu-west-1",
        FOUNDRY_AI_API_KEY: "ABSKdGVzdA==",
        FOUNDRY_AI_MODEL: "eu.anthropic.claude-sonnet-5-5",
      }),
    ).toEqual({
      ...NONE,
      PROVIDER: "bedrock",
      REGION: "eu-west-1",
      API_KEY: "ABSKdGVzdA==",
      MODELS__DRAFT: "eu.anthropic.claude-sonnet-5-5",
      MODELS__ASK: "eu.anthropic.claude-sonnet-5-5",
      MODELS__EXTRACT: "eu.anthropic.claude-sonnet-5-5",
      MODELS__VERIFY: "eu.anthropic.claude-sonnet-5-5",
    });
  });

  it("falls back to FOUNDRY_AI_MODEL for a task without its own", () => {
    expect(
      aiDefault({
        FOUNDRY_AI_PROVIDER: "anthropic",
        FOUNDRY_AI_API_KEY: "k",
        FOUNDRY_AI_MODEL: "claude-sonnet-5-5",
        FOUNDRY_AI_DRAFT_MODEL: "claude-opus-5-5",
      }),
    ).toMatchObject({
      MODELS__DRAFT: "claude-opus-5-5",
      MODELS__ASK: "claude-sonnet-5-5",
      MODELS__EXTRACT: "claude-sonnet-5-5",
      MODELS__VERIFY: "claude-sonnet-5-5",
    });
  });

  it("passes nothing once the provider is emptied, whatever else is set", () => {
    expect(aiDefault({ ...CLAUDE, FOUNDRY_AI_PROVIDER: "" })).toEqual(NONE);
    expect(
      aiDefault({
        FOUNDRY_AI_MODEL: "claude-sonnet-5-5",
        FOUNDRY_AI_BASE_URL: "http://host.docker.internal:11434",
        FOUNDRY_AI_REGION: "us-east-1",
        FOUNDRY_AI_ENDPOINT: "https://acme.openai.azure.com",
        FOUNDRY_AI_API: "chat_completions",
        FOUNDRY_AI_CONTEXT_TOKENS: "131072",
      }),
    ).toEqual(NONE);
  });

  it("passes run.sh's Claude alone, whatever kind local/.env holds", () => {
    // Every FOUNDRY_AI_* the AI default reads, as another kind would set it.
    const every = Object.fromEntries(
      [
        ...readFileSync("local/compose.yaml", "utf8").matchAll(
          /\$\{(FOUNDRY_AI_[A-Z_]+):-/g,
        ),
      ].map(([, name]) => [name, "x"]),
    );
    expect(Object.keys(every)).toContain("FOUNDRY_AI_BASE_URL");
    for (const env of [CLAUDE, COMPATIBLE, every]) {
      expect(aiDefault(env, stackShell(true))).toEqual({
        ...NONE,
        PROVIDER: "anthropic",
        API_KEY: "sk-ant-api03-stack",
        MODELS__DRAFT: "claude-opus-5-5",
        MODELS__ASK: "claude-sonnet-5-5",
        MODELS__EXTRACT: "claude-haiku-5-5",
        MODELS__VERIFY: "claude-sonnet-5-5",
      });
      expect(aiDefault(env, stackShell(false))).toEqual(NONE);
    }
  });

  const example = () =>
    Object.fromEntries(
      readFileSync("local/.env.example", "utf8")
        .split("\n")
        .filter((line) => /^[A-Z_]+=/.test(line))
        .map((line) => line.split(/=(.*)/).slice(0, 2) as [string, string]),
    );

  it("passes nothing for .env.example as shipped", () => {
    expect(Object.keys(example())).toContain("FOUNDRY_AI_PROVIDER");
    expect(aiDefault({ ...example(), ...SECRETS })).toEqual(NONE);
  });

  // An .env written by hand from .env.example, as its comments say: no
  // task keeps another provider's model.
  it("runs every task on FOUNDRY_AI_MODEL in an .env filled in from .env.example", () => {
    expect(
      aiDefault({
        ...example(),
        ...SECRETS,
        FOUNDRY_AI_PROVIDER: "ollama",
        FOUNDRY_AI_BASE_URL: "http://host.docker.internal:11434",
        FOUNDRY_AI_MODEL: "qwen3.5:4b",
      }),
    ).toEqual({
      ...NONE,
      PROVIDER: "ollama",
      BASE_URL: "http://host.docker.internal:11434",
      MODELS__DRAFT: "qwen3.5:4b",
      MODELS__ASK: "qwen3.5:4b",
      MODELS__EXTRACT: "qwen3.5:4b",
      MODELS__VERIFY: "qwen3.5:4b",
    });
  });
});

// The foundry service: the standalone image (Foundry's API, its directory
// and its workers in one process) in the pod's namespace, given setup.sh's
// secrets and the AI default alone, and healthy once its readiness probe
// answers on the pod's loopback.
describe.skipIf(!compose)("local/compose.yaml's foundry", () => {
  it("runs the foundry image, FOUNDRY_IMAGE's when set", () => {
    expect(composed({}).services.foundry.image).toBe(
      "ghcr.io/helixdb/foundry:0.1.0",
    );
    expect(
      composed({ FOUNDRY_IMAGE: "ghcr.io/helixdb/foundry:1.2.3" }).services
        .foundry.image,
    ).toBe("ghcr.io/helixdb/foundry:1.2.3");
  });

  it("passes the secrets and the AI default, nothing else", () => {
    const { services } = composed({});
    const foundry = services.foundry;
    const environment = foundry.environment as Record<string, string>;
    expect(
      Object.keys(environment)
        .filter((k) => !k.startsWith("HELIX_FOUNDRY__AI__DEFAULT__"))
        .sort(),
    ).toEqual([
      "HELIX_DB_DSN",
      "HELIX_FOUNDRY__CONNECTORS__CURRENT_KEY",
      "HELIX_LOCAL__S3__SECRET_ACCESS_KEY",
      "HELIX_LOCAL__SITE_TOKEN",
    ]);
    expect(environment).toMatchObject({
      HELIX_DB_DSN:
        "helix:m@tcp(mysql:3306)/helix?parseTime=true&multiStatements=true",
      HELIX_FOUNDRY__CONNECTORS__CURRENT_KEY: "c",
      HELIX_LOCAL__S3__SECRET_ACCESS_KEY: "s",
      HELIX_LOCAL__SITE_TOKEN: "t",
    });
    expect(foundry.network_mode).toBe("service:pod");
    expect(foundry.healthcheck.test).toEqual([
      "CMD",
      "curl",
      "-fsS",
      "http://127.0.0.1:8083/readyz",
    ]);
    expect(services.site.depends_on.foundry.condition).toBe("service_healthy");
    expect(services.site.environment.HELIX_WFE_ORIGIN).toBe("http://pod:8081");
  });
});

// The ollama profile (setup.sh's "Ollama in Docker"): Ollama beside the agent
// on the pod's loopback, pinned by digest, and its one-shot model pull; the
// GPU file gives it the NVIDIA GPUs.
describe.skipIf(!compose)("local/compose.yaml's ollama profile", () => {
  // What setup.sh writes for Ollama in Docker.
  const OLLAMA = {
    COMPOSE_PROFILES: "ollama",
    FOUNDRY_OLLAMA_MODEL: "qwen3.5:4b",
    FOUNDRY_OLLAMA_DIGEST: `sha256:${"ab".repeat(32)}`,
    FOUNDRY_AI_PROVIDER: "ollama",
    FOUNDRY_AI_BASE_URL: "http://127.0.0.1:11434",
    FOUNDRY_AI_MODEL: "qwen3.5:4b",
  };

  it("is off unless .env turns it on", () => {
    const services = Object.keys(composed({}).services);
    expect(services).not.toContain("ollama");
    expect(services).not.toContain("ollama-pull");
    expect(Object.keys(composed(OLLAMA).services)).toEqual(
      expect.arrayContaining(["ollama", "ollama-pull"]),
    );
  });

  it("runs Ollama on the agent's loopback, one model and call at a time", () => {
    const { services, volumes } = composed(OLLAMA);
    const ollama = services.ollama;
    expect(ollama.image).toMatch(/^ollama\/ollama:[\d.]+@sha256:[0-9a-f]{64}$/);
    expect(ollama.network_mode).toBe("service:pod");
    expect(services["ontology-agent"].network_mode).toBe("service:pod");
    expect(ollama.environment).toEqual({
      // The pod's loopback only (the image's own is 0.0.0.0): its API takes
      // no key, and the pod is on the project's network.
      OLLAMA_HOST: "127.0.0.1:11434",
      // Drafting's window, the card's default, so a call never reloads.
      OLLAMA_CONTEXT_LENGTH: "65536",
      OLLAMA_NUM_PARALLEL: "1",
      OLLAMA_MAX_LOADED_MODELS: "1",
      OLLAMA_KEEP_ALIVE: "10m",
      OLLAMA_NO_CLOUD: "1",
    });
    expect(ollama.volumes).toEqual([
      expect.objectContaining({
        type: "volume",
        source: "ollama",
        target: "/root/.ollama",
      }),
    ]);
    expect(volumes).toHaveProperty("ollama");
    expect(ollama.healthcheck.test).toEqual(["CMD", "ollama", "list"]);
    expect(ollama.oom_score_adj).toBe(500);
    // No published port: only what shares the pod's namespace reaches it.
    expect(ollama.ports).toBeUndefined();
    // The pod maps host.docker.internal for "Ollama on this computer".
    expect(services.pod.extra_hosts).toEqual([
      "host.docker.internal=host-gateway",
    ]);
  });

  it("pulls and checks .env's model once Ollama answers", () => {
    const pull = composed(OLLAMA).services["ollama-pull"];
    expect(pull.image).toBe(composed(OLLAMA).services.ollama.image);
    expect(pull.network_mode).toBe("service:pod");
    expect(pull.environment).toEqual({
      OLLAMA_HOST: "127.0.0.1:11434",
      MODEL: "qwen3.5:4b",
      DIGEST: `sha256:${"ab".repeat(32)}`,
    });
    expect(pull.depends_on.ollama.condition).toBe("service_healthy");
    expect(pull.restart).toBe("no");
    // Its default model is the install's.
    expect(
      composed({ COMPOSE_PROFILES: "ollama" }).services["ollama-pull"]
        .environment,
    ).toEqual({
      OLLAMA_HOST: "127.0.0.1:11434",
      MODEL: "qwen3.5:4b",
      DIGEST: "",
    });
  });

  it("makes it the install's AI as setup.sh writes it", () => {
    expect(aiDefault(OLLAMA)).toEqual({
      ...NONE,
      PROVIDER: "ollama",
      BASE_URL: "http://127.0.0.1:11434",
      MODELS__DRAFT: "qwen3.5:4b",
      MODELS__ASK: "qwen3.5:4b",
      MODELS__EXTRACT: "qwen3.5:4b",
      MODELS__VERIFY: "qwen3.5:4b",
    });
  });

  it("loads models with the window the install's default asks for", () => {
    const wide = { ...OLLAMA, FOUNDRY_AI_CONTEXT_TOKENS: "131072" };
    expect(aiDefault(wide)).toMatchObject({ CONTEXT_TOKENS: "131072" });
    expect(composed(wide).services.ollama.environment).toMatchObject({
      OLLAMA_CONTEXT_LENGTH: "131072",
    });
  });

  it("gives it the NVIDIA GPUs with compose.gpu.yaml", () => {
    const files = ["local/compose.yaml", "local/compose.gpu.yaml"];
    expect(
      composed(OLLAMA, files).services.ollama.deploy.resources.reservations
        .devices,
    ).toEqual([{ driver: "nvidia", count: -1, capabilities: ["gpu"] }]);
    // And nothing more, without the profile.
    expect(Object.keys(composed({}, files).services)).not.toContain("ollama");
  });
});
