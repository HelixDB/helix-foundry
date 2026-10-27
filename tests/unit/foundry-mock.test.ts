import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FOUNDRY_NODES } from "@/shared/foundry-labels";
import type { HttpMethod } from "@/shared/route-template";
import type { FoundryPropValue } from "@/shared/wfe";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import { contractErrors } from "../../mock/agent-contract";
import { config } from "../../mock/state";
import type { MockReply } from "../../mock/http";
import {
  decisionValue,
  decisions,
  pruneDecisions,
} from "../../mock/review-decisions";
import { routes } from "../../mock/routes";
import { tenantGraph } from "../../mock/routes/databases";
import { MAX_PAGES, textPdf } from "../../mock/pdf-text";
import { FORMATS, handleDocumentUpload } from "../../mock/routes/documents";
import { sendPieces } from "../../mock/routes/event-sources";
import { db, resetState } from "../../mock/state";

// The mock WFE's ontologies, Helix databases, documents and loads behave as
// the revised Foundry routes and helix-website's Helix routes do: the e2e
// suite and dev:mock rely on their refusals and states.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    return (await r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const refusal = (r: MockReply & { body: any }) => [r.status, r.body.message];

const W = "/v1/workspaces/ws_acme/foundry/ontologies";
const O = `${W}/ont_acme_contracts`;
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
const sha = (b: Buffer) => createHash("sha256").update(b).digest("hex");
/** A born-digital PDF of two pages, naming a Stripe customer. */
const TERMS = textPdf([
  ["Renewal terms", "Globex Corporation renews on 1 December 2026."],
  ["Notice", "Either party gives 90 days notice."],
]);

/** The bucket's side of a presigned PUT, as the browser sends it. */
async function put(
  url: string,
  bytes: Buffer,
  headers: Record<string, string>,
  method = "PUT",
) {
  const u = new URL(url);
  const req = Object.assign(Readable.from([bytes]), {
    method,
    headers: Object.fromEntries(
      Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]),
    ),
  }) as unknown as IncomingMessage;
  let status = 0,
    text = "",
    head: Record<string, string> = {};
  const res = {
    setHeader: () => res,
    writeHead: (s: number, h?: Record<string, string>) => (
      (status = s),
      (head = h || {}),
      res
    ),
    end: (body?: string) => {
      text = body || "";
    },
  } as unknown as ServerResponse;
  expect(await handleDocumentUpload(req, res, u)).toBe(true);
  return { status, text, head };
}

const WS = "/v1/workspaces/ws_acme/foundry";
const DOCS = `${WS}/documents`,
  SETS = `${WS}/document-sets`;
/** Acme's General set: where uploads go unless told otherwise. */
const GENERAL = "set_acme_general";

/** A file's media type by its extension, as the site sends it. */
const typeOf = (name: string) =>
  Object.entries(FORMATS).find(([, f]) => f.extensions.test(name))?.[0];

/** Creates a document in a set (General by default), PUTs its bytes and completes it. */
async function uploaded(name: string, bytes: Buffer, setId = GENERAL) {
  const created = await call("POST", DOCS, {
    name,
    mediaType: typeOf(name),
    sizeBytes: String(bytes.length),
    sha256: sha(bytes),
    clientToken: `tok-${name}`,
    setId,
  });
  expect(created.status, JSON.stringify(created.body)).toBe(200);
  const { document, upload } = created.body;
  if (upload)
    expect((await put(upload.url, bytes, upload.headers)).status).toBe(200);
  const done = await call("POST", `${DOCS}/${document.id}:complete`, {});
  expect(done.status).toBe(200);
  return done.body.document;
}
/** What completing or moving a document did, by ontology. */
const outcomes = (r: MockReply & { body: any }) =>
  Object.fromEntries(
    (r.body.reads as { ontologyId: string; outcome: string }[]).map((x) => [
      x.ontologyId,
      x.outcome.replace("FOUNDRY_DOCUMENT_READ_OUTCOME_", ""),
    ]),
  );

const read = async (tenantId: string, request: unknown) => {
  const r = await call("POST", "/v1/databases:query-read", {
    database: { tenantId },
    queryJson: Buffer.from(JSON.stringify(request)).toString("base64"),
  });
  expect(r.status).toBe(200);
  return {
    statusCode: r.body.statusCode,
    body: JSON.parse(Buffer.from(r.body.responseJson, "base64").toString()),
  };
};
const count = (label: string) => ({
  request_type: "read",
  query: {
    read: {
      entries: [
        {
          query: {
            name: "n",
            root: {
              count: {
                input: {
                  nodes_where: {
                    predicate: {
                      eq: {
                        left: { property: "$label" },
                        right: { constant: { string: label } },
                      },
                    },
                  },
                },
              },
            },
          },
        },
      ],
      returns: ["n"],
    },
  },
});

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-18T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("mock Helix projects and databases", () => {
  it("lists the workspace's projects and their databases, deleted ones too", async () => {
    const projects = await call("GET", "/v1/projects?workspaceId=ws_acme");
    expect(projects.body.projects.map((p: any) => p.displayName)).toEqual([
      "Staging",
      "Production",
    ]);
    const staging = await call("GET", "/v1/tenants?projectId=prj_acme_staging");
    expect(staging.body.tenants.map((t: any) => [t.name, t.status])).toEqual(
      expect.arrayContaining([
        ["sandbox", "RESOURCE_STATUS_ACTIVE"],
        ["old-orders", "RESOURCE_STATUS_DELETED"],
      ]),
    );
    // Pages of page.size, with an opaque token.
    const page = await call(
      "GET",
      "/v1/projects?workspaceId=ws_acme&page.size=1",
    );
    expect(page.body.projects).toHaveLength(1);
    expect(
      (
        await call(
          "GET",
          `/v1/projects?workspaceId=ws_acme&page.size=1&page.token=${page.body.nextPageToken}`,
        )
      ).body.projects[0].displayName,
    ).toBe("Production");
    expect(
      (await call("GET", "/v1/projects?workspaceId=ws_other")).status,
    ).toBe(403);
    expect((await call("GET", "/v1/tenants?projectId=nope")).status).toBe(404);
  });

  it("creates projects and shared-cluster databases for owners and admins", async () => {
    const project = await call("POST", "/v1/projects", {
      workspaceId: "ws_acme",
      slug: "foundry",
      displayName: "Foundry",
    });
    expect(project.body).toMatchObject({
      slug: "foundry",
      workspaceId: "ws_acme",
    });
    // A slug in use takes the next free number, as CreateProject allocates.
    for (const n of [2, 3])
      expect(
        (
          await call("POST", "/v1/projects", {
            workspaceId: "ws_acme",
            slug: "foundry",
            displayName: "Again",
          })
        ).body,
      ).toMatchObject({ slug: `foundry-${n}`, displayName: `Again ${n}` });
    expect(
      (
        await call("POST", "/v1/projects", {
          workspaceId: "ws_acme",
          slug: "Not A Slug",
          displayName: "x",
        })
      ).status,
    ).toBe(400);
    const tenant = await call("POST", "/v1/tenants", {
      projectId: project.body.id,
      name: "Renewals",
      slug: "renewals",
    });
    expect(tenant.body.tenant).toMatchObject({
      name: "Renewals",
      status: "RESOURCE_STATUS_ACTIVE",
      clusterId: "clu_shared_use2",
    });
    expect(
      refusal(
        await call("POST", "/v1/tenants", {
          projectId: project.body.id,
          name: "Again",
          slug: "renewals",
        }),
      ),
    ).toEqual([409, 'tenant "renewals" already exists in cluster']);
    expect(
      (
        await call("POST", "/v1/tenants", {
          projectId: project.body.id,
          name: "Dedicated",
          slug: "dedicated",
          clusterId: "clu_dedicated",
        })
      ).status,
    ).toBe(400);
    expect(
      (
        await call(
          "POST",
          "/v1/tenants",
          { projectId: "prj_acme_prod", name: "x", slug: "x" },
          "usr_alan",
        )
      ).status,
    ).toBe(403);
  });

  it("gives a workspace that never had a Helix plan its free one on its first database", async () => {
    const ws = (
      await call("POST", "/v1/workspaces", {
        slug: "globex",
        displayName: "Globex",
        region: "us-east-2",
      })
    ).body.id;
    const project = (
      await call("POST", "/v1/projects", {
        workspaceId: ws,
        slug: "globex",
        displayName: "Globex",
      })
    ).body.id;
    // The first database is granted the plan, and the next has it.
    for (const slug of ["ledger", "ledger-2"])
      expect(
        (
          await call("POST", "/v1/tenants", {
            projectId: project,
            name: slug,
            slug,
          })
        ).body.tenant,
      ).toMatchObject({
        name: slug,
        workspaceId: ws,
        status: "RESOURCE_STATUS_ACTIVE",
      });
  });

  it("refuses a database once every Helix plan lapsed, with its ErrorInfo, granting none", async () => {
    Object.assign(config, { helixSubscription: false });
    const ws = (
      await call("POST", "/v1/workspaces", {
        slug: "globex",
        displayName: "Globex",
        region: "us-east-2",
      })
    ).body.id;
    const project = (
      await call("POST", "/v1/projects", {
        workspaceId: ws,
        slug: "globex",
        displayName: "Globex",
      })
    ).body.id;
    // Acme's plan and a new workspace's alike: a lapsed plan is not granted
    // again, and nothing is created.
    for (const [workspaceId, projectId] of [
      ["ws_acme", "prj_acme_prod"],
      [ws, project],
    ]) {
      const refused = await call("POST", "/v1/tenants", {
        projectId,
        name: "Pipeline",
        slug: "pipeline",
      });
      expect(refused.status).toBe(400);
      expect(refused.body).toMatchObject({
        code: 9,
        message: "this workspace has no active subscription",
        details: [
          {
            "@type": "type.googleapis.com/google.rpc.ErrorInfo",
            reason: "NO_WORKSPACE_SUBSCRIPTION",
            domain: "billing.helix-db.com",
            metadata: { workspace_id: workspaceId, state: "expired" },
          },
        ],
      });
      const listed = await call("GET", `/v1/tenants?projectId=${projectId}`);
      expect(
        listed.body.tenants.map((t: { slug: string }) => t.slug),
      ).not.toContain("pipeline");
    }
  });

  it("answers query-read over a database's graph, for its workspace only", async () => {
    const answer = await read("ten_acme_orders", count("Account"));
    expect(answer).toEqual({ statusCode: 200, body: { n: 3 } });
    // A request the database can't decode is its own 400, inside a 200.
    const bad = await read("ten_acme_orders", { request_type: "read" });
    expect(bad.statusCode).toBe(400);
    expect(bad.body.error).toBe("invalid_query_json");
    for (const [database, status] of [
      [{ tenantId: "ten_acme_old" }, 404],
      [{ tenantId: "nope" }, 404],
      [{ clusterId: "clu_shared_use2" }, 400],
    ] as const)
      expect(
        (
          await call("POST", "/v1/databases:query-read", {
            database,
            queryJson: "",
          })
        ).status,
      ).toBe(status);
    expect(
      (
        await call("POST", "/v1/databases:query-read", {
          database: { tenantId: "ten_acme_orders" },
          queryJson: "!!!",
        })
      ).status,
    ).toBe(400);
  });

  it("refuses a database's first reads while it is starting, as the gateway does", async () => {
    Object.assign(config, { startingQueries: { orders: 2 } });
    const refused = {
      statusCode: 503,
      body: {
        error: "rate_limit_unavailable",
        msg: "tenant rate limit is unavailable",
      },
    };
    expect(await read("ten_acme_orders", count("Account"))).toEqual(refused);
    // Only that database.
    expect(await read("ten_acme_sandbox", count("Account"))).toEqual({
      statusCode: 200,
      body: { n: 0 },
    });
    expect(await read("ten_acme_orders", count("Account"))).toEqual(refused);
    expect(await read("ten_acme_orders", count("Account"))).toEqual({
      statusCode: 200,
      body: { n: 3 },
    });
    // A reset forgets it.
    Object.assign(config, { startingQueries: { orders: 1 } });
    resetState();
    expect((await read("ten_acme_orders", count("Account"))).statusCode).toBe(
      200,
    );
  });
});

