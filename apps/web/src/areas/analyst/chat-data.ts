// Chat data for the Analyst: a chat is a question and its follow-ups, one
// assistant run per turn. The chat list lives in ./launcher (shared with the
// sidebar); this file holds one chat, its runs and how they read.
import type {
  AnswerCheck,
  AnswerStep,
  Resource,
} from "../../../../../packages/shared/src";
import { fieldName } from "../../ui";
import { formatNumber, plural } from "../../kit/util";
import { chatListKey } from "./launcher";

export type Chat = { id: string; title: string; runs: Resource[] };

// Under the list's key, so refreshing the list (or the live event stream,
// which refreshes everything under the workspace) refreshes open chats too.
export const chatKey = (workspaceId: string, chatId: string) => [
  ...chatListKey(workspaceId),
  chatId,
];

export const isWorking = (status?: string) =>
  status === "queued" || status === "running";
// The chat a run belongs to: its first run's id.
export const chatIdOf = (run: Pick<Resource, "id" | "data">) =>
  (run.data.threadId as string | undefined) || run.id;

// GET that resolves to null on 404, so "not found" can be told apart from a
// failed request.
async function getOrNull<T>(path: string): Promise<T | null> {
  const res = await fetch("/api/v1" + path, { credentials: "same-origin" });
  if (res.status === 404) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "Request failed");
  return data as T;
}
export const fetchChat = (workspaceId: string, chatId: string) =>
  getOrNull<Chat>(
    `/workspaces/${workspaceId}/threads/${encodeURIComponent(chatId)}`,
  );
// Sent without a JSON content type: the API rejects an empty JSON body.
export async function deleteChat(workspaceId: string, chatId: string) {
  const res = await fetch(
    `/api/v1/workspaces/${workspaceId}/threads/${encodeURIComponent(chatId)}`,
    { method: "DELETE", credentials: "same-origin" },
  );
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "Couldn’t delete this chat");
}
const fetchRun = (workspaceId: string, runId: string) =>
  getOrNull<Resource>(
    `/workspaces/${workspaceId}/resources/run/${encodeURIComponent(runId)}`,
  );

// For an id that is not a chat: the run it names, and the chat that run
// belongs to. Runs from before chats had ids follow the question they
// followed up on back to the first one.
export async function resolveRun(workspaceId: string, runId: string) {
  const run = await fetchRun(workspaceId, runId);
  if (!run) return null;
  let first = run;
  for (let hops = 0; hops < 50 && !first.data.threadId; hops++) {
    const previous = first.data.context?.resourceId;
    if (!previous || previous === first.id) break;
    const before = await fetchRun(workspaceId, previous).catch(() => null);
    if (!before || before.data.task) break;
    first = before;
  }
  return { run, chatId: chatIdOf(first) };
}

// The major steps of a run, as people see them while it works and after:
// the server's stages, in a few plain words. Prompts, retries and tool calls
// stay in the run's events.
export type StepId =
  | AnswerStep
  | "data"
  | "prepare"
  | "model"
  | "view"
  | "test"
  | "publish";
export type StepState =
  | "pending"
  | "running"
  | "done"
  | "skipped"
  | "failed"
  | "stopped";
