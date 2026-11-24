// Suggestions area: what audits of an ontology's current load found its
// ontology and mappings lack (self-healing), the Review page's Ontology tab
// (../reviews/page.tsx, /ontology/:ontologyId/review?tab=ontology). Each
// suggestion shows its kind, its support, examples and what it proposes; a
// manager may dismiss one, or add a link for a relationship no link covers:
// in place (the current version's Edit tab saves a version with the link,
// and making it current moves the current load to it, ./add-link.ts), or by
// drafting with the agent (Generate). Where foundry.heal.enabled is off, the
// page has no Ontology tab.
import "./suggestions.css";
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Lightbulb, Link2, Sparkles } from "lucide-react";
import type { ListHealFindingsResponse } from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  EmptyState,
  Panel,
  SkeletonText,
  Tag,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, canManage, useWorkspace } from "../../ui";
import { documentSetQueries } from "../documents/documents-api";
import { setCurrentVersion } from "../ontology/queries";
import { useOntology } from "../ontology/scope";
import { stateText } from "./add-link";
import { dismissSuggestion, suggestionQueries } from "./api";
import {
  auditPending,
  emptyText,
  lastRunText,
  suggestion,
  type Suggestion,
} from "./model";

// How many examples a card shows before "Show all".
const SHOWN_EXAMPLES = 3;
// How long a dismissal waits behind its toast, for Undo, before it is sent.
const UNDO_MS = 5000;

// How often the page asks again while the current load isn't audited
// whole, or an audit of it runs.
const POLL_MS = 15_000;
// How often it asks while the current load moves to a version: a move
// asks no model, so it ends in seconds.
const MOVE_POLL_MS = 2_000;

// The dismissal behind the Undo toast: its finding, and the timer that
// sends it.
type Held = { fingerprint: string; timer: number };