describe("mock ontologies", () => {
  it("binds one ontology to one live database of the workspace", async () => {
    const list = await call("GET", W);
    // Newest first: Renewals is older.
    expect(list.body.ontologies).toEqual([
      expect.objectContaining({
        id: "ont_acme_contracts",
        tenantId: "ten_acme_contracts",
        databaseName: "contracts",
      }),
      expect.objectContaining({
        id: "ont_acme_sales",
        tenantId: "ten_acme_sales",
        databaseName: "sales",
      }),
      expect.objectContaining({
        id: "ont_acme_renewals",
        tenantId: "ten_acme_subscriptions",
        currentVersionId: "ver_renewals_1",
      }),
    ]);
    const made = await call("POST", W, {
      name: "Orders",
      tenantId: "ten_acme_orders",
      clientToken: "a",
    });
    expect(made.body.ontology).toMatchObject({
      name: "Orders",
      projectId: "prj_acme_prod",
      currentVersionId: "",
      lastLoad: null,
      counts: { documents: 0 },
    });
    // The same client token is the same ontology.
    expect(
      (
        await call("POST", W, {
          name: "Orders",
          tenantId: "x",
          clientToken: "a",
        })
      ).body.ontology.id,
    ).toBe(made.body.ontology.id);
    expect(
      refusal(
        await call("POST", W, {
          name: "Again",
          tenantId: "ten_acme_orders",
          clientToken: "b",
        }),
      ),
    ).toEqual([
      409,
      "another ontology already uses this database; choose or create another",
    ]);
    for (const tenantId of ["ten_acme_old", "nope"])
      expect(
        (await call("POST", W, { name: "X", tenantId, clientToken: tenantId }))
          .status,
      ).toBe(404);
    expect(
      (await call("POST", W, { name: " ", tenantId: "x", clientToken: "c" }))
        .status,
    ).toBe(400);
    expect(
      (
        await call(
          "POST",
          W,
          { name: "X", tenantId: "ten_acme_sandbox", clientToken: "d" },
          "usr_alan",
        )
      ).status,
    ).toBe(403);
  });

  it("deletes Foundry's rows and its loads, never the database's own data", async () => {
    await call("POST", `${O}:set-current`, {
      versionId: "ver_seed_3",
      expectedCurrentVersionId: "ver_seed_1",
    });
    Object.assign(config, { loadStepMs: 100 });
    const first = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "l",
      })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    // The team's own node, linked to nothing of the load's.
    const own = await read("ten_acme_contracts", count("Customer"));
    expect(own.body).toEqual({ n: 64 });
    // A second load is live when the ontology goes: it stops.
    await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "m",
    });
    expect((await call("DELETE", O)).status).toBe(200);
    expect((await call("GET", O)).status).toBe(404);
    expect(
      (await call("GET", W)).body.ontologies.map((o: { id: string }) => o.id),
    ).toEqual(["ont_acme_sales", "ont_acme_renewals"]);
    expect(first.id).toMatch(/^load_/);
    // Both loads left the database.
    expect((await read("ten_acme_contracts", count("Customer"))).body).toEqual({
      n: 0,
    });
  });
});

