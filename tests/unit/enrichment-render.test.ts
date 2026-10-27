import { createElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import * as toml from "smol-toml";
import { describe, expect, it, vi } from "vitest";
import * as edit from "@/web/areas/ontology/edit-model";
import { readAgentBundle } from "@/web/areas/ontology/model";
import { EnrichPeople } from "@/web/areas/ontology/version-edit";
import { EnrichmentTab } from "@/web/areas/workspace/enrichment";
import {
  ENRICHMENT_NOTICE,
  enrichmentKey,
} from "@/web/areas/workspace/enrichment-api";
import { WorkspaceContext, type WorkspaceRole } from "@/web/ui";
import type * as wfe from "@/shared/wfe";
import { salesGraphBundle } from "../../mock/fixtures/sales-graph";
import { sourceId } from "../../mock/routes/ontology-agent";

// The Enrich people form as it first renders: from the pending edits, and
// off while another type holds the file's one item. Settings › People
// enrichment as the owner and others first see it, in every availability.

// A version's item notes read its loads under the ontology's scope, which
// these renders have none of (tests/e2e/enrichment.spec.ts covers them).
vi.mock("@/web/areas/ontology/version-parts", async (original) => ({
  ...(await original<object>()),
  EnrichmentNotes: () => null,
}));

const SOURCE = sourceId("conn_acme_shop", "people");
const CLUES = {
  people: 16,
  usable: 9,
  email: 9,
  linkedin_url: 2,
  no_name: 1,
  shared: 2,
  conflicting: 0,
};

/** The Sales graph with Company named by its domain, so it could be enriched too. */
const named = (clues?: typeof CLUES) => {
  const def = salesGraphBundle(SOURCE, clues);
  const o = toml.parse(readAgentBundle(def).ontologyToml) as Record<
    string,
    any
  >;
  o.types.Company.names = ["domain"];
  return readAgentBundle({ ...def, ontology_toml: toml.stringify(o) });
};
const render = (
  bundle: ReturnType<typeof readAgentBundle>,
  type: string,
  edits: wfe.OntologyEdit[] = [],
  settings?: wfe.FoundryEnrichment,
) =>
  renderToStaticMarkup(
    createElement(
      MemoryRouter,
      null,
      createElement(EnrichPeople, {
        bundle,
        versionId: "ver_1",
        type,
        edits,
        settings,
        nextVersion: 3,
        blocked: false,
        onSave: () => {},
        onUnstage: () => {},
      }),
    ),
  );
/** The markup of the input labelled `label`; it must be there. */
const input = (html: string, label: string) => {
  const found = html.match(new RegExp(`<input[^>]*>(?=${label}<)`))?.[0];
  if (!found) throw new Error(`no input labelled ${label}`);
  return found;
};

describe("the Enrich people form", () => {
  it("starts from the pending edits, so a refused save keeps its choices", () => {
    const plain = named();
    const form = edit.enrichmentDefaults(plain, "Person");
    if (!form.on) throw new Error("on");
    const pending = edit.stageEnrichment(plain, [], "Person", {
      ...form,
      fields: form.fields.filter((f) => f.prop !== "country"),
    });
    const html = render(plain, "Person", pending);
    expect(input(html, "Enrich people")).toContain("checked");
    expect(input(html, "Current title")).toContain("checked");
    expect(input(html, "Country")).not.toContain("checked");
    expect(html).toContain("Save as version 3");
    // Without pending edits, the version's: off.
    expect(input(render(plain, "Person"), "Enrich people")).not.toContain(
      "checked",
    );
  });

  it("stays off for a second type while one is enriched", () => {
    const enriched = named(CLUES);
    const html = render(enriched, "Company");
    expect(input(html, "Enrich people")).toContain("disabled");
    expect(html).toContain(
      "Only one type can be enriched, and Person is. Turn it off there first.",
    );
    // Once the pending edits turn Person's off, Company may be.
    const off = render(enriched, "Company", [
      { remove: { itemId: "enrichment.people" } },
    ]);
    expect(input(off, "Enrich people")).not.toContain("disabled");
    expect(off).not.toContain("Only one type can be enriched");
  });

  it("is hidden while Helix has People enrichment off", () => {
    const plain = named();
    expect(render(plain, "Person")).toContain("Enrich people");
    expect(
      render(plain, "Person", [], {
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
        enabled: true,
      }),
    ).toBe("");
    // Off Pro, or while Helix can't read the plan, it still shows.
    for (const availability of [
      "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
      "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED",
    ] as const)
      expect(render(plain, "Person", [], { availability })).toContain(
        "Enrich people",
      );
  });

  it("can't be switched on for a type without a key or names", () => {
    const plain = readAgentBundle(salesGraphBundle(SOURCE));
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    o.types.Person.key = [];
    const keyless = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
    });
    // Company has no names; Person here has no key.
    for (const [bundle, type] of [
      [plain, "Company"],
      [keyless, "Person"],
    ] as const) {
      const html = render(bundle, type);
      expect(input(html, "Enrich people")).toContain("disabled");
      expect(html).toContain("Needs a key and a name field.");
    }
    const html = render(named(), "Company");
    expect(input(html, "Enrich people")).not.toContain("disabled");
    expect(html).not.toContain("Needs a key and a name field.");
  });

  it("says when a clue is a prop no table fills, and when no type can be an employer", () => {
    // Person gains a work_email nothing fills, and Company no key.
    const def = salesGraphBundle(SOURCE);
    const o = toml.parse(readAgentBundle(def).ontologyToml) as Record<
      string,
      any
    >;
    o.types.Person.props.work_email = "string";
    o.types.Company.key = [];
    const bundle = readAgentBundle({
      ...def,
      ontology_toml: toml.stringify(o),
    });
    expect(edit.employerOptions(bundle, "Person")).toEqual([]);
    const form = edit.enrichmentDefaults(bundle, "Person");
    if (!form.on) throw new Error("on");
    expect(form.workHistory).toBeUndefined();
    const html = render(
      bundle,
      "Person",
      edit.stageEnrichment(bundle, [], "Person", {
        ...form,
        clues: { email: "work_email", linkedinUrl: "linkedin_url" },
      }),
    );
    expect(html).toContain("work_email isn’t filled by any table.");
    expect(html).not.toContain("linkedin_url isn’t filled by any table.");
    expect(html).toContain(
      "No other type is known by a web domain to link employers to.",
    );
    expect(html).not.toContain("Link each employer");
    // Company known by its domain: the employers' checkbox instead.
    const known = render(
      readAgentBundle(def),
      "Person",
      edit.stageEnrichment(
        readAgentBundle(def),
        [],
        "Person",
        edit.enrichmentDefaults(readAgentBundle(def), "Person"),
      ),
    );
    expect(known).toContain("Link each employer");
    expect(known).not.toContain("No other type is known by a web domain");
    expect(known).not.toContain("isn’t filled by any table.");
  });

  it("says loads apply no profiles while the workspace hasn't allowed it", () => {
    const plain = named();
    const on = edit.stageEnrichment(
      plain,
      [],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    const NOTICE =
      "People enrichment is off for this workspace, so loads apply no profiles. An owner can allow it in";
    const settings: wfe.FoundryEnrichment = {
      availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE",
      enabled: false,
    };
    const html = render(plain, "Person", on, settings);
    expect(html).toContain(NOTICE);
    expect(html).toContain('href="/settings/enrichment"');
    // Allowed, switched off, or before the setting is read: no note.
    expect(
      render(plain, "Person", on, { ...settings, enabled: true }),
    ).not.toContain(NOTICE);
    expect(render(plain, "Person", [], settings)).not.toContain(NOTICE);
    expect(render(plain, "Person", on)).not.toContain(NOTICE);
  });

  it("says what turning it off leaves, empty: the props it fills and its link", () => {
    /** The Sales graph with its item patched (undefined: key removed). */
    const patched = (patch: Record<string, unknown>) => {
      const def = salesGraphBundle(SOURCE, CLUES);
      const m = toml.parse(readAgentBundle(def).mappingsToml) as Record<
        string,
        any
      >;
      for (const [k, v] of Object.entries(patch))
        if (v === undefined) delete m.enrichments.people[k];
        else m.enrichments.people[k] = v;
      return readAgentBundle({ ...def, mappings_toml: toml.stringify(m) });
    };
    const off = (bundle: ReturnType<typeof readAgentBundle>) =>
      render(
        bundle,
        "Person",
        edit.stageEnrichment(bundle, [], "Person", { on: false }),
      ).match(/<p>(Lookups stop from the next load\.[^<]*)<\/p>/)?.[1];
    expect(off(patched({}))).toBe(
      "Lookups stop from the next load. current_title, location, country, profile_url and worked_at stay, empty.",
    );
    expect(off(patched({ work_history: undefined }))).toBe(
      "Lookups stop from the next load. current_title, location, country and profile_url stay, empty.",
    );
    expect(off(patched({ props: { country: "location_country" } }))).toBe(
      "Lookups stop from the next load. country and worked_at stay, empty.",
    );
    // Work history alone, or neither.
    expect(off(patched({ props: undefined }))).toBe(
      "Lookups stop from the next load. worked_at stays, empty.",
    );
    expect(off(patched({ props: undefined, work_history: undefined }))).toBe(
      "Lookups stop from the next load.",
    );
  });

  it("shows the prop a field fills even when it can't, and says so", () => {
    // The people table fills country, which the pending item fills too.
    const def = salesGraphBundle(SOURCE);
    const o = toml.parse(readAgentBundle(def).ontologyToml) as Record<
      string,
      any
    >;
    Object.assign(o.types.Person.props, { country: "string", title: "string" });
    const m = toml.parse(readAgentBundle(def).mappingsToml) as Record<
      string,
      any
    >;
    m.mappings.people.bind.person.props.country = "company";
    const taken = readAgentBundle({
      ...def,
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
    const form = edit.enrichmentDefaults(taken, "Person");
    const pending = edit.stageEnrichment(taken, [], "Person", {
      ...form,
      fields: form.fields.map((f) =>
        f.field === "location_country" ? { ...f, prop: "country" } : f,
      ),
    });
    // The Select shows the prop chosen, by its option's label.
    const shown = (html: string) =>
      html.match(
        /aria-label="Country fills"[^>]*><span class="hf-select-value[^"]*">(.*?)<\/span>/,
      )?.[1];
    expect(shown(render(taken, "Person", pending))).toBe(
      "country (unavailable)",
    );
    // Switched on afresh, it fills a prop of its own.
    expect(
      shown(
        render(
          taken,
          "Person",
          edit.stageEnrichment(taken, [], "Person", form),
        ),
      ),
    ).toBe("country_2 (new)");
  });
});

/** Settings › People enrichment, with the setting already read. */
const settingsPage = (
  e: wfe.FoundryEnrichment,
  role: WorkspaceRole = "owner",
) => {
  const qc = new QueryClient();
  qc.setQueryData(enrichmentKey("ws_1"), e);
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: qc },
      createElement(
        WorkspaceContext.Provider,
        {
          value: {
            id: "ws_1",
            slug: "acme",
            name: "Acme",
            role,
            user: { id: "usr_1", name: "Ada", email: "ada@example.com" },
            onboarding: false,
            notify: () => {},
          },
        },
        createElement(MemoryRouter, null, createElement(EnrichmentTab)),
      ),
    ),
  );
};
const ON: wfe.FoundryEnrichment = {
  availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE",
  enabled: true,
  monthlyCreditCap: "50000",
  creditsUsed: "0",
  creditsReserved: "0",
};
const PAUSED =
  "Helix has paused lookups for now. Loads still apply profiles found before.";
