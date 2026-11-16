import { createServer, type IncomingMessage } from "node:http";
import { compileTemplate, matchTemplate } from "../src/shared/route-template";
import { fail, type MockReply, type Principal } from "./http";
import { handleReminders, mockReminders } from "./reminders";
import { handleSharedDashboard } from "./routes/dashboards";
import { mockFeedback } from "./routes/feedback";
import { routes } from "./routes";
import { handleDirectoryPage } from "./routes/access";
import { pairHelper, startHelper } from "./routes/agent-hosts";
import { pastRuns, pruneRun } from "./routes/automations";
import { handleBillingPage } from "./routes/billing";
import { handleDocumentUpload } from "./routes/documents";
import { sendPieces } from "./routes/event-sources";
import { handleConnectPage } from "./routes/sign-ins";
import {
  LOCAL_TOKEN,
  SEED_EMAIL,
  applyConfig,
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
// allowedEmails, ontologyStepMs, ontologyOutcome, agentStepMs, agentOutcome,
// agentActionTtlMs, agentHosts, documentUploadFailures,
// loadStepMs, loadOutcome, documentOutcome, helixSubscription,
// ontologyDatabaseHidden, startingQueries, indexesPending, readErrors,
// s3RoleAccess, legacyDocumentSets, dataLayoutConflicts, enrichmentStepMs,
// enrichmentOutcome, documentsPerStream, eventSyncMs, eventCaps,
// eventSetAllowanceSpent, aiProbeMs, aiProbeOutcome, aiDefault,
// aiDefaultKind, aiDefaultNamed, aiDetect, ollamaBundled, aiKinds,
// memberQueryRead, invoiceOnlyCustomers, healEnabled, healRun, healDismiss,
// healRowsMeasured, freshInstall, signIn, signInsRunOut, accountBroken,
// githubInstalled, directoryTeams, directoryEntitled, directoryIssues,
// directorySyncMs, remindersPerHour, remindersPerClientPerHour, assessEnabled,
// assessMs, assessOutcome, automationClockMs, automationStepMs,
// automationRunsPerDay, automationsDisabled, automationAnswer,
// automationCount, automationSuggestionsMs, automationSuggestionsOutcome,
// automationSuggestionsAi }.
//
// POST /v1/reminders and /v1/reminders/stop are Foundry's door's, not the
// WFE's: the landing page's reminders (./reminders.ts), which the site's
// server calls with the mock's secret. GET /__mock/reminders lists them.
//
// GET /__mock/password-resets lists the reset links WorkOS would have
// emailed, oldest first: [{ email, token }] (./routes/auth.ts).
//
// GET /__mock/feedback lists the feedback POST /v1/feedback took
// (./routes/feedback.ts), which the WFE would tell Helix's team of.
//
// /mock/directory-portal is WorkOS's Admin Portal for directory sync
// (./routes/access.ts).
// /mock/connect is a provider's consent page for connecting a source by
// signing in (./routes/sign-ins.ts), as /mock/checkout is Stripe Checkout.
//
// POST /__mock/event-sources/{connectorId}/pieces sends pieces to an API
// source, as an agent does through the Helix MCP: { clientToken?, pieces:
// [{ id, occurredAt?, data }], workflowRunId? }, answered as
// SendFoundryEvents answers. The WFE has no route that sends pieces, so
// neither has the app.
//
// POST /__mock/automations/{id}/past-runs { runs: [{ status, ago,
// usdMicros? }] } records ended runs of a workflow (an automation) that long
// ago (ms), for the history the workflows' overview counts
// (./routes/automations.ts).
//
// POST /__mock/automations/{id}/runs/{runId}/prune drops an ended run, as the
// backend's retention does; the use case runs it started stay.
//
// POST /__mock/agent-host/pair { code, name?, runtimes?, account?,
// codexVersion? } spends a pairing code as the Assistant's helper's pair
// command does (./routes/agent-hosts.ts): the helper it pairs offers
// runtimes (Claude Code by default), and runs once
// POST /__mock/agent-host/hello { host_id } starts it.
//
// /__mock/agent/v1/... calls the WFE route /v1/... as Ada's agent does
// through the Helix MCP, with no token: starting and finishing a use case's
// runs, and reading and asking under one, which the app never does.
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
  as?: Principal,
): Promise<MockReply> {
  const method = (req.method || "GET").toUpperCase();
  for (const route of compiled) {
    const params =
      route.method === method && matchTemplate(route.c, url.pathname);
    if (!params) continue;
    const caller = route.public ? null : as || (await principal(req));
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

/** A WFE call as Ada's agent makes it through the Helix MCP. */
function asAgent(req: IncomingMessage, url: URL, raw: string) {
  const ada = [...db.users.values()].find((u) => u.email === SEED_EMAIL);
  if (!ada) return fail("UNAUTHENTICATED", "Ada has not signed in");
  const wfeUrl = new URL(
    url.pathname.slice("/__mock/agent".length) + url.search,
    url,
  );
  return wfe(req, wfeUrl, raw, { user: ada, sessionId: "agent" });
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
      const past = /^\/__mock\/automations\/([^/]+)\/past-runs$/.exec(
        url.pathname,
      );
      const prune =
        /^\/__mock\/automations\/([^/]+)\/runs\/([^/]+)\/prune$/.exec(
          url.pathname,
        );
      if (prune && req.method === "POST") {
        const reply = pruneRun(
          decodeURIComponent(prune[1]!),
          decodeURIComponent(prune[2]!),
        );
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(reply.body));
        return;
      }
      if (past && req.method === "POST") {
        const reply = pastRuns(
          decodeURIComponent(past[1]!),
          JSON.parse(raw || "{}"),
        );
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(reply.body));
        return;
      }
      if (
        (url.pathname === "/__mock/agent-host/pair" ||
          url.pathname === "/__mock/agent-host/hello") &&
        req.method === "POST"
      ) {
        const body = JSON.parse(raw || "{}");
        const reply =
          url.pathname === "/__mock/agent-host/pair"
            ? pairHelper(body)
            : startHelper(body);
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(reply.body));
        return;
      }
      if (url.pathname === "/__mock/feedback" && req.method === "GET") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ feedback: mockFeedback() }));
        return;
      }
      if (url.pathname === "/__mock/password-resets" && req.method === "GET") {
        const resets = [...db.passwordResets].map(([token, userId]) => ({
          email: db.users.get(userId)?.email,
          token,
        }));
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ resets }));
        return;
      }
      if (url.pathname === "/__mock/reminders" && req.method === "GET") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ reminders: mockReminders() }));
        return;
      }
      if (url.pathname.startsWith("/__mock/agent/v1/")) {
        const reply = await asAgent(req, url, raw);
        res.writeHead(reply.status, { "Content-Type": "application/json" });
        res.end(reply.body === undefined ? "" : JSON.stringify(reply.body));
        return;
      }
      if (url.pathname === "/__mock/reset" && req.method === "POST")
        resetState();
      else if (url.pathname === "/__mock/config" && req.method === "POST") {
        Object.assign(config, JSON.parse(raw || "{}"));
        applyConfig();
      } else if (url.pathname !== "/__mock/stats") {
        res.writeHead(404).end();
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ stats, config }));
      return;
    }
    if (await handleWorkos(req, res, url, raw)) return;
    if (handleBillingPage(req, res, url, raw)) return;
    if (handleConnectPage(req, res, url, raw)) return;
    if (handleDirectoryPage(req, res, url)) return;
    // Foundry's door's reminder routes (ingest.helix-db.com), not the WFE's.
    if (handleReminders(req, res, url, raw)) return;
    // And its dashboards' public snapshots.
    if (handleSharedDashboard(req, res, url)) return;
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