describe("mock document sets", () => {
  it("starts Acme with General, and keeps names unique in the workspace", async () => {
    const list = async () =>
      (await call("GET", SETS)).body.sets.map(
        (x: { name: string; documents: number }) => `${x.name} ${x.documents}`,
      );
    expect(await list()).toEqual(["General 0"]);
    const made = await call("POST", SETS, {
      name: "Board minutes",
      description: "Minutes of every board meeting.",
    });
    expect(made.body.set).toMatchObject({
      id: expect.stringMatching(/^set_/),
      name: "Board minutes",
      description: "Minutes of every board meeting.",
      documents: 0,
    });
    // Oldest first.
    expect(await list()).toEqual(["General 0", "Board minutes 0"]);
    // Names are unique in the workspace, case and accents aside.
    for (const name of ["general", "GENERAL", "Général"])
      expect(refusal(await call("POST", SETS, { name }))).toEqual([
        409,
        `the workspace already has a document set named "${name}"`,
      ]);
    expect((await call("POST", SETS, { name: "x".repeat(120) })).status).toBe(
      200,
    );
    // Trimmed, as the WFE takes it, then 1 to 120 characters.
    expect(
      (
        await call("POST", SETS, {
          name: "  Spaced ",
          description: " Padded. ",
        })
      ).body.set,
    ).toMatchObject({ name: "Spaced", description: "Padded." });
    for (const name of ["", "   ", "x".repeat(121)])
      expect(refusal(await call("POST", SETS, { name })), name).toEqual([
        400,
        "name must be 1 to 120 characters",
      ]);
    expect(
      refusal(
        await call("POST", SETS, {
          name: "Long",
          description: "x".repeat(1001),
        }),
      ),
    ).toEqual([400, "description must be at most 1000 characters"]);
    // Each field sent is set; one left out stays.
    const id = made.body.set.id;
    expect(
      (await call("PATCH", `${SETS}/${id}`, { name: "Minutes" })).body.set,
    ).toMatchObject({
      name: "Minutes",
      description: "Minutes of every board meeting.",
    });
    expect(
      (await call("PATCH", `${SETS}/${id}`, { description: "" })).body.set,
    ).toMatchObject({ name: "Minutes", description: "" });
    expect((await call("PATCH", `${SETS}/${id}`, { name: "" })).status).toBe(
      400,
    );
    // Renaming to its own name is no clash.
    expect(
      (await call("PATCH", `${SETS}/${id}`, { name: "minutes" })).status,
    ).toBe(200);
    expect(
      refusal(await call("PATCH", `${SETS}/${id}`, { name: "GENERAL" })),
    ).toEqual([
      409,
      'the workspace already has a document set named "GENERAL"',
    ]);
    expect(
      (await call("PATCH", `${SETS}/set_nope`, { name: "x" })).status,
    ).toBe(404);
    // A plain member reads them and changes none.
    expect((await call("GET", SETS, undefined, "usr_alan")).status).toBe(200);
    for (const [method, path] of [
      ["POST", SETS],
      ["PATCH", `${SETS}/${id}`],
      ["DELETE", `${SETS}/${id}`],
    ] as const)
      expect(
        (await call(method, path, { name: "Mine" }, "usr_alan")).status,
      ).toBe(403);
    // A workspace creates up to 20 sets.
    const held = (await call("GET", SETS)).body.sets.length;
    for (let i = held; i < 20; i++)
      expect((await call("POST", SETS, { name: `Set ${i}` })).status).toBe(200);
    expect(refusal(await call("POST", SETS, { name: "One more" }))).toEqual([
      400,
      "a workspace holds up to 20 document sets; delete an empty one first",
    ]);
  });

  it("deletes only an empty set, and moves a document between sets", async () => {
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    const doc = await uploaded("notes.md", Buffer.from("# Notes\n"), other);
    expect(doc.setId).toBe(other);
    expect(refusal(await call("DELETE", `${SETS}/${other}`))).toEqual([
      400,
      "the document set still holds documents; move or delete them first",
    ]);
    // No ontology's load reads either set yet: nothing to read it.
    const moved = await call("POST", `${DOCS}/${doc.id}:move`, {
      setId: GENERAL,
    });
    expect(moved.body).toEqual({
      document: expect.objectContaining({ id: doc.id, setId: GENERAL }),
      reads: [],
    });
    expect(
      refusal(await call("POST", `${DOCS}/${doc.id}:move`, { setId: "nope" })),
    ).toEqual([404, "document set not found"]);
    expect(refusal(await call("POST", `${DOCS}/${doc.id}:move`, {}))).toEqual([
      400,
      "set_id is required: the document set to move into",
    ]);
    expect(
      (await call("GET", SETS)).body.sets.map(
        (x: { documents: number }) => x.documents,
      ),
    ).toEqual([1, 0]);
    expect(await call("DELETE", `${SETS}/${other}`)).toMatchObject({
      status: 200,
      body: {},
    });
    expect((await call("GET", SETS)).body.sets).toHaveLength(1);
    // A deleted document holds no set back, can't move, and reads as deleted.
    await call("DELETE", `${DOCS}/${doc.id}`);
    expect(
      refusal(await call("POST", `${DOCS}/${doc.id}:move`, { setId: GENERAL })),
    ).toEqual([400, "the document is deleted"]);
    expect((await call("GET", `${DOCS}/${doc.id}`)).body.document.status).toBe(
      "deleted",
    );
    expect((await call("DELETE", `${SETS}/${GENERAL}`)).status).toBe(200);
  });

  it("renames a document as an upload names one, and serves its text", async () => {
    const doc = await uploaded("notes.md", Buffer.from("# Notes\n"));
    expect(
      (await call("PATCH", `${DOCS}/${doc.id}`, { name: " Notes (Q3) " })).body
        .document,
    ).toMatchObject({
      id: doc.id,
      name: "Notes (Q3)",
      mediaType: "text/markdown",
    });
    for (const name of ["", "a/b", "a\\b", "a\u0007b", "x".repeat(256)])
      expect(
        refusal(await call("PATCH", `${DOCS}/${doc.id}`, { name })),
        name,
      ).toEqual([
        400,
        "name must be 1 to 255 characters with no slash, backslash or control character",
      ]);
    expect(
      (await call("PATCH", `${DOCS}/${doc.id}`, { name: "x" }, "usr_alan"))
        .status,
    ).toBe(403);
    expect((await call("GET", `${DOCS}/${doc.id}:content`)).body).toEqual({
      text: "# Notes\n",
    });
    // Only an uploaded document has text.
    const pending = await call("POST", DOCS, {
      name: "later.md",
      mediaType: "text/markdown",
      sizeBytes: "3",
      sha256: sha(Buffer.from("abc")),
      clientToken: "later",
      setId: GENERAL,
    });
    expect(
      refusal(await call("GET", `${DOCS}/${pending.body.document.id}:content`)),
    ).toEqual([
      400,
      "the document is pending_upload; only an uploaded document can be read",
    ]);
    await call("DELETE", `${DOCS}/${doc.id}`);
    expect(
      refusal(await call("PATCH", `${DOCS}/${doc.id}`, { name: "Again" })),
    ).toEqual([400, "the document is deleted"]);
    // Another workspace's document is none of this one's.
    expect(
      (await call("GET", `/v1/workspaces/ws_none/foundry/documents/${doc.id}`))
        .status,
    ).not.toBe(200);
  });

  it("serves a text document's text, and any document's bytes", async () => {
    const csv = Buffer.from("a,b\n1,2\n"),
      pdf = Buffer.from("%PDF-1.7\n\u00e2\u00e3\n%%EOF\n", "latin1");
    const table = await uploaded("q3.csv", csv),
      deck = await uploaded("deck.pdf", pdf);
    expect(table.mediaType).toBe("text/csv");
    expect(deck.mediaType).toBe("application/pdf");
    expect((await call("GET", `${DOCS}/${table.id}:content`)).body).toEqual({
      text: "a,b\n1,2\n",
    });
    // A PDF's text is what its read gave: none until then, and none from
    // bytes that aren't a PDF.
    expect(refusal(await call("GET", `${DOCS}/${deck.id}:content`))).toEqual([
      400,
      "the PDF's text is still being read; try again shortly",
    ]);
    vi.setSystemTime(Date.now() + 10_000);
    expect(refusal(await call("GET", `${DOCS}/${deck.id}:content`))).toEqual([
      400,
      "the PDF's text can't be read: the file is damaged or is not a PDF",
    ]);
    for (const [d, bytes] of [
      [table, csv],
      [deck, pdf],
    ] as const) {
      const r = await call("GET", `${DOCS}/${d.id}:bytes`);
      expect(r.body.mediaType).toBe(d.mediaType);
      expect(Buffer.from(r.body.data, "base64")).toEqual(bytes);
    }
    // Read access, as :content: a member reads them.
    expect(
      (await call("GET", `${DOCS}/${deck.id}:bytes`, undefined, "usr_alan"))
        .status,
    ).toBe(200);
    const pending = await call("POST", DOCS, {
      name: "later.parquet",
      mediaType: "application/vnd.apache.parquet",
      sizeBytes: "4",
      sha256: sha(Buffer.from("PAR1")),
      clientToken: "later",
      setId: GENERAL,
    });
    expect(
      refusal(await call("GET", `${DOCS}/${pending.body.document.id}:bytes`)),
    ).toEqual([
      400,
      "the document is pending_upload; only an uploaded document can be read",
    ]);
    // A binary type has no text, uploaded or not.
    expect(
      refusal(await call("GET", `${DOCS}/${pending.body.document.id}:content`)),
    ).toEqual([400, "Parquet documents have no text; use :bytes"]);
    expect(
      (
        await call(
          "GET",
          `/v1/workspaces/ws_none/foundry/documents/${deck.id}:bytes`,
        )
      ).status,
    ).not.toBe(200);
  });

  it("reads a PDF's text once, page by page, and says why one can't be read", async () => {
    const terms = await uploaded("terms.pdf", TERMS),
      scan = await uploaded("scan.pdf", textPdf([[], []])),
      broken = await uploaded("broken.pdf", Buffer.from("%PDF-1.7\n")),
      long = await uploaded(
        "long.pdf",
        textPdf(Array.from({ length: MAX_PAGES + 1 }, () => ["A page."])),
      );
    const read = async (d: { id: string }) =>
      (await call("GET", `${DOCS}/${d.id}`)).body.document.read;
    expect(await read(terms)).toMatchObject({
      kind: "FOUNDRY_READ_KIND_PDF",
      state: "FOUNDRY_FILE_READ_STATE_READING",
    });
    vi.setSystemTime(Date.now() + 2_000);
    expect(await read(terms)).toMatchObject({
      state: "FOUNDRY_FILE_READ_STATE_READY",
      pages: 2,
      error: null,
    });
    // Each page's text ends its lines with \n; a form feed between pages.
    expect((await call("GET", `${DOCS}/${terms.id}:content`)).body).toEqual({
      text: "Renewal terms\nGlobex Corporation renews on 1 December 2026.\n\fNotice\nEither party gives 90 days notice.\n",
    });
    expect(await read(scan)).toMatchObject({
      state: "FOUNDRY_FILE_READ_STATE_FAILED",
      error: { code: "unsupported", message: "Scanned PDF: no text to read" },
    });
    expect(await read(broken)).toMatchObject({
      state: "FOUNDRY_FILE_READ_STATE_FAILED",
      error: {
        code: "unsupported",
        message: "the file is damaged or is not a PDF",
      },
    });
    expect(await read(long)).toMatchObject({
      state: "FOUNDRY_FILE_READ_STATE_FAILED",
      error: {
        code: "too_large",
        message: `the PDF has ${MAX_PAGES + 1} pages; Helix reads PDFs of up to ${MAX_PAGES}`,
      },
    });
    expect(refusal(await call("GET", `${DOCS}/${scan.id}:content`))).toEqual([
      400,
      "the PDF's text can't be read: Scanned PDF: no text to read",
    ]);
    // Its pages are its bytes, read or not.
    expect(
      Buffer.from(
        (await call("GET", `${DOCS}/${scan.id}:bytes`)).body.data,
        "base64",
      ),
    ).toEqual(textPdf([[], []]));
    // Deleted, its read goes with it.
    await call("DELETE", `${DOCS}/${terms.id}`);
    expect(await read(terms)).toBeNull();
  });

  it("holds a CSV, a JSON, a PDF and a scan in Acme's General for dev:mock only", async () => {
    const general = async () =>
      (await call("GET", `${DOCS}?setId=${GENERAL}`)).body.documents;
    expect(await general()).toEqual([]);
    vi.stubEnv("MOCK_DOCUMENT_FIXTURES", "1");
    try {
      resetState();
      // Each is read as the mock starts; the scan has no text to read.
      await vi.waitFor(async () =>
        expect(
          (await general()).map(
            (d: { read: { state: string } }) => d.read.state,
          ),
        ).toEqual([
          "FOUNDRY_FILE_READ_STATE_FAILED",
          ...Array(3).fill("FOUNDRY_FILE_READ_STATE_READY"),
        ]),
      );
      const [scan, ...docs] = await general();
      expect(
        [scan, ...docs].map(
          (d: { name: string; mediaType: string; status: string }) => [
            d.name,
            d.mediaType,
            d.status,
          ],
        ),
      ).toEqual([
        ["signed-order-form.pdf", "application/pdf", "uploaded"],
        ["renewal-terms.pdf", "application/pdf", "uploaded"],
        ["pricing.json", "application/json", "uploaded"],
        ["renewals.csv", "text/csv", "uploaded"],
      ]);
      expect(scan.read.error).toEqual({
        code: "unsupported",
        message: "Scanned PDF: no text to read",
      });
      expect(docs[0].read).toMatchObject({ pages: 2 });
      const pdf = (await call("GET", `${DOCS}/${docs[0].id}:content`)).body;
      expect(pdf.text.split("\f").map((p: string) => p.split("\n")[0])).toEqual(
        ["Renewal terms", "Notice"],
      );
      const json = (await call("GET", `${DOCS}/${docs[1].id}:content`)).body;
      expect(JSON.parse(json.text).plans).toHaveLength(2);
      const csv = (await call("GET", `${DOCS}/${docs[2].id}:content`)).body;
      expect(csv.text.split("\n")[0]).toBe(
        "customer,plan,seats,renews_on,owner",
      );
      expect(docs[2].sha256).toBe(sha(Buffer.from(csv.text)));
    } finally {
      vi.unstubAllEnvs();
    }
  });

  it("returns the workspace's document for the same bytes, wherever it is", async () => {
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    const first = await uploaded("contract.md", CONTRACT, other);
    const again = await call("POST", DOCS, {
      name: "contract-copy.md",
      mediaType: "text/markdown",
      sizeBytes: String(CONTRACT.length),
      sha256: sha(CONTRACT),
      clientToken: "copy",
      setId: GENERAL,
    });
    // One copy: in the set it is in, with nothing to upload.
    expect(again.body).toEqual({
      document: expect.objectContaining({
        id: first.id,
        name: "contract.md",
        setId: other,
      }),
      upload: null,
      existing: true,
    });
    const fresh = await call("POST", DOCS, {
      name: "new.md",
      mediaType: "text/markdown",
      sizeBytes: "3",
      sha256: sha(Buffer.from("new")),
      clientToken: "new",
      setId: "set_nope",
    });
    expect(refusal(fresh)).toEqual([404, "document set not found"]);
    expect(
      refusal(
        await call("POST", DOCS, {
          name: "new.md",
          mediaType: "text/markdown",
          sizeBytes: "3",
          sha256: sha(Buffer.from("new")),
          clientToken: "new",
        }),
      ),
    ).toEqual([400, "set_id is required: the document set to upload into"]);
    // The workspace's list, by set.
    expect(
      (await call("GET", `${DOCS}?setId=${other}`)).body.documents.map(
        (d: { id: string }) => d.id,
      ),
    ).toEqual([first.id]);
    expect(
      (await call("GET", `${DOCS}?setId=${GENERAL}`)).body.documents,
    ).toEqual([]);
  });

  it("says which ontologies read each set, by their reading version's scope", async () => {
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    const readBy = async () =>
      Object.fromEntries(
        (await call("GET", SETS)).body.sets.map(
          (x: { name: string; readBy: unknown[] }) => [x.name, x.readBy],
        ),
      );
    // Renewals' current version reads General; Customer contracts' version
    // 1 (Definition v1) reads nothing.
    expect(await readBy()).toEqual({
      General: [{ ontologyId: "ont_acme_renewals", routed: true }],
      Other: [],
    });
    await call("POST", `${O}:set-current`, {
      versionId: "ver_seed_3",
      expectedCurrentVersionId: "ver_seed_1",
    });
    expect(await readBy()).toEqual({
      General: [
        { ontologyId: "ont_acme_contracts", routed: true },
        { ontologyId: "ont_acme_renewals", routed: true },
      ],
      Other: [],
    });
    // A document's ontologies: those whose reading version has its set.
    const doc = await uploaded("contract.md", CONTRACT);
    expect(
      (await call("GET", `${DOCS}/${doc.id}`)).body.document.ontologies,
    ).toEqual([
      { ontologyId: "ont_acme_contracts", routed: true, extraction: null },
      { ontologyId: "ont_acme_renewals", routed: true, extraction: null },
    ]);
    const stray = await uploaded("notes.md", Buffer.from("# Notes\n"), other);
    expect(
      (await call("GET", `${DOCS}/${stray.id}`)).body.document.ontologies,
    ).toEqual([]);
    // An ontology lists what it reads: its scope's documents.
    expect(
      (await call("GET", `${O}/documents`)).body.documents.map(
        (d: { id: string }) => d.id,
      ),
    ).toEqual([doc.id]);
    // And counts them.
    expect((await call("GET", O)).body.ontology.counts).toEqual({
      documents: 1,
    });
    // Any document of the workspace opens in an ontology's view.
    expect(
      (await call("GET", `${O}/documents/${stray.id}`)).body.document,
    ).toMatchObject({ id: stray.id, extraction: null });
  });

  it("fans an upload out to every ontology whose loads read its set", async () => {
    Object.assign(config, { loadStepMs: 100 });
    const R = `${W}/ont_acme_renewals`;
    await call("POST", `${O}:set-current`, {
      versionId: "ver_seed_3",
      expectedCurrentVersionId: "ver_seed_1",
    });
    // Neither has loaded: no load reads General yet.
    const early = await uploaded("a.md", Buffer.from("# A\nGlobex.\n"));
    expect(
      (await call("GET", `${DOCS}/${early.id}`)).body.document.ontologies,
    ).toHaveLength(2);
    for (const [o, versionId] of [
      [O, "ver_seed_3"],
      [R, "ver_renewals_1"],
    ])
      await call("POST", `${o}/loads`, { versionId, clientToken: o });
    // Live loads read what joins their scope while they run.
    await uploaded("b.md", Buffer.from("# B\n"));
    vi.setSystemTime(Date.now() + 30_000);
    const c = await call("POST", DOCS, {
      name: "c.md",
      mediaType: "text/markdown",
      sizeBytes: "4",
      sha256: sha(Buffer.from("# C\n")),
      clientToken: "c",
      setId: GENERAL,
    });
    await put(c.body.upload.url, Buffer.from("# C\n"), c.body.upload.headers);
    const done = await call(
      "POST",
      `${DOCS}/${c.body.document.id}:complete`,
      {},
    );
    // Both loads are current now: each queues an extract. The oldest
    // ontology comes first, as the WFE locks them.
    expect(outcomes(done)).toEqual({
      ont_acme_contracts: "QUEUED",
      ont_acme_renewals: "QUEUED",
    });
    expect(
      done.body.reads.map((r: { ontologyId: string }) => r.ontologyId),
    ).toEqual(["ont_acme_renewals", "ont_acme_contracts"]);
    // While those extracts run, the next upload is owed.
    const d = await call("POST", DOCS, {
      name: "d.md",
      mediaType: "text/markdown",
      sizeBytes: "4",
      sha256: sha(Buffer.from("# D\n")),
      clientToken: "d",
      setId: GENERAL,
    });
    await put(d.body.upload.url, Buffer.from("# D\n"), d.body.upload.headers);
    expect(
      outcomes(
        await call("POST", `${DOCS}/${d.body.document.id}:complete`, {}),
      ),
    ).toEqual({ ont_acme_contracts: "OWED", ont_acme_renewals: "OWED" });
    vi.setSystemTime(Date.now() + 60_000);
    // Every document of General was read into both loads.
    for (const o of [O, R])
      expect(
        (await call("GET", `${o}/documents?status=waiting`)).body.documents,
        o,
      ).toEqual([]);
    expect(
      (
        await call("GET", `${DOCS}/${d.body.document.id}`)
      ).body.document.ontologies.map(
        (x: { extraction: { status: string } }) => x.extraction.status,
      ),
    ).toEqual(["extracted", "extracted"]);
    // Moving a document a current load holds keeps what it read.
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    await call("POST", `${DOCS}/${early.id}:move`, { setId: other });
    expect(
      outcomes(
        await call("POST", `${DOCS}/${early.id}:move`, { setId: GENERAL }),
      ),
    ).toEqual({ ont_acme_contracts: "HELD", ont_acme_renewals: "HELD" });
    // A caller who can't write the databases reads into none.
    Object.assign(config, { ontologyDatabaseHidden: true });
    const e = await call("POST", DOCS, {
      name: "e.md",
      mediaType: "text/markdown",
      sizeBytes: "4",
      sha256: sha(Buffer.from("# E\n")),
      clientToken: "e",
      setId: GENERAL,
    });
    await put(e.body.upload.url, Buffer.from("# E\n"), e.body.upload.headers);
    expect(
      outcomes(
        await call("POST", `${DOCS}/${e.body.document.id}:complete`, {}),
      ),
    ).toEqual({ ont_acme_contracts: "SKIPPED", ont_acme_renewals: "SKIPPED" });
    // Completing twice reads nothing again.
    expect(
      (await call("POST", `${DOCS}/${e.body.document.id}:complete`, {})).body
        .reads,
    ).toBeUndefined();
  });

  it("reads into a load only its version's sets; a set no route names is stored", async () => {
    Object.assign(config, { loadStepMs: 100, ontologyStepMs: 100 });
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    const general = await uploaded("contract.md", CONTRACT);
    const stored = await uploaded(
      "notes.md",
      Buffer.from("# Notes\nGlobex.\n"),
      other,
    );
    // A generation that picks Other reads it, and only it; its draft routes
    // nothing.
    const g = (
      await call("POST", `${O}:generate`, {
        connectorIds: ["conn_acme_stripe"],
        documentSetIds: [other],
      })
    ).body.generation;
    vi.setSystemTime(Date.now() + 10_000);
    const v = (await call("GET", `${O}/generations/${g.id}`)).body.generation
      .resultVersionId;
    expect(
      (await call("GET", `${O}/versions/${v}`)).body.version.documentSetIds,
    ).toEqual([other]);
    await call("POST", `${O}:set-current`, {
      versionId: v,
      expectedCurrentVersionId: "ver_seed_1",
    });
    await call("POST", `${O}/loads`, { versionId: v, clientToken: "l" });
    vi.setSystemTime(Date.now() + 10_000);
    const docs = (await call("GET", `${O}/documents`)).body.documents;
    expect(docs.map((d: { id: string }) => d.id)).toEqual([stored.id]);
    // Stored with its sections; nothing read, nothing spent.
    expect(docs[0].extraction).toMatchObject({
      status: "out_of_scope",
      entities: 0,
      usdMicros: "0",
    });
    expect(
      (await call("GET", `${O}/documents?status=out_of_scope`)).body.documents,
    ).toHaveLength(1);
    expect(
      (await call("GET", `${O}/documents/${general.id}`)).body.document
        .extraction,
    ).toBeNull();
    expect(refusal(await call("GET", `${O}/documents?status=nope`))).toEqual([
      400,
      'status "nope" is none of waiting, extracted, review, duplicate, failed or out_of_scope',
    ]);
  });

  it("checks a set create's cap before its name, and takes a set's own name as no rename", async () => {
    Object.assign(config, {
      legacyDocumentSets: [
        "GENERAL",
        ...Array.from({ length: 18 }, (_, i) => `Legacy ${i + 1}`),
      ],
    });
    const sets = (await call("GET", SETS)).body.sets;
    expect(sets).toHaveLength(20);
    // Full: the cap refuses before the name does.
    expect(refusal(await call("POST", SETS, { name: "General" }))).toEqual([
      400,
      "a workspace holds up to 20 document sets; delete an empty one first",
    ]);
    // An edit that sends the set's own name works beside its legacy twin;
    // a rename onto it doesn't.
    expect(
      (
        await call("PATCH", `${SETS}/${GENERAL}`, {
          name: "General",
          description: "Everything else.",
        })
      ).body.set,
    ).toMatchObject({ name: "General", description: "Everything else." });
    expect(
      refusal(await call("PATCH", `${SETS}/${GENERAL}`, { name: "general" })),
    ).toEqual([
      409,
      'the workspace already has a document set named "general"',
    ]);
  });

  it("reads at most 20 sets in a generation, checked after a live one, the connectors and tables", async () => {
    Object.assign(config, {
      legacyDocumentSets: Array.from({ length: 20 }, (_, i) => `Legacy ${i}`),
    });
    const all = (await call("GET", SETS)).body.sets.map(
      (x: { id: string }) => x.id,
    );
    expect(all).toHaveLength(21);
    expect(
      refusal(await call("POST", `${O}:generate`, { documentSetIds: all })),
    ).toEqual([
      400,
      "a generation reads at most 20 document sets; choose fewer",
    ]);
    // The connectors and tables are checked before the sets.
    expect(
      refusal(
        await call("POST", `${O}:generate`, {
          connectorIds: ["conn_nope"],
          documentSetIds: ["set_nope"],
        }),
      ),
    ).toEqual([404, "connector conn_nope not found"]);
    expect(
      (
        await call("POST", `${O}:generate`, {
          documentSetIds: all.slice(0, 20),
        })
      ).status,
    ).toBe(200);
    // A live generation is refused before anything else is checked.
    expect(
      refusal(
        await call("POST", `${O}:generate`, {
          connectorIds: ["conn_nope"],
          documentSetIds: all,
        }),
      ),
    ).toEqual([409, "an ontology generation is already queued or running"]);
  });

  it("keeps in a version's scope only the sets that are live when it is stored", async () => {
    Object.assign(config, { ontologyStepMs: 100 });
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    const g = (
      await call("POST", `${O}:generate`, {
        connectorIds: ["conn_acme_stripe"],
        documentSetIds: [GENERAL, other],
      })
    ).body.generation;
    expect(g.documentSetIds).toEqual([GENERAL, other]);
    // Deleted while the agent drafts: the version doesn't read it.
    expect((await call("DELETE", `${SETS}/${other}`)).status).toBe(200);
    vi.setSystemTime(Date.now() + 10_000);
    const done = (await call("GET", `${O}/generations/${g.id}`)).body
      .generation;
    expect(done.documentSetIds).toEqual([GENERAL, other]);
    expect(
      (await call("GET", `${O}/versions/${done.resultVersionId}`)).body.version
        .documentSetIds,
    ).toEqual([GENERAL]);
    // Lists carry the scope too.
    expect(
      (await call("GET", `${O}/versions`)).body.versions.find(
        (v: { id: string }) => v.id === "ver_seed_3",
      ).documentSetIds,
    ).toEqual([GENERAL]);
  });

  it("records the document sets a generation drafts from: none when none", async () => {
    const other = (await call("POST", SETS, { name: "Other" })).body.set.id;
    expect(
      refusal(
        await call("POST", `${O}:generate`, { documentSetIds: ["set_nope"] }),
      ),
    ).toEqual([404, "document set set_nope not found"]);
    const g = (
      await call("POST", `${O}:generate`, {
        documentSetIds: [other, GENERAL, other],
      })
    ).body.generation;
    // In the sets' order, as the WFE records them.
    expect(g.documentSetIds).toEqual([GENERAL, other]);
    // None asked for is none.
    vi.setSystemTime(Date.now() + 60_000);
    const none = (await call("POST", `${O}:generate`, { documentSetIds: [] }))
      .body.generation;
    expect(none.documentSetIds).toEqual([]);
  });

  it("generates from a set's files alone, leaving out a file still being read", async () => {
    const other = (await call("POST", SETS, { name: "Files" })).body.set.id;
    const generate = (body: object) => call("POST", `${O}:generate`, body);
    // Nothing in the set yet.
    expect(
      (await generate({ noConnectors: true, documentSetIds: [other] })).status,
    ).toBe(400);
    expect(
      refusal(
        await generate({
          noConnectors: true,
          connectorIds: ["x"],
          documentSetIds: [other],
        }),
      ),
    ).toEqual([400, "connector_ids must be empty with no_connectors"]);
    const csv = await uploaded(
      "renewals.csv",
      Buffer.from("customer,amount\nGlobex,12\n"),
      other,
    );
    // Still being read: shown so, and nothing to draft from yet.
    expect(
      (await call("GET", `${DOCS}/${csv.id}`)).body.document.read,
    ).toMatchObject({
      kind: "FOUNDRY_READ_KIND_TABLE",
      state: "FOUNDRY_FILE_READ_STATE_READING",
    });
    expect(
      (await generate({ noConnectors: true, documentSetIds: [other] })).status,
    ).toBe(400);
    vi.setSystemTime(Date.now() + 10_000);
    expect(
      (await call("GET", `${DOCS}/${csv.id}`)).body.document.read,
    ).toMatchObject({ state: "FOUNDRY_FILE_READ_STATE_READY", rows: "1" });
    const g = (await generate({ noConnectors: true, documentSetIds: [other] }))
      .body.generation;
    expect(g.tables || []).toEqual([]);
    expect(g.documents).toEqual([{ id: csv.id, setId: other }]);
  });

  it("generates from a set's PDFs once their text is read, never a scanned one's", async () => {
    const set = async (name: string) =>
      (await call("POST", SETS, { name })).body.set.id as string;
    const pdfs = await set("PDFs"),
      scans = await set("Scans");
    const generate = (id: string) =>
      call("POST", `${O}:generate`, {
        noConnectors: true,
        documentSetIds: [id],
      });
    await uploaded("terms.pdf", TERMS, pdfs);
    await uploaded("scan.pdf", textPdf([[]]), scans);
    // No text to read yet.
    expect((await generate(pdfs)).status).toBe(400);
    vi.setSystemTime(Date.now() + 2_000);
    // A scanned PDF has none.
    expect((await generate(scans)).status).toBe(400);
    const g = (await generate(pdfs)).body.generation;
    expect(g.documentSetIds).toEqual([pdfs]);
    // A PDF is text, not a table file of its own.
    expect(g.documents || []).toEqual([]);
  });
});

