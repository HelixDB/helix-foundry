import { describe, expect, it } from "vitest";
import * as toml from "smol-toml";
import type * as wfe from "@/shared/wfe";
import * as edit from "@/web/areas/ontology/edit-model";
import {
  EXPERIENCE_FIELDS,
  PROFILE_DEFAULTS,
  PROFILE_FIELDS,
  UNMAPPABLE_FIELDS,
} from "@/web/areas/ontology/enrichment-fields";
import { readAgentBundle } from "@/web/areas/ontology/model";
import { PEOPLE, salesGraphBundle } from "../../mock/fixtures/sales-graph";
import { sourceId } from "../../mock/routes/ontology-agent";

// The version editor's People enrichment: the edits the Enrich people form
// stages (one merged type put, a new link, the item) and the boundary they
// pass, as Revise checks it.

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
const plain = readAgentBundle(salesGraphBundle(SOURCE));
const enriched = readAgentBundle(salesGraphBundle(SOURCE, CLUES));
const put = (itemId: string, source: string): wfe.OntologyEdit => ({
  put: { itemId, toml: source },
});
const remove = (itemId: string): wfe.OntologyEdit => ({
  remove: { itemId },
});
/** Every edit of a batch passes, within the batch. */
const problems = (bundle: typeof plain, batch: wfe.OntologyEdit[]) =>
  batch.map((e) => edit.validateEdit(bundle, e, batch)).filter(Boolean);
const doc = (e: wfe.OntologyEdit | undefined) =>
  toml.parse(e?.put?.toml || "") as Record<string, any>;
const ITEM = enriched.mappingsToml.slice(
  enriched.mappingsToml.indexOf("[enrichments.people]"),
);
/** The enrichment item with one key replaced (undefined: removed). */
const item = (patch: Record<string, unknown>) => {
  const value = (toml.parse(ITEM).enrichments as any).people;
  for (const [k, v] of Object.entries(patch))
    if (v === undefined) delete value[k];
    else value[k] = v;
  return put(
    "enrichment.people",
    toml.stringify({ enrichments: { people: value } }),
  );
};

describe("the catalog", () => {
  // The engine's sources/src/builtin.rs (people-enrichment a8bf30f), field by
  // field and in its order: no catalog is vendored, so this copy pins it.
  it("holds the built-in catalog's mappable fields, and never the lookup's own", () => {
    // FIELDS whose `used` is Use::Props, with their types.
    expect(Object.entries(PROFILE_FIELDS)).toEqual([
      ["full_name", "string"],
      ["first_name", "string"],
      ["last_name", "string"],
      ["current_title", "string"],
      ["current_role_status", "string"],
      ["current_org_name", "string"],
      ["current_org_domain", "string"],
      ["linkedin_url", "string"],
      ["location", "string"],
      ["location_city", "string"],
      ["location_region", "string"],
      ["location_country", "string"],
    ]);
    // FIELDS whose `used` is Use::Never, but `experience`, which an item
    // maps through work_history.
    expect(UNMAPPABLE_FIELDS).toEqual([
      "profile_id",
      "clue_email",
      "clue_linkedin_url",
      "clue_name",
      "matched_on",
      "score",
      "last_updated",
      "checked_at",
      "fetched_at",
      "load_id",
      "call_id",
      "provider_request_id",
    ]);
    // EXPERIENCE_FIELDS, every one Use::WorkHistory.
    expect(Object.entries(EXPERIENCE_FIELDS)).toEqual(
      Object.entries({
        id: "integer",
        title: "string",
        "/organization/name": "string",
        "/organization/domain": "string",
        location: "string",
        start_date: "string",
        end_date: "string",
        is_current: "boolean",
      }),
    );
    for (const f of UNMAPPABLE_FIELDS) {
      expect(Object.hasOwn(PROFILE_FIELDS, f)).toBe(false);
      expect(Object.hasOwn(EXPERIENCE_FIELDS, f)).toBe(false);
    }
    for (const d of PROFILE_DEFAULTS)
      expect(Object.hasOwn(PROFILE_FIELDS, d.field)).toBe(true);
  });
});

