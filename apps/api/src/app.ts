import { registerOnboarding } from "./onboarding.js";
import { registerHosting } from "./hosting.js";
import { publicResource } from "./onboarding-state.js";
import { effectiveObject } from "./context.js";
import { openapiTransform } from "./openapi.js";
import Fastify from "fastify";
import cookie from "@fastify/cookie";
import multipart from "@fastify/multipart";
import rateLimit from "@fastify/rate-limit";
import swagger from "@fastify/swagger";
import swaggerUI from "@fastify/swagger-ui";
import staticFiles from "@fastify/static";
import { z } from "zod";
import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { resolve } from "node:path";
import {
  type Resource,
  type Role,
} from "../../../packages/shared/src/index.js";
import { config } from "./config.js";
import { assert, digest } from "./security.js";
import { resource, transaction, type Store } from "./store.js";
import {
  generationKinds,
  publicKinds,
  visibleIn,
  type RouteDeps,
} from "./routes/deps.js";
import { registerDataRoutes } from "./routes/data.js";
import { registerPipelinesRoutes } from "./routes/pipelines.js";
import { registerRecordCollectionRoutes } from "./routes/record-collections.js";
import { recordLabel, registerOntologyRoutes } from "./routes/ontology.js";
import { registerAnalystRoutes } from "./routes/analyst.js";
import { registerWorkspaceRoutes } from "./routes/workspace.js";
// Helix Foundry runs on one computer for one person, with no sign-in. That
// person is the installation's owner; their session never expires because
// provider sign-in (hosting.ts) is bound to a session.
const LOCAL_USER = "local-user",
  LOCAL_SESSION = "local-session";
const isLoopback = (host: string) =>
  host === "localhost" ||
  host.endsWith(".localhost") ||
  host === "127.0.0.1" ||
  host === "::1" ||
  host === "[::1]";
