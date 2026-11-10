// What the Suggestions page shows of a self-healing finding: the ontology
// agent's Finding JSON (contract/ontology_agent/audit_result.json
// `$defs/Finding`), read without trust, in words. A kind, proposal or
// field a later contract adds reads as what it is called, never as an
// error.
import type {
  FoundryDocumentSet,
  FoundryHealFinding,
  FoundryHealRun,
  ListHealFindingsResponse,
} from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";
import { MAX_GOAL } from "../ontology/model";

/** One review behind a finding: its document and the text it is about. */
export type SuggestionExample = { docId: string; quote: string };

/** A finding, as a card shows it. */
export type Suggestion = {
  fingerprint: string;
  // The engine's kind, and its label.
  kind: string;
  kindLabel: string;
  // What the finding says, and the detail under it.
  title: string;
  detail?: string;
  // Reviews (a row finding's rows) and documents; documents is 0 for rows.
  support: number;
  documents: number;
  // Reviews' quotes, or a row finding's row errors (docId "").
  examples: SuggestionExample[];
  // The forms a relationship's reports took (contract 4.8.0's spellings):
  // shown beside the quotes when there are more than one, since only a
  // person's reading tells whether they belong together.
  spellings: string[];
  // What it proposes, in words; none for a finding that proposes nothing.
  proposal?: string;
  // The goal Generate's form is filled with to add a link for it: only a
  // relationship no link covers (`unmatched`) that names one has it.
  goal?: string;
  // The load its support was counted over: another than the current one
  // until the current load is audited.
  loadId: string;
};

const KIND_LABELS: Record<string, string> = {
  unmatched: "Relationship no link covers",
  unresolved: "Unresolved name",
  clash: "Entities that clash",
  row_errors: "Rows that didn't load",
  source_drift: "Source changed",
};

const text = (v: unknown) => (typeof v === "string" ? v : "");
const texts = (v: unknown) =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
const count = (v: unknown) =>
  typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : 0;
/** An int64 the WFE writes as its digits; 0 when it isn't one. */
const int64 = (v: string | undefined) => {
  const n = Number(v);
  return Number.isSafeInteger(n) && n >= 0 ? n : 0;
};
/** A canonical name in words: `supplier_of` reads "supplier of". */
const words = (canonical: string) => canonical.replaceAll("_", " ");
const quoted = (s: string) => `“${s}”`;
const list = (items: string[]) =>
  items.length <= 1
    ? items.join("")
    : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;

/** An entity key as the engine writes one: its type and name or key. */
function entity(v: unknown): string {
  if (!isRecord(v)) return "an entity";
  const id = text(v.norm_name) || text(v.key);
  return id ? `${text(v.type) || "entity"} ${quoted(id)}` : "an entity";
}

function kindLabel(kind: string) {
  return KIND_LABELS[kind] || words(kind) || "Finding";
}

/** What a signal says, by its kind; set names a document set by its id. */
function said(
  kind: string,
  s: Record<string, unknown>,
  set: (id: string) => string,
) {
  switch (kind) {
    case "unmatched": {
      const canonical = text(s.canonical);
      return {
        title: `Documents state ${quoted(words(canonical))}, which no link covers`,
        detail: text(s.set) ? `In document set ${set(text(s.set))}` : undefined,
      };
    }
    case "unresolved": {
      const now = isRecord(s.now) ? s.now : {},
        resolves = text(now.resolves);
      return {
        title: `Documents name a ${text(s.type) || "thing"} ${quoted(text(s.norm_name))} that resolved to no one`,
        detail:
          resolves === "one"
            ? `It resolves to ${entity(now.entity)} now`
            : resolves === "many"
              ? `It resolves to ${Array.isArray(now.entities) ? now.entities.length : "several"} entities now`
              : resolves === "none"
                ? "No entity has the name"
                : undefined,
      };
    }
    case "clash": {
      const clash = isRecord(s.clash) ? s.clash : {},
        entities = Array.isArray(clash.entities)
          ? clash.entities.map(entity)
          : [];
      switch (text(clash.clash)) {
        case "ambiguous":
          return {
            title: "Entities share a name documents use",
            detail: list(entities),
          };
        case "identifier_conflict":
          return {
            title: "A document's identifiers point at different entities",
            detail: list(entities),
          };
        case "collision":
          return {
            title: `Two names a document uses resolve to ${entity(clash.entity)}`,
          };
      }
      return { title: "Entities clash" };
    }
    case "row_errors":
      return {
        title: `Rows of ${quoted(text(s.mapping))} didn't load`,
      };
    case "source_drift": {
      const drift = isRecord(s.drift) ? s.drift : {},
        added = texts(drift.added),
        removed = texts(drift.removed);
      return {
        title: `The source ${quoted(text(s.mapping))} reads changed`,
        detail: [
          added.length ? `Gained ${list(added)}` : "",
          removed.length ? `lost ${list(removed)}` : "",
        ]
          .filter(Boolean)
          .join("; "),
      };
    }
  }
  return { title: kindLabel(kind) };
}