export function OntologySuggestions() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const qc = useQueryClient();
  const listKey = suggestionQueries.list(ws, o).queryKey;
  const current = ontology.currentLoad?.id;
  const list = useQuery({
      ...suggestionQueries.list(ws, o),
      // Until the current load's audit completes, its suggestions are on
      // their way; while a load moves to a version that adds a link, or
      // fills the documents behind one with it, its card says how far.
      refetchInterval: (q) =>
        auditPending(q.state.data, current)
          ? POLL_MS
          : q.state.data?.findings?.some(
                (f) => f.state?.moving || f.state?.applying,
              )
            ? MOVE_POLL_MS
            : false,
    }),
    sets = useQuery(documentSetQueries.list(ws));
  // The card a dismissal, its undo or its failure moves focus to: a
  // fingerprint, "" for the list, null for none.
  const [focusNext, setFocusNext] = useState<string | null>(null),
    listRef = useRef<HTMLDivElement>(null);
  // Every dismissal not done yet stays hidden, whatever the list says:
  // the one behind the Undo toast, and those being sent. Then the last one
  // that failed.
  const [held, setHeld] = useState<Held | null>(null),
    [hidden, setHidden] = useState<ReadonlySet<string>>(new Set()),
    [failed, setFailed] = useState(""),
    heldRef = useRef<Held | null>(null);
  heldRef.current = held;
  const hide = (fingerprint: string, on: boolean) =>
    setHidden((now) => {
      const next = new Set(now);
      if (on) next.add(fingerprint);
      else next.delete(fingerprint);
      return next;
    });
  const shown = (list.data?.findings || [])
    .filter((f) => !hidden.has(f.fingerprint))
    .map((f) => suggestion(f, sets.data));
  const send = async (fingerprint: string) => {
    try {
      await dismissSuggestion(ws, o, fingerprint);
      // Dismissed: the list no longer holds it, refreshed or not.
      qc.setQueryData<ListHealFindingsResponse>(
        listKey,
        (data) =>
          data && {
            ...data,
            findings: data.findings?.filter(
              (f) => f.fingerprint !== fingerprint,
            ),
          },
      );
    } catch (e) {
      // Not dismissed: it is back where it stood (unless the refreshed list
      // drops it, as superseded), and focus with it.
      setFailed((e as Error).message);
      setFocusNext(fingerprint);
    }
    hide(fingerprint, false);
    void qc.invalidateQueries({ queryKey: listKey });
  };
  // A dismissal waiting when the page goes is sent at once. A tab closed
  // within its toast's few seconds keeps the suggestion open.
  useEffect(
    () => () => {
      const h = heldRef.current;
      if (!h) return;
      clearTimeout(h.timer);
      void dismissSuggestion(ws, o, h.fingerprint);
    },
    [ws, o],
  );
  useEffect(() => {
    if (focusNext === null) return;
    const card =
      focusNext &&
      listRef.current?.querySelector<HTMLElement>(
        `[data-fingerprint="${focusNext}"]`,
      );
    (card || listRef.current)?.focus();
    setFocusNext(null);
  }, [focusNext, list.data, hidden]);
  if (list.isPending) return <SkeletonText lines={6} />;
  if (!list.data)
    return (
      <EmptyState
        icon={Lightbulb}
        title="Couldn’t load the suggestions"
        text={list.error?.message}
        action={<Button onClick={() => void list.refetch()}>Retry</Button>}
      />
    );
  const run = lastRunText(list.data.lastRun, {
    current,
    shown: shown.length > 0,
  });
  // A card goes at once, and its dismissal waits behind the toast for
  // UNDO_MS, then is sent; one already waiting is sent now.
  const dismiss = (fingerprint: string) => {
    const at = shown.findIndex((s) => s.fingerprint === fingerprint),
      next = shown[at + 1] || shown[at - 1];
    if (held) {
      clearTimeout(held.timer);
      void send(held.fingerprint);
    }
    setFailed("");
    hide(fingerprint, true);
    const h: Held = { fingerprint, timer: 0 };
    h.timer = window.setTimeout(() => {
      setHeld((now) => (now === h ? null : now));
      void send(fingerprint);
    }, UNDO_MS);
    setHeld(h);
    setFocusNext(next ? next.fingerprint : "");
  };
  const undo = () => {
    if (!held) return;
    clearTimeout(held.timer);
    setHeld(null);
    hide(held.fingerprint, false);
    setFocusNext(held.fingerprint);
  };
  return (
    <>
      <div className="on-stack" ref={listRef} tabIndex={-1}>
        <p className="on-muted sg-run">
          Most backed first. Changes to a source’s fields aren’t checked yet.
        </p>
        <RefreshFailed query={list} />
        {failed && (
          <p role="alert" className="on-error-text">
            Couldn’t dismiss the suggestion: {failed}
          </p>
        )}
        {run && <p className="on-muted sg-run">{run}</p>}
        {!shown.length ? (
          <EmptyState
            icon={Lightbulb}
            title="No suggestions"
            text={emptyText(current, list.data.lastCompleteRun)}
          />
        ) : (
          <ul className="sg-list" aria-label="Suggestions">
            {shown.map((s) => (
              <li key={s.fingerprint}>
                <SuggestionCard
                  suggestion={s}
                  another={!!current && !!s.loadId && s.loadId !== current}
                  onDismiss={dismiss}
                />
              </li>
            ))}
          </ul>
        )}
      </div>
      {held && (
        <div role="status" className="hf-toast sg-toast">
          <span>Suggestion dismissed.</span>
          <button type="button" className="sg-undo" onClick={undo}>
            Undo
          </button>
        </div>
      )}
    </>
  );
}

