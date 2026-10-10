import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { open, readFile, readdir, rm, type FileHandle } from "node:fs/promises";
import { platform } from "node:os";
import { resolve } from "node:path";

vi.mock("node:os", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:os")>();
  return { ...actual, platform: vi.fn(actual.platform) };
});
vi.mock("node:fs/promises", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:fs/promises")>();
  return { ...actual, open: vi.fn(actual.open) };
});

import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";

const fs =
  await vi.importActual<typeof import("node:fs/promises")>("node:fs/promises");
const directory = {
  sync: vi.fn<() => Promise<void>>(),
  close: vi.fn<() => Promise<void>>(),
};
const fileSync = vi.fn<(sync: () => Promise<void>) => Promise<void>>();
let app: Awaited<ReturnType<typeof createApp>>,
  store: MemoryStore,
  scope: string,
  dir: string;

beforeEach(async () => {
  vi.mocked(platform).mockReset().mockReturnValue(process.platform);
  directory.sync.mockReset().mockResolvedValue(undefined);
  directory.close.mockReset().mockResolvedValue(undefined);
  fileSync.mockReset().mockImplementation((sync) => sync());
  // Keep real file writes and flushes; emulate directory handles on each OS.
  vi.mocked(open)
    .mockReset()
    .mockImplementation(async (...args) => {
      if (args[0] === dir) return directory as unknown as FileHandle;
      const handle = await fs.open(...args);
      const sync = handle.sync.bind(handle);
      vi.spyOn(handle, "sync").mockImplementation(() => fileSync(sync));
      return handle;
    });
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const response = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(response.statusCode).toBe(200);
  scope = response.json().workspaces[0].id;
  dir = resolve(config.dataDir, "incoming", scope);
});

afterEach(async () => {
  vi.restoreAllMocks();
  await app.close();
  await rm(dir, { recursive: true, force: true });
});

function upload(csv = "id,name\nA,Acme\n") {
  const boundary = "upload-sync-boundary";
  return app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/onboarding/uploads`,
    headers: {
      "content-type": `multipart/form-data; boundary=${boundary}`,
      "idempotency-key": "upload-sync-operation",
    },
    payload: `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="Customers.csv"\r\nContent-Type: text/csv\r\n\r\n${csv}\r\n--${boundary}--\r\n`,
  });
}

it.each(["win32", "linux", "darwin"] as const)(
  "stages uploads and preserves retry checks on %s",
  async (os) => {
    vi.mocked(platform).mockReturnValue(os);
    if (os === "win32")
      directory.sync.mockRejectedValue(
        Object.assign(new Error("Directory flush is unsupported"), {
          code: "EPERM",
        }),
      );

    const first = await upload();
    expect(first.statusCode, first.body).toBe(200);
    const job = (await store.get(scope, first.json().id))!;
    expect(job.data.status).toBe("queued");
    expect(first.json().data.file).toBeUndefined();
    expect(await readFile(resolve(dir, job.data.file), "utf8")).toBe(
      "id,name\nA,Acme\n",
    );
    const retry = await upload();
    expect(retry.statusCode, retry.body).toBe(200);
    expect(retry.json().id).toBe(job.id);
    const conflict = await upload("id,name\nB,Changed\n");
    expect(conflict.statusCode).toBe(409);
    expect(await store.list(scope, "job")).toHaveLength(1);
    expect((await readdir(dir)).some((file) => file.endsWith(".staging"))).toBe(
      false,
    );
    expect(fileSync).toHaveBeenCalledTimes(3);
    expect(directory.sync).toHaveBeenCalledTimes(os === "win32" ? 0 : 3);
    expect(directory.close).toHaveBeenCalledTimes(os === "win32" ? 0 : 3);
  },
);

it.each([
  ["win32", "EPERM"],
  ["win32", "EIO"],
  ["linux", "EPERM"],
  ["linux", "EIO"],
  ["darwin", "EPERM"],
  ["darwin", "EIO"],
] as const)("rejects a file flush failure on %s (%s)", async (os, code) => {
  vi.mocked(platform).mockReturnValue(os);
  fileSync.mockRejectedValueOnce(
    Object.assign(new Error("File flush failed"), { code }),
  );

  const response = await upload();
  expect(response.statusCode).toBe(500);
  expect(fileSync).toHaveBeenCalledOnce();
  expect(await store.list(scope, "job")).toHaveLength(0);
  expect(await readdir(dir)).toEqual([]);
});

it.each([
  ["linux", "EPERM"],
  ["linux", "EIO"],
  ["darwin", "EPERM"],
  ["darwin", "EIO"],
] as const)(
  "rejects a directory flush failure on %s (%s)",
  async (os, code) => {
    vi.mocked(platform).mockReturnValue(os);
    directory.sync.mockRejectedValueOnce(
      Object.assign(new Error("Directory flush failed"), { code }),
    );

    const response = await upload();
    expect(response.statusCode).toBe(500);
    expect(await store.list(scope, "job")).toHaveLength(0);
    expect(directory.sync).toHaveBeenCalledOnce();
    expect(directory.close).toHaveBeenCalledOnce();
  },
);