/** What a proposal proposes, in words; set names a document set by its id. */
function proposed(p: unknown, set: (id: string) => string): string | undefined {
  if (!isRecord(p)) return undefined;
  switch (text(p.proposal)) {
    case "route": {
      const links = texts(p.links);
      return `Let documents of ${text(p.set) ? set(text(p.set)) : "the set"} state ${list(links.map(quoted))}`;
    }
    case "draft": {
      const brief = isRecord(p.brief) ? p.brief : {};
      switch (text(brief.brief)) {
        case "description":
          return `Describe ${quoted(text(brief.link))} so documents use it`;
        case "mapping":
          return `Map ${quoted(text(brief.mapping))} again`;
        case "link":
          return `Add a link for ${list(texts(brief.names).map((n) => quoted(words(n))))}`;
      }
      return "Draft a change";
    }
    case "ask":
      return "A person decides what to change";
  }
  return undefined;
}

/**
 * A stored finding as its card shows it. Support and documents are the
 * stored sums; examples are a backing's quotes, or a row finding's row
 * errors. A document set is named by its name in sets, or by its id when
 * sets doesn't hold it.
 *
 * @example
 * suggestion({ fingerprint: "ab", kind: "unmatched", status: "open", support: "12", sources: "5",
 *   findingJson: '{"signal":{"kind":"unmatched","canonical":"supplier_of","set":"set_1","backing":{"count":12,"sources":5,"examples":[]}}}' })
 *   .title; // "Documents state “supplier of”, which no link covers"
 */
export function suggestion(
  f: FoundryHealFinding,
  sets: readonly FoundryDocumentSet[] = [],
): Suggestion {
  const set = (id: string) => {
    const name = sets.find((s) => s.id === id)?.name;
    return name ? quoted(name) : id;
  };
  let finding: unknown;
  try {
    finding = JSON.parse(f.findingJson);
  } catch {
    finding = undefined;
  }
  const record = isRecord(finding) ? finding : {},
    signal = isRecord(record.signal) ? record.signal : {},
    kind = text(signal.kind) || f.kind;
  const backing = isRecord(signal.backing) ? signal.backing : undefined,
    errors = isRecord(signal.errors) ? signal.errors : undefined;
  const examples: SuggestionExample[] = backing
    ? (Array.isArray(backing.examples) ? backing.examples : [])
        .filter(isRecord)
        .map((e) => ({ docId: text(e.doc_id), quote: text(e.quote) }))
        .filter((e) => e.quote)
    : texts(errors?.examples).map((quote) => ({ docId: "", quote }));
  const proposal = proposed(record.proposal, set),
    spellings = texts(signal.spellings),
    canonical = kind === "unmatched" ? text(signal.canonical) : "";
  return {
    fingerprint: f.fingerprint,
    kind,
    kindLabel: kindLabel(kind),
    ...said(kind, signal, set),
    support: int64(f.support) || count(backing?.count) || count(errors?.count),
    documents: int64(f.sources),
    loadId: f.loadId || "",
    examples,
    spellings,
    proposal,
    goal: canonical
      ? linkGoal({
          canonical,
          spellings,
          set: text(signal.set) ? set(text(signal.set)) : "",
          proposal,
          quotes: examples.map((e) => e.quote),
        })
      : undefined,
  };
}

// How many of a suggestion's quotes its goal gives.
const GOAL_QUOTES = 3;

/**
 * The goal that asks Generate to add a link for a relationship no link
 * covers: what the finding proposes when it proposes something, else a link
 * for its canonical name, in words, with the other spellings it was
 * reported as, then up to GOAL_QUOTES quotes, one a line, whitespace in
 * each collapsed. At most MAX_GOAL code points (as the WFE counts): the
 * most whole quotes that fit, else the first one, cut short with "…".
 *
 * @example
 * linkGoal({ canonical: "supplier_of", spellings: ["supplier_of"], set: "“General”",
 *   quotes: ["Globex supplies Acme."] });
 * // "Add a link for “supplier of”: documents of “General” state it.\nFor example:\n“Globex supplies Acme.”"
 */
