import { it, expect, vi, afterEach } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { config } from "../apps/api/src/config.js";
import { importSource } from "../apps/api/src/connectors.js";
vi.mock("@aws-sdk/client-s3", () => ({
  S3Client: class {
    async send() {
      return {
        Body: (async function* () {
          yield Buffer.from("id\n1\n");
        })(),
      };
    }
    destroy() {}
  },
  GetObjectCommand: class {},
  ListObjectsV2Command: class {},
}));
it("imports uppercase extensions accepted by discovery", async () => {
  const scope = "uppercase-format-fixture";
  try {
    const result = await importSource(scope, "s3", {
      key: "DATA.CSV",
      bucket: "fixture",
    });
    expect(result.file).toMatch(/\.csv$/);
  } finally {
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
