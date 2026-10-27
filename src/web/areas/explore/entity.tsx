import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowLeft,
  ArrowRight,
  FileText,
  MessageSquarePlus,
  Network,
  Sparkles,
  Table2,
  UserSearch,
  Waypoints,
} from "lucide-react";
import {
  Button,
  ButtonLink,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  SourceLogo,
  TabPanel,
  Tabs,
  Tag,
  formatDate,
  plural,
  useTabParam,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { reviewCopy, spanAnchor } from "../documents/model";
import { SavedDecisions, SettleReview } from "../reviews/settle";
import { AskDrawer } from "../ontology/ask";
import { ENRICHMENT_SOURCE } from "../ontology/enrichment-fields";
import { profilesDate } from "../ontology/model";
import { ontologyQueries } from "../ontology/queries";
import { OntologyPicker, useOntology } from "../ontology/scope";
import { useSources } from "../ontology/sources";
import { DerivedFacts, Why, type RefView } from "./derived";
import { refKey } from "./rule-breaks";
import {
  graphReadQueries,
  identityOf,
  liveViolationOf,
  readFailureText,
  refOf,
  rulesOf,
} from "./graph-read";
import {
  entityId,
  factGroups,
  mentionsByDocument,
  parseEntityId,
  propertyViews,
  provenanceText,
  resolvedByText,
  snippet,
  valueText,
  type Evidence,
  type EntityIdentity,
  type PropEvidence,
  type PropertyView,
  type SourceRow,
} from "./model";
import {
  LoadNotice,
  LoadPicker,
  NothingLoaded,
  useLoadSelection,
} from "../ontology/load-selection";
import { pausedOf, readQueries, startingOf } from "./reads";
import { DatabasePaused, DatabaseStarting } from "./starting";
import { useTenantReads } from "./tenant-reads";
import { useNaming } from "./use-naming";

// /explore/:ontologyId/entities/:entityId: one entity of the ontology's
// database, read from it with the user's session: its properties, each
// with the row or quote it comes from (a quote opens its span in the
// document; values documents disagree on show with their quotes), the
// tables and documents that state it, its facts with the rows and quotes
// behind them, where documents name it and how each mention found it, and
// what about it needs review. A Stripe customer named in a contract is one
// entity: the contract's mention and quote sit beside its Stripe row. It
// reads the page's load (`?load=`, the current one by default), and its
// links carry it. In a load whose version has rules, Derived shows what they
// derive about it, evaluated when the page reads it (./derived.tsx), and
// Needs review adds the rule breaks found then that the load keeps no record
// of. A value People enrichment found carries its chip, which names the
// profile and opens the load that looked it up.

const tabs = ["overview", "facts", "derived", "mentions", "reviews"] as const;

/**
 * A People enrichment profile as a chip, opening `loadId` (the load the page
 * reads, whose lookup found it); `title` says what it stated.
 */
function EnrichmentChip({ loadId, title }: { loadId: string; title: string }) {
  const { id: o } = useOntology();
  const content = (
    <>
      <UserSearch size={13} aria-hidden />
      People enrichment
    </>
  );
  return loadId ? (
    <Link
      className="an-chip"
      to={paths.load(o, loadId)}
      aria-label={title}
      title={title}
    >
      {content}
    </Link>
  ) : (
    <span className="an-chip" title={title}>
      {content}
    </span>
  );
}

/** A row that set a property: its table, linked to it, or a profile. */
function RowChip({
  prop,
  row,
  sources,
  loadId,
}: {
  prop: string;
  row: SourceRow;
  sources: ReturnType<typeof useSources>;
  loadId: string;
}) {
  if (row.source === ENRICHMENT_SOURCE)
    return (
      <EnrichmentChip
        loadId={loadId}
        title={`${prop} from People enrichment (context.dev), profile ${row.rowKey || "unknown"}`}
      />
    );
  const t = sources.byAgentId(row.source);
  const text = t ? `${t.connector} · ${t.table}` : "A table";
  const label = `${prop} from ${text}${row.rowKey ? ` row ${row.rowKey}` : ""}`;
  return t ? (
    <Link className="an-chip" to={t.to} aria-label={label} title={label}>
      <SourceLogo source={t.kind || "upload"} size={14} />
      {text}
    </Link>
  ) : (
    <span className="an-chip" title={label}>
      <Table2 size={13} aria-hidden />
      {text}
    </span>
  );
}

/** A quote that states a property: its document, opened on the span. */
function QuoteChip({
  prop,
  quote: q,
  docName,
  link,
}: {
  prop: string;
  quote: PropEvidence;
  docName: (id: string) => string;
  link: (path: string) => string;
}) {
  const { id: o } = useOntology();
  const label = `${prop} quoted in ${docName(q.documentId)}`;
  return (
    <Link
      className="an-chip"
      to={link(paths.document(o, q.documentId, undefined, q))}
      aria-label={label}
      title={q.quote}
    >
      <FileText size={13} aria-hidden />
      {docName(q.documentId)}
    </Link>
  );
}

/** Where a property's value comes from, and values documents disagree on. */
function Provenance({
  view: v,
  sources,
  docName,
  link,
  loadId,
}: {
  view: PropertyView;
  sources: ReturnType<typeof useSources>;
  docName: (id: string) => string;
  link: (path: string) => string;
  loadId: string;
}) {
  if (!v.rows.length && !v.quotes.length && !v.others.length) return null;
  return (
    <span className="ex-prop-sources">
      {(v.rows.length > 0 || v.quotes.length > 0) && (
        <span className="ex-prop-chips">
          {v.rows.map((r, i) => (
            <RowChip
              key={`r${i}`}
              prop={v.prop}
              row={r}
              sources={sources}
              loadId={loadId}
            />
          ))}
          {v.quotes.map((q, i) => (
            <QuoteChip
              key={`q${i}`}
              prop={v.prop}
              quote={q}
              docName={docName}
              link={link}
            />
          ))}
        </span>
      )}
      {v.others.length > 0 && (
        <span
          className="ex-prop-conflict"
          aria-label={`Values documents state for ${v.prop}`}
        >
          <strong>
            {v.value === undefined
              ? v.others.length > 1
                ? "Documents disagree, so it holds no value:"
                : "It holds no value; a document says:"
              : v.rows.length
                ? "A document says otherwise; the table’s value is kept:"
                : "Documents also say:"}
          </strong>
          <span className="ex-prop-others">
            {v.others.map((o, i) => (
              <span key={i} className="ex-prop-other">
                <span className="ex-prop-value">{valueText(o.value)}</span>
                {o.quotes.map((q, j) => (
                  <span key={j} className="ex-prop-quote">
                    <q className="ex-quote">{q.quote}</q>
                    <QuoteChip
                      prop={v.prop}
                      quote={q}
                      docName={docName}
                      link={link}
                    />
                  </span>
                ))}
              </span>
            ))}
          </span>
        </span>
      )}
    </span>
  );
}

export function EntityPage({ entityId }: { entityId: string }) {
  const identity = parseEntityId(entityId);
  const { id: o } = useOntology();
  const crumbs = [{ label: "Explorer", to: paths.explore(o) }];
  if (!identity)
    return (
      <Page>
        <PageHeader
          breadcrumbs={crumbs}
          title="Entity unavailable"
          actions={<OntologyPicker ontologyId={o} to={paths.explore} />}
        />
        <EmptyState
          icon={Waypoints}
          title="That isn’t an entity’s address"
          text="The link may be cut short. Find the entity in the Explorer."
          action={<ButtonLink to={paths.explore(o)}>Explorer</ButtonLink>}
        />
      </Page>
    );
  return <Entity identity={identity} crumbs={crumbs} />;
}

function Entity({
  identity,
  crumbs,
}: {
  identity: EntityIdentity;
  crumbs: { label: string; to: string }[];
}) {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const tenant = ontology.tenantId || "";
  const selection = useLoadSelection(),
    { link } = selection,
    scope = selection.scope ?? { kind: "load", id: "" };
  const { naming, ready } = useNaming(selection.versionId);
  const { read } = useTenantReads();
  const entityRead = readQueries.entity(id, o, read, scope, identity, naming);
  const query = useQuery({
    ...entityRead,
    enabled: !!tenant && ready && !!selection.scope,
  });
  const page = query.data;
  const evidenceRead = readQueries.evidence(
    id,
    o,
    read,
    scope,
    identity,
    page?.facts,
  );
  const evidence = useQuery({
    ...evidenceRead,
    enabled: !!selection.scope && !!page?.facts.some((f) => f.factKey),
  });
  // What the load's rules derive about it: only for a version with rules.
  const version = useQuery({
    ...ontologyQueries.version(id, o, selection.versionId),
    enabled: !!selection.versionId,
  });
  const rules = rulesOf(version.data?.version);
  const loadId = selection.scope?.kind === "load" ? selection.scope.id : "";
  const derived = useQuery({
    ...graphReadQueries.entity(id, o, loadId, identity),
    enabled: !!tenant && !!rules && !!loadId,
  });
  // Its documents may be a connector's, read by id.
  const docName = useDocumentNames(id, "A document"),
    sources = useSources();
  const [asked, setTab] = useTabParam(tabs);
  // Derived only shows for a load whose version has rules: else Overview.
  const tab =
    asked === "derived" && !version.isPending && !(rules && loadId)
      ? "overview"
      : asked;
  const [asking, setAsking] = useState(false);
  // An entity is the ontology's own: another ontology opens its Explorer.
  const pickers = (
    <>
      <OntologyPicker ontologyId={o} to={paths.explore} />
      <LoadPicker selection={selection} />
    </>
  );
  if (!tenant)
    return (
      <Page>
        <PageHeader
          breadcrumbs={crumbs}
          title="Entity"
          actions={<OntologyPicker ontologyId={o} to={paths.explore} />}
        />
        <EmptyState
          icon={Waypoints}
          title="You can’t read the ontology’s database"
          text="It lives in a project you can’t see. Ask a workspace admin for access to that project."
          action={<ButtonLink to={paths.ontology(o)}>Ontology</ButtonLink>}
        />
      </Page>
    );
  if (
    (selection.choice.kind === "missing" && !selection.pending) ||
    selection.failed
  )
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Entity" actions={pickers} />
        <LoadNotice selection={selection} />
      </Page>
    );
  if (selection.choice.kind === "none" && !selection.pending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Entity" actions={pickers} />
        <NothingLoaded purpose="explore its entities and facts" />
      </Page>
    );
  if (startingOf(query.error ?? query.failureReason))
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Entity" actions={pickers} />
        <DatabaseStarting queryKey={entityRead.queryKey} />
      </Page>
    );
  if (query.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Entity" actions={pickers} />
        <SkeletonText lines={6} />
      </Page>
    );
  const paused = pausedOf(query.error);
  if (paused)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Entity" actions={pickers} />
        <DatabasePaused failure={paused} />
      </Page>
    );
  const e = page?.entity;
  if (!page || !e)
    return (
      <Page>
        <PageHeader
          breadcrumbs={crumbs}
          title="Entity unavailable"
          actions={pickers}
        />
        <EmptyState
          icon={Waypoints}
          title={
            page
              ? "This entity isn’t in this load"
              : "Couldn’t read this entity"
          }
          text={
            page
              ? "The load doesn’t hold it: pick another load, or it was changed outside Foundry."
              : query.error?.message
          }
          action={
            <span className="on-card-actions">
              <ButtonLink to={link(paths.explore(o))}>Explorer</ButtonLink>
              {!page && (
                <Button onClick={() => void query.refetch()}>Retry</Button>
              )}
            </span>
          }
        />
      </Page>
    );
  const facts = page.facts,
    mentions = page.mentions,
    reviews = page.reviews;
  const props = propertyViews(e.props, page.rows, page.propEvidence);
  // The tables (by the rows that state it) and documents (by where they
  // name it) it comes from, each once.
  const tables = [
    ...new Map(
      page.rows.map((r) => [r.source, sources.byAgentId(r.source)]),
    ).entries(),
  ];
  const docs = [...new Set(mentions.map((m) => m.documentId))];
  // When the load's own lookups found its profiles: the evidence's snapshot
  // is the profiles file's fingerprint, not a date, and a profile reused
  // from an earlier load has none.
  const profilesAt =
    selection.choice.kind === "load" ? profilesDate(selection.choice.load) : "";
  const evidenceOf = (ev: Evidence, i: number) => {
    // A profile the load's lookup found: "People enrichment · profile
    // li:ada-lovelace · profiles as of Oct 20".
    if (ev.kind === "row" && ev.source === ENRICHMENT_SOURCE)
      return (
        <li key={i} className="ex-evidence">
          <UserSearch size={16} aria-hidden />
          <span>
            {loadId ? (
              <Link to={paths.load(o, loadId)}>People enrichment</Link>
            ) : (
              "People enrichment"
            )}{" "}
            · profile{" "}
            <code className="hf-mono">
              {(ev.rowKey || "unknown").split("/")[0]}
            </code>
            {profilesAt && <small> · profiles as of {profilesAt}</small>}
          </span>
        </li>
      );
    if (ev.kind === "row") {
      const t = sources.byAgentId(ev.source);
      const snapshot = Date.parse(ev.snapshot || "");
      return (
        <li key={i} className="ex-evidence">
          <SourceLogo source={t?.kind || "upload"} size={16} />
          <span>
            {t ? (
              <Link to={t.to}>
                {t.connector} · {t.table}
              </Link>
            ) : (
              "A table"
            )}{" "}
            row <code className="hf-mono">{ev.rowKey}</code>
            {ev.snapshot && (
              <small>
                {" "}
                · synced{" "}
                {Number.isNaN(snapshot) ? ev.snapshot : formatDate(ev.snapshot)}
              </small>
            )}
          </span>
        </li>
      );
    }
    return (
      <li key={i} className="ex-evidence">
        <FileText size={16} aria-hidden />
        <span>
          <q className="ex-quote">{ev.quote}</q>{" "}
          <Link
            to={link(
              paths.document(o, ev.documentId, "facts") +
                "#" +
                spanAnchor(ev.section, ev.start),
            )}
          >
            Open in {docName(ev.documentId)}
          </Link>
        </span>
      </li>
    );
  };
  const name = e.name || ("key" in identity ? identity.key : identity.name);
  // Entities of the rules' answers, named as this page names them.
  const known = new Map(
    [
      { identity, name },
      ...facts.flatMap((f) => (f.other ? [f.other] : [])),
    ].map((x) => [refKey(refOf(x.identity)), x.name]),
  );
  const view: RefView = {
    loadId,
    naming,
    name: (r) => known.get(refKey(r)),
    to: (r) => {
      const i = identityOf(r, naming);
      return i && link(paths.entity(o, entityId(i)));
    },
  };
  const rulesRead = derived.data?.ok ? derived.data.value : null;
  const live = rulesRead?.violations || [];
  // Rule breaks found now that the load keeps no record of.
  const unrecorded = live.filter((v) => !v.stored);
  // Why the rules couldn't say which rules it breaks now, if they couldn't.
  const liveFailed = rulesRead?.errors.find((e) => e.part === "violations");
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: name }]}
        eyebrow={e.type}
        title={name}
        meta={
          <>
            <span>
              {"key" in identity ? `Key ${identity.key}` : "Known by its name"}
            </span>
            {e.madeBy === "documents" && <span>Created by documents</span>}
            {tables.map(([source, t]) =>
              source === ENRICHMENT_SOURCE ? (
                <EnrichmentChip
                  key={source}
                  loadId={loadId}
                  title="Profile from People enrichment (context.dev)"
                />
              ) : t ? (
                <Link key={source} className="an-chip" to={t.to}>
                  <SourceLogo source={t.kind || "upload"} size={14} />
                  {t.connector} · {t.table}
                </Link>
              ) : (
                <span key={source} className="an-chip">
                  <Table2 size={13} aria-hidden />A table
                </span>
              ),
            )}
            {docs.map((d) => (
              <Link key={d} className="an-chip" to={link(paths.document(o, d))}>
                <FileText size={13} aria-hidden />
                {docName(d)}
              </Link>
            ))}
          </>
        }
        actions={
          <>
            {pickers}
            {selection.choice.kind === "load" && (
              <Button icon={MessageSquarePlus} onClick={() => setAsking(true)}>
                Ask about this
              </Button>
            )}
            <ButtonLink icon={Network} to={link(paths.explore(o))}>
              Explorer
            </ButtonLink>
          </>
        }
        tabs={
          <Tabs
            label="Entity"
            value={tab}
            onChange={setTab}
            items={[
              { id: "overview", label: "Overview" },
              { id: "facts", label: "Facts", count: facts.length },
              ...(rules && loadId
                ? [
                    {
                      id: "derived" as const,
                      label: "Derived",
                      count: rulesRead
                        ? rulesRead.derived.length + rulesRead.flags.length
                        : undefined,
                    },
                  ]
                : []),
              { id: "mentions", label: "Mentions", count: mentions.length },
              {
                id: "reviews",
                label: "Needs review",
                count:
                  reviews.length + page.violations.length + unrecorded.length,
              },
            ]}
          />
        }
      />
      <LoadNotice selection={selection} />
      <TabPanel id="overview" value={tab} idPrefix="tab">
        <Panel
          title="Properties"
          subtitle={
            props.some((p) => p.quotes.length || p.others.length)
              ? "Each with the table row or the document quote it comes from."
              : undefined
          }
        >
          {page.truncated.about && (
            <p className="on-muted">
              More rows and quotes state it than this page reads: the first 200
              show here.
            </p>
          )}
          {props.length ? (
            <KeyValue
              className="ex-props"
              items={props.map((v) => ({
                label: v.prop,
                value:
                  v.value === undefined ? (
                    <span className="on-muted">No value</span>
                  ) : (
                    valueText(v.value)
                  ),
                hint: (
                  <Provenance
                    view={v}
                    sources={sources}
                    docName={docName}
                    link={link}
                    loadId={loadId}
                  />
                ),
              }))}
            />
          ) : (
            <p className="on-muted">
              {"name" in identity
                ? "A type known by its name has no other properties."
                : "No properties."}
            </p>
          )}
        </Panel>
      </TabPanel>
      <TabPanel id="facts" value={tab} idPrefix="tab">
        {!facts.length ? (
          <EmptyState
            size="sm"
            icon={Waypoints}
            title="No facts"
            text="Nothing in your tables or documents links it to another entity."
          />
        ) : (
          <div className="on-stack">
            {page.truncated.facts && (
              <p className="on-muted">
                It has more facts than this page reads: the first 200 each way
                show here. The Explorer shows the rest.
              </p>
            )}
            {startingOf(evidence.error ?? evidence.failureReason) ? (
              <DatabaseStarting inline queryKey={evidenceRead.queryKey} />
            ) : (
              evidence.error && (
                <p role="alert" className="on-error-text">
                  Couldn’t read what states these facts:{" "}
                  {evidence.error.message}
                </p>
              )
            )}
            {factGroups(facts).map((g) => (
              <section
                key={g.link + g.direction}
                className="ex-group"
                aria-label={`${g.link} ${g.direction === "out" ? "to" : "from"}`}
              >
                <h2>
                  {g.direction === "out" ? (
                    <>
                      {g.link} <ArrowRight size={14} aria-hidden />
                    </>
                  ) : (
                    <>
                      <ArrowLeft size={14} aria-hidden /> {g.link}
                    </>
                  )}
                  <small>{plural(g.facts.length, "fact")}</small>
                </h2>
                <ul className="ex-facts">
                  {g.facts.map((f, i) => {
                    // Stated by a profile People enrichment found: the
                    // role it describes, and its chip.
                    const enriched = (
                      evidence.data?.get(f.factKey || "") || []
                    ).some(
                      (ev: Evidence) =>
                        ev.kind === "row" && ev.source === ENRICHMENT_SOURCE,
                    );
                    const role = Object.entries(f.props)
                      .flatMap(([k, v]) =>
                        typeof v === "string" && v
                          ? [v]
                          : v === true
                            ? [k.replace(/^is_/, "").replace(/_/g, " ")]
                            : [],
                      )
                      .join(" · ");
                    return (
                      <li key={f.factKey || i}>
                        <div className="ex-fact-head">
                          {f.other ? (
                            <Link to={link(paths.entity(o, f.other.id))}>
                              {f.other.name}
                            </Link>
                          ) : (
                            "?"
                          )}
                          {f.other && (
                            <span className="on-muted">{f.other.type}</span>
                          )}
                          {enriched && role && <span>{role}</span>}
                          {enriched ? (
                            <EnrichmentChip
                              loadId={loadId}
                              title={`${g.link} from People enrichment (context.dev)`}
                            />
                          ) : (
                            <Tag
                              tone={
                                f.provenance === "extracted"
                                  ? "info"
                                  : "neutral"
                              }
                              icon={
                                f.provenance === "extracted" ? FileText : Table2
                              }
                            >
                              {provenanceText(f.provenance)}
                            </Tag>
                          )}
                          {f.confidence !== undefined && f.confidence < 1 && (
                            <span className="on-muted">
                              {Math.round(f.confidence * 100)}% confident
                            </span>
                          )}
                          {(f.evidenceCount || 0) > 1 && (
                            <span className="on-muted">
                              {plural(f.evidenceCount!, "source")}
                            </span>
                          )}
                        </div>
                        <ul className="ex-evidence-list" aria-label="Evidence">
                          {evidence.isPending && f.factKey ? (
                            <li className="on-muted">Reading the evidence…</li>
                          ) : (
                            (evidence.data?.get(f.factKey || "") || []).map(
                              evidenceOf,
                            )
                          )}
                        </ul>
                      </li>
                    );
                  })}
                </ul>
              </section>
            ))}
          </div>
        )}
      </TabPanel>
      {rules && loadId && (
        <TabPanel id="derived" value={tab} idPrefix="tab">
          {derived.isPending ? (
            <SkeletonText lines={4} />
          ) : derived.error ? (
            <EmptyState
              size="sm"
              icon={Sparkles}
              title="Couldn’t work out what the rules derive"
              text={derived.error.message}
              action={
                <Button onClick={() => void derived.refetch()}>Retry</Button>
              }
            />
          ) : !derived.data.ok ? (
            <EmptyState
              size="sm"
              icon={Sparkles}
              title={readFailureText(derived.data.error)}
              text={derived.data.error.message}
            />
          ) : (
            <DerivedFacts
              self={refOf(identity)}
              result={derived.data.value}
              view={view}
            />
          )}
        </TabPanel>
      )}
      <TabPanel id="mentions" value={tab} idPrefix="tab">
        {!mentions.length ? (
          <EmptyState
            size="sm"
            icon={FileText}
            title="No document names it"
            text="Documents a load reads that name it show here."
          />
        ) : (
          <div className="on-stack">
            {mentionsByDocument(mentions).map((d) => (
              <Panel
                key={d.documentId}
                title={
                  <Link to={link(paths.document(o, d.documentId))}>
                    {docName(d.documentId)}
                  </Link>
                }
                subtitle={plural(d.mentions.length, "mention")}
              >
                <ul className="ex-evidence-list">
                  {d.mentions.map((m, i) => {
                    const s = snippet(m.text, m.start, m.end);
                    return (
                      <li key={i} className="ex-evidence">
                        <FileText size={16} aria-hidden />
                        <span>
                          <Link
                            to={link(
                              paths.document(o, d.documentId, "entities") +
                                "#" +
                                spanAnchor(m.section, m.start),
                            )}
                          >
                            {s.before}
                            <strong>{s.match}</strong>
                            {s.after}
                          </Link>{" "}
                          <small>
                            section {m.section + 1}
                            {resolvedByText(m.resolvedBy) &&
                              ` · ${resolvedByText(m.resolvedBy)!.toLowerCase()}`}
                          </small>
                          {m.resolvedBy === "adjudicated" && (
                            <Link
                              to={
                                link(
                                  paths.document(o, m.documentId, "review"),
                                ) +
                                "#" +
                                spanAnchor(m.section, m.start)
                              }
                            >
                              Review link
                            </Link>
                          )}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              </Panel>
            ))}
          </div>
        )}
      </TabPanel>
      <TabPanel id="reviews" value={tab} idPrefix="tab">
        {liveFailed && (
          <p className="on-muted">
            The rules it breaks now couldn’t be worked out:{" "}
            {readFailureText(liveFailed.failure).toLowerCase()}. The load’s
            records are below.
          </p>
        )}
        {!reviews.length && !page.violations.length && !unrecorded.length ? (
          <EmptyState size="sm" icon={Waypoints} title="Nothing needs review" />
        ) : (
          <Panel>
            <ul className="dc-doc-list" aria-label="Needs review">
              {page.violations.map((v, i) => {
                const args = liveViolationOf(live, v)?.entities;
                return (
                  <li key={`v${i}`} className="dc-review">
                    <strong>Breaks a rule: {v.constraint}</strong>
                    <p>
                      {v.severity === "block"
                        ? "A rule the ontology enforces doesn’t hold for it."
                        : "A rule the ontology checks doesn’t hold for it."}
                    </p>
                    {args && (
                      <div className="ex-fact-head">
                        <Why
                          pred={{ kind: "violation", name: v.constraint }}
                          args={args}
                          view={view}
                        />
                      </div>
                    )}
                  </li>
                );
              })}
              {unrecorded.map((v, i) => (
                <li key={`l${i}`} className="dc-review">
                  <strong>Breaks a rule: {v.constraint}</strong>
                  <p>
                    {v.constraint.endsWith(".self_loop")
                      ? "A rule would link it to itself. Rules never do, so the load keeps no record of this and doesn’t check it."
                      : "Found when this page was read; the load keeps no record of it yet."}
                  </p>
                  {!v.constraint.endsWith(".self_loop") && (
                    <div className="ex-fact-head">
                      <Why
                        pred={{ kind: "violation", name: v.constraint }}
                        args={v.entities}
                        view={view}
                      />
                    </div>
                  )}
                </li>
              ))}
              {reviews.map((rv, i) => {
                const copy = reviewCopy(rv.reason);
                return (
                  <li key={i} className="dc-review">
                    <strong>
                      {copy.title}
                      {rv.prop && `: ${rv.prop}`}
                    </strong>
                    <p>{copy.text}</p>
                    {rv.quote && <q className="ex-quote">{rv.quote}</q>}
                    <small>
                      <Link
                        to={link(
                          paths.document(o, rv.documentId, "review") +
                            "#" +
                            spanAnchor(rv.section, rv.start),
                        )}
                      >
                        Open in {docName(rv.documentId)}
                      </Link>
                      {rv.suggestedLink &&
                        ` · suggested link: ${rv.suggestedLink}`}
                    </small>
                    <SettleReview
                      review={rv}
                      link={link}
                      load={
                        selection.choice.kind === "load"
                          ? selection.choice.load
                          : undefined
                      }
                    />
                  </li>
                );
              })}
            </ul>
          </Panel>
        )}
        <SavedDecisions
          entity={{
            type: identity.type,
            key: "key" in identity ? identity.key : identity.name,
          }}
        />
      </TabPanel>
      {asking && selection.choice.kind === "load" && (
        <AskDrawer
          focus={{
            type: identity.type,
            id: "key" in identity ? identity.key : identity.name,
          }}
          label={name}
          loadId={selection.choice.load.id}
          onClose={() => setAsking(false)}
        />
      )}
    </Page>
  );
}
