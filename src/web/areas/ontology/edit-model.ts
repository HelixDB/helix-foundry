import * as toml from "smol-toml";
import type * as wfe from "../../../shared/wfe";
import {
  EXPERIENCE_FIELDS,
  PROFILE_DEFAULTS,
  PROFILE_FIELDS,
  UNMAPPABLE_FIELDS,
  WORK_HISTORY_DEFAULT,
  type ExperienceField,
  type ProfileField,
} from "./enrichment-fields";
import {
  parseAgentEnrichments,
  readRuleCounts,
  type AgentBundle,
} from "./model";

export type EditKind =
  | "extraction"
  | "type"
  | "rule"
  | "constraint"
  | "enrichment"
  | "link";
export const editTables = {
  extraction: "extractions",
  type: "types",
  rule: "rules",
  constraint: "constraints",
  enrichment: "enrichments",
  link: "links",
} as const;
/** The file an item of each kind lives in. */
const inMappings = (kind: EditKind) =>
  kind === "extraction" || kind === "enrichment";
const object = (v: unknown): v is Record<string, any> =>
  !!v && typeof v === "object" && !Array.isArray(v);
// TOML key order is not part of a type's schema.
const canonical = (v: any): string =>
  JSON.stringify(v, (_, x) =>
    object(x)
      ? Object.fromEntries(
          Object.entries(x).sort(([a], [b]) => a.localeCompare(b)),
        )
      : x,
  );
// A link's declarations: a link that has one, or that another names, is
// never People enrichment's.
const DECLARATIONS = [
  "inverse_of",
  "implies",
  "symmetric",
  "transitive",
  "chain",
] as const;

/**
 * The item an edit puts or removes.
 *
 * @example
 * editItem({ remove: { itemId: "rule.overdue" } }); // "rule.overdue"
 */
export const editItem = (edit: wfe.OntologyEdit) =>
  edit.put ? edit.put.itemId : edit.remove.itemId;

/** Read the actual files, not draft snippets, which can omit nested tables. */
export function editItems(bundle: AgentBundle): Record<string, string> {
  const ontology = toml.parse(bundle.ontologyToml),
    mappings = toml.parse(bundle.mappingsToml);
  return Object.fromEntries(
    Object.entries(editTables).flatMap(([kind, table]) =>
      Object.entries(
        (inMappings(kind as EditKind) ? mappings : ontology)[table] || {},
      ).map(([name, value]) => [
        `${kind}.${name}`,
        toml.stringify({ [table]: { [name]: value } }),
      ]),
    ),
  );
}

/** The kinds the Edit tab edits item by item; enrichment has its own form. */
export const HAND_EDITED: readonly EditKind[] = [
  "extraction",
  "type",
  "rule",
  "constraint",
];

/**
 * The types, links, enrichment items and routes a batch of edits leaves,
 * and the user mappings, as plain TOML values.
 * An edit whose snippet doesn't parse changes nothing here: its own check
 * says why.
 */
function resulting(bundle: AgentBundle, batch: readonly wfe.OntologyEdit[]) {
  const ontology = toml.parse(bundle.ontologyToml) as Record<string, any>,
    mappings = toml.parse(bundle.mappingsToml) as Record<string, any>;
  const out = {
    types: { ...(object(ontology.types) ? ontology.types : {}) },
    links: { ...(object(ontology.links) ? ontology.links : {}) },
    enrichments: {
      ...(object(mappings.enrichments) ? mappings.enrichments : {}),
    },
    routes: {
      ...(object(mappings.extractions) ? mappings.extractions : {}),
    } as Record<string, any>,
    // Relations and user mappings: no edit changes them.
    relations: (object(ontology.relations) ? ontology.relations : {}) as Record<
      string,
      any
    >,
    mappings: (object(mappings.mappings) ? mappings.mappings : {}) as Record<
      string,
      any
    >,
  };
  for (const e of batch) {
    const [kind, name = ""] = editItem(e).split(".") as [string, string?];
    const into =
      kind === "type"
        ? out.types
        : kind === "link"
          ? out.links
          : kind === "enrichment"
            ? out.enrichments
            : kind === "extraction"
              ? out.routes
              : undefined;
    if (!into) continue;
    if (!e.put) {
      delete into[name];
      continue;
    }
    try {
      const value = (
        toml.parse(e.put.toml)[editTables[kind as EditKind]] as Record<
          string,
          any
        >
      )?.[name];
      if (object(value)) into[name] = value;
    } catch {
      // Reported by the edit's own check.
    }
  }
  return out;
}

/**
 * The props of `type` a create binding of a user mapping fills: by key or
 * by props. Only these can hold a person's clues.
 *
 * @example
 * createdProps(bundle, "Customer"); // Set { "customer_id", "email", ... }
 */
export function createdProps(bundle: AgentBundle, type: string): Set<string> {
  const mappings = bundle.mappings.ok ? bundle.mappings.value : [];
  return new Set(
    mappings
      .flatMap((m) => m.bindings)
      .filter((b) => b.type === type && b.missing === "create")
      .flatMap((b) => [
        ...b.props.map(([p]) => p),
        ...(b.identify.kind === "key" ? b.identify.fields.map(([p]) => p) : []),
      ]),
  );
}

/**
 * Where a user mapping or a route fills a prop of `type` already, as
 * "mapping.field" or "documents (route)": People enrichment can't fill it
 * too.
 */
function filledBy(
  r: ReturnType<typeof resulting>,
  type: string,
  prop: string,
): string | undefined {
  for (const [mapping, m] of Object.entries(r.mappings))
    for (const b of Object.values(object(m?.bind) ? m.bind : {}) as any[]) {
      if (!object(b) || b.type !== type) continue;
      const field = (object(b.props) ? b.props : {})[prop];
      if (field !== undefined)
        return `${mapping}.${typeof field === "string" ? field : object(field) ? field.field : prop}`;
    }
  for (const [route, x] of Object.entries(r.routes)) {
    const scope = object(x?.types) ? x.types[type] : undefined;
    if (
      object(scope) &&
      Array.isArray(scope.props) &&
      scope.props.includes(prop)
    )
      return `documents (${route})`;
  }
  return undefined;
}

