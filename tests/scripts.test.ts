// Runs scripts/backup.sh, restore.sh and upgrade.sh against a fake docker
// (tests/fixtures/fake-docker.sh) in a throwaway copy of the repository.
import { afterEach, describe, expect, it } from "vitest";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  symlinkSync,
  truncateSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { gunzipSync } from "node:zlib";
import { SCHEMA_VERSION } from "../apps/api/src/store.js";

const repo = fileURLToPath(new URL("..", import.meta.url));
const KEY = "a1".repeat(32);
const INTERNAL = "b2".repeat(32);
const SECRET = "dummy-neon-client-secret";
const ENV = [
  `ENCRYPTION_KEY=${KEY}`,
  `INTERNAL_KEY=${INTERNAL}`,
  "PUBLIC_PORT=3105",
  "PUBLIC_ORIGIN=http://localhost:3105",
  "NEON_CLIENT_ID=neon-id",
  `NEON_CLIENT_SECRET=${SECRET}`,
  "",
].join("\n");
const PASSPHRASE = "correct horse battery staple";
const sha256 = (v: string) => createHash("sha256").update(v).digest("hex");
const fingerprint = (key: string) =>
  sha256("helix-foundry-key-fingerprint:" + key);
const pinned = readFileSync(join(repo, "compose.yaml"), "utf8").match(
  /^ {4}image: (ghcr\.io\/helixdb\/helixdb:v(\d+\.\d+\.\d+)\S*)$/m,
)!;
const image = (version: string) =>
  `ghcr.io/helixdb/helixdb:v${version}@sha256:${"0".repeat(64)}`;
const mode = (path: string) => statSync(path).mode & 0o777;
const field = (dir: string, name: string) =>
  readFileSync(join(dir, "manifest.txt"), "utf8").match(
    new RegExp(`^${name}=(.*)$`, "m"),
  )?.[1];
const VERSION = "version --format {{.Server.Version}}";
const RESTORE_CALLS = [
  VERSION,
  "compose stop app worker executor helix",
  "run --rm -i -v helix-foundry_helix-data:/data alpine:3.22 sh -c find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data",
  "run --rm -i -v helix-foundry_foundry-data:/data alpine:3.22 sh -c find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data",
  "compose up -d --build helix executor app worker",
];

const bases: string[] = [];
afterEach(() => {
  for (const base of bases.splice(0)) rmSync(base, { recursive: true });
});

function fixture() {
  const base = mkdtempSync(join(tmpdir(), "hf-scripts-"));
  bases.push(base);
  const root = join(base, "install");
  const [home, bin, restored] = ["home", "bin", "restored"].map((d) =>
    join(base, d),
  );
  for (const dir of [home, bin, restored, join(root, "scripts")])
    mkdirSync(dir, { recursive: true });
  mkdirSync(join(root, "apps/api/src"), { recursive: true });
  for (const file of [
    "scripts/backup.sh",
    "scripts/backup-lib.sh",
    "scripts/restore.sh",
    "scripts/upgrade.sh",
    "compose.yaml",
    ".gitignore",
    "apps/api/src/store.ts",
  ])
    copyFileSync(join(repo, file), join(root, file));
  writeFileSync(join(root, ".env"), ENV, { mode: 0o600 });
  copyFileSync(
    join(repo, "tests/fixtures/fake-docker.sh"),
    join(bin, "docker"),
  );
  chmodSync(join(bin, "docker"), 0o755);
  const log = join(base, "docker.log");
  writeFileSync(log, "");
  const env = {
    PATH: `${bin}:${process.env.PATH}`,
    HOME: home,
    LANG: "C",
    STUB_LOG: log,
    STUB_RESTORED: restored,
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_NAME: "Test",
    GIT_AUTHOR_EMAIL: "test@example.com",
    GIT_COMMITTER_NAME: "Test",
    GIT_COMMITTER_EMAIL: "test@example.com",
  };
  return {
    base,
    root,
    home,
    restored,
    // flags: ["-x"] traces the script, as someone debugging it would.
    run: (
      script: string,
      args: string[] = [],
      extra = {},
      flags: string[] = [],
    ) =>
      spawnSync("bash", [...flags, join(root, "scripts", script), ...args], {
        cwd: base,
        env: { ...env, ...extra },
        encoding: "utf8",
        input: "",
      }),
    // Runs a bash command line, for process substitution; $1... are args.
    shell: (command: string, args: string[] = [], extra = {}) =>
      spawnSync("bash", ["-c", command, "bash", ...args], {
        cwd: base,
        env: { ...env, ...extra },
        encoding: "utf8",
        input: "",
      }),
    git: (cwd: string, ...args: string[]) => {
      const r = spawnSync("git", args, { cwd, env, encoding: "utf8" });
      if (r.status) throw new Error(r.stderr);
      return r.stdout.trim();
    },
    calls: () => readFileSync(log, "utf8").split("\n").filter(Boolean),
    clearCalls: () => writeFileSync(log, ""),
    env: () => readFileSync(join(root, ".env"), "utf8"),
    // Pins another HelixDB image in this checkout's compose.yaml.
    pin: (helix: string) => {
      const compose = join(root, "compose.yaml");
      writeFileSync(
        compose,
        readFileSync(compose, "utf8").replace(
          /^( {4}image: )ghcr\.io\/helixdb\/helixdb:\S+$/m,
          `$1${helix}`,
        ),
      );
    },
  };
}
type Fixture = ReturnType<typeof fixture>;