const UNKNOWN =
  "Helix can’t read this workspace’s plan just now. Try again later to allow People enrichment or change its cap; turning it off still works.";

describe("Settings › People enrichment", () => {
  it("offers the owner the cap on Pro", () => {
    const html = settingsPage(ON);
    expect(input(html, "Allow People enrichment")).toContain("checked");
    expect(input(html, "Allow People enrichment")).not.toContain("disabled");
    expect(html).toContain("Monthly credit cap");
    expect(html).toContain("Save cap");
    expect(html).not.toContain(PAUSED);
    expect(html).not.toContain(UNKNOWN);
  });

  it.each([
    ["off Pro", "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED", undefined],
    [
      "while Helix pauses lookups",
      "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
      PAUSED,
    ],
    [
      "while Helix can't read the plan",
      "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED",
      UNKNOWN,
    ],
    ["with no availability", undefined, UNKNOWN],
  ] as const)("lets the owner only turn it off %s", (_, availability, note) => {
    // On: the switch turns it off; no cap to change.
    const on = settingsPage({ ...ON, availability });
    expect(input(on, "Allow People enrichment")).toContain("checked");
    expect(input(on, "Allow People enrichment")).not.toContain("disabled");
    expect(on).not.toContain("Monthly credit cap");
    expect(on).not.toContain("Save cap");
    if (note) expect(on).toContain(note);
    expect(on.includes("People enrichment needs Foundry Pro")).toBe(
      availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
    );
    // Off: nothing to change, and the page says why.
    const off = settingsPage({ ...ON, availability, enabled: false });
    expect(input(off, "Allow People enrichment")).toContain("disabled");
    expect(off).not.toContain(ENRICHMENT_NOTICE);
    if (note) expect(off).toContain(note);
  });

  it("tells admins and members whether it is on, and Helix's pause", () => {
    for (const role of ["admin", "member"] as const) {
      const html = settingsPage(
        { ...ON, availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED" },
        role,
      );
      expect(html).toContain(
        "On · cap 50,000 · 0 used. Only owners can change this.",
      );
      expect(html).toContain(PAUSED);
      expect(html).not.toContain("Allow People enrichment<");
      expect(
        settingsPage(
          {
            ...ON,
            availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED",
          },
          role,
        ),
      ).not.toContain(UNKNOWN);
    }
  });
});
