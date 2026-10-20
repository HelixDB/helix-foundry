#!/usr/bin/env node
// Read-only MCP server for a local Helix Foundry instance.
// Env: FOUNDRY_TOKEN (required, from Settings → Developer tools),
//      FOUNDRY_URL (default http://localhost:3001),
//      FOUNDRY_WORKSPACE (optional; detected from the token if omitted).
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

const BASE = (process.env.FOUNDRY_URL || "http://localhost:3001").replace(/\/$/, "");
const TOKEN = process.env.FOUNDRY_TOKEN;
if (!TOKEN) {
  console.error("FOUNDRY_TOKEN is not set. Create one in Helix Foundry → Settings → Developer tools.");
  process.exit(1);
}

async function http(path, body, method) {
  const r = await fetch(BASE + "/api/v1" + path, {
    method: method || (body === undefined ? "GET" : "POST"),
    headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await r.text();
  let data;
  try { data = JSON.parse(text); } catch { data = text; }
  if (!r.ok) throw new Error(`Foundry ${r.status}: ${data?.error ?? text}`);
  return data;
}

let workspaceId = process.env.FOUNDRY_WORKSPACE;
async function ws(path, body, method) {
  if (!workspaceId) {
    const me = await http("/me");
    workspaceId = me.workspaces?.[0]?.id;
    if (!workspaceId) throw new Error("No workspace found for this token. Set FOUNDRY_WORKSPACE.");
  }
  return http(`/workspaces/${workspaceId}${path}`, body, method);
}

const out = (v) => ({ content: [{ type: "text", text: typeof v === "string" ? v : JSON.stringify(v, null, 2) }] });
const fail = (e) => ({ isError: true, content: [{ type: "text", text: String(e?.message ?? e) }] });
const safe = (fn) => async (args) => { try { return out(await fn(args)); } catch (e) { return fail(e); } };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const KINDS = ["dataset", "version", "source", "pipeline", "proposal", "run", "dashboard", "objectType",
  "object", "relation", "actionType", "audit", "finding", "publication", "query", "job", "lineage"];

const server = new McpServer({ name: "helix-foundry", version: "0.1.0" });

server.tool(
  "ask_question",
  "Ask Helix Foundry's Analyst a question about the company data. It writes and runs SQL over the connected sources and returns a summary, the SQL, result rows and snapshot citations. Can take a minute or two with a local model.",
  { question: z.string().describe("Plain-language question, e.g. 'Top 10 customers by revenue last quarter'"),
    timeout_seconds: z.number().int().min(10).max(600).optional().describe("How long to wait (default 240)") },
  safe(async ({ question, timeout_seconds = 240 }) => {
    let run = await ws("/assistant/runs", { goal: question, intent: "answer" });
    const deadline = Date.now() + timeout_seconds * 1000;
    while (["queued", "running"].includes(run?.data?.status) && Date.now() < deadline) {
      await sleep(2000);
      run = await ws(`/resources/run/${run.id}`);
    }
    const d = run.data || {};
    if (["queued", "running"].includes(d.status))
      return { status: d.status, runId: run.id, note: "Still running. Call get_run with this runId to check later." };
    const a = d.answer || {};
    const rows = Array.isArray(a.rows) ? a.rows : undefined;
    return {
      status: d.status, runId: run.id,
      summary: a.summary, sql: a.sql,
      rowCount: rows?.length, rows: rows?.slice(0, 100),
      citations: a.citations, check: a.check,
      error: d.error,
    };
  }),
);

server.tool(
  "get_run",
  "Fetch an Analyst run by id (status, answer, SQL, rows).",
  { runId: z.string() },
  safe(async ({ runId }) => ws(`/resources/run/${encodeURIComponent(runId)}`)),
);

server.tool(
  "workspace_home",
  "Workspace summary: connection health, ontology snapshot, open findings, pending reviews, recent activity, dashboards.",
  {},
  safe(() => ws("/home")),
);

server.tool(
  "list_resources",
  "List resources of a kind (datasets, ontology object types, objects, relations, dashboards, sources, ...). Use 'dataset' to discover dataset ids for run_sql, 'objectType' to see the ontology.",
  { kind: z.enum(KINDS), search: z.string().optional(),
    offset: z.number().int().min(0).optional(), limit: z.number().int().min(1).max(100).optional() },
  safe(({ kind, search, offset = 0, limit = 50 }) => {
    const q = new URLSearchParams({ offset: String(offset), limit: String(limit) });
    if (search) q.set("search", search);
    return ws(`/resources/${kind}?${q}`);
  }),
);

server.tool(
  "get_resource",
  "Get one resource by kind and id.",
  { kind: z.enum(KINDS), id: z.string() },
  safe(({ kind, id }) => ws(`/resources/${kind}/${encodeURIComponent(id)}`)),
);

server.tool(
  "get_record",
  "Get an ontology record by logical id, with effective values, related records and action history.",
  { logicalId: z.string() },
  safe(({ logicalId }) => ws(`/records/${encodeURIComponent(logicalId)}`)),
);

server.tool(
  "get_neighbors",
  "Graph traversal: ids of objects directly linked to an ontology object.",
  { objectId: z.string() },
  safe(({ objectId }) => ws(`/objects/${encodeURIComponent(objectId)}/neighbors`)),
);

server.tool(
  "preview_dataset",
  "Preview rows and the column profile of a dataset.",
  { datasetId: z.string(), offset: z.number().int().min(0).optional(), limit: z.number().int().min(1).max(500).optional() },
  safe(({ datasetId, offset = 0, limit = 50 }) => ws(`/datasets/${encodeURIComponent(datasetId)}/preview`, { offset, limit })),
);

server.tool(
  "run_sql",
  "Run read-only DuckDB SQL over one or more datasets. Pass the dataset ids as inputs; use preview_dataset or the dataset resource to learn table and column names.",
  { inputs: z.array(z.string()).min(1).describe("Dataset ids"), sql: z.string() },
  safe(({ inputs, sql }) => ws("/query", { inputs, sql })),
);

await server.connect(new StdioServerTransport());