/**
 * Why work history can't state `linkName` from `typeName`: it must start at
 * the type, be many-valued and optional, be in no declaration, and be
 * stated by no mapping or route.
 */
function linkRefusal(
  r: ReturnType<typeof resulting>,
  linkName: string,
  typeName: string,
): string | undefined {
  if (!linkName) return "EnrichmentLink: no link can hold work history.";
  const link = r.links[linkName];
  if (!object(link)) return `EnrichmentLink: no link ${linkName}.`;
  if (link.from !== typeName)
    return `EnrichmentLink: ${linkName} must start at ${typeName}.`;
  if (link.cardinality === "one" || link.required === true)
    return `EnrichmentLink: ${linkName} must be many-valued and optional.`;
  if (
    DECLARATIONS.some((d) => link[d] !== undefined) ||
    [...Object.entries(r.links), ...Object.entries(r.relations)].some(
      ([other, l]) =>
        other !== linkName &&
        object(l) &&
        JSON.stringify(DECLARATIONS.map((d) => l[d])).includes(
          JSON.stringify(linkName),
        ),
    )
  )
    return `EnrichmentLink: ${linkName} is in a declaration.`;
  const stated = Object.entries(r.mappings).find(
    ([, m]) =>
      Array.isArray(m?.links) &&
      m.links.some((l: unknown) => object(l) && l.link === linkName),
  );
  if (stated)
    return `EnrichmentLink: ${linkName} is already stated by ${stated[0]}.`;
  const routed = Object.entries(r.routes).find(
    ([, x]) => Array.isArray(x?.links) && x.links.includes(linkName),
  );
  if (routed)
    return `EnrichmentLink: ${linkName} is already stated by documents (${routed[0]}).`;
  return undefined;
}

/** Why a People enrichment item can't be put, by the engine's rules. */
function enrichmentRefusal(
  bundle: AgentBundle,
  name: string,
  value: Record<string, any>,
  r: ReturnType<typeof resulting>,
): string | undefined {
  if (Object.keys(r.enrichments).some((other) => other !== name))
    return "EnrichmentTooMany: mappings.toml has at most one enrichment item.";
  if (
    Object.keys(value).some(
      (k) => !["type", "clues", "props", "work_history"].includes(k),
    )
  )
    return "Unknown enrichment field.";
  const typeName = String(value.type ?? ""),
    type = r.types[typeName];
  if (!object(type)) return `EnrichmentUnknownType: no type ${typeName}.`;
  const props: Record<string, unknown> = object(type.props) ? type.props : {},
    key: unknown[] = Array.isArray(type.key) ? type.key : [],
    names: unknown[] = Array.isArray(type.names) ? type.names : [],
    identifiers: Record<string, unknown> = object(type.identifiers)
      ? type.identifiers
      : {};
  if (!key.length) return `EnrichmentTypeNotKeyed: ${typeName} needs a key.`;
  if (!names.length)
    return `EnrichmentNoNames: ${typeName} needs a name field.`;
  const clues = object(value.clues) ? value.clues : {};
  if (
    !object(value.clues) ||
    Object.keys(clues).some((k) => !["email", "linkedin_url"].includes(k)) ||
    !Object.keys(clues).length
  )
    return "EnrichmentClueKind: pick an email or LinkedIn URL field.";
  if (clues.email !== undefined && clues.email === clues.linkedin_url)
    return "EnrichmentClueKind: pick different fields.";
  const created = createdProps(bundle, typeName);
  for (const [clue, kind] of [
    ["email", "email"],
    ["linkedin_url", "exact"],
  ] as const) {
    const prop = clues[clue];
    if (prop === undefined) continue;
    if (typeof prop !== "string" || props[prop] !== "string")
      return `EnrichmentClueKind: ${String(prop)} isn't a text field of ${typeName}.`;
    if (names.includes(prop))
      return `EnrichmentClueKind: ${prop} is a name field.`;
    if (identifiers[prop] !== kind)
      return `EnrichmentClueKind: ${prop} must be an ${kind} identifier.`;
    if (!created.has(prop))
      return `EnrichmentClueNotCreated: ${prop} isn’t filled by any table.`;
  }
  const clueProps = Object.values(clues);
  const target = (prop: string, kindOf: string, owner: string) =>
    props[prop] === undefined
      ? `EnrichmentPropType: ${owner} has no field ${prop}.`
      : props[prop] !== kindOf
        ? `EnrichmentPropType: ${prop} isn’t a ${kindOf} field.`
        : undefined;
  if (value.props !== undefined && !object(value.props))
    return "EnrichmentField: props must map fields to profile fields.";
  for (const [prop, field] of Object.entries(value.props || {})) {
    if (UNMAPPABLE_FIELDS.includes(field as never))
      return `EnrichmentField: ${String(field)} is the lookup’s own and can’t be mapped.`;
    if (!Object.hasOwn(PROFILE_FIELDS, String(field)))
      return `EnrichmentField: ${String(field)} isn’t a profile field.`;
    const problem = target(
      prop,
      PROFILE_FIELDS[field as ProfileField],
      typeName,
    );
    if (problem) return problem;
    if (
      key.includes(prop) ||
      names.includes(prop) ||
      Object.hasOwn(identifiers, prop) ||
      clueProps.includes(prop)
    )
      return `EnrichmentFillsIdentity: ${prop} identifies a ${typeName}, so People enrichment can’t fill it.`;
    const owner = filledBy(r, typeName, prop);
    if (owner)
      return `EnrichmentSharedOwner: ${prop} is already filled by ${owner}.`;
  }
  if (value.work_history === undefined) return undefined;
  const history = value.work_history;
  if (
    !object(history) ||
    Object.keys(history).some((k) => !["link", "match", "props"].includes(k))
  )
    return "EnrichmentLink: work history needs a link, a match and props.";
  const linkName = String(history.link ?? ""),
    link = r.links[linkName];
  const refused = linkRefusal(r, linkName, typeName);
  if (refused) return refused;
  // As the engine's match_kind: a text prop, the `to` type's only key or
  // an exact identifier.
  const to = r.types[String(link.to ?? "")],
    toName = String(link.to),
    match = String(history.match ?? "");
  const toKey: unknown[] = object(to) && Array.isArray(to.key) ? to.key : [],
    toProps: Record<string, unknown> =
      object(to) && object(to.props) ? to.props : {};
  if (toProps[match] === undefined)
    return `EnrichmentLink: ${toName} has no field ${match}.`;
  if (toProps[match] !== "string")
    return `EnrichmentLink: match ${match} must be a text field of ${toName}.`;
  const exact =
    object(to) && object(to.identifiers) && to.identifiers[match] === "exact";
  const byKey = toKey.length === 1 && toKey[0] === match;
  if (!byKey && !exact)
    return `EnrichmentLink: match ${match} must be ${toName}’s key or an exact identifier.`;
  // An exact identifier finds only the employers a table creates by it.
  if (!byKey && !createdProps(bundle, toName).has(match))
    return `EnrichmentLink: match ${match} isn’t filled by any table that creates ${toName}.`;
  const linkProps: Record<string, unknown> = object(link.props)
      ? link.props
      : {},
    historyProps: Record<string, unknown> = object(history.props)
      ? history.props
      : {};
  // The props that tell one fact of the link from another.
  const unset = (Array.isArray(link.identity) ? link.identity : []).find(
    (p: unknown) => !Object.hasOwn(historyProps, String(p)),
  );
  if (unset !== undefined)
    return `EnrichmentLink: ${linkName} tells facts apart by ${String(unset)}, which work history doesn’t fill.`;
  for (const [prop, field] of Object.entries(historyProps)) {
    if (!Object.hasOwn(EXPERIENCE_FIELDS, String(field)))
      return `EnrichmentField: ${String(field)} isn’t an experience field.`;
    const kindOf = EXPERIENCE_FIELDS[field as keyof typeof EXPERIENCE_FIELDS];
    if (linkProps[prop] === undefined)
      return `EnrichmentPropType: ${linkName} has no field ${prop}.`;
    if (linkProps[prop] !== kindOf)
      return `EnrichmentPropType: ${prop} isn’t a ${kindOf} field.`;
  }
  return undefined;
}

