import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as toml from "smol-toml";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type * as wfe from "@/shared/wfe";
import { FOUNDRY_NODES } from "@/shared/foundry-labels";
import {
  editItem,
  stageEnrichment,
  enrichmentDefaults,
  enrichmentForm,
} from "@/web/areas/ontology/edit-model";
import {
  confidenceText,
  readAgentBundle,
  readEnrichmentClues,
} from "@/web/areas/ontology/model";
import { PEOPLE, salesGraphBundle } from "../../mock/fixtures/sales-graph";
import {
  applyProfiles,
  type EnrichmentBinding,
  type Experience,
  type ProfileRow,
} from "../../mock/graph/extract";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { setFoundryPlan } from "../../mock/routes/billing";
import { tenantGraph } from "../../mock/routes/databases";
import {
  fakeEnrich,
  linkedinSlug,
  measureClues,
  planLookups,
} from "../../mock/routes/enrichment";
import { draftFromTables, sourceId } from "../../mock/routes/ontology-agent";
import { comparableSchema } from "../../mock/routes/ontology-revise";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's People enrichment:
// the owner's setting and credits, the planner and fakeEnrich, a load
// of the Sales graph parking while it looks people up and applying the
// profiles with its tables, carry-forward, every way the step stops, the
// purge when consent is turned off, the agent's proposal and Revise.

const compiled = routes.map((r) => ({
  ...r,
  c: compileTemplate(r.path),
}));
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

const WS = "/v1/workspaces/ws_acme";
const SETTINGS = `${WS}/foundry/enrichment`;
const O = `${WS}/foundry/ontologies/ont_acme_sales`;
const people = PEOPLE.map((r) => ({
  key: r.contact_id,
  name: r.full_name || "",
  email: r.email,
  linkedin: r.linkedin_url,
}));

const allow = (cap = 50_000) =>
  call("PATCH", SETTINGS, {
    enabled: true,
    monthlyCreditCap: String(cap),
    noticeVersion: "2026-10-20",
  });
const settings = async () =>
  (await call("GET", SETTINGS)).body.enrichment as wfe.FoundryEnrichment;

/** Loads `version` of the Sales graph and runs the clock until it settles. */
async function load(version = "ver_sales_2", token = "a", ms = 60_000) {
  const made = await call("POST", `${O}/loads`, {
    versionId: version,
    clientToken: token,
  });
  expect(made.status, JSON.stringify(made.body)).toBe(200);
  return settle(made.body.load.id, ms);
}
async function settle(id: string, ms = 60_000) {
  for (let t = 0; t < ms; t += 50) {
    vi.setSystemTime(Date.now() + 50);
    const l = (await call("GET", `${O}/loads/${id}`)).body.load;
    if (l.status !== "writing") return l as wfe.FoundryLoad;
  }
  return (await call("GET", `${O}/loads/${id}`)).body.load as wfe.FoundryLoad;
}
/** Runs the clock until the load's enrichment step is `status`. */
async function until(id: string, status: string) {
  for (let t = 0; t < 10_000; t += 10) {
    const l = (await call("GET", `${O}/loads/${id}`)).body.load;
    if (l.enrichment?.status === status) return l as wfe.FoundryLoad;
    vi.setSystemTime(Date.now() + 10);
  }
  throw new Error(`never ${status}`);
}
/** The snapshot the engine gives a staged profiles file: its fingerprint. */
const fingerprint = (profiles: ProfileRow[]) =>
  `sha256:${createHash("sha256")
    .update(profiles.map((p) => JSON.stringify(p)).join("\n"))
    .digest("hex")}`;
/** The People enrichment chunk the graph holds for a profiles file. */
const enrichmentChunk = (
  g: { nodes(): Iterable<{ label: string; props: Record<string, unknown> }> },
  profiles: ProfileRow[],
) =>
  [...g.nodes()].find(
    (n) =>
      n.label === FOUNDRY_NODES.Chunk &&
      n.props.source === "foundry_enrichment" &&
      n.props.snapshot === fingerprint(profiles),
  );
const person = (key: string) =>
  [...tenantGraph("ten_acme_sales").nodes.values()].find(
    (n) => n.label === "Person" && n.props.entity_key === key,
  );

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-20T12:00:00Z"));
  resetState();
  Object.assign(config, {
    loadStepMs: 10,
    enrichmentStepMs: 10,
    ontologyStepMs: 10,
  });
});
afterEach(() => vi.useRealTimers());

describe("the Sales graph fixture", () => {
  // As the engine renders a draft: every binding says what a missing
  // entity does, a link's cardinality is an item of its own, and an item
  // serves a key, a join or a CQ by its item id.
  it.each([
    ["version 1", undefined],
    ["version 2", measureClues(people)],
  ])("is a draft the engine's grammar reads (%s)", (_, clues) => {
    const def = salesGraphBundle(sourceId("conn_acme_shop", "people"), clues);
    const mappings = toml.parse(def.mappings_toml!) as Record<string, any>;
    const bindings = Object.values(mappings.mappings).flatMap((m: any) =>
      Object.values(m.bind),
    ) as Record<string, unknown>[];
    expect(bindings.map((b) => b.missing)).toEqual(["create", "create"]);
    const items = Object.entries(def.draft!.draft!.items!);
    for (const [id, item] of items) {
      for (const serves of item.serves || [])
        expect(serves, id).toMatch(/^(key|join|cq\..+)$/);
      if (/^link\.[^.]+$/.test(id))
        for (const link of Object.values(
          toml.parse(item.toml).links as Record<string, object>,
        ))
          expect(Object.keys(link), id).not.toContain("cardinality");
    }
    expect(Object.fromEntries(items)["link.works_at.cardinality"]?.toml).toBe(
      '[links.works_at]\ncardinality = "one"\n\n',
    );
  });
});

