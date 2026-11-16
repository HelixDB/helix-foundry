import type {
  FoundryAutomationSuggestion,
  FoundryAutomationSuggestionIcon,
  FoundryAutomationSuggestionKind,
  FoundryAutomationSuggestionSource,
  ListAutomationSuggestionsResponse,
  OntologyVersion,
} from "../../src/shared/wfe";
import {
  SUGGESTION_PROMPT_MAX as MAX_PROMPT,
  SUGGESTION_TITLE_MAX as MAX_TITLE,
  SUGGESTIONS_MAX as MAX_SUGGESTIONS,
} from "../../src/web/areas/automations/model";
import { versionFormat } from "../../src/web/areas/ontology/format";
import {
  founderSuggestions,
  SUGGESTION_KINDS,
  SUGGESTION_LIMITS,
  type SuggestionKind,
} from "../../src/web/areas/ontology/founder-model";
import {
  readAgentBundle,
  versionLayout,
} from "../../src/web/areas/ontology/model";
import {
  isSampleConnector,
  isSampleSet,
} from "../../src/web/areas/ontology/sample";
import { setLabel } from "../../src/web/areas/documents/model";
import { fail, ok, type MockRoute } from "../http";
import { config, isReply, membership, onReset } from "../state";
import { aiSource } from "./ai";
import { workspaceConnectors } from "./data";
import { listedSets } from "./documents";
import { ontologyById } from "./ontologies";
import {
  currentVersionOf,
  ontologyVersion,
  versionInputs,
  versionScope,
} from "./ontology";
import { openFindings } from "./suggestions";

// Founder's suggestions, as the backend serves them
// (api/automation_suggestions.go, the suggest package, Founder's worker):
// for an ontology's current version, each kind's in order (2 questions, 2
// dashboards, 2 changes to the ontology and 4 workflows: `COUNTS`), each
// filled to its count from (`kindFill`): questions, the version's
// competency questions, then the model's; dashboards, the model's, then
// "Build me a dashboard that answers: …" of the competency questions (those
// after the first 2 first, so they differ from the questions' cards); changes
// to the ontology, its setup (SETUP: its open "unmatched" audit findings by
// support, ./suggestions.ts, then the workspace's connections its version's
// generation didn't read, then the sets holding documents outside its scope;
// at most 5 of each, no API source or the sample's), then the model's; workflows, the model's, then the competency
// questions' "Every Monday, answer: …" (`fallbackSuggestions`, the
// backend's Fallback); then the fixed examples (founder-model.ts
// `founderSuggestions`, which the site shows where it reads none). Within a
// kind a prompt shows once, case aside, and every prompt passes `strict`.
// Without a current version, every kind is the fixed examples (the
// ontology's connect your data and draft its first version). Workflows are
// left out while the mock's workflows are off (`automationsDisabled`, from
// ./automations.ts); the route itself refuses only what any member route
// refuses (a grantee, by ./access.ts). The response's `source` is the first
// workflow's.
//
// The first read of a version queues the model's writing (the backend's 20 a
// workspace an hour aren't counted) and answers the fallback with
// `generating`; it is written `automationSuggestionsMs` (1500) after, which
// 0 makes the first read's. The mock's "model" decides by the version's
// types: contracts or invoices get contract suggestions of every kind,
// people theirs, any other none (the writing then fails: the fallbacks
// fill every kind). Knobs, through POST /__mock/config:
// `automationSuggestionsOutcome: "fail"` fails the writing (the fallback,
// not generating); `automationSuggestionsAi: false` is a workspace without
// an AI provider (as `aiSource` none, the local mock's `aiDefault: false`)
// and Founder off (`agentEnabled: false`) never writes them;
// `automationsDisabled: true` leaves the workflows out; `healEnabled: false`
// (./suggestions.ts) leaves the findings' changes out.