/**
 * Why the People enrichment item a batch of edits leaves can't stay, and
 * its id. Revise checks the whole revised draft (the engine's
 * `validate::offline`), so an edit to a type or a route that breaks the
 * item is refused though the batch doesn't put it: a clue that is no longer
 * an identifier of its kind, or a route that fills a prop the item fills or
 * states its work history's link.
 *
 * @example
 * // Person's email, a clue, is no longer an email identifier.
 * batchEnrichmentRefusal(bundle, [identifierEdit(bundle, "Person", { linkedin_url: "exact" })]);
 * // { itemId: "enrichment.people", message: "EnrichmentClueKind: email must be an email identifier." }
 */
export function batchEnrichmentRefusal(
  bundle: AgentBundle,
  batch: readonly wfe.OntologyEdit[],
): { itemId: string; message: string } | undefined {
  const r = resulting(bundle, batch);
  for (const [name, value] of Object.entries(r.enrichments)) {
    const message = object(value)
      ? enrichmentRefusal(bundle, name, value, r)
      : undefined;
    if (message) return { itemId: `enrichment.${name}`, message };
  }
  return undefined;
}

/**
 * Validate a single item's boundary, within the batch it is saved with
 * (`batch`, this edit included or not): a type's new props and a new link
 * are allowed only when the batch's People enrichment item fills them. The
 * engine remains the rule compiler.
 */
