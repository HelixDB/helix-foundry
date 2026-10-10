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
it("retains repeated aliases owned by one input but suppresses ambiguous aliases", async () => {
  const root = await fixture([{ id: 1 }]);
  try {
    await execute(
      { mode: "ingest", scope: "fixture", version: "one", file: "input.jsonl" },
      root,
    );
    const request = {
      mode: "query" as const,
      scope: "fixture",
      inputs: [
        {
          table: "input",
          version: "one",
          aliases: ["Orders", "orders", "Orders"],
        },
      ],
      sql: 'SELECT * FROM "orders"',
    };
    expect((await execute(request, root)).rows).toHaveLength(1);
    await expect(
      execute(
        {
          ...request,
          inputs: [
            ...request.inputs,
            { table: "other", version: "one", aliases: ["orders"] },
          ],
        },
        root,
      ),
    ).rejects.toThrow();
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