describe("the workspace's setting", () => {
  it("starts off, available on Pro, and needs Pro on Free", async () => {
    expect(await settings()).toEqual({
      availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE",
      enabled: false,
      monthlyCreditCap: "0",
      creditsUsed: "0",
      creditsReserved: "0",
      capResetsAt: "2026-11-01T00:00:00.000Z",
      enabledBy: "",
      enabledAt: null,
      purgesPending: 0,
    });
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    expect((await settings()).availability).toBe(
      "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
    );
  });

  it("lets the owner allow it with a cap, and says who did and when", async () => {
    expect((await allow(20_000)).status).toBe(200);
    expect(await settings()).toMatchObject({
      enabled: true,
      monthlyCreditCap: "20000",
      enabledBy: "usr_ada",
      enabledAt: "2026-10-20T12:00:00.000Z",
    });
    // A new cap keeps who allowed it.
    vi.setSystemTime(Date.now() + 60_000);
    await allow(30_000);
    expect(await settings()).toMatchObject({
      monthlyCreditCap: "30000",
      enabledAt: "2026-10-20T12:00:00.000Z",
    });
    await call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "30000",
      noticeVersion: "2026-10-20",
    });
    expect(await settings()).toMatchObject({
      enabled: false,
      enabledBy: "",
      enabledAt: null,
    });
  });

  it("is the owner's alone to change, and readable by members", async () => {
    const alan = "usr_alan";
    expect((await call("GET", SETTINGS, undefined, alan)).status).toBe(200);
    expect(
      (
        await call(
          "PATCH",
          SETTINGS,
          { enabled: true, monthlyCreditCap: "1", noticeVersion: "n" },
          alan,
        )
      ).status,
    ).toBe(403);
    expect(
      (await call("GET", "/v1/workspaces/ws_none/foundry/enrichment")).status,
    ).toBe(403);
  });

  it.each([
    [
      { monthlyCreditCap: "1" },
      "notice_version is required, at most 32 characters",
    ],
    [
      { enabled: false, monthlyCreditCap: "0", noticeVersion: "" },
      "notice_version is required, at most 32 characters",
    ],
    [
      { enabled: true, monthlyCreditCap: "10", noticeVersion: "n".repeat(33) },
      "notice_version is required, at most 32 characters",
    ],
    [
      { enabled: true, monthlyCreditCap: "100001", noticeVersion: "n" },
      "monthly_credit_cap must be 0 to 100000",
    ],
    [
      { enabled: true, monthlyCreditCap: "-1", noticeVersion: "n" },
      "monthly_credit_cap must be 0 to 100000",
    ],
    [
      { enabled: true, monthlyCreditCap: "1.5", noticeVersion: "n" },
      "monthly_credit_cap must be 0 to 100000",
    ],
  ])("refuses %j", async (body, message) => {
    const r = await call("PATCH", SETTINGS, body);
    expect([r.status, r.body.message]).toEqual([400, message]);
    expect((await settings()).enabled).toBe(false);
  });

  it("needs Foundry Pro to turn it on, never to turn it off", async () => {
    await allow();
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    const on = await allow(10_000);
    expect([on.status, on.body.message, on.body.details]).toEqual([
      400,
      "People enrichment needs Foundry Pro",
      [
        expect.objectContaining({
          reason: "FOUNDRY_PRO_REQUIRED",
          metadata: { workspace_id: "ws_acme" },
        }),
      ],
    ]);
    expect((await settings()).monthlyCreditCap).toBe("50000");
    const off = await call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "50000",
      noticeVersion: "2026-10-20",
    });
    expect(off.status).toBe(200);
    expect(off.body.enrichment).toMatchObject({
      availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
      enabled: false,
    });
  });
});

// The vendored swagger's definitions, to hold the mock's answers to.
type SwaggerSchema = {
  type?: string;
  format?: string;
  enum?: string[];
  $ref?: string;
  properties?: Record<string, SwaggerSchema>;
  items?: SwaggerSchema;
};
const definitions = (
  JSON.parse(
    readFileSync(
      new URL("../../contract/foundry.swagger.json", import.meta.url),
      "utf8",
    ),
  ) as { definitions: Record<string, SwaggerSchema> }
).definitions;
/**
 * Where `value` differs from what protojson writes for `name` (the WFE's
 * gateway emits unpopulated fields): every field present but the proto3
 * `optional` ones, int64 as digit strings, enums by name, an unset message
 * or timestamp as null, and nothing the definition doesn't name.
 */
function wireProblems(
  value: unknown,
  name: string,
  optional: readonly string[] = [],
): string[] {
  const at = (schema: SwaggerSchema, v: unknown, path: string): string[] => {
    if (schema.$ref) {
      const ref = schema.$ref.replace("#/definitions/", "");
      return definitions[ref]!.type === "object" && v === null
        ? []
        : at(definitions[ref]!, v, path);
    }
    const bad = (what: string) => [`${path}: ${JSON.stringify(v)} is ${what}`];
    switch (schema.type) {
      case "object": {
        if (typeof v !== "object" || v === null || Array.isArray(v))
          return bad("not an object");
        const props = schema.properties || {};
        return [
          ...Object.keys(v)
            .filter((k) => !(k in props))
            .map((k) => `${path}.${k}: not in the definition`),
          ...Object.entries(props).flatMap(([k, s]) =>
            !(k in v)
              ? optional.includes(`${path}.${k}`)
                ? []
                : [`${path}.${k}: missing`]
              : at(s, (v as Record<string, unknown>)[k], `${path}.${k}`),
          ),
        ];
      }
      case "string":
        if (schema.format === "date-time")
          return v === null ||
            (typeof v === "string" && !Number.isNaN(Date.parse(v)))
            ? []
            : bad("not a time or null");
        if (schema.format === "int64")
          return typeof v === "string" && /^-?\d+$/.test(v)
            ? []
            : bad("not an int64 string");
        if (schema.enum)
          return schema.enum.includes(v as string) ? [] : bad("not an enum");
        return typeof v === "string" ? [] : bad("not a string");
      case "integer":
        return Number.isInteger(v) ? [] : bad("not an integer");
      case "boolean":
        return typeof v === "boolean" ? [] : bad("not a boolean");
      default:
        return bad(`of a type the check doesn't know (${schema.type})`);
    }
  };
  return at(definitions[name]!, value, name);
}

describe("the WFE's shapes", () => {
  const STEP = "v1FoundryLoadEnrichment";
  // proto3 optional: set on a partial step only.
  const stoppedBy = [`${STEP}.stoppedBy`];

  it("answers the setting as v1FoundryEnrichment, off and on", async () => {
    expect(wireProblems(await settings(), "v1FoundryEnrichment")).toEqual([]);
    const on = await allow();
    expect(wireProblems(on.body.enrichment, "v1FoundryEnrichment")).toEqual([]);
    expect(
      wireProblems(
        (await call("GET", SETTINGS, undefined, "usr_alan")).body.enrichment,
        "v1FoundryEnrichment",
      ),
    ).toEqual([]);
  });

  it("answers a load's step as v1FoundryLoadEnrichment in every state", async () => {
    await allow(60);
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    for (const status of ["queued", "running"]) {
      const e = (await until(id, status)).enrichment;
      expect(e).not.toHaveProperty("stoppedBy");
      expect(wireProblems(e, STEP, stoppedBy)).toEqual([]);
    }
    // The credit cap: partial, with why.
    const capped = (await settle(id)).enrichment;
    expect(capped).toMatchObject({ status: "partial", stoppedBy: "cap" });
    expect(wireProblems(capped, STEP)).toEqual([]);
    const done = (await load("ver_sales_1", "b")).enrichment;
    expect(done).toBeNull();
    await call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "60",
      noticeVersion: "2026-10-20",
    });
    const off = (await load("ver_sales_2", "c")).enrichment;
    expect(off).toMatchObject({ status: "off" });
    expect(wireProblems(off, STEP, stoppedBy)).toEqual([]);
  });

  it("answers Stop with the load, as helixwfev1FoundryLoadResponse holds it", async () => {
    await allow();
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(id, "queued");
    const stopped = await call("POST", `${O}/loads/${id}:stop-enrichment`, {});
    expect(stopped.status).toBe(200);
    expect(Object.keys(stopped.body)).toEqual(["load"]);
    expect(stopped.body.load.enrichment.stoppedBy).toBe("stopped");
    expect(wireProblems(stopped.body.load.enrichment, STEP)).toEqual([]);
  });

  it("knows what protojson writes", () => {
    expect(
      wireProblems(
        { status: "done", people: 3, extra: true, publishedAt: "never" },
        STEP,
      ),
    ).toEqual(
      expect.arrayContaining([
        "v1FoundryLoadEnrichment.extra: not in the definition",
        "v1FoundryLoadEnrichment.stoppedBy: missing",
        "v1FoundryLoadEnrichment.people: 3 is not an int64 string",
        'v1FoundryLoadEnrichment.publishedAt: "never" is not a time or null',
        "v1FoundryLoadEnrichment.noName: missing",
      ]),
    );
    expect(
      wireProblems(
        { availability: "AVAILABLE", purgesPending: "1", enabled: 1 },
        "v1FoundryEnrichment",
      ),
    ).toEqual(
      expect.arrayContaining([
        'v1FoundryEnrichment.availability: "AVAILABLE" is not an enum',
        'v1FoundryEnrichment.purgesPending: "1" is not an integer',
        "v1FoundryEnrichment.enabled: 1 is not a boolean",
      ]),
    );
  });
});

