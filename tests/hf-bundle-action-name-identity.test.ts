import { it, expect } from "vitest";
import { validateBundle } from "../apps/api/src/assistant.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { BundleSchema } from "../packages/shared/src/index.js";
it("includes action names in definition identity validation", async () => {
  const b = BundleSchema.parse({
    summary: "",
    assumptions: [],
    catalog: [],
    pipelines: [],
    ontology: [],
    relationships: [],
    dashboards: [],
    actions: [
      { name: "Edit", objectType: "Missing", fields: [] },
      { name: "Edit", objectType: "Missing", fields: [] },
    ],
  });
  const r = await validateBundle(new MemoryStore(), "test", b);
  expect(r.checks.find((c) => c.name === "Definition identities")?.passed).toBe(
    false,
  );
});