export type Step = {
  id: StepId;
  state: StepState;
  startedAt?: number;
  endedAt?: number;
};
type Words = { label: string; pending: string; running: string; done: string };
export const STEPS: Record<StepId, Words> = {
  understand: {
    label: "Understand the question",
    pending: "Read the question and plan a query",
    running: "Reading the question and planning a query",
    done: "Planned the query",
  },
  analyze: {
    label: "Analyze the data",
    pending: "Run the query on your data snapshots",
    running: "Running a read-only query on your data snapshots",
    done: "Ran a read-only query on your data snapshots",
  },
  check: {
    label: "Check the results",
    pending: "Check the result’s shape and completeness",
    running: "Checking the result",
    done: "Checked the result",
  },
  write: {
    label: "Write the answer",
    pending: "Summarize the result in plain words",
    running: "Writing a short summary from the result",
    done: "Summarized the result in plain words",
  },
  data: {
    label: "Understand your data",
    pending: "Describe your datasets",
    running: "Describing your datasets",
    done: "Described your datasets",
  },
  prepare: {
    label: "Prepare the data",
    pending: "Write the pipelines",
    running: "Writing the pipelines",
    done: "Wrote the pipelines",
  },
  model: {
    label: "Connect business records",
    pending: "Map records and their relationships",
    running: "Mapping records and their relationships",
    done: "Mapped records and their relationships",
  },
  view: {
    label: "Create your view",
    pending: "Design the dashboard",
    running: "Designing the dashboard",
    done: "Designed the dashboard",
  },
  test: {
    label: "Test the result",
    pending: "Run every query and check the records",
    running: "Running every query and checking the records",
    done: "Ran every query and checked the records",
  },
  publish: {
    label: "Publish your changes",
    pending: "Publish to your workspace",
    running: "Publishing to your workspace",
    done: "Published to your workspace",
  },
};
// Steps in the order they happen; an answer's and a build's never mix.
const ORDER = Object.keys(STEPS) as StepId[];
const stepOf = (stage: string): StepId | undefined =>
  stage === "routing" || stage === "analysis"
    ? "understand"
    : stage === "query" || stage === "analysis-repair"
      ? "analyze"
      : stage === "check"
        ? "check"
        : stage === "summary"
          ? "write"
          : stage === "catalog"
            ? "data"
            : stage === "pipelines"
              ? "prepare"
              : stage === "ontology"
                ? "model"
                : stage === "dashboards"
                  ? "view"
                  : stage === "validation" || stage.includes("repair")
                    ? "test"
                    : stage === "publishing"
                      ? "publish"
                      : undefined;
const ENDS = ["complete", "failed", "review", "published"];
type RunEvent = { at: string; stage: string; message?: string };
// This attempt's events: a retry starts again after its "queued" event.
const attemptEvents = (run: Resource): RunEvent[] => {
  const events: RunEvent[] = run.data.events || [];
  return events.slice(events.findLastIndex((e) => e.stage === "queued") + 1);
};
export const isAnswering = (data: Record<string, any>) =>
  data.intent === "answer" || data.resolvedIntent === "answer";

// The current attempt's steps, in order, with when each started and ended.
// An answer's planned steps that haven't started follow as pending.
export function runSteps(run: Resource): Step[] {
  const d = run.data,
    steps: Step[] = [],
    building = d.intent === "build" || d.resolvedIntent === "build";
  let ended: number | undefined;
  for (const e of attemptEvents(run)) {
    const at = Date.parse(e.at);
    if (ENDS.includes(e.stage)) {
      ended ??= at;
      continue;
    }
    // Choosing to build is not one of a build's steps.
    const id = e.stage === "routing" && building ? undefined : stepOf(e.stage),
      last = steps.at(-1);
    // Work inside the current step, or going back to an earlier one (a
    // query corrected after it failed), continues the current step.
    if (!id || (last && ORDER.indexOf(id) <= ORDER.indexOf(last.id))) continue;
    if (last) {
      last.state = "done";
      last.endedAt = at;
    }
    steps.push({ id, state: "running", startedAt: at });
  }
  const last = steps.at(-1),
    finished = ended ?? Date.parse(run.updatedAt);
  if (last && !isWorking(d.status)) {
    last.endedAt = finished;
    last.state =
      d.status === "failed"
        ? "failed"
        : d.status === "canceled"
          ? "stopped"
          : "done";
  }
  if (d.answer && !d.answer.summary)
    for (const s of steps) if (s.id === "write") s.state = "skipped";
  const reached = last ? ORDER.indexOf(last.id) : -1;
  for (const id of (isAnswering(d) && (d.outline as StepId[])) || [])
    if (ORDER.indexOf(id) > reached)
      steps.push({
        id,
        // A finished answer that never got to a step skipped it.
        state: d.status === "succeeded" ? "skipped" : "pending",
      });
  if (!steps.length && isWorking(d.status))
    steps.push({
      id: building ? "data" : "understand",
      state: "pending",
    });
  return steps;
}
// The step being worked on, or where the run stopped.
const isCurrent = (s: Step) =>
  s.state === "running" || s.state === "failed" || s.state === "stopped";