// The web app may be opened as localhost or 127.0.0.1; both are this computer.
function sameApp(origin: string) {
  if (origin === config.origin) return true;
  try {
    const a = new URL(origin),
      b = new URL(config.origin);
    return (
      a.protocol === b.protocol &&
      a.port === b.port &&
      isLoopback(a.hostname) &&
      isLoopback(b.hostname)
    );
  } catch {
    return false;
  }
}
const scrub = publicResource;
export async function createApp(store: Store) {
  const app = Fastify({
    logger:
      process.env.NODE_ENV !== "test"
        ? {
            redact: [
              "req.headers.authorization",
              "req.headers.cookie",
              "body.password",
              "body.apiKey",
              "body.config",
            ],
            serializers: {
              req: (req: any) => ({
                method: req.method,
                url: req.url?.split("?")[0],
                hostname: req.hostname,
                remoteAddress: req.ip,
              }),
            },
          }
        : false,
    bodyLimit: 16 * 1024 * 1024,
  });
  await app.register(cookie);
  await app.register(multipart, {
    limits: { fileSize: 256 * 1024 * 1024, files: 1 },
  });
  await app.register(rateLimit, {
    max: config.rateLimit,
    timeWindow: "1 minute",
  });
  await app.register(swagger, {
    transform: openapiTransform,
    openapi: {
      info: { title: "Helix Foundry API", version: "0.1.0" },
      components: {
        securitySchemes: {
          token: { type: "http", scheme: "bearer" },
        },
      },
    },
  });
  await app.register(swaggerUI, { routePrefix: "/api/docs" });
  app.setErrorHandler((e: any, req, reply) => {
    const status = e instanceof z.ZodError ? 400 : e.statusCode || 500;
    reply.code(status).send({
      error:
        status === 500
          ? "Internal error; check server logs"
          : e instanceof z.ZodError
            ? e.issues
                .map((x: any) => x.path.join(".") + ": " + x.message)
                .join("; ")
            : e.message,
    });
    if (status === 500) req.log.error(e);
  });
  // With no sign-in, these checks keep other sites out: only names for this
  // computer may reach the API (a site re-pointed here by DNS rebinding may
  // not read it), and only the web app may change anything.
  const appHost = new URL(config.origin).hostname;
  app.addHook("onRequest", async (req) => {
    const host = req.headers.host ? req.hostname : "";
    assert(
      !host || isLoopback(host) || host === appHost,
      "Host is not allowed",
      403,
    );
    if (!["GET", "HEAD", "OPTIONS"].includes(req.method) && req.headers.origin)
      assert(sameApp(req.headers.origin), "Origin is not allowed", 403);
  });
  // Scripts and the SDK may still use an API token; a token that is wrong
  // fails rather than falling back to the local owner.
  async function user(req: any) {
    const header = req.headers.authorization;
    if (header === undefined) return localIdentity();
    const bearer = /^Bearer (.+)$/.exec(header)?.[1];
    assert(bearer, "Use an API token: Authorization: Bearer <token>", 401);
    const t = await store.get("global", digest(bearer));
    assert(
      t?.kind === "token" &&
        (!t.data.expiresAt || t.data.expiresAt > Date.now()),
      "API token is invalid or revoked",
      401,
    );
    const u = await store.get("global", t.data.userId);
    assert(u, "API token is invalid or revoked", 401);
    return { user: u, session: t };
  }
  async function access(
    req: any,
    min: "viewer" | "editor" | "owner" = "viewer",
  ) {
    const a = await user(req),
      scope = req.params.w;
    const member = await store.get("global", `${a.user.id}:${scope}`);
    assert(member?.kind === "member", "Workspace not found", 404);
    assert(
      !a.session.data.workspaceId || a.session.data.workspaceId === scope,
      "Workspace not found",
      404,
    );
    const roles = { viewer: 0, editor: 1, owner: 2 };
    assert(
      roles[member.data.role as Role] >= roles[min] &&
        !(
          a.session.kind === "token" &&
          a.session.data.readOnly &&
          min !== "viewer"
        ),
      "Insufficient workspace permissions",
      403,
    );
    return { ...a, scope, role: member.data.role };
  }
  async function newWorkspace(owner: Resource, name: string) {
    const id = randomUUID();
    await transaction(store, id, async (tx) =>
      tx.put(
        resource(
          id,
          "workspace",
          name,
          { activeGeneration: null, previousGeneration: null },
          "workspace",
        ),
      ),
    );
    await transaction(store, "global", async (tx) => {
      tx.put(resource("global", "workspaceRef", name, {}, id));
      tx.put(
        resource(
          "global",
          "member",
          owner.name,
          { userId: owner.id, role: "owner", workspaceId: id },
          `${owner.id}:${id}`,
        ),
      );
    });
    return { id, name };
  }
  // The first request sets up this computer's person and a workspace, so the
  // app opens straight into setup. An existing installation keeps its owner,
  // who becomes the owner of every workspace here.
  let local: Promise<{ user: Resource; session: Resource }> | undefined;
  function localIdentity() {
    local ??= provisionLocal().catch((e) => {
      local = undefined;
      throw e;
    });
    return local;
  }
  async function provisionLocal() {
    const identity = await transaction(store, "global", async (tx) => {
      const installation = await tx.get("installation");
      let u = installation && (await tx.get(installation.data.ownerId));
      if (u?.kind !== "user") {
        u = tx.put(resource("global", "user", "Local user", {}, LOCAL_USER));
        tx.put(
          installation
            ? { ...installation, data: { ...installation.data, ownerId: u.id } }
            : resource(
                "global",
                "installation",
                "Helix Foundry",
                { ownerId: u.id },
                "installation",
              ),
        );
      }
      let session = await tx.get(LOCAL_SESSION);
      if (session?.kind !== "session" || session.data.userId !== u.id)
        session = tx.put(
          resource(
            "global",
            "session",
            "Local session",
            { userId: u.id, expiresAt: Number.MAX_SAFE_INTEGER },
            LOCAL_SESSION,
          ),
        );
      for (const ref of await tx.list("workspaceRef")) {
        const m = await tx.get(`${u.id}:${ref.id}`);
        if (m?.kind === "member" && m.data.role === "owner") continue;
        tx.put(
          resource(
            "global",
            "member",
            u.name,
            { userId: u.id, role: "owner", workspaceId: ref.id },
            `${u.id}:${ref.id}`,
          ),
        );
      }
      return { user: u, session };
    });
    if (
      !(await store.list("global", "member")).some(
        (m) => m.data.userId === identity.user.id,
      )
    )
      await newWorkspace(identity.user, "My workspace");
    return identity;
  }
  app.get("/api/health", async () => ({ ok: true, service: "helix-foundry" }));
  app.get("/api/v1/me", async (req) => {
    const a = await user(req),
      members = (await store.list("global", "member")).filter(
        (m) =>
          m.data.userId === a.user.id &&
          (!a.session.data.workspaceId ||
            m.data.workspaceId === a.session.data.workspaceId),
      );
    return {
      user: { id: a.user.id, name: a.user.name, email: a.user.data.email },
      workspaces: await Promise.all(
        members.map(async (m) => ({
          id: m.data.workspaceId,
          name: (await store.get(m.data.workspaceId, "workspace"))?.name,
          role: m.data.role,
        })),
      ),
    };
  });
  app.post("/api/v1/workspaces", async (req) => {
    const a = await user(req);
    assert(
      a.session.kind === "session",
      "API tokens can't create workspaces",
      403,
    );
    return newWorkspace(
      a.user,
      z.object({ name: z.string().min(1).max(100) }).parse(req.body).name,
    );
  });
  app.get("/api/v1/workspaces/:w/resources/:kind", async (req: any) => {
    const a = await access(req);
    assert(publicKinds.has(req.params.kind), "Resource type not found", 404);
    const ws = await store.get(a.scope, "workspace");
    let rows = (await store.list(a.scope, req.params.kind)).filter(
      (r) =>
        (!generationKinds.has(r.kind) &&
          !(r.kind === "dataset" && r.data.generation)) ||
        r.data.generation === ws?.data.activeGeneration,
    );
    if (req.params.kind === "dataset")
      rows = await Promise.all(
        rows.map(async (r) => ({
          ...r,
          data: {
            ...r.data,
            snapshotAt: (await store.get(a.scope, r.data.activeVersion))
              ?.createdAt,
          },
        })),
      );
    if (req.params.kind === "object") {
      // Records carry a readable name and how many records they link to.
      const links = new Map<string, number>();
      for (const r of await store.list(a.scope, "relation"))
        if (r.data.generation === ws?.data.activeGeneration)
          for (const end of [r.data.from, r.data.to])
            links.set(end, (links.get(end) || 0) + 1);
      rows = await Promise.all(
        rows.map(async (r) => {
          const o = await effectiveObject(store, a.scope, r);
          return {
            ...o,
            data: {
              ...o.data,
              label: recordLabel(o.name, o.data.type, o.data.effective),
              links: links.get(o.id) || 0,
            },
          };
        }),
      );
    }
    if (req.query.search)
      rows = rows.filter((r) =>
        JSON.stringify([r.name, r.description, r.data.effective || r.data.base])
          .toLowerCase()
          .includes(String(req.query.search).toLowerCase()),
      );
    if (req.query.type)
      rows = rows.filter((r) => r.data.type === req.query.type);
    if (req.query.field && req.query.value !== undefined) {
      assert(req.params.kind === "object", "Field filters apply to records");
      rows = rows.filter(
        (r) =>
          Object.hasOwn(r.data.effective, String(req.query.field)) &&
          String(r.data.effective[String(req.query.field)]) ===
            String(req.query.value),
      );
    }
    const total = rows.length,
      offset = Math.max(Number(req.query.offset) || 0, 0),
      limit = Math.max(1, Math.min(Number(req.query.limit) || 100, 500));
    // Records sort by name (?sort=record), a field, or their link count
    // (?sort=links), with ?dir=desc to reverse; empty values go last.
    const field = String(req.query.sort || ""),
      dir = req.query.dir === "desc" ? -1 : 1,
      valueOf = (r: Resource) =>
        field === "links" ? r.data.links : r.data.effective?.[field],
      blank = (v: unknown) =>
        v == null || ["", "None", "null", "NaN"].includes(String(v));
    const byField = (x: Resource, y: Resource) => {
      const a = valueOf(x),
        b = valueOf(y);
      if (blank(a) || blank(b)) return Number(blank(a)) - Number(blank(b));
      const n = Number(a) - Number(b);
      return (
        dir *
        (Number.isFinite(n) &&
        String(a).trim() !== "" &&
        String(b).trim() !== ""
          ? n
          : String(a).localeCompare(String(b), undefined, { numeric: true }))
      );
    };
    rows = rows
      .sort((x, y) =>
        req.params.kind === "object"
          ? (field && field !== "record" ? byField(x, y) : 0) ||
            (field === "record" ? dir : 1) *
              String(x.data.label || x.name).localeCompare(
                String(y.data.label || y.name),
                undefined,
                { numeric: true },
              ) ||
            x.id.localeCompare(y.id)
          : y.updatedAt.localeCompare(x.updatedAt) || x.id.localeCompare(y.id),
      )
      .slice(offset, offset + limit);
    return { items: rows.map(scrub), total };
  });
  app.get("/api/v1/workspaces/:w/resources/:kind/:id", async (req: any) => {
    const a = await access(req);
    assert(publicKinds.has(req.params.kind), "Resource type not found", 404);
    const r = await store.get(a.scope, req.params.id);
    assert(r && r.kind === req.params.kind, "Resource not found", 404);
    if (
      generationKinds.has(r.kind) ||
      (r.kind === "dataset" && r.data.generation)
    )
      assert(
        r.data.generation ===
          (await store.get(a.scope, "workspace"))?.data.activeGeneration,
        "Resource not found",
        404,
      );
    return scrub(
      r.kind === "object" ? await effectiveObject(store, a.scope, r) : r,
    );
  });
  app.get("/api/openapi.json", async () => app.swagger());
  const web = resolve("apps/web/dist");
  if (existsSync(web)) {
    await app.register(staticFiles, { root: web });
    app.setNotFoundHandler((req, reply) =>
      req.url.startsWith("/api/")
        ? reply.code(404).send({ error: "Not found" })
        : reply.sendFile("index.html"),
    );
  }
  registerOnboarding(app, store, access);
  registerHosting(app, store, access);
  // Area route modules; each area owns its own file under ./routes.
  const deps: RouteDeps = {
    access,
    scrub,
    activeGeneration: async (scope) =>
      (await store.get(scope, "workspace"))?.data.activeGeneration ?? null,
    visibleIn,
  };
  registerDataRoutes(app, store, deps);
  registerPipelinesRoutes(app, store, deps);
  registerOntologyRoutes(app, store, deps);
  registerRecordCollectionRoutes(app, store, deps);
  registerAnalystRoutes(app, store, deps);
  registerWorkspaceRoutes(app, store, deps);
  return app;
}