describe("the planner and fakeEnrich", () => {
  it("canonicalises LinkedIn URLs to a profile's slug", () => {
    expect(linkedinSlug("https://www.linkedin.com/in/alan-turing/")).toBe(
      "alan-turing",
    );
    expect(
      linkedinSlug(
        "https://uk.linkedin.com/in/Ada%2DLovelace/details/experience?x=1#top",
      ),
    ).toBe("ada-lovelace");
    expect(linkedinSlug("m.linkedin.com/in/grace")).toBe("grace");
    for (const bad of [
      "https://www.linkedin.com/pub/lin-kedin/1/2/3",
      "https://www.linkedin.com/sales/people/x",
      "https://www.linkedin.com/in/ACoAAB12",
      "https://example.com/in/x",
      "%E0%A4%A",
    ])
      expect(linkedinSlug(bad), bad).toBeUndefined();
  });

  it("counts who can't be looked up, and plans the rest", () => {
    const plan = planLookups(people, []);
    expect(plan.counts).toEqual({
      people: 14,
      // Cher.
      noName: 1,
      // Nobody Known, and those the filters left without one: free mail, a
      // role mailbox and a URL that isn't a profile's.
      noClue: 4,
      shared: 0,
      skipped: 5,
    });
    expect(plan.carried).toEqual([]);
    expect(plan.calls.map((l) => l.key)).toEqual([
      "c_17",
      "c_18",
      "c_19",
      "c_20",
      "c_21",
      "c_22",
      "c_23",
      "c_24",
      "c_25",
    ]);
    expect(plan.calls[2]).toEqual({
      key: "c_19",
      name: "Alan Turing",
      email: undefined,
      slug: "alan-turing",
      // What the profile row binds by: the entity's own value.
      clues: {
        email: undefined,
        linkedin: "https://www.linkedin.com/in/alan-turing/",
      },
    });
  });

  it("drops a value two people share for both, and a shared slug", () => {
    const plan = planLookups(
      [
        { key: "a", name: "Pat One", email: "shared@acme.io" },
        { key: "b", name: "Sam Two", email: "Shared@acme.io" },
        {
          key: "c",
          name: "Lee Three",
          email: "lee@acme.io",
          linkedin: "linkedin.com/in/twin",
        },
        {
          key: "d",
          name: "Kim Four",
          linkedin: "https://linkedin.com/in/twin/",
        },
        {
          key: "e",
          name: "Max Five",
          email: "max@gmail.com",
          linkedin: "linkedin.com/in/max",
        },
      ],
      [],
    );
    // Pat and Sam lose their address, Lee and Kim their slug; Pat, Sam and
    // Kim have nothing left.
    expect(plan.counts).toMatchObject({ noClue: 3, shared: 4, skipped: 3 });
    // Lee keeps the email; Max keeps LinkedIn without the free mail.
    expect(plan.calls).toEqual([
      {
        key: "c",
        name: "Lee Three",
        email: "lee@acme.io",
        slug: undefined,
        clues: { email: "lee@acme.io", linkedin: "linkedin.com/in/twin" },
      },
      {
        key: "e",
        name: "Max Five",
        email: undefined,
        slug: "max",
        clues: { email: "max@gmail.com", linkedin: "linkedin.com/in/max" },
      },
    ]);
  });

  it("decides by the clue's text", () => {
    const outcome = (email: string, name = "Pat Person") => {
      const { outcome, reason, credits } = fakeEnrich({
        key: "k",
        name,
        email,
        clues: { email },
      });
      return { outcome, reason, credits };
    };
    expect(outcome("bea@badgerworks.io")).toEqual({
      outcome: "not_found",
      reason: undefined,
      credits: 0,
    });
    expect(outcome("rob.lowell@acme.io", "Rob Lowell")).toEqual({
      outcome: "held",
      reason: "low_score",
      credits: 20,
    });
    expect(outcome("otto@another.io", "Otto Mann")).toEqual({
      outcome: "held",
      reason: "name_mismatch",
      credits: 20,
    });
    expect(outcome("timeout@acme.io")).toEqual({
      outcome: "uncertain",
      reason: undefined,
      credits: 20,
    });
    expect(outcome("dana@dupont.io", "Dana Dupont")).toMatchObject({
      outcome: "accepted",
    });
    const ada = fakeEnrich({
      key: "c_17",
      name: "Ada Lovelace",
      email: "ada@acme.io",
      clues: { email: "ada@acme.io" },
    });
    expect(ada).toMatchObject({
      outcome: "accepted",
      credits: 20,
      profile: {
        profile_id: "li:ada-lovelace",
        clue_email: "ada@acme.io",
        clue_name: "Ada Lovelace",
        fields: {
          current_title: "VP Engineering",
          location_country: "United Kingdom",
        },
      },
    });
    expect(ada.profile!.experience.map((x) => x.organization.domain)).toEqual([
      "acme.io",
      "initech.com",
    ]);
  });

  it("measures an item's clues for the agent as the engine's Clues counts them", () => {
    // Free mail, a role mailbox and a URL that isn't a profile's count: the
    // planner's filters don't apply, and Cher's email counts without a
    // full name.
    expect(measureClues(people)).toEqual({
      people: 14,
      usable: 12,
      email: 11,
      linkedin_url: 2,
      no_name: 1,
      shared: 0,
      conflicting: 0,
    });
    // So a load looks up fewer.
    expect(planLookups(people, []).calls).toHaveLength(9);
    // An address two people share counts for neither.
    expect(
      measureClues([
        ...people,
        { key: "c_31", name: "Ada Twin", email: "ADA@acme.io" },
      ]),
    ).toEqual({
      people: 15,
      usable: 11,
      email: 10,
      linkedin_url: 2,
      no_name: 1,
      shared: 2,
      conflicting: 0,
    });
  });
});