describe("mock document dedupe and the cap", () => {
  const create = (name: string, bytes: Buffer, clientToken = name) =>
    call("POST", DOCS, {
      name,
      mediaType: "text/markdown",
      sizeBytes: String(bytes.length),
      sha256: sha(bytes),
      clientToken,
      setId: GENERAL,
    });

  it("returns a deleted document for its own client token, and keeps every URL it gave working", async () => {
    const bytes = Buffer.from("# Minutes\n");
    const first = await create("minutes.md", bytes, "tok");
    const again = await create("minutes.md", bytes, "tok");
    // The same upload, asked for again: a fresh URL, the first still good.
    expect(again.body).toMatchObject({
      document: { id: first.body.document.id },
      existing: true,
    });
    expect(again.body.upload.url).not.toBe(first.body.upload.url);
    expect(
      (await put(first.body.upload.url, bytes, first.body.upload.headers))
        .status,
    ).toBe(200);
    await call("POST", `${DOCS}/${first.body.document.id}:complete`, {});
    await call("DELETE", `${DOCS}/${first.body.document.id}`);
    // Deleted, its token still names it; its bytes no longer do.
    expect((await create("minutes.md", bytes, "tok")).body).toMatchObject({
      document: { id: first.body.document.id, status: "deleted" },
      upload: null,
      existing: true,
    });
    const fresh = await create("minutes-2.md", bytes, "tok-2");
    expect(fresh.body).toMatchObject({ existing: false });
    expect(fresh.body.document.id).not.toBe(first.body.document.id);
  });

  it("returns the oldest of documents with the same bytes", async () => {
    const bytes = Buffer.from("# Same\n");
    const a = await create("a.md", bytes, "a");
    expect((await create("b.md", bytes, "b")).body.document.id).toBe(
      a.body.document.id,
    );
  });

  it("counts uploaded documents and live uploads, not abandoned ones", async () => {
    for (let i = 0; i < 200; i++)
      expect((await create(`d${i}.md`, Buffer.from(`# ${i}\n`))).status).toBe(
        200,
      );
    expect(refusal(await create("over.md", Buffer.from("# over\n")))).toEqual([
      400,
      "a workspace holds up to 200 documents; delete one first",
    ]);
    // Fifteen minutes on, the uploads that never came are abandoned.
    vi.setSystemTime(Date.now() + 16 * 60_000);
    for (let i = 0; i < 200; i++)
      expect(
        (await create(`n${i}.md`, Buffer.from(`# new ${i}\n`))).status,
      ).toBe(200);
    // Asked for again, an abandoned one takes a place again: there is none.
    expect(refusal(await create("d0.md", Buffer.from("# 0\n")))).toEqual([
      400,
      "a workspace holds up to 200 documents; delete one first",
    ]);
  });
});

