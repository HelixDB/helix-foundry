import "./ask.css";
import "./founder.css";
import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../../api";
import { readStorage, useMedia, useTitle, writeStorage } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  Composer,
  RuntimeLine,
  Transcript,
  useThreadRuntime,
} from "../agent/dock";
import { agentRefusal, threadKey } from "../agent/model";
import { agentQueries } from "../agent/queries";
import { useReadAccess, useReadsPart } from "../explore/graph-reader";
import { PartNote } from "../explore/part-note";
import { AskPage, ChatBar, ThreadView } from "./ask";
import { askSuggestions, threadsOf } from "./ask-model";
import { founderView } from "./founder-model";
import { FounderThreads } from "./founder-threads";
import {
  LoadNotice,
  LoadPicker,
  NotShared,
  useLoadSelection,
} from "./load-selection";
import { ontologyQueries, questionQueries } from "./queries";
import { OntologyPicker, ontologyName, useOntology } from "./scope";

// /ask/:ontologyId where the build has Founder: Founder, full page. The
// Analyst page's chrome (its slim bar with the thread's title and the
// Ontology and Load pickers, Getting into production's card under it)
// around the dock's conversation (its turns, the Analyst's cited answers
// inline, cards, Undo) and composer, which sends this page (the ontology
// and the load picked) with each message. Its
// threads are the dock's, in the sidebar (founder-threads.tsx): `?chat=`
// opens one and makes it the dock's too, so Founder holds one conversation
// wherever it is asked; without it, the page opens the dock's thread, else a
// new chat. `?thread=` is an Analyst thread from before Founder, read only,
// so every Analyst link still reads. A new chat asks what the user wants to
// know, its composer centred, with the load's competency questions as chips
// that fill it; `?about=` (an entity page's Ask about this) starts its
// message. The shell shows no dock here. Where the WFE has Founder off, the
// Analyst's page. A grantee's Founder only reads and asks, about the load its
// grants cover (no Load picker, no cards); where its grants don't cover the
// ontology's current load, the page says so in place of any composer.

