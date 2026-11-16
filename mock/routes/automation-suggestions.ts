import type {
  FoundryAutomationSuggestion,
  FoundryAutomationSuggestionIcon,
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
  readAgentBundle,
  versionLayout,
} from "../../src/web/areas/ontology/model";
import { fail, ok, type MockRoute } from "../http";
import { config, onReset } from "../state";
import { aiSource } from "./ai";
import { caller, isCaller } from "./automations";
import { ontologyById } from "./ontologies";
import { currentVersionOf, ontologyVersion } from "./ontology";

// The new automation page's suggestions, as the backend serves them
// (api/automation_suggestions.go, the suggest package, Founder's
// worker): for an ontology's current version, written once by the
// Assistant, and until then, or where it can't write them, made from the
// version's competency questions (`fallbackSuggestions`, the backend's
// rule). The first read of a version queues the writing (the backend's 20 a
// workspace an hour aren't counted) and answers the fallback with
// `generating`; it is
// written `automationSuggestionsMs` (1500) after, which 0 makes the first
// read's. The mock's "model" decides by the version's types: contracts or
// invoices get contract suggestions, people theirs, any other its
// competency questions, and one with none fails. Knobs, through POST
// /__mock/config:
// `automationSuggestionsOutcome: "fail"` fails the writing (the fallback,
// not generating); `automationSuggestionsAi: false` is a workspace without
// an AI provider (as `aiSource` none, the local mock's `aiDefault: false`)
// and Founder off (`agentEnabled: false`) never writes them.

type Knobs = {
  automationSuggestionsMs?: number;
  automationSuggestionsOutcome?: "fail";
  automationSuggestionsAi?: boolean;
  agentEnabled?: boolean;
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
 * unless an auxiliary follows or nothing does; cut after the last whole
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
      next = rest.split(" ")[0]!.toLowerCase();
    if (rest && !AUXILIARIES.has(next)) q = rest;
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

/** What the mock's Assistant writes for a version: by its types, for the ontology named. */
function written(name: string, types: readonly string[]) {
  const has = (t: string) => types.includes(t);
  if (has("Contract") || has("Invoice"))
    return [
      [
        "Renewal radar",
        "watch",
        `Every morning at 8am, find contracts in ${name} that renew in the next 30 days and email me each one with its customer and what they are billed.`,
      ],
      [
        "Contract check",
        "check",
        `When new documents arrive in a set ${name} reads, check each new contract's term and signing date against the customer it names, and email me any that don't match.`,
      ],
      [
        "Invoice watch",
        "alert",
        `Every day, look in ${name} for invoices that are past due or failed and, if there are any, email me which customers they belong to.`,
      ],
      [
        "Monday contracts brief",
        "briefing",
        `Every Monday at 9am, summarise what changed in ${name} last week: new contracts, renewals coming up and invoices raised, and email it to me.`,
      ],
    ];
  if (has("Person"))
    return [
      [
        "New contacts brief",
        "briefing",
        `Every Monday at 9am, list the people added to ${name} last week with the company each works at, and email it to me.`,
      ],
      [
        "Job change alert",
        "alert",
        `Every day, look in ${name} for people whose title or company changed and, if there are any, email me who and what changed.`,
      ],
      [
        "Account map refresh",
        "refresh",
        `Every night at 2am, sync the connections ${name} reads, load it again once they finish, and email me if either fails.`,
      ],
      [
        "Weekly people report",
        "report",
        `Every Friday at 4pm, answer "How many people work at each company?" from ${name}, and email me the answer.`,
      ],
    ];
  return undefined;
}

export const automationSuggestionRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automation-suggestions",
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!,
        ontologyId = req.query.get("ontologyId") || "";
      if (!ontologyId) return fail("INVALID_ARGUMENT", "ontology_id required");
      const o = ontologyById(ontologyId);
      if (o?.workspaceId !== ws) return fail("NOT_FOUND", "ontology not found");
      const versionId = currentVersionOf(o.id) || "",
        v = versionId ? ontologyVersion(o.id, versionId) : undefined;
      const answer = (
        suggestions: FoundryAutomationSuggestion[],
        source: "MODEL" | "QUESTIONS" | "UNSPECIFIED",
        generating = false,
      ) =>
        ok({
          suggestions,
          source: `FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_${source}`,
          generating,
          versionId,
        } satisfies ListAutomationSuggestionsResponse);
      if (!v) return answer([], "UNSPECIFIED");
      const version = {
        id: v.id,
        format: v.format,
        definition: v.definition,
      } as OntologyVersion;
      const fallback = fallbackSuggestions(o.name, versionQuestions(version)),
        questions = (generating = false) =>
          answer(
            fallback,
            fallback.length ? "QUESTIONS" : "UNSPECIFIED",
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
          return questions();
        row = {
          status: "queued",
          requestedAt: now,
          updatedAt: now,
          suggestions: [],
        };
        rows.set(key, row);
      }
      // The worker writes it once its time came, read or not. Ready
      // always holds some (the backend's Parse needs one): a version with
      // nothing to write from fails.
      if (row.status === "queued" && now - row.requestedAt >= wait) {
        const made = written(o.name, versionLayout(version).types)?.map(
          ([title, kind, prompt]) => ({
            title: title!,
            prompt: prompt!,
            icon: icon(kind!),
          }),
        );
        row.updatedAt = row.requestedAt + wait;
        const suggestions = made ?? fallback;
        if (
          knobs.automationSuggestionsOutcome === "fail" ||
          !suggestions.length
        )
          row.status = "failed";
        else {
          row.status = "ready";
          row.suggestions = suggestions;
        }
      }
      if (row.status === "ready") return answer(row.suggestions, "MODEL");
      return questions(row.status === "queued");
    },
  },
];