describe("mock documents and loads", () => {
  it("uploads through a presigned PUT that checks what S3 checks", async () => {
    const bytes = Buffer.from("# Notes\n\nInitech asked for a discount.\n");
    const created = await call("POST", DOCS, {
      name: "notes.md",
      mediaType: "text/markdown",
      sizeBytes: String(bytes.length),
      sha256: sha(bytes),
      clientToken: "n",
      setId: "set_acme_general",
    });
    const { document, upload } = created.body;
    expect(document.status).toBe("pending_upload");
    // Not complete until the bytes arrived.
    expect(
      refusal(await call("POST", `${DOCS}/${document.id}:complete`, {})),
    ).toEqual([400, "upload the document's bytes to its URL first"]);
    // As S3 presigns it: the checksum in the URL, Content-Type the only
    // header, and a preflight that allows no other.
    expect(upload.headers).toEqual({ "Content-Type": "text/markdown" });
    const signed = new URL(upload.url);
    expect(signed.searchParams.get("X-Amz-Checksum-Sha256")).toBe(
      Buffer.from(sha(bytes), "hex").toString("base64"),
    );
    expect(
      (await put(upload.url, Buffer.alloc(0), {}, "OPTIONS")).head,
    ).toMatchObject({ "Access-Control-Allow-Headers": "content-type" });
    signed.searchParams.set("X-Amz-Checksum-Sha256", "x");
    expect((await put(signed.href, bytes, upload.headers)).status).toBe(403);
    const wrong = await put(upload.url, Buffer.from("other"), upload.headers);
    expect(wrong.status).toBe(403);
    expect(wrong.text).toContain("SignatureDoesNotMatch");
    expect(
      (
        await put(
          upload.url.replace(/token=\w+/, "token=x"),
          bytes,
          upload.headers,
        )
      ).status,
    ).toBe(403);
    Object.assign(config, { documentUploadFailures: 1 });
    expect((await put(upload.url, bytes, upload.headers)).status).toBe(503);
    expect((await put(upload.url, bytes, upload.headers)).status).toBe(200);
    const done = await call("POST", `${DOCS}/${document.id}:complete`, {});
    expect(done.body.document).toMatchObject({
      status: "uploaded",
      extraction: null,
    });
    // The same bytes again are the same document, with nothing to upload.
    const again = await call("POST", DOCS, {
      name: "copy.md",
      mediaType: "text/markdown",
      sizeBytes: String(bytes.length),
      sha256: sha(bytes),
      clientToken: "n2",
      setId: "set_acme_general",
    });
    expect(again.body).toEqual({
      document: expect.objectContaining({
        id: document.id,
        setId: "set_acme_general",
      }),
      upload: null,
      existing: true,
    });
    // Version 1 is current: a Definition v1 version reads no set. Renewals'
    // reads General.
    expect((await call("GET", O)).body.ontology.counts).toEqual({
      documents: 0,
    });
    expect(
      (await call("GET", `${W}/ont_acme_renewals`)).body.ontology.counts,
    ).toEqual({ documents: 1 });
  });

  it("refuses files it can't read", async () => {
    const make = (body: object) =>
      call("POST", DOCS, {
        name: "a.md",
        mediaType: "text/markdown",
        sizeBytes: "5",
        sha256: "a".repeat(64),
        clientToken: "t",
        setId: "set_acme_general",
        ...body,
      });
    expect(refusal(await make({ name: "deck.docx" }))).toEqual([
      400,
      'Foundry takes Markdown (.md, .markdown), MDX (.mdx), plain text (.txt), JSON (.json), CSV (.csv), TSV (.tsv), JSONL (.jsonl, .ndjson), Parquet (.parquet) and PDF (.pdf) documents; "deck.docx" is none of them',
    ]);
    // Every format, by its own extension; a type it doesn't know, never.
    for (const [mediaType, name] of [
      ["text/mdx", "guide.mdx"],
      ["text/plain", "a.txt"],
      ["application/json", "a.json"],
      ["text/csv", "a.CSV"],
      ["text/tab-separated-values", "a.tsv"],
      ["application/x-ndjson", "a.ndjson"],
      ["application/vnd.apache.parquet", "a.parquet"],
      ["application/pdf", "deck.pdf"],
    ])
      expect(
        (
          await make({
            name,
            mediaType,
            clientToken: name,
            sha256: sha(Buffer.from(name)),
          })
        ).status,
        name,
      ).toBe(200);
    expect(
      (await make({ name: "a.pdf", mediaType: "text/markdown" })).status,
    ).toBe(400);
    expect(
      (await make({ name: "toString", mediaType: "toString" })).status,
    ).toBe(400);
    for (const name of [
      " a.md",
      "a/b.md",
      "a\u0007.md",
      "a".repeat(253) + ".md",
    ])
      expect((await make({ name })).status, name).toBe(400);
    expect(
      (await make({ name: "notes.text", mediaType: "text/plain" })).status,
    ).toBe(400);
    expect((await make({ name: "a.md", mediaType: "text/plain" })).status).toBe(
      400,
    );
    expect((await make({ sizeBytes: "0" })).status).toBe(400);
    expect(
      (
        await make({
          sizeBytes: String(1_048_576),
          clientToken: "mib",
          sha256: sha(Buffer.from("mib")),
        })
      ).status,
    ).toBe(200);
    expect(refusal(await make({ sizeBytes: String(1_048_577) }))).toEqual([
      400,
      "a Markdown document holds 1 byte to 1024 KiB; this one is 1048577 bytes",
    ]);
    expect((await make({ sha256: "nope" })).status).toBe(400);
    expect(refusal(await make({ setId: "" }))).toEqual([
      400,
      "set_id is required: the document set to upload into",
    ]);
    expect(refusal(await make({ setId: "set_other" }))).toEqual([
      404,
      "document set not found",
    ]);
    expect((await make({})).status).toBe(200);
  });

  it("loads a done agent draft: tables, then documents, into the ontology's database", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    // Only a done agent draft loads.
    expect(
      refusal(
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_1",
          clientToken: "x",
        }),
      ),
    ).toEqual([
      400,
      "only an ontology agent draft can be loaded; generate one",
    ]);
    const started = await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "go",
    });
    const load = started.body.load;
    expect(load).toMatchObject({
      status: "writing",
      current: false,
      versionId: "ver_seed_3",
      version: "foundry-v3",
      jobs: [
        { id: load.id, kind: "load", status: "queued", phase: "provisioning" },
      ],
    });
    // One live load; the same token is the same load.
    expect(
      refusal(
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: "again",
        }),
      ),
    ).toEqual([
      400,
      "a load or extract of this ontology is already queued or running",
    ]);
    expect(
      (
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: "go",
        })
      ).body.load.id,
    ).toBe(load.id);
    expect((await call("GET", O)).body.ontology.activeJob).toMatchObject({
      kind: "load",
      loadId: load.id,
    });
    vi.setSystemTime(Date.now() + 10_000);
    const finished = (await call("GET", `${O}/loads/${load.id}`)).body.load;
    expect(finished).toMatchObject({ status: "ready", current: true });
    expect(finished.jobs[0]).toMatchObject({
      status: "succeeded",
      progress: { chunksDone: 2, documentsDone: 1, documentsTotal: 1 },
      usdMicros: "18400",
    });
    const doc = (await call("GET", `${O}/documents`)).body.documents[0];
    expect(doc.extraction).toMatchObject({
      status: "extracted",
      sections: 4,
      entities: 3,
      facts: 2,
      reviews: 1,
    });
    const ontology = (await call("GET", O)).body.ontology;
    expect(ontology).toMatchObject({
      currentLoad: { id: load.id, version: "foundry-v3" },
      lastLoad: { jobId: load.id, status: "succeeded" },
      activeJob: null,
    });
    expect((await read("ten_acme_contracts", count("Customer"))).body).toEqual({
      n: 64,
    });
    // A document uploaded once a load is current queues an extract of that
    // load: it reads only the documents.
    await uploaded("notes.txt", Buffer.from("Initech asked for a discount."));
    const jobs = (await call("GET", `${O}/loads`)).body.loads[0].jobs;
    expect(jobs).toHaveLength(2);
    expect(jobs[0]).toMatchObject({
      kind: "extract",
      status: "queued",
      phase: "documents",
    });
    vi.setSystemTime(Date.now() + 10_000);
    const second = (await call("GET", `${O}/loads/${load.id}`)).body.load
      .jobs[0];
    expect(second).toMatchObject({
      kind: "extract",
      status: "succeeded",
      phase: "finishing",
      progress: { chunksDone: 0, documentsDone: 1, documentsTotal: 1 },
    });
    expect(
      (await read("ten_acme_contracts", count(FOUNDRY_NODES.Document))).body,
    ).toEqual({
      n: 2,
    });
  });

  it("retries a load's attempt while its database is starting", async () => {
    Object.assign(config, {
      loadStepMs: 100,
      startingQueries: { contracts: 2 },
    });
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "go",
      })
    ).body.load.id;
    const at = async (ms: number) => {
      vi.setSystemTime(Date.now() + ms);
      return (await call("GET", `${O}/loads/${id}`)).body.load.jobs[0];
    };
    // Queued, running, then the ingest's first write is refused: the
    // attempt fails and the load waits, queued, to try again.
    expect(await at(350)).toMatchObject({
      status: "queued",
      phase: "ingest",
      error: "the database refused a write; Helix will retry",
    });
    expect((await at(100)).status).toBe("queued");
    expect(await at(10_000)).toMatchObject({
      status: "succeeded",
      error: "",
      progress: { chunksDone: 2 },
    });
    expect(config).toMatchObject({ startingQueries: { contracts: 0 } });
  });

  it("writes foundry_load on every node and edge, and a marker per load", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    const load = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "a",
      })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    await call("GET", `${O}/loads`);
    const g = tenantGraph("ten_acme_contracts");
    expect(g.nodes.size).toBeGreaterThan(100);
    for (const n of g.nodes.values())
      expect(n.props.foundry_load, n.label).toBe(load.id);
    for (const e of g.edges.values())
      expect(e.props.foundry_load, e.label).toBe(load.id);
    const marker = g
      .view(load.id)
      .find(FOUNDRY_NODES.Load, "foundry_load", load.id);
    expect(marker?.props).toMatchObject({
      ontology_version: "foundry-v3",
      deleting: false,
      node_labels: expect.arrayContaining([
        "Customer",
        "Invoice",
        "Contract",
        FOUNDRY_NODES.Document,
        FOUNDRY_NODES.Load,
      ]),
      edge_labels: expect.arrayContaining(["billed_to", "signed_with"]),
    });
  });

  it("reads MDX as Markdown with its code blanked, at the original's offsets", async () => {
    Object.assign(config, { loadStepMs: 100 });
    const original = [
      'import { Callout } from "./callout";',
      "",
      "# Renewal",
      "",
      '<Callout type="info">',
      "Globex Corporation pays {fmt(“€”, 48000)} a year.",
      "</Callout>",
    ].join("\n");
    const doc = await uploaded("renewal.mdx", Buffer.from(original));
    const load = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "mdx",
      })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    await call("GET", `${O}/loads`);
    const g = tenantGraph("ten_acme_contracts").view(load.id);
    expect(
      g.find(FOUNDRY_NODES.Document, "doc_id", doc.id)?.props.media_type,
    ).toBe("markdown");
    const sections = [...g.nodes()].filter(
      (n) => n.label === FOUNDRY_NODES.Section && n.props.doc_id === doc.id,
    );
    const bytes = Buffer.from(original);
    const callout = sections.find((s) =>
      String(s.props.text).includes("<Callout"),
    )!;
    // Its text is the blanked bytes at its range: the original's, with
    // spaces where the code was.
    const { start, end, text } = callout.props as {
      start: number;
      end: number;
      text: string;
    };
    expect(Buffer.byteLength(text)).toBe(end - start);
    expect(text).not.toContain("fmt");
    expect(bytes.subarray(start, end).toString()).toBe(
      '# Renewal\n\n<Callout type="info">\nGlobex Corporation pays {fmt(“€”, 48000)} a year.\n</Callout>',
    );
    // The import is no section's; the name is found where it was written.
    expect(sections.some((s) => String(s.props.text).includes("import"))).toBe(
      false,
    );
    const mention = [...g.nodes()].find(
      (n) =>
        n.label === FOUNDRY_NODES.Mention &&
        n.props.doc_id === doc.id &&
        n.props.surface === "Globex Corporation",
    )!;
    const at = start + Number(mention.props.start);
    expect(
      bytes.subarray(at, start + Number(mention.props.end)).toString(),
    ).toBe("Globex Corporation");
  });

  it("keeps several loads side by side; set-current and delete as the WFE does", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    const loadOnce = async (token: string) => {
      const l = (
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: token,
        })
      ).body.load;
      vi.setSystemTime(Date.now() + 10_000);
      return (await call("GET", `${O}/loads/${l.id}`)).body.load;
    };
    const a = await loadOnce("a");
    const b = await loadOnce("b");
    expect(a.status).toBe("ready");
    expect(b).toMatchObject({ status: "ready", current: true });
    // Two full copies: 64 customers each.
    expect((await read("ten_acme_contracts", count("Customer"))).body).toEqual({
      n: 128,
    });
    let loads = (await call("GET", `${O}/loads`)).body.loads;
    expect(loads.map((l: { id: string }) => l.id)).toEqual([b.id, a.id]);
    // The current load can't be deleted; a ready one can be made current.
    expect(refusal(await call("DELETE", `${O}/loads/${b.id}`))).toEqual([
      400,
      "the current load can't be deleted; make another load current first",
    ]);
    const made = await call("POST", `${O}/loads/${a.id}:set-current`, {});
    expect(made.body.load).toMatchObject({ id: a.id, current: true });
    expect((await call("GET", O)).body.ontology.currentLoad.id).toBe(a.id);
    // The team's own node, linked to one of b's: deleted with b.
    const g = tenantGraph("ten_acme_contracts");
    const theirs = g.addNode("Note", { text: "call Globex" });
    const globexOfB = g
      .view(b.id)
      .find("Customer", "entity_key", "cus_GLOBEX01")!;
    g.addEdge("about", theirs, globexOfB.id);
    const edgesBefore = g.edges.size;
    const deleting = await call("DELETE", `${O}/loads/${b.id}`);
    expect(deleting.body.load.status).toBe("deleting");
    // A load being deleted no longer serves reads.
    expect(
      refusal(
        await call("POST", `${O}/loads/${b.id}:read`, {
          readsJson: JSON.stringify([
            { entity: { type: "Customer", id: "cus_GLOBEX01" } },
          ]),
        }),
      ),
    ).toEqual([400, "the load is being deleted"]);
    expect((await call("GET", O)).body.ontology.activeJob).toMatchObject({
      kind: "delete",
      loadId: b.id,
    });
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", `${O}/loads/${b.id}`)).status).toBe(404);
    loads = (await call("GET", `${O}/loads`)).body.loads;
    expect(loads.map((l: { id: string }) => l.id)).toEqual([a.id]);
    expect((await read("ten_acme_contracts", count("Customer"))).body).toEqual({
      n: 64,
    });
    // The team's node stays; its edge to b's node went with it.
    expect(g.nodes.get(theirs)).toBeDefined();
    expect(g.edges.size).toBeLessThan(edgesBefore);
    expect([...g.edges.values()].some((e) => e.label === "about")).toBe(false);
  });

  it("owes the current load an extract: after a delete, or on set-current", async () => {
    Object.assign(config, { loadStepMs: 100 });
    const loadOnce = async (token: string) => {
      const l = (
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: token,
        })
      ).body.load;
      vi.setSystemTime(Date.now() + 10_000);
      return l.id as string;
    };
    const a = await loadOnce("a");
    const b = await loadOnce("b");
    // An upload while a's delete runs: one extract of b follows it.
    await call("DELETE", `${O}/loads/${a}`);
    await uploaded("contract.md", CONTRACT);
    vi.setSystemTime(Date.now() + 10_000);
    const kinds = async (id: string) =>
      (await call("GET", `${O}/loads/${id}`)).body.load.jobs.map(
        (j: { kind: string; status: string }) => `${j.kind}:${j.status}`,
      );
    expect(await kinds(b)).toEqual(["extract:succeeded", "load:succeeded"]);
    // c is current when a second document arrives; b lacks it, so making b
    // current again reads it into b.
    const c = await loadOnce("c");
    await uploaded("notes.txt", Buffer.from("Initech asked for a discount."));
    vi.setSystemTime(Date.now() + 10_000);
    expect(await kinds(c)).toEqual(["extract:succeeded", "load:succeeded"]);
    await call("POST", `${O}/loads/${b}:set-current`, {});
    vi.setSystemTime(Date.now() + 10_000);
    expect(await kinds(b)).toEqual([
      "extract:succeeded",
      "extract:succeeded",
      "load:succeeded",
    ]);
  });

  it("keeps an API source's pieces out of a load a person's job writes: the extract its end owes reads them", async () => {
    Object.assign(config, { loadStepMs: 1000, ontologyStepMs: 100 });
    const source = (
      await call("POST", `${WS}/connectors`, {
        name: "Replies",
        config: { events: { textField: "reply" } },
      })
    ).body.connector;
    // A piece sent, then its sync (about a second) writes its document.
    const piece = async (id: string) => {
      expect(
        sendPieces(source.id, { pieces: [{ id, data: { reply: id } }] }).status,
      ).toBe(200);
      vi.setSystemTime(Date.now() + 1000);
      await call("GET", SETS);
    };
    await piece("t-1");
    await uploaded("early.md", Buffer.from("# Early\n"));
    const g = (
      await call("POST", `${O}:generate`, {
        noConnectors: true,
        documentSetIds: [GENERAL],
        eventSources: [
          {
            connectorId: source.id,
            mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION",
          },
        ],
      })
    ).body.generation;
    vi.setSystemTime(Date.now() + 10_000);
    const v = (await call("GET", `${O}/generations/${g.id}`)).body.generation
      .resultVersionId;
    await call("POST", `${O}:set-current`, {
      versionId: v,
      expectedCurrentVersionId: "ver_seed_1",
    });
    const start = Date.now();
    const load = (
      await call("POST", `${O}/loads`, { versionId: v, clientToken: "ada" })
    ).body.load;
    // While Ada's load runs (a step a second): a piece, then an upload of
    // hers, which joins alone, never bringing the piece with it.
    await piece("t-2");
    await uploaded("note.md", Buffer.from("# Note\n"));
    // Her job reads early.md, t-1 and note.md, and ends 7 s in, current; the
    // extract its end owes, no one's, takes t-2, and t-3, which arrives while
    // it runs.
    vi.setSystemTime(start + 7500);
    await piece("t-3");
    vi.setSystemTime(start + 60_000);
    const jobs = (await call("GET", `${O}/loads/${load.id}`)).body.load.jobs;
    expect(
      jobs.map(
        (j: { kind: string; status: string; progress: any }) =>
          `${j.kind}:${j.status}:${j.progress.documentsTotal}`,
      ),
    ).toEqual(["extract:succeeded:2", "load:succeeded:3"]);
    expect(
      (await call("GET", `${O}/documents?status=waiting`)).body.documents,
    ).toEqual([]);
  });

  it("holds an API source's pieces back from work no one started once its allowance is spent, and a person's load reads them", async () => {
    Object.assign(config, { loadStepMs: 1000, ontologyStepMs: 100 });
    const source = (
      await call("POST", `${WS}/connectors`, {
        name: "Replies",
        config: { events: { textField: "reply" } },
      })
    ).body.connector;
    const piece = async (id: string) => {
      expect(
        sendPieces(source.id, {
          pieces: [{ id, data: { reply: id, body: `Body ${id}` } }],
        }).status,
      ).toBe(200);
      vi.setSystemTime(Date.now() + 1000);
      await call("GET", SETS);
    };
    const pieces = async () =>
      (await call("GET", SETS)).body.sets.find(
        (s: { connector?: { connectorId: string } }) =>
          s.connector?.connectorId === source.id,
      );
    await piece("t-1");
    const setId = (await pieces()).id;
    // The source's documents the current load hasn't read.
    const waiting = async () =>
      (
        (await call("GET", `${O}/documents?status=waiting`)).body.documents as {
          setId: string;
        }[]
      ).filter((d) => d.setId === setId);
    const g = (
      await call("POST", `${O}:generate`, {
        noConnectors: true,
        eventSources: [
          {
            connectorId: source.id,
            mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION",
          },
        ],
      })
    ).body.generation;
    vi.setSystemTime(Date.now() + 10_000);
    const v = (await call("GET", `${O}/generations/${g.id}`)).body.generation
      .resultVersionId;
    await call("POST", `${O}:set-current`, {
      versionId: v,
      expectedCurrentVersionId: "ver_seed_1",
    });
    const first = (
      await call("POST", `${O}/loads`, { versionId: v, clientToken: "ada" })
    ).body.load;
    vi.setSystemTime(Date.now() + 60_000);
    expect(
      (await call("GET", `${O}/loads/${first.id}`)).body.load,
    ).toMatchObject({ status: "ready" });
    expect(await waiting()).toEqual([]);

    // Spent: a send's sync writes nothing into the set, which holds t-1.
    Object.assign(config, { eventSetAllowanceSpent: true });
    expect((await pieces()).connector.paused).toBe(true);
    await piece("t-2");
    expect((await pieces()).documents).toBe(1);
    // A text field's change is the person's sync: it writes both pieces by
    // the new field. The extract it owes is no one's: it holds them back,
    // and ends at the cap naming the source.
    expect(
      (
        await call("PATCH", `${WS}/connectors/${source.id}`, {
          config: { events: { textField: "body" } },
        })
      ).status,
    ).toBe(200);
    vi.setSystemTime(Date.now() + 1000);
    expect((await pieces()).documents).toBe(2);
    vi.setSystemTime(Date.now() + 60_000);
    const held = (await call("GET", `${O}/loads/${first.id}`)).body.load.jobs;
    expect(
      held.map(
        (j: { kind: string; status: string; progress: any }) =>
          `${j.kind}:${j.status}:${j.progress.documentsTotal}`,
      ),
    ).toEqual(["extract:budget_exhausted:0", "load:succeeded:1"]);
    expect(held[0].error).toBe(
      'the API source "Replies" spent its $5.00 of this month\'s extraction by work no one started; its newest pieces wait for next month or a load a person starts',
    );
    expect(await waiting()).toHaveLength(2);
    // Held, it is not tried again.
    vi.setSystemTime(Date.now() + 5 * 60_000);
    expect(
      (await call("GET", `${O}/loads/${first.id}`)).body.load.jobs,
    ).toHaveLength(2);

    // A load a person starts reads them.
    const second = (
      await call("POST", `${O}/loads`, { versionId: v, clientToken: "ada-2" })
    ).body.load;
    vi.setSystemTime(Date.now() + 60_000);
    const read = (await call("GET", `${O}/loads/${second.id}`)).body.load;
    expect(read).toMatchObject({ status: "ready" });
    expect(read.jobs.map((j: { status: string }) => j.status)).toEqual([
      "succeeded",
    ]);
    expect(await waiting()).toEqual([]);
  });

  it("lets a new load go before a delete, which runs again after it", async () => {
    Object.assign(config, { loadStepMs: 100 });
    const loadOnce = async (token: string) => {
      const l = (
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: token,
        })
      ).body.load;
      vi.setSystemTime(Date.now() + 10_000);
      return l.id as string;
    };
    const a = await loadOnce("a");
    await loadOnce("b");
    await call("DELETE", `${O}/loads/${a}`);
    // The delete is queued; a new load stops it.
    const c = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "c",
      })
    ).body.load;
    const stopped = (await call("GET", `${O}/loads/${a}`)).body.load;
    expect(stopped.status).toBe("deleting");
    expect(stopped.jobs[0]).toMatchObject({
      kind: "delete",
      status: "cancelled",
      loadId: a,
    });
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", `${O}/loads/${c.id}`)).body.load.current).toBe(
      true,
    );
    expect((await call("GET", `${O}/loads/${a}`)).status).toBe(404);
  });

  it("keeps the current load and the newest others, five in all", async () => {
    Object.assign(config, { loadStepMs: 50 });
    const ids: string[] = [];
    for (const token of ["1", "2", "3", "4", "5", "6"]) {
      ids.push(
        (
          await call("POST", `${O}/loads`, {
            versionId: "ver_seed_3",
            clientToken: token,
          })
        ).body.load.id,
      );
      vi.setSystemTime(Date.now() + 5_000);
      await call("GET", `${O}/loads`);
    }
    vi.setSystemTime(Date.now() + 5_000);
    const loads = (await call("GET", `${O}/loads`)).body.loads;
    // The oldest was deleted by the chain once the sixth load ended.
    expect(loads.map((l: { id: string }) => l.id)).toEqual(
      ids.slice(1).reverse(),
    );
    expect(loads[0].current).toBe(true);
  });

  it("lists a load's newest 20 jobs, with the count and spend of all", async () => {
    Object.assign(config, { loadStepMs: 50 });
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "t",
      })
    ).body.load.id;
    vi.setSystemTime(Date.now() + 5_000);
    // One extract per upload, each read alone.
    for (let i = 0; i < 22; i++) {
      await uploaded(`note-${i}.txt`, Buffer.from(`Initech note ${i}.`));
      vi.setSystemTime(Date.now() + 5_000);
      await call("GET", `${O}/loads`);
    }
    const l = (await call("GET", `${O}/loads/${id}`)).body.load;
    expect(l.jobCount).toBe(23);
    expect(l.jobs).toHaveLength(20);
    expect(l.jobs.every((j: { kind: string }) => j.kind === "extract")).toBe(
      true,
    );
    const listed = l.jobs.reduce(
      (n: number, j: { usdMicros: string }) => n + Number(j.usdMicros),
      0,
    );
    expect(Number(l.usdMicros)).toBeGreaterThan(listed);
  });

  it("refuses set-current and delete as the WFE does, and retries a failed delete", async () => {
    Object.assign(config, { loadStepMs: 100, loadOutcome: "fail" });
    const failed = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "f",
      })
    ).body.load;
    // Writing: neither deleted nor made current.
    expect(refusal(await call("DELETE", `${O}/loads/${failed.id}`))).toEqual([
      400,
      "a job is writing this load; delete it once the job ends",
    ]);
    vi.setSystemTime(Date.now() + 10_000);
    expect(
      (await call("GET", `${O}/loads/${failed.id}`)).body.load.status,
    ).toBe("failed");
    expect(
      refusal(await call("POST", `${O}/loads/${failed.id}:set-current`, {})),
    ).toEqual([400, "only a ready or partial load can be made current"]);
    Object.assign(config, { deleteOutcome: "fail" });
    await call("DELETE", `${O}/loads/${failed.id}`);
    vi.setSystemTime(Date.now() + 10_000);
    expect(
      (await call("GET", `${O}/loads/${failed.id}`)).body.load.status,
    ).toBe("delete_failed");
    Object.assign(config, { deleteOutcome: "succeed" });
    expect(
      (await call("DELETE", `${O}/loads/${failed.id}`)).body.load.status,
    ).toBe("deleting");
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", `${O}/loads/${failed.id}`)).status).toBe(404);
    expect(
      refusal(await call("POST", `${O}/loads/nope:set-current`, {})),
    ).toEqual([404, "load not found"]);
  });

  it("fails, stops at the cap, or reads a document as failed, as told", async () => {
    Object.assign(config, { loadStepMs: 100, loadOutcome: "sources_changed" });
    await uploaded("contract.md", CONTRACT);
    const load = async (token: string) => {
      const id = (
        await call("POST", `${O}/loads`, {
          versionId: "ver_seed_3",
          clientToken: token,
        })
      ).body.load.id;
      vi.setSystemTime(Date.now() + 10_000);
      return (await call("GET", `${O}/loads/${id}`)).body.load;
    };
    const changed = await load("a");
    expect(changed.status).toBe("failed");
    expect(changed.jobs[0]).toMatchObject({
      status: "failed",
      error:
        "the tables were synced again while this load was written; start a new load",
    });
    Object.assign(config, { loadOutcome: "succeed", documentOutcome: "fail" });
    expect((await load("b")).status).toBe("ready");
    expect(
      (await call("GET", `${O}/documents`)).body.documents[0].extraction,
    ).toMatchObject({
      status: "failed",
      error: { code: "model", retryable: true },
    });
    Object.assign(config, {
      loadOutcome: "budget_exhausted",
      documentOutcome: "succeed",
    });
    await uploaded("a.txt", Buffer.from("Hooli Inc"));
    vi.setSystemTime(Date.now() + 10_000);
    const partial = await load("c");
    // Partial, and not current while a ready load is.
    expect(partial).toMatchObject({ status: "partial", current: false });
  });

  it("reads a PDF's text as plain text, records one with no text for good, at no cost, and loads no table file as a document", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    await uploaded("renewals.csv", Buffer.from("customer\nGlobex\n"));
    const terms = await uploaded("terms.pdf", TERMS);
    await uploaded("scan.pdf", textPdf([[], []]));
    // Their reads done before the load.
    vi.setSystemTime(Date.now() + 2_000);
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "go",
      })
    ).body.load.id;
    vi.setSystemTime(Date.now() + 10_000);
    const load = (await call("GET", `${O}/loads/${id}`)).body.load;
    expect(load).toMatchObject({ status: "ready", current: true });
    // The Markdown document and the PDF's text cost a read each; the CSV
    // isn't a member.
    expect(load.jobs[0]).toMatchObject({
      progress: { documentsDone: 3, documentsTotal: 3 },
      usdMicros: "36800",
    });
    const extractions = Object.fromEntries(
      (await call("GET", `${O}/documents`)).body.documents.map(
        (d: { name: string; extraction: unknown }) => [d.name, d.extraction],
      ),
    );
    expect(extractions["contract.md"]).toMatchObject({ status: "extracted" });
    expect(extractions["terms.pdf"]).toMatchObject({
      status: "extracted",
      sections: 1,
    });
    expect(extractions["scan.pdf"]).toMatchObject({
      status: "failed",
      error: { code: "unsupported", retryable: false },
      usdMicros: "0",
    });
    expect(extractions["renewals.csv"]).toBeFalsy();
    // The PDF's section is its text as read, plain, its pages' break in it.
    const graph = tenantGraph("ten_acme_contracts").view(id);
    expect(
      graph.find(FOUNDRY_NODES.Document, "doc_id", terms.id)?.props.media_type,
    ).toBe("plain_text");
    expect(
      graph.find(FOUNDRY_NODES.Section, "doc_id", terms.id)?.props.text,
    ).toBe(
      "Renewal terms\nGlobex Corporation renews on 1 December 2026.\n\fNotice\nEither party gives 90 days notice.",
    );
    // Not retried: a later extract reads none of them again.
    expect(
      (await call("GET", `${O}/documents?status=waiting`)).body.documents,
    ).toEqual([]);
  });

  it("reads a PDF into the current load once its text is read", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    await call("POST", `${O}/loads`, {
      versionId: "ver_seed_3",
      clientToken: "first",
    });
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", O)).body.ontology.currentLoad).toBeTruthy();
    // Being read: the current load holds it, and waits for its text.
    const created = await call("POST", DOCS, {
      name: "terms.pdf",
      mediaType: "application/pdf",
      sizeBytes: String(TERMS.length),
      sha256: sha(TERMS),
      clientToken: "terms",
      setId: GENERAL,
    });
    const { document, upload } = created.body;
    await put(upload.url, TERMS, upload.headers);
    const done = await call("POST", `${DOCS}/${document.id}:complete`, {});
    expect(outcomes(done)).toEqual({ ont_acme_contracts: "READING" });
    expect(done.body.document.read).toMatchObject({
      kind: "FOUNDRY_READ_KIND_PDF",
      state: "FOUNDRY_FILE_READ_STATE_READING",
    });
    expect(
      (await call("GET", `${O}/documents?status=waiting`)).body.documents.map(
        (d: { name: string }) => d.name,
      ),
    ).toEqual(["terms.pdf"]);
    // Read, the ontology that reads its set reads it into its current load.
    vi.setSystemTime(Date.now() + 2_000);
    expect((await call("GET", O)).body.ontology.activeJob).toMatchObject({
      kind: "extract",
    });
    vi.setSystemTime(Date.now() + 10_000);
    expect(
      (await call("GET", `${O}/documents/${document.id}`)).body.document
        .extraction,
    ).toMatchObject({ status: "extracted" });
    // Read at once, it is read as its upload completes, as text is.
    Object.assign(config, { documentReadMs: 0 });
    const more = await uploaded(
      "more.pdf",
      textPdf([["Initech LLC renews on 15 January 2027."]]),
    );
    expect(more.read).toMatchObject({
      state: "FOUNDRY_FILE_READ_STATE_READY",
      pages: 1,
    });
    expect((await call("GET", O)).body.ontology.activeJob).toMatchObject({
      kind: "extract",
    });
  });

  it("reads a load with its rules evaluated, and sweeps its rule breaks", async () => {
    Object.assign(config, { loadStepMs: 100 });
    await uploaded("contract.md", CONTRACT);
    const load = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: "rules",
      })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    await call("GET", `${O}/loads`);
    // The sweep at its end stored one record per invoice whose customer no
    // contract names: all but Globex's two and Initech's one.
    expect(
      (await read("ten_acme_contracts", count(FOUNDRY_NODES.Violation))).body,
    ).toEqual({ n: 62 });
    const reads = (r: unknown[], id = load.id) =>
      call("POST", `${O}/loads/${id}:read`, { readsJson: JSON.stringify(r) });
    const globex = { type: "Customer", id: "cus_GLOBEX01" };
    const r = await reads([
      { entity: globex },
      {
        explain: {
          pred: "relation:co_signed",
          args: [globex, { type: "Customer", id: "cus_INITECH2" }],
        },
      },
      { violations: { constraint: "invoice_under_contract", limit: 50 } },
      { entity: { type: "Customer", id: "nobody" } },
    ]);
    expect(r.status).toBe(200);
    const result = JSON.parse(r.body.resultJson);
    expect(contractErrors("read_result", result)).toEqual([]);
    expect(result.as_of).toBe("2026-10-18");
    const [entity, explain, violations, unknown] = result.results;
    expect(entity.ok.flags.map((f: { name: string }) => f.name).sort()).toEqual(
      ["invoiced", "under_contract"],
    );
    expect(
      entity.ok.derived.map((f: { pred: string; args: { id: string }[] }) => [
        f.pred,
        f.args.map((a) => a.id),
      ]),
    ).toEqual([
      ["relation:co_signed", ["cus_GLOBEX01", "cus_INITECH2"]],
      ["relation:co_signed", ["cus_INITECH2", "cus_GLOBEX01"]],
    ]);
    expect(entity.ok.violations).toEqual([
      {
        constraint: "co_signed.self_loop",
        severity: "warn",
        entities: [globex],
        stored: false,
      },
    ]);
    expect(explain.ok.steps[explain.ok.root].via).toBe("rule:co_signers");
    // A property's evidence: each document quote stating a value of it,
    // with the value.
    const msa = { type: "Contract", id: "master services agreement" };
    const ev = JSON.parse(
      (
        await reads([
          { evidence: { entity: msa, prop: "signed_on" } },
          { evidence: { entity: msa } },
        ])
      ).body.resultJson,
    );
    expect(contractErrors("read_result", ev)).toEqual([]);
    expect(ev.results[0].ok.items).toEqual([
      expect.objectContaining({ kind: "quote", value: "2026-10-01" }),
    ]);
    expect(ev.results[1].ok.items).toEqual([]);
    // A full page of the 62 records, and where the next starts.
    expect(violations.ok.records).toHaveLength(50);
    expect(violations.ok.next).toBe(violations.ok.records[49].violation_key);
    expect(unknown.error.code).toBe("unknown_entity");
    // A pred told to fail.
    Object.assign(config, { readErrors: { "derived:invoiced": "budget" } });
    const told = JSON.parse(
      (await reads([{ entity: globex }])).body.resultJson,
    );
    expect(contractErrors("read_result", told)).toEqual([]);
    expect(told.results[0].ok.errors).toEqual([
      {
        pred: "derived:invoiced",
        code: "budget",
        message: expect.any(String),
        limit: 64,
      },
    ]);
    // Refusals, as the WFE's.
    expect(refusal(await reads([]))).toEqual([
      400,
      "readsJson must be a list of reads",
    ]);
    expect(refusal(await reads([{ violations: { constraint: 3 } }]))[0]).toBe(
      400,
    );
    expect(refusal(await reads([{ entity: globex }], "load_nope"))).toEqual([
      404,
      "load not found",
    ]);
    Object.assign(config, { ontologyDatabaseHidden: true });
    expect(refusal(await reads([{ entity: globex }]))).toEqual([
      403,
      "permission denied",
    ]);
  });
});

