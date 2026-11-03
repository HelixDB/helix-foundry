import { expect, it, vi } from "vitest";
import { api } from "@/web/api";
import { reviseOntologyVersion } from "@/web/areas/ontology/queries";
vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
it("encodes each revision path segment, sends only scoped edits, and propagates failure", async () => {
  const body = {
    edits: [{ remove: { itemId: "rule.flag" } }],
    dryRun: true,
  };
  vi.mocked(api).mockResolvedValueOnce({ generation: { id: "gen_1" } });
  await reviseOntologyVersion("ws /", "o ?", "v/#", body);
  expect(api).toHaveBeenCalledWith(
    "/v1/workspaces/ws%20%2F/foundry/ontologies/o%20%3F/versions/v%2F%23:revise",
    body,
  );
  vi.mocked(api).mockRejectedValueOnce(new Error("unavailable"));
  await expect(reviseOntologyVersion("ws", "o", "v", body)).rejects.toThrow(
    "unavailable",
  );
});