type Knobs = {
  automationSuggestionsMs?: number;
  automationSuggestionsOutcome?: "fail";
  automationSuggestionsAi?: boolean;
  agentEnabled?: boolean;
  automationsDisabled?: boolean;
};
const knobs = config as typeof config & Knobs;

// The backend's suggest constants; its limits are the site's (model.ts).
const MAX_QUESTIONS = 10,
  RETRY_AFTER_MS = 3_600_000;
const WRITE_MS = 1500;

type Row = {
  status: "queued" | "ready" | "failed";
  requestedAt: number;
  // When it turned ready or failed: a failed row is queued again an hour
  // after, as the backend counts from its updated_at.
  updatedAt: number;
  suggestions: FoundryAutomationSuggestion[];
};
const rows = new Map<string, Row>();
onReset(() => {
  rows.clear();
  delete knobs.automationSuggestionsMs;
  delete knobs.automationSuggestionsOutcome;
  delete knobs.automationSuggestionsAi;
});

const icon = (name: string) =>
  `FOUNDRY_AUTOMATION_SUGGESTION_ICON_${name.toUpperCase()}` as FoundryAutomationSuggestionIcon;
const runes = (s: string) => [...s];
// suggest.go's word lists: the interrogatives a title drops (longest
// first), the words after one that keep it, the words it never ends on.
const INTERROGATIVES = ["how many", "which", "what"];
const AUXILIARIES = new Set(
  "is are was were do does did has have had can could will would should shall may might must".split(
    " ",
  ),
);
// An interrogative, a copula and a determiner keep only the determiner's
// phrase ("What is the total revenue" is "The total revenue").
const COPULAS = new Set(["is", "are", "was", "were"]);
const DETERMINERS = new Set(
  "the each every a an our my your their its this these those all".split(" "),
);
const TRAILING = new Set(
  "a an the of and or to in for with on by at".split(" "),
);
const MAX_NAME = 100;
// suggest.go's collapse and cut.
const collapse = (s: string) => s.split(/\s+/).filter(Boolean).join(" ");
const cut = (s: string, limit: number) => {
  const r = runes(s);
  if (r.length <= limit) return s;
  return limit < 1
    ? ""
    : r
        .slice(0, limit - 1)
        .join("")
        .replace(/ +$/, "") + "…";
};
const trimEnd = (s: string, chars: string) => {
  let end = s.length;
  while (end > 0 && chars.includes(s[end - 1]!)) end--;
  return s.slice(0, end);
};

/**
 * A competency question as a suggestion's title, as the backend's suggest
 * package's Title writes it: its whitespace collapsed and closing
 * punctuation dropped; a leading "How many", "Which" or "What" dropped
 * unless an auxiliary follows or nothing does, and with the copula after it
 * when a determiner follows that ("What is the total revenue" is "The total
 * revenue"); cut after the last whole
 * word within 32 characters (a longer first word cut at it), then words
 * such as "the" and "of" and punctuation dropped from its end; its first
 * letter capitalised.
 *
 * @example
 * questionTitle("Which customers are under contract, and which contracts are they signed with?");
 * // "Customers are under contract"
 */
export function questionTitle(question: string): string {
  let q = trimEnd(collapse(question), "?.!,;: ");
  for (const word of INTERROGATIVES) {
    if (!q.toLowerCase().startsWith(word + " ")) continue;
    const rest = q.slice(word.length + 1),
      [next = "", then = ""] = rest.toLowerCase().split(" ");
    if (COPULAS.has(next) && DETERMINERS.has(then))
      q = rest.slice(next.length + 1);
    else if (rest && !AUXILIARIES.has(next)) q = rest;
    break;
  }
  const words = q.split(" ").filter(Boolean);
  if (!words.length) return "";
  const kept: string[] = [];
  let size = 0;
  for (const w of words) {
    const n = runes(w).length + (kept.length ? 1 : 0);
    if (size + n > MAX_TITLE) break;
    kept.push(w);
    size += n;
  }
  if (!kept.length) kept.push(runes(words[0]!).slice(0, MAX_TITLE).join(""));
  while (
    kept.length > 1 &&
    TRAILING.has(trimEnd(kept[kept.length - 1]!, ",;:").toLowerCase())
  )
    kept.pop();
  const title = trimEnd(kept.join(" "), "?.!,;: ");
  const [first = ""] = runes(title);
  return first.toUpperCase() + title.slice(first.length);
}