export function validateEdit(
  bundle: AgentBundle,
  edit: wfe.OntologyEdit,
  batch: readonly wfe.OntologyEdit[] = [],
): string | undefined {
  const itemId = editItem(edit);
  const match =
    /^(extraction|type|rule|constraint|enrichment|link)\.([A-Za-z_][A-Za-z0-9_]*)$/.exec(
      itemId,
    );
  if (!match)
    return "EditNotAllowed: edit a route, identifiers, People enrichment, rule or constraint.";
  const kind = match[1] as EditKind,
    name = match[2]!,
    table = editTables[kind];
  try {
    const items = editItems(bundle);
    if (!edit.put) {
      if (kind === "type")
        return "EditNotAllowed: a type can only change its identifiers.";
      if (kind === "link") return "EditNotAllowed: a link can't be removed.";
      return items[itemId] ? undefined : "This item does not exist.";
    }
    const doc = toml.parse(edit.put.toml) as Record<string, any>;
    if (
      Object.keys(doc).length !== 1 ||
      !object(doc[table]) ||
      Object.keys(doc[table]).length !== 1 ||
      !object(doc[table][name])
    )
      return "EditNotAllowed: the snippet must contain only this item.";
    const value = doc[table][name];
    if (kind === "type") {
      const original = toml.parse(bundle.ontologyToml).types as Record<
        string,
        any
      >;
      if (
        !object(original) ||
        !Object.hasOwn(original, name) ||
        !object(original[name])
      )
        return "EditNotAllowed: types must already exist.";
      const rest = ({
        identifiers: _ids,
        props: _props,
        ...others
      }: Record<string, unknown>) => others;
      const before: Record<string, unknown> = object(original[name].props)
          ? original[name].props
          : {},
        after: Record<string, unknown> = object(value.props) ? value.props : {};
      if (
        canonical(rest(value)) !== canonical(rest(original[name])) ||
        (value.props !== undefined && !object(value.props)) ||
        Object.entries(before).some(([p, t]) => after[p] !== t)
      )
        return "EditNotAllowed: a type can only change its identifiers.";
      // New props: only those the batch's People enrichment item fills.
      const enrichment = Object.values(resulting(bundle, batch).enrichments)
        .filter(object)
        .find((e) => e.type === name);
      const filled = new Set(
        Object.keys(object(enrichment?.props) ? enrichment.props : {}),
      );
      const added = Object.keys(after).find(
        (p) => !Object.hasOwn(before, p) && !filled.has(p),
      );
      if (added)
        return `EditNotAllowed: ${added} is new; a type only gains the props its People enrichment fills.`;
      if (value.identifiers !== undefined && !object(value.identifiers))
        return "Identifiers must map properties to kinds.";
      for (const [prop, kind] of Object.entries(value.identifiers || {})) {
        if (!Object.hasOwn(value.props || {}, prop))
          return `UnknownProp: ${prop}.`;
        if (!["email", "code", "number", "exact"].includes(String(kind)))
          return `Unknown identifier kind: ${kind}.`;
        if (value.props[prop] !== (kind === "number" ? "integer" : "string"))
          return `IdentifierType: ${prop} cannot use ${kind}.`;
      }
    } else if (kind === "link") {
      const original = toml.parse(bundle.ontologyToml).links as
        | Record<string, unknown>
        | undefined;
      if (object(original) && Object.hasOwn(original, name))
        return canonical(value) === canonical(original[name])
          ? undefined
          : "EditNotAllowed: a link can't change.";
      const enrichment = Object.values(resulting(bundle, batch).enrichments)
        .filter(object)
        .find((e) => object(e.work_history) && e.work_history.link === name);
      if (!enrichment || value.from !== enrichment.type)
        return "EditNotAllowed: a new link must be the one People enrichment's work history adds.";
      if (
        Object.keys(value).some(
          (k) => !["from", "to", "description", "props"].includes(k),
        )
      )
        return "EditNotAllowed: a new link can't set cardinality, required or declarations.";
      const types = toml.parse(bundle.ontologyToml).types;
      if (!object(types) || !Object.hasOwn(types, String(value.to ?? "")))
        return `Unknown type: ${String(value.to)}.`;
      if (
        value.description !== undefined &&
        typeof value.description !== "string"
      )
        return "A link's description is text.";
      if (
        value.props !== undefined &&
        (!object(value.props) ||
          Object.values(value.props).some((t) => typeof t !== "string"))
      )
        return "A link's props map names to types.";
    } else if (kind === "enrichment") {
      return enrichmentRefusal(
        bundle,
        name,
        value,
        resulting(bundle, [...batch, edit]),
      );
    } else if (kind === "extraction") {
      if (
        value.documents !== "all" &&
        (!Array.isArray(value.documents) ||
          !value.documents.length ||
          value.documents.some((s: unknown) => typeof s !== "string" || !s))
      )
        return 'Documents must be "all" or a nonempty list of set ids.';
      if (!object(value.types)) return "A route needs its scoped types.";
      const ontology = bundle.ontology.ok ? bundle.ontology.value : undefined;
      for (const [name, scope] of Object.entries(value.types)) {
        const type = ontology?.types.find((t) => t.name === name);
        if (!type || !object(scope)) return `Unknown type: ${name}.`;
        if (scope.create !== undefined && typeof scope.create !== "boolean")
          return "Create must be true or false.";
        if (
          scope.props !== undefined &&
          (!Array.isArray(scope.props) ||
            scope.props.some(
              (p: unknown) => !type.props.some((x) => x.name === p),
            ))
        )
          return `UnknownProp in ${name}.`;
      }
      if (
        value.links !== undefined &&
        (!Array.isArray(value.links) ||
          value.links.some((name: unknown) => {
            const link = ontology?.links.find((l) => l.name === name);
            return (
              !link ||
              !Object.hasOwn(value.types, link.from) ||
              !Object.hasOwn(value.types, link.to)
            );
          }))
      )
        return "Each link must exist and both ends must be in the route.";
      if (
        Object.keys(value).some(
          (k) => !["documents", "types", "links"].includes(k),
        ) ||
        Object.values(value.types).some((scope: any) =>
          Object.keys(scope).some((k) => !["create", "props"].includes(k)),
        )
      )
        return "Unknown route field.";
    } else {
      // The engine's RuleShape and ConstraintShape require both.
      if (typeof value.description !== "string" || !value.description.trim())
        return "Enter a description.";
      if (typeof value.datalog !== "string" || !value.datalog.trim())
        return "Enter a Datalog expression.";
      if (
        Object.keys(value).some(
          (k) =>
            ![
              "description",
              "datalog",
              ...(kind === "constraint" ? ["severity"] : []),
            ].includes(k),
        )
      )
        return "Unknown rule field.";
      if (
        kind === "constraint" &&
        !["block", "warn", "info"].includes(value.severity)
      )
        return "Choose block, warn or info severity.";
    }
    return undefined;
  } catch (e) {
    return `Invalid TOML: ${(e as Error).message}`;
  }
}

/**
 * Build a full type item while preserving every field other than
 * identifiers, from the type as the `pending` edits leave it (so People
 * enrichment's props stay), adding the `added` props. No identifiers leaves
 * the key out, as the engine writes such a type, so an untouched type without any
 * stages nothing.
 *
 * @example
 * identifierEdit(bundle, "Invoice", {}).put?.toml; // no [types.Invoice.identifiers]
 * identifierEdit(bundle, "Person", { email: "email" }, [], { country: "string" }).put?.toml; // props gain country
 */
export function identifierEdit(
  bundle: AgentBundle,
  name: string,
  identifiers: Record<string, string>,
  pending: readonly wfe.OntologyEdit[] = [],
  added: Record<string, string> = {},
): wfe.OntologyEdit {
  const { identifiers: _, ...rest } = (resulting(bundle, pending).types[name] ||
    {}) as Record<string, unknown>;
  const props = Object.keys(added).length
    ? { ...(object(rest.props) ? rest.props : {}), ...added }
    : rest.props;
  const type = props === undefined ? rest : { ...rest, props };
  return {
    put: {
      itemId: `type.${name}`,
      toml: toml.stringify({
        types: {
          [name]: Object.keys(identifiers).length
            ? { ...type, identifiers }
            : type,
        },
      }),
    },
  };
}

// ---------------------------------------------------------------- People enrichment