describe("a load that enriches", () => {
  it("parks while it looks people up, then loads the profiles with its tables", async () => {
    await allow();
    const made = await call("POST", `${O}/loads`, {
      versionId: "ver_sales_2",
      clientToken: "a",
    });
    const id = made.body.load.id;
    const queued = await until(id, "queued");
    expect(queued.jobs![0]).toMatchObject({
      status: "queued",
      phase: "provisioning",
    });
    expect(queued.enrichment).toMatchObject({
      people: "14",
      noName: "1",
      noClue: "4",
      skipped: "5",
      toLookUp: "9",
      lookedUp: "0",
    });
    expect(queued.enrichment).not.toHaveProperty("stoppedBy");
    // A new load waits for this one.
    expect(
      (
        await call("POST", `${O}/loads`, {
          versionId: "ver_sales_2",
          clientToken: "b",
        })
      ).status,
    ).toBe(400);
    const running = await until(id, "running");
    expect(Number((await settings()).creditsReserved)).toBeGreaterThan(0);
    expect(Number(running.enrichment!.lookedUp)).toBeLessThan(9);
    const l = await settle(id);
    expect(l).toMatchObject({ status: "ready", current: true });
    // As protojson writes v1FoundryLoadEnrichment: int64 strings, and no
    // stoppedBy unless partial.
    expect(l.enrichment).toEqual({
      status: "done",
      people: "14",
      noName: "1",
      noClue: "4",
      shared: "0",
      skipped: "5",
      carried: "0",
      toLookUp: "9",
      lookedUp: "9",
      // Ada, Grace, Alan and Katherine; Rob scores low, Bea isn't found,
      // Dana and Dan share a profile, Otto's is someone else's.
      found: "4",
      held: "4",
      notFound: "1",
      uncertain: "0",
      credits: "160",
      applied: "4",
      unmatched: "0",
      publishedAt: expect.any(String),
    });
    expect(await settings()).toMatchObject({
      creditsUsed: "160",
      creditsReserved: "0",
    });
    // Ada's profile fills her row's entity, beside the table's values.
    expect(person("c_17")!.props).toMatchObject({
      email: "ada@acme.io",
      current_title: "VP Engineering",
      location: "London, United Kingdom",
      country: "United Kingdom",
      profile_url: "https://www.linkedin.com/in/ada-lovelace",
    });
    const g = tenantGraph("ten_acme_sales");
    const evidence = [...g.nodes.values()].find(
      (n) =>
        n.label === FOUNDRY_NODES.Evidence &&
        n.props.source === "foundry_enrichment" &&
        n.props.row_key === "li:ada-lovelace",
    );
    expect(evidence!.props).toMatchObject({
      confidence: 0.7,
      props: ["country", "current_title", "location", "profile_url"],
    });
    // As the engine reads the staged file pinned: its snapshot is the
    // file's fingerprint, not a date.
    expect(evidence!.props.snapshot).toMatch(/^sha256:[0-9a-f]{64}$/);
    // Her work history links the employers the graph has, at 0.7.
    const worked = [...g.edges.values()].filter(
      (e) => e.label === "worked_at" && e.from === person("c_17")!.id,
    );
    expect(
      worked.map((e) => [g.nodes.get(e.to)!.props.entity_key, e.props]),
    ).toEqual([
      [
        "acme.io",
        expect.objectContaining({
          title: "VP Engineering",
          is_current: true,
          confidence: 0.7,
          provenance: "stated",
        }),
      ],
      [
        "initech.com",
        expect.objectContaining({ title: "Engineer", is_current: false }),
      ],
    ]);
    // Held and unfound people get nothing.
    expect(person("c_21")!.props).not.toHaveProperty("current_title");
  });

  it("carries what it found to the next load, which looks nobody up and doesn't park", async () => {
    await allow();
    await load();
    const again = await call("POST", `${O}/loads`, {
      versionId: "ver_sales_2",
      clientToken: "b",
    });
    const id = again.body.load.id;
    for (let t = 0; t < 200; t += 10) {
      vi.setSystemTime(Date.now() + 10);
      const l = (await call("GET", `${O}/loads/${id}`)).body.load;
      expect(l.jobs[0].status === "queued" && l.jobs[0].startedAt).toBeFalsy();
      if (l.status !== "writing") break;
    }
    const l = await settle(id);
    // Everyone looked up before is carried: found, held and not found.
    expect(l.enrichment).toMatchObject({
      status: "done",
      toLookUp: "0",
      lookedUp: "0",
      carried: "9",
      found: "0",
      credits: "0",
      applied: "4",
    });
    expect((await settings()).creditsUsed).toBe("160");
  });

  it("applies no profiles without consent", async () => {
    const l = await load();
    expect(l.enrichment).toMatchObject({
      status: "off",
      people: "0",
      applied: "0",
    });
    expect(person("c_17")!.props).not.toHaveProperty("current_title");
  });

  it("runs as before for a version without the item", async () => {
    await allow();
    const l = await load("ver_sales_1");
    expect(l.status).toBe("ready");
    expect(l.enrichment).toBeNull();
    expect(person("c_17")!.props.full_name).toBe("Ada Lovelace");
  });

  it("looks nobody up off Pro, and applies what was found before", async () => {
    await allow();
    expect((await load()).enrichment).toMatchObject({
      status: "done",
      applied: "4",
    });
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    // Nobody is due a lookup: done, as on Pro.
    expect((await load("ver_sales_2", "b")).enrichment).toMatchObject({
      status: "done",
      toLookUp: "0",
      carried: "9",
      applied: "4",
    });
    // Bea, who wasn't found, is due one again after the cache days: off
    // Pro, nobody is looked up, and the profiles found before still apply.
    vi.setSystemTime(Date.now() + 91 * 86_400_000);
    const l = await load("ver_sales_2", "c");
    expect(l).toMatchObject({ status: "ready", current: true });
    expect(l.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "pro",
      carried: "8",
      toLookUp: "1",
      lookedUp: "0",
      credits: "0",
      applied: "4",
    });
    // With nothing found before, nothing applies.
    resetState();
    Object.assign(config, { loadStepMs: 10, enrichmentStepMs: 10 });
    await allow();
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    expect((await load()).enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "pro",
      toLookUp: "9",
      lookedUp: "0",
      applied: "0",
    });
  });

  it("stops at the credit cap, and with no credits doesn't park", async () => {
    await allow(60);
    const capped = await load();
    expect(capped).toMatchObject({ status: "ready", current: true });
    expect(capped.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "cap",
      toLookUp: "9",
      lookedUp: "3",
      credits: "60",
    });
    // The month is spent: the next load applies what was found.
    const spent = await load("ver_sales_2", "b");
    expect(spent.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "cap",
      lookedUp: "0",
      credits: "0",
    });
  });

  it.each(["cap", "provider_credits", "provider", "time"] as const)(
    "ends partial(%s) after a batch, still ready and current",
    async (outcome) => {
      await allow();
      Object.assign(config, { enrichmentOutcome: outcome });
      const l = await load();
      expect(l).toMatchObject({ status: "ready", current: true });
      expect(l.enrichment).toMatchObject({
        status: "partial",
        stoppedBy: outcome,
        lookedUp: "2",
        toLookUp: "9",
      });
    },
  );

  it("ends partial(busy) when no slot frees up, applying what was found before", async () => {
    await allow();
    await load();
    Object.assign(config, { enrichmentOutcome: "busy" });
    // Bea, who wasn't found, is looked up again after the cache days.
    vi.setSystemTime(Date.now() + 91 * 86_400_000);
    const l = await load("ver_sales_2", "b");
    expect(l).toMatchObject({ status: "ready", current: true });
    expect(l.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "busy",
      carried: "8",
      toLookUp: "1",
      lookedUp: "0",
      applied: "4",
    });
  });

  it("stops while queued at once, and while running after its batch", async () => {
    await allow();
    const first = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(first, "queued");
    const stopped = await call(
      "POST",
      `${O}/loads/${first}:stop-enrichment`,
      {},
    );
    expect(stopped.status).toBe(200);
    expect(stopped.body.load.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "stopped",
      lookedUp: "0",
    });
    // Woken: queued at its first step, started, until it is picked up again.
    const startedAt = stopped.body.load.jobs[0].startedAt;
    expect(stopped.body.load.jobs[0]).toMatchObject({
      status: "queued",
      phase: "provisioning",
      startedAt: expect.any(String),
    });
    const picked = await until(first, "partial");
    expect(picked.jobs![0].status).toBe("queued");
    vi.setSystemTime(Date.now() + 10);
    const again = (await call("GET", `${O}/loads/${first}`)).body.load;
    expect(again.jobs[0]).toMatchObject({
      status: "running",
      phase: "provisioning",
      startedAt,
    });
    expect((await settle(first)).status).toBe("ready");
    const second = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "b",
      })
    ).body.load.id;
    await until(second, "running");
    vi.setSystemTime(Date.now() + 10);
    await call("POST", `${O}/loads/${second}:stop-enrichment`, {});
    const l = await settle(second);
    expect(l.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "stopped",
    });
    expect(Number(l.enrichment!.lookedUp)).toBeGreaterThan(0);
    expect(Number(l.enrichment!.lookedUp)).toBeLessThan(9);
  });

  it("leaves an ended step as it is, and refuses a load with no step and members", async () => {
    await allow();
    const l = await load();
    const stop = (id: string, user?: string) =>
      call("POST", `${O}/loads/${id}:stop-enrichment`, {}, user);
    const again = await stop(l.id);
    expect(again.status).toBe(200);
    expect(again.body.load.enrichment).toEqual(l.enrichment);
    const plain = await load("ver_sales_1", "b");
    for (const id of [plain.id, "load_none"])
      expect([(await stop(id)).status, (await stop(id)).body.message]).toEqual([
        404,
        "this load looks no one up",
      ]);
    expect((await stop(l.id, "usr_alan")).status).toBe(403);
  });

  const off = () =>
    call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "50000",
      noticeVersion: "2026-10-20",
    });
  const loadOf = async (id: string) =>
    (await call("GET", `${O}/loads/${id}`)).body.load as
      | wfe.FoundryLoad
      | undefined;

  it("purges when consent is turned off: a clean load, and old ones can't be current", async () => {
    await allow();
    const enriched = await load();
    expect(enriched.enrichment!.applied).toBe("4");
    await off();
    expect((await settings()).purgesPending).toBe(1);
    // While the clean load runs, the purged one can't be made current again.
    const back = await call(
      "POST",
      `${O}/loads/${enriched.id}:set-current`,
      {},
    );
    expect([back.status, back.body.message]).toEqual([
      400,
      "this load holds People enrichment profiles the workspace no longer allows; load the version again",
    ]);
    const loads = (await call("GET", `${O}/loads`)).body.loads;
    const clean = await settle(loads[0].id);
    expect(clean.enrichment).toMatchObject({ status: "off", applied: "0" });
    expect(clean.current).toBe(true);
    expect((await settings()).purgesPending).toBe(0);
    // Then it is deleted.
    expect((await loadOf(enriched.id))?.status).toBe("deleting");
    // Results were deleted too: allowing it again looks everyone up.
    await allow();
    expect((await load("ver_sales_2", "c")).enrichment).toMatchObject({
      carried: "0",
      toLookUp: "9",
    });
  });

  it("purges once the live job ends when consent is turned off meanwhile", async () => {
    await allow();
    const enriched = await load();
    expect(enriched.enrichment!.applied).toBe("4");
    // Bea, who wasn't found, is looked up again after the cache days.
    vi.setSystemTime(Date.now() + 91 * 86_400_000);
    const live = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "b",
      })
    ).body.load.id;
    await until(live, "running");
    await off();
    expect((await settings()).purgesPending).toBe(1);
    // Nothing more while the job is live.
    expect(
      (await call("GET", `${O}/loads`)).body.loads.map(
        (l: wfe.FoundryLoad) => l.id,
      ),
    ).toEqual([live, enriched.id]);
    // The live load published nothing, so once current it is the clean
    // load: no other is needed, and the enriched one is deleted.
    const done = await settle(live);
    expect(done).toMatchObject({ status: "ready", current: true });
    expect(done.enrichment).toMatchObject({
      status: "cancelled",
      applied: "0",
    });
    expect((await settings()).purgesPending).toBe(0);
    expect(
      (await call("GET", `${O}/loads`)).body.loads.map((l: wfe.FoundryLoad) => [
        l.id,
        l.status,
      ]),
    ).toEqual([
      [live, "ready"],
      [enriched.id, "deleting"],
    ]);
  });

  it("purges a load still writing the profiles it found when consent is turned off", async () => {
    await allow();
    // The Sales graph's first load: nothing is current yet.
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    let l = await loadOf(id);
    while (!(l?.status === "writing" && l.enrichment?.applied === "4")) {
      vi.setSystemTime(Date.now() + 1);
      l = await loadOf(id);
      expect(l?.status).toBe("writing");
    }
    expect(l.enrichment).toMatchObject({ status: "done", applied: "4" });
    await off();
    expect((await settings()).purgesPending).toBe(1);
    // It ends current, as the WFE ends it, and is then loaded again
    // without its profiles.
    while (l?.status === "writing") {
      vi.setSystemTime(Date.now() + 1);
      l = await loadOf(id);
    }
    expect(l).toMatchObject({ status: "ready", current: true });
    expect((await settings()).purgesPending).toBe(1);
    const [clean] = (await call("GET", `${O}/loads`)).body.loads;
    expect(clean.id).not.toBe(id);
    const done = await settle(clean.id);
    expect(done).toMatchObject({ status: "ready", current: true });
    expect(done.enrichment).toMatchObject({ status: "off", applied: "0" });
    expect((await settings()).purgesPending).toBe(0);
    expect((await loadOf(id))?.status).toBe("deleting");
  });

  it("deletes an older load's profiles without reloading a clean current one", async () => {
    await allow();
    const enriched = await load();
    const plain = await load("ver_sales_1", "b");
    expect(plain.current).toBe(true);
    expect(plain.enrichment).toBeFalsy();
    await off();
    // No clean load is needed: the current one holds no profiles.
    expect((await settings()).purgesPending).toBe(0);
    expect(
      (await call("GET", `${O}/loads`)).body.loads.map((x: wfe.FoundryLoad) => [
        x.id,
        x.status,
      ]),
    ).toEqual([
      [plain.id, "ready"],
      [enriched.id, "deleting"],
    ]);
  });

  it("doesn't purge an ontology none of whose loads applied profiles", async () => {
    await allow();
    await load("ver_sales_1");
    await off();
    expect((await settings()).purgesPending).toBe(0);
    expect((await call("GET", `${O}/loads`)).body.loads).toHaveLength(1);
  });

  it("checks Pro again when it claims a queued step", async () => {
    await allow();
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(id, "queued");
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    const l = await settle(id);
    expect(l).toMatchObject({ status: "ready", current: true });
    expect(l.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "pro",
      toLookUp: "9",
      lookedUp: "0",
      credits: "0",
    });
  });

  it("applies a profile only to the person it was found for, and counts the rest unmatched", async () => {
    await allow();
    const l = await load();
    const g = tenantGraph("ten_acme_sales").view(l.id);
    const role = (id: number, name: string, domain: string): Experience => ({
      id,
      title: "Advisor",
      organization: { name, domain },
      location: "",
      start_date: "2020",
      end_date: null,
      is_current: false,
    });
    const item: EnrichmentBinding = {
      type: "Person",
      names: ["full_name"],
      clues: { email: "email", linkedinUrl: "linkedin_url" },
      props: [["current_title", "current_title"]],
      workHistory: {
        link: "worked_at",
        to: "Company",
        match: "domain",
        key: true,
        props: [
          ["title", "title"],
          ["employer", "/organization/name"],
        ],
      },
    };
    const profiles: ProfileRow[] = [
      // Ada's address, but her row names someone else now: the name guard.
      {
        profile_id: "li:ada-byron",
        clue_email: "ada@acme.io",
        clue_name: "Ada Byron",
        fields: { current_title: "Countess" },
        experience: [role(0, "Acme", "acme.io")],
      },
      // Grace's, with an employer the graph has and one it lacks.
      {
        profile_id: "li:grace-hopper",
        clue_email: "grace.hopper@initech.com",
        clue_name: "Grace Hopper",
        fields: { current_title: "Rear Admiral" },
        experience: [role(0, "Acme", "acme.io"), role(1, "Navy", "navy.mil")],
      },
    ];
    const chunk = { ontologyVersion: "v2" };
    expect(applyProfiles(g, chunk, item, profiles)).toEqual({
      applied: 1,
      unmatched: 1,
    });
    // The chunk counts the profile and the employer it couldn't bind.
    expect(enrichmentChunk(g, profiles)!.props.unmatched).toBe(2);
    expect(person("c_17")!.props.current_title).toBe("VP Engineering");
    expect(person("c_18")!.props.current_title).toBe("Rear Admiral");
    // A pointer reads into the role: the employer's name.
    const roles = [...g.graph.edges.values()].filter(
      (e) =>
        e.label === "worked_at" &&
        e.from === person("c_18")!.id &&
        e.props.title === "Advisor",
    );
    expect(roles.map((e) => [e.props.employer, e.props.title])).toEqual([
      ["Acme", "Advisor"],
    ]);
    // A chunk the load holds already is skipped.
    expect(applyProfiles(g, chunk, item, profiles)).toBeUndefined();
  });

  it("links employers by an exact identifier, and without work history fills props only", async () => {
    await allow();
    const l = await load();
    const g = tenantGraph("ten_acme_sales").view(l.id);
    // Acme is known by its website too; Initech isn't.
    const acme = [...g.nodes()].find(
      (n) => n.label === "Company" && n.props.entity_key === "acme.io",
    )!;
    acme.props.website = "acme.io";
    const role = (id: number, domain: string): Experience => ({
      id,
      title: "Advisor",
      organization: { name: domain, domain },
      location: "",
      start_date: "2020",
      end_date: null,
      is_current: false,
    });
    const item: EnrichmentBinding = {
      type: "Person",
      names: ["full_name"],
      clues: { email: "email" },
      props: [["current_title", "current_title"]],
      workHistory: {
        link: "worked_at",
        to: "Company",
        match: "website",
        key: false,
        props: [["title", "title"]],
      },
    };
    const grace = (profile: string, title: string): ProfileRow => ({
      profile_id: profile,
      clue_email: "grace.hopper@initech.com",
      clue_name: "Grace Hopper",
      fields: { current_title: title },
      experience: [role(7, "acme.io"), role(8, "initech.com")],
    });
    const advisor = () =>
      [...g.graph.edges.values()]
        .filter(
          (e) =>
            e.label === "worked_at" &&
            e.from === person("c_18")!.id &&
            e.props.title === "Advisor",
        )
        .map((e) => g.graph.nodes.get(e.to)!.props.entity_key);
    const byWebsite = [grace("li:grace-hopper", "Rear Admiral")];
    expect(
      applyProfiles(g, { ontologyVersion: "v2" }, item, byWebsite),
    ).toEqual({ applied: 1, unmatched: 0 });
    // By its website, Acme; Initech has none, so the chunk counts it.
    expect(advisor()).toEqual(["acme.io"]);
    expect(enrichmentChunk(g, byWebsite)!.props.unmatched).toBe(1);
    // An item without work history links no one.
    const { workHistory: _, ...props } = item;
    const plain = [grace("li:grace-hopper-2", "Commodore")];
    expect(applyProfiles(g, { ontologyVersion: "v2" }, props, plain)).toEqual({
      applied: 1,
      unmatched: 0,
    });
    expect(person("c_18")!.props.current_title).toBe("Commodore");
    expect(advisor()).toEqual(["acme.io"]);
    expect(enrichmentChunk(g, plain)!.props.unmatched).toBe(0);
  });

  it("cancels a live step when consent is turned off, publishing nothing", async () => {
    await allow();
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(id, "running");
    await call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "50000",
      noticeVersion: "2026-10-20",
    });
    const l = await settle(id);
    expect(l.status).toBe("ready");
    expect(l.enrichment).toMatchObject({ status: "cancelled", applied: "0" });
    expect((await settings()).creditsReserved).toBe("0");
  });

  it("cancels a live step when the ontology is deleted", async () => {
    await allow();
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(id, "running");
    expect((await call("DELETE", O)).status).toBe(200);
    expect((await settings()).creditsReserved).toBe("0");
  });
});

