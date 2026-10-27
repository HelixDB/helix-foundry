import { describe, expect, it } from "vitest";
import type {
  FoundryEnrichmentStop,
  FoundryLoad,
  FoundryLoadEnrichment,
  FoundryOntology,
  GraphJob,
} from "@/shared/wfe";
import {
  confidenceText,
  cluesText,
  enrichmentOutcome,
  enrichmentStepText,
  jobBadge,
  lastLoadText,
  loadSteps,
  lookingUp,
  parseAgentEnrichments,
  profilesDate,
  readAgentBundle,
  readEnrichmentClues,
  shortDate,
  stoppedWaiting,
  utcDay,
} from "@/web/areas/ontology/model";
import { lastLoadBadge } from "@/web/areas/ontology/parts";

// People enrichment as the Ontology area reads it: the [enrichments] item of
// mappings.toml, the agent's confidence in it, and a load's enrichment step.

const ITEM = `
[enrichments.people]
type = "Person"
clues = { email = "email", linkedin_url = "linkedin_url" }
props = { current_title = "current_title", location = "location", country = "location_country", profile_url = "linkedin_url" }
work_history = { link = "worked_at", match = "domain", props = { title = "title", start_date = "start_date", end_date = "end_date", is_current = "is_current" } }
`;

describe("the [enrichments] item", () => {
  it("reads its type, clues, target props and work history", () => {
    const parsed = parseAgentEnrichments(ITEM);
    expect(parsed).toEqual({
      ok: true,
      value: [
        {
          name: "people",
          type: "Person",
          clues: { email: "email", linkedinUrl: "linkedin_url" },
          props: [
            ["current_title", "current_title"],
            ["location", "location"],
            ["country", "location_country"],
            ["profile_url", "linkedin_url"],
          ],
          workHistory: {
            link: "worked_at",
            match: "domain",
            props: [
              ["title", "title"],
              ["start_date", "start_date"],
              ["end_date", "end_date"],
              ["is_current", "is_current"],
            ],
          },
        },
      ],
    });
  });

  it("reads an item with one clue and no work history, trusting no shape", () => {
    const parsed = parseAgentEnrichments(
      '[enrichments.b]\ntype = 3\nclues = { linkedin_url = "li", email = 4 }\nprops = { title = "current_title", bad = 7 }\n[enrichments.a]\nclues = "email"\nprops = "x"\nwork_history = "worked_at"',
    );
    expect(parsed.ok && parsed.value).toEqual([
      {
        name: "a",
        type: "",
        clues: { email: undefined, linkedinUrl: undefined },
        props: [],
        workHistory: undefined,
      },
      {
        name: "b",
        type: "",
        clues: { email: undefined, linkedinUrl: "li" },
        props: [["title", "current_title"]],
        workHistory: undefined,
      },
    ]);
  });

  it("has none without the table, and reports TOML that doesn't parse", () => {
    expect(parseAgentEnrichments('[mappings.x]\nsource = "s"')).toEqual({
      ok: true,
      value: [],
    });
    expect(parseAgentEnrichments("[enrichments")).toMatchObject({ ok: false });
  });

  it("is part of a version's bundle", () => {
    const bundle = readAgentBundle({
      format: "agent_bundle",
      ontology_toml: "",
      mappings_toml: ITEM,
    });
    expect(bundle.enrichments.ok && bundle.enrichments.value[0]?.type).toBe(
      "Person",
    );
  });

  it("names its clues in words", () => {
    const parsed = parseAgentEnrichments(ITEM);
    if (!parsed.ok) throw new Error(parsed.error);
    const e = parsed.value[0]!;
    expect(cluesText(e)).toBe("email, LinkedIn");
    expect(cluesText({ ...e, clues: { email: "work_email" } })).toBe("email");
    expect(cluesText({ ...e, clues: { linkedinUrl: "li" } })).toBe("LinkedIn");
  });
});