function backup(f: Fixture, args: string[] = [], extra = {}) {
  const dir = join(f.base, "backup-" + Math.random().toString(36).slice(2));
  const r = f.run("backup.sh", [...args, dir], extra);
  expect(r.status, r.stderr).toBe(0);
  f.clearCalls();
  return dir;
}

// Every byte a backup holds, with the archives decompressed.
const contents = (dir: string) =>
  readdirSync(dir)
    .map((name) => {
      const data = readFileSync(join(dir, name));
      return (name.endsWith(".tgz") ? gunzipSync(data) : data).toString(
        "latin1",
      );
    })
    .join("\n");

it.each(["backup.sh", "backup-lib.sh", "restore.sh", "upgrade.sh"])(
  "%s parses",
  (script) => {
    const r = spawnSync("bash", ["-n", join(repo, "scripts", script)], {
      encoding: "utf8",
    });
    expect(r.status, r.stderr).toBe(0);
  },
);

describe("backup.sh", () => {
  it("leaves the keys out and records a fingerprint and the HelixDB image", () => {
    const f = fixture();
    const dir = join(f.base, "backup");
    const r = f.run("backup.sh", [dir]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("Keep a separate copy of .env");
    expect(readdirSync(dir).sort()).toEqual([
      "foundry-data.tgz",
      "helix-data.tgz",
      "manifest.txt",
      "settings.env",
    ]);
    const all = contents(dir);
    expect(all).toContain("placeholder helix-data");
    // sha256(ENCRYPTION_KEY) is the key the app encrypts credentials with.
    for (const secret of [KEY, INTERNAL, SECRET, sha256(KEY)])
      expect(all).not.toContain(secret);
    expect(field(dir, "key_fingerprint")).toBe(fingerprint(KEY));
    expect(field(dir, "key_file")).toBe("none");
    expect(field(dir, "helix_image")).toBe(pinned[1]);
    expect(field(dir, "helix_image_running")).toBe("");
    expect(field(dir, "app_schema")).toBe(String(SCHEMA_VERSION));
    expect(field(dir, "app_commit")).toBe("unknown");
    expect(readFileSync(join(dir, "settings.env"), "utf8")).toBe(
      "PUBLIC_PORT=3105\nPUBLIC_ORIGIN=http://localhost:3105\nNEON_CLIENT_ID=neon-id\n",
    );
    expect(mode(dir)).toBe(0o700);
    for (const name of readdirSync(dir))
      expect(mode(join(dir, name))).toBe(0o600);
    expect(f.calls()).toEqual([
      VERSION,
      "volume inspect helix-foundry_helix-data",
      "volume inspect helix-foundry_foundry-data",
      "compose ps -a -q helix",
      "compose ps --services --status running",
      "compose stop app worker executor helix",
      "run --rm -v helix-foundry_helix-data:/data:ro alpine:3.22 tar czf - -C /data .",
      "run --rm -v helix-foundry_foundry-data:/data:ro alpine:3.22 tar czf - -C /data .",
      "compose start helix executor app worker",
    ]);
  });

  it("records the image of the running HelixDB container", () => {
    const f = fixture();
    const dir = backup(f, [], { STUB_HELIX_IMAGE: image("0.0.5") });
    expect(field(dir, "helix_image_running")).toBe(image("0.0.5"));
  });

  it("reads a quoted key and CRLF line endings, as Compose does", () => {
    const f = fixture();
    writeFileSync(
      join(f.root, ".env"),
      ENV.replace(`=${KEY}`, `="${KEY}"`).replaceAll("\n", "\r\n"),
    );
    expect(field(backup(f), "key_fingerprint")).toBe(fingerprint(KEY));
  });

  it("refuses a folder inside the repository, or one in use", () => {
    const f = fixture();
    symlinkSync(f.root, join(f.base, "link"));
    const inside = [f.root, join(f.base, "link")];
    // Another letter case names the same folder on a case-insensitive disk.
    if (existsSync(join(f.base, "INSTALL")))
      inside.push(join(f.base, "INSTALL"));
    for (const path of inside) {
      const r = f.run("backup.sh", [join(path, "backups", "today")]);
      expect(r.status, path).toBe(1);
      expect(r.stderr).toContain("outside the repository");
    }
    expect(existsSync(join(f.root, "backups"))).toBe(false);
    const used = join(f.base, "used");
    mkdirSync(used);
    writeFileSync(join(used, "notes.txt"), "");
    const r = f.run("backup.sh", [used]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("is not empty");
    expect(f.calls()).toEqual([]);
  });

  it("says when Docker is not running", () => {
    const f = fixture();
    const r = f.run("backup.sh", [join(f.base, "backup")], { STUB_DOWN: "1" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(
      "Docker is not available: Cannot connect to the Docker daemon",
    );
    expect(r.stderr).not.toContain("does not exist");
    expect(existsSync(join(f.base, "backup"))).toBe(false);
    expect(f.calls()).toEqual([VERSION]);
  });

  it("starts again only the services that were running", () => {
    const f = fixture();
    let r = f.run("backup.sh", [join(f.base, "stopped")], { STUB_RUNNING: "" });
    expect(r.status, r.stderr).toBe(0);
    expect(existsSync(join(f.base, "stopped", "manifest.txt"))).toBe(true);
    expect(f.calls().some((c) => c.startsWith("compose start"))).toBe(false);
    f.clearCalls();
    r = f.run("backup.sh", [join(f.base, "partly")], {
      STUB_RUNNING: "worker helix",
    });
    expect(r.status, r.stderr).toBe(0);
    expect(f.calls().at(-1)).toBe("compose start helix worker");
  });

  it("keeps a complete backup when the services do not start again", () => {
    const f = fixture();
    const dir = join(f.base, "backup");
    const r = f.run("backup.sh", [dir], { STUB_FAIL: "compose start*" });
    expect(r.status).toBe(3);
    expect(r.stderr).toContain(
      "the backup is complete, but docker compose start",
    );
    expect(readdirSync(dir).sort()).toEqual([
      "foundry-data.tgz",
      "helix-data.tgz",
      "manifest.txt",
      "settings.env",
    ]);
  });

  it("restarts the services when archiving fails", () => {
    const f = fixture();
    const r = f.run("backup.sh", [join(f.base, "backup")], {
      STUB_FAIL: "run *",
    });
    expect(r.status).not.toBe(0);
    expect(f.calls().at(-1)).toBe("compose start helix executor app worker");
    expect(existsSync(join(f.base, "backup"))).toBe(false);
  });

  it("restarts the services when stopping them fails partway", () => {
    const f = fixture();
    const r = f.run("backup.sh", [join(f.base, "backup")], {
      STUB_FAIL: "compose stop*",
    });
    expect(r.status).toBe(1);
    expect(f.calls().slice(-2)).toEqual([
      "compose stop app worker executor helix",
      "compose start helix executor app worker",
    ]);
    expect(existsSync(join(f.base, "backup"))).toBe(false);
    // A folder the user made stays.
    mkdirSync(join(f.base, "made"));
    f.run("backup.sh", [join(f.base, "made")], { STUB_FAIL: "run *" });
    expect(readdirSync(join(f.base, "made"))).toEqual([]);
  });

  it("reads export, spaces and inline comments in .env, as Compose does", () => {
    const f = fixture();
    writeFileSync(
      join(f.root, ".env"),
      [
        `export ENCRYPTION_KEY=${KEY} # the key  `,
        `  INTERNAL_KEY = '${INTERNAL}'`,
        "export PUBLIC_PORT=3105",
        "",
      ].join("\n"),
    );
    const dir = backup(f);
    expect(field(dir, "key_fingerprint")).toBe(fingerprint(KEY));
    expect(readFileSync(join(dir, "settings.env"), "utf8")).toBe(
      "export PUBLIC_PORT=3105\n",
    );
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("Keeping .env");
  });

  it("keeps BACKUP_PASSPHRASE from the commands it runs", () => {
    const f = fixture();
    for (const args of [[], ["--include-key"]]) {
      const dir = join(f.base, "backup" + args.length);
      const r = f.run("backup.sh", [...args, dir], {
        BACKUP_PASSPHRASE: PASSPHRASE,
      });
      expect(r.status, r.stderr).toBe(0);
    }
    expect(f.calls()).toContain("compose start helix executor app worker");
    expect(f.calls()).not.toContain(
      "a backup passphrase is in the environment",
    );
  });

  it("encrypts .env with --include-key and a passphrase", () => {
    const f = fixture();
    let r = f.run("backup.sh", ["--include-key", join(f.base, "a")]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("Set BACKUP_PASSPHRASE");
    r = f.run("backup.sh", ["--include-key", join(f.base, "b")], {
      BACKUP_PASSPHRASE: "short",
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("at least 12 characters");
    expect(f.calls()).toEqual([]);
    const dir = backup(f, ["--include-key"], { BACKUP_PASSPHRASE: PASSPHRASE });
    const encrypted = readFileSync(join(dir, "env.enc"));
    expect(encrypted.subarray(0, 8).toString()).toBe("Salted__");
    expect(mode(join(dir, "env.enc"))).toBe(0o600);
    for (const secret of [KEY, INTERNAL, SECRET, sha256(KEY)])
      expect(contents(dir)).not.toContain(secret);
    expect(field(dir, "key_file")).toBe("env.enc");
    expect(field(dir, "key_iterations")).toBe("600000");
  });

  it("prints no key or passphrase when traced with bash -x", () => {
    const f = fixture();
    const r = f.run(
      "backup.sh",
      ["--include-key", join(f.base, "backup")],
      { BACKUP_PASSPHRASE: PASSPHRASE },
      ["-x"],
    );
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toMatch(/^\+ umask 077$/m);
    for (const secret of [KEY, INTERNAL, SECRET, PASSPHRASE])
      expect(r.stdout + r.stderr).not.toContain(secret);
  });
});

describe("restore.sh", () => {
  it("keeps a matching .env and replaces both volumes", () => {
    const f = fixture();
    const dir = backup(f);
    expect(f.run("restore.sh", [dir]).stderr).toContain("Pass --replace");
    // A kept .env that others could read becomes private.
    chmodSync(join(f.root, ".env"), 0o644);
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("Keeping .env");
    expect(f.env()).toBe(ENV);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
    for (const volume of ["helix-data", "foundry-data"])
      expect(readFileSync(join(f.restored, volume + ".tgz"))).toEqual(
        readFileSync(join(dir, volume + ".tgz")),
      );
    expect(f.calls()).toEqual(RESTORE_CALLS);
  });

  it("refuses another key unless --env-file names the kept .env", () => {
    const f = fixture();
    const dir = backup(f);
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    chmodSync(join(f.root, ".env"), 0o644);
    let r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("Pass --env-file");
    expect(r.stderr).toContain("Nothing was changed.");
    expect(f.calls()).toEqual([]);
    expect(f.env()).toBe(other);
    writeFileSync(join(f.base, "wrong.env"), other);
    r = f.run("restore.sh", [
      dir,
      "--replace",
      "--env-file",
      join(f.base, "wrong.env"),
    ]);
    expect(r.status).toBe(1);
    expect(f.calls()).toEqual([]);
    writeFileSync(join(f.base, "kept.env"), ENV);
    r = f.run("restore.sh", [
      dir,
      "--replace",
      "--env-file",
      join(f.base, "kept.env"),
    ]);
    expect(r.status, r.stderr).toBe(0);
    expect(f.env()).toBe(ENV);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
    const saved = readdirSync(f.root).filter((n) =>
      n.startsWith(".env.pre-restore-"),
    );
    expect(saved).toHaveLength(1);
    expect(readFileSync(join(f.root, saved[0]), "utf8")).toBe(other);
    expect(mode(join(f.root, saved[0]))).toBe(0o600);
    expect(r.stdout).toContain("delete it once the restore is confirmed");
    expect(f.calls()).toEqual(RESTORE_CALLS);
  });

  it("reads --env-file from a pipe, as from a password manager", () => {
    const f = fixture();
    const dir = backup(f);
    writeFileSync(join(f.root, ".env"), ENV.replace(KEY, "c3".repeat(32)));
    writeFileSync(join(f.base, "kept.env"), ENV);
    const r = f.shell('bash "$1" "$2" --replace --env-file <(cat "$3")', [
      join(f.root, "scripts", "restore.sh"),
      dir,
      join(f.base, "kept.env"),
    ]);
    expect(r.status, r.stderr).toBe(0);
    expect(f.env()).toBe(ENV);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
    expect(
      readdirSync(f.root).filter((n) => /^\.env\.[A-Za-z0-9]{6}$/.test(n)),
    ).toEqual([]);
    const missing = f.run("restore.sh", [
      dir,
      "--replace",
      "--env-file",
      join(f.base, "missing.env"),
    ]);
    expect(missing.status).toBe(1);
    expect(missing.stderr).toContain("Cannot read");
  });

  it("changes .env only after the data is restored", () => {
    const f = fixture();
    const dir = backup(f);
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    writeFileSync(join(f.base, "kept.env"), ENV);
    const args = [dir, "--replace", "--env-file", join(f.base, "kept.env")];
    let r = f.run("restore.sh", args, { STUB_DOWN: "1" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("Docker is not available");
    expect(r.stderr).toContain("Nothing was changed.");
    expect(f.calls()).toEqual([VERSION]);
    f.clearCalls();
    r = f.run("restore.sh", args, { STUB_FAIL: "compose stop*" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("The restore did not finish");
    r = f.run("restore.sh", args, { STUB_FAIL: "run *foundry-data*" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("The restore did not finish");
    expect(f.env()).toBe(other);
    expect(readdirSync(f.root).filter((n) => n.startsWith(".env"))).toEqual([
      ".env",
    ]);
    expect(f.calls().some((c) => c.includes("up -d"))).toBe(false);
    r = f.run("restore.sh", args, {
      STUB_FAIL: "compose up -d --build *",
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(
      "The data is restored, but the services did not start",
    );
    expect(f.env()).toBe(ENV);
  });

  it("refuses a damaged archive before it stops anything", () => {
    const f = fixture();
    const dir = backup(f);
    truncateSync(join(dir, "foundry-data.tgz"), 20);
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("foundry-data.tgz in");
    expect(r.stderr).toContain("Nothing was changed.");
    expect(f.calls()).toEqual([]);
  });

  it("reads relative paths from the folder it runs in", () => {
    const f = fixture();
    let r = f.run("backup.sh", ["relative"]);
    expect(r.status, r.stderr).toBe(0);
    expect(existsSync(join(f.base, "relative", "manifest.txt"))).toBe(true);
    writeFileSync(join(f.base, "kept.env"), ENV);
    writeFileSync(join(f.root, ".env"), ENV.replace(KEY, "c3".repeat(32)));
    r = f.run("restore.sh", [
      "relative",
      "--replace",
      "--env-file",
      "kept.env",
    ]);
    expect(r.status, r.stderr).toBe(0);
    expect(f.env()).toBe(ENV);
  });

  it("restores with another key only when told to", () => {
    const f = fixture();
    // Even a backup with env.enc, when its passphrase is lost.
    const dir = backup(f, ["--include-key"], { BACKUP_PASSPHRASE: PASSPHRASE });
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    const r = f.run("restore.sh", [dir, "--replace", "--accept-key-mismatch"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toContain("re-enter them after the restore");
    expect(f.env()).toBe(other);
  });

  it("refuses a new installation without the kept .env", () => {
    const f = fixture();
    const dir = backup(f);
    rmSync(join(f.root, ".env"));
    for (const args of [[], ["--accept-key-mismatch"]]) {
      const r = f.run("restore.sh", [dir, "--replace", ...args]);
      expect(r.status).toBe(1);
      expect(r.stderr).toContain("There is no .env here");
      expect(r.stderr).toContain("run ./scripts/setup.sh");
    }
    expect(f.calls()).toEqual([]);
  });

  it("decrypts env.enc with BACKUP_PASSPHRASE", () => {
    const f = fixture();
    const dir = backup(f, ["--include-key"], { BACKUP_PASSPHRASE: PASSPHRASE });
    rmSync(join(f.root, ".env"));
    let r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("Set BACKUP_PASSPHRASE");
    r = f.run("restore.sh", [dir, "--replace"], {
      BACKUP_PASSPHRASE: "not the passphrase",
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("check the passphrase");
    expect(readdirSync(f.root).filter((n) => n.startsWith(".env"))).toEqual([]);
    expect(f.calls()).toEqual([]);
    r = f.run("restore.sh", [dir, "--replace"], {
      BACKUP_PASSPHRASE: PASSPHRASE,
    });
    expect(r.status, r.stderr).toBe(0);
    expect(f.env()).toBe(ENV);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
    expect(r.stdout + r.stderr).not.toContain(KEY);
    expect(f.calls()).toEqual(RESTORE_CALLS);
  });

  it("prints no key or passphrase when traced with bash -x", () => {
    const f = fixture();
    const dir = backup(f, ["--include-key"], { BACKUP_PASSPHRASE: PASSPHRASE });
    rmSync(join(f.root, ".env"));
    let r = f.run(
      "restore.sh",
      [dir, "--replace"],
      { BACKUP_PASSPHRASE: PASSPHRASE },
      ["-x"],
    );
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toMatch(/^\+ umask 077$/m);
    let output = r.stdout + r.stderr;
    writeFileSync(join(f.base, "kept.env"), ENV);
    writeFileSync(join(f.root, ".env"), ENV.replace(KEY, "c3".repeat(32)));
    r = f.run(
      "restore.sh",
      [dir, "--replace", "--env-file", join(f.base, "kept.env")],
      {},
      ["-x"],
    );
    expect(r.status, r.stderr).toBe(0);
    output += r.stdout + r.stderr;
    for (const secret of [KEY, "c3".repeat(32), INTERNAL, SECRET, PASSPHRASE])
      expect(output).not.toContain(secret);
  });

  it("restores an old backup that holds config.env, with a warning", () => {
    const f = fixture();
    const dir = backup(f);
    rmSync(join(dir, "manifest.txt"));
    rmSync(join(dir, "settings.env"));
    const old = ENV.replace("3105", "3205");
    writeFileSync(join(dir, "config.env"), old);
    rmSync(join(f.root, ".env"));
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toContain("config.env, which holds an encryption key");
    expect(r.stderr).toContain("does not record its HelixDB version");
    expect(f.env()).toBe(old);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
  });

  it("restores an old backup whose config.env was removed only when told which key", () => {
    const f = fixture();
    const dir = backup(f);
    for (const name of ["manifest.txt", "settings.env"])
      rmSync(join(dir, name));
    let r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("pass --env-file with the copy of .env");
    expect(f.calls()).toEqual([]);
    writeFileSync(join(f.base, "kept.env"), ENV.replace("3105", "3205"));
    r = f.run("restore.sh", [
      dir,
      "--replace",
      "--env-file",
      join(f.base, "kept.env"),
    ]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toContain("cannot check which ENCRYPTION_KEY");
    expect(f.env()).toBe(ENV.replace("3105", "3205"));
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    r = f.run("restore.sh", [dir, "--replace", "--accept-key-mismatch"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("Keeping .env, as asked.");
    expect(f.env()).toBe(other);
  });

  it("checks a config.env against the manifest", () => {
    const f = fixture();
    const dir = backup(f);
    writeFileSync(join(dir, "config.env"), ENV.replace(KEY, "d4".repeat(32)));
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    let r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("config.env, which holds an encryption key");
    expect(r.stderr).toContain(
      "The ENCRYPTION_KEY in config.env in the backup is not the key",
    );
    expect(f.env()).toBe(other);
    expect(f.calls()).toEqual([]);
    writeFileSync(join(dir, "config.env"), ENV);
    r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(f.env()).toBe(ENV);
  });

  it("refuses data from a newer HelixDB storage format", () => {
    const f = fixture();
    f.pin(image("0.0.9"));
    for (const version of ["0.0.10", "99.0.0"]) {
      const dir = backup(f, [], { STUB_HELIX_IMAGE: image(version) });
      const r = f.run("restore.sh", [dir, "--replace"]);
      expect(r.status).toBe(1);
      expect(r.stderr).toContain(
        `HelixDB ${version} made this backup, but this checkout runs HelixDB 0.0.9`,
      );
      expect(r.stderr).toContain("check out the Helix Foundry release");
      expect(f.calls()).toEqual([]);
    }
  });

  it("restores data HelixDB can open, and says when it upgrades one way", () => {
    const f = fixture();
    const restore = (running: string) => {
      const r = f.run("restore.sh", [
        backup(f, [], { STUB_HELIX_IMAGE: running }),
        "--replace",
      ]);
      expect(r.status, r.stderr).toBe(0);
      return r.stdout + r.stderr;
    };
    // The same version, recorded without its digest.
    expect(restore(`ghcr.io/helixdb/helixdb:v${pinned[2]}`)).not.toContain(
      "HelixDB",
    );
    f.pin(image("0.0.6"));
    expect(restore(image("0.0.9"))).toContain(
      "HelixDB 0.0.9 made this backup. This checkout runs HelixDB 0.0.6, which uses the same storage format.",
    );
    f.pin(image("0.0.10"));
    expect(restore(image("0.0.11"))).toContain("uses the same storage format");
    let output = restore(image("0.0.9"));
    expect(output).toContain(
      "HelixDB 0.0.9 made this backup. This checkout runs HelixDB 0.0.10, which upgrades the restored data's index storage one way",
    );
    f.pin(image("0.0.12"));
    output = restore(image("0.0.10"));
    expect(output).toContain("This checkout runs HelixDB 0.0.12.");
    expect(output).not.toContain("one way");
  });

  it("refuses data with a newer metadata schema", () => {
    const f = fixture();
    const dir = backup(f);
    const manifest = join(dir, "manifest.txt");
    writeFileSync(
      manifest,
      readFileSync(manifest, "utf8").replace(
        /^app_schema=.*$/m,
        "app_schema=99",
      ),
    );
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("metadata schema 99");
    expect(f.calls()).toEqual([]);
  });
});

describe("upgrade.sh", () => {
  // A checkout of release 1 whose upstream has release 2 with HelixDB 99.0.0.
  function release(f: Fixture, newImage = image("99.0.0")) {
    f.git(f.root, "init", "-q", "-b", "main");
    f.git(f.root, "add", "-A");
    f.git(f.root, "commit", "-q", "-m", "Release 1");
    const remote = join(f.base, "remote.git");
    f.git(f.base, "clone", "-q", "--bare", f.root, remote);
    f.git(f.root, "remote", "add", "origin", remote);
    f.git(f.root, "fetch", "-q", "origin");
    f.git(f.root, "branch", "-q", "-u", "origin/main");
    const other = join(f.base, "other");
    f.git(f.base, "clone", "-q", remote, other);
    const compose = join(other, "compose.yaml");
    writeFileSync(
      compose,
      readFileSync(compose, "utf8").replace(pinned[1], newImage),
    );
    f.git(other, "commit", "-q", "-am", "Release 2");
    f.git(other, "push", "-q");
    return {
      from: f.git(f.root, "rev-parse", "HEAD"),
      to: f.git(other, "rev-parse", "HEAD"),
      composeMode: mode(compose),
    };
  }
  const backups = (f: Fixture) => join(f.home, "helix-foundry-backups");

  it("stops before upgrading when the backup fails", () => {
    const f = fixture();
    const { from } = release(f);
    const r = f.run("upgrade.sh", ["--yes"], { STUB_FAIL: "run *" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("the backup failed. Nothing was upgraded.");
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(from);
    expect(f.calls()).toContain("compose start helix executor app worker");
    expect(f.calls().some((c) => c.includes("up -d"))).toBe(false);
  });

  it("asks before a one-way HelixDB upgrade", () => {
    const f = fixture();
    const { from } = release(f);
    const r = f.run("upgrade.sh");
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("this upgrade changes the HelixDB image");
    expect(r.stderr).toContain("Run again with --yes");
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(from);
    expect(f.calls()).toEqual(["compose ps -a -q helix"]);
    expect(existsSync(backups(f))).toBe(false);
  });

  it("backs up, records the old release, fast-forwards and rebuilds", () => {
    const f = fixture();
    const { from, to, composeMode } = release(f);
    const r = f.run("upgrade.sh", ["--yes"], { STUB_OLLAMA: "1" });
    expect(r.status, r.stderr).toBe(0);
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(to);
    // Checked-out files keep their usual modes; only backup files are private.
    expect(mode(join(f.root, "compose.yaml"))).toBe(composeMode);
    const [name] = readdirSync(backups(f));
    const dir = join(backups(f), name);
    expect(field(dir, "app_commit")).toBe(from);
    expect(field(dir, "helix_image")).toBe(pinned[1]);
    expect(readFileSync(join(dir, "upgrade.txt"), "utf8")).toBe(
      `from_commit=${from}\nfrom_helix_image=${pinned[1]}\nto_commit=${to}\nto_helix_image=${image("99.0.0")}\n`,
    );
    expect(mode(join(dir, "upgrade.txt"))).toBe(0o600);
    const calls = f.calls();
    expect(
      calls.indexOf("compose stop app worker executor helix"),
    ).toBeLessThan(calls.indexOf("compose --profile local up -d --build"));
    expect(calls.at(-1)).toBe("compose --profile local up -d --build");
    expect(r.stdout).toContain(
      `git reset --keep ${from}, then scripts/restore.sh`,
    );
    f.clearCalls();
    const again = f.run("upgrade.sh", [], {
      STUB_HELIX_IMAGE: image("99.0.0"),
    });
    expect(again.stdout).toContain("Already up to date");
    expect(f.calls()).toEqual([
      "compose ps -a -q helix",
      "inspect --format {{.Config.Image}} stub-helix",
    ]);
  });

  it("refuses to move HelixDB back to an older version", () => {
    const f = fixture();
    const { from } = release(f, image("0.0.1"));
    const r = f.run("upgrade.sh", ["--yes"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("moves HelixDB back");
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(from);
    expect(f.calls()).toEqual(["compose ps -a -q helix"]);
  });

  it("finishes an upgrade whose rebuild failed", () => {
    const f = fixture();
    const { from, to } = release(f);
    const old = { STUB_HELIX_IMAGE: pinned[1] };
    let r = f.run("upgrade.sh", ["--yes"], {
      ...old,
      STUB_FAIL: "compose up -d --build",
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("The rebuild failed");
    expect(r.stderr).toContain(`git reset --keep ${from}`);
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(to);
    // The checkout is at the new release, but HelixDB has not started it yet.
    f.clearCalls();
    r = f.run("upgrade.sh", [], old);
    expect(r.status).toBe(1);
    expect(r.stdout).toContain("the services may not run it yet");
    expect(r.stderr).toContain("Run again with --yes");
    r = f.run("upgrade.sh", ["--yes"], old);
    expect(r.status, r.stderr).toBe(0);
    expect(f.calls().at(-1)).toBe("compose up -d --build");
    const dirs = readdirSync(backups(f)).sort();
    expect(dirs).toHaveLength(2);
    const dir = join(backups(f), dirs[1]);
    expect(field(dir, "app_commit")).toBe(from);
    expect(field(dir, "helix_image")).toBe(pinned[1]);
    expect(readFileSync(join(dir, "upgrade.txt"), "utf8")).toContain(
      `from_commit=${from}\n`,
    );
    expect(
      f.run("upgrade.sh", [], { STUB_HELIX_IMAGE: image("99.0.0") }).stdout,
    ).toContain("Already up to date");
  });

  it("keeps BACKUP_PASSPHRASE from docker and git", () => {
    const f = fixture();
    release(f);
    const git = spawnSync("sh", ["-c", "command -v git"], {
      encoding: "utf8",
    }).stdout.trim();
    const wrapper = join(f.base, "bin", "git");
    writeFileSync(
      wrapper,
      `#!/bin/sh\n[ -z "\${BACKUP_PASSPHRASE+x}" ] || echo 'git: a backup passphrase is in the environment' >> "$STUB_LOG"\nexec ${git} "$@"\n`,
    );
    chmodSync(wrapper, 0o755);
    const r = f.run("upgrade.sh", ["--yes", "--include-key"], {
      BACKUP_PASSPHRASE: PASSPHRASE,
    });
    expect(r.status, r.stderr).toBe(0);
    const [name] = readdirSync(backups(f));
    expect(existsSync(join(backups(f), name, "env.enc"))).toBe(true);
    expect(f.calls().join("\n")).not.toContain("passphrase");
    expect(f.calls()).toContain("compose up -d --build");
    // Nor does tracing print it.
    const traced = fixture();
    release(traced);
    const t = traced.run(
      "upgrade.sh",
      ["--yes", "--include-key"],
      { BACKUP_PASSPHRASE: PASSPHRASE },
      ["-x"],
    );
    expect(t.status, t.stderr).toBe(0);
    expect(t.stderr).toMatch(/^\+ take_passphrase$/m);
    for (const secret of [KEY, INTERNAL, SECRET, PASSPHRASE])
      expect(t.stdout + t.stderr).not.toContain(secret);
  });

  it("finishes the upgrade again after going back to the old release", () => {
    const f = fixture();
    const { from, to } = release(f);
    const old = { STUB_HELIX_IMAGE: pinned[1] };
    let r = f.run("upgrade.sh", ["--yes"], old);
    expect(r.status, r.stderr).toBe(0);
    const [name] = readdirSync(backups(f));
    // Go back with a detached checkout, then return to the branch.
    f.git(f.root, "checkout", "-q", "--detach", from);
    r = f.run("restore.sh", [join(backups(f), name), "--replace"], old);
    expect(r.status, r.stderr).toBe(0);
    f.git(f.root, "switch", "-q", "main");
    f.clearCalls();
    r = f.run("upgrade.sh", [], old);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("this upgrade changes the HelixDB image");
    expect(f.calls()).toEqual([
      "compose ps -a -q helix",
      "inspect --format {{.Config.Image}} stub-helix",
    ]);
    r = f.run("upgrade.sh", ["--yes"], old);
    expect(r.status, r.stderr).toBe(0);
    expect(f.calls().at(-1)).toBe("compose up -d --build");
    // Go back as upgrade.sh says, which keeps the branch behind its upstream.
    f.git(f.root, "reset", "-q", "--keep", from);
    r = f.run("restore.sh", [join(backups(f), name), "--replace"], old);
    expect(r.status, r.stderr).toBe(0);
    r = f.run("upgrade.sh", ["--yes"], old);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain(`Updating ${from} to ${to}.`);
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(to);
  });

  it("rebuilds a checkout that was pulled by hand", () => {
    const f = fixture();
    const { to } = release(f);
    f.git(f.root, "merge", "-q", "--ff-only", "origin/main");
    const old = { STUB_HELIX_IMAGE: pinned[1] };
    let r = f.run("upgrade.sh", [], old);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("this upgrade changes the HelixDB image");
    r = f.run("upgrade.sh", ["--yes"], old);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain(`Upgraded to ${to}.`);
    expect(f.calls().at(-1)).toBe("compose up -d --build");
  });
});

describe("compose.yaml", () => {
  it("pins the development HelixDB in the docs to the image Compose runs", () => {
    for (const doc of ["README.md", "AGENTS.md", "docs/GUIDE.md"]) {
      const images = readFileSync(join(repo, doc), "utf8").match(
        /ghcr\.io\/helixdb\/helixdb:\S+/g,
      );
      expect(images, doc).not.toBeNull();
      for (const found of images!) expect(found, doc).toBe(pinned[1]);
    }
  });
});