/**
 * What the Enrich people form chose for a type: off, or the props it looks
 * people up by, the profile fields it fills (each into a prop, new or not)
 * and the employers it links them to.
 */
export type EnrichmentForm =
  | { on: false }
  | {
      on: true;
      clues: { email?: string; linkedinUrl?: string };
      fields: { field: ProfileField; prop: string }[];
      // No link when none from the type to `to` can hold work history: the
      // form says so, and the item is refused.
      workHistory?: { link?: string; to: string; match: string };
    };
export type EnrichmentOn = Extract<EnrichmentForm, { on: true }>;

/** The type's text props that may hold a clue: any but its name fields. */
export function clueOptions(bundle: AgentBundle, type: string): string[] {
  const t = bundle.ontology.ok
    ? bundle.ontology.value.types.find((x) => x.name === type)
    : undefined;
  return (t?.props || [])
    .filter((p) => p.type === "string" && !t!.names.includes(p.name))
    .map((p) => p.name);
}

/**
 * Where a type's employers can be matched by domain: types (other than
 * `type`) with a one-prop text key, or a text exact identifier a table
 * creates them by, each with those props (the engine's match_kind).
 */
export function employerOptions(
  bundle: AgentBundle,
  type: string,
): { type: string; match: string[] }[] {
  const types = bundle.ontology.ok ? bundle.ontology.value.types : [];
  return types
    .filter((t) => t.name !== type)
    .map((t) => {
      const text = (prop: string) =>
        t.props.some((p) => p.name === prop && p.type === "string");
      return {
        type: t.name,
        match: [
          ...new Set([
            ...(t.key.length === 1 && text(t.key[0]!) ? t.key : []),
            ...t.identifiers
              .filter(
                (i) =>
                  i.kind === "exact" &&
                  text(i.prop) &&
                  createdProps(bundle, t.name).has(i.prop),
              )
              .map((i) => i.prop),
          ]),
        ],
      };
    })
    .filter((t) => t.match.length > 0);
}

/**
 * The type's props a profile field may fill as they are: text props that
 * identify nothing (no key, name, identifier or clue) and no table or
 * document route fills.
 */
export function targetOptions(
  bundle: AgentBundle,
  type: string,
  clues: EnrichmentOn["clues"],
): string[] {
  const r = resulting(bundle, []),
    t = r.types[type];
  if (!object(t)) return [];
  const fixed = new Set<unknown>([
    ...(Array.isArray(t.key) ? t.key : []),
    ...(Array.isArray(t.names) ? t.names : []),
    ...Object.keys(object(t.identifiers) ? t.identifiers : {}),
    clues.email,
    clues.linkedinUrl,
  ]);
  return Object.entries(object(t.props) ? t.props : {})
    .filter(
      ([p, kind]) =>
        kind === "string" && !fixed.has(p) && !filledBy(r, type, p),
    )
    .map(([p]) => p);
}

/**
 * What a profile field's select offers, the prop it fills (`prop`) always
 * among them so the select shows it: its default prop (`fallback`) and
 * `prop` while the type lacks them, as new, then the props `targetOptions`
 * lets it fill as they are. A prop the type has that the field can't fill
 * (a table or a route fills it, or it identifies the type) is listed only
 * while the field fills it, as unavailable, and can't be picked: Save says
 * why.
 *
 * @example
 * // The people table fills country; Person has title too.
 * fieldOptions(bundle, "Person", clues, "country_2", "country");
 * // [{ value: "country", label: "country (unavailable)", disabled: true },
 * //  { value: "country_2", label: "country_2 (new)", disabled: false },
 * //  { value: "title", label: "title", disabled: false }]
 */
export function fieldOptions(
  bundle: AgentBundle,
  type: string,
  clues: EnrichmentOn["clues"],
  fallback: string,
  prop: string,
): { value: string; label: string; disabled: boolean }[] {
  const t = resulting(bundle, []).types[type],
    props: Record<string, unknown> = object(t?.props) ? t.props : {},
    fillable = targetOptions(bundle, type, clues);
  const listed = [
    ...new Set([
      ...[fallback, prop].filter((p) => !Object.hasOwn(props, p)),
      ...fillable,
    ]),
  ];
  return (listed.includes(prop) ? listed : [prop, ...listed]).map((p) => {
    const unavailable = Object.hasOwn(props, p) && !fillable.includes(p);
    return {
      value: p,
      label: !Object.hasOwn(props, p)
        ? `${p} (new)`
        : unavailable
          ? `${p} (unavailable)`
          : p,
      disabled: unavailable,
    };
  });
}

/** The links from `type` to `to` work history may state as they are. */
export function historyLinks(
  bundle: AgentBundle,
  type: string,
  to: string,
): string[] {
  const r = resulting(bundle, []);
  return Object.entries(r.links)
    .filter(
      ([name, l]) => object(l) && l.to === to && !linkRefusal(r, name, type),
    )
    .map(([name]) => name);
}

/**
 * The link work history states from `type` to `to` unless told otherwise:
 * the first it may state as it is, else a new worked_at while no link has
 * that name; undefined when neither.
 *
 * @example
 * historyLink(bundle, "Person", "Company"); // "worked_at"
 */
export function historyLink(
  bundle: AgentBundle,
  type: string,
  to: string,
): string | undefined {
  return (
    historyLinks(bundle, type, to)[0] ??
    (Object.hasOwn(resulting(bundle, []).links, WORK_HISTORY_DEFAULT.link)
      ? undefined
      : WORK_HISTORY_DEFAULT.link)
  );
}

/**
 * Work history to the employer `to` as the form offers it: matched by its
 * prop named like a domain, else its first, through the link `historyLink`
 * names (none when there is no such link). Without `to`, to the first type
 * a domain matches, else the first employer. Undefined when there is no
 * such employer.
 *
 * @example
 * employerHistory(bundle, "Person"); // { to: "Company", match: "domain", link: "worked_at" }
 */
