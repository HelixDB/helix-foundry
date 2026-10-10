import { afterAll, expect, it } from "vitest";
import { execFile } from "node:child_process";
import {
  chmod,
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { promisify } from "node:util";
const run = promisify(execFile);
const root = await mkdtemp(join(tmpdir(), "foundry-backup-"));
afterAll(() => rm(root, { recursive: true, force: true }));
// Stands in for Docker: compose calls are logged, and `docker run` runs its
// command on this machine with each mounted path mapped to a local directory.
// This machine's tar stands in for the image's BusyBox tar.
const docker = `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$STUB_LOG"
[ "$1" = compose ] && exit 0
[ "$1" = run ] && shift || exit 1
maps=()
while [ "$1" != alpine:3.22 ]; do
  case "$1" in
    --rm) shift ;;
    -v)
      source="\${2%%:*}" target="\${2#*:}"
      case "$source" in /*) ;; *) source="$STUB_VOLUMES/$source" ;; esac
      maps+=("\${target%%:*}=$source")
      shift 2 ;;
    *) exit 1 ;;
  esac
done
shift
args=()
for arg in "$@"; do
  for map in "\${maps[@]}"; do
    target="\${map%%=*}"
    case "$arg" in "$target" | "$target"/*) arg="\${map#*=}\${arg#"$target"}" ;; esac
  done
  args+=("$arg")
done
exec "\${args[@]}"
`;
async function put(path: string, content = "x") {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, content);
}
it("backs up the data volumes without executor scratch or publication results", async () => {
  // A copy of the script, so it never reads this checkout's .env.
  const repo = join(root, "repo"),
    bin = join(root, "bin"),
    volumes = join(root, "volumes"),
    log = join(root, "docker.log"),
    backup = join(root, "backup");
  await mkdir(join(repo, "scripts"), { recursive: true });
  await copyFile("scripts/backup.sh", join(repo, "scripts", "backup.sh"));
  await put(join(repo, ".env"), "ENCRYPTION_KEY=test\n");
  await put(join(bin, "docker"), docker);
  await chmod(join(bin, "docker"), 0o755);
  const helix = join(volumes, "helix-foundry_helix-data"),
    foundry = join(volumes, "helix-foundry_foundry-data");
  await put(join(helix, "data.mdb"));
  await put(join(helix, "scratch", "kept"));
  await put(join(foundry, "datasets", "w", "v1.parquet"));
  await put(join(foundry, "incoming", "w", "upload.csv"));
  await put(join(foundry, "scratch", "job-1", "duckdb_temp_storage.tmp"));
  await put(join(foundry, "results", "w", "rows.jsonl"));
  await run("bash", [join(repo, "scripts", "backup.sh"), backup], {
    env: {
      ...process.env,
      PATH: bin + ":" + process.env.PATH,
      STUB_LOG: log,
      STUB_VOLUMES: volumes,
      COMPOSE_PROJECT_NAME: "",
    },
  });
  const files = async (archive: string) =>
    (await run("tar", ["tzf", join(backup, archive)])).stdout
      .split("\n")
      .filter((f) => f && !f.endsWith("/"))
      .sort();
  expect(await files("foundry-data.tgz")).toEqual([
    "./datasets/w/v1.parquet",
    "./incoming/w/upload.csv",
  ]);
  expect(await files("helix-data.tgz")).toEqual([
    "./data.mdb",
    "./scratch/kept",
  ]);
  expect((await readFile(log, "utf8")).split("\n")).toEqual([
    "compose stop app worker executor helix",
    expect.stringContaining("helix-foundry_helix-data:/data:ro"),
    expect.stringContaining("helix-foundry_foundry-data:/data:ro"),
    "compose start helix executor app worker",
    "",
  ]);
});