describe("review decisions", () => {
  const variant = Buffer.from(
    "# Variant contract\n\nGlobexx Corporation signed. The Agreement runs for 24 months.\n",
  );
  async function loaded(token: string) {
    const load = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_seed_3",
        clientToken: token,
      })
    ).body.load;
    vi.setSystemTime(Date.now() + 10_000);
    expect((await call("GET", `${O}/loads/${load.id}`)).body.load.status).toBe(
      "ready",
    );
    return tenantGraph("ten_acme_contracts").view(load.id);
  }
  const reviews = (g: Awaited<ReturnType<typeof loaded>>) =>
    [...g.nodes()].filter((n) => n.label === FOUNDRY_NODES.Review);

  it("suggests candidates; settles, keeps old data, reloads linked, and removes a decision", async () => {
    await uploaded("variant.md", variant);
    const old = await loaded("first");
    const rv = reviews(old)[0]!;
    expect(rv.props).toMatchObject({
      reason: "unresolved",
      type: "Customer",
      norm_name: "globexx corporation",
    });
    expect(rv.props.candidates).toContain("cus_GLOBEX01");
    const at = `${O}/reviews/${rv.props.review_key}:settle`;
    const saved = (
      await call("POST", at, {
        decision: { resolve: { entityKey: "cus_GLOBEX01" } },
      })
    ).body.decision;
    expect(saved).toMatchObject({
      kind: "resolve",
      type: "Customer",
      docId: expect.any(String),
      normName: "globexx corporation",
      entityKey: "cus_GLOBEX01",
    });
    expect(reviews(old)).toHaveLength(1);
    expect(
      [...old.nodes()].filter((n) => n.props.resolved_by === "user"),
    ).toHaveLength(0);
    const next = await loaded("next");
    expect(reviews(next)).toHaveLength(0);
    expect(
      [...next.nodes()].filter((n) => n.props.resolved_by === "user"),
    ).toHaveLength(1);
    expect(
      (await call("GET", `${O}/review-decisions`)).body.decisions,
    ).toHaveLength(1);
    // A review from an old load can no longer be settled.
    expect(
      (await call("POST", at, { decision: { resolve: { notInData: true } } }))
        .status,
    ).toBe(404);
    expect(
      (await call("DELETE", `${O}/review-decisions/${saved.id}`)).status,
    ).toBe(200);
    expect(reviews(await loaded("third"))).toHaveLength(1);
    expect(
      (await call("DELETE", `${O}/review-decisions/${saved.id}`)).status,
    ).toBe(404);
  });

  it("settles the current review after an unchanged reload retains the same review key", async () => {
    await uploaded("variant.md", variant);
    const old = await loaded("first");
    const next = await loaded("unchanged");
    const previous = reviews(old)[0]!;
    const current = reviews(next)[0]!;
    expect(current.id).not.toBe(previous.id);
    expect(current.props.review_key).toBe(previous.props.review_key);
    const settled = await call(
      "POST",
      `${O}/reviews/${current.props.review_key}:settle`,
      { decision: { resolve: { entityKey: "cus_GLOBEX01" } } },
    );
    expect(settled.status).toBe(200);
    expect(settled.body.decision).toMatchObject({
      kind: "resolve",
      entityKey: "cus_GLOBEX01",
    });
    expect(reviews(old)).toHaveLength(1);
    expect(reviews(next)).toHaveLength(1);
  });

  it("replaces a decision with not-in-data, with no duplicate and no link", async () => {
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    const at = `${O}/reviews/${rv.props.review_key}:settle`;
    const a = await call("POST", at, {
      decision: { resolve: { entityKey: "cus_GLOBEX01" } },
    });
    const b = await call("POST", at, {
      decision: { resolve: { notInData: true } },
    });
    expect(b.body.decision.id).toBe(a.body.decision.id);
    expect(b.body.decision.entityKey).toBeUndefined();
    const next = await loaded("next");
    expect(reviews(next)).toHaveLength(0);
    expect(
      [...next.nodes()].filter(
        (n) =>
          n.label === FOUNDRY_NODES.Mention &&
          n.props.surface === "Globexx Corporation",
      ),
    ).toHaveLength(0);
  });

  it("settles property conflicts without changing old evidence", async () => {
    await uploaded("contract.md", CONTRACT);
    await uploaded(
      "amendment.md",
      Buffer.from(
        "# Master services agreement\n\nGlobex Corporation signed. The Agreement runs for 36 months.\n",
      ),
    );
    const g = await loaded("first");
    const rv = reviews(g).find((n) => n.props.reason === "prop_conflict")!;
    expect(typeof rv.props.values).toBe("string");
    const contract = g.find(
      "Contract",
      "norm_name",
      "master services agreement",
    )!;
    expect(contract.props.term_months).toBeUndefined();
    // term_months is an integer: the backend refuses any other kind, and a choice
    // that isn't one FoundryPropValue arm.
    for (const value of [
      { boolean: true },
      { text: "36" },
      { number: 36.5 },
      { integer: "36.5" },
      { integer: "9223372036854775808" },
      { integer: "36", text: "36" },
      36,
      [],
      {},
    ]) {
      expect(
        (
          await call("POST", `${O}/reviews/${rv.props.review_key}:settle`, {
            decision: { prop: value },
          })
        ).status,
        JSON.stringify(value),
      ).toBe(400);
    }
    const saved = await call(
      "POST",
      `${O}/reviews/${rv.props.review_key}:settle`,
      { decision: { prop: { integer: "36" } } },
    );
    expect(saved.body.decision).toMatchObject({
      kind: "prop",
      type: "Contract",
      entityKey: "master services agreement",
      prop: "term_months",
      value: { integer: "36" },
    });
    expect(contract.props.term_months).toBeUndefined();
    const next = await loaded("next");
    expect(
      next.find("Contract", "norm_name", "master services agreement")!.props
        .term_months,
    ).toBe(36);
    expect(reviews(next).some((n) => n.props.reason === "prop_conflict")).toBe(
      false,
    );
    expect(
      [...next.nodes()].filter(
        (n) =>
          n.props.prop === "term_months" && n.label === FOUNDRY_NODES.Evidence,
      ),
    ).toHaveLength(2);
  });

  it("seeds an opt-in adjudicated review and undoes it on the next load", async () => {
    Object.assign(config, { documentAdjudicated: true });
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    expect(rv.props.reason).toBe("adjudicated");
    expect(
      [...g.nodes()].some((n) => n.props.resolved_by === "adjudicated"),
    ).toBe(true);
    expect(
      (
        await call("POST", `${O}/reviews/${rv.props.review_key}:settle`, {
          decision: { resolve: { notInData: true } },
        })
      ).status,
    ).toBe(200);
    const next = await loaded("next");
    expect(
      [...next.nodes()].some((n) => n.props.resolved_by === "adjudicated"),
    ).toBe(false);
    expect(reviews(next)).toHaveLength(0);
  });

  it("pages decisions with an opaque last-id token and no duplicates", async () => {
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    for (let i = 0; i < 101; i++) {
      const key = `review-${i}`;
      g.addNode(FOUNDRY_NODES.Review, {
        ...rv.props,
        review_key: key,
        norm_name: `variant ${i}`,
      });
      expect(
        (
          await call("POST", `${O}/reviews/${key}:settle`, {
            decision: { resolve: { notInData: true } },
          })
        ).status,
      ).toBe(200);
    }
    const first = (await call("GET", `${O}/review-decisions`)).body;
    expect(first.decisions).toHaveLength(100);
    expect(first.nextPageToken).toBe(first.decisions.at(-1).id);
    const last = (
      await call(
        "GET",
        `${O}/review-decisions?pageToken=${encodeURIComponent(first.nextPageToken)}`,
      )
    ).body;
    expect(last.decisions).toHaveLength(1);
    expect(last.nextPageToken).toBe("");
    expect(
      new Set(
        [...first.decisions, ...last.decisions].map(
          (d: { id: string }) => d.id,
        ),
      ).size,
    ).toBe(101);
  });

  it("guards membership, permissions, choice shape, review scope and candidate existence", async () => {
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    const at = `${O}/reviews/${rv.props.review_key}:settle`,
      body = { decision: { resolve: { notInData: true } } };
    for (const decision of [
      undefined,
      {},
      [],
      { resolve: { notInData: false } },
      { resolve: { entityKey: "" } },
      { resolve: { entityKey: "cus_GLOBEX01", notInData: true } },
      { resolve: { notInData: true }, prop: { boolean: false } },
      { prop: 2 },
      { prop: { number: 2, integer: "2" } },
    ])
      expect((await call("POST", at, { decision })).status).toBe(400);
    expect(
      refusal(
        await call("POST", at, {
          decision: { resolve: { entityKey: "missing" } },
        }),
      ),
    ).toEqual([400, "the chosen entity is not in this load"]);
    expect((await call("POST", at, body, "usr_alan")).status).toBe(403);
    expect(
      (await call("GET", `${O}/review-decisions`, undefined, "usr_alan"))
        .status,
    ).toBe(200);
    expect(
      (await call("POST", `${O}/reviews/missing:settle`, body)).status,
    ).toBe(404);
    const saved = (await call("POST", at, body)).body.decision;
    expect(
      (
        await call(
          "DELETE",
          `${O}/review-decisions/${saved.id}`,
          undefined,
          "usr_alan",
        )
      ).status,
    ).toBe(403);
    const elsewhere = O.replace("ws_acme", "ws_elsewhere");
    expect((await call("GET", `${elsewhere}/review-decisions`)).status).toBe(
      403,
    );
    // A token is a decision id: the backend lists what follows it, and refuses only
    // one longer than an id.
    expect(
      (await call("GET", `${O}/review-decisions?pageToken=unknown`)).status,
    ).toBe(200);
    expect(
      (await call("GET", `${O}/review-decisions?pageToken=${"x".repeat(27)}`))
        .status,
    ).toBe(400);
  });

  it("refuses as the backend does: no current load, no identity, a type or prop the version lacks", async () => {
    const body = { decision: { resolve: { notInData: true } } };
    expect(
      refusal(await call("POST", `${O}/reviews/any:settle`, body)),
    ).toEqual([400, "load the ontology before settling a review"]);
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    const settle = (props: Record<string, unknown>, decision: unknown) => {
      const key = `synthetic-${reviews(g).length}`;
      g.addNode(FOUNDRY_NODES.Review, {
        ...rv.props,
        ...props,
        review_key: key,
      } as never);
      return call("POST", `${O}/reviews/${key}:settle`, { decision });
    };
    expect(refusal(await settle({ type: "" }, body.decision))).toEqual([
      400,
      "this review is missing its identity; start a new load before settling it",
    ]);
    expect(refusal(await settle({ type: "Ghost" }, body.decision))).toEqual([
      400,
      "the review type is not in the loaded version",
    ]);
    expect(refusal(await settle({ doc_id: "" }, body.decision))).toEqual([
      400,
      "the review is missing its decision identity",
    ]);
    expect(
      refusal(await settle({ reason: "unverified" }, body.decision)),
    ).toEqual([400, "this review kind cannot be settled"]);
    expect(
      refusal(
        await settle({ reason: "ambiguous" }, { prop: { integer: "1" } }),
      ),
    ).toEqual([400, "this review requires a resolve decision"]);
    const contract = (prop: string) => ({
      reason: "prop_conflict",
      type: "Contract",
      entity_key: "variant contract",
      prop,
    });
    expect(
      refusal(await settle(contract("ghost"), { prop: { text: "x" } })),
    ).toEqual([400, "the property is not in the loaded version"]);
    expect(
      refusal(await settle({ ...contract("signed_on") }, body.decision)),
    ).toEqual([400, "this review requires a prop decision"]);
  });

  it("checks a value against the prop's type, and takes null as a removal", async () => {
    await uploaded("variant.md", variant);
    const g = await loaded("first"),
      rv = reviews(g)[0]!;
    // A signed_on (date) conflict on the variant's Contract.
    g.addNode(FOUNDRY_NODES.Review, {
      ...rv.props,
      review_key: "dated",
      reason: "prop_conflict",
      type: "Contract",
      entity_key: "variant contract",
      prop: "signed_on",
    } as never);
    const at = `${O}/reviews/dated:settle`;
    for (const value of [
      { text: "2026-13-45" },
      { text: "2026-02-30" },
      { text: "2026-00" },
      { text: "2026-3" },
      { text: "14 March 2026" },
      { text: "" },
      { integer: "20260314" },
      { number: 20260314 },
    ])
      expect(
        refusal(await call("POST", at, { decision: { prop: value } })),
      ).toEqual([400, "the chosen value does not match the property's type"]);
    // A year or a month is a date; what's stored is what the engine keeps.
    for (const [value, kept] of [
      ["2026", "2026"],
      ["2026-03", "2026-03"],
      [" 2026-03-14 ", "2026-03-14"],
    ])
      expect(
        (await call("POST", at, { decision: { prop: { text: value } } })).body
          .decision.value,
      ).toEqual({ text: kept });
    // A string is trimmed, and a blank one refused.
    g.addNode(FOUNDRY_NODES.Review, {
      ...rv.props,
      review_key: "billed",
      reason: "prop_conflict",
      type: "Customer",
      entity_key: "cus_GLOBEX01",
      prop: "currency",
    } as never);
    const billed = `${O}/reviews/billed:settle`;
    expect(
      refusal(
        await call("POST", billed, { decision: { prop: { text: "   " } } }),
      ),
    ).toEqual([400, "the chosen value does not match the property's type"]);
    expect(
      (await call("POST", billed, { decision: { prop: { text: "  GBP " } } }))
        .body.decision.value,
    ).toEqual({ text: "GBP" });
    const removed = await call("POST", at, {
      decision: { prop: { null: null } },
    });
    expect(removed.body.decision).toMatchObject({
      kind: "prop",
      value: { null: null },
    });
    // The removal applies from the next load.
    const contract = g.find("Contract", "norm_name", "variant contract")!;
    contract.props.signed_on = "2026-01-01";
    const next = await loaded("next");
    expect(
      next.find("Contract", "norm_name", "variant contract")!.props,
    ).not.toHaveProperty("signed_on");
  });

  it("keeps a decision a newer draft drops until that draft is made current", async () => {
    await uploaded("contract.md", CONTRACT);
    await uploaded(
      "amendment.md",
      Buffer.from(
        "# Master services agreement\n\nGlobex Corporation signed. The Agreement runs for 36 months.\n",
      ),
    );
    const g = await loaded("first");
    const rv = reviews(g).find((n) => n.props.reason === "prop_conflict")!;
    // The mock's agent drafts from tables only: no Contract.
    Object.assign(config, { ontologyStepMs: 10 });
    expect((await call("POST", `${O}:generate`, {})).status).toBe(200);
    vi.setSystemTime(Date.now() + 50);
    expect(
      (await call("GET", `${O}/generations`)).body.generations[0].status,
    ).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    // The loaded version still has it: the backend takes it.
    expect(
      (
        await call("POST", `${O}/reviews/${rv.props.review_key}:settle`, {
          decision: { prop: { integer: "36" } },
        })
      ).status,
    ).toBe(200);
    const listed = async () =>
      (await call("GET", `${O}/review-decisions`)).body.decisions;
    expect(await listed()).toHaveLength(1);
    // Making the draft current drops what it doesn't take.
    const versions = (await call("GET", `${O}/versions`)).body.versions;
    const current = versions.find((v: { isCurrent: boolean }) => v.isCurrent);
    expect(
      (
        await call("POST", `${O}:set-current`, {
          versionId: versions[0].id,
          expectedCurrentVersionId: current.id,
        })
      ).status,
    ).toBe(200);
    expect(await listed()).toEqual([]);
  });

  it("lists only the quotes that disagree with a table's value, and a decision replaces it", async () => {
    await uploaded(
      "billing.md",
      Buffer.from(
        "# Billing terms\n\nGlobex Corporation is billed in GBP. Globex Corporation signed.\n",
      ),
    );
    await uploaded(
      "agrees.md",
      Buffer.from(
        "# Renewal\n\nGlobex Corporation is billed in USD. Globex Corporation renewed.\n",
      ),
    );
    const g = await loaded("first");
    const customer = g.find("Customer", "entity_key", "cus_GLOBEX01")!;
    // Rows win: the table's value stays.
    expect(customer.props.currency).toBe("usd");
    const conflicts = reviews(g).filter(
      (n) => n.props.reason === "prop_conflict",
    );
    expect(conflicts).toHaveLength(1);
    const rv = conflicts[0]!;
    expect(rv.props).toMatchObject({
      type: "Customer",
      entity_key: "cus_GLOBEX01",
      prop: "currency",
    });
    // The quote agreeing with the table is evidence, not a review value.
    expect(
      JSON.parse(String(rv.props.values)).map(
        (v: { value: string }) => v.value,
      ),
    ).toEqual(["GBP"]);
    const saved = await call(
      "POST",
      `${O}/reviews/${rv.props.review_key}:settle`,
      { decision: { prop: { text: "GBP" } } },
    );
    expect(saved.status).toBe(200);
    const next = await loaded("next");
    expect(
      next.find("Customer", "entity_key", "cus_GLOBEX01")!.props.currency,
    ).toBe("GBP");
    expect(reviews(next).some((n) => n.props.reason === "prop_conflict")).toBe(
      false,
    );
  });

  it("lists an adjudicated review's choice only", async () => {
    Object.assign(config, { documentAdjudicated: true });
    await uploaded("variant.md", variant);
    const rv = reviews(await loaded("first"))[0]!;
    expect(rv.props).toMatchObject({
      reason: "adjudicated",
      entity_key: "cus_GLOBEX01",
      candidates: ["cus_GLOBEX01"],
    });
  });

  it("reads into a load the decisions saved when it was created, catch-up extracts included", async () => {
    await uploaded("contract.md", CONTRACT);
    await uploaded(
      "amendment.md",
      Buffer.from(
        "# Master services agreement\n\nGlobex Corporation signed. The Agreement runs for 36 months.\n",
      ),
    );
    const g = await loaded("first");
    const rv = reviews(g).find((n) => n.props.reason === "prop_conflict")!;
    expect(
      (
        await call("POST", `${O}/reviews/${rv.props.review_key}:settle`, {
          decision: { prop: { integer: "36" } },
        })
      ).status,
    ).toBe(200);
    // An upload reads into the current load, with the decisions it started with.
    await uploaded(
      "third.md",
      Buffer.from(
        "# Master services agreement\n\nGlobex Corporation signed. The Agreement runs for 48 months.\n",
      ),
    );
    vi.setSystemTime(Date.now() + 10_000);
    const docs = (await call("GET", `${O}/documents`)).body.documents;
    expect(
      docs.find((d: { name: string }) => d.name === "third.md").extraction
        .status,
    ).toBe("extracted");
    const contract = g.find(
      "Contract",
      "norm_name",
      "master services agreement",
    )!;
    expect(contract.props.term_months).toBeUndefined();
    expect(
      JSON.parse(String(rv.props.values)).map(
        (v: { value: number }) => v.value,
      ),
    ).toEqual([24, 36, 48]);
    // The next load applies it.
    expect(
      (await loaded("next")).find(
        "Contract",
        "norm_name",
        "master services agreement",
      )!.props.term_months,
    ).toBe(36);
  });
});