export function employerHistory(
  bundle: AgentBundle,
  type: string,
  to?: string,
): EnrichmentOn["workHistory"] {
  const employers = employerOptions(bundle, type),
    domain = (m: string) => /domain/i.test(m);
  const employer =
    to === undefined
      ? (employers.find((o) => o.match.some(domain)) ?? employers[0])
      : employers.find((o) => o.type === to);
  if (!employer) return undefined;
  return {
    to: employer.type,
    match: employer.match.find(domain) ?? employer.match[0]!,
    link: historyLink(bundle, type, employer.type),
  };
}

/**
 * The type the file's People enrichment item enriches once the `pending`
 * edits apply, if it has one. A file has at most one, so the form enriches
 * no other type while it does.
 */
export function enrichedType(
  bundle: AgentBundle,
  pending: readonly wfe.OntologyEdit[] = [],
): string | undefined {
  const item = Object.values(resulting(bundle, pending).enrichments).find(
    object,
  );
  return typeof item?.type === "string" ? item.type : undefined;
}

/**
 * The form as the enrichment item of `type` sets it once the `pending`
 * edits apply (the version's, by default), or off when there is none.
 */
export function enrichmentForm(
  bundle: AgentBundle,
  type: string,
  pending: readonly wfe.OntologyEdit[] = [],
): EnrichmentForm {
  const r = resulting(bundle, pending),
    items = parseAgentEnrichments(
      toml.stringify({ enrichments: r.enrichments }),
    );
  const e = items.ok ? items.value.find((x) => x.type === type) : undefined;
  if (!e) return { on: false };
  const link = e.workHistory ? r.links[e.workHistory.link] : undefined;
  return {
    on: true,
    clues: e.clues,
    fields: e.props.flatMap(([prop, field]) =>
      Object.hasOwn(PROFILE_FIELDS, field)
        ? [{ field: field as ProfileField, prop }]
        : [],
    ),
    workHistory:
      e.workHistory && object(link) && typeof link.to === "string"
        ? { link: e.workHistory.link, to: link.to, match: e.workHistory.match }
        : undefined,
  };
}

/**
 * The form switched on for a type without an item: its email identifier,
 * else a prop named like an email, and a prop named like a LinkedIn URL
 * (one a table fills first; an `exact` identifier alone says nothing of
 * LinkedIn); every default profile field into a prop it can fill: its
 * default name, else that name numbered, while a table, a route or an
 * identity holds it; and work history with the first type a domain
 * matches, through a link it may state (none when there is no such link).
 *
 * @example
 * enrichmentDefaults(bundle, "Person").fields[2]; // { field: "location_country", prop: "country" }, or "country_2" when a table fills country
 */
export function enrichmentDefaults(
  bundle: AgentBundle,
  type: string,
): EnrichmentOn {
  const t = bundle.ontology.ok
    ? bundle.ontology.value.types.find((x) => x.name === type)
    : undefined;
  const options = clueOptions(bundle, type),
    created = createdProps(bundle, type);
  const identified = (p: string, kind: string) =>
    !!t?.identifiers.some((i) => i.prop === p && i.kind === kind);
  const named = (pattern: RegExp, kind: string) => {
    const like = options.filter((p) => pattern.test(p));
    return (
      like.find((p) => created.has(p)) ??
      like.find((p) => identified(p, kind)) ??
      like[0]
    );
  };
  const clues = {
    email:
      options.find((p) => identified(p, "email")) ?? named(/email/i, "email"),
    linkedinUrl: named(/linkedin/i, "exact"),
  };
  const props = new Set(t?.props.map((p) => p.name)),
    fillable = new Set(targetOptions(bundle, type, clues));
  // One of these is new: there are more of them than the type has props.
  const free = (prop: string) =>
    [prop, ...[...props].map((_, i) => `${prop}_${i + 2}`)].find(
      (p) => !props.has(p) || fillable.has(p),
    )!;
  const history = employerHistory(bundle, type);
  return {
    on: true,
    clues,
    fields: PROFILE_DEFAULTS.map((d) => ({
      field: d.field,
      prop: free(d.prop),
    })),
    workHistory:
      history?.link && /domain/i.test(history.match) ? history : undefined,
  };
}

/**
 * A type's identifiers as the pending edits leave them, with those the item
 * there declared for its clues (a try's, or the version's) taken back to
 * `before`: the type's identifiers before the form first staged that try,
 * which the version page keeps, since the edits alone can't tell an
 * identifier a try declared from one staged by hand. Without `before`, the
 * version's.
 *
 * @example
 * const ids = identifierEdit(bundle, "Person", { email: "email", linkedin_url: "exact" });
 * const tried = stageEnrichment(bundle, [ids], "Person", enrichmentDefaults(bundle, "Person"));
 * handIdentifiers(bundle, tried, "Person"); // { email: "email" }, as the version has them
 * handIdentifiers(bundle, tried, "Person", { email: "email", linkedin_url: "exact" }); // both, as staged by hand
 */
export function handIdentifiers(
  bundle: AgentBundle,
  pending: readonly wfe.OntologyEdit[],
  type: string,
  before?: Readonly<Record<string, string>>,
): Record<string, string> {
  const r = resulting(bundle, pending),
    base = r.types[type],
    own = resulting(bundle, []).types[type];
  const prior: Record<string, unknown> =
    before ?? (object(own?.identifiers) ? own.identifiers : {});
  // A file has at most one item.
  const item = Object.values(r.enrichments).find(
      (e) => object(e) && e.type === type,
    ),
    clues = new Set(
      object(item) && object(item.clues) ? Object.values(item.clues) : [],
    );
  return Object.fromEntries(
    Object.entries<string>(
      object(base?.identifiers) ? base.identifiers : {},
    ).flatMap(([p, kind]) =>
      !clues.has(p)
        ? [[p, kind]]
        : Object.hasOwn(prior, p)
          ? [[p, String(prior[p])]]
          : [],
    ),
  );
}

