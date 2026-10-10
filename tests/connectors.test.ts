import { it, expect, vi } from "vitest";
import { createServer } from "node:http";
import { createServer as createTcpServer } from "node:net";
import { once } from "node:events";
import { readFile, rm } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";
import pg from "pg";
import { discover, importSource } from "../apps/api/src/connectors.js";
import { config } from "../apps/api/src/config.js";
it("discovers and imports paginated REST data without forwarding credentials across origins", async () => {
  const seen: string[] = [];
  const server = createServer((req, res) => {
    seen.push(req.url!);
    res.setHeader("content-type", "application/json");
    if (req.url === "/escape") {
      res.end(JSON.stringify({ items: [], next: "http://example.com/steal" }));
      return;
    }
    const u = new URL(req.url!, "http://localhost");
    const offset = Number(u.searchParams.get("offset") || 0);
    res.end(
      JSON.stringify({
        items: Array.from(
          { length: Math.max(0, Math.min(2, 5 - offset)) },
          (_, i) => ({ id: offset + i, label: "Unicode 東京" }),
        ),
      }),
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const base = "http://127.0.0.1:" + (server.address() as any).port,
    scope = "rest-" + randomUUID();
  try {
    const c = {
      url: base + "/data",
      itemsPath: "items",
      pagination: "offset",
      pageSize: 2,
      token: "test-token",
    };
    expect(((await discover("rest", c)) as any).sample).toHaveLength(2);
    const file = await importSource(scope, "rest", c);
    const lines = (
      await readFile(
        resolve(config.dataDir, "incoming", scope, file.file),
        "utf8",
      )
    )
      .trim()
      .split("\n");
    expect(lines).toHaveLength(5);
    expect(seen).toContain("/data?offset=0&limit=2");
    await expect(
      importSource(scope, "rest", {
        ...c,
        url: base + "/escape",
        pagination: "next",
      }),
    ).rejects.toThrow("configured origin");
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
it("speaks S3-compatible list and streamed object protocols", async () => {
  const server = createServer((req, res) => {
    if (req.url?.includes("list-type=2")) {
      res.setHeader("content-type", "application/xml");
      res.end(
        '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>fixture</Name><IsTruncated>false</IsTruncated><Contents><Key>rows.csv</Key><Size>14</Size></Contents></ListBucketResult>',
      );
    } else {
      res.setHeader("content-type", "text/csv");
      res.end("id,name\n1,Ada\n");
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const scope = "s3-" + randomUUID(),
    c = {
      endpoint: "http://127.0.0.1:" + (server.address() as any).port,
      bucket: "fixture",
      key: "rows.csv",
      accessKeyId: "test-access",
      secretAccessKey: "test-secret",
    };
  try {
    const found = await discover("s3", c);
    expect(found.tables).toEqual([{ name: "rows.csv", size: 14 }]);
    const result = await importSource(scope, "s3", c);
    expect(
      await readFile(
        resolve(config.dataDir, "incoming", scope, result.file),
        "utf8",
      ),
    ).toBe("id,name\n1,Ada\n");
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
it("imports S3 keys whose extension is upper case", async () => {
  const server = createServer((_, res) => res.end("id\n1\n"));
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const scope = "s3-" + randomUUID();
  try {
    const result = await importSource(scope, "s3", {
      endpoint: "http://127.0.0.1:" + (server.address() as any).port,
      bucket: "fixture",
      key: "DATA.CSV",
      accessKeyId: "test-access",
      secretAccessKey: "test-secret",
    });
    expect(result.file).toMatch(/\.csv$/);
    expect(
      await readFile(
        resolve(config.dataDir, "incoming", scope, result.file),
        "utf8",
      ),
    ).toBe("id\n1\n");
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
it("closes the Postgres pool when no snapshot connection can be acquired", async () => {
  // Hangs up on every connection.
  const server = createTcpServer((socket) => socket.destroy());
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const end = vi.spyOn(pg.Pool.prototype, "end"),
    scope = "postgres-" + randomUUID();
  try {
    await expect(
      importSource(scope, "postgres", {
        host: "127.0.0.1",
        port: (server.address() as any).port,
        user: "fixture",
        password: "fixture",
        database: "fixture",
        table: "orders",
      }),
    ).rejects.toThrow();
    expect(end).toHaveBeenCalledOnce();
  } finally {
    end.mockRestore();
    await new Promise<void>((r) => server.close(() => r()));
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});
