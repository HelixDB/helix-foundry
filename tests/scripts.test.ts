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
    run: (script: string, args: string[] = [], extra = {}) =>
      spawnSync("bash", [join(root, "scripts", script), ...args], {
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
      "volume inspect helix-foundry_helix-data",
      "volume inspect helix-foundry_foundry-data",
      "compose ps -a -q helix",
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

  it("refuses a folder inside the repository, or one in use", () => {
    const f = fixture();
    let r = f.run("backup.sh", [join(f.root, "backups", "today")]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("outside the repository");
    expect(existsSync(join(f.root, "backups", "today"))).toBe(false);
    const used = join(f.base, "used");
    mkdirSync(used);
    writeFileSync(join(used, "notes.txt"), "");
    r = f.run("backup.sh", [used]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("is not empty");
    expect(f.calls()).toEqual([]);
  });

  it("restarts the services when archiving fails", () => {
    const f = fixture();
    const r = f.run("backup.sh", [join(f.base, "backup")], {
      STUB_FAIL_RUN: "1",
    });
    expect(r.status).not.toBe(0);
    expect(f.calls().at(-1)).toBe("compose start helix executor app worker");
    expect(existsSync(join(f.base, "backup", "manifest.txt"))).toBe(false);
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
});

describe("restore.sh", () => {
  it("keeps a matching .env and replaces both volumes", () => {
    const f = fixture();
    const dir = backup(f);
    expect(f.run("restore.sh", [dir]).stderr).toContain("Pass --replace");
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("Keeping .env");
    expect(f.env()).toBe(ENV);
    for (const volume of ["helix-data", "foundry-data"])
      expect(readFileSync(join(f.restored, volume + ".tgz"))).toEqual(
        readFileSync(join(dir, volume + ".tgz")),
      );
    expect(f.calls()).toEqual([
      "compose stop app worker executor helix",
      "run --rm -i -v helix-foundry_helix-data:/data alpine:3.22 sh -c find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data",
      "run --rm -i -v helix-foundry_foundry-data:/data alpine:3.22 sh -c find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data",
      "compose up -d --build helix executor app worker",
    ]);
  });

  it("refuses another key unless --env-file names the kept .env", () => {
    const f = fixture();
    const dir = backup(f);
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
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
    expect(f.calls()).toContain(
      "compose up -d --build helix executor app worker",
    );
  });

  it("restores with another key only when told to", () => {
    const f = fixture();
    const dir = backup(f);
    const other = ENV.replace(KEY, "c3".repeat(32));
    writeFileSync(join(f.root, ".env"), other);
    const r = f.run("restore.sh", [dir, "--replace", "--accept-key-mismatch"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stderr).toContain("Re-enter them");
    expect(f.env()).toBe(other);
  });

  it("refuses a new installation without the kept .env", () => {
    const f = fixture();
    const dir = backup(f);
    rmSync(join(f.root, ".env"));
    const r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("There is no .env here");
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
    expect(r.stderr).toContain("config.env holds your encryption key");
    expect(r.stderr).toContain("does not record its HelixDB version");
    expect(f.env()).toBe(old);
    expect(mode(join(f.root, ".env"))).toBe(0o600);
  });

  it("refuses data from a newer HelixDB, and upgrades older data", () => {
    const f = fixture();
    let dir = backup(f, [], { STUB_HELIX_IMAGE: image("99.0.0") });
    let r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(
      `HelixDB 99.0.0 made this backup, but this checkout runs HelixDB ${pinned[2]}`,
    );
    expect(r.stderr).toContain("check out the Helix Foundry release");
    expect(f.calls()).toEqual([]);
    dir = backup(f, [], { STUB_HELIX_IMAGE: image("0.0.1") });
    r = f.run("restore.sh", [dir, "--replace"]);
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toContain("upgrades the restored data one way");
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
    const r = f.run("upgrade.sh", ["--yes"], { STUB_FAIL_RUN: "1" });
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
    expect(f.calls()).toEqual([]);
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
    expect(r.stdout).toContain(`git checkout ${from}`);
    expect(f.run("upgrade.sh").stdout).toContain("Already up to date");
  });

  it("refuses to move HelixDB back to an older version", () => {
    const f = fixture();
    const { from } = release(f, image("0.0.1"));
    const r = f.run("upgrade.sh", ["--yes"]);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("moves HelixDB back");
    expect(f.git(f.root, "rev-parse", "HEAD")).toBe(from);
    expect(f.calls()).toEqual([]);
  });
});