export const currentStep = (steps: Step[]) => steps.find(isCurrent);
// At most three steps for the compact trace: the one in progress (or where the
// run stopped) with its neighbours, or the last two that ran.
export function compactSteps(steps: Step[]) {
  const at = steps.findIndex(isCurrent);
  if (at >= 0) {
    const from = Math.max(0, Math.min(at - 1, steps.length - 3));
    return steps.slice(from, from + 3);
  }
  const ran = steps.filter((s) => s.state !== "pending");
  return ran.length ? ran.slice(-2) : steps.slice(0, 3);
}

// What a step did, or is doing, from what the run reports, as a sentence.
export const stepText = (step: Step, run: Resource) => {
  const text = stepWords(step, run);
  return /[.!?”]$/.test(text) ? text : text + ".";
};
function stepWords(step: Step, run: Resource): string {
  const d = run.data,
    words = STEPS[step.id];
  if (step.state === "pending") return words.pending;
  if (step.state === "failed") return "Stopped with an error";
  if (step.state === "stopped") return "Stopped before it finished";
  const repaired = attemptEvents(run).some(
    (e) => e.stage === "analysis-repair",
  );
  if (step.state === "running")
    return step.id === "analyze" && repaired
      ? "The first query didn’t run, so it is being corrected"
      : words.running;
  if (step.state === "skipped")
    return step.id === "write"
      ? "No summary this time; the result is below"
      : "Skipped";
  if (step.id === "understand") {
    const title =
      d.answer?.title ||
      d.checkpoints?.["analysis-repair"]?.title ||
      d.checkpoints?.analysis?.title;
    return title ? `Planned “${title}”` : words.done;
  }
  if (step.id === "analyze") {
    const names: string[] = [
      ...new Set<string>(
        (d.answer?.citations || []).map((c: { name: string }) => c.name),
      ),
    ];
    const read = names.length
      ? `a read-only query on ${listText(names)}`
      : "a read-only query on your data snapshots";
    return repaired
      ? `Corrected the query once, then ran ${read}`
      : `Ran ${read}`;
  }
  if (step.id === "check" && d.answer?.check)
    return resultText(
      d.answer.check,
      d.answer.rows?.length || 0,
      typeof d.answer.total === "number"
        ? d.answer.total
        : d.answer.rows?.length || 0,
    );
  return words.done;
}

const acronyms = new Set(
  "id url sku mrr arr aov ltv cac arpu roi nps gmv usd eur gbp kpi utm".split(
    " ",
  ),
);
// Column names read as labels: "total_mrr" -> "Total MRR".
export const columnLabel = (key: string) =>
  fieldName(key)
    .split(" ")
    .map((w) => (acronyms.has(w.toLowerCase()) ? w.toUpperCase() : w))
    .join(" ");
// The same name inside a sentence: "order count", "total MRR".
export const columnWords = (key: string) =>
  columnLabel(key)
    .split(" ")
    .map((w) => (acronyms.has(w.toLowerCase()) ? w : w.toLowerCase()))
    .join(" ");
// What the server's check found, in one line: "10 rows, ranked by order
// count, highest first. 2 share first place."
export function resultText(check: AnswerCheck, rows: number, total: number) {
  const count =
    total > rows
      ? `First ${formatNumber(rows)} of ${plural(total, "row")}`
      : plural(total, "row");
  const measure = check.measure ? columnWords(check.measure) : "";
  const shape =
    check.kind === "empty"
      ? "No rows matched"
      : check.kind === "value"
        ? "A single result"
        : check.kind === "ranking"
          ? `${count}, ranked by ${measure}, ${check.order === "asc" ? "lowest" : "highest"} first`
          : check.kind === "trend"
            ? `${count}: ${measure} over time`
            : check.kind === "breakdown"
              ? `${count}: ${measure} by ${columnWords(check.label!)}`
              : count;
  const notes = [
    check.kind === "ranking" && (check.leaders || 0) > 1
      ? `${check.leaders} share first place`
      : "",
    check.duplicates
      ? `${plural(check.duplicates, "row")} repeated exactly`
      : "",
    check.missing && check.measure
      ? `${plural(check.missing, "row")} without ${measure}`
      : "",
  ].filter(Boolean);
  return [shape, ...notes].join(". ") + ".";
}

