import { isRecord } from "../../../shared/json";
import type {
  FoundryAgentThread,
  FoundryAutomationSuggestionKind,
  FoundryAutomationSuggestionSource,
  ListAutomationSuggestionsResponse,
} from "../../../shared/wfe";

// Founder's page (/ask/:ontologyId, founder-page.tsx) and its threads in the
// sidebar (founder-threads.tsx), as pure functions: what the page shows from
// its URL, Founder's threads as the sidebar lists them, and the new chat's
// suggestions (founder-suggestions.tsx): which kinds it offers, the cards
// of each, and the fixed examples where the WFE gives none.

/** What Founder's page shows. */
export type FounderView =
  // One of Founder's threads.
  | { kind: "chat"; id: string }
  // A new chat: the composer, centred.
  | { kind: "new" }
  // An Analyst thread from before Founder, read only.
  | { kind: "analyst"; id: string }
  // A narrow screen's list of threads, where a wide one has the sidebar.
  | { kind: "threads" };

/**
 * What Founder's page shows, from its URL, the screen, Founder's threads
 * (undefined while they're read or when they couldn't be) and the thread the
 * dock last held (`stored`, "" for none): `?thread=` is an Analyst thread,
 * `?chat=` one of Founder's; `?new`, or no threads at all, a new chat; else,
 * on a narrow screen, the threads; else the dock's thread, while the list
 * names it (or isn't read yet), so the page and the dock hold one
 * conversation; else a new chat.
 *
 * @example
 * const p = (q: string) => new URLSearchParams(q);
 * founderView(p("thread=q1"), false, [], ""); // { kind: "analyst", id: "q1" }
 * founderView(p("chat=t1"), true, undefined, ""); // { kind: "chat", id: "t1" }
 * founderView(p("new=1"), true, [{ id: "t1" }], "t1"); // { kind: "new" }
 * founderView(p(""), true, [{ id: "t1" }], "t1"); // { kind: "threads" }
 * founderView(p(""), false, [{ id: "t1" }], "t1"); // { kind: "chat", id: "t1" }
 * founderView(p(""), false, [{ id: "t1" }], "t9"); // { kind: "new" }
 * founderView(p(""), false, [], "t1"); // { kind: "new" }
 */
export function founderView(
  params: URLSearchParams,
  narrow: boolean,
  threads: readonly { id: string }[] | undefined,
  stored: string,
): FounderView {
  const analyst = params.get("thread");
  if (analyst) return { kind: "analyst", id: analyst };
  const chat = params.get("chat");
  if (chat) return { kind: "chat", id: chat };
  if (params.has("new") || threads?.length === 0) return { kind: "new" };
  if (narrow) return { kind: "threads" };
  return stored && (!threads || threads.some((t) => t.id === stored))
    ? { kind: "chat", id: stored }
    : { kind: "new" };
}

/** One of Founder's threads as the sidebar lists it. */
export type FounderThread = {
  id: string;
  title: string;
  // When it last changed (threadGroups groups by it).
  last: string;
  // Started in Slack (its first turn's surface), through Helix Foundry's
  // Slack app.
  slack: boolean;
};

/**
 * Founder's threads as the sidebar lists them, in the list's order (newest
 * first): untitled ones named so, dated by their last change, else by
 * their start, and those started in Slack said so.
 *
 * @example
 * founderThreads([
 *   { id: "t1", title: "Unpaid invoices", updatedAt: "2026-11-16T09:00:00Z" },
 *   { id: "t2", createdAt: "2026-11-15T09:00:00Z", surface: "FOUNDRY_AGENT_SURFACE_SLACK" },
 * ]);
 * // [{ id: "t1", title: "Unpaid invoices", last: "2026-11-16T09:00:00Z", slack: false },
 * //  { id: "t2", title: "Untitled", last: "2026-11-15T09:00:00Z", slack: true }]
 * founderThreads(undefined); // []
 */
export function founderThreads(
  threads: readonly FoundryAgentThread[] | undefined,
): FounderThread[] {
  return (threads || []).map((t) => ({
    id: t.id,
    title: t.title || "Untitled",
    last: t.updatedAt || t.createdAt || "",
    slack: t.surface === "FOUNDRY_AGENT_SURFACE_SLACK",
  }));
}

// -------------------------------------------------------------- suggestions