/**
 * The backend's Fallback: for the first 4 questions whose titles differ
 * (case aside), a report answering it every Monday by email, the question
 * cut with "…" where the prompt would pass 400 characters (the ontology's
 * name at 100).
 *
 * @example
 * fallbackSuggestions("Renewals", ["Who renews?"])[0].prompt;
 * // 'Every Monday, answer: "Who renews?" from Renewals, and email me the answer.'
 */
export function fallbackSuggestions(
  ontologyName: string,
  questions: readonly string[],
): FoundryAutomationSuggestion[] {
  const name = cut(collapse(ontologyName), MAX_NAME),
    before = 'Every Monday, answer: "',
    after = `" from ${name}, and email me the answer.`,
    room = MAX_PROMPT - runes(before).length - runes(after).length;
  const out: FoundryAutomationSuggestion[] = [],
    seen = new Set<string>();
  for (const question of questions) {
    if (out.length === MAX_SUGGESTIONS) break;
    const q = collapse(question),
      title = questionTitle(q);
    if (!title || seen.has(title.toLowerCase())) continue;
    seen.add(title.toLowerCase());
    out.push({
      title,
      prompt: before + cut(q, room) + after,
      icon: icon("report"),
    });
  }
  return out;
}

/**
 * The backend's suggest.Questions as the API reads them for the fallback:
 * an agent bundle's competency questions in order, each once, the first 10,
 * so the fallback reads past questions whose titles repeat.
 */
function versionQuestions(v: OntologyVersion): string[] {
  if (versionFormat(v) !== "agent_bundle") return [];
  return [
    ...new Set(
      readAgentBundle(v.definition)
        .competency.map((c) => c.question.trim())
        .filter(Boolean),
    ),
  ].slice(0, MAX_QUESTIONS);
}

/** How many of each kind the route gives, in order. */
const COUNTS: Record<SuggestionKind, number> = {
  question: 2,
  dashboard: 2,
  ontology: 2,
  workflow: MAX_SUGGESTIONS,
};
const KIND = (k: SuggestionKind) =>
  `FOUNDRY_AUTOMATION_SUGGESTION_KIND_${k.toUpperCase()}` as FoundryAutomationSuggestionKind;
type Source = "MODEL" | "QUESTIONS" | "SETUP" | "FIXED";
const SOURCE = (s: Source | "UNSPECIFIED") =>
  `FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_${s}` as FoundryAutomationSuggestionSource;
const kindOf = (x: FoundryAutomationSuggestion): SuggestionKind =>
  x.kind === KIND("question")
    ? "question"
    : x.kind === KIND("dashboard")
      ? "dashboard"
      : x.kind === KIND("ontology")
        ? "ontology"
        : "workflow";

/**
 * The backend's check of every suggestion, a model's or not (suggest.go
 * `Clean`): a title, prompt and topic within their limits, with no control
 * or invisible format character, link, markup or id (a ULID holding a
 * digit, `src_1a2b`, any `ont_…`); a dashboard's prompt says "dashboard",
 * a change's "draft" or "revise", and a workflow has an icon.
 *
 * @example
 * strict({ kind: "FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION", title: "Unpaid", prompt: "Which invoices are unpaid?", topic: "Revenue" }); // true
 * strict({ kind: "FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION", title: "Link", prompt: "Open https://example.com", topic: "Web" }); // false
 */
