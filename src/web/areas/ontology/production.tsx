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
import { useLocation } from "react-router-dom";
import { Check, ChevronDown, Copy, X } from "lucide-react";
import type { FoundryOntology } from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  IconButton,
  cx,
  readStorage,
  writeStorage,
} from "../../kit";
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
  promptOntology,
  stripShown,
  type ProductionState,
} from "./production-model";
import { productionPrompt } from "./production-prompt";
import { ontologyQueries } from "./queries";
import { lastOntology, useOntology } from "./scope";
import { useSampleOntology } from "./use-sample-ontology";

// Getting into production: creating an ontology, then querying it through the
// Helix MCP with a prompt for the user's own agent. On the Analyst, a card
// under the bar (ProductionCard); everywhere else, a strip above the page
// (ProductionStrip, AreaShell's strip). A local install has no Helix MCP, so
// neither.

const TITLE = "Getting into production";

// The card and the strip share what this browser remembers: the strip stays
// mounted, showing nothing, while the card copies. Another tab's change comes
// through `storage`.
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
    ontologies: ontologies || [],
    sample,
    progress:
      own === undefined ? undefined : productionProgress(own, state.copied),
  };
}

/**
 * The Analyst's card, under the bar. It floats beside the conversation's
 * column where it fits, and else is a row the conversation sits under
 * (production.css), so it never covers the column or the composer. Never
 * dismissed, so the prompt stays reachable; whether it starts open is decided
 * once, before it paints, so copying leaves it open until the next visit.
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
          getComputedStyle(card.current).position === "absolute",
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

/**
 * The strip above every other page: its label, the two stations and the
 * current step's action, until both steps are done or it is dismissed. On a
 * phone, just the stations and the action.
 */
export function ProductionStrip() {
  const { id, role, onboarding } = useWorkspace(),
    { pathname } = useLocation(),
    { state, update, copy, ontologies, sample, progress } = useProduction();
  if (!progress || !stripShown(pathname, onboarding, state, progress))
    return null;
  const ontology =
    progress.current === "query"
      ? promptOntology(pathname, ontologies, lastOntology(id), sample?.id)
      : undefined;
  return (
    <div className="hf-strip" role="region" aria-label={TITLE}>
      <span className="pd-strip-label">{TITLE}</span>
      <ol className="pd-stations" aria-label={`${progress.done} of 2 done`}>
        {progress.steps.map((s) => (
          <li
            key={s.id}
            className={cx("pd-station", `is-${s.state}`)}
            aria-current={s.state === "current" ? "step" : undefined}
            title={s.title}
          >
            <span className="hf-sr-only">
              {s.title}
              {s.state === "done" && " (done)"}
            </span>
          </li>
        ))}
      </ol>
      <span className="pd-strip-action">
        {ontology ? (
          <Button size="sm" icon={Copy} onClick={() => copy(ontology)}>
            Copy MCP prompt
          </Button>
        ) : canManage(role) ? (
          <ButtonLink size="sm" to={paths.newOntology()}>
            Create an ontology
          </ButtonLink>
        ) : (
          <span className="pd-strip-note">An owner or admin creates one</span>
        )}
      </span>
      <IconButton
        size="sm"
        icon={X}
        label={`Dismiss ${TITLE}`}
        className="pd-strip-dismiss"
        onClick={() => update({ dismissed: true })}
      />
    </div>
  );
}