/** What a suggestion asks Founder for (FoundryAutomationSuggestionKind). */
export type SuggestionKind = "question" | "dashboard" | "ontology" | "workflow";
/** The kinds in the order the new chat's tabs show them (the WFE's). */
export const SUGGESTION_KINDS: readonly SuggestionKind[] = [
  "question",
  "dashboard",
  "ontology",
  "workflow",
];
/** Where a suggestion came from (FoundryAutomationSuggestionSource). */
export type SuggestionSource = "model" | "questions" | "setup" | "fixed";
/** A suggestion as a card shows it: its prompt, topic and kind. */
export type FounderSuggestion = {
  kind: SuggestionKind;
  title: string;
  prompt: string;
  topic: string;
  source: SuggestionSource;
};
/** How many cards a kind shows. */
export const SUGGESTION_CARDS = 2;
/** A suggestion's limits, in characters, as the WFE writes them. */
export const SUGGESTION_LIMITS = { title: 32, prompt: 400, topic: 24 } as const;
/** A kind's own topic, for a suggestion without one of its own. */
export const KIND_TOPIC: Record<SuggestionKind, string> = {
  question: "Question",
  dashboard: "Dashboard",
  ontology: "Ontology",
  workflow: "Workflow",
};
/** Where the kind last chosen is kept, per browser. */
export const SUGGESTION_KIND_KEY = "foundry.agent.suggestionKind";

const KINDS: Partial<Record<FoundryAutomationSuggestionKind, SuggestionKind>> =
  {
    FOUNDRY_AUTOMATION_SUGGESTION_KIND_UNSPECIFIED: "workflow",
    FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION: "question",
    FOUNDRY_AUTOMATION_SUGGESTION_KIND_DASHBOARD: "dashboard",
    FOUNDRY_AUTOMATION_SUGGESTION_KIND_ONTOLOGY: "ontology",
    FOUNDRY_AUTOMATION_SUGGESTION_KIND_WORKFLOW: "workflow",
  };
const SOURCES: Partial<
  Record<FoundryAutomationSuggestionSource, SuggestionSource>
> = {
  FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL: "model",
  FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS: "questions",
  FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_SETUP: "setup",
  FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_FIXED: "fixed",
};
const length = (s: string) => [...s].length;
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;
/** A field trimmed, if it is text of 1 to `max` characters with no control character. */
const field = (v: unknown, max: number) => {
  const t = typeof v === "string" ? v.trim() : "";
  return t && length(t) <= max && !CONTROL.test(t) ? t : undefined;
};

/**
 * A response's suggestions for Founder's new chat, read without trust (a
 * model may have written them), in order: each with a title and prompt
 * within the WFE's limits once trimmed, of a known kind (none is a WFE
 * before kinds: a workflow), its topic the kind's own when it has none
 * that fits, and a source it doesn't name read as fixed.
 *
 * @example
 * readFounderSuggestions({ suggestions: [
 *   { title: "Unpaid", prompt: " Which invoices are unpaid? ", kind: "FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION", topic: "Revenue", source: "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL" },
 *   { title: "Brief", prompt: "Every Monday, email me a brief." },
 *   { title: "", prompt: "x", kind: "FOUNDRY_AUTOMATION_SUGGESTION_KIND_DASHBOARD" },
 * ] });
 * // [{ kind: "question", title: "Unpaid", prompt: "Which invoices are unpaid?", topic: "Revenue", source: "model" },
 * //  { kind: "workflow", title: "Brief", prompt: "Every Monday, email me a brief.", topic: "Workflow", source: "fixed" }]
 */
export function readFounderSuggestions(
  r?: ListAutomationSuggestionsResponse,
): FounderSuggestion[] {
  const given: unknown = r?.suggestions;
  return (Array.isArray(given) ? given : []).flatMap((x: unknown) => {
    if (!isRecord(x)) return [];
    const kind =
      x.kind === undefined
        ? "workflow"
        : typeof x.kind === "string" && Object.hasOwn(KINDS, x.kind)
          ? KINDS[x.kind as FoundryAutomationSuggestionKind]
          : undefined;
    const title = field(x.title, SUGGESTION_LIMITS.title),
      prompt = field(x.prompt, SUGGESTION_LIMITS.prompt);
    if (!kind || !title || !prompt) return [];
    const source =
      (typeof x.source === "string" && Object.hasOwn(SOURCES, x.source)
        ? SOURCES[x.source as FoundryAutomationSuggestionSource]
        : undefined) ?? "fixed";
    return [
      {
        kind,
        title,
        prompt,
        topic: field(x.topic, SUGGESTION_LIMITS.topic) ?? KIND_TOPIC[kind],
        source,
      },
    ];
  });
}

