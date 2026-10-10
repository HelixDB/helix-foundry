import { it, expect } from "vitest";
import { normalizeWorkos, workosObjects } from "../apps/api/src/workos.js";
it("retains the authoritative filtered parent when the returned parent field is null", () => {
  const obj = workosObjects.find((o) => o.name === "directory_users")!;
  expect(
    normalizeWorkos(obj, { id: "du_1", directory_id: null }, "dir_1"),
  ).toMatchObject({ directory_user_id: "du_1", directory_id: "dir_1" });
});
