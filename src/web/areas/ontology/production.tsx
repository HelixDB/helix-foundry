import "./production.css";
import {
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { useQuery } from "@tanstack/react-query";
import { Check, ChevronDown, Copy } from "lucide-react";
import type { FoundryOntology } from "../../../shared/wfe";
import { Button, ButtonLink, cx, readStorage, writeStorage } from "../../kit";
import { useWorkspace } from "../../ui";
import { MCP_URL } from "./agent-setup";
import { canManage } from "./parts";
import { ontologyPaths as paths } from "./paths";
import {
  cardStartsMinimised,
  hasOwnOntology,
  parseProductionState,
  productionKey,
  productionProgress,
  type ProductionState,
} from "./production-model";
import { productionPrompt } from "./production-prompt";
import { ontologyQueries } from "./queries";
import { useOntology } from "./scope";
import { useSampleOntology } from "./use-sample-ontology";

// Getting into production: creating an ontology, then querying it through the
// Helix MCP with a prompt for the user's own agent: a card under the Analyst's
// bar. A local install has no Helix MCP, so no card.

const TITLE = "Getting into production";

// What this browser remembers. Another tab's change comes through `storage`.
const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
};

/**
 * The workspace's steps (`progress`, undefined until they can be told, from
 * the list the app already keeps and the sample's hook), what this browser
 * remembers of them, and copying the prompt for an ontology.
 */
function useProduction() {
  const { id, name, notify } = useWorkspace(),
    key = productionKey(id);
  const raw = useSyncExternalStore(subscribe, () => readStorage(key)),
    state = useMemo(() => parseProductionState(raw), [raw]);
  const list = useQuery({ ...ontologyQueries.list(id), enabled: false }).data,
    ontologies = list && (list.ontologies || []),
    sample = useSampleOntology(),
    own = hasOwnOntology(ontologies, sample);
  const update = (change: Partial<ProductionState>) => {
    writeStorage(
      key,
      JSON.stringify({
        ...parseProductionState(readStorage(key)),
        ...change,
      }),
    );
    listeners.forEach((listener) => listener());
  };
  const copy = (ontology: FoundryOntology) =>
    navigator.clipboard
      .writeText(
        productionPrompt({
          origin: window.location.origin,
          mcpUrl: MCP_URL,
          workspace: { id, name },
          ontology,
        }),
      )
      .then(() => {
        update({ copied: true });
        notify("Copied the prompt: paste it into your agent.");
      })
      .catch(() => notify("Couldn’t copy the prompt.", true));
  return {
    state,
    update,
    copy,
    progress:
      own === undefined ? undefined : productionProgress(own, state.copied),
  };
}

/**
 * The Analyst's card: a box under the bar's right that opens over the
 * conversation, or on a phone a row the conversation sits under
 * (production.css). Whether it starts open is decided once, before it paints,
 * so copying leaves it open until the next visit.
 */
export function ProductionCard() {
  const { role } = useWorkspace(),
    { ontology } = useOntology(),
    { state, update, copy, progress } = useProduction();
  const card = useRef<HTMLElement>(null),
    stepsId = useId(),
    [open, setOpen] = useState(false),
    known = !!progress;
  useLayoutEffect(() => {
    if (known && card.current)
      setOpen(
        !cardStartsMinimised(
          state.minimised,
          progress?.current === null,
          getComputedStyle(card.current)
            .getPropertyValue("--pd-fits")
            .trim() === "1",
        ),
      );
  }, [known]);
  if (!progress) return null;
  return (
    <div className="pd-room">
      <section
        ref={card}
        className={cx("pd-card", !open && "is-minimised")}
        aria-label={TITLE}
      >
        <button
          type="button"
          className="pd-card-head"
          aria-expanded={open}
          aria-controls={open ? stepsId : undefined}
          title={open ? "Minimise" : "Show the steps"}
          onClick={() => {
            setOpen(!open);
            update({ minimised: open });
          }}
        >
          <span className="pd-card-title">{TITLE}</span>
          <span className="pd-card-count">{progress.done} of 2</span>
          <ChevronDown size={16} className="pd-card-chevron" aria-hidden />
        </button>
        {open && (
          <ol id={stepsId} className="pd-steps">
            {progress.steps.map((s, i) => (
              <li
                key={s.id}
                className={cx("pd-step", `is-${s.state}`)}
                aria-current={s.state === "current" ? "step" : undefined}
              >
                <span className="pd-step-mark" aria-hidden>
                  {s.state === "done" ? <Check size={12} /> : i + 1}
                </span>
                <div className="pd-step-text">
                  <span className="pd-step-title">
                    {s.title}
                    {s.state === "done" && (
                      <span className="hf-sr-only"> (done)</span>
                    )}
                  </span>
                  {s.id === "query" ? (
                    <>
                      <p className="pd-step-note">
                        A prompt for Claude Code, Codex or another agent: it
                        adds the MCP, queries this ontology and helps wire it
                        into your app.
                      </p>
                      <Button
                        size="sm"
                        icon={Copy}
                        disabled={s.state === "upcoming"}
                        onClick={() => copy(ontology)}
                      >
                        Copy prompt
                      </Button>
                    </>
                  ) : s.state === "done" ? null : canManage(role) ? (
                    <>
                      <p className="pd-step-note">
                        The sample shows how it works. Production needs your own
                        data.
                      </p>
                      <ButtonLink size="sm" to={paths.newOntology()}>
                        New ontology
                      </ButtonLink>
                    </>
                  ) : (
                    <p className="pd-step-note">
                      An owner or admin of this workspace creates one.
                    </p>
                  )}
                </div>
              </li>
            ))}
          </ol>
        )}
      </section>
    </div>
  );
}