// When the current attempt started: a retry restarts the clock.
export function startedAt(run: Resource) {
  const retry = [...(run.data.events || [])]
    .reverse()
    .find((e: any) => e.stage === "queued");
  return Date.parse(retry?.at || run.createdAt);
}
// When the current attempt ended: its last event, or the run's last change
// (a stop records no event).
export function endedAt(run: Resource) {
  const end = attemptEvents(run).find((e) => ENDS.includes(e.stage));
  return end ? Date.parse(end.at) : Date.parse(run.updatedAt);
}
// The server's estimate for an answer, rounded outwards to whole seconds,
// fives, quarter minutes or minutes, so it reads as the range it is.
export function estimateRange(estimate?: { low: number; high: number } | null) {
  if (!estimate) return null;
  const hi = estimate.high / 1000,
    step = hi <= 20 ? 1 : hi <= 60 ? 5 : hi <= 180 ? 15 : 60,
    low = Math.max(step, Math.floor(estimate.low / 1000 / step) * step),
    high = Math.max(low + step, Math.ceil(hi / step) * step);
  const text =
    high < 60
      ? `${low}–${high}s`
      : `${duration(low * 1000)}–${duration(high * 1000)}`;
  return { low: low * 1000, high: high * 1000, text };
}
// "14s", "2m 05s".
export const duration = (ms: number) => {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60
    ? `${s}s`
    : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
};

// Viewers may stop or retry their own questions, but not changes.
export function canControl(
  run: Resource,
  role: string,
  userId: string,
): boolean {
  const d = run.data;
  return (
    role !== "viewer" ||
    (d.authority?.userId === userId &&
      (d.resolvedIntent || d.intent) !== "build")
  );
}

// A failure in plain words; the raw message stays available as detail.
export function plainError(error?: string | null): {
  text: string;
  detail?: string;
} {
  const raw = (error || "").trim();
  if (!raw) return { text: "Something went wrong before an answer was ready." };
  if (
    /(Binder|Parser|Catalog|Conversion|Invalid Input|Constraint) Error|syntax error|SQL/i.test(
      raw,
    )
  )
    return {
      text: "The query written for this question didn’t run against your data. Trying again, or rephrasing the question, usually helps.",
      detail: raw,
    };
  if (/timed? ?out|timeout|deadline/i.test(raw))
    return { text: "This took too long to answer.", detail: raw };
  if (/api key|provider|model|rate limit|quota|401|429/i.test(raw))
    return {
      text: "The AI model couldn’t be reached. Check the model settings, then try again.",
      detail: raw,
    };
  // Short messages are already written for people.
  return raw.length <= 160 && !/\n|at \w+ \(/.test(raw)
    ? { text: raw }
    : { text: "Something went wrong before an answer was ready.", detail: raw };
}

// "A", "A and B", "A, B and C".
export const listText = (names: string[]) =>
  names.length <= 1
    ? names[0] || ""
    : names.slice(0, -1).join(", ") + " and " + names.at(-1);

// Drafts are kept per chat for the session. Storage may be unavailable.
export function readDraft(key: string) {
  try {
    return sessionStorage.getItem(key) || "";
  } catch {
    return "";
  }
}
export function writeDraft(key: string, value: string) {
  try {
    if (value) sessionStorage.setItem(key, value);
    else sessionStorage.removeItem(key);
  } catch {
    /* the draft is a convenience */
  }
}