/**
 * An ontology's name as the fixed examples write it, as the backend's
 * suggest package does: whitespace collapsed, cut with "…" at 100
 * characters, "this ontology" for none.
 */
const named = (name: string) => {
  const n = name.split(/\s+/).filter(Boolean).join(" ");
  if (!n || CONTROL.test(n)) return "this ontology";
  return length(n) <= 100 ? n : [...n].slice(0, 99).join("").trimEnd() + "…";
};

/**
 * The fixed examples, named for the ontology, of each kind in order (more
 * than a kind's cards, so a repeated prompt still leaves enough), as the
 * backend's suggest package writes them (fill.go `fixed`, `Starter`) where
 * nothing else fills a kind; the new chat shows them where the WFE gives
 * none. Without a current version (`versioned` false), the ontology's
 * examples connect data or upload documents and draft its first version.
 * A dashboard's prompt says "dashboard", a change's "draft" (so Founder
 * prepares it), and a workflow's when it starts and what it does.
 *
 * @example
 * founderSuggestions("Renewals", true).find((x) => x.kind === "dashboard")?.prompt;
 * // "Build me a dashboard of Renewals at a glance: how many of each type it holds and what was added most recently."
 * founderSuggestions("Renewals", false).find((x) => x.kind === "ontology")?.prompt;
 * // "Help me connect my data and draft the first version of Renewals."
 */
export function founderSuggestions(
  name: string,
  versioned: boolean,
): FounderSuggestion[] {
  const n = named(name);
  const of = (
    kind: SuggestionKind,
    rows: readonly (readonly [title: string, prompt: string])[],
  ) =>
    rows.map(([title, prompt]) => ({
      kind,
      title,
      prompt,
      topic: KIND_TOPIC[kind],
      source: "fixed" as const,
    }));
  return [
    ...of("question", [
      [
        "What it holds",
        `What does ${n} hold? List its types and how many of each it has.`,
      ],
      [
        "What changed recently",
        `What was added to ${n} most recently, and where did it come from?`,
      ],
      [
        "Missing details",
        `Which records in ${n} are missing important details?`,
      ],
    ]),
    ...of("dashboard", [
      [
        "Overview",
        `Build me a dashboard of ${n} at a glance: how many of each type it holds and what was added most recently.`,
      ],
      [
        "Data quality",
        `Build me a dashboard of the data quality of ${n}: the records missing key details and the open reviews.`,
      ],
      [
        "Activity by month",
        `Build me a dashboard of the activity in ${n} by month.`,
      ],
    ]),
    ...of(
      "ontology",
      versioned
        ? [
            [
              "Add missing details",
              `Draft a new version of ${n} that adds the properties people ask about most but it doesn't hold yet.`,
            ],
            [
              "Link related records",
              `Draft a new version of ${n} with links between the types that are related but not linked yet.`,
            ],
            [
              "Flag what breaks a standard",
              `Draft a new version of ${n} with rules that flag records breaking the business's standards.`,
            ],
          ]
        : [
            [
              "Connect your data",
              `Help me connect my data and draft the first version of ${n}.`,
            ],
            [
              "Start from documents",
              `Help me upload documents and draft the first version of ${n} from them.`,
            ],
            [
              "Draft from connected data",
              `Draft the first version of ${n} from the data already connected.`,
            ],
          ],
    ),
    ...of("workflow", [
      [
        "Monday brief",
        `Every Monday at 9am, summarise what changed in ${n} last week and email me.`,
      ],
      [
        "Fresh data daily",
        `Every day at 6am, sync my connections, then load ${n}.`,
      ],
      [
        "Load check",
        `When a load of ${n} is ready, check for records missing key details and email me any it finds.`,
      ],
      [
        "New documents digest",
        `When documents arrive in a document set ${n} reads, summarise what they add and email me.`,
      ],
      [
        "Rule breaks alert",
        `When a load of ${n} is ready, email me if it holds more open rule breaks than the last one.`,
      ],
    ]),
  ];
}

