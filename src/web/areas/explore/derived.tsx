import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, ArrowRight, Sparkles } from "lucide-react";
import type {
  DerivedFact,
  EntityRef,
  EntityResult,
  ExplainResult,
  Pred,
  StepPred,
  Via,
} from "../../../shared/foundry-read";
import { Button, EmptyState, Panel, Tag, plural } from "../../kit";
import { useWorkspace } from "../../ui";
import { useOntology } from "../ontology/scope";
import {
  graphReadQueries,
  predLabel,
  readFailureText,
  sameRef,
} from "./graph-read";
import type { Naming } from "./naming";
import { readQueries } from "./reads";
import { refKey } from "./rule-breaks";
import { useTenantReads } from "./tenant-reads";

// What a load's rules derive about an entity, evaluated when the page reads
// it (./graph-read.ts): derived links, relations and facts beside its
// stated ones, its flags, and "why" for each: the proof, down to the
// stated facts and types it rests on. Nothing here is stored; a pred too
// costly to compute live says so instead of showing part of its answer.

/**
 * How a page names and links an entity of a read: the load and version it
 * reads, and the names it already has (useNamed reads the others).
 */
export type RefView = {
  loadId: string;
  naming: Naming;
  name: (r: EntityRef) => string | undefined;
  // Its page, or undefined when its type isn't the version's.
  to: (r: EntityRef) => string | undefined;
};
type Named = RefView & { name: (r: EntityRef) => string };

/**
 * The view with every entity of `refs` named: the page's names, else one
 * lookup of the rest in the load (their ids until it answers).
 */
function useNamed(view: RefView, refs: EntityRef[]): Named {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    { read } = useTenantReads();
  const names = useQuery(
    readQueries.names(
      ws,
      o,
      read,
      view.loadId,
      refs.filter((r) => !view.name(r)),
      view.naming,
    ),
  );
  return {
    ...view,
    name: (r) => view.name(r) || names.data?.get(refKey(r)) || r.id,
  };
}

function RefLink({ r, view }: { r: EntityRef; view: Named }) {
  const to = view.to(r);
  return to ? <Link to={to}>{view.name(r)}</Link> : <>{view.name(r)}</>;
}

/** A declaration (`decl:<owner>.<decl>`) in words: "in is declared transitive". */
function declText(owner: string, decl: string) {
  const [what = "", target] = decl.split(/\.(.*)/);
  switch (what) {
    case "inverse_of":
      return `${owner} is declared the inverse of ${target}`;
    case "implies":
      return `${owner} is declared to imply ${target}`;
    case "symmetric":
    case "transitive":
      return `${owner} is declared ${what}`;
    default:
      return `by ${owner}’s ${what.replace(/_/g, " ")} declaration`;
  }
}

const viaText = (v: Via) => {
  switch (v.kind) {
    case "stated":
      return "stated in the data";
    case "property":
      return "its stored values";
    case "type":
      return "its type";
    case "member":
      return "the same fact, through its relation";
    case "not":
      return "nothing states it";
    case "rule":
      return `by the rule “${v.name}”`;
    case "constraint":
      return `by the constraint “${v.name}”`;
    case "decl":
      return declText(v.owner, v.decl);
  }
};

/** What derives a fact, as a person reads it: `rule:x` is "x". */
export const ruleLabel = (r: string) =>
  r.replace(/^(?:rule|constraint|decl):/, "");

/**
 * A fact as a sentence: "A covers B", "invoiced: A", "breaks x: A", "A is a
 * Customer", "A: amount > 1000" (a value rule's test, as the rule writes
 * it). An open place of a `not` step reads "anything".
 */
function FactText({
  pred,
  args,
  view,
}: {
  pred: StepPred;
  args: (EntityRef | null)[];
  view: Named;
}) {
  const refs = args.map((r, i) =>
    r ? <RefLink key={i} r={r} view={view} /> : <em key={i}>anything</em>,
  );
  if (pred.kind === "test" || pred.kind === "compare")
    return (
      <>
        {join(refs)}: <strong className="ex-pred">{pred.name}</strong>
      </>
    );
  const name = <strong className="ex-pred">{predLabel(pred)}</strong>;
  if (pred.kind === "type")
    return (
      <>
        {refs[0]} is a {name}
      </>
    );
  if (pred.kind === "violation")
    return (
      <>
        breaks {name}: {join(refs)}
      </>
    );
  if (args.length === 2)
    return (
      <>
        {refs[0]} {name} {refs[1]}
      </>
    );
  return (
    <>
      {name}: {join(refs)}
    </>
  );
}

const join = (parts: ReactNode[]) =>
  parts.flatMap((p, i) => (i ? [", ", p] : [p]));

/**
 * A proof as nested lists, root first; a step reached twice shows once
 * and is named the second time.
 */
export function Proof({
  proof,
  view,
}: {
  proof: ExplainResult;
  view: RefView;
}) {
  const named = useNamed(
    view,
    proof.steps.flatMap((s) => s.args.filter((a) => a !== null)),
  );
  if (proof.root === null)
    return <p className="on-muted">It doesn’t hold in this load any more.</p>;
  const shown = new Set<number>();
  const step = (i: number): ReactNode => {
    const s = proof.steps[i]!;
    const again = shown.has(i);
    shown.add(i);
    // A value test that fails reads as the test, not as a missing fact.
    const test = s.pred.kind === "test" || s.pred.kind === "compare";
    return (
      <li key={i}>
        <span className="ex-proof-step">
          {s.via.kind === "not" && !test && "no "}
          <FactText pred={s.pred} args={s.args} view={named} />
          <small>
            {again
              ? "(shown above)"
              : test && s.via.kind === "not"
                ? "it doesn’t hold"
                : viaText(s.via)}
          </small>
        </span>
        {!again && s.premises.length > 0 && (
          <ol className="ex-proof">{s.premises.map(step)}</ol>
        )}
      </li>
    );
  };
  return (
    <ol className="ex-proof" aria-label="Why it holds">
      {step(proof.root)}
    </ol>
  );
}