describe("the agent's confidence", () => {
  const clues = {
    people: 980,
    usable: 902,
    email: 870,
    linkedin_url: 412,
    no_name: 13,
    shared: 0,
    conflicting: 2,
  };

  it("reads the stored clues, refusing ones that don't add up", () => {
    expect(readEnrichmentClues(clues)).toEqual({
      people: 980,
      usable: 902,
      shared: 0,
      noName: 13,
    });
    for (const bad of [
      undefined,
      null,
      "high",
      [],
      { ...clues, usable: 981 },
      { ...clues, people: -1 },
      { ...clues, shared: 1.5 },
      { ...clues, no_name: "13" },
      { people: 10, usable: 5, shared: 0 },
    ])
      expect(readEnrichmentClues(bad), JSON.stringify(bad)).toBeUndefined();
  });

  it("is High from 80% usable with none shared, Medium from 50%, else Low", () => {
    expect(confidenceText(readEnrichmentClues(clues))).toBe(
      "Confidence: High · 902 of 980 people have a usable email or LinkedIn (92%) · 0 shared · 13 without a full name",
    );
    expect(
      confidenceText({ people: 980, usable: 610, shared: 0, noName: 0 }),
    ).toBe("Confidence: Medium · 610 of 980 usable (62%)");
    expect(
      confidenceText({ people: 980, usable: 310, shared: 0, noName: 0 }),
    ).toBe("Confidence: Low · 310 of 980 usable (32%)");
    // On the thresholds.
    expect(
      confidenceText({ people: 10, usable: 8, shared: 0, noName: 0 }),
    ).toMatch(/^Confidence: High · 8 of 10 people/);
    expect(
      confidenceText({ people: 10, usable: 5, shared: 0, noName: 0 }),
    ).toBe("Confidence: Medium · 5 of 10 usable (50%)");
    expect(
      confidenceText({ people: 10, usable: 4, shared: 0, noName: 0 }),
    ).toBe("Confidence: Low · 4 of 10 usable (40%)");
    // Just under a threshold, the percent doesn't round up to it.
    expect(
      confidenceText({ people: 250, usable: 199, shared: 0, noName: 0 }),
    ).toBe("Confidence: Medium · 199 of 250 usable (79%)");
    expect(
      confidenceText({ people: 200, usable: 99, shared: 0, noName: 0 }),
    ).toBe("Confidence: Low · 99 of 200 usable (49%)");
    // Elsewhere it rounds as usual.
    expect(
      confidenceText({ people: 200, usable: 157, shared: 0, noName: 0 }),
    ).toBe("Confidence: Medium · 157 of 200 usable (79%)");
    expect(
      confidenceText({ people: 1000, usable: 999, shared: 0, noName: 0 }),
    ).toMatch(/\(100%\)/);
  });

  it("is never High while a value is shared, and says how many are", () => {
    expect(
      confidenceText({ people: 980, usable: 960, shared: 2, noName: 0 }),
    ).toBe("Confidence: Medium · 960 of 980 usable (98%) · 2 shared");
  });

  it("is Low for no people, and not measured without clues", () => {
    expect(confidenceText({ people: 0, usable: 0, shared: 0, noName: 0 })).toBe(
      "Confidence: Low · 0 of 0 usable (0%)",
    );
    expect(confidenceText(undefined)).toBe(
      "Confidence: not measured. Load to see who can be looked up.",
    );
  });
});

const job = (over: Partial<GraphJob> = {}): GraphJob => ({
  id: "j",
  kind: "load",
  status: "running",
  phase: "provisioning",
  ...over,
});
const NOV_1 = utcDay(new Date(Date.UTC(2026, 10, 1)));