/** One kind's tab: its cards, or `undefined` while they are still coming. */
export type SuggestionTab = {
  kind: SuggestionKind;
  cards: readonly FounderSuggestion[] | undefined;
};
/** What the new chat shows under its composer. */
export type FounderSuggestionsView =
  // A tab per kind.
  | { kind: "tabs"; tabs: readonly SuggestionTab[] }
  // A grantee: the load's competency questions, no tabs.
  | { kind: "questions"; cards: readonly FounderSuggestion[] }
  | { kind: "none" };

/**
 * Whether a kind's cards are final while Founder writes the rest: a
 * question from the competency questions and a change from the ontology's
 * setup come first, so the model's won't replace them; the model's own are.
 */
const final = (x: FounderSuggestion) =>
  x.source === "model" ||
  (x.kind === "question" && x.source === "questions") ||
  (x.kind === "ontology" && x.source === "setup");

/**
 * What the new chat shows from its read of the suggestions. A grantee
 * never reads them: the load's competency questions (`questions`), else
 * nothing. A refusal (`refused`), or a failed read with nothing read before,
 * is the fixed examples (no workflows when refused: the WFE has none);
 * while the first read is pending, every kind is placeholders. Else a tab
 * per kind the response holds, a question, dashboard or change it lacks
 * filled from the fixed examples, each its first 2; while Founder writes
 * them (`generating`), a kind whose cards aren't all final is placeholders.
 *
 * @example
 * founderSuggestionsView({ grantee: true, questions: [], name: "R", versioned: true, pending: false, failed: false }).kind; // "none"
 * founderSuggestionsView({ grantee: false, questions: [], name: "R", versioned: true, pending: true, failed: false });
 * // { kind: "tabs", tabs: [{ kind: "question", cards: undefined }, …4] }
 */
export function founderSuggestionsView(q: {
  grantee: boolean;
  questions: readonly string[];
  name: string;
  versioned: boolean;
  pending: boolean;
  failed: boolean;
  refused?: boolean;
  data?: ListAutomationSuggestionsResponse;
}): FounderSuggestionsView {
  if (q.grantee)
    return q.questions.length
      ? {
          kind: "questions",
          cards: q.questions.map((question) => ({
            kind: "question",
            title: question,
            prompt: question,
            topic: KIND_TOPIC.question,
            source: "questions",
          })),
        }
      : { kind: "none" };
  const fixed = founderSuggestions(q.name, q.versioned);
  const tab = (
    kind: SuggestionKind,
    from: readonly FounderSuggestion[],
  ): SuggestionTab => ({
    kind,
    cards: from.filter((x) => x.kind === kind).slice(0, SUGGESTION_CARDS),
  });
  if (q.refused || (q.failed && !q.data))
    return {
      kind: "tabs",
      tabs: SUGGESTION_KINDS.filter((k) => !q.refused || k !== "workflow").map(
        (k) => tab(k, fixed),
      ),
    };
  if (!q.data)
    return {
      kind: "tabs",
      tabs: SUGGESTION_KINDS.map((kind) => ({ kind, cards: undefined })),
    };
  const read = readFounderSuggestions(q.data),
    generating = q.data.generating === true;
  return {
    kind: "tabs",
    tabs: SUGGESTION_KINDS.flatMap((kind): SuggestionTab[] => {
      const own = tab(kind, read).cards!;
      if (kind === "workflow" && !own.length) return [];
      if (generating && !(own.length && own.every(final)))
        return [{ kind, cards: undefined }];
      return [own.length ? { kind, cards: own } : tab(kind, fixed)];
    }),
  };
}

/**
 * The kind the new chat opens on: the one chosen last (kept per browser)
 * while a tab shows it, else Questions, else the first.
 *
 * @example
 * chosenKind("dashboard", ["question", "dashboard"]); // "dashboard"
 * chosenKind("workflow", ["question", "dashboard"]); // "question"
 * chosenKind("toString", ["question"]); // "question"
 * chosenKind("", ["dashboard"]); // "dashboard"
 */
export function chosenKind(
  stored: string,
  kinds: readonly SuggestionKind[],
): SuggestionKind | undefined {
  return (
    kinds.find((k) => k === stored) ??
    kinds.find((k) => k === "question") ??
    kinds[0]
  );
}
