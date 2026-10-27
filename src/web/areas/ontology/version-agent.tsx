import { Fragment, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  Check,
  CircleHelp,
  Copy,
  FileText,
  KeyRound,
  ListChecks,
  Sparkles,
  TriangleAlert,
  Unlink,
  UserSearch,
} from "lucide-react";
import type { AgentQuestion, FoundryDocumentSet } from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  Segmented,
  Tag,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { documentSetQueries } from "../documents/documents-api";
import { enrichmentQuery } from "../workspace/enrichment-api";
import {
  cluesText,
  fieldText,
  linkWords,
  mappingSource,
  sourceMatrix,
  splitSourceText,
  type AgentBundle,
  type AgentMapping,
  type AgentOntology,
  type DocumentSelector,
  type FieldRef,
  type Graph,
  type SourceCell,
} from "./model";
import { canManage } from "./parts";
import { Prose } from "./prose";
import { useOntology } from "./scope";
import type { Sources } from "./sources";
import {
  CardFields,
  EnrichmentNotes,
  EvidenceWithheld,
  SourceLine,
  TypeCards,
  useHashScroll,
} from "./version-parts";

// The tabs of an agent_bundle version: the agent's summary over the graph,
// then its types, links, mappings, sources, rules, questions and the raw
// TOML.

/** Above the graph: what the agent said, and what it is waiting for. */
export function AgentSummary({
  bundle,
  onQuestions,
}: {
  bundle: AgentBundle;
  onQuestions: () => void;
}) {
  // The opening paragraph; the rest (keys, dry runs, what was left out) on request.
  const [all, setAll] = useState(false);
  const blocks = bundle.summary.trim().split(/\n\s*\n/);
  return (
    <section className="on-agent-summary" aria-label="Agent summary">
      <p className="so-eyebrow">
        <Sparkles size={14} aria-hidden /> Drafted by the ontology agent
      </p>
      {bundle.summary ? (
        <>
          <Prose
            className="on-prose"
            text={all ? bundle.summary : blocks[0]!}
          />
          {blocks.length > 1 && (
            <button
              type="button"
              className="on-link on-more"
              aria-expanded={all}
              onClick={() => setAll(!all)}
            >
              {all ? "Show less" : "Read the agent’s full summary"}
            </button>
          )}
        </>
      ) : (
        <p className="on-muted">The agent left no summary.</p>
      )}
      {bundle.status === "needs_input" && bundle.questions.length > 0 && (
        <p className="on-agent-status">
          <CircleHelp size={14} aria-hidden /> The agent asked{" "}
          {plural(bundle.questions.length, "question")} about this draft.{" "}
          <button type="button" className="on-link" onClick={onQuestions}>
            See them
          </button>
        </p>
      )}
      {bundle.status === "budget_exhausted" && (
        <p className="on-agent-status">
          <TriangleAlert size={14} aria-hidden /> The agent stopped at its
          budget. This is the last complete draft it made.
        </p>
      )}
    </section>
  );
}

/** A file the agent wrote that does not parse, with the parser's reason. */
export function ParseProblem({
  file,
  error,
  raw = false,
}: {
  file: string;
  error: string;
  // Shown above the raw file itself, so it doesn't point to the TOML tab.
  raw?: boolean;
}) {
  return (
    <div className="on-card is-danger" role="alert">
      <span className="on-card-icon" aria-hidden>
        <TriangleAlert size={16} />
      </span>
      <div className="on-card-text">
        <strong>{file} can’t be read</strong>
        <p>
          {error.replace(/\.$/, "")}.
          {!raw && " The raw file is in the TOML tab."}
        </p>
      </div>
    </div>
  );
}

