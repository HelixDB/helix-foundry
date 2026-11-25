import { useRef, useState } from "react";
import { ArrowUp, CornerDownRight, Hourglass } from "lucide-react";
import { Button, Panel, cx } from "../../kit";
import { trackEvent } from "../../analytics";
import { PromptInput } from "../agent/dock";
import { MAX_QUESTION } from "./ask-model";
import { SAMPLE_LOAD_MINUTES, type SampleLoadWait } from "./sample";

// The sample tour's question while its load runs (./sample-tour.tsx): typed
// or picked from the draft's questions, it is held in this browser and asked
// the moment the load is ready, and its answer then opens in Ask. One is held
// at a time; holding another replaces it.

const MOD =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform)
    ? "⌘"
    : "Ctrl+";

/** The wait in words, against the sample's usual load time. */
function waitText(wait: SampleLoadWait) {
  switch (wait.kind) {
    case "not_started":
      return `The load starts once the data is in, and usually takes about ${SAMPLE_LOAD_MINUTES.usual} minutes.`;
    case "usual":
      return `Loading usually takes about ${SAMPLE_LOAD_MINUTES.usual} minutes: about ${wait.minutes === 1 ? "a minute" : `${wait.minutes} minutes`} left.`;
    case "late":
      return `Nearly there: most loads finish within ${SAMPLE_LOAD_MINUTES.most} minutes.`;
    case "slow":
      return "This load is taking longer than usual. It carries on by itself, here or not.";
  }
}

export function SampleAskWhileLoading({
  held,
  suggestions,
  wait,
  onHold,
  onDrop,
}: {
  held?: string;
  // The draft's questions, once its version is read.
  suggestions: string[];
  wait: SampleLoadWait;
  onHold: (question: string) => void;
  onDrop: () => void;
}) {
  const [text, setText] = useState(""),
    input = useRef<HTMLTextAreaElement>(null);
  const question = text.trim(),
    tooLong = [...question].length > MAX_QUESTION;
  const hold = (q: string) => {
    trackEvent("sample_question_held");
    onHold(q);
    setText("");
  };
  return (
    <Panel
      className="on-tour-wait"
      title="Ask Acme Corp a question while it loads"
      subtitle={waitText(wait)}
    >
      {held ? (
        <div className="on-tour-held" role="status">
          <Hourglass size={16} aria-hidden />
          <span>
            <strong>“{held}”</strong>
            <small>
              Asked the moment the load is ready, and its answer opens. You can
              leave and come back: it waits in this browser.
            </small>
          </span>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setText(held);
              onDrop();
              input.current?.focus();
            }}
          >
            Change
          </Button>
        </div>
      ) : (
        <form
          className="ag-composer"
          aria-label="Ask once loaded"
          onSubmit={(e) => {
            e.preventDefault();
            if (question && !tooLong) hold(question);
          }}
        >
          <div
            className="ag-box"
            onClick={(e) => {
              if (e.target === e.currentTarget) input.current?.focus();
            }}
          >
            <PromptInput
              inputRef={input}
              className="ag-input"
              aria-label="Question"
              aria-keyshortcuts="Meta+Enter Control+Enter"
              minRows={2}
              maxRows={6}
              value={text}
              placeholder="e.g. Which renewals are at risk, and why?"
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                  e.preventDefault();
                  e.currentTarget.form?.requestSubmit();
                }
              }}
            />
            <button
              type="submit"
              className="ag-send"
              aria-label="Ask once loaded"
              title="Ask once loaded"
              disabled={!question || tooLong}
            >
              <ArrowUp size={17} aria-hidden />
            </button>
          </div>
          <p className={cx("ag-status", tooLong && "is-error")}>
            {tooLong
              ? `${[...question].length - MAX_QUESTION} characters too long.`
              : `Held until the load is ready, then asked · ${MOD}Enter`}
          </p>
          {suggestions.length > 0 && (
            <div
              className="ak-suggestions"
              role="group"
              aria-label="Questions to ask Acme Corp"
            >
              {suggestions.map((q) => (
                <button
                  key={q}
                  type="button"
                  className="ak-suggestion"
                  onClick={() => hold(q)}
                >
                  <CornerDownRight size={14} aria-hidden />
                  <span>{q}</span>
                </button>
              ))}
            </div>
          )}
        </form>
      )}
    </Panel>
  );
}
