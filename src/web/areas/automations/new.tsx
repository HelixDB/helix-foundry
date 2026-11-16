import { useEffect, useId, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CalendarClock,
  FileSearch,
  FileText,
  type LucideIcon,
  Newspaper,
  Plus,
  RefreshCw,
  Sparkles,
  TrendingDown,
} from "lucide-react";
import { ApiError } from "../../api";
import { ButtonLink, Section, Select, Skeleton } from "../../kit";
import { readStorage, writeStorage } from "../../kit/util";
import { useWorkspace } from "../../ui";
import {
  Composer,
  RuntimeLine,
  Transcript,
  useThreadRuntime,
} from "../agent/dock";
import { agentRefusal, resultLink } from "../agent/model";
import { agentQueries } from "../agent/queries";
import { ontologyQueries } from "../ontology/queries";
import { lastOntology, ontologyName } from "../ontology/scope";
import { readAccess } from "../workspace/access-model";
import { Crumbs, DraftPage, Frame } from "./editor";
import {
  shownSuggestions,
  SUGGESTION_TONE,
  suggestionOntology,
  type SuggestionIcon,
} from "./model";
import { Templates } from "./parts";
import { automationPaths as paths } from "./paths";
import { automationQueries, useNames } from "./queries";

// /automations/new: a new automation starts as a conversation with the
// Founder (the general one, the dock's: its thread, turns and cards), its
// composer centred over the canvas's dots, a suggestion's prompt a chip away
// (hovered or focused, it is the composer's placeholder; clicked, its text),
// or from scratch on the canvas (?template=blank, as every template's
// draft). Sending opens a new thread, which the page then holds whole
// (?thread=), and which the dock lists too; the page context says the
// person is making an automation. Nothing runs until they click a card:
// once their click on Founder's create_automation card made it, the
// page opens it on its canvas. The suggestions are written for one of the
// workspace's ontologies with a current version (model.ts
// suggestionOntology), which a picker under them changes when there are
// several; the fixed ones where none gives any.

const ICONS: Record<SuggestionIcon, LucideIcon> = {
  briefing: Newspaper,
  watch: CalendarClock,
  check: FileSearch,
  alert: TrendingDown,
  refresh: RefreshCw,
  report: FileText,
};
// The ontology chosen here, per browser: a choice of suggestions only, so
// it never moves the ontology last opened.
const chosenKey = (ws: string) =>
  "foundry.automations.suggestions.ontology.v1:" + ws;

export function NewAutomationPage() {
  const [params] = useSearchParams();
  const template = params.get("template");
  return template ? <DraftPage templateId={template} /> : <NewChat />;
}

function NewChat() {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    lists = useNames(ws);
  const [params, setParams] = useSearchParams();
  const threadId = params.get("thread") || "";
  const threads = useQuery(agentQueries.threads(ws));
  const thread = useQuery({
    ...agentQueries.thread(ws, threadId),
    enabled: !!threadId,
    retry: false,
  });
  const where = useThreadRuntime(threadId, thread.data);
  const disabled = agentRefusal(threads.error) === "FOUNDRY_AGENT_DISABLED";
  // A thread deleted elsewhere starts afresh.
  const gone = thread.error instanceof ApiError && thread.error.status === 404;
  useEffect(() => {
    if (gone) setParams({}, { replace: true });
  }, [gone]);
  // The automation its card made, once the person's click made it: each
  // create_automation action's status as this page first saw it, so one
  // made before the page opened isn't followed.
  const seen = useRef(new Map<string, string>());
  useEffect(() => {
    for (const x of thread.data?.actions || []) {
      if (x.operation !== "create_automation") continue;
      const before = seen.current.get(x.id);
      seen.current.set(x.id, x.status || "");
      const to = resultLink(x)?.to;
      if (
        before !== undefined &&
        before !== "succeeded" &&
        x.status === "succeeded" &&
        to
      ) {
        navigate(to);
        return;
      }
    }
  }, [thread.data]);
  const [seed, setSeed] = useState({ text: "", n: 0 }),
    [preview, setPreview] = useState("");
  const sent = (id: string) => {
    void qc.invalidateQueries({ queryKey: [ws, "agent", "threads"] });
    void qc.invalidateQueries({ queryKey: [ws, "agent", "thread", id] });
    if (id !== threadId) setParams({ thread: id });
  };
  const scratch = (
    <ButtonLink to={paths.newAutomation("blank")} icon={Plus} size="sm">
      Start from scratch
    </ButtonLink>
  );
  const runtime = where && where.runtime !== "api" && (
    <RuntimeLine
      runtime={where.runtime}
      state={where.state}
      thread={!!threadId}
    />
  );

  if (disabled)
    return (
      <Frame bar={<Crumbs name="New automation" />}>
        <div className="au-frame">
          <Section
            title="Start from a template"
            description="Founder isn’t available here, so start from a template or from scratch: you can change every step before it runs."
            className="au-templates-section"
          >
            <Templates lists={lists} />
          </Section>
        </div>
      </Frame>
    );

  if (!threadId)
    return (
      <Frame bar={<Crumbs name="New automation" />}>
        <div className="au-new-page" data-replay-block>
          <div className="au-new-inner">
            <span className="au-new-icon" aria-hidden>
              <Sparkles size={18} />
            </span>
            <h2 className="au-new-title">What should happen?</h2>
            <p className="au-new-text">
              Say when it starts and what it does, in your own words. Founder
              sets it up; nothing runs until you approve it.
            </p>
            {runtime}
            <div className="au-new-composer">
              <Composer
                key={seed.n}
                threadId=""
                runtime={where?.runtime ?? "api"}
                hostState={where?.state ?? "ready"}
                placeholder={preview || "Describe what should happen…"}
                initialText={seed.text}
                onSent={sent}
              />
            </div>
            <Suggestions
              onPreview={setPreview}
              onPick={(prompt) => {
                setPreview("");
                setSeed((s) => ({ text: prompt, n: s.n + 1 }));
              }}
            />
            <p className="au-or">or</p>
            {scratch}
          </div>
        </div>
      </Frame>
    );

  return (
    <Frame
      bar={
        <>
          <Crumbs name={thread.data?.thread.title || "New automation"} />
          <div className="au-top-tools">{scratch}</div>
        </>
      }
    >
      <div className="au-chat" data-replay-block>
        {runtime}
        <Transcript
          data={thread.data}
          loading={thread.isPending}
          failed={thread.isError && !gone}
          onRetry={() => void thread.refetch()}
        />
        <div className="au-chat-composer">
          <Composer
            threadId={threadId}
            thread={thread.data}
            runtime={where?.runtime ?? "api"}
            hostState={where?.state ?? "ready"}
            placeholder="Reply to Founder…"
            onSent={sent}
          />
        </div>
      </div>
    </Frame>
  );
}

