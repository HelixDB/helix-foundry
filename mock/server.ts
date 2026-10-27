import { createServer, type IncomingMessage } from "node:http";
import { compileTemplate, matchTemplate } from "../src/shared/route-template";
import { fail, type MockReply, type Principal } from "./http";
import { routes } from "./routes";
import { handleBillingPage } from "./routes/billing";
import { handleDocumentUpload } from "./routes/documents";
import { sendPieces } from "./routes/event-sources";
import {
  LOCAL_TOKEN,
  SEED_EMAIL,
  config,
  db,
  resetState,
  stats,
} from "./state";
import { verify } from "./tokens";
import { handleWorkos } from "./workos";

// The mock WFE: a small HTTP server with in-memory state that answers the
// WFE routes the app calls (mock/routes) with the swagger's shapes, plus the
// WorkOS endpoints AuthKit uses and a fake sign-in page. `pnpm dev:mock` and
// `pnpm e2e` run it; it is never deployed.
//
// With MOCK_LOCAL_TOKEN it is a local build's API instead (mock/state.ts).
//
// Test hooks: POST /__mock/reset, GET /__mock/stats, and POST /__mock/config
// with { refuseLocalToken, accessTokenTtlSeconds, allowedRegions, checkoutCardOnFile,
// allowedEmails, ontologyStepMs, ontologyOutcome, documentUploadFailures,
// loadStepMs, loadOutcome, documentOutcome, helixSubscription,
// ontologyDatabaseHidden, startingQueries, readErrors, s3RoleAccess,
// legacyDocumentSets, dataLayoutConflicts, enrichmentStepMs,
// enrichmentOutcome, documentsPerStream, eventSyncMs, eventCaps,
// eventSetAllowanceSpent, aiProbeMs, aiProbeOutcome, aiDefault, aiKinds }.
//
// POST /__mock/event-sources/{connectorId}/pieces sends pieces to an API
// source, as an agent does through the Helix MCP: { clientToken?, pieces:
// [{ id, occurredAt?, data }] }, answered as SendFoundryEvents answers. The
// WFE has no route that sends pieces, so neither has the app.
//
// /__uploads/ stands in for the bucket a document's presigned PUT goes to:
// binary bodies with CORS, so it reads its own body.

const port = Number(process.env.MOCK_PORT || 4010);
process.env.MOCK_ORIGIN ||= `http://127.0.0.1:${port}`;

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let data = "";
    req.setEncoding("utf8");
    req.on("data", (chunk) => (data += chunk));
    req.on("end", () => resolve(data));
    req.on("error", reject);
  });
}

async function principal(req: IncomingMessage): Promise<Principal | null> {
  const [scheme, token] = (req.headers.authorization || "").split(" ");
  if (scheme !== "Bearer" || !token) return null;
  if (LOCAL_TOKEN) {
    const owner = [...db.users.values()].find((u) => u.email === SEED_EMAIL);
    return token === LOCAL_TOKEN && !config.refuseLocalToken && owner
      ? { user: owner, sessionId: "local" }
      : null;
  }
  const found = await verify(token);
  return found && { user: found.user, sessionId: found.session.id };
}

async function wfe(
  req: IncomingMessage,
  url: URL,
  raw: string,
): Promise<MockReply> {
  const method = (req.method || "GET").toUpperCase();
  for (const route of compiled) {
    const params =
      route.method === method && matchTemplate(route.c, url.pathname);
    if (!params) continue;
    const caller = route.public ? null : await principal(req);
    if (!route.public && !caller)
      return fail("UNAUTHENTICATED", "missing or invalid bearer token");
    let body: unknown;
    try {
      body = raw ? JSON.parse(raw) : undefined;
    } catch {
      return fail("INVALID_ARGUMENT", "invalid JSON body");
    }
    return route.handle({
      method: route.method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: caller!,
    });
  }
  return fail("NOT_FOUND", "Not Found");
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url || "/", process.env.MOCK_ORIGIN);
  try {
    if (await handleDocumentUpload(req, res, url)) return;
    const raw = await readBody(req);
    if (url.pathname.startsWith("/__mock/")) {
      const send = /^\/__mock\/event-sources\/([^/]+)\/pieces$/.exec(
        url.pathname,
      );
      if (send && req.method === "POST") {
        const reply = sendPieces(
          decodeURIComponent(send[1]!),
          JSON.parse(raw || "{}"),
        );
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(reply.body));
        return;
      }
      if (url.pathname === "/__mock/reset" && req.method === "POST")
        resetState();
      else if (url.pathname === "/__mock/config" && req.method === "POST")
        Object.assign(config, JSON.parse(raw || "{}"));
      else if (url.pathname !== "/__mock/stats") {
        res.writeHead(404).end();
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ stats, config }));
      return;
    }
    if (await handleWorkos(req, res, url, raw)) return;
    if (handleBillingPage(req, res, url, raw)) return;
    const reply = url.pathname.startsWith("/v1/")
      ? await wfe(req, url, raw)
      : fail("NOT_FOUND", "Not Found");
    if (url.pathname.startsWith("/v1/"))
      stats.requests.push(`${req.method} ${url.pathname} ${reply.status}`);
    res.writeHead(reply.status, {
      "Content-Type": "application/json",
      ...reply.headers,
    });
    res.end(reply.body === undefined ? "" : JSON.stringify(reply.body));
  } catch (error) {
    console.error("mock:", error);
    res.writeHead(500, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ code: 13, message: "mock failure", details: [] }));
  }
});

resetState();
server.listen(port, "127.0.0.1", () =>
  console.log(`Mock WFE on ${process.env.MOCK_ORIGIN}`),
);