describe("a load's enrichment step", () => {
  it("is looking people up only at the first step, queued or running", () => {
    const e: FoundryLoadEnrichment = { status: "running" };
    expect(lookingUp(job(), e)).toBe(true);
    // Parked: the job waits as a retry while the lookups run.
    expect(lookingUp(job({ status: "queued" }), { status: "queued" })).toBe(
      true,
    );
    expect(lookingUp(job({ phase: undefined }), e)).toBe(true);
    expect(lookingUp(job({ phase: "ingest" }), e)).toBe(false);
    expect(lookingUp(job({ status: "succeeded" }), e)).toBe(false);
    expect(lookingUp(job(), { status: "done" })).toBe(false);
    // Finding its people, before it parks.
    expect(lookingUp(job(), { status: "preparing" })).toBe(false);
    expect(lookingUp(job(), null)).toBe(false);
  });

  it("badges the job as looking people up", () => {
    expect(jobBadge(job({ status: "queued" }), { status: "queued" })).toEqual({
      status: "running",
      label: "Looking up people",
    });
    expect(jobBadge(job(), { status: "done" })).toEqual({
      status: "running",
      label: "Preparing",
    });
    expect(jobBadge(job({ status: "queued" }))).toEqual({
      status: "queued",
      label: "Queued",
    });
  });

  it("badges the overview's last load as looking people up while its job is parked", () => {
    const o: FoundryOntology = {
      id: "ont_1",
      lastLoad: { jobId: "j", status: "queued" },
    };
    const live = (enrichment: FoundryLoadEnrichment | undefined) => ({
      load: { id: "l", enrichment },
      job: job({ status: "queued" }),
    });
    expect(lastLoadBadge(o, live({ status: "queued" }))).toEqual({
      status: "running",
      label: "Looking up people",
    });
    // Its row has only the job's status; a load without the step, or past
    // it, reads as its job.
    for (const read of [undefined, live(undefined), live({ status: "done" })])
      expect(lastLoadBadge(o, read)).toEqual({
        status: "queued",
        label: "Queued",
      });
    expect(lastLoadBadge({ id: "ont_1" })).toEqual({
      status: "draft",
      label: "Not loaded",
    });
  });

  it("says how far the lookups are, and that a queued step waits for a slot", () => {
    expect(enrichmentStepText({ status: "preparing" })).toBe(
      "Finding the people to look up",
    );
    expect(enrichmentStepText({ status: "queued" })).toBe(
      "Waiting for a lookup slot (up to 5 min)",
    );
    expect(
      enrichmentStepText({
        status: "running",
        toLookUp: "612",
        lookedUp: "380",
        found: "104",
      }),
    ).toBe("380 of 612 looked up · 104 found");
    expect(enrichmentStepText({ status: "running" })).toBe(
      "0 of 0 looked up · 0 found",
    );
  });

  it("says what a finished step found, held and charged", () => {
    expect(
      enrichmentStepText({
        status: "done",
        toLookUp: "612",
        lookedUp: "612",
        found: "172",
        held: "38",
        carried: "272",
        credits: "4240",
      }),
    ).toBe("612 looked up · 172 found (38 held) · 4,240 credits");
    // Once its tables are in: the profiles found before it applied, as the
    // callout counts them (carried counts every earlier result reused, a no
    // match too).
    expect(
      enrichmentStepText({
        status: "done",
        toLookUp: "612",
        lookedUp: "612",
        found: "172",
        held: "38",
        carried: "272",
        credits: "4240",
        applied: "409",
        unmatched: "3",
      }),
    ).toBe(
      "612 looked up · 172 found (38 held) · 240 found before · 4,240 credits",
    );
    // A second load: everyone was looked up before.
    expect(
      enrichmentStepText({
        status: "done",
        carried: "412",
        credits: "0",
        applied: "238",
        unmatched: "2",
      }),
    ).toBe("0 looked up · 0 found · 240 found before · 0 credits");
    expect(
      enrichmentStepText({ status: "done", carried: "412", credits: "0" }),
    ).toBe("0 looked up · 0 found · 0 credits");
  });

  const partial = (
    stoppedBy: FoundryEnrichmentStop | undefined,
  ): FoundryLoadEnrichment => ({
    status: "partial",
    stoppedBy,
    toLookUp: "612",
    lookedUp:
      stoppedBy === "time" ? "3600" : stoppedBy === "cap" ? "400" : "380",
    carried: "272",
    publishedAt: "2026-10-20T14:06:00Z",
  });
  it.each<[FoundryEnrichmentStop | undefined, string]>([
    [
      "cap",
      `400 of 612 looked up · credit cap reached; 212 wait until it resets on ${NOV_1}`,
    ],
    [
      "provider_credits",
      "context.dev stopped answering after 380; the rest next load",
    ],
    ["provider", "context.dev stopped answering after 380; the rest next load"],
    ["time", "Time limit reached after 3,600; the rest next load"],
    [
      "busy",
      "Lookups were busy; profiles found before applied, the rest next load",
    ],
    ["stopped", "Stopped after 380; the rest next load"],
    ["pro", "Not on Foundry Pro: profiles found before applied"],
    ["flag", "Lookups paused by Helix: profiles found before applied"],
    [undefined, "380 of 612 looked up; the rest next load"],
  ])("labels a partial step stopped by %s", (stoppedBy, text) => {
    expect(enrichmentStepText(partial(stoppedBy))).toBe(text);
  });

  it("counts the profiles found before once the load's tables are in", () => {
    const done = { applied: "238", unmatched: "2" };
    expect(enrichmentStepText({ ...partial("busy"), ...done })).toBe(
      "Lookups were busy; 240 profiles found before applied, the rest next load",
    );
    expect(enrichmentStepText({ ...partial("pro"), ...done })).toBe(
      "Not on Foundry Pro: 240 profiles found before applied",
    );
    expect(
      enrichmentStepText({ ...partial("pro"), applied: "1", unmatched: "0" }),
    ).toBe("Not on Foundry Pro: 1 profile found before applied");
  });

  it("dates a cap's reset from now when the step has no publish time", () => {
    const e = { ...partial("cap"), publishedAt: null };
    expect(enrichmentStepText(e, Date.UTC(2026, 11, 31, 23))).toContain(
      `resets on ${utcDay(new Date(Date.UTC(2027, 0, 1)))}`,
    );
  });

  it("says a step that applied nothing new why", () => {
    expect(enrichmentStepText({ status: "off" })).toBe(
      "Off for this workspace: no profiles applied",
    );
    expect(enrichmentStepText({ status: "failed" })).toBe(
      "Lookups failed: profiles found before applied",
    );
    expect(enrichmentStepText({ status: "cancelled" })).toBe(
      "Cancelled: no profiles published",
    );
    expect(enrichmentStepText({})).toBe("");
    expect(
      enrichmentStepText({
        status: "later" as FoundryLoadEnrichment["status"],
      }),
    ).toBe("");
  });

  it("steps through the lookups in place of connecting to the database", () => {
    const labels = (j: GraphJob, e?: FoundryLoadEnrichment | null) =>
      loadSteps(j, 3, e).map((s) => `${s.label}: ${s.status}`);
    // Parked while the enrichment worker looks people up.
    expect(labels(job({ status: "queued" }), { status: "queued" })).toEqual([
      "Started: done",
      "Waiting for a lookup slot (up to 5 min): active",
      "Loading 3 tables: pending",
      "Reading documents: pending",
      "Finishing: pending",
    ]);
    expect(
      labels(job({ status: "queued" }), {
        status: "running",
        toLookUp: "612",
        lookedUp: "380",
        found: "104",
      })[1],
    ).toBe("Looking up people · 380 of 612 looked up · 104 found: active");
    // Finding its people first, at the same step.
    expect(labels(job(), { status: "preparing" })[1]).toBe(
      "People enrichment · Finding the people to look up: active",
    );
    const done: FoundryLoadEnrichment = {
      status: "done",
      lookedUp: "2",
      toLookUp: "2",
      found: "1",
      credits: "20",
    };
    // Woken: queued at its first step until the graph worker picks it up
    // again, then the tables load with the profiles. It has started.
    expect(labels(job({ status: "queued" }), done)).toEqual([
      "Started: done",
      "People enrichment · 2 looked up · 1 found · 20 credits: active",
      "Loading 3 tables: pending",
      "Reading documents: pending",
      "Finishing: pending",
    ]);
    expect(
      labels(job({ status: "queued" }), {
        status: "partial",
        stoppedBy: "stopped",
        toLookUp: "612",
        lookedUp: "380",
      }).slice(0, 2),
    ).toEqual([
      "Started: done",
      "People enrichment · Stopped after 380; the rest next load: active",
    ]);
    // A job queued again past its first step waits to start as before.
    expect(
      labels(job({ status: "queued", phase: "ingest" }), done).slice(0, 2),
    ).toEqual([
      "Waiting to start: active",
      "People enrichment · 2 looked up · 1 found · 20 credits: pending",
    ]);
    expect(labels(job({ phase: "ingest" }), done).slice(0, 3)).toEqual([
      "Started: done",
      "People enrichment · 2 looked up · 1 found · 20 credits: done",
      "Loading 3 tables: active",
    ]);
    expect(labels(job({ status: "succeeded" }), done)[1]).toMatch(/: done$/);
    // Picked up again, before the tables: the step's result shows.
    expect(labels(job(), { status: "off" })[1]).toBe(
      "People enrichment · Off for this workspace: no profiles applied: active",
    );
    // Not picked up yet: no step to show.
    expect(labels(job({ status: "queued" }), null)).toEqual([
      "Waiting to start: active",
      "Connecting to the database: pending",
      "Loading 3 tables: pending",
      "Reading documents: pending",
      "Finishing: pending",
    ]);
    // A status this build doesn't know reads as no step.
    expect(labels(job(), { status: "later" as never })[1]).toBe(
      "Connecting to the database: active",
    );
    // Still looking people up once the load stopped waiting for it (past its
    // deadline and the grace): the load went on with the profiles found
    // before, and what the step finds is for the next load.
    const behind: FoundryLoadEnrichment = {
      status: "running",
      toLookUp: "612",
      lookedUp: "380",
      found: "104",
    };
    for (const j of [
      job({ phase: "ingest" }),
      job({ phase: "documents" }),
      job({ status: "succeeded", phase: "finishing" }),
    ])
      expect(labels(j, behind)[1]).toBe(
        "People enrichment · Still looking up after the load stopped waiting; profiles found before applied, the rest next load: done",
      );
    expect(
      labels(job({ status: "succeeded", phase: "finishing" }), {
        ...behind,
        applied: "238",
        unmatched: "2",
      })[1],
    ).toBe(
      "People enrichment · Still looking up after the load stopped waiting; 240 profiles found before applied, the rest next load: done",
    );
    expect(labels(job({ phase: "documents" }), { status: "queued" })[1]).toBe(
      "People enrichment · Still looking up after the load stopped waiting; profiles found before applied, the rest next load: done",
    );
  });

  it("keeps an extract to its documents", () => {
    expect(
      loadSteps(job({ kind: "extract", phase: "documents" }), 0, {
        status: "done",
      }).map((s) => s.label),
    ).toEqual(["Started", "Reading documents", "Finishing"]);
  });
});

