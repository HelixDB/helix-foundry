import { it, expect } from "vitest";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execute } from "../services/executor/src/engine.js";
async function fixture(rows: unknown[]) {
  const root = await mkdtemp(join(tmpdir(), "foundry-regression-"));
  await mkdir(join(root, "incoming", "fixture"), { recursive: true });
  await writeFile(
    join(root, "incoming", "fixture", "input.jsonl"),
    rows.map((r) => JSON.stringify(r)).join("\n"),
  );
  return root;
}
it("distinguishes changed duplicate pairs without depending on row order", async () => {
  const root = await fixture([{ id: "a" }, { id: "a" }]);
  try {
    const a = await execute(
      { mode: "ingest", scope: "fixture", version: "a", file: "input.jsonl" },
      root,
    );
    await writeFile(
      join(root, "incoming", "fixture", "input.jsonl"),
      '{"id":"b"}\n{"id":"b"}',
    );
    const b = await execute(
      { mode: "ingest", scope: "fixture", version: "b", file: "input.jsonl" },
      root,
    );
    expect(a.profile.fingerprint).not.toBe(b.profile.fingerprint);
    await writeFile(
      join(root, "incoming", "fixture", "input.jsonl"),
      '{"id":"a"}\n{"id":"b"}\n{"id":"a"}',
    );
    const c = await execute(
      { mode: "ingest", scope: "fixture", version: "c", file: "input.jsonl" },
      root,
    );
    await writeFile(
      join(root, "incoming", "fixture", "input.jsonl"),
      '{"id":"a"}\n{"id":"a"}\n{"id":"b"}',
    );
    const d = await execute(
      { mode: "ingest", scope: "fixture", version: "d", file: "input.jsonl" },
      root,
    );
    expect(c.profile.fingerprint).toBe(d.profile.fingerprint);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