describe("a mock decision's value, as the backend keeps it", () => {
  it("is the engine's: UTC datetimes, partial dates, no coercion", () => {
    for (const [type, value, kept] of [
      ["datetime", "2026-10-19T20:00:00Z", "2026-10-19T20:00:00Z"],
      ["datetime", "2026-03-14T09:30:00.25+01:00", "2026-03-14T08:30:00.250Z"],
      ["datetime", "2026-03-14T08:30:00.0001Z", "2026-03-14T08:30:00.000100Z"],
      [
        "datetime",
        "2026-03-14T08:30:00.000000001Z",
        "2026-03-14T08:30:00.000000001Z",
      ],
      ["datetime", "0000-01-01T00:30:00+01:00", undefined],
      ["datetime", "9999-12-31T23:30:00-01:00", undefined],
      ["datetime", "2026-10-19", undefined],
      ["datetime", "2026-10-19t20:00:00z", undefined],
      ["date", "2024-02-29", "2024-02-29"],
      ["date", "2026-02-29", undefined],
      ["date", "+026", undefined],
      ["string", "\tsigned ", "signed"],
      ["integer", "3", undefined],
      ["boolean", "true", undefined],
      ["bytes", "a", undefined],
    ] as const)
      expect(decisionValue(type, { text: value }), `${type} ${value}`).toEqual(
        kept === undefined ? undefined : { text: kept },
      );
  });
  it("keeps an integer's every digit, and reads a whole float back as an integer", () => {
    for (const [type, value, kept] of [
      ["integer", { integer: "3" }, { integer: "3" }],
      ["integer", { integer: "-0" }, { integer: "0" }],
      [
        "integer",
        { integer: "9223372036854775807" },
        { integer: "9223372036854775807" },
      ],
      ["integer", { integer: "9223372036854775808" }, undefined],
      ["integer", { number: 3 }, { integer: "3" }],
      ["integer", { number: 1.5 }, undefined],
      ["integer", { number: 1e21 }, undefined],
      ["float", { number: 1.5 }, { number: 1.5 }],
      ["float", { number: 3 }, { integer: "3" }],
      ["float", { integer: "3" }, { integer: "3" }],
      ["float", { number: 1e20 }, { number: 1e20 }],
      ["string", { integer: "3" }, undefined],
      ["boolean", { boolean: false }, { boolean: false }],
      ["integer", { boolean: true }, undefined],
      ["string", { null: null }, { null: null }],
    ] satisfies [string, FoundryPropValue, FoundryPropValue | undefined][])
      expect(
        decisionValue(type, value),
        `${type} ${JSON.stringify(value)}`,
      ).toEqual(kept);
  });
});

