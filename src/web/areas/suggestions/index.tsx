// Suggestions area: what audits of an ontology's current load found its
// ontology and mappings lack (self-healing), lazy-loaded by the Ontology
// area's route table under /ontology/:ontologyId/suggestions. Each
// suggestion shows its kind, its support, examples and what it proposes; a
// manager may dismiss one. Nothing here changes the ontology. Where
// foundry.heal.enabled is off, it says suggestions aren't turned on.
import "./suggestions.css";
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Lightbulb } from "lucide-react";
import type { ListHealFindingsResponse } from "../../../shared/wfe";
import {
  Button,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  Tag,
  plural,
  useTitle,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import { documentSetQueries } from "../documents/documents-api";
import { canManage } from "../ontology/parts";
import { ontologyName, useOntology } from "../ontology/scope";
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

// The dismissal behind the Undo toast: its finding, and the timer that
// sends it.
type Held = { fingerprint: string; timer: number };

export function SuggestionsPage() {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  useTitle("Suggestions");
  const qc = useQueryClient();
  const listKey = suggestionQueries.list(ws, o).queryKey;
  const current = ontology.currentLoad?.id;
  const list = useQuery({
      ...suggestionQueries.list(ws, o),
      // Until the current load's audit completes, its suggestions are on
      // their way.
      refetchInterval: (q) =>
        auditPending(q.state.data, current) ? POLL_MS : false,
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
  const header = (
    <PageHeader
      breadcrumbs={[
        { label: "Ontologies", to: paths.ontologies() },
        { label: ontologyName(ontology), to: paths.ontology(o) },
        { label: "Suggestions" },
      ]}
      title="Suggestions"
      subtitle="What audits of the current load found the ontology lacks, most backed first. Changes to a source’s fields aren’t checked yet."
    />
  );
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
  if (list.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={6} />
      </Page>
    );
  if (!list.data)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Lightbulb}
          title="Couldn’t load the suggestions"
          text={list.error?.message}
          action={<Button onClick={() => void list.refetch()}>Retry</Button>}
        />
      </Page>
    );
  if (!list.data.enabled)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Lightbulb}
          title="Suggestions aren’t turned on"
          text="This Foundry doesn’t audit loads, so it has nothing to suggest."
        />
      </Page>
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
    <Page>
      {header}
      <div className="on-stack" ref={listRef} tabIndex={-1}>
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
    </Page>
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
  const { role } = useWorkspace(),
    { id: o } = useOntology();
  const [all, setAll] = useState(false);
  const examples = all ? s.examples : s.examples.slice(0, SHOWN_EXAMPLES);
  // The button is named Dismiss alone, described by the card's title: an
  // accessible name may reach analytics, and the title quotes the data.
  const titleId = `sg-title-${s.fingerprint}`,
    examplesId = `sg-examples-${s.fingerprint}`;
  return (
    <Panel
      title={<span id={titleId}>{s.title}</span>}
      subtitle={s.detail}
      actions={
        canManage(role) && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => onDismiss(s.fingerprint)}
            aria-describedby={titleId}
            data-fingerprint={s.fingerprint}
          >
            Dismiss
          </Button>
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