describe("the operators' flag off", () => {
  const pause = () => Object.assign(config, { enrichmentFlag: false });

  it("reads DISABLED, refuses turning it on or a new cap, and still turns it off", async () => {
    await allow();
    pause();
    expect((await settings()).availability).toBe(
      "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
    );
    for (const cap of ["50000", "20000"]) {
      const refused = await call("PATCH", SETTINGS, {
        enabled: true,
        monthlyCreditCap: cap,
        noticeVersion: "2026-10-20",
      });
      expect([refused.status, refused.body.message]).toEqual([
        400,
        "People enrichment isn't available",
      ]);
    }
    const off = await call("PATCH", SETTINGS, {
      enabled: false,
      monthlyCreditCap: "50000",
      noticeVersion: "2026-10-20",
    });
    expect(off.status).toBe(200);
    expect(off.body.enrichment).toMatchObject({
      availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
      enabled: false,
    });
    // Off Pro too, the flag answers first.
    setFoundryPlan("ws_acme", "FOUNDRY_TIER_FREE");
    expect((await settings()).availability).toBe(
      "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
    );
  });

  it("looks no one up, and applies what was found before", async () => {
    await allow();
    pause();
    const first = await load();
    expect(first).toMatchObject({ status: "ready", current: true });
    expect(first.enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "flag",
      toLookUp: "9",
      lookedUp: "0",
      credits: "0",
      applied: "0",
    });
    // Found while the flag was on, then carried while it is off.
    Object.assign(config, { enrichmentFlag: true });
    expect((await load("ver_sales_2", "b")).enrichment).toMatchObject({
      status: "done",
      applied: "4",
    });
    pause();
    // Bea, who wasn't found, is due a lookup again.
    vi.setSystemTime(Date.now() + 91 * 86_400_000);
    expect((await load("ver_sales_2", "c")).enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "flag",
      carried: "8",
      toLookUp: "1",
      lookedUp: "0",
      applied: "4",
    });
  });

  it("ends a queued step when the worker claims it", async () => {
    await allow();
    const id = (
      await call("POST", `${O}/loads`, {
        versionId: "ver_sales_2",
        clientToken: "a",
      })
    ).body.load.id;
    await until(id, "queued");
    pause();
    expect((await settle(id)).enrichment).toMatchObject({
      status: "partial",
      stoppedBy: "flag",
      lookedUp: "0",
    });
  });

  it("refuses enrichment edits in Revise, and the agent never proposes it", async () => {
    await allow();
    pause();
    const v1 = readAgentBundle(
      (await call("GET", `${O}/versions/ver_sales_1`)).body.version.definition,
    );
    const refused = await call("POST", `${O}/versions/ver_sales_1:revise`, {
      edits: stageEnrichment(
        v1,
        [],
        "Person",
        enrichmentDefaults(v1, "Person"),
      ),
      dryRun: true,
    });
    expect([refused.status, refused.body.message]).toEqual([
      400,
      "People enrichment isn't available",
    ]);
    // Other edits still revise.
    const ids = (toml.parse(v1.ontologyToml).types as any).Person;
    ids.identifiers.contact_id = "exact";
    expect(
      (
        await call("POST", `${O}/versions/ver_sales_1:revise`, {
          edits: [
            {
              put: {
                itemId: "type.Person",
                toml: toml.stringify({ types: { Person: ids } }),
              },
            },
          ],
          dryRun: true,
        })
      ).status,
    ).toBe(200);
    const g = (
      await call(
        "POST",
        `${WS}/foundry/ontologies/ont_acme_contracts:generate`,
        { connectorIds: ["conn_acme_shop"] },
      )
    ).body.generation;
    vi.setSystemTime(Date.now() + 100);
    const done = (
      await call(
        "GET",
        `${WS}/foundry/ontologies/ont_acme_contracts/generations/${g.id}`,
      )
    ).body.generation;
    const def = (
      await call(
        "GET",
        `${WS}/foundry/ontologies/ont_acme_contracts/versions/${done.resultVersionId}`,
      )
    ).body.version.definition;
    expect(readAgentBundle(def).enrichments).toEqual({ ok: true, value: [] });
  });
});