describe("what a load's enrichment applied", () => {
  it("counts the profiles, new and found before, and those that didn't apply", () => {
    // The profiles are what the load applied, bound or not: 272 carried
    // held 240 profiles and 32 people found or held before.
    expect(
      enrichmentOutcome({
        status: "done",
        found: "172",
        carried: "272",
        applied: "409",
        unmatched: "3",
      }),
    ).toEqual({
      text: "412 profiles (172 new, 240 found before) · 409 applied, 3 not: the person changed since the lookup.",
      cap: false,
    });
    expect(
      enrichmentOutcome({ status: "done", found: "1", applied: "1" })?.text,
    ).toBe("1 profile (1 new, 0 found before) · 1 applied.");
    // A load that never got its tables in applied nothing yet.
    expect(
      enrichmentOutcome({ status: "done", found: "2", carried: "5" })?.text,
    ).toBe("2 profiles (2 new, 0 found before) · 0 applied.");
  });

  it("says who waits for the next load, and links the cap when it stopped them", () => {
    const e: FoundryLoadEnrichment = {
      status: "partial",
      stoppedBy: "cap",
      found: "172",
      carried: "272",
      applied: "412",
      toLookUp: "612",
      lookedUp: "400",
    };
    expect(enrichmentOutcome(e)).toEqual({
      text: "412 profiles (172 new, 240 found before) · 412 applied. 212 people wait for the next load: this month’s credit cap was reached.",
      cap: true,
    });
    expect(
      enrichmentOutcome({ ...e, stoppedBy: "stopped", lookedUp: "611" }),
    ).toEqual({
      text: "412 profiles (172 new, 240 found before) · 412 applied. 1 person waits for the next load: the lookups were stopped.",
      cap: false,
    });
    for (const [stoppedBy, reason] of [
      ["provider_credits", "context.dev stopped answering"],
      ["provider", "context.dev stopped answering"],
      ["time", "the time limit was reached"],
      ["busy", "lookups were busy"],
      ["pro", "the workspace isn’t on Foundry Pro"],
      ["flag", "Helix paused lookups"],
      [undefined, "the lookups stopped early"],
      // A reason from a newer WFE.
      ["later" as FoundryEnrichmentStop, "the lookups stopped early"],
    ] as const)
      expect(enrichmentOutcome({ ...e, stoppedBy })?.text).toMatch(
        new RegExp(`212 people wait for the next load: ${reason}\\.$`),
      );
    // Everyone was reached after all.
    expect(enrichmentOutcome({ ...e, lookedUp: "612" })).toEqual({
      text: "412 profiles (172 new, 240 found before) · 412 applied.",
      cap: false,
    });
  });

  it("says a step that published after its load stopped waiting found for the next load", () => {
    // The worker published past its deadline and the grace: the load had
    // applied the 240 profiles carried, and found counts the step's own.
    const e: FoundryLoadEnrichment = {
      status: "done",
      toLookUp: "612",
      lookedUp: "612",
      found: "172",
      held: "38",
      carried: "300",
      credits: "4240",
      applied: "240",
      unmatched: "0",
      publishedAt: "2026-10-20T14:00:00Z",
    };
    const load = (patch: Partial<FoundryLoad>): FoundryLoad => ({
      id: "load_1",
      status: "ready",
      finishedAt: "2026-10-20T13:00:00Z",
      enrichment: e,
      ...patch,
    });
    expect(stoppedWaiting(load({}))).toBe(true);
    expect(enrichmentOutcome(e, Date.now(), true)?.text).toBe(
      "240 profiles (0 new, 240 found before) · 240 applied. 172 profiles found after the load stopped waiting apply from the next load.",
    );
    expect(enrichmentStepText(e, Date.now(), true)).toBe(
      "612 looked up · 172 found (38 held) for the next load · 240 found before · 4,240 credits",
    );
    expect(loadSteps(job({ status: "succeeded" }), 3, e, true)[1]?.label).toBe(
      "People enrichment · 612 looked up · 172 found (38 held) for the next load · 240 found before · 4,240 credits",
    );
    // Published before the load ended, it reads as on time.
    expect(stoppedWaiting(load({ finishedAt: "2026-10-20T15:00:00Z" }))).toBe(
      false,
    );
    // Nothing reads late before the load ends, or before the step published.
    expect(stoppedWaiting(load({ finishedAt: null }))).toBe(false);
    expect(
      stoppedWaiting(load({ enrichment: { ...e, publishedAt: null } })),
    ).toBe(false);
    expect(stoppedWaiting(load({ enrichment: null }))).toBe(false);
    // A late step that found none adds nothing.
    expect(
      enrichmentOutcome({ ...e, found: "0" }, Date.now(), true)?.text,
    ).toBe("240 profiles (0 new, 240 found before) · 240 applied.");
  });

  it("tells a step still looking people up after its load stopped waiting", () => {
    const running: FoundryLoadEnrichment = {
      status: "running",
      toLookUp: "612",
      lookedUp: "380",
      found: "104",
    };
    const load = (jobs?: GraphJob[], enrichment = running): FoundryLoad => ({
      id: "load_1",
      status: "ready",
      jobs,
      enrichment,
    });
    // Its load job went on past its first step, or ended.
    for (const j of [
      job({ phase: "ingest" }),
      job({ phase: "documents" }),
      job({ status: "succeeded", phase: "finishing" }),
      job({ status: "failed" }),
    ]) {
      expect(stoppedWaiting(load([j]))).toBe(true);
      expect(stoppedWaiting(load([j], { status: "queued" }))).toBe(true);
    }
    // Uploads since then queued extracts, newest first; its load job may be
    // past the newest 20.
    expect(
      stoppedWaiting(
        load([
          job({ kind: "extract", status: "succeeded", phase: "finishing" }),
          job({ status: "succeeded", phase: "finishing" }),
        ]),
      ),
    ).toBe(true);
    expect(
      stoppedWaiting(
        load([
          job({ kind: "extract", status: "succeeded", phase: "finishing" }),
        ]),
      ),
    ).toBe(true);
    // Waiting for it, parked at its first step, or not known.
    expect(stoppedWaiting(load([job()]))).toBe(false);
    expect(stoppedWaiting(load([job({ status: "queued" })]))).toBe(false);
    expect(stoppedWaiting(load([]))).toBe(false);
    expect(stoppedWaiting(load(undefined))).toBe(false);
  });

  it("says why a step applied nothing new, and nothing while it runs", () => {
    expect(enrichmentOutcome({ status: "off" })).toEqual({
      text: "Off for this workspace: no profiles applied.",
      cap: false,
    });
    expect(enrichmentOutcome({ status: "failed" })?.text).toBe(
      "Lookups failed: profiles found before applied.",
    );
    expect(enrichmentOutcome({ status: "cancelled" })?.text).toBe(
      "Cancelled: no profiles published.",
    );
    expect(enrichmentOutcome({ status: "preparing" })).toBeUndefined();
    expect(enrichmentOutcome({ status: "queued" })).toBeUndefined();
    expect(enrichmentOutcome({ status: "running" })).toBeUndefined();
    // Unless the load stopped waiting for it: it applied what was found
    // before.
    expect(
      enrichmentOutcome(
        { status: "running", found: "104", applied: "238", unmatched: "2" },
        Date.now(),
        true,
      ),
    ).toEqual({
      text: "Still looking up after the load stopped waiting; 240 profiles found before applied, the rest next load.",
      cap: false,
    });
    expect(enrichmentOutcome({ status: "queued" }, Date.now(), true)).toEqual({
      text: "Still looking up after the load stopped waiting; profiles found before applied, the rest next load.",
      cap: false,
    });
    expect(enrichmentOutcome({})).toBeUndefined();
    expect(enrichmentOutcome({ status: "later" as never })).toBeUndefined();
  });

  it("dates a load's profiles only when its own lookups found every one", () => {
    const at = "2026-10-20T14:00:00Z",
      load = (e: FoundryLoadEnrichment): FoundryLoad => ({
        id: "load_1",
        status: "ready",
        finishedAt: "2026-10-20T15:00:00Z",
        enrichment: { status: "done", publishedAt: at, ...e },
      });
    expect(profilesDate(load({ found: "4", carried: "0" }))).toBe(
      shortDate(at),
    );
    expect(profilesDate(load({ found: "4" }))).not.toBe("");
    // A load that reused earlier results publishes them again, with its own
    // date: nothing is refreshed, so that date isn't theirs.
    expect(profilesDate(load({ found: "0", carried: "4" }))).toBe("");
    expect(profilesDate(load({ carried: "4" }))).toBe("");
    expect(profilesDate(load({ found: "172", carried: "272" }))).toBe("");
    for (const stoppedBy of ["pro", "flag", "busy"] as const)
      expect(
        profilesDate(
          load({ status: "partial", stoppedBy, found: "0", carried: "4" }),
        ),
      ).toBe("");
    // A step that published after the load stopped waiting: the load
    // applied none of what it found.
    expect(
      profilesDate({
        ...load({ found: "4", carried: "0" }),
        finishedAt: "2026-10-20T13:00:00Z",
      }),
    ).toBe("");
    expect(
      profilesDate(load({ found: "4", carried: "0", publishedAt: null })),
    ).toBe("");
    expect(profilesDate({ id: "load_1", enrichment: null })).toBe("");
  });

  it("says who the version's newest load could look up, or why it looked no one up", () => {
    const step = (
      id: string,
      enrichment?: FoundryLoadEnrichment,
      versionId = "ver_2",
    ): FoundryLoad => ({ id, versionId, enrichment });
    const done = step("load_1", {
      status: "done",
      people: "980",
      noName: "13",
      noClue: "43",
      skipped: "56",
      found: "172",
    });
    expect(lastLoadText([done], "ver_2")).toBe(
      "Last load: 924 of 980 could be looked up · 172 found",
    );
    expect(
      lastLoadText([step("load_2", { status: "partial" }), done], "ver_2"),
    ).toBe("Last load: 0 of 0 could be looked up · 0 found");
    // A newer load that looked no one up says why; an older one's counts
    // never stand in.
    for (const [status, text] of [
      ["off", "Last load: off for this workspace, so no profiles applied"],
      ["failed", "Last load: lookups failed, so profiles found before applied"],
      ["cancelled", "Last load: cancelled, so no profiles published"],
      ["running", undefined],
      ["queued", undefined],
      ["later", undefined],
    ] as const)
      expect(
        lastLoadText(
          [step("load_2", { status: status as never }), done],
          "ver_2",
        ),
      ).toBe(text);
    // Loads without a step, and other versions' loads, don't count.
    expect(
      lastLoadText(
        [step("load_3"), step("load_2", { status: "off" }, "ver_1"), done],
        "ver_2",
      ),
    ).toBe("Last load: 924 of 980 could be looked up · 172 found");
    expect(lastLoadText([], "ver_2")).toBeUndefined();
  });
});
