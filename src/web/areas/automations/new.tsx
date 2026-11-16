import { useEffect, useId, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CalendarClock,
  FileSearch,
  type LucideIcon,
  Newspaper,
  Plus,
  Sparkles,
  TrendingDown,
} from "lucide-react";
import { ApiError } from "../../api";
import { ButtonLink, Section } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  Composer,
  RuntimeLine,
  Transcript,
  useThreadRuntime,
} from "../agent/dock";
import { agentRefusal, resultLink } from "../agent/model";
import { agentQueries } from "../agent/queries";
import { Crumbs, DraftPage, Frame } from "./editor";
import { AUTOMATION_SUGGESTIONS } from "./model";
import { Templates } from "./parts";
import { automationPaths as paths } from "./paths";
import { useNames } from "./queries";

// /automations/new: a new automation starts as a conversation with the
// Founder (the general one, the dock's: its thread, turns and cards), its
// composer centred over the canvas's dots, a suggestion's prompt a chip away
// (hovered or focused, it is the composer's placeholder; clicked, its text),
// or from scratch on the canvas (?template=blank, as every template's
// draft). Sending opens a new thread, which the page then holds whole
// (?thread=), and which the dock lists too; the page context says the
// person is making an automation. Nothing runs until they click a card:
// once their click on Founder's create_automation card made it, the
// page opens it on its canvas.

const ICONS: Record<
  (typeof AUTOMATION_SUGGESTIONS)[number]["tone"],
  LucideIcon
> = {
  info: Newspaper,
  accent: CalendarClock,
  warning: FileSearch,
  danger: TrendingDown,
};

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
    [preview, setPreview] = useState(""),
    described = useId();
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
        <div className="au-new-page ag-dots" data-replay-block>
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
                minRows={3}
                onSent={sent}
              />
            </div>
            <ul className="au-suggestions" aria-label="Suggestions">
              {AUTOMATION_SUGGESTIONS.map((x, i) => {
                const Icon = ICONS[x.tone];
                return (
                  <li key={x.title}>
                    <button
                      type="button"
                      className="au-suggestion"
                      aria-describedby={`${described}-${i}`}
                      // A mouse's hover or the keyboard's focus previews
                      // it; a tap, or a click's focus, only fills it.
                      onPointerEnter={(e) =>
                        e.pointerType === "mouse" && setPreview(x.prompt)
                      }
                      onPointerLeave={() => setPreview("")}
                      onFocus={(e) =>
                        e.currentTarget.matches(":focus-visible") &&
                        setPreview(x.prompt)
                      }
                      onBlur={() => setPreview("")}
                      onClick={() => {
                        setPreview("");
                        setSeed((s) => ({ text: x.prompt, n: s.n + 1 }));
                      }}
                    >
                      <span className={`au-suggestion-icon is-${x.tone}`}>
                        <Icon size={14} aria-hidden />
                      </span>
                      {x.title}
                    </button>
                    <span id={`${described}-${i}`} className="hf-sr-only">
                      {x.prompt}
                    </span>
                  </li>
                );
              })}
            </ul>
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
      <div className="au-chat ag-dots" data-replay-block>
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
            minRows={1}
            onSent={sent}
          />
        </div>
      </div>
    </Frame>
  );
}
