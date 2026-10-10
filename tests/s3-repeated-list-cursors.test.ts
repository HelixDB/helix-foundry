import { createServer } from "node:http";
import { expect, it } from "vitest";
import { discover } from "../apps/api/src/connectors.js";

it("stops repeated S3 listing cursors even when pages contain no importable files", async () => {
  const cursors: (string | null)[] = [];
  const server = createServer((request, response) => {
    cursors.push(
      new URL(request.url!, "http://localhost").searchParams.get(
        "continuation-token",
      ),
    );
    response.setHeader("Content-Type", "application/xml");
    // The third response bounds the faulty baseline without waiting for its infinite loop.
    response.end(
      '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>true</IsTruncated>' +
        (cursors.length < 3
          ? "<NextContinuationToken>repeat</NextContinuationToken>"
          : "") +
        "<Contents><Key>ignored.txt</Key><Size>0</Size></Contents></ListBucketResult>",
    );
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address();
    if (!address || typeof address === "string")
      throw new Error("Missing listener");
    await expect(
      discover("s3", {
        endpoint: `http://127.0.0.1:${address.port}`,
        bucket: "fixture",
        accessKeyId: "test-key",
        secretAccessKey: "test-secret",
      }),
    ).rejects.toThrow("Bucket listing repeated a continuation token");
    expect(cursors).toEqual([null, "repeat"]);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});