/**
 * The pending edits with the form saved for `type`:
 * one `type.T` put, merged with any pending one, declaring the clues as
 * email and exact identifiers and adding the props it fills that the type
 * lacks; `link.<l>` when the work history link is new; and the
 * `enrichment.<name>` item (a file has at most one, so the form saves one
 * only for the type `enrichedType` names, or while none is). Off removes the
 * type's item only, so the version's props and link stay, empty. What an
 * earlier try of the form staged and this one no longer uses is dropped: a
 * new link, the props it added to the type, which a type gains only while
 * its item fills them, and the identifiers it declared for its clues, which
 * go back as `before` has them (see `handIdentifiers`).
 */
export function stageEnrichment(
  bundle: AgentBundle,
  pending: wfe.OntologyEdit[],
  type: string,
  form: EnrichmentForm,
  before?: Readonly<Record<string, string>>,
): wfe.OntologyEdit[] {
  const existing = bundle.enrichments.ok
      ? bundle.enrichments.value[0]
      : undefined,
    itemId = `enrichment.${existing?.name || "people"}`,
    links = toml.parse(bundle.ontologyToml).links;
  const original = (id: string) =>
    object(links) && Object.hasOwn(links, id.slice("link.".length));
  const kept = pending.filter(
    (e) => !editItem(e).startsWith("link.") || original(editItem(e)),
  );
  const r = resulting(bundle, kept),
    base = r.types[type] || {},
    own = resulting(bundle, []).types[type],
    ownProps: Record<string, unknown> = object(own?.props) ? own.props : {},
    fills = new Set(form.on ? form.fields.map((f) => f.prop) : []);
  // The type's own props, those an earlier try added that the form still
  // fills, and those it fills that the type lacks.
  const props = Object.fromEntries(
    Object.entries(object(base.props) ? base.props : {}).filter(
      ([p]) => Object.hasOwn(ownProps, p) || fills.has(p),
    ),
  );
  for (const f of form.on ? form.fields : [])
    if (!Object.hasOwn(props, f.prop)) props[f.prop] = PROFILE_FIELDS[f.field];
  const typePut = identifierEdit(
    bundle,
    type,
    {
      ...handIdentifiers(bundle, kept, type, before),
      ...(form.on && form.clues.email ? { [form.clues.email]: "email" } : {}),
      ...(form.on && form.clues.linkedinUrl
        ? { [form.clues.linkedinUrl]: "exact" }
        : {}),
    },
    kept.filter((e) => editItem(e) !== `type.${type}`),
    Object.fromEntries(
      Object.entries(props).flatMap(([p, kind]) =>
        Object.hasOwn(ownProps, p) ? [] : [[p, String(kind)]],
      ),
    ),
  );
  if (!form.on) {
    const pendingItem = r.enrichments[itemId.slice("enrichment.".length)];
    const rest =
      existing?.type === type
        ? stageEdit(bundle, kept, { remove: { itemId } })
        : object(pendingItem) && pendingItem.type === type
          ? kept.filter((e) => editItem(e) !== itemId)
          : kept;
    return object(own) ? stageEdit(bundle, rest, typePut) : rest;
  }
  const history = form.workHistory,
    link = history?.link ? r.links[history.link] : undefined,
    linkProps: Record<string, unknown> = object(link?.props) ? link.props : {};
  // The item's own work history when it keeps its link; else a new link
  // holds every default experience field, and an existing one those of them
  // it has, of the same type.
  const historyProps =
    existing?.type === type &&
    existing.workHistory &&
    link &&
    existing.workHistory.link === history?.link
      ? Object.fromEntries(existing.workHistory.props)
      : Object.fromEntries(
          Object.entries(WORK_HISTORY_DEFAULT.props).filter(
            ([prop, field]) =>
              !link || linkProps[prop] === EXPERIENCE_FIELDS[field],
          ),
        );
  // The engine reads a missing props table as empty, so the item writes one
  // only when it maps something or the version's item has it: an item left
  // as it is stages nothing, and keeps its confidence.
  const versionItem =
      existing?.type === type
        ? resulting(bundle, []).enrichments[existing.name]
        : undefined,
    fieldProps = Object.fromEntries(form.fields.map((f) => [f.prop, f.field]));
  const edits: wfe.OntologyEdit[] = [
    typePut,
    ...(history?.link && !link
      ? [
          {
            put: {
              itemId: `link.${history.link}`,
              toml: toml.stringify({
                links: {
                  [history.link]: {
                    from: type,
                    to: history.to,
                    description: WORK_HISTORY_DEFAULT.description,
                    props: Object.fromEntries(
                      // A new link's are the defaults, all experience fields.
                      Object.entries(historyProps).map(([p, f]) => [
                        p,
                        EXPERIENCE_FIELDS[f as ExperienceField],
                      ]),
                    ),
                  },
                },
              }),
            },
          },
        ]
      : []),
    {
      put: {
        itemId,
        toml: toml.stringify({
          enrichments: {
            [itemId.slice("enrichment.".length)]: {
              type,
              clues: {
                ...(form.clues.email ? { email: form.clues.email } : {}),
                ...(form.clues.linkedinUrl
                  ? { linkedin_url: form.clues.linkedinUrl }
                  : {}),
              },
              ...(form.fields.length ||
              (object(versionItem) && Object.hasOwn(versionItem, "props"))
                ? { props: fieldProps }
                : {}),
              ...(history
                ? {
                    work_history: {
                      ...(history.link ? { link: history.link } : {}),
                      match: history.match,
                      ...(Object.keys(historyProps).length ||
                      (object(versionItem?.work_history) &&
                        Object.hasOwn(versionItem.work_history, "props"))
                        ? { props: historyProps }
                        : {}),
                    },
                  }
                : {}),
            },
          },
        }),
      },
    },
  ];
  return edits.reduce((all, e) => stageEdit(bundle, all, e), kept);
}