function SuggestionCard({
  suggestion: s,
  another,
  onDismiss,
}: {
  suggestion: Suggestion;
  // Counted over another load than the current one.
  another: boolean;
  onDismiss: (fingerprint: string) => void;
}) {
  const { id: ws, role, notify } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const qc = useQueryClient(),
    navigate = useNavigate();
  const [all, setAll] = useState(false),
    [pending, setPending] = useState(false);
  const state = s.state,
    open = !state || state.kind === "open",
    line = state && stateText(state);
  // Covered: make the version that adds the link current, moving the load.
  const makeCurrent = async (versionId: string) => {
    setPending(true);
    try {
      await setCurrentVersion(ws, o, {
        versionId,
        expectedCurrentVersionId: ontology.currentVersionId || "",
        fillFinding: s.fingerprint,
      });
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
      void qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
    }
  };
  const examples = all ? s.examples : s.examples.slice(0, SHOWN_EXAMPLES);
  // Its buttons are named Add this link and Dismiss alone, described by the
  // card's title: an accessible name may reach analytics, and the title
  // quotes the data. Add this link opens Generate with the goal filled in.
  const titleId = `sg-title-${s.fingerprint}`,
    examplesId = `sg-examples-${s.fingerprint}`,
    manager = canManage(role);
  return (
    <Panel
      title={<span id={titleId}>{s.title}</span>}
      subtitle={s.detail}
      actions={
        manager && (
          <>
            {open && s.addLink && ontology.currentVersionId && (
              <ButtonLink
                size="sm"
                icon={Link2}
                to={paths.addLink(o, ontology.currentVersionId, {
                  fingerprint: s.fingerprint,
                  ...s.addLink,
                })}
                aria-describedby={titleId}
              >
                Add this link
              </ButtonLink>
            )}
            {open && s.goal && (
              <ButtonLink
                size="sm"
                variant={s.addLink ? "ghost" : undefined}
                icon={Sparkles}
                to={paths.generate(o, { goal: s.goal })}
                aria-describedby={titleId}
              >
                {s.addLink ? "Draft with the agent" : "Add this link"}
              </ButtonLink>
            )}
            {state?.kind === "covered" && state.versionId && (
              <Button
                size="sm"
                pending={pending}
                onClick={() => void makeCurrent(state.versionId)}
                aria-describedby={titleId}
              >
                Make it current
              </Button>
            )}
            {state?.kind === "needs_load" && state.versionId && (
              <Button
                size="sm"
                onClick={() =>
                  navigate(paths.version(o, state.versionId), {
                    state: { openLoad: true },
                  })
                }
                aria-describedby={titleId}
              >
                Start a new load
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => onDismiss(s.fingerprint)}
              aria-describedby={titleId}
              data-fingerprint={s.fingerprint}
            >
              Dismiss
            </Button>
          </>
        )
      }
    >
      <div className="on-stack">
        <p className="sg-facts">
          <Tag>{s.kindLabel}</Tag>
          {another && <Tag>Another load</Tag>}
          <span>
            {s.documents
              ? `${plural(s.support, "review")} in ${plural(s.documents, "document")}`
              : plural(s.support, "row")}
          </span>
        </p>
        {s.proposal && <p>Suggested: {s.proposal}</p>}
        {line && (
          <p role="status" className="sg-state">
            {line}
          </p>
        )}
        {manager && open && s.goal && s.support > 0 && (
          // Support is the reviews behind it: each a claim a document
          // states that no link let a load store.
          <p>
            {`Adding this link lets future loads store ${s.support === 1 ? "this claim" : `these ${plural(s.support, "claim")}`}.`}
          </p>
        )}
        {s.spellings.length > 1 && (
          <p className="on-muted">
            Reported as {s.spellings.map((x) => `“${x}”`).join(", ")}: check the
            quotes say the same thing.
          </p>
        )}
        {examples.length > 0 && (
          <ul className="sg-examples" aria-label="Examples" id={examplesId}>
            {examples.map((e, i) => (
              <li key={i}>
                <q>{e.quote}</q>
                {e.docId && (
                  <Link to={paths.document(o, e.docId)}>Open document</Link>
                )}
              </li>
            ))}
          </ul>
        )}
        {s.examples.length > SHOWN_EXAMPLES && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setAll(!all)}
            aria-expanded={all}
            aria-controls={examplesId}
          >
            {all ? "Show fewer" : `Show all ${s.examples.length} examples`}
          </Button>
        )}
      </div>
    </Panel>
  );
}