describe("decisions a version made current keeps, as the backend's Extractable", () => {
  const decided = {
    decidedByUserId: "usr_ada",
    decidedAt: "2026-10-20T10:00:00Z",
  };
  const resolve = (id: string, type: string, entityKey: string) => ({
    ...decided,
    id,
    kind: "resolve" as const,
    type,
    docId: "doc",
    normName: "globexx corporation",
    entityKey,
  });
  it("keeps a resolve whose entity key the type's key no longer reads, so its name is reviewed again", () => {
    decisions.set("o", [
      // Settled when Customer was keyed; now it is named.
      resolve("d1", "Customer", "cus_GLOBEX01"),
      resolve("d2", "Customer", "globex corporation"),
      resolve("d3", "Supplier", "acme"),
      {
        ...decided,
        id: "d4",
        kind: "prop",
        type: "Customer",
        entityKey: "globex corporation",
        prop: "currency",
        value: { text: "GBP" },
      },
      {
        ...decided,
        id: "d5",
        kind: "prop",
        type: "Customer",
        entityKey: "globex corporation",
        prop: "tier",
        value: { integer: "2" },
      },
    ]);
    pruneDecisions("o", {
      format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
      definition: {
        ontology_toml:
          '[types.Customer]\nprops = { name = "string", currency = "string" }',
      },
    });
    expect(decisions.get("o")!.map((d) => d.id)).toEqual(["d1", "d2", "d4"]);
  });
});