/** "Why": reads the proof once opened. */
export function Why({
  pred,
  args,
  view,
}: {
  pred: Pred;
  args: EntityRef[];
  view: RefView;
}) {
  const [open, setOpen] = useState(false);
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const q = useQuery({
    ...graphReadQueries.explain(ws, o, view.loadId, pred, args),
    enabled: open,
  });
  return (
    <>
      <Button
        size="sm"
        variant="ghost"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {open ? "Hide why" : "Why"}
      </Button>
      {open && (
        <div className="ex-why">
          {q.isPending ? (
            <p className="on-muted">Working out why…</p>
          ) : q.error ? (
            <p role="alert" className="on-error-text">
              Couldn’t work out why: {q.error.message}{" "}
              <Button size="sm" onClick={() => void q.refetch()}>
                Retry
              </Button>
            </p>
          ) : !q.data.ok ? (
            <p className="on-muted">{readFailureText(q.data.error)}.</p>
          ) : (
            <Proof proof={q.data.value} view={view} />
          )}
        </div>
      )}
    </>
  );
}

/**
 * The derived facts of a read, grouped by pred and the place the entity
 * holds in them (`at`, its first when it holds two).
 */
export function derivedGroups(derived: DerivedFact[], self: EntityRef) {
  const groups = new Map<
    string,
    { pred: Pred; at: number; facts: DerivedFact[] }
  >();
  for (const f of derived) {
    const at = Math.max(
      0,
      f.args.findIndex((a) => sameRef(a, self)),
    );
    const k = `${f.pred.kind}:${f.pred.name}\u0000${at}`;
    const g = groups.get(k) || { pred: f.pred, at, facts: [] };
    g.facts.push(f);
    groups.set(k, g);
  }
  return [...groups.values()].sort(
    (a, b) => a.pred.name.localeCompare(b.pred.name) || a.at - b.at,
  );
}

const basisTag = (b: DerivedFact["basis"]) =>
  b === "both" ? (
    <Tag tone="neutral" icon={Sparkles}>
      Stated and derived
    </Tag>
  ) : (
    <Tag tone="info" icon={Sparkles}>
      Derived
    </Tag>
  );

/** The Derived tab: flags, derived facts with "why", and what couldn't be computed. */
export function DerivedFacts({
  self,
  result,
  view,
}: {
  // The entity the page reads: a flag's one argument.
  self: EntityRef;
  result: EntityResult;
  view: RefView;
}) {
  const { derived, flags, errors } = result;
  const named = useNamed(
    view,
    derived.flatMap((f) => f.args),
  );
  return (
    <div className="on-stack">
      {errors.length > 0 && (
        <ul className="ex-read-errors" aria-label="Not computed">
          {errors.map((e) => (
            <li
              key={
                e.part === "violations"
                  ? e.part
                  : `${e.part.kind}:${e.part.name}`
              }
            >
              <strong className="ex-pred">
                {e.part === "violations" ? "rule breaks" : predLabel(e.part)}
              </strong>
              : {readFailureText(e.failure).toLowerCase()}.
            </li>
          ))}
        </ul>
      )}
      {!derived.length && !flags.length ? (
        <EmptyState
          size="sm"
          icon={Sparkles}
          title="Nothing derived"
          text="The version’s rules derive no facts or flags for it in this load."
        />
      ) : (
        <>
          {flags.length > 0 && (
            <Panel title="Flags" subtitle="What the rules say holds of it.">
              <ul className="ex-facts" aria-label="Flags">
                {flags.map((f) => (
                  <li key={f.name}>
                    <div className="ex-fact-head">
                      <Tag tone="accent" icon={Sparkles}>
                        {predLabel(f)}
                      </Tag>
                      {f.rules.length > 0 && (
                        <span className="on-muted">
                          by {f.rules.map(ruleLabel).join(", ")}
                        </span>
                      )}
                      <Why
                        pred={{ kind: "derived", name: f.name }}
                        args={[self]}
                        view={view}
                      />
                    </div>
                  </li>
                ))}
              </ul>
            </Panel>
          )}
          {derivedGroups(derived, self).map((g) => (
            <section
              key={`${g.pred.kind}:${g.pred.name}\u0000${g.at}`}
              className="ex-group"
              aria-label={`${g.pred.name} ${g.at === 0 ? "to" : "from"}`}
            >
              <h2>
                {g.at === 0 ? (
                  <>
                    {g.pred.name} <ArrowRight size={14} aria-hidden />
                  </>
                ) : (
                  <>
                    <ArrowLeft size={14} aria-hidden /> {g.pred.name}
                  </>
                )}
                <small>{plural(g.facts.length, "fact")}</small>
              </h2>
              <ul className="ex-facts">
                {g.facts.map((f, i) => (
                  <li key={i}>
                    <div className="ex-fact-head">
                      {join(
                        f.args
                          .filter((_, at) => at !== g.at)
                          .map((r, at) => (
                            <RefLink key={at} r={r} view={named} />
                          )),
                      )}
                      {basisTag(f.basis)}
                      {f.rules.length > 0 && (
                        <span className="on-muted">
                          by {f.rules.map(ruleLabel).join(", ")}
                        </span>
                      )}
                      <Why pred={f.pred} args={f.args} view={view} />
                    </div>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </>
      )}
    </div>
  );
}
