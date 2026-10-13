import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { mkdir, readdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { request as httpRequest } from "node:http";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";
let app: Awaited<ReturnType<typeof createApp>>, scope: string, dir: string;
beforeEach(async () => {
  app = await createApp(new MemoryStore());
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
  dir = resolve(config.dataDir, "incoming", scope);
  await mkdir(dir, { recursive: true });
});
afterEach(async () => {
  await app.close();
  for (const area of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, area, scope), {
      recursive: true,
      force: true,
    });
});
const body = (finish: boolean) =>
  '--fixture\r\nContent-Disposition: form-data; name="file"; filename="orders.csv"\r\nContent-Type: text/csv\r\n\r\nid,amount\n1,42\n' +
  (finish ? "\r\n--fixture--\r\n" : "");
const upload = (finish: boolean) =>
  app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/upload`,
    headers: { "content-type": "multipart/form-data; boundary=fixture" },
    payload: body(finish),
  });
it("removes the partial file when an HTTP upload disconnects", async () => {
  const address = await app.listen({ host: "127.0.0.1", port: 0 });
  const req = httpRequest(address + `/api/v1/workspaces/${scope}/upload`, {
    method: "POST",
    headers: { "content-type": "multipart/form-data; boundary=fixture" },
  });
  req.on("error", () => {});
  try {
    req.write(body(false));
    for (let i = 0; i < 100 && !(await readdir(dir)).length; i++)
      await new Promise((r) => setTimeout(r, 10));
    expect(await readdir(dir)).toHaveLength(1);
    req.destroy();
    for (let i = 0; i < 100 && (await readdir(dir)).length; i++)
      await new Promise((r) => setTimeout(r, 10));
    expect(await readdir(dir)).toEqual([]);
  } finally {
    req.destroy();
  }
});

it("retains the complete source file and commits a valid native upload", async () => {
  const response = await upload(true);
  expect(response.statusCode, response.body).toBe(200);
  expect(response.json().kind).toBe("dataset");
  expect(response.json().data.profile.rows).toBe(1);
  expect(await readdir(dir)).toHaveLength(1);
});