/**
 * The pending edits without `itemId`'s. A type gains props, and a file a
 * link, only while the People enrichment item fills them, so undoing the
 * item, the new link its work history adds or its type's put stages the
 * enrichment again as the other edits leave it: the version's item or off,
 * on without work history, or on with the type as the version has it plus
 * what the item needs of it (its props and clue identifiers). Undoing the
 * item or the link keeps the identifiers staged by hand (`before`, as
 * `stageEnrichment` takes it); undoing the type drops them.
 *
 * @example
 * const pending = stageEnrichment(bundle, [], "Person", enrichmentDefaults(bundle, "Person"));
 * undoEdit(bundle, pending, "enrichment.people"); // [] for a version without an item
 * undoEdit(bundle, pending, "type.Person"); // pending again: the item needs that put
 */
export function undoEdit(
  bundle: AgentBundle,
  edits: wfe.OntologyEdit[],
  itemId: string,
  before?: Readonly<Record<string, string>>,
): wfe.OntologyEdit[] {
  const rest = edits.filter((e) => editItem(e) !== itemId),
    kind = itemId.split(".")[0];
  if (kind !== "enrichment" && kind !== "link" && kind !== "type") return rest;
  const type = enrichedType(bundle, edits) ?? enrichedType(bundle, rest);
  if (type === undefined || (kind === "type" && itemId !== `type.${type}`))
    return rest;
  // Staging the item again replaces it, and drops what only the undone try
  // staged; a link's or the type's undo keeps the item as the edits have it.
  return stageEnrichment(
    bundle,
    kind === "enrichment" ? edits : rest,
    type,
    enrichmentForm(bundle, type, rest),
    kind === "type" ? undefined : before,
  );
}

/**
 * Why a route's `documents` can't read what it names: the engine checks routes
 * without the ontology's document sets, and loads read nothing from an id
 * no set has, so the page checks them against the sets (`sets`, id to
 * name). Undefined for any other edit, or a route whose sets all exist.
 *
 * @example
 * const route = { put: { itemId: "extraction.r", toml: '[extractions.r]\ndocuments = ["General"]' } };
 * routeSetRefusal(route, new Map([["set_1", "General"]])); // "General is a set's name. Use its id: set_1."
 */
export function routeSetRefusal(
  edit: wfe.OntologyEdit,
  sets: ReadonlyMap<string, string>,
): string | undefined {
  if (!edit.put?.itemId.startsWith("extraction.")) return undefined;
  const name = edit.put.itemId.slice("extraction.".length);
  const route = (
    toml.parse(edit.put.toml).extractions as Record<
      string,
      { documents?: unknown }
    >
  )[name];
  const ids = Array.isArray(route?.documents) ? route.documents : [];
  const unknown = ids.find((id) => !sets.has(String(id)));
  if (unknown === undefined) return undefined;
  const named = [...sets].find(([, n]) => n === unknown);
  return named
    ? `${unknown} is a set's name. Use its id: ${named[0]}.`
    : `No document set has the id ${unknown}.`;
}

/** At most one pending operation per item; putting its original value undoes it. */
export function stageEdit(
  bundle: AgentBundle,
  edits: wfe.OntologyEdit[],
  edit: wfe.OntologyEdit,
): wfe.OntologyEdit[] {
  const itemId = editItem(edit);
  const rest = edits.filter((e) => editItem(e) !== itemId);
  const original = editItems(bundle)[itemId];
  if (
    edit.put &&
    original &&
    canonical(toml.parse(original)) === canonical(toml.parse(edit.put.toml))
  )
    return rest;
  return [...rest, edit];
}

/**
 * Creation is enabled only for a successful preview of this exact edit
 * batch, read whole (GET .../generations/{id}: lists leave the result out).
 * A preview whose result is withheld (the caller can't query the
 * ontology's database) succeeded only if it revised: Revise fails a refusal.
 */
export function canCreateRevision(
  g: wfe.OntologyGeneration | undefined | null,
  snapshot: string,
  edits: wfe.OntologyEdit[],
): boolean {
  return (
    !!edits.length &&
    snapshot === JSON.stringify(edits) &&
    g?.revision?.dryRun === true &&
    g.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" &&
    !g.resultVersionId &&
    (g.revision.resultWithheld ||
      readRevisionResult(g.revision.resultJson)?.status === "revised")
  );
}

/** Validate the backend's result and rule counts; preserve engine evidence verbatim. */
export function readRevisionResult(
  raw: string | undefined,
): wfe.OntologyReviseResult | undefined {
  if (!raw) return undefined;
  try {
    const value: unknown = JSON.parse(raw);
    if (
      !object(value) ||
      !["revised", "refused"].includes(value.status) ||
      !Array.isArray(value.findings) ||
      !Array.isArray(value.cards)
    )
      return undefined;
    if (Object.hasOwn(value, "definition")) return undefined;
    if (
      value.findings.some(
        (f: unknown) =>
          !object(f) ||
          ![
            "EditNotAllowed",
            "InvalidDefinition",
            "InvalidEdit",
            "Validation",
            "Evaluation",
            "Lint",
            "no_comparable_load",
            "budget",
          ].includes(f.code) ||
          !(f.item_id === null || typeof f.item_id === "string") ||
          typeof f.message !== "string",
      )
    )
      return undefined;
    if (
      value.cards.some(
        (c: unknown) =>
          !object(c) ||
          typeof c.item !== "string" ||
          !c.item ||
          !["rule", "warn", "refused"].includes(c.verdict) ||
          !object(c.card) ||
          ![
            undefined,
            null,
            "rows",
            "documents",
            "rows, documents",
            "load",
            "entities",
          ].includes(c.card.unmeasured),
      )
    )
      return undefined;
    // Measuring past its budget refuses the revision: nothing is accepted.
    if (
      value.status === "revised" &&
      value.findings.some((f: { code: string }) => f.code === "budget")
    )
      return undefined;
    const rules = readRuleCounts(value.rules);
    if (
      value.status === "revised" &&
      (!rules || rules.evaluated !== rules.count)
    )
      return undefined;
    if (value.status === "refused" && Object.hasOwn(value, "rules"))
      return undefined;
    const evidence = { findings: value.findings, cards: value.cards };
    return value.status === "revised"
      ? { ...evidence, status: "revised", rules: rules! }
      : { ...evidence, status: "refused" };
  } catch {
    return undefined;
  }
}