describe("the agent and Revise", () => {
  const generate = async () => {
    const g = (
      await call(
        "POST",
        `${WS}/foundry/ontologies/ont_acme_contracts:generate`,
        {
          connectorIds: ["conn_acme_shop"],
        },
      )
    ).body.generation;
    vi.setSystemTime(Date.now() + 100);
    const done = (
      await call(
        "GET",
        `${WS}/foundry/ontologies/ont_acme_contracts/generations/${g.id}`,
      )
    ).body.generation;
    return (
      await call(
        "GET",
        `${WS}/foundry/ontologies/ont_acme_contracts/versions/${done.resultVersionId}`,
      )
    ).body.version.definition as wfe.AgentBundleDefinition;
  };

  it("proposes enrichment for the people table only with consent", async () => {
    const without = readAgentBundle(await generate());
    expect(without.enrichments).toEqual({ ok: true, value: [] });
    await allow();
    const def = await generate();
    const bundle = readAgentBundle(def);
    expect(bundle.enrichments.ok && bundle.enrichments.value).toEqual([
      {
        name: "people",
        type: "People",
        clues: { email: "email", linkedinUrl: "linkedin_url" },
        props: [
          ["current_title", "current_title"],
          ["location", "location"],
          ["country", "location_country"],
          ["profile_url", "linkedin_url"],
        ],
        workHistory: undefined,
      },
    ]);
    expect(bundle.items["enrichment.people"]).toMatchObject({
      status: "proposed",
      clues: measureClues(people),
    });
    const type = bundle.ontology.ok
      ? bundle.ontology.value.types.find((t) => t.name === "People")
      : undefined;
    expect(type).toMatchObject({
      names: ["full_name"],
      identifiers: [
        { prop: "email", kind: "email" },
        { prop: "linkedin_url", kind: "exact" },
      ],
    });
  });

  it("leaves People enrichment out, saying why, when its people can't be looked up well", () => {
    const connector = {
      id: "conn_acme_shop",
      name: "Shop database",
      kind: "postgres",
      tables: [],
    };
    const draft = (columns: string[]) =>
      readAgentBundle(
        draftFromTables(
          [
            {
              connector,
              table: {
                name: "people",
                columns: columns.map((name) => ({ name, type: "text" })),
                primaryKey: ["contact_id"],
              },
            },
          ],
          "",
          [],
          true,
        ),
      );
    const left = (columns: string[]) => {
      const bundle = draft(columns);
      expect(bundle.enrichments).toEqual({ ok: true, value: [] });
      expect(bundle.ontologyToml).not.toContain("current_title");
      return bundle.leftOut;
    };
    const what = sourceId("conn_acme_shop", "people"),
      unnamed = {
        what,
        reason:
          "People enrichment needs a full name, and a work email or LinkedIn URL to look people up by.",
      };
    expect(left(["contact_id", "email"])).toEqual([unnamed]);
    expect(left(["contact_id", "full_name"])).toEqual([unnamed]);
    // Only Alan and Lin have a LinkedIn URL.
    expect(left(["contact_id", "full_name", "linkedin_url"])).toEqual([
      {
        what,
        reason:
          "People enrichment needs half the people to have a usable email or LinkedIn URL, none shared: 2 of 14 have one.",
      },
    ]);
    // A work email is enough.
    const email = draft(["contact_id", "full_name", "email"]);
    expect(email.enrichments.ok && email.enrichments.value[0]?.clues).toEqual({
      email: "email",
      linkedinUrl: undefined,
    });
    expect(email.leftOut).toEqual([]);
  });

  const revise = async (
    version: string,
    edits: wfe.OntologyEdit[],
    dryRun = false,
  ) => {
    const created = await call("POST", `${O}/versions/${version}:revise`, {
      edits,
      dryRun,
    });
    expect(created.status, JSON.stringify(created.body)).toBe(200);
    vi.setSystemTime(Date.now() + 100);
    return (await call("GET", `${O}/generations/${created.body.generation.id}`))
      .body.generation as wfe.OntologyGeneration;
  };

  it("turns enrichment on in a revision, and the new version loads it", async () => {
    const v1 = (await call("GET", `${O}/versions/ver_sales_1`)).body.version;
    const bundle = readAgentBundle(v1.definition);
    const edits = stageEnrichment(
      bundle,
      [],
      "Person",
      enrichmentDefaults(bundle, "Person"),
    );
    const g = await revise("ver_sales_1", edits);
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    const made = (await call("GET", `${O}/versions/${g.resultVersionId}`)).body
      .version;
    const after = readAgentBundle(made.definition);
    expect(after.enrichments.ok && after.enrichments.value[0]?.type).toBe(
      "Person",
    );
    expect(toml.parse(after.mappingsToml).enrichments).toEqual(
      toml.parse(
        readAgentBundle(
          (await call("GET", `${O}/versions/ver_sales_2`)).body.version
            .definition,
        ).mappingsToml,
      ).enrichments,
    );
    // Added by hand: no measure, and the link serves what Person serves.
    expect(after.items["enrichment.people"]).toMatchObject({
      status: "user_edited",
    });
    expect(after.items["enrichment.people"]).not.toHaveProperty("clues");
    expect(after.items["link.worked_at"]?.serves).toEqual(["cq.CQ1"]);
  });

  it("revises and loads an item with one clue", async () => {
    await allow();
    const v1 = (await call("GET", `${O}/versions/ver_sales_1`)).body.version;
    const bundle = readAgentBundle(v1.definition);
    const g = await revise(
      "ver_sales_1",
      stageEnrichment(bundle, [], "Person", {
        ...enrichmentDefaults(bundle, "Person"),
        clues: { email: "email" },
      }),
    );
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    const made = readAgentBundle(
      (await call("GET", `${O}/versions/${g.resultVersionId}`)).body.version
        .definition,
    );
    expect(made.enrichments.ok && made.enrichments.value[0]?.clues).toEqual({
      email: "email",
      linkedinUrl: undefined,
    });
    // Alan and Lin have only a LinkedIn URL: nobody looks them up.
    const l = await load(g.resultVersionId!, "one");
    expect(l.enrichment).toMatchObject({
      status: "done",
      toLookUp: "8",
      found: "3",
      applied: "3",
    });
    expect(person("c_19")?.props.current_title).toBeUndefined();
    expect(person("c_17")?.props.current_title).toBe("VP Engineering");
  });

  it("revises and loads an item without work history", async () => {
    await allow();
    const v1 = (await call("GET", `${O}/versions/ver_sales_1`)).body.version;
    const bundle = readAgentBundle(v1.definition);
    const edits = stageEnrichment(bundle, [], "Person", {
      ...enrichmentDefaults(bundle, "Person"),
      workHistory: undefined,
    });
    expect(edits.map(editItem)).toEqual(["type.Person", "enrichment.people"]);
    const g = await revise("ver_sales_1", edits);
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    const l = await load(g.resultVersionId!, "plain");
    expect(l.enrichment).toMatchObject({
      status: "done",
      found: "4",
      applied: "4",
    });
    expect(person("c_17")?.props.current_title).toBe("VP Engineering");
    const view = tenantGraph("ten_acme_sales").view(l.id);
    expect(
      [...view.graph.edges.values()].filter((e) => e.label === "worked_at"),
    ).toEqual([]);
  });

  it("drops the measure of an item a revision puts", async () => {
    const v2 = readAgentBundle(
      (await call("GET", `${O}/versions/ver_sales_2`)).body.version.definition,
    );
    expect(
      confidenceText(readEnrichmentClues(v2.items["enrichment.people"]?.clues)),
    ).toMatch(/^Confidence: High/);
    const on = enrichmentForm(v2, "Person");
    if (!on.on) throw new Error("on");
    // Only the item changes: Person identifies by email already.
    const edits = stageEnrichment(v2, [], "Person", {
      ...on,
      clues: { email: "email" },
    });
    expect(edits.map(editItem)).toEqual(["enrichment.people"]);
    const g = await revise("ver_sales_2", edits);
    expect(g.status).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
    const made = readAgentBundle(
      (await call("GET", `${O}/versions/${g.resultVersionId}`)).body.version
        .definition,
    );
    expect(made.items["enrichment.people"]).toMatchObject({
      status: "user_edited",
    });
    expect(made.items["enrichment.people"]).not.toHaveProperty("clues");
    expect(
      confidenceText(
        readEnrichmentClues(made.items["enrichment.people"]?.clues),
      ),
    ).toBe("Confidence: not measured. Load to see who can be looked up.");
  });

  it("turns it off, keeping the props and the link, and drops a stale measure", async () => {
    const off = await revise("ver_sales_2", [
      { remove: { itemId: "enrichment.people" } },
    ]);
    const made = readAgentBundle(
      (await call("GET", `${O}/versions/${off.resultVersionId}`)).body.version
        .definition,
    );
    expect(made.enrichments).toEqual({ ok: true, value: [] });
    expect(made.mappingsToml).not.toContain("enrichments");
    expect(made.ontologyToml).toContain("current_title");
    expect(made.ontologyToml).toContain("[links.worked_at]");
    // An identifier edit to Person drops the item's measure.
    const v2 = readAgentBundle(
      (await call("GET", `${O}/versions/ver_sales_2`)).body.version.definition,
    );
    const person = toml.parse(v2.ontologyToml).types as any;
    person.Person.identifiers.contact_id = "exact";
    const edited = await revise("ver_sales_2", [
      {
        put: {
          itemId: "type.Person",
          toml: toml.stringify({ types: { Person: person.Person } }),
        },
      },
    ]);
    const after = readAgentBundle(
      (await call("GET", `${O}/versions/${edited.resultVersionId}`)).body
        .version.definition,
    );
    expect(after.items["enrichment.people"]).not.toHaveProperty("clues");
  });

  it("refuses enrichment the engine would, as a validation finding", async () => {
    const v2 = readAgentBundle(
      (await call("GET", `${O}/versions/ver_sales_2`)).body.version.definition,
    );
    const item = (toml.parse(v2.mappingsToml).enrichments as any).people;
    item.props.contact_id = "current_title";
    const g = await revise(
      "ver_sales_2",
      [
        {
          put: {
            itemId: "enrichment.people",
            toml: toml.stringify({ enrichments: { people: item } }),
          },
        },
      ],
      true,
    );
    expect(JSON.parse(g.revision!.resultJson!)).toMatchObject({
      status: "refused",
      findings: [{ code: "Validation", item_id: "enrichment.people" }],
    });
  });

  it("measures a draft that only adds props and links on its load", async () => {
    const v1 = (await call("GET", `${O}/versions/ver_sales_1`)).body.version
      .definition;
    const v2 = (await call("GET", `${O}/versions/ver_sales_2`)).body.version
      .definition;
    expect(comparableSchema(v2, v1)).toBe(true);
    expect(comparableSchema(v1, v2)).toBe(false);
    expect(comparableSchema(v1, v1)).toBe(true);
    expect(comparableSchema(v2, { format: "foundry_v1" })).toBe(false);
  });
});