export function FounderPage() {
  const { id: ws, grantee } = useWorkspace(),
    qc = useQueryClient(),
    { id: o, ontology } = useOntology(),
    access = useReadAccess(),
    part = useReadsPart();
  const [params, setParams] = useSearchParams();
  const selection = useLoadSelection();
  const load =
    selection.choice.kind === "load" ? selection.choice.load : undefined;
  const threads = useQuery(agentQueries.threads(ws));
  const list = threads.data?.threads;
  // The thread the dock holds, which the page opens without `?chat=`.
  const [stored, setStored] = useState(() => readStorage(threadKey(ws)));
  useEffect(() => setStored(readStorage(threadKey(ws))), [ws]);
  const view = founderView(
    params,
    useMedia("(max-width: 800px)"),
    list,
    stored,
  );
  const chat = view.kind === "chat" ? view.id : "";
  const thread = useQuery({
    ...agentQueries.thread(ws, chat),
    enabled: !!chat,
    retry: false,
  });
  const where = useThreadRuntime(chat, thread.data);
  const gone = thread.error instanceof ApiError && thread.error.status === 404;
  // The thread open here is the dock's too, on every other page.
  useEffect(() => {
    if (!chat || gone) return;
    writeStorage(threadKey(ws), chat);
    setStored(chat);
  }, [ws, chat, gone]);
  // A thread deleted elsewhere starts a new chat.
  useEffect(() => {
    if (!gone) return;
    writeStorage(threadKey(ws), null);
    setStored("");
    go({ new: true }, true);
  }, [gone]);
  // An Analyst thread's questions, and its asker's budget.
  const questions = useQuery({
    ...questionQueries.list(ws, o),
    enabled: view.kind === "analyst" && access !== "none",
  });
  // The version the load holds names the suggestions. A version can be
  // large: it is read for them only while Founder has no thread yet, else
  // only if a page already read it.
  const version = useQuery({
    ...ontologyQueries.version(ws, o, load?.versionId || ""),
    enabled: !!load?.versionId && list?.length === 0,
  }).data?.version;
  const suggestions = useMemo(() => askSuggestions(version), [version]);
  // What the new chat's composer starts with: a chip's question, or the
  // entity an Ask about this link names; a new key makes a new composer.
  const [seed, setSeed] = useState(() => {
    const about = params.get("about")?.trim();
    return { text: about ? `About ${about}: ` : "", n: 0 };
  });
  useTitle(
    view.kind === "analyst"
      ? undefined
      : view.kind === "chat"
        ? thread.data
          ? thread.data.thread.title || "Founder"
          : undefined
        : "Founder",
  );
  // Opens a chat, a new one, or (empty) the threads, keeping `?load=`.
  function go(to: { chat?: string; new?: boolean }, replace = false) {
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        for (const k of ["chat", "thread", "new", "focus", "about"])
          next.delete(k);
        if (to.chat) next.set("chat", to.chat);
        if (to.new) next.set("new", "1");
        return next;
      },
      { replace },
    );
  }
  const sent = (id: string) => {
    void qc.invalidateQueries({ queryKey: [ws, "agent", "threads"] });
    void qc.invalidateQueries({ queryKey: [ws, "agent", "thread", id] });
    setSeed((s) => ({ text: "", n: s.n + 1 }));
    if (id !== chat) go({ chat: id });
  };
  if (agentRefusal(threads.error) === "FOUNDRY_AGENT_DISABLED")
    return <AskPage />;
  const picker = (
    <>
      <OntologyPicker ontologyId={o} to={paths.ask} />
      <LoadPicker selection={selection} />
    </>
  );
  const back = list?.length ? () => go({}) : undefined;
  const runtime = where && where.runtime !== "api" && (
    <RuntimeLine
      runtime={where.runtime}
      state={where.state}
      helper={where.helper}
      thread={!!chat}
    />
  );

  // Read only through grants that don't cover the current load: nothing to
  // ask about, so no composer, whichever view the link names.
  if (access === "none")
    return (
      <div className="ak-chat">
        <ChatBar
          title={view.kind === "analyst" ? "Earlier question" : "Founder"}
        >
          {picker}
        </ChatBar>
        <div className="ak-scroll ak-chat--empty">
          <NotShared />
        </div>
      </div>
    );

  if (view.kind === "analyst") {
    const asked = threadsOf(questions.data?.questions || []);
    return (
      <ThreadView
        key={view.id}
        readOnly
        thread={view.id}
        title={asked.find((t) => t.id === view.id)?.questions[0]?.question}
        load={load}
        budget={questions.data?.budget}
        picker={picker}
        notice={
          <>
            <PartNote />
            <LoadNotice selection={selection} />
          </>
        }
        onBack={() => go({})}
      />
    );
  }

  if (view.kind === "threads")
    return (
      <div className="ak-chat">
        <ChatBar title="Founder">{picker}</ChatBar>
        <div className="ak-scroll">
          <FounderThreads />
        </div>
      </div>
    );

  if (view.kind === "chat")
    return (
      <div className="ak-chat">
        <ChatBar title={thread.data?.thread.title || "Thread"} onBack={back}>
          {picker}
        </ChatBar>
        <div className="fd-chat" data-replay-block>
          {part && (
            <div className="fd-notice">
              <PartNote />
            </div>
          )}
          {runtime}
          <Transcript
            data={thread.data}
            loading={thread.isPending}
            failed={thread.isError && !gone}
            onRetry={() => void thread.refetch()}
          />
          <div className="fd-composer">
            <Composer
              key={chat}
              threadId={chat}
              thread={thread.data}
              runtime={where?.runtime ?? "api"}
              hostState={where?.state ?? "ready"}
              placeholder="Reply to Founder…"
              onSent={sent}
            />
          </div>
        </div>
      </div>
    );

  // Nothing loaded: the Analyst has nothing to answer from, but Founder can
  // load data, or say how (a grantee's can't: it only reads).
  const nothing = selection.choice.kind === "none" && !selection.pending;
  const name = ontologyName(ontology);
  return (
    <div className="ak-chat">
      <ChatBar onBack={back}>{picker}</ChatBar>
      <div className="ak-scroll">
        <div className="ak-start fd-start" data-replay-block>
          <LoadNotice selection={selection} />
          <h1 className="ak-hello">What do you want to know about {name}?</h1>
          <PartNote />
          {nothing && (
            <p className="fd-note">
              {grantee
                ? "Nothing shared with you is loaded yet."
                : "Nothing is loaded yet, so Founder can’t answer questions about the data until it is; it can help you load it."}
            </p>
          )}
          {runtime}
          <Composer
            key={seed.n}
            threadId=""
            runtime={where?.runtime ?? "api"}
            hostState={where?.state ?? "ready"}
            placeholder={
              grantee
                ? "Ask about the data shared with you…"
                : `Ask about ${name}, or ask Founder to do something…`
            }
            initialText={seed.text}
            onSent={sent}
          />
          {!nothing && suggestions.length > 0 && (
            <div
              className="ak-suggestions"
              role="group"
              aria-label="Suggested questions"
            >
              {suggestions.map((q) => (
                <button
                  key={q}
                  type="button"
                  className="ak-suggestion"
                  onClick={() => setSeed((s) => ({ text: q, n: s.n + 1 }))}
                >
                  {q}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