describe("the Enrich people form", () => {
  it("lists the props that may hold a clue, and where employers match", () => {
    expect(edit.clueOptions(plain, "Person")).toEqual([
      "contact_id",
      "email",
      "linkedin_url",
    ]);
    expect(edit.clueOptions(plain, "Nobody")).toEqual([]);
    expect(edit.employerOptions(plain, "Person")).toEqual([
      { type: "Company", match: ["domain"] },
    ]);
    expect(edit.createdProps(plain, "Person")).toEqual(
      new Set(["contact_id", "email", "full_name", "linkedin_url"]),
    );
    expect(edit.createdProps(plain, "Company")).toEqual(new Set(["domain"]));
  });

  it("offers the props a field may fill, and the links work history may state", () => {
    const clues = { email: "email", linkedinUrl: "linkedin_url" };
    // contact_id is the key, full_name a name, email and linkedin_url clues.
    expect(edit.targetOptions(plain, "Person", clues)).toEqual([]);
    expect(edit.targetOptions(enriched, "Person", clues)).toEqual([
      "current_title",
      "location",
      "country",
      "profile_url",
    ]);
    // A prop a table fills isn't offered.
    const shared = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      mappings_toml: enriched.mappingsToml.replace(
        'full_name = "full_name"',
        'full_name = "full_name"\nlocation = "city"',
      ),
    });
    expect(edit.targetOptions(shared, "Person", { email: "email" })).toEqual([
      "current_title",
      "country",
      "profile_url",
    ]);
    expect(edit.targetOptions(plain, "Nobody", {})).toEqual([]);
    // works_at is one-valued and a mapping states it.
    expect(edit.historyLinks(plain, "Person", "Company")).toEqual([]);
    expect(edit.historyLinks(enriched, "Person", "Company")).toEqual([
      "worked_at",
    ]);
    expect(edit.historyLinks(enriched, "Company", "Person")).toEqual([]);
  });

  it("reads a version's item, and is off without one", () => {
    expect(edit.enrichmentForm(enriched, "Person")).toEqual({
      on: true,
      clues: { email: "email", linkedinUrl: "linkedin_url" },
      fields: [
        { field: "current_title", prop: "current_title" },
        { field: "location", prop: "location" },
        { field: "location_country", prop: "country" },
        { field: "linkedin_url", prop: "profile_url" },
      ],
      workHistory: { link: "worked_at", to: "Company", match: "domain" },
    });
    expect(edit.enrichmentForm(enriched, "Company")).toEqual({ on: false });
    expect(edit.enrichmentForm(plain, "Person")).toEqual({ on: false });
  });

  it("switches on with the type's clues, every default field and its employers", () => {
    expect(edit.enrichmentDefaults(plain, "Person")).toEqual({
      on: true,
      clues: { email: "email", linkedinUrl: "linkedin_url" },
      fields: PROFILE_DEFAULTS.map((d) => ({ field: d.field, prop: d.prop })),
      workHistory: { link: "worked_at", to: "Company", match: "domain" },
    });
    // Nothing to match employers by, and no clue-like props.
    expect(edit.enrichmentDefaults(plain, "Company")).toEqual({
      on: true,
      clues: { email: undefined, linkedinUrl: undefined },
      fields: PROFILE_DEFAULTS.map((d) => ({ field: d.field, prop: d.prop })),
      workHistory: undefined,
    });
  });

  it("stages one merged type put, the new link and the item, which Revise accepts", () => {
    const batch = edit.stageEnrichment(
      plain,
      [],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    expect(batch.map((e) => edit.editItem(e))).toEqual([
      "type.Person",
      "link.worked_at",
      "enrichment.people",
    ]);
    expect(problems(plain, batch)).toEqual([]);
    // The type gains the props the item fills and the clues as identifiers,
    // and keeps everything else.
    expect(doc(batch[0]).types.Person).toEqual(
      (toml.parse(enriched.ontologyToml).types as any).Person,
    );
    expect(doc(batch[1]).links.worked_at).toEqual(
      (toml.parse(enriched.ontologyToml).links as any).worked_at,
    );
    expect(doc(batch[2])).toEqual(toml.parse(ITEM));
  });

  it("merges with a pending type put, and a later type edit keeps the new props", () => {
    const ids = edit.identifierEdit(plain, "Person", {
      email: "email",
      contact_id: "exact",
    });
    const batch = edit.stageEnrichment(
      plain,
      [ids],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    expect(
      batch.filter((e) => edit.editItem(e) === "type.Person"),
    ).toHaveLength(1);
    const person = doc(batch.find((e) => edit.editItem(e) === "type.Person"))
      .types.Person;
    expect(person.identifiers).toEqual({
      email: "email",
      contact_id: "exact",
      linkedin_url: "exact",
    });
    expect(person.props.current_title).toBe("string");
    expect(problems(plain, batch)).toEqual([]);
    // The identifiers editor builds from the pending put.
    const again = edit.identifierEdit(
      plain,
      "Person",
      { email: "email", linkedin_url: "exact" },
      batch,
    );
    expect(doc(again).types.Person.props.country).toBe("string");
    expect(
      edit.validateEdit(plain, again, edit.stageEdit(plain, batch, again)),
    ).toBeUndefined();
  });

  it("stages nothing for an item left as it is, and only what changed", () => {
    const form = edit.enrichmentForm(enriched, "Person");
    expect(edit.stageEnrichment(enriched, [], "Person", form)).toEqual([]);
    if (!form.on) throw new Error("on");
    const fewer = edit.stageEnrichment(enriched, [], "Person", {
      ...form,
      fields: form.fields.filter((f) => f.prop !== "country"),
    });
    expect(fewer.map((e) => edit.editItem(e))).toEqual(["enrichment.people"]);
    expect(doc(fewer[0]).enrichments.people.props).not.toHaveProperty(
      "country",
    );
    expect(problems(enriched, fewer)).toEqual([]);
  });

  it("stages nothing for an item without props, and writes them only when they map something", () => {
    // The engine reads a missing props table as empty: an employer-only
    // item may have none, or none for its work history.
    const without = (drop: (e: any) => void) => {
      const mappings = toml.parse(enriched.mappingsToml) as any;
      drop(mappings.enrichments.people);
      return readAgentBundle({
        ...salesGraphBundle(SOURCE, CLUES),
        mappings_toml: toml.stringify(mappings),
      });
    };
    const bare = [
      without((e) => delete e.props),
      without((e) => delete e.work_history.props),
      without((e) => {
        delete e.props;
        delete e.work_history.props;
      }),
    ];
    for (const b of bare) {
      const form = edit.enrichmentForm(b, "Person");
      expect(edit.stageEnrichment(b, [], "Person", form)).toEqual([]);
    }
    // An empty table the version has stays, as it is.
    const empty = without((e) => {
      e.props = {};
      e.work_history.props = {};
    });
    expect(
      edit.stageEnrichment(
        empty,
        [],
        "Person",
        edit.enrichmentForm(empty, "Person"),
      ),
    ).toEqual([]);
    // A field mapped writes props; dropping the last one leaves the table
    // as the version has it.
    const [noProps] = bare,
      form = edit.enrichmentForm(noProps, "Person");
    if (!form.on) throw new Error("on");
    const one = edit.stageEnrichment(noProps, [], "Person", {
      ...form,
      fields: [{ field: "current_title", prop: "current_title" }],
    });
    expect(doc(one.at(-1)).enrichments.people.props).toEqual({
      current_title: "current_title",
    });
    expect(problems(noProps, one)).toEqual([]);
    const none = edit.stageEnrichment(enriched, [], "Person", {
      ...form,
      fields: [],
    });
    expect(doc(none.at(-1)).enrichments.people.props).toEqual({});
    expect(problems(enriched, none)).toEqual([]);
  });

  it("maps an existing link's own experience props only", () => {
    // worked_at is in the ontology already, and no item states it yet.
    const narrow = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: enriched.ontologyToml.replace(
        'title = "string"\nstart_date = "string"\nend_date = "string"\nis_current = "boolean"',
        'title = "string"\nis_current = "string"',
      ),
    });
    const form = edit.enrichmentDefaults(narrow, "Person");
    expect(form.workHistory?.link).toBe("worked_at");
    const batch = edit.stageEnrichment(narrow, [], "Person", form);
    expect(batch.map((e) => edit.editItem(e))).toEqual(["enrichment.people"]);
    expect(doc(batch[0]).enrichments.people.work_history.props).toEqual({
      title: "title",
    });
  });

  it("drops a new link it no longer uses", () => {
    const form = edit.enrichmentDefaults(plain, "Person");
    const first = edit.stageEnrichment(plain, [], "Person", form);
    const without = edit.stageEnrichment(plain, first, "Person", {
      ...form,
      workHistory: undefined,
    });
    expect(without.map((e) => edit.editItem(e))).toEqual([
      "type.Person",
      "enrichment.people",
    ]);
    expect(doc(without[1]).enrichments.people).not.toHaveProperty(
      "work_history",
    );
    expect(problems(plain, without)).toEqual([]);
  });

  it("turns off by removing the item only", () => {
    const off = edit.stageEnrichment(enriched, [], "Person", { on: false });
    expect(off).toEqual([remove("enrichment.people")]);
    expect(problems(enriched, off)).toEqual([]);
    // Another type's switch leaves Person's item, and an unknown type
    // stages nothing.
    expect(
      edit.stageEnrichment(enriched, [], "Company", { on: false }),
    ).toEqual([]);
    expect(edit.stageEnrichment(enriched, [], "Nobody", { on: false })).toEqual(
      [],
    );
    // Off before it was saved drops what it staged: the item, the new link,
    // the props it added and the identifiers it declared for its clues.
    const pending = edit.stageEnrichment(
      plain,
      [],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    expect(
      edit.stageEnrichment(plain, pending, "Person", { on: false }),
    ).toEqual([]);
    // A hand-staged identifier stays.
    const ids = edit.identifierEdit(plain, "Person", {
      email: "email",
      contact_id: "exact",
    });
    const dropped = edit.stageEnrichment(
      plain,
      edit.stageEnrichment(
        plain,
        [ids],
        "Person",
        edit.enrichmentDefaults(plain, "Person"),
      ),
      "Person",
      { on: false },
    );
    expect(dropped).toEqual([ids]);
    expect(problems(plain, dropped)).toEqual([]);
    // Off for a version's item after a try that added a prop: only the
    // removal, which Revise accepts.
    const on = edit.enrichmentForm(enriched, "Person");
    if (!on.on) throw new Error("on");
    const more = edit.stageEnrichment(enriched, [], "Person", {
      ...on,
      fields: [...on.fields, { field: "current_org_name", prop: "employer" }],
    });
    expect(doc(more[0]).types.Person.props.employer).toBe("string");
    const removed = edit.stageEnrichment(enriched, more, "Person", {
      on: false,
    });
    expect(removed).toEqual([remove("enrichment.people")]);
    expect(problems(enriched, removed)).toEqual([]);
  });

  it("undoes the item, or its new link, leaving a batch Revise accepts", () => {
    const rule = put(
      "rule.everyone",
      "[rules.everyone]\ndescription = \"Every person.\"\ndatalog = 'everyone(p) :- Person(p).'",
    );
    const pending = edit.stageEnrichment(
      plain,
      [rule],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    expect(pending.map((e) => edit.editItem(e))).toEqual([
      "rule.everyone",
      "type.Person",
      "link.worked_at",
      "enrichment.people",
    ]);
    // Dropped alone, the item leaves props and a link nothing fills.
    expect(
      problems(
        plain,
        pending.filter((e) => edit.editItem(e) !== "enrichment.people"),
      ),
    ).toEqual([
      "EditNotAllowed: current_title is new; a type only gains the props its People enrichment fills.",
      "EditNotAllowed: a new link must be the one People enrichment's work history adds.",
    ]);
    // Undone, it takes them with it, and the identifiers it declared.
    expect(edit.undoEdit(plain, pending, "enrichment.people")).toEqual([rule]);
    // The link's undo keeps the item, without work history.
    const unlinked = edit.undoEdit(plain, pending, "link.worked_at");
    expect(unlinked.map((e) => edit.editItem(e)).sort()).toEqual([
      "enrichment.people",
      "rule.everyone",
      "type.Person",
    ]);
    expect(
      doc(unlinked.find((e) => edit.editItem(e) === "enrichment.people"))
        .enrichments.people,
    ).not.toHaveProperty("work_history");
    expect(problems(plain, unlinked)).toEqual([]);
    // The type's undo keeps what the item needs of it: its props and clue
    // identifiers, and none staged by hand.
    const ids = edit.identifierEdit(plain, "Person", {
      email: "email",
      contact_id: "exact",
    });
    const withIds = edit.stageEnrichment(
      plain,
      [rule, ids],
      "Person",
      edit.enrichmentDefaults(plain, "Person"),
    );
    expect(
      doc(withIds.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual({ email: "email", contact_id: "exact", linkedin_url: "exact" });
    for (const batch of [pending, withIds]) {
      const untyped = edit.undoEdit(plain, batch, "type.Person");
      expect(untyped).toEqual(pending);
      expect(problems(plain, untyped)).toEqual([]);
    }
    // Any other undo drops that edit only.
    expect(edit.undoEdit(plain, pending, "rule.everyone")).toEqual(
      pending.slice(1),
    );
    // Another type's undo drops that edit only.
    const company = edit.identifierEdit(plain, "Company", { domain: "exact" });
    expect(edit.undoEdit(plain, [company, ...pending], "type.Company")).toEqual(
      pending,
    );
    // A hand-staged identifier stays.
    expect(
      edit.undoEdit(
        plain,
        edit.stageEnrichment(
          plain,
          [ids],
          "Person",
          edit.enrichmentDefaults(plain, "Person"),
        ),
        "enrichment.people",
      ),
    ).toEqual([ids]);
    // A version's item comes back as it was, after a try or a removal.
    const on = edit.enrichmentForm(enriched, "Person");
    if (!on.on) throw new Error("on");
    const more = edit.stageEnrichment(enriched, [], "Person", {
      ...on,
      fields: [...on.fields, { field: "current_org_name", prop: "employer" }],
    });
    expect(more.map((e) => edit.editItem(e))).toEqual([
      "type.Person",
      "enrichment.people",
    ]);
    expect(edit.undoEdit(enriched, more, "enrichment.people")).toEqual([]);
    // The type's undo there keeps the prop the try's item fills, and drops
    // identifiers staged by hand alone.
    expect(edit.undoEdit(enriched, more, "type.Person")).toEqual(more);
    expect(
      edit.undoEdit(
        enriched,
        [
          edit.identifierEdit(enriched, "Person", {
            email: "email",
            linkedin_url: "exact",
            contact_id: "exact",
          }),
        ],
        "type.Person",
      ),
    ).toEqual([]);
    expect(
      edit.undoEdit(
        enriched,
        [remove("enrichment.people")],
        "enrichment.people",
      ),
    ).toEqual([]);
  });

  it("takes a try back to the identifiers staged by hand on its clues", () => {
    const rule = put(
      "rule.everyone",
      "[rules.everyone]\ndescription = \"Every person.\"\ndatalog = 'everyone(p) :- Person(p).'",
    );
    // LinkedIn URL identified by hand, then the default try, whose LinkedIn
    // clue is that prop.
    const hand = edit.identifierEdit(plain, "Person", {
      email: "email",
      linkedin_url: "exact",
    });
    const before = edit.handIdentifiers(plain, [rule, hand], "Person");
    expect(before).toEqual({ email: "email", linkedin_url: "exact" });
    const form = edit.enrichmentDefaults(plain, "Person");
    if (!form.on) throw new Error("on");
    expect(form.clues.linkedinUrl).toBe("linkedin_url");
    const tried = edit.stageEnrichment(
      plain,
      [rule, hand],
      "Person",
      form,
      before,
    );
    expect(tried.map((e) => edit.editItem(e))).toEqual([
      "rule.everyone",
      "type.Person",
      "link.worked_at",
      "enrichment.people",
    ]);
    // The edits alone can't tell it from the try's.
    expect(edit.handIdentifiers(plain, tried, "Person")).toEqual({
      email: "email",
    });
    expect(edit.handIdentifiers(plain, tried, "Person", before)).toEqual(
      before,
    );
    // Off, or the item undone, the hand-staged put comes back.
    expect(
      edit.stageEnrichment(plain, tried, "Person", { on: false }, before),
    ).toEqual([rule, hand]);
    expect(edit.undoEdit(plain, tried, "enrichment.people", before)).toEqual([
      rule,
      hand,
    ]);
    expect(problems(plain, [rule, hand])).toEqual([]);
    // The link's undo keeps it too, and the type's drops it.
    const unlinked = edit.undoEdit(plain, tried, "link.worked_at", before);
    expect(
      doc(unlinked.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual(before);
    expect(edit.undoEdit(plain, tried, "type.Person", before)).toEqual(
      edit.stageEnrichment(plain, [rule], "Person", form),
    );
    // A later try with another LinkedIn clue keeps it as staged.
    const email = edit.stageEnrichment(
      plain,
      tried,
      "Person",
      { ...form, clues: { email: "email" } },
      before,
    );
    expect(
      doc(email.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual(before);
    // Identified by hand after a try that didn't use it as a clue, then
    // used as one: what the page keeps, read before that try is saved.
    const first = edit.stageEnrichment(plain, [rule], "Person", {
      ...form,
      clues: { email: "email" },
    });
    const later = edit.stageEdit(
      plain,
      first,
      edit.identifierEdit(
        plain,
        "Person",
        { email: "email", linkedin_url: "exact" },
        first,
      ),
    );
    const kept = edit.handIdentifiers(
      plain,
      later,
      "Person",
      edit.handIdentifiers(plain, [rule], "Person"),
    );
    expect(kept).toEqual({ email: "email", linkedin_url: "exact" });
    const second = edit.stageEnrichment(plain, later, "Person", form, kept);
    expect(
      edit.stageEnrichment(plain, second, "Person", { on: false }, kept),
    ).toEqual([
      rule,
      edit.identifierEdit(plain, "Person", {
        email: "email",
        linkedin_url: "exact",
      }),
    ]);
  });

  it("keeps only the props a later try still fills", () => {
    const form = edit.enrichmentDefaults(plain, "Person");
    if (!form.on) throw new Error("on");
    const first = edit.stageEnrichment(plain, [], "Person", form);
    const fewer = edit.stageEnrichment(plain, first, "Person", {
      ...form,
      fields: form.fields.filter((f) => f.prop !== "country"),
    });
    const props = doc(fewer.find((e) => edit.editItem(e) === "type.Person"))
      .types.Person.props;
    expect(props).not.toHaveProperty("country");
    expect(props.current_title).toBe("string");
    expect(problems(plain, fewer)).toEqual([]);
  });

  it("declares only the clues the latest try uses as identifiers", () => {
    const form = edit.enrichmentDefaults(plain, "Person");
    // A refused try took the email from the wrong field.
    const first = edit.stageEnrichment(plain, [], "Person", {
      ...form,
      clues: { email: "linkedin_url" },
    });
    expect(
      doc(first.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual({ email: "email", linkedin_url: "email" });
    const fixed = edit.stageEnrichment(plain, first, "Person", {
      ...form,
      clues: { email: "email" },
    });
    expect(
      doc(fixed.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual({ email: "email" });
    expect(fixed).toEqual(
      edit.stageEnrichment(plain, [], "Person", {
        ...form,
        clues: { email: "email" },
      }),
    );
    expect(problems(plain, fixed)).toEqual([]);
    // The version's own identifiers stay as it has them, whatever the
    // tries named: the type is left as it is.
    const wrong = edit.stageEnrichment(enriched, [], "Person", {
      ...form,
      clues: { email: "linkedin_url" },
    });
    expect(
      doc(wrong.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual({ email: "email", linkedin_url: "email" });
    const own = edit.stageEnrichment(enriched, wrong, "Person", {
      ...form,
      clues: { email: "email" },
    });
    expect(own.map((e) => edit.editItem(e))).toEqual(["enrichment.people"]);
    expect(problems(enriched, own)).toEqual([]);
  });

  it.each([
    ["an email", { email: "email" }, { email: "email" }],
    [
      "a LinkedIn URL",
      { linkedinUrl: "linkedin_url" },
      { linkedin_url: "linkedin_url" },
    ],
  ])("stages an item with only %s to look people up by", (_, clues, item) => {
    const batch = edit.stageEnrichment(plain, [], "Person", {
      ...edit.enrichmentDefaults(plain, "Person"),
      clues,
    });
    expect(
      doc(batch.find((e) => edit.editItem(e) === "enrichment.people"))
        .enrichments.people.clues,
    ).toEqual(item);
    // Person identifies by email already; a LinkedIn clue adds its own.
    expect(
      doc(batch.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .identifiers,
    ).toEqual(
      "email" in clues
        ? { email: "email" }
        : { email: "email", linkedin_url: "exact" },
    );
    expect(problems(plain, batch)).toEqual([]);
    expect(edit.enrichmentForm(plain, "Person", batch)).toMatchObject({
      on: true,
      clues,
    });
  });

  it("reads the form as the pending edits leave it", () => {
    const form = edit.enrichmentDefaults(plain, "Person");
    const pending = edit.stageEnrichment(plain, [], "Person", form);
    expect(edit.enrichmentForm(plain, "Person", pending)).toEqual(form);
    expect(edit.enrichmentForm(plain, "Company", pending)).toEqual({
      on: false,
    });
    expect(
      edit.enrichmentForm(enriched, "Person", [remove("enrichment.people")]),
    ).toEqual({ on: false });
  });

  it("knows which type the file's one item enriches", () => {
    expect(edit.enrichedType(enriched)).toBe("Person");
    expect(edit.enrichedType(plain)).toBeUndefined();
    expect(
      edit.enrichedType(enriched, [remove("enrichment.people")]),
    ).toBeUndefined();
    expect(
      edit.enrichedType(
        plain,
        edit.stageEnrichment(
          plain,
          [],
          "Person",
          edit.enrichmentDefaults(plain, "Person"),
        ),
      ),
    ).toBe("Person");
  });

  /** The plain Sales graph with a worked_at link of its own. */
  const withWorkedAt = (link: Record<string, unknown>, statedBy = false) => {
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    o.links.worked_at = {
      from: "Person",
      to: "Company",
      description: "A role.",
      props: { title: "string" },
      ...link,
    };
    const m = toml.parse(plain.mappingsToml) as Record<string, any>;
    if (statedBy)
      m.mappings.people.links.push({
        link: "worked_at",
        from: "person",
        to: "company",
      });
    return readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
  };
  it.each([
    ["one-valued", withWorkedAt({ cardinality: "one" })],
    ["required", withWorkedAt({ required: true })],
    ["from another type", withWorkedAt({ from: "Company", to: "Person" })],
    ["declared", withWorkedAt({ inverse_of: "works_at" })],
    ["stated by a table", withWorkedAt({}, true)],
  ])("switches on without work history when worked_at is %s", (_, bundle) => {
    expect(edit.historyLinks(bundle, "Person", "Company")).toEqual([]);
    expect(edit.historyLink(bundle, "Person", "Company")).toBeUndefined();
    const form = edit.enrichmentDefaults(bundle, "Person");
    expect(form.workHistory).toBeUndefined();
    expect(
      problems(bundle, edit.stageEnrichment(bundle, [], "Person", form)),
    ).toEqual([]);
  });

  it("fills a default field into a prop of its own when a table fills its name", () => {
    // Person gains country and title; the people table fills country.
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    Object.assign(o.types.Person.props, {
      country: "string",
      title: "string",
    });
    const m = toml.parse(plain.mappingsToml) as Record<string, any>;
    m.mappings.people.bind.person.props.country = "company";
    const taken = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
    const clues = { email: "email", linkedinUrl: "linkedin_url" };
    expect(edit.targetOptions(taken, "Person", clues)).toEqual(["title"]);
    const form = edit.enrichmentDefaults(taken, "Person");
    expect(form.fields).toEqual([
      { field: "current_title", prop: "current_title" },
      { field: "location", prop: "location" },
      { field: "location_country", prop: "country_2" },
      { field: "linkedin_url", prop: "profile_url" },
    ]);
    const batch = edit.stageEnrichment(taken, [], "Person", form);
    expect(problems(taken, batch)).toEqual([]);
    expect(
      doc(batch.find((e) => edit.editItem(e) === "type.Person")).types.Person
        .props.country_2,
    ).toBe("string");
    // A prop of that name the item may fill is its own.
    o.types.Person.props.country_2 = "string";
    const fillable = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
    expect(edit.enrichmentDefaults(fillable, "Person").fields[2]).toEqual({
      field: "location_country",
      prop: "country_2",
    });
    // An identity prop of that name isn't.
    o.types.Person.identifiers = { email: "email", country_2: "exact" };
    const identity = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
    expect(edit.enrichmentDefaults(identity, "Person").fields[2]).toEqual({
      field: "location_country",
      prop: "country_3",
    });
  });

  it("offers a field the props it may fill, and one it can't only while it fills it", () => {
    // Person gains country and title; the people table fills country.
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    Object.assign(o.types.Person.props, {
      country: "string",
      title: "string",
    });
    const m = toml.parse(plain.mappingsToml) as Record<string, any>;
    m.mappings.people.bind.person.props.country = "company";
    const taken = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
      mappings_toml: toml.stringify(m),
    });
    const clues = { email: "email", linkedinUrl: "linkedin_url" };
    const NEW = {
        value: "country_2",
        label: "country_2 (new)",
        disabled: false,
      },
      TITLE = { value: "title", label: "title", disabled: false };
    // Into a prop a table fills: shown, unavailable, never picked.
    expect(
      edit.fieldOptions(taken, "Person", clues, "country_2", "country"),
    ).toEqual([
      { value: "country", label: "country (unavailable)", disabled: true },
      NEW,
      TITLE,
    ]);
    // Into its own prop, that one isn't offered.
    expect(
      edit.fieldOptions(taken, "Person", clues, "country_2", "country_2"),
    ).toEqual([NEW, TITLE]);
    // Into a new prop of another name: that one and its default, both new.
    expect(
      edit.fieldOptions(taken, "Person", clues, "country_2", "nation"),
    ).toEqual([
      NEW,
      { value: "nation", label: "nation (new)", disabled: false },
      TITLE,
    ]);
    // A clue identifies the type: never offered, unavailable while chosen.
    expect(
      edit
        .fieldOptions(taken, "Person", clues, "country_2", "email")
        .find((x) => x.value === "email"),
    ).toEqual({ value: "email", label: "email (unavailable)", disabled: true });
    // Once the version has the prop the item fills, it is offered as it is.
    expect(
      edit.fieldOptions(enriched, "Person", clues, "location", "location"),
    ).toEqual(
      ["current_title", "location", "country", "profile_url"].map((p) => ({
        value: p,
        label: p,
        disabled: false,
      })),
    );
  });

  it("takes the LinkedIn URL from a prop named so, not any exact identifier", () => {
    // crm_id is an exact identifier the table fills, beside linkedin_url.
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    o.types.Person.props.crm_id = "string";
    o.types.Person.identifiers = { crm_id: "exact", email: "email" };
    const m = toml.parse(plain.mappingsToml) as Record<string, any>;
    m.mappings.people.bind.person.props.crm_id = "contact_id";
    const bundle = (ontology: Record<string, any>) =>
      readAgentBundle({
        ...salesGraphBundle(SOURCE),
        ontology_toml: toml.stringify(ontology),
        mappings_toml: toml.stringify(m),
      });
    expect(edit.enrichmentDefaults(bundle(o), "Person").clues).toEqual({
      email: "email",
      linkedinUrl: "linkedin_url",
    });
    // A LinkedIn-named prop no table fills gives way to one that is an
    // exact identifier, then to any.
    o.types.Person.props.linkedin_profile = "string";
    o.types.Person.identifiers.linkedin_profile = "exact";
    delete m.mappings.people.bind.person.props.linkedin_url;
    expect(edit.enrichmentDefaults(bundle(o), "Person").clues.linkedinUrl).toBe(
      "linkedin_profile",
    );
    delete o.types.Person.identifiers.linkedin_profile;
    expect(edit.enrichmentDefaults(bundle(o), "Person").clues.linkedinUrl).toBe(
      "linkedin_url",
    );
    // No prop named like a LinkedIn URL: none, whatever is exact.
    o.types.Person.props = {
      contact_id: "string",
      full_name: "string",
      email: "string",
      crm_id: "string",
    };
    expect(
      edit.enrichmentDefaults(bundle(o), "Person").clues.linkedinUrl,
    ).toBeUndefined();
  });

  it.each([
    ["one-valued", withWorkedAt({ cardinality: "one" })],
    ["stated by a table", withWorkedAt({}, true)],
  ])(
    "refuses work history when worked_at is %s and no other link can hold it",
    (_, bundle) => {
      const form = edit.enrichmentDefaults(bundle, "Person");
      // Link each employer ticked: no link is offered.
      expect(edit.historyLink(bundle, "Person", "Company")).toBeUndefined();
      const batch = edit.stageEnrichment(bundle, [], "Person", {
        ...form,
        workHistory: { to: "Company", match: "domain" },
      });
      expect(batch.map((e) => edit.editItem(e))).toEqual([
        "type.Person",
        "enrichment.people",
      ]);
      expect(doc(batch[1]).enrichments.people.work_history).not.toHaveProperty(
        "link",
      );
      expect(problems(bundle, batch)).toEqual([
        "EnrichmentLink: no link can hold work history.",
      ]);
    },
  );

  it("links employers through worked_at, new or as it is", () => {
    expect(edit.historyLink(plain, "Person", "Company")).toBe("worked_at");
    const fit = withWorkedAt({});
    expect(edit.historyLink(fit, "Person", "Company")).toBe("worked_at");
    expect(edit.enrichmentDefaults(fit, "Person").workHistory).toEqual({
      link: "worked_at",
      to: "Company",
      match: "domain",
    });
  });

  it("links employers to the type a domain matches, not the first by name", () => {
    // Account, keyed by a text id, sorts before Company.
    const o = toml.parse(plain.ontologyToml) as Record<string, any>;
    o.types.Account = {
      description: "An account, by its id.",
      props: { account_id: "string" },
      key: ["account_id"],
    };
    const accounts = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
    });
    expect(edit.employerOptions(accounts, "Person")).toEqual([
      { type: "Account", match: ["account_id"] },
      { type: "Company", match: ["domain"] },
    ]);
    const company = { to: "Company", match: "domain", link: "worked_at" };
    // Link each employer, ticked: the defaults' employer.
    expect(edit.employerHistory(accounts, "Person")).toEqual(company);
    expect(edit.enrichmentDefaults(accounts, "Person").workHistory).toEqual(
      company,
    );
    // Another employer picked: matched by its own key.
    expect(edit.employerHistory(accounts, "Person", "Account")).toEqual({
      to: "Account",
      match: "account_id",
      link: "worked_at",
    });
    expect(edit.employerHistory(accounts, "Person", "Company")).toEqual(
      company,
    );
    // No type known by a domain: the first employer, and no defaults.
    delete o.types.Company;
    o.links = {};
    const only = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
    });
    expect(edit.employerHistory(only, "Person")).toEqual({
      to: "Account",
      match: "account_id",
      link: "worked_at",
    });
    expect(edit.enrichmentDefaults(only, "Person").workHistory).toBeUndefined();
    // Not that employer, or none at all.
    expect(edit.employerHistory(only, "Person", "Company")).toBeUndefined();
    delete o.types.Account;
    const alone = readAgentBundle({
      ...salesGraphBundle(SOURCE),
      ontology_toml: toml.stringify(o),
    });
    expect(edit.employerHistory(alone, "Person")).toBeUndefined();
  });
});

describe("the boundary", () => {
  const batch = edit.stageEnrichment(
    plain,
    [],
    "Person",
    edit.enrichmentDefaults(plain, "Person"),
  );
  const [typePut, linkPut, itemPut] = batch as [
    wfe.OntologyEdit,
    wfe.OntologyEdit,
    wfe.OntologyEdit,
  ];

  it("lists the item among the file's items, and accepts it unchanged", () => {
    const items = edit.editItems(enriched);
    expect(items["enrichment.people"]).toContain("work_history");
    expect(items["link.worked_at"]).toContain("Person");
    for (const [id, source] of Object.entries(items))
      expect(edit.validateEdit(enriched, put(id, source)), id).toBeUndefined();
  });

  it("refuses new type props the item doesn't fill, and any change to existing ones", () => {
    expect(edit.validateEdit(plain, typePut)).toContain("EditNotAllowed");
    expect(edit.validateEdit(plain, typePut, [itemPut])).toBeUndefined();
    const person = doc(typePut).types.Person;
    const stray = put(
      "type.Person",
      toml.stringify({
        types: {
          Person: { ...person, props: { ...person.props, x: "string" } },
        },
      }),
    );
    expect(edit.validateEdit(plain, stray, [itemPut])).toBe(
      "EditNotAllowed: x is new; a type only gains the props its People enrichment fills.",
    );
    const { email: _, ...without } = person.props;
    for (const props of [without, { ...person.props, email: "integer" }])
      expect(
        edit.validateEdit(
          plain,
          put(
            "type.Person",
            toml.stringify({ types: { Person: { ...person, props } } }),
          ),
          [itemPut],
        ),
      ).toContain("EditNotAllowed");
    expect(
      edit.validateEdit(
        plain,
        put(
          "type.Person",
          toml.stringify({ types: { Person: { ...person, props: "x" } } }),
        ),
        [itemPut],
      ),
    ).toContain("EditNotAllowed");
  });

  it("adds only the link the item's work history names, as a plain many-valued link", () => {
    expect(edit.validateEdit(plain, linkPut)).toContain("EditNotAllowed");
    expect(edit.validateEdit(plain, linkPut, batch)).toBeUndefined();
    const link = doc(linkPut).links.worked_at;
    const with_ = (patch: Record<string, unknown>) =>
      put(
        "link.worked_at",
        toml.stringify({ links: { worked_at: { ...link, ...patch } } }),
      );
    for (const patch of [
      { cardinality: "many" },
      { required: false },
      { implies: ["works_at"] },
      { identity: ["title"] },
    ])
      expect(edit.validateEdit(plain, with_(patch), batch)).toBe(
        "EditNotAllowed: a new link can't set cardinality, required or declarations.",
      );
    expect(
      edit.validateEdit(plain, with_({ from: "Company" }), batch),
    ).toContain("EditNotAllowed");
    expect(edit.validateEdit(plain, with_({ to: "Nobody" }), batch)).toBe(
      "Unknown type: Nobody.",
    );
    expect(edit.validateEdit(plain, with_({ description: 3 }), batch)).toBe(
      "A link's description is text.",
    );
    expect(edit.validateEdit(plain, with_({ props: { a: 1 } }), batch)).toBe(
      "A link's props map names to types.",
    );
    // Existing links don't change, nor go.
    expect(
      edit.validateEdit(
        enriched,
        put(
          "link.worked_at",
          toml.stringify({
            links: { worked_at: { ...link, cardinality: "one" } },
          }),
        ),
      ),
    ).toBe("EditNotAllowed: a link can't change.");
    expect(edit.validateEdit(enriched, remove("link.worked_at"))).toBe(
      "EditNotAllowed: a link can't be removed.",
    );
  });

  it("removes an item that exists", () => {
    expect(edit.validateEdit(enriched, remove("enrichment.people"))).toBe(
      undefined,
    );
    expect(edit.validateEdit(plain, remove("enrichment.people"))).toBe(
      "This item does not exist.",
    );
  });

  it.each<[string, Record<string, unknown>, string]>([
    ["an unknown field", { refresh: true }, "Unknown enrichment field."],
    [
      "an unknown type",
      { type: "Nobody" },
      "EnrichmentUnknownType: no type Nobody.",
    ],
    [
      "a type without a key",
      { type: "Company" },
      "EnrichmentNoNames: Company needs a name field.",
    ],
    [
      "no clue",
      { clues: {} },
      "EnrichmentClueKind: pick an email or LinkedIn URL field.",
    ],
    [
      "an unknown clue",
      { clues: { phone: "email" } },
      "EnrichmentClueKind: pick an email or LinkedIn URL field.",
    ],
    [
      "one prop as both clues",
      { clues: { email: "email", linkedin_url: "email" } },
      "EnrichmentClueKind: pick different fields.",
    ],
    [
      "a clue that isn't a text prop",
      { clues: { email: "nothing" } },
      "EnrichmentClueKind: nothing isn't a text field of Person.",
    ],
    [
      "a clue in names",
      { clues: { email: "full_name" } },
      "EnrichmentClueKind: full_name is a name field.",
    ],
    [
      "a clue of the wrong identifier kind",
      { clues: { email: "linkedin_url" } },
      "EnrichmentClueKind: linkedin_url must be an email identifier.",
    ],
    [
      "a metadata field",
      { props: { current_title: "score" } },
      "EnrichmentField: score is the lookup’s own and can’t be mapped.",
    ],
    [
      "an unknown field",
      { props: { current_title: "skills" } },
      "EnrichmentField: skills isn’t a profile field.",
    ],
    [
      "props that aren't a map",
      { props: "current_title" },
      "EnrichmentField: props must map fields to profile fields.",
    ],
    [
      "an undeclared target",
      { props: { title: "current_title" } },
      "EnrichmentPropType: Person has no field title.",
    ],
    [
      "a key target",
      { props: { contact_id: "current_title" } },
      "EnrichmentFillsIdentity: contact_id identifies a Person, so People enrichment can’t fill it.",
    ],
    [
      "a name target",
      { props: { full_name: "full_name" } },
      "EnrichmentFillsIdentity: full_name identifies a Person, so People enrichment can’t fill it.",
    ],
    [
      "a clue target",
      { props: { linkedin_url: "linkedin_url" } },
      "EnrichmentFillsIdentity: linkedin_url identifies a Person, so People enrichment can’t fill it.",
    ],
    [
      "work history that isn't a table",
      { work_history: "worked_at" },
      "EnrichmentLink: work history needs a link, a match and props.",
    ],
    [
      "an unknown link",
      { work_history: { link: "met", match: "domain", props: {} } },
      "EnrichmentLink: no link met.",
    ],
    [
      "a link a mapping states",
      { work_history: { link: "works_at", match: "domain", props: {} } },
      "EnrichmentLink: works_at must be many-valued and optional.",
    ],
    [
      "an unknown experience field",
      {
        work_history: {
          link: "worked_at",
          match: "domain",
          props: { title: "salary" },
        },
      },
      "EnrichmentField: salary isn’t an experience field.",
    ],
    [
      "an undeclared link prop",
      {
        work_history: {
          link: "worked_at",
          match: "domain",
          props: { role: "title" },
        },
      },
      "EnrichmentPropType: worked_at has no field role.",
    ],
    [
      "a link prop of another type",
      {
        work_history: {
          link: "worked_at",
          match: "domain",
          props: { title: "is_current" },
        },
      },
      "EnrichmentPropType: title isn’t a boolean field.",
    ],
    [
      "a match the employer type lacks",
      {
        work_history: { link: "worked_at", match: "name", props: {} },
      },
      "EnrichmentLink: Company has no field name.",
    ],
  ])("refuses an item with %s", (_, patch, message) => {
    expect(edit.validateEdit(enriched, item(patch))).toBe(message);
  });

  it("matches employers by a text key or exact identifier only, as the engine's match_kind", () => {
    const o = toml.parse(enriched.ontologyToml) as Record<string, any>;
    o.types.Company.props.name = "string";
    o.types.Account = {
      description: "An account, by its number.",
      props: { account_id: "integer" },
      key: ["account_id"],
    };
    o.links.employed_by = { from: "Person", to: "Account" };
    const accounts = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: toml.stringify(o),
    });
    // An integer key isn't offered, nor accepted.
    expect(edit.employerOptions(accounts, "Person")).toEqual([
      { type: "Company", match: ["domain"] },
    ]);
    expect(
      edit.validateEdit(
        accounts,
        item({
          work_history: { link: "employed_by", match: "account_id", props: {} },
        }),
      ),
    ).toBe("EnrichmentLink: match account_id must be a text field of Account.");
    expect(
      edit.validateEdit(
        accounts,
        item({ work_history: { link: "worked_at", match: "name", props: {} } }),
      ),
    ).toBe(
      "EnrichmentLink: match name must be Company’s key or an exact identifier.",
    );
  });

  it("checks a type's new props against the batch's item, removed or not", () => {
    const person = (toml.parse(enriched.ontologyToml).types as any).Person;
    const added = put(
      "type.Person",
      toml.stringify({
        types: {
          Person: {
            ...person,
            props: { ...person.props, employer: "string" },
          },
        },
      }),
    );
    const on = (toml.parse(ITEM).enrichments as any).people;
    const fills = put(
      "enrichment.people",
      toml.stringify({
        enrichments: {
          people: {
            ...on,
            props: { ...on.props, employer: "current_org_name" },
          },
        },
      }),
    );
    expect(edit.validateEdit(enriched, added, [fills])).toBeUndefined();
    expect(
      edit.validateEdit(enriched, added, [remove("enrichment.people")]),
    ).toBe(
      "EditNotAllowed: employer is new; a type only gains the props its People enrichment fills.",
    );
  });

  it("refuses a second item, and a clue only a report binding fills", () => {
    expect(
      edit.validateEdit(
        enriched,
        put(
          "enrichment.more",
          ITEM.replaceAll("enrichments.people", "enrichments.more"),
        ),
      ),
    ).toBe("EnrichmentTooMany: mappings.toml has at most one enrichment item.");
    const report = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      mappings_toml: enriched.mappingsToml.replace(
        /(\[mappings\.people\.bind\.person\][^[]*)missing = "create"/,
        '$1missing = "report"',
      ),
    });
    expect(edit.validateEdit(report, item({}))).toBe(
      "EnrichmentClueNotCreated: email isn’t filled by any table.",
    );
  });

  it("refuses a type without a key or names", () => {
    const keyless = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: enriched.ontologyToml.replace(
        'key = [ "contact_id" ]',
        "key = [ ]",
      ),
    });
    expect(edit.validateEdit(keyless, item({}))).toBe(
      "EnrichmentTypeNotKeyed: Person needs a key.",
    );
  });

  it("refuses a profile field into a prop of another type", () => {
    const o = toml.parse(enriched.ontologyToml) as Record<string, any>;
    o.types.Person.props.current_title = "integer";
    const counted = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: toml.stringify(o),
    });
    expect(
      edit.validateEdit(
        counted,
        item({ props: { current_title: "current_title" } }),
      ),
    ).toBe("EnrichmentPropType: current_title isn’t a string field.");
  });

  it("refuses a target a table or a document route fills already", () => {
    const shared = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      mappings_toml: enriched.mappingsToml.replace(
        'full_name = "full_name"',
        'full_name = "full_name"\ncurrent_title = { field = "title" }',
      ),
    });
    expect(edit.validateEdit(shared, item({}))).toBe(
      "EnrichmentSharedOwner: current_title is already filled by people.title.",
    );
    const plainField = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      mappings_toml: enriched.mappingsToml.replace(
        'full_name = "full_name"',
        'full_name = "full_name"\nlocation = "city"',
      ),
    });
    expect(edit.validateEdit(plainField, item({}))).toBe(
      "EnrichmentSharedOwner: location is already filled by people.city.",
    );
    const route = put(
      "extraction.notes",
      '[extractions.notes]\ndocuments = "all"\n[extractions.notes.types.Person]\nprops = ["current_title"]',
    );
    expect(edit.validateEdit(enriched, item({}), [route])).toBe(
      "EnrichmentSharedOwner: current_title is already filled by documents (notes).",
    );
  });

  it("refuses a type or route edit that breaks the item the batch leaves", () => {
    // Person's email, a clue, is no longer an email identifier: the edit
    // passes on its own, the item it leaves doesn't.
    const ids = edit.identifierEdit(enriched, "Person", {
      linkedin_url: "exact",
    });
    expect(edit.validateEdit(enriched, ids, [ids])).toBeUndefined();
    expect(edit.batchEnrichmentRefusal(enriched, [ids])).toEqual({
      itemId: "enrichment.people",
      message: "EnrichmentClueKind: email must be an email identifier.",
    });
    // A route filling a prop the item fills, or stating its link.
    const route = (scope: string) =>
      put("extraction.bios", `[extractions.bios]\ndocuments = "all"\n${scope}`);
    const fills = route(
        '[extractions.bios.types.Person]\nprops = ["current_title"]',
      ),
      states = route(
        'links = ["worked_at"]\n[extractions.bios.types.Person]\n[extractions.bios.types.Company]',
      );
    expect(problems(enriched, [fills])).toEqual([]);
    expect(edit.batchEnrichmentRefusal(enriched, [fills])).toEqual({
      itemId: "enrichment.people",
      message:
        "EnrichmentSharedOwner: current_title is already filled by documents (bios).",
    });
    expect(problems(enriched, [states])).toEqual([]);
    expect(edit.batchEnrichmentRefusal(enriched, [states])).toEqual({
      itemId: "enrichment.people",
      message:
        "EnrichmentLink: worked_at is already stated by documents (bios).",
    });
    // The version as it is, a route that leaves the item be, the item
    // removed with the edit, or a version without one.
    const other = route('[extractions.bios.types.Person]\nprops = ["email"]');
    expect(edit.batchEnrichmentRefusal(enriched, [])).toBeUndefined();
    expect(edit.batchEnrichmentRefusal(enriched, [other])).toBeUndefined();
    expect(
      edit.batchEnrichmentRefusal(enriched, [ids, remove("enrichment.people")]),
    ).toBeUndefined();
    expect(
      edit.batchEnrichmentRefusal(plain, [
        edit.identifierEdit(plain, "Person", {}),
      ]),
    ).toBeUndefined();
  });

  it("refuses work history on a link that is declared, or stated elsewhere", () => {
    const declared = (ontology: string) =>
      edit.validateEdit(
        readAgentBundle({
          ...salesGraphBundle(SOURCE, CLUES),
          ontology_toml: ontology,
        }),
        item({}),
      );
    expect(
      declared(
        enriched.ontologyToml.replace(
          "[links.worked_at]",
          '[links.worked_at]\nimplies = [ "works_at" ]',
        ),
      ),
    ).toBe("EnrichmentLink: worked_at is in a declaration.");
    expect(
      declared(
        enriched.ontologyToml +
          '\n[relations.knows]\ndescription = "d"\nchain = [ [ "worked_at", "works_at" ] ]\n',
      ),
    ).toBe("EnrichmentLink: worked_at is in a declaration.");
    expect(
      declared(
        enriched.ontologyToml.replace(
          "[links.worked_at]",
          "[links.worked_at]\nrequired = true",
        ),
      ),
    ).toBe("EnrichmentLink: worked_at must be many-valued and optional.");
    expect(
      declared(
        enriched.ontologyToml.replace(
          '[links.worked_at]\nfrom = "Person"',
          '[links.worked_at]\nfrom = "Company"',
        ),
      ),
    ).toBe("EnrichmentLink: worked_at must start at Person.");
    const stated = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      mappings_toml: enriched.mappingsToml.replace(
        'link = "works_at"',
        'link = "worked_at"',
      ),
    });
    expect(edit.validateEdit(stated, item({}))).toBe(
      "EnrichmentLink: worked_at is already stated by people.",
    );
    const routed = put(
      "extraction.notes",
      '[extractions.notes]\ndocuments = "all"\nlinks = ["worked_at"]\n[extractions.notes.types.Person]\n[extractions.notes.types.Company]',
    );
    expect(edit.validateEdit(enriched, item({}), [routed])).toBe(
      "EnrichmentLink: worked_at is already stated by documents (notes).",
    );
  });

  it("matches employers by an exact identifier a table creates them by", () => {
    const ontology_toml = enriched.ontologyToml.replace(
      '[types.Company.props]\ndomain = "string"',
      '[types.Company.props]\ndomain = "string"\nwebsite = "string"\n\n[types.Company.identifiers]\nwebsite = "exact"',
    );
    const byWebsite = (created: boolean) =>
      readAgentBundle({
        ...salesGraphBundle(SOURCE, CLUES),
        ontology_toml,
        mappings_toml: created
          ? enriched.mappingsToml.replace(
              "[mappings.people.bind.company]",
              '[mappings.people.bind.company]\nprops = { website = "company" }',
            )
          : enriched.mappingsToml,
      });
    const website = item({
      work_history: {
        link: "worked_at",
        match: "website",
        props: { title: "title" },
      },
    });
    expect(edit.validateEdit(byWebsite(true), website)).toBeUndefined();
    expect(edit.employerOptions(byWebsite(true), "Person")).toEqual([
      { type: "Company", match: ["domain", "website"] },
    ]);
    // No table creates a Company by its website: it would find no one.
    expect(edit.validateEdit(byWebsite(false), website)).toBe(
      "EnrichmentLink: match website isn’t filled by any table that creates Company.",
    );
    expect(edit.employerOptions(byWebsite(false), "Person")).toEqual([
      { type: "Company", match: ["domain"] },
    ]);
  });

  it("fills the props that tell a link's facts apart", () => {
    const byTitle = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: enriched.ontologyToml.replace(
        '[links.worked_at]\nfrom = "Person"',
        '[links.worked_at]\nidentity = [ "title" ]\nfrom = "Person"',
      ),
    });
    expect(edit.validateEdit(byTitle, item({}))).toBeUndefined();
    expect(
      edit.validateEdit(
        byTitle,
        item({
          work_history: {
            link: "worked_at",
            match: "domain",
            props: { is_current: "is_current" },
          },
        }),
      ),
    ).toBe(
      "EnrichmentLink: worked_at tells facts apart by title, which work history doesn’t fill.",
    );
  });

  it("maps an experience's number and organization, as the engine's catalog has them", () => {
    const withOrg = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: enriched.ontologyToml.replace(
        'is_current = "boolean"',
        'is_current = "boolean"\nemployer = "string"\nposition = "integer"',
      ),
    });
    const org = item({
      work_history: {
        link: "worked_at",
        match: "domain",
        props: {
          title: "title",
          employer: "/organization/name",
          position: "id",
        },
      },
    });
    expect(edit.validateEdit(withOrg, org)).toBeUndefined();
    // Saving the form again keeps the item's own work history.
    const reread = readAgentBundle({
      ...salesGraphBundle(SOURCE, CLUES),
      ontology_toml: withOrg.ontologyToml,
      mappings_toml: withOrg.mappingsToml.replace(
        ITEM,
        org.put!.toml.trimEnd() + "\n",
      ),
    });
    const form = edit.enrichmentForm(reread, "Person");
    expect(edit.stageEnrichment(reread, [], "Person", form)).toEqual([]);
    if (!form.on) throw new Error("on");
    const fewer = edit.stageEnrichment(reread, [], "Person", {
      ...form,
      fields: form.fields.filter((f) => f.prop !== "country"),
    });
    expect(doc(fewer[0]).enrichments.people.work_history.props).toEqual({
      title: "title",
      employer: "/organization/name",
      position: "id",
    });
    // An experience field is no profile field.
    expect(
      edit.validateEdit(
        withOrg,
        item({ props: { current_title: "/organization/name" } }),
      ),
    ).toBe("EnrichmentField: /organization/name isn’t a profile field.");
  });

  it("names People enrichment among what may be edited", () => {
    expect(edit.validateEdit(plain, put("enrichment.", ""))).toBe(
      "EditNotAllowed: edit a route, identifiers, People enrichment, rule or constraint.",
    );
    expect(edit.HAND_EDITED).not.toContain("enrichment");
    expect(edit.HAND_EDITED).not.toContain("link");
    expect(PEOPLE.length).toBeGreaterThan(0);
  });
});