function linkGoal({
  canonical,
  spellings,
  set,
  proposal,
  quotes,
}: {
  canonical: string;
  spellings: string[];
  // The documents' set, named; "" for none.
  set: string;
  proposal?: string;
  quotes: string[];
}): string {
  const others = spellings.filter((s) => s !== canonical),
    also = others.length
      ? `, also reported as ${list(others.map((s) => quoted(words(s))))}`
      : "",
    name = quoted(words(canonical));
  const head = proposal
    ? `${proposal}: documents state ${name}${also}.`
    : `Add a link for ${name}: ${set ? `documents of ${set}` : "documents"} state it${also}.`;
  const shown = quotes
    .map((q) => q.replace(/\s+/g, " ").trim())
    .filter(Boolean)
    .slice(0, GOAL_QUOTES)
    .map(quoted);
  const goal = (n: number) =>
    [head, ...(n ? ["For example:", ...shown.slice(0, n)] : [])].join("\n");
  const fits = shown
    .map((_, i) => shown.length - i)
    .find((n) => [...goal(n)].length <= MAX_GOAL);
  const chars = [...goal(fits ?? Math.min(1, shown.length))];
  return chars.length <= MAX_GOAL
    ? chars.join("")
    : `${chars.slice(0, MAX_GOAL - 1).join("")}…`;
}

/**
 * The latest audit, in words; none before the first. One of another load
 * than `current`, the ontology's current one, says so; a complete one that
 * didn't measure the load's rows says they weren't checked; one that didn't
 * finish says the suggestions `shown` are from the audit before it.
 *
 * @example
 * lastRunText({ id: "r", loadId: "l1", status: "complete", pages: 1 }, { current: "l2" });
 * // "The last audit was of another load."
 */
export function lastRunText(
  run: FoundryHealRun | undefined,
  {
    current,
    shown = false,
    now = Date.now(),
  }: { current?: string; shown?: boolean; now?: number } = {},
): string | undefined {
  if (!run) return undefined;
  const at = Date.parse(run.finishedAt || run.startedAt || "");
  const when = Number.isFinite(at) ? ` ${ago(now - at)}` : "";
  if (current && run.loadId && run.loadId !== current)
    return `The last audit${when} was of another load.`;
  const before = shown ? ", so these are from the audit before it" : "";
  switch (run.status) {
    case "running":
      return `Auditing the current load now (started${when}).`;
    case "complete":
      return `Last audited${when}, ${run.pages || 0} ${run.pages === 1 ? "page" : "pages"} of documents.${run.rowsMeasured ? "" : " Rows weren’t checked on this load."}`;
    case "incomplete":
      return `The last audit${when} didn’t finish${before}; it will audit again soon.`;
    case "failed":
      return `The last audit${when} failed${before}; it will audit again soon.`;
  }
  return undefined;
}

/**
 * What the page says when it shows no suggestion, by the newest complete
 * audit of the ontology (`complete`): one of the current load left none
 * open, which isn't to say it found none (a person may have dismissed
 * them).
 *
 * @example
 * emptyText("l1", { id: "r", loadId: "l0", status: "complete" });
 * // "The current load hasn’t been fully audited yet."
 */
export function emptyText(
  current: string | undefined,
  complete: FoundryHealRun | undefined,
): string {
  if (!current)
    return "Load the ontology: each load is audited once it’s ready, and every night after.";
  if (complete?.loadId === current)
    return "No open suggestions from the last audit of the current load.";
  return "The current load hasn’t been fully audited yet.";
}

/**
 * Whether the list is still on its way, so the page asks again: audits run
 * here, the ontology has a current load, and an audit of it runs or none
 * of it has completed.
 *
 * @example
 * auditPending({ enabled: true, lastCompleteRun: { id: "r", loadId: "l1", status: "complete" } }, "l1"); // false
 */
export function auditPending(
  data: ListHealFindingsResponse | undefined,
  current: string | undefined,
): boolean {
  if (!data?.enabled || !current) return false;
  return (
    data.lastRun?.status === "running" ||
    data.lastCompleteRun?.loadId !== current
  );
}

function ago(ms: number) {
  const minutes = Math.max(0, Math.round(ms / 60_000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.round(hours / 24)} days ago`;
}