/**
 * The suggestion chips under the composer, written for the chosen ontology:
 * a mouse's hover or the keyboard's focus previews one (`onPreview`), a
 * click or tap puts it in the composer (`onPick`). With several ontologies
 * the picker follows the chips, so Tab goes from the composer to them.
 */
function Suggestions({
  onPreview,
  onPick,
}: {
  onPreview: (prompt: string) => void;
  onPick: (prompt: string) => void;
}) {
  const me = useWorkspace(),
    ws = me.id,
    described = useId();
  const list = useQuery(ontologyQueries.list(ws));
  // Only a current version has suggestions of its own: the others would
  // show the fixed ones as if written for them.
  const ontologies = (list.data?.ontologies || []).filter(
    (o) => o.currentVersionId && readAccess(me, o) !== "none",
  );
  // Per workspace, as the page may stay while the workspace changes.
  const [chosen, setChosen] = useState<Record<string, string>>({});
  const ontologyId = suggestionOntology(
    ontologies.map((o) => o.id),
    chosen[ws] ?? readStorage(chosenKey(ws)),
    lastOntology(ws),
  );
  const read = useQuery({
    ...automationQueries.suggestions(ws, ontologyId || ""),
    enabled: !!ontologyId,
  });
  const shown = shownSuggestions({
    ontology: !!ontologyId || list.isPending,
    pending: list.isPending || read.isPending,
    failed: read.isError,
    data: read.data,
  });
  const generating = shown.generating;
  // Chips replaced under the pointer or the focus (Founder's in
  // place of the fallback, or another ontology's) leave no stale preview,
  // and a screen reader hears that they changed.
  const chipsSeen = shown.kind === "chips" ? shown.chips : undefined,
    signature = (chipsSeen || [])
      .map((x) => `${x.title}\n${x.prompt}`)
      .join("\n\n"),
    before = useRef(signature),
    [updated, setUpdated] = useState(false);
  useEffect(() => {
    if (before.current === signature) return;
    const had = before.current !== "";
    before.current = signature;
    onPreview("");
    setUpdated(had && signature !== "");
  }, [signature]);
  return (
    <>
      {shown.kind === "loading" ? (
        <ul className="au-suggestions" aria-hidden>
          {[0, 1, 2, 3].map((i) => (
            <li key={i}>
              <Skeleton className="au-suggestion-skeleton" height={42} />
            </li>
          ))}
        </ul>
      ) : (
        <ul className="au-suggestions" aria-label="Suggestions">
          {shown.chips.map((x, i) => {
            const Icon = ICONS[x.icon];
            return (
              <li key={i}>
                <button
                  type="button"
                  className="au-suggestion"
                  aria-describedby={`${described}-${i}`}
                  // A mouse's hover or the keyboard's focus previews
                  // it; a tap, or a click's focus, only fills it.
                  onPointerEnter={(e) =>
                    e.pointerType === "mouse" && onPreview(x.prompt)
                  }
                  onPointerLeave={() => onPreview("")}
                  onFocus={(e) =>
                    e.currentTarget.matches(":focus-visible") &&
                    onPreview(x.prompt)
                  }
                  onBlur={() => onPreview("")}
                  onClick={() => onPick(x.prompt)}
                >
                  <span
                    className={`au-suggestion-icon is-${SUGGESTION_TONE[x.icon]}`}
                  >
                    <Icon size={14} aria-hidden />
                  </span>
                  <span className="au-suggestion-title">{x.title}</span>
                </button>
                <span id={`${described}-${i}`} className="hf-sr-only">
                  {x.prompt}
                </span>
              </li>
            );
          })}
        </ul>
      )}
      <span role="status" className="hf-sr-only">
        {generating
          ? "Writing suggestions…"
          : shown.kind === "loading"
            ? "Loading suggestions…"
            : updated
              ? "Suggestions updated"
              : ""}
      </span>
      {ontologyId && ontologies.length > 1 && (
        <div className="au-suggestions-for">
          {/* The picker's name says it too (Label in Name), so a screen
              reader hears it once. */}
          <span aria-hidden>Suggestions for</span>
          <Select
            label="Suggestions for"
            value={ontologyId}
            onChange={(id) => {
              setChosen((c) => ({ ...c, [ws]: id }));
              writeStorage(chosenKey(ws), id);
            }}
            options={ontologies.map((o) => ({
              value: o.id,
              label: ontologyName(o),
            }))}
          />
        </div>
      )}
    </>
  );
}
