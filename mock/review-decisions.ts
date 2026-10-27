import { parse } from "smol-toml";
import type {
  FoundryPropValue,
  FoundryReviewDecision,
} from "../src/shared/wfe";
import { normName } from "./graph/extract";
import { onReset } from "./state";

// Ontology-owned decisions, which survive new loads. A load reads the
// decisions saved when it is created (the backend's foundry_load_decisions): its
// load, extract and retry jobs all read that copy, so settling or removing
// one never changes a load already started.
export const decisions = new Map<string, FoundryReviewDecision[]>();
/** A copy of an ontology's decisions now, for a load being created. */
export const decisionsFor = (o: string): FoundryReviewDecision[] => [
  ...(decisions.get(o) || []),
];
onReset(() => decisions.clear());

export type DecisionTypes = Record<
  string,
  { props: Record<string, string>; key: string[] }
>;
/** A version's types, their props' types and keys; none when it doesn't parse. */
export function decisionTypes(
  definition: Record<string, unknown> | undefined,
): DecisionTypes {
  try {
    const doc = parse(String(definition?.ontology_toml || "")) as {
      types?: Record<
        string,
        { props?: Record<string, string>; key?: string[] }
      >;
    };
    return Object.fromEntries(
      Object.entries(doc.types || {}).map(([t, v]) => [
        t,
        { props: v.props || {}, key: v.key || [] },
      ]),
    );
  } catch {
    return {};
  }
}

// schema's is_iso_date: YYYY, YYYY-MM or YYYY-MM-DD with a real month and day.
const isDate = (s: string) => {
  const m = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(s);
  if (!m) return false;
  if (m[2] === undefined) return true;
  const month = +m[2];
  if (month < 1 || month > 12) return false;
  if (m[3] === undefined) return true;
  const d = new Date(Date.UTC(+m[1]!, month - 1, +m[3]));
  d.setUTCFullYear(+m[1]!);
  return d.getUTCDate() === +m[3] && d.getUTCMonth() === month - 1;
};

// An RFC 3339 instant as schema keeps a datetime: UTC, Z, its fraction in
// groups of three; undefined when it isn't one or its UTC year isn't 4 digits.
function dateTime(s: string): string | undefined {
  const m =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|([+-])(\d{2}):(\d{2}))$/.exec(
      s,
    );
  if (!m || !isDate(`${m[1]}-${m[2]}-${m[3]}`)) return undefined;
  const [h, min, sec] = [+m[4]!, +m[5]!, +m[6]!];
  if (h > 23 || min > 59 || sec > 59) return undefined;
  const offset =
    m[9] === undefined ? 0 : (m[9] === "-" ? -1 : 1) * (+m[10]! * 60 + +m[11]!);
  const at = new Date(Date.UTC(+m[1]!, +m[2]! - 1, +m[3]!, h, min, sec));
  at.setUTCFullYear(+m[1]!);
  at.setUTCMinutes(at.getUTCMinutes() - offset);
  const year = at.getUTCFullYear();
  if (year < 0 || year > 9999) return undefined;
  const ns = (m[7] || "").padEnd(9, "0");
  const fraction =
    ns === "000000000"
      ? ""
      : ns.endsWith("000000")
        ? `.${ns.slice(0, 3)}`
        : ns.endsWith("000")
          ? `.${ns.slice(0, 6)}`
          : `.${ns}`;
  return `${at.toISOString().slice(0, 19)}${fraction}Z`;
}

const INT64 = { min: -(2n ** 63n), max: 2n ** 63n - 1n };
/** Whether `s` is an int64's decimal digits, as protobuf's JSON reads one. */
export const isInt64 = (s: string) =>
  /^-?\d+$/.test(s) && BigInt(s) >= INT64.min && BigInt(s) <= INT64.max;

/**
 * A chosen value as the engine keeps it, as the backend answers it, or undefined
 * when it refuses it (the backend's FoundryPropType.DecisionValue, on the JSON the
 * arm writes): null removes the prop; text is trimmed and not blank, a date
 * YYYY, YYYY-MM or YYYY-MM-DD, a datetime kept in UTC; an integer prop
 * takes an integer literal within int64, a float one any finite number; no
 * coercion between JSON kinds. A whole number within int64 reads back as
 * integer, even a float prop's.
 */
export function decisionValue(
  type: string,
  v: FoundryPropValue,
): FoundryPropValue | undefined {
  if (v.null !== undefined) return { null: null };
  if (v.text !== undefined) {
    const text = v.text.trim();
    switch (type) {
      case "string":
        return text ? { text } : undefined;
      case "date":
        return isDate(text) ? { text } : undefined;
      case "datetime": {
        const kept = dateTime(text);
        return kept === undefined ? undefined : { text: kept };
      }
      default:
        return undefined;
    }
  }
  if (v.boolean !== undefined)
    return type === "boolean" ? { boolean: v.boolean } : undefined;
  // The number's JSON as the backend writes the arm: an integer's digits, or Go's
  // shortest float, which is JavaScript's below 1e21.
  const raw = v.integer ?? String(v.number);
  if (type === "integer")
    return isInt64(raw) ? { integer: BigInt(raw).toString() } : undefined;
  const n = Number(raw);
  if (type !== "float" || !Number.isFinite(n)) return undefined;
  return Number.isInteger(n) && isInt64(BigInt(n).toString())
    ? { integer: BigInt(n).toString() }
    : { number: n };
}

/**
 * Whether the ontology of `types` still takes a decision, as the backend's
 * FoundryReviewDecision.Fits does: its type is there, its entity key names
 * an entity of that type (a named type's normalized name; the mock keeps a
 * keyed type's key as the row's, not the engine's tagged encoding, so any
 * key does), and a prop decision's prop is there with a value of its type.
 */
export function decisionFits(
  d: FoundryReviewDecision,
  types: DecisionTypes,
): boolean {
  const t = types[d.type];
  if (!t) return false;
  const key = d.entityKey;
  if (key !== undefined && !(key && (t.key.length || normName(key) === key)))
    return false;
  if (d.kind === "resolve")
    return !!d.docId.trim() && normName(d.normName) === d.normName;
  const type = t.props[d.prop];
  return type !== undefined && decisionValue(type, d.value) !== undefined;
}

/**
 * Whether an Extract of the ontology of `types` takes a decision, as the backend's
 * FoundryReviewDecision.Extractable does: every decision that fits, and a
 * resolve of a type still there whose entity key the type's key no longer
 * reads. The engine keeps that resolve, so the next load reviews its name
 * again rather than matching it another way.
 */
export function decisionExtractable(
  d: FoundryReviewDecision,
  types: DecisionTypes,
): boolean {
  if (d.kind !== "resolve") return decisionFits(d, types);
  return (
    d.type in types && !!d.docId.trim() && normName(d.normName) === d.normName
  );
}

/**
 * Drops the decisions a version just made current doesn't take
 * (decisionExtractable), as the backend's SetCurrentOntologyVersion does: only an
 * agent draft whose ontology names types prunes. Loads already made keep
 * the copy they read.
 */
export function pruneDecisions(
  ontologyId: string,
  version: { format?: string; definition?: Record<string, unknown> },
) {
  if (version.format !== "ONTOLOGY_FORMAT_AGENT_BUNDLE") return;
  const types = decisionTypes(version.definition);
  if (!Object.keys(types).length) return;
  const own = decisions.get(ontologyId);
  if (own)
    decisions.set(
      ontologyId,
      own.filter((d) => decisionExtractable(d, types)),
    );
}