export function strict(x: FoundryAutomationSuggestion): boolean {
  const ok = (v: string | undefined, max: number) => {
    // eslint-disable-next-line no-control-regex
    if (!v || /[\u0000-\u001f\u007f-\u009f\p{Cf}]/u.test(v)) return false;
    const t = collapse(v),
      lower = t.toLowerCase();
    return (
      !!t &&
      runes(t).length <= max &&
      !/http|www\.|:\/\//.test(lower) &&
      !/[<>`]|\*\*|^#|\[[^\]]*\]\(/.test(t) &&
      !(t.match(/\b[0-9A-HJKMNP-TV-Z]{26}\b/gi) || []).some((x) =>
        /[0-9]/.test(x),
      ) &&
      !/\b[a-z]{2,10}_[a-z0-9_-]*[0-9][a-z0-9_-]*\b|\b(src|ont)_\w+/i.test(t)
    );
  };
  const word = (w: string) =>
    new RegExp(`(?:^|[^\\p{L}\\p{N}_])(?:${w})(?:$|[^\\p{L}\\p{N}_])`, "iu");
  const kind = kindOf(x);
  return (
    ok(x.title, SUGGESTION_LIMITS.title) &&
    ok(x.prompt, SUGGESTION_LIMITS.prompt) &&
    ok(x.topic, SUGGESTION_LIMITS.topic) &&
    (kind !== "dashboard" || word("dashboards?").test(x.prompt!)) &&
    (kind !== "ontology" || word("draft|revise").test(x.prompt!)) &&
    (kind !== "workflow" ||
      (!!x.icon && x.icon !== "FOUNDRY_AUTOMATION_SUGGESTION_ICON_UNSPECIFIED"))
  );
}

/** A suggestion of a kind, as the route answers it (a workflow's icon only). */
const make = (
  kind: SuggestionKind,
  source: Source,
  title: string,
  prompt: string,
  topic: string,
  iconName = "report",
): FoundryAutomationSuggestion => ({
  title: cut(collapse(title), MAX_TITLE),
  prompt,
  icon:
    kind === "workflow"
      ? icon(iconName)
      : "FOUNDRY_AUTOMATION_SUGGESTION_ICON_UNSPECIFIED",
  kind: KIND(kind),
  topic,
  source: SOURCE(source),
});

/** The fixed examples, of each kind, named for the ontology. */
const fixed = (name: string, versioned: boolean) => {
  const icons = ["briefing", "refresh", "check", "report", "alert"];
  let w = 0;
  return founderSuggestions(name, versioned).map((x) =>
    make(
      x.kind,
      "FIXED",
      x.title,
      x.prompt,
      x.topic,
      x.kind === "workflow" ? icons[w++] : undefined,
    ),
  );
};

/**
 * What the mock's model writes for a version, by its types: every kind, for
 * the ontology named; undefined for types it has nothing for.
 */
export function modelSuggestions(
  name: string,
  types: readonly string[],
): FoundryAutomationSuggestion[] | undefined {
  const has = (t: string) => types.includes(t);
  type Row = [
    kind: SuggestionKind,
    title: string,
    topic: string,
    prompt: string,
    icon?: string,
  ];
  const rows: Row[] | undefined =
    has("Contract") || has("Invoice")
      ? [
          [
            "question",
            "Contracts renewing soon",
            "Renewals",
            `Which contracts in ${name} renew in the next 90 days, and what is each customer billed?`,
          ],
          [
            "question",
            "Past-due invoices",
            "Revenue",
            `Which customers in ${name} have invoices that are past due, and how much do they owe?`,
          ],
          [
            "dashboard",
            "Revenue by customer",
            "Revenue",
            `Build me a dashboard of revenue in ${name}: invoices by status, the largest customers by amount billed, and what is overdue.`,
          ],
          [
            "dashboard",
            "Renewals pipeline",
            "Renewals",
            `Build me a dashboard of contracts in ${name} by renewal month, with each one’s customer and term.`,
          ],
          [
            "ontology",
            "Link invoices to contracts",
            "Billing",
            `Revise ${name} so each invoice links to the contract it is billed under.`,
          ],
          [
            "ontology",
            "Track contract owners",
            "Accounts",
            `Draft a new version of ${name} that records who owns each contract on our side.`,
          ],
          [
            "workflow",
            "Renewal radar",
            "Renewals",
            `Every morning at 8am, find contracts in ${name} that renew in the next 30 days and email me each one with its customer and what they are billed.`,
            "watch",
          ],
          [
            "workflow",
            "Contract check",
            "Data quality",
            `When new documents arrive in a document set that ${name} reads, check each new contract's term and signing date against the customer it names, and email me any that don't match.`,
            "check",
          ],
          [
            "workflow",
            "Invoice watch",
            "Revenue",
            `Every day, look in ${name} for invoices that are past due or failed and, if there are any, email me which customers they belong to.`,
            "alert",
          ],
          [
            "workflow",
            "Monday contracts brief",
            "Briefing",
            `Every Monday at 9am, summarise what changed in ${name} last week: new contracts, renewals coming up and invoices raised, and email it to me.`,
            "briefing",
          ],
        ]
      : has("Person")
        ? [
            [
              "question",
              "Recent job changes",
              "People",
              `Which people in ${name} changed title or company in the last 90 days?`,
            ],
            [
              "question",
              "Contacts per company",
              "Accounts",
              `How many contacts does ${name} hold at each company?`,
            ],
            [
              "dashboard",
              "People by company",
              "Accounts",
              `Build me a dashboard of ${name}: people by company, the newest contacts and recent job changes.`,
            ],
            [
              "dashboard",
              "Contact coverage",
              "Data quality",
              `Build me a dashboard that shows which people in ${name} are missing a title or a company.`,
            ],
            [
              "ontology",
              "Add work history",
              "People",
              `Draft a new version of ${name} that keeps each person’s past companies as their work history.`,
            ],
            [
              "ontology",
              "Link people to deals",
              "Sales",
              `Revise ${name} so each person links to the deals they are part of.`,
            ],
            [
              "workflow",
              "New contacts brief",
              "Briefing",
              `Every Monday at 9am, list the people added to ${name} last week with the company each works at, and email it to me.`,
              "briefing",
            ],
            [
              "workflow",
              "Job change alert",
              "People",
              `Every day, look in ${name} for people whose title or company changed and, if there are any, email me who and what changed.`,
              "alert",
            ],
            [
              "workflow",
              "Account map refresh",
              "Sync",
              `Every night at 2am, sync the connections that ${name} reads, load it again once they finish, and email me if either fails.`,
              "refresh",
            ],
            [
              "workflow",
              "Weekly people report",
              "Reporting",
              `Every Friday at 4pm, answer "How many people work at each company?" from ${name}, and email me the answer.`,
              "report",
            ],
          ]
        : undefined;
  return rows?.map(([kind, title, topic, prompt, i]) =>
    make(kind, "MODEL", title, prompt, topic, i),
  );
}

/** The backend's "1 claim … states", "12 claims … state". */
const claims = (n: number) =>
  n === 1
    ? "which 1 claim in your documents states"
    : `which ${n} claims in your documents state`;

/** The most of each of the setup's lists (suggest.MaxSetup). */
const MAX_SETUP = 5;
/** A title when it fits, else the short one (fill.go's titled). */
const titled = (title: string, short: string) =>
  runes(title).length <= MAX_TITLE ? title : short;

/**
 * The changes to the ontology its setup suggests (fill.go's SetupOf and
 * Fill), at most 5 of each, in order: a link for each open "unmatched"
 * audit finding, by support, each relationship once; then each connection
 * of the workspace its version's generation didn't read; then each set
 * holding documents outside the version's scope, a connection's named with
 * it. API sources and their sets are left out, as are the sample company's
 * connection and set.
 */
function setupSuggestions(
  ws: string,
  ontologyId: string,
  versionId: string,
  name: string,
): FoundryAutomationSuggestion[] {
  const words: { words: string; claims: number }[] = [];
  for (const f of openFindings(ontologyId)) {
    if (f.kind !== "unmatched" || words.length === MAX_SETUP) continue;
    let canonical: unknown;
    try {
      canonical = (
        JSON.parse(f.findingJson || "{}") as {
          signal?: { canonical?: unknown };
        }
      ).signal?.canonical;
    } catch {
      continue;
    }
    const w = collapse(typeof canonical === "string" ? canonical : "")
      .replace(/_/g, " ")
      .trim();
    if (!w || words.some((x) => x.words === w)) continue;
    words.push({ words: w, claims: Number(f.support) || 0 });
  }
  const read = new Set(
    (versionInputs(ontologyId, versionId)?.tables || []).map(
      (t) => t.connectorId,
    ),
  );
  const sources = workspaceConnectors(ws)
    .filter(
      (c) => !read.has(c.id) && c.kind !== "events" && !isSampleConnector(c),
    )
    .slice(0, MAX_SETUP);
  const scope = new Set(versionScope(ontologyId, versionId));
  const sets = listedSets(ws)
    .filter(
      (x) =>
        Number(x.documents) > 0 &&
        !scope.has(x.id) &&
        x.connector?.connectorKind !== "events" &&
        !isSampleSet(x),
    )
    .slice(0, MAX_SETUP);
  return [
    ...words.map((l) =>
      make(
        "ontology",
        "SETUP",
        titled(`Add the “${l.words}” link`, "Add a missing link"),
        `Draft a new version of ${name} with a link for “${l.words}”, ${claims(l.claims)}.`,
        "Missing link",
      ),
    ),
    ...sources.map((c) =>
      make(
        "ontology",
        "SETUP",
        titled(`Add ${collapse(c.name || "")}`, "Add a connection"),
        `Draft a new version of ${name} that also reads ${collapse(c.name || "")}.`,
        "New source",
      ),
    ),
    ...sets.map((x) =>
      make(
        "ontology",
        "SETUP",
        titled(`Add ${collapse(setLabel(x))}`, "Add a document set"),
        `Draft a new version of ${name} that also reads the documents in ${collapse(setLabel(x))}.`,
        "New documents",
      ),
    ),
  ];
}

/**
 * Each kind filled to its count from its sources in order (each source's
 * suggestions of that kind): a prompt seen before in its kind (case aside),
 * or one that fails `strict`, is skipped.
 */
function kindFill(
  from: Record<SuggestionKind, readonly FoundryAutomationSuggestion[][]>,
  workflows: boolean,
): FoundryAutomationSuggestion[] {
  return SUGGESTION_KINDS.filter((k) => workflows || k !== "workflow").flatMap(
    (kind) => {
      const seen = new Set<string>(),
        out: FoundryAutomationSuggestion[] = [];
      for (const x of from[kind].flat()) {
        if (out.length === COUNTS[kind]) break;
        if (kindOf(x) !== kind) continue;
        const key = (x.prompt || "").toLowerCase();
        if (seen.has(key) || !strict(x)) continue;
        seen.add(key);
        out.push(x);
      }
      return out;
    },
  );
}

/** The competency questions' questions, as the Questions cards ask them. */
const questionSuggestions = (questions: readonly string[]) =>
  questions.flatMap((q) => {
    const question = cut(collapse(q), MAX_PROMPT),
      title = questionTitle(question);
    return title
      ? [make("question", "QUESTIONS", title, question, "Question")]
      : [];
  });

/**
 * The competency questions' dashboards: those after the first 2 first (the
 * Questions cards ask those), then the first 2.
 */
function dashboardSuggestions(questions: readonly string[]) {
  const before = "Build me a dashboard that answers: ",
    order = [...questions.slice(2), ...questions.slice(0, 2)];
  return order.flatMap((q) => {
    const question = collapse(q),
      title = questionTitle(question);
    return title
      ? [
          make(
            "dashboard",
            "QUESTIONS",
            title,
            before + cut(question, MAX_PROMPT - runes(before).length),
            "Dashboard",
          ),
        ]
      : [];
  });
}

export const automationSuggestionRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automation-suggestions",
    handle: (req) => {
      const ws = req.params.workspaceId!;
      const m = membership(ws, req.principal.user.id);
      if (isReply(m)) return m;
      const ontologyId = req.query.get("ontologyId") || "";
      if (!ontologyId) return fail("INVALID_ARGUMENT", "ontology_id required");
      const o = ontologyById(ontologyId);
      if (o?.workspaceId !== ws) return fail("NOT_FOUND", "ontology not found");
      const workflows = !knobs.automationsDisabled,
        name = cut(collapse(o.name), MAX_NAME);
      const versionId = currentVersionOf(o.id) || "",
        v = versionId ? ontologyVersion(o.id, versionId) : undefined;
      const answer = (
        suggestions: FoundryAutomationSuggestion[],
        generating = false,
      ) => {
        const first = suggestions.find((x) => kindOf(x) === "workflow");
        return ok({
          suggestions,
          source: first?.source || SOURCE("UNSPECIFIED"),
          generating,
          versionId,
        } satisfies ListAutomationSuggestionsResponse);
      };
      if (!v) {
        const f = fixed(name, false);
        return answer(
          kindFill(
            { question: [f], dashboard: [f], ontology: [f], workflow: [f] },
            workflows,
          ),
        );
      }
      const version = {
        id: v.id,
        format: v.format,
        definition: v.definition,
      } as OntologyVersion;
      const questions = versionQuestions(version),
        f = fixed(name, true);
      // Each kind's sources around the model's, in the backend's order.
      const fill = (
        model: FoundryAutomationSuggestion[],
        generating: boolean,
      ) =>
        answer(
          kindFill(
            {
              question: [questionSuggestions(questions), model, f],
              dashboard: [model, dashboardSuggestions(questions), f],
              ontology: [setupSuggestions(ws, o.id, versionId, name), model, f],
              workflow: [
                model,
                fallbackSuggestions(o.name, questions).map((x) => ({
                  ...x,
                  kind: KIND("workflow"),
                  topic: "Workflow",
                  source: SOURCE("QUESTIONS"),
                })),
                f,
              ],
            },
            workflows,
          ),
          generating,
        );
      const key = `${o.id}\0${versionId}`,
        now = Date.now(),
        wait = knobs.automationSuggestionsMs ?? WRITE_MS;
      let row = rows.get(key);
      if (
        !row ||
        (row.status === "failed" && now - row.updatedAt > RETRY_AFTER_MS)
      ) {
        if (
          knobs.agentEnabled === false ||
          knobs.automationSuggestionsAi === false ||
          aiSource(ws) === "none"
        )
          return fill([], false);
        row = {
          status: "queued",
          requestedAt: now,
          updatedAt: now,
          suggestions: [],
        };
        rows.set(key, row);
      }
      // The worker writes it once its time came, read or not. Ready always
      // holds some (the backend's Parse needs one): a version the model
      // writes nothing for fails.
      if (row.status === "queued" && now - row.requestedAt >= wait) {
        const made = modelSuggestions(name, versionLayout(version).types);
        row.updatedAt = row.requestedAt + wait;
        if (knobs.automationSuggestionsOutcome === "fail" || !made?.length)
          row.status = "failed";
        else {
          row.status = "ready";
          row.suggestions = made;
        }
      }
      return fill(
        row.status === "ready" ? row.suggestions : [],
        row.status === "queued",
      );
    },
  },
];