export function AgentTypes({
  bundle,
  graph,
  sources,
  versionId,
}: {
  bundle: AgentBundle;
  graph: Graph;
  sources: Sources;
  versionId: string;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  // While Helix has lookups off, Edit offers no Enrich people form to open.
  const paused =
    useQuery(enrichmentQuery(ws)).data?.availability ===
    "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED";
  if (!bundle.ontology.ok)
    return <ParseProblem file="ontology.toml" error={bundle.ontology.error} />;
  const { types, links } = bundle.ontology.value;
  if (!types.length)
    return <EmptyState title="No types" text="This draft defines no types." />;
  // The type People enrichment looks up, and the props it fills.
  const enrichment = bundle.enrichments.ok
    ? bundle.enrichments.value[0]
    : undefined;
  const filled = new Set(enrichment?.props.map(([p]) => p));
  return (
    <TypeCards
      sources={sources}
      items={types.map((t) => {
        const enriched = enrichment?.type === t.name ? enrichment : undefined;
        return {
          name: t.name,
          description: t.description,
          badges: (
            <>
              {t.key.length ? (
                <Tag tone="accent" icon={KeyRound}>
                  {t.key.join(", ")}
                </Tag>
              ) : (
                <Tag>Identified by name</Tag>
              )}
              {enriched && (
                <Tag tone="info" icon={UserSearch}>
                  Enriched · {cluesText(enriched)}
                </Tag>
              )}
              {enriched &&
                bundle.items[`enrichment.${enriched.name}`]?.status ===
                  "proposed" && (
                  <Tag icon={Sparkles}>Suggested by the agent</Tag>
                )}
            </>
          ),
          props: t.props.map((p) => {
            const id = t.identifiers.find((i) => i.prop === p.name);
            return {
              ...p,
              key: t.key.includes(p.name),
              note:
                [
                  ...(t.names.includes(p.name) ? ["Name"] : []),
                  ...(id ? [`Identifier · ${id.kind}`] : []),
                  ...(enriched && filled.has(p.name)
                    ? ["From People enrichment"]
                    : []),
                ].join(" · ") || undefined,
            };
          }),
          sources: graph.nodes.find((n) => n.name === t.name)?.sources || [],
          extra: (
            <>
              <LinksOf name={t.name} links={links} />
              {enriched && (
                <EnrichmentNotes bundle={bundle} versionId={versionId} />
              )}
              {enriched && canManage(role) && !paused && (
                <ButtonLink
                  size="sm"
                  icon={UserSearch}
                  to={paths.version(o, versionId, "edit", `type.${t.name}`)}
                >
                  Edit enrichment
                </ButtonLink>
              )}
            </>
          ),
          why: bundle.items[`type.${t.name}`]?.rationale,
        };
      })}
    />
  );
}

function LinksOf({
  name,
  links,
}: {
  name: string;
  links: AgentOntology["links"];
}) {
  const mine = links.filter((l) => l.from === name || l.to === name);
  if (!mine.length) return null;
  return (
    <ul className="on-type-links" aria-label={`${name} links`}>
      {mine.map((l) => (
        <li key={l.name}>
          <ArrowRight size={12} aria-hidden />
          <span>
            {l.from === name ? (
              <>
                {linkWords(l.name)} <strong>{l.to}</strong>
              </>
            ) : (
              <>
                <strong>{l.from}</strong> {linkWords(l.name)} it
              </>
            )}
          </span>
        </li>
      ))}
    </ul>
  );
}

export function AgentLinks({ bundle }: { bundle: AgentBundle }) {
  if (!bundle.ontology.ok)
    return <ParseProblem file="ontology.toml" error={bundle.ontology.error} />;
  const { links } = bundle.ontology.value;
  return (
    <DataGrid
      label="Links"
      rows={links.map((l) => ({
        ...l,
        endpoints: `${l.from} → ${l.to}`,
        why: bundle.items[`link.${l.name}`]?.rationale || "",
      }))}
      getRowKey={(r) => r.name}
      rowHeight={44}
      maxHeight={640}
      emptyText="This draft defines no links."
      columns={[
        {
          key: "name",
          label: "Link",
          width: 180,
          render: (r) => <span className="hf-mono">{r.name}</span>,
        },
        {
          key: "endpoints",
          label: "From → to",
          width: 220,
          render: (r) => (
            <span className="on-endpoints">
              {r.from} <ArrowRight size={12} aria-label="to" /> {r.to}
            </span>
          ),
        },
        {
          key: "cardinality",
          label: "Cardinality",
          width: 110,
          render: (r) => (r.cardinality === "one" ? "One" : "Many"),
        },
        {
          key: "required",
          label: "Required",
          width: 100,
          type: "boolean",
          render: (r) =>
            r.required ? <Tag tone="accent">Required</Tag> : "Optional",
        },
        {
          key: "description",
          label: "Description",
          width: 220,
          render: (r) => (
            <span className="on-cell-text" title={r.why || undefined}>
              {r.description}
              {r.traits.length > 0 && <small> · {r.traits.join(" · ")}</small>}
            </span>
          ),
        },
      ]}
    />
  );
}

function Field({ field }: { field: FieldRef }) {
  return (
    <code className="on-field" title={fieldText(field)}>
      {field.field}
      {field.transform.map((t) => (
        <span key={t} className="on-field-step">
          {t}
        </span>
      ))}
      {field.parse && <span className="on-field-step">{field.parse}</span>}
    </code>
  );
}
function Pairs({ pairs }: { pairs: [string, FieldRef][] }) {
  return (
    <span className="on-pairs">
      {pairs.map(([prop, field]) => (
        <span key={prop}>
          <span className="hf-mono">{prop}</span> ← <Field field={field} />
        </span>
      ))}
    </span>
  );
}

function MappingCard({
  m,
  mappings,
  why,
}: {
  m: AgentMapping;
  mappings: AgentMapping[];
  why?: string;
}) {
  return (
    <section className="on-item on-mapping" id={`mapping-${m.name}`}>
      <header>
        <h3 className="hf-mono">{m.name}</h3>
        <span className="on-muted">
          {m.rows.kind === "nested"
            ? `Each element of ${m.rows.records} in a row of ${m.rows.parent}`
            : m.rows.object
              ? `Every row of ${m.rows.object}`
              : "Every row"}
          {m.rowKey.length > 0 && <> · row key {m.rowKey.join(", ")}</>}
        </span>
      </header>
      <table className="on-binds">
        <thead>
          <tr>
            <th scope="col">Entity</th>
            <th scope="col">Found by</th>
            <th scope="col">Properties</th>
          </tr>
        </thead>
        <tbody>
          {m.bindings.map((b) => (
            <tr key={b.alias}>
              <th scope="row">
                <span className="hf-mono">{b.alias}</span>{" "}
                <strong>{b.type}</strong>
                <span className="on-bind-tags">
                  {b.missing === "create" ? (
                    <Tag tone="success">Creates</Tag>
                  ) : (
                    <Tag>Looks up</Tag>
                  )}
                  {b.optional && <Tag>Optional</Tag>}
                </span>
              </th>
              <td>
                {b.identify.kind === "key" ? (
                  <Pairs pairs={b.identify.fields} />
                ) : b.identify.kind === "lookup" ? (
                  <span className="on-pairs">
                    <span>
                      the row of{" "}
                      <a href={`#mapping-${b.identify.mapping}`}>
                        {b.identify.mapping}
                      </a>{" "}
                      with
                    </span>
                    <Pairs pairs={b.identify.on} />
                  </span>
                ) : (
                  <span className="on-pairs">
                    <span>
                      name ← <Field field={b.identify.field} />
                    </span>
                  </span>
                )}
              </td>
              <td>
                {b.props.length ? (
                  <Pairs pairs={b.props} />
                ) : (
                  <span className="on-muted">—</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {m.links.length > 0 && (
        <p className="on-mapping-links">
          {m.links.map((l, i) => (
            <Fragment key={i}>
              {i > 0 && " · "}
              <span className="hf-mono">{l.from}</span>{" "}
              <span className="on-path-link">{linkWords(l.link)} →</span>{" "}
              <span className="hf-mono">{l.to}</span>
            </Fragment>
          ))}
        </p>
      )}
      {why && <p className="on-why">{why}</p>}
      {mappingSource(mappings, m) === undefined && (
        <p className="on-muted">Its source can’t be found.</p>
      )}
    </section>
  );
}

/** Mappings grouped by the synced table their rows come from. */
export function AgentMappings({
  bundle,
  sources,
}: {
  bundle: AgentBundle;
  sources: Sources;
}) {
  useHashScroll();
  if (!bundle.mappings.ok)
    return <ParseProblem file="mappings.toml" error={bundle.mappings.error} />;
  const mappings = bundle.mappings.value;
  if (!mappings.length)
    return (
      <EmptyState title="No mappings" text="This draft maps no tables yet." />
    );
  const groups = new Map<string, AgentMapping[]>();
  for (const m of mappings) {
    const key = mappingSource(mappings, m) || "";
    groups.set(key, [...(groups.get(key) || []), m]);
  }
  return (
    <div className="on-groups">
      {[...groups].map(([sourceId, list]) => (
        <section
          key={sourceId}
          className="on-group"
          aria-labelledby={`on-source-${sourceId}`}
        >
          <h2 id={`on-source-${sourceId}`}>
            <SourceLine
              sources={sources}
              at={sourceId ? { kind: "agent", sourceId } : undefined}
            />
          </h2>
          {list.map((m) => (
            <MappingCard
              key={m.name}
              m={m}
              mappings={mappings}
              why={bundle.items[`mapping.${m.name}`]?.rationale}
            />
          ))}
        </section>
      ))}
    </div>
  );
}

/** Which sets a route reads, by name. */
function setsText(d: DocumentSelector, sets: FoundryDocumentSet[] = []) {
  if (d.kind === "all") return "All documents";
  if (!d.ids.length) return "No set";
  return d.ids
    .map((id) => sets.find((s) => s.id === id)?.name || "an unknown set")
    .join(", ");
}
const roleText: Record<SourceCell["role"], string> = {
  creates: "Creates",
  looks_up: "Looks up",
  resolves: "Finds",
};
function SourceCellView({ cell }: { cell: SourceCell | null }) {
  if (!cell) return <span className="on-muted">—</span>;
  return (
    <span className="on-source-cell">
      <Tag tone={cell.role === "creates" ? "success" : "neutral"}>
        {roleText[cell.role]}
      </Tag>
      {cell.props.length > 0 && (
        <small className="hf-mono">{cell.props.join(", ")}</small>
      )}
    </span>
  );
}

/**
 * Per type and link, what fills it: each source table's mappings, and each
 * document route with the sets it reads. Derived from the two files; a
 * type nothing creates is flagged.
 */
export function AgentSources({
  bundle,
  sources,
}: {
  bundle: AgentBundle;
  sources: Sources;
}) {
  const { id } = useWorkspace();
  const sets = useQuery(documentSetQueries.list(id));
  if (!bundle.ontology.ok)
    return <ParseProblem file="ontology.toml" error={bundle.ontology.error} />;
  if (!bundle.mappings.ok || !bundle.routes.ok)
    return (
      <ParseProblem
        file="mappings.toml"
        error={
          !bundle.mappings.ok
            ? bundle.mappings.error
            : !bundle.routes.ok
              ? bundle.routes.error
              : ""
        }
      />
    );
  const m = sourceMatrix(
    bundle.ontology.value,
    bundle.mappings.value,
    bundle.routes.value,
  );
  if (!m.columns.length)
    return (
      <EmptyState
        title="No sources"
        text="This draft maps no tables and reads no documents yet."
      />
    );
  const span = m.columns.length + 1;
  return (
    <div className="on-sources-wrap">
      <p className="on-muted">
        What fills each type and link: a table’s rows through its mappings, or
        documents through a route. A route reads the document sets it names;
        documents in other sets are stored, not read.
      </p>
      <table className="on-sources" aria-label="Sources">
        <thead>
          <tr>
            <th scope="col">Type or link</th>
            {m.columns.map((c) =>
              c.kind === "table" ? (
                <th scope="col" key={`t:${c.sourceId}`}>
                  <SourceLine
                    sources={sources}
                    at={
                      c.sourceId
                        ? { kind: "agent", sourceId: c.sourceId }
                        : undefined
                    }
                  />
                  <small className="hf-mono">{c.mappings.join(", ")}</small>
                </th>
              ) : (
                <th scope="col" key={`r:${c.route}`}>
                  <span className="on-source">
                    <FileText size={16} aria-hidden />
                    <span>
                      Documents <span className="on-source-sep">·</span>{" "}
                      {setsText(c.documents, sets.data)}
                    </span>
                  </span>
                  <small className="hf-mono">{c.route}</small>
                </th>
              ),
            )}
          </tr>
        </thead>
        <tbody>
          <tr className="on-sources-group">
            <th scope="colgroup" colSpan={span}>
              Types
            </th>
          </tr>
          {m.types.map((t) => (
            <tr key={`type:${t.name}`}>
              <th scope="row">
                <span className="on-source-cell">
                  <strong>{t.name}</strong>
                  {!t.created && <Tag tone="warning">Nothing creates it</Tag>}
                </span>
              </th>
              {t.cells.map((cell, i) => (
                <td key={i}>
                  <SourceCellView cell={cell} />
                </td>
              ))}
            </tr>
          ))}
          {m.links.length > 0 && (
            <tr className="on-sources-group">
              <th scope="colgroup" colSpan={span}>
                Links
              </th>
            </tr>
          )}
          {m.links.map((l) => (
            <tr key={`link:${l.name}`}>
              <th scope="row" className="hf-mono">
                {l.name}
              </th>
              {l.cells.map((on, i) => (
                <td key={i}>
                  {on ? (
                    <span className="on-source-cell">
                      <Check size={14} aria-hidden /> States it
                    </span>
                  ) : (
                    <span className="on-muted">—</span>
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const severityTone = {
  block: "danger",
  warn: "warning",
  info: "info",
} as const;
/** `evidenceWithheld`: the WFE sent the items without their evidence cards. */
export function AgentRules({
  bundle,
  evidenceWithheld,
}: {
  bundle: AgentBundle;
  evidenceWithheld?: boolean;
}) {
  if (!bundle.ontology.ok)
    return <ParseProblem file="ontology.toml" error={bundle.ontology.error} />;
  const { rules, constraints, relations } = bundle.ontology.value;
  if (!rules.length && !constraints.length && !relations.length)
    return (
      <EmptyState
        icon={ListChecks}
        title="No rules"
        text="This draft derives nothing and checks nothing yet."
      />
    );
  return (
    <div className="on-groups">
      {evidenceWithheld && <EvidenceWithheld />}
      {rules.length > 0 && (
        <section className="on-group">
          <h2>Rules</h2>
          <p className="on-muted">Facts derived from the data, in Datalog.</p>
          {rules.map((r) => (
            <section key={r.name} className="on-item">
              <header>
                <h3 className="hf-mono">{r.name}</h3>
                {bundle.items[`rule.${r.name}`]?.verdict && (
                  <Tag>{bundle.items[`rule.${r.name}`]!.verdict}</Tag>
                )}
              </header>
              {bundle.items[`rule.${r.name}`]?.card != null && (
                <section aria-label="Evidence card">
                  <CardFields card={bundle.items[`rule.${r.name}`]!.card} />
                </section>
              )}
              <p>{r.description}</p>
              <pre className="on-code">
                <code>{r.datalog}</code>
              </pre>
              {bundle.items[`rule.${r.name}`]?.rationale && (
                <p className="on-why">
                  {bundle.items[`rule.${r.name}`]!.rationale}
                </p>
              )}
            </section>
          ))}
        </section>
      )}
      {constraints.length > 0 && (
        <section className="on-group">
          <h2>Constraints</h2>
          <p className="on-muted">
            Checks on the data: a violation blocks, warns or informs.
          </p>
          {constraints.map((c) => (
            <section key={c.name} className="on-item">
              <header>
                <h3 className="hf-mono">{c.name}</h3>
                <Tag tone={severityTone[c.severity]}>{c.severity}</Tag>
                {bundle.items[`constraint.${c.name}`]?.verdict && (
                  <Tag>{bundle.items[`constraint.${c.name}`]!.verdict}</Tag>
                )}
              </header>
              {bundle.items[`constraint.${c.name}`]?.card != null && (
                <section aria-label="Evidence card">
                  <CardFields
                    card={bundle.items[`constraint.${c.name}`]!.card}
                  />
                </section>
              )}
              <p>{c.description}</p>
              <pre className="on-code">
                <code>{c.datalog}</code>
              </pre>
              {bundle.items[`constraint.${c.name}`]?.rationale && (
                <p className="on-why">
                  {bundle.items[`constraint.${c.name}`]!.rationale}
                </p>
              )}
            </section>
          ))}
        </section>
      )}
      {relations.length > 0 && (
        <section className="on-group">
          <h2>Relations</h2>
          <p className="on-muted">Derived only: a union of links.</p>
          {relations.map((r) => (
            <section key={r.name} className="on-item">
              <header>
                <h3 className="hf-mono">{r.name}</h3>
              </header>
              <p>{r.description}</p>
              {r.traits.length > 0 && (
                <p className="on-muted">{r.traits.join(" · ")}</p>
              )}
            </section>
          ))}
        </section>
      )}
    </div>
  );
}

// "type.Product" reads "Product (type)"; "mapping.products" "products (mapping)".
const itemText = (item: string) => {
  const [kind, ...rest] = item.split(".");
  return `${rest.join(".")} (${kind})`;
};
function Question({ q }: { q: AgentQuestion }) {
  return (
    <li className="on-question">
      <div className="on-question-head">
        <span className="on-question-id">{q.id}</span>
        {q.blocking && <Tag tone="warning">Blocks approval</Tag>}
      </div>
      <p>{q.text}</p>
      {(q.options?.length || 0) > 0 && (
        <ul className="on-options" aria-label="Options">
          {q.options!.map((o) => (
            <li key={o} className={o === q.default ? "is-default" : undefined}>
              {o}
              {o === q.default && <Tag tone="accent">Suggested</Tag>}
            </li>
          ))}
        </ul>
      )}
      {!q.options?.length && q.default && (
        <p className="on-muted">Until answered: {q.default}.</p>
      )}
      {(q.items?.length || 0) > 0 && (
        <p className="on-muted">About {q.items!.map(itemText).join(", ")}</p>
      )}
    </li>
  );
}

export function AgentQuestions({
  bundle,
  sources,
}: {
  bundle: AgentBundle;
  sources: Sources;
}) {
  const leftOut = bundle.leftOut.map((l) => {
    const { sourceId, rest } = splitSourceText(l.what);
    const info = sourceId ? sources.byAgentId(sourceId) : undefined;
    return {
      connector: info?.connector,
      what: info ? `${info.table}${rest ? `.${rest}` : ""}` : l.what,
      reason: l.reason,
    };
  });
  return (
    <div className="on-groups">
      <section className="on-group">
        <h2>Questions from the agent</h2>
        <p className="on-muted">
          To act on an answer, generate again with it in the goal.
        </p>
        {bundle.questions.length ? (
          <ol className="on-questions">
            {bundle.questions.map((q) => (
              <Question key={q.id} q={q} />
            ))}
          </ol>
        ) : (
          <p>The agent has no questions about this draft.</p>
        )}
      </section>
      {bundle.competency.length > 0 && (
        <section className="on-group">
          <h2>What it answers</h2>
          <p className="on-muted">
            The questions the draft was built to answer, and the path each takes
            through the types.
          </p>
          <ol className="on-cqs">
            {bundle.competency.map((c) => (
              <li key={c.id}>
                <span className="on-question-id">{c.id}</span>
                <span>
                  {c.question}
                  <small className="on-path">
                    {c.path.map((step, i) => (
                      <Fragment key={i}>
                        {i > 0 && " "}
                        {/^[<]?[a-z_]+[>]?$/.test(step) ? (
                          <span className="on-path-link">
                            {step.startsWith("<") ? "←" : ""}
                            {linkWords(step.replace(/[<>]/g, ""))}
                            {step.endsWith(">") ? "→" : ""}
                          </span>
                        ) : (
                          <strong>{step}</strong>
                        )}
                      </Fragment>
                    ))}
                  </small>
                </span>
              </li>
            ))}
          </ol>
        </section>
      )}
      {(bundle.gaps.length > 0 || leftOut.length > 0) && (
        <div className="so-notes">
          {bundle.gaps.length > 0 && (
            <section>
              <h2>Not modelled yet</h2>
              <ul>
                {bundle.gaps.map((g) => (
                  <li key={g.id}>
                    <TriangleAlert size={14} aria-hidden />
                    {g.description}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {leftOut.length > 0 && (
            <section>
              <h2>Left out</h2>
              <ul>
                {leftOut.map((l, i) => (
                  <li key={i}>
                    <Unlink size={14} aria-hidden />
                    <span>
                      {l.connector && <>{l.connector} · </>}
                      <span className="hf-mono">{l.what}</span>: {l.reason}
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          )}
        </div>
      )}
    </div>
  );
}

/** The two files as the agent wrote them. */
export function AgentToml({ bundle }: { bundle: AgentBundle }) {
  const { notify } = useWorkspace();
  const [file, setFile] = useState("ontology");
  const source =
      file === "mappings" ? bundle.mappingsToml : bundle.ontologyToml,
    parsed = file === "mappings" ? bundle.mappings : bundle.ontology,
    name = file === "mappings" ? "mappings.toml" : "ontology.toml";
  return (
    <div className="on-toml">
      <div className="on-graph-bar">
        <Segmented
          label="File"
          value={file}
          onChange={setFile}
          options={[
            { value: "ontology", label: "ontology.toml" },
            { value: "mappings", label: "mappings.toml" },
          ]}
        />
        <Button
          size="sm"
          icon={Copy}
          onClick={() =>
            navigator.clipboard
              .writeText(source)
              .then(() => notify(`Copied ${name}.`))
              .catch(() => notify(`Couldn’t copy ${name}.`, true))
          }
        >
          Copy
        </Button>
      </div>
      {!parsed.ok && <ParseProblem file={name} error={parsed.error} raw />}
      <pre className="on-code on-code--file" aria-label={name}>
        <code>{source || `# ${name} is empty`}</code>
      </pre>
    </div>
  );
}
