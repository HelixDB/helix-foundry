import {
  Fragment,
  Suspense,
  createContext,
  lazy,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ComponentProps,
  type CSSProperties,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  CircleAlert,
  Copy,
  FileClock,
  FileText,
  FolderOpen,
  LoaderCircle,
  Table2,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import type { FoundryDocument } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  StatusBadge,
  TabPanel,
  Tabs,
  Tag,
  cx,
  formatDate,
  formatRelative,
  plural,
  useTabParam,
} from "../../kit";
import {
  resolvedByText,
  type DocFact,
  type DocumentGraph,
  type Span,
} from "../explore/model";
import { readQueries, startingOf } from "../explore/reads";
import { DatabaseStarting } from "../explore/starting";
import { useTenantReads } from "../explore/tenant-reads";
import { useNaming } from "../explore/use-naming";
import { typeHue } from "../ontology/colors";
import { Callout } from "../ontology/parts";
import {
  LoadNotice,
  LoadPicker,
  useLoadSelection,
} from "../ontology/load-selection";
import { POLL_MS, ontologyQueries } from "../ontology/queries";
import { SavedDecisions, SettleReview } from "../reviews/settle";
import { ontologyName, useOntology } from "../ontology/scope";
import { DocumentView, PageBreak, PdfReadCallout } from "./document-view";
import type { MarkRun } from "./marks";
import {
  documentQueries,
  documentSetQueries,
  useDocumentNames,
} from "./documents-api";
import {
  documentBadge,
  documentViewer,
  failureText,
  fileRead,
  formatBytes,
  formatUsd,
  reviewCopy,
  segments,
  spanAnchor,
  spanOf,
  spanTarget,
  utf16At,
  liveRead,
  markTarget,
  originalSections,
  readKindOf,
  sectionPages,
  type MarkTarget,
} from "./model";

// /ontology/:ontologyId/documents/:documentId: one document of the
// workspace as this ontology read it (read only: Data moves and deletes it).
// Its text is shown as a load stored it in the ontology's database, read
// from there with the user's session, rendered as the document is (Markdown
// as Markdown, through ./markdown.tsx), with every name it resolved
// underlined in its type's colour, the quotes behind its facts tinted, and
// what needs review marked. Until a load holds it, the document shows as
// uploaded, by its media type (./document-view.tsx). The side tabs list the
// facts, the entities and the reviews; choosing one scrolls the text to it.
// A click on a mark goes the other way: a name opens its entity, a quote its
// fact's entity (or the fact, in Facts), a review its card in Needs review.
// Links to a mark look like #s{section}-{byte start}; a link to any span of
// a section, #s{section}-{start}-{end} (an Ask citation's quote, or a
// property's quote from an entity's page), marks that span and opens on it.
// A load reads MDX with its code blanked to spaces, so an MDX document's
// sections are shown from the original at their ranges (originalSections):
// its JSX and expressions as written, its marks where they were. A load
// reads a PDF's text as Foundry read it, as plain text, so its sections are
// that text and marks fall on it as on any plain text; each page starts
// after a rule with its number (sectionPages, from the whole text).

const Markdown = lazy(() =>
  import("./markdown").then((m) => ({ default: m.Markdown })),
);

type Kind = "mention" | "quote" | "review" | "span";
type TextMark = {
  from: number;
  to: number;
  kind: Kind;
  anchor: string;
  entityId?: string;
  factKey?: string;
  // A review's index in the document's reviews.
  review?: string;
  hue?: string;
};
const tabs = ["facts", "entities", "review", "details"] as const;

/**
 * The section texts cut into runs, in UTF-16 ranges, each marked run with
 * its <mark>'s attributes; `target` is a span the page's link points at,
 * marked on its own. Each anchor goes on the first run of its first mark.
 */
function marked(x: DocumentGraph, types: string[], target?: Span) {
  const placed = new Set<string>();
  return x.sections.map((s) => {
    const text = s.text,
      ordinal = s.ordinal,
      at = utf16At(text);
    const mark = (
      kind: Kind,
      section: number | undefined,
      start: number | undefined,
      end: number | undefined,
      quote: string | undefined,
      extra: Partial<TextMark> = {},
    ): TextMark[] => {
      if ((section ?? 0) !== ordinal) return [];
      const span = spanOf(text, start, end, quote, at);
      return span
        ? [{ ...span, kind, anchor: spanAnchor(ordinal, start), ...extra }]
        : [];
    };
    const marks = [
      ...x.mentions.flatMap((m) =>
        mark("mention", m.section, m.start, m.end, m.surface, {
          entityId: m.entity.id,
          hue: typeHue(types, m.entity.type) || undefined,
        }),
      ),
      ...x.evidence.flatMap((e) =>
        mark("quote", e.section, e.start, e.end, e.quote, {
          factKey: e.factKey,
        }),
      ),
      ...x.reviews.flatMap((r, i) =>
        mark("review", r.section, r.start, r.end, r.quote, {
          review: String(i),
        }),
      ),
      ...(target
        ? mark("span", target.section, target.start, target.end, undefined, {
            anchor: `s${target.section}-${target.start}-${target.end}`,
          })
        : []),
    ];
    const runs = segments(text, marks).map(({ marks, ...run }) => {
      if (!marks.length) return run;
      const kinds = new Set(marks.map((m) => m.kind));
      // A span the link points at takes the anchor before another mark
      // starting where it does.
      const starts =
        marks.find(
          (m) =>
            m.kind === "span" && m.from === run.from && !placed.has(m.anchor),
        ) || marks.find((m) => m.from === run.from && !placed.has(m.anchor));
      if (starts) placed.add(starts.anchor);
      // Innermost first: a click goes to the most specific mark.
      const inner = marks.toSorted((a, b) => a.to - a.from - (b.to - b.from));
      return {
        ...run,
        mark: {
          className: cx(
            "dc-mark",
            kinds.has("mention") && "is-mention",
            kinds.has("quote") && "is-quote",
            kinds.has("review") && "is-review",
            kinds.has("span") && "is-span",
          ),
          id: starts?.anchor,
          anchors: marks.map((m) => m.anchor).join(" "),
          entities: inner.flatMap((m) => m.entityId ?? []).join(" "),
          facts: inner.flatMap((m) => m.factKey ?? []).join(" "),
          reviews: inner.flatMap((m) => m.review ?? []).join(" "),
          hue: inner.find((m) => m.kind === "mention")?.hue,
        },
      };
    });
    return { ordinal, text, runs };
  });
}

// What the page points at: the span a link or a side tab chose, and the
// entity hovered in the side tabs. Marks read it, so pointing lights them
// without rendering the text again.
const Pointed = createContext({ active: "", hovered: "" });

// Where a click on a mark goes: the facts (read after the text, so a quote
// finds its entity once they have loaded), the entities' names and pages,
// and what opens a fact or a review in the side tabs.
const Destinations = createContext<{
  facts?: DocFact[];
  names: Map<string, string>;
  entity: (id: string) => string;
  open: (target: Extract<MarkTarget, { kind: "fact" | "review" }>) => void;
}>({ names: new Map(), entity: () => "", open: () => {} });

/**
 * A marked run: lit when it holds the span or the entity pointed at, and a
 * link to its entity or a button to its fact or review (markTarget). One
 * inside a link of the document stays a mark.
 */
function Mark({
  node: _,
  className,
  "data-anchors": anchors = "",
  "data-entities": entities = "",
  "data-facts": facts = "",
  "data-reviews": reviews = "",
  "data-in-link": inLink,
  "data-hue": hue,
  ...props
}: ComponentProps<"mark"> & {
  node?: unknown;
  "data-anchors"?: string;
  "data-entities"?: string;
  "data-facts"?: string;
  "data-reviews"?: string;
  "data-in-link"?: boolean | string;
  "data-hue"?: string;
}) {
  const { active, hovered } = useContext(Pointed),
    to = useContext(Destinations);
  const words = (list: string) => list.split(" ").filter(Boolean);
  const target = markTarget(
    {
      entities: words(entities),
      facts: words(facts),
      reviews: words(reviews),
      inLink: !!inLink,
    },
    to.facts,
  );
  const mark = {
    ...props,
    className: cx(
      className,
      !!active && anchors.split(" ").includes(active) && "is-active",
      !!hovered && entities.split(" ").includes(hovered) && "is-hover",
      target.kind !== "none" && "is-clickable",
    ),
    style: hue ? ({ "--dc-hue": hue } as CSSProperties) : undefined,
  };
  if (target.kind === "none") return <mark {...mark} />;
  if (target.kind === "entity") {
    const name = to.names.get(target.id);
    return (
      <Link
        to={to.entity(target.id)}
        className="dc-mark-link"
        title={name ? `Open ${name}` : "Open the entity"}
      >
        <mark {...mark} />
      </Link>
    );
  }
  return (
    <mark
      {...mark}
      role="button"
      tabIndex={0}
      title={
        target.kind === "fact" ? "Show the fact" : "See why it needs review"
      }
      // A drag that selects text is no click.
      onClick={() => {
        if (window.getSelection()?.isCollapsed === false) return;
        to.open(target);
      }}
      onKeyDown={(e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        e.preventDefault();
        to.open(target);
      }}
    />
  );
}

/**
 * A run of a PDF's text with its page breaks (form feeds) drawn as rules,
 * the run starting on page `page`; any other run as it is.
 */
const withBreaks = (text: string, page: number | undefined) =>
  page === undefined
    ? text
    : text
        .split("\f")
        .flatMap((t, i) =>
          i ? [<PageBreak key={i} page={page + i} />, t] : [t],
        );

function DocumentText({
  x,
  types,
  markdown,
  mdx,
  pages,
  active,
  hovered,
  target,
  facts,
  entity,
  open,
}: {
  x: DocumentGraph;
  types: string[];
  // Rendered as Markdown, rather than as plain text; MDX as Markdown.
  markdown: boolean;
  mdx: boolean;
  // A PDF's: the pages each section is on.
  pages?: { first: number; last: number }[];
  active: string;
  hovered: string;
  target?: Span;
  // Undefined until they have loaded.
  facts?: DocFact[];
  // An entity's page.
  entity: (id: string) => string;
  // Opens a fact or a review in the side tabs.
  open: (target: Extract<MarkTarget, { kind: "fact" | "review" }>) => void;
}) {
  const sections = useMemo(
    () => marked(x, types, target),
    [x, types.join(), target?.section, target?.start, target?.end],
  );
  const names = useMemo(
    () => new Map(x.entities.map((e) => [e.id, e.name])),
    [x],
  );
  // Markdown is rendered once, as one source: the sections, which are whole
  // blocks, one after another.
  const rendered = useMemo(() => {
    if (!markdown) return null;
    const runs: MarkRun[] = [];
    let at = 0;
    for (const s of sections) {
      for (const r of s.runs)
        if ("mark" in r)
          runs.push({
            from: at + r.from,
            to: at + r.to,
            properties: {
              className: r.mark.className.split(" "),
              id: r.mark.id,
              dataAnchors: r.mark.anchors,
              dataEntities: r.mark.entities,
              dataFacts: r.mark.facts,
              dataReviews: r.mark.reviews,
              dataHue: r.mark.hue,
            },
          });
      at += s.text.length + 2;
    }
    return (
      <Markdown
        text={sections.map((s) => s.text).join("\n\n")}
        mdx={mdx}
        marks={runs}
        mark={Mark}
      />
    );
  }, [sections, markdown, mdx]);
  return (
    <Pointed.Provider value={{ active, hovered }}>
      <Destinations.Provider value={{ facts, names, entity, open }}>
        {rendered ? (
          <Suspense fallback={<SkeletonText lines={8} />}>{rendered}</Suspense>
        ) : (
          <div className="dc-doc-text" aria-label="Document text">
            {sections.map((s, i) => {
              const page = pages?.[i],
                // The page a run starts on: the section's, past the breaks before it.
                at = (from: number) =>
                  page &&
                  page.first + s.text.slice(0, from).split("\f").length - 1;
              return (
                <Fragment key={s.ordinal}>
                  {page && (!i || page.first > pages![i - 1]!.last) && (
                    <PageBreak page={page.first} />
                  )}
                  <p className="dc-doc-section">
                    {s.runs.map((r) =>
                      "mark" in r ? (
                        <Mark
                          key={r.from}
                          className={r.mark.className}
                          id={r.mark.id}
                          data-anchors={r.mark.anchors}
                          data-entities={r.mark.entities}
                          data-facts={r.mark.facts}
                          data-reviews={r.mark.reviews}
                          data-hue={r.mark.hue}
                        >
                          {withBreaks(r.text, at(r.from))}
                        </Mark>
                      ) : (
                        <span key={r.from}>
                          {withBreaks(r.text, at(r.from))}
                        </span>
                      ),
                    )}
                  </p>
                </Fragment>
              );
            })}
          </div>
        )}
      </Destinations.Provider>
    </Pointed.Provider>
  );
}

// The callout over a document: what it waits for, or why reading it failed.
function DocumentCallout({
  d,
  live,
  loaded,
  docName,
  setName,
}: {
  d: FoundryDocument;
  // The load whose job is reading documents now.
  live?: string;
  // Whether the ontology has loaded once.
  loaded: boolean;
  docName: (id: string) => string;
  // The name of its set.
  setName: string;
}) {
  const { id: o } = useOntology();
  const x = d.extraction;
  if (d.status === "deleted")
    return (
      <Callout
        icon={Trash2}
        tone="muted"
        title="This document was deleted"
        text="What a load read from it stays in the ontology’s database."
      />
    );
  if (d.status === "pending_upload")
    return (
      <Callout
        icon={CircleAlert}
        tone="muted"
        title="This upload didn’t finish"
        text="Upload the file again from Connectors."
      />
    );
  if (readKindOf(d) === "table")
    return (
      <Callout
        icon={Table2}
        tone="muted"
        title="Loads read its rows as a table"
        text="A table file is loaded through a version’s mappings, not read as text. Open it in Connectors to see which versions map it."
        action={
          <ButtonLink size="sm" to={paths.dataDocument(d.id)}>
            Open in Connectors
          </ButtonLink>
        }
      />
    );
  const read = fileRead(d);
  if (
    !x &&
    readKindOf(d) === "pdf" &&
    read.state !== "ready" &&
    read.state !== "none"
  )
    return <PdfReadCallout document={d} />;
  if (!x)
    return live ? (
      <Callout
        icon={LoaderCircle}
        live
        title="A load is reading it"
        text="Its text, names and facts show here once it is read."
        action={
          <ButtonLink size="sm" to={paths.load(o, live)}>
            View progress
          </ButtonLink>
        }
      />
    ) : (
      <Callout
        icon={FileClock}
        tone="muted"
        title={loaded ? "Waiting to be read" : "Waiting for a load"}
        text="A load reads it into the ontology’s database: its text, names and facts show here then."
        action={
          !loaded && (
            <ButtonLink size="sm" to={paths.ontology(o)}>
              Go to the overview
            </ButtonLink>
          )
        }
      />
    );
  if (x.status === "failed")
    return (
      <Callout
        icon={TriangleAlert}
        tone="danger"
        title="It couldn’t be read"
        text={failureText(x.error, d)}
      />
    );
  if (x.status === "out_of_scope")
    return (
      <Callout
        icon={FolderOpen}
        tone="muted"
        title="The current load didn’t read it"
        text={`It is stored, with its text, but the load’s version reads nothing from ${setName ? `the ${setName} set` : "its set"}. A version whose routes read that set reads it, or move it to a set this one reads; either applies from the next load.`}
      />
    );
  if (x.status === "duplicate")
    return (
      <Callout
        icon={Copy}
        tone="muted"
        title="It has the same text as another document"
        text={
          <>
            The database holds that text once, from{" "}
            {x.duplicateOf ? (
              <Link to={paths.document(o, x.duplicateOf)}>
                {docName(x.duplicateOf)}
              </Link>
            ) : (
              "the other document"
            )}
            .
          </>
        }
      />
    );
  return null;
}

export function DocumentPage({ documentId }: { documentId: string }) {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    location = useLocation();
  const loads = useQuery(ontologyQueries.loads(id, o));
  const running = liveRead(loads.data?.loads),
    live = running?.load.id,
    loaded = !!ontology.currentLoad;
  // The load the page reads: its extraction is the current load's, so
  // another load's is read from the database and found there or not.
  const selection = useLoadSelection(),
    { link } = selection,
    scope = selection.scope ?? { kind: "load", id: "" },
    current =
      selection.choice.kind === "load" && !!selection.choice.load.current;
  const { read: readDb } = useTenantReads();
  const doc = useQuery({
    ...documentQueries.document(id, o, documentId),
    refetchInterval: (q: {
      state: { data?: { document: FoundryDocument } };
    }) => (live && !q.state.data?.document.extraction ? POLL_MS : false),
  });
  // The name of the one this one duplicates, a connector's too.
  const docName = useDocumentNames(id, "the other document");
  const sets = useQuery(documentSetQueries.list(id));
  const d = doc.data?.document;
  const setName = sets.data?.find((s) => s.id === d?.setId)?.name || "";
  // Other data holds no documents (a document is only ever read into a
  // load), so it reads nothing.
  // Out of scope, a load stores its text and sections, nothing else.
  const read =
    selection.scope?.kind === "load" &&
    (current
      ? d?.extraction?.status === "extracted" ||
        d?.extraction?.status === "out_of_scope"
      : d?.status === "uploaded" || d?.status === "deleted");
  const { naming, ready } = useNaming(selection.versionId);
  const graphRead = readQueries.document(
    id,
    o,
    readDb,
    scope,
    documentId,
    naming,
  );
  const graph = useQuery({
    ...graphRead,
    enabled: read && !!ontology.tenantId && ready,
  });
  const x = read && graph.data?.stored ? graph.data : undefined;
  const factsRead = readQueries.documentFacts(
    id,
    o,
    readDb,
    scope,
    documentId,
    x,
    naming,
  );
  const facts = useQuery(factsRead);
  const viewer = documentViewer(d?.mediaType),
    mdx = viewer.kind === "markdown" && viewer.mdx,
    pdf = viewer.kind === "pdf";
  // An MDX document's text as uploaded, which its sections are shown from;
  // a PDF's whole text, which numbers its sections' pages.
  const original = useQuery({
    ...documentQueries.content(id, documentId),
    enabled: !!x && (mdx || pdf) && d?.status === "uploaded",
  });
  const shown = useMemo(
    () =>
      x && mdx && original.data
        ? {
            ...x,
            sections: originalSections(x.sections, original.data.text || ""),
          }
        : x,
    [x, mdx, original.data],
  );
  const pages = useMemo(
    () =>
      x && pdf ? sectionPages(x.sections, original.data?.text) : undefined,
    [x, pdf, original.data],
  );
  const [tab, setTab] = useTabParam(tabs);
  const [active, setActive] = useState(""),
    [hovered, setHovered] = useState("");
  // A link to a span opens on it; a link to any span marks it too.
  const hash = location.hash.slice(1);
  const target = spanTarget(hash);
  // Scrolls the text to a span and flashes it. Markdown is rendered once its
  // code has loaded, so the span is looked for over a few seconds.
  const show = (anchor: string) => {
    setActive("");
    const scroll = (tries: number) => {
      const mark = document.getElementById(anchor);
      if (mark) {
        setActive(anchor);
        mark.scrollIntoView({ block: "center", behavior: "smooth" });
      } else if (tries) setTimeout(() => scroll(tries - 1), 50);
    };
    requestAnimationFrame(() => scroll(60));
  };
  useEffect(() => {
    if (hash && x) show(hash);
  }, [hash, !!x]);
  const types = useMemo(
    () => [...new Set((x?.entities || []).map((e) => e.type))].sort(),
    [x],
  );
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Documents", to: paths.documents(o) },
  ];
  if (doc.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Document" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!d)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Document unavailable" />
        <EmptyState
          icon={FileText}
          title={
            doc.error instanceof ApiError && doc.error.status === 404
              ? "This document doesn’t exist in this workspace"
              : "Couldn’t load this document"
          }
          text={doc.error?.message}
          action={
            <ButtonLink to={paths.documents(o)}>All documents</ButtonLink>
          }
        />
      </Page>
    );
  const badge = documentBadge(d, running?.job);
  // The entities it names, by type, with their mentions.
  const entities = new Map<
    string,
    {
      id: string;
      type: string;
      name: string;
      surfaces: string[];
      count: number;
      anchor: string;
      // How its mentions found it, in words: by a short name, an identifier…
      by: Set<string>;
    }
  >();
  for (const m of x?.mentions || []) {
    const e = entities.get(m.entity.id) || {
      id: m.entity.id,
      type: m.entity.type,
      name: m.entity.name,
      surfaces: [],
      count: 0,
      anchor: spanAnchor(m.section, m.start),
      by: new Set<string>(),
    };
    if (m.surface && !e.surfaces.includes(m.surface))
      e.surfaces.push(m.surface);
    e.count += 1;
    const by = resolvedByText(m.resolvedBy);
    if (by) e.by.add(by);
    entities.set(m.entity.id, e);
  }
  const byType = [...new Set([...entities.values()].map((e) => e.type))].sort();
  const factList = facts.data || [],
    reviews = x?.reviews || [];
  const run = x?.sections.find((s) => s.model);
  return (
    <Page width="wide">
      <PageHeader
        breadcrumbs={[...crumbs, { label: d.name || d.id }]}
        icon={
          <span className="dc-table-icon">
            <FileText size={18} aria-hidden />
          </span>
        }
        eyebrow="Document"
        title={d.name || d.id}
        meta={
          <>
            <StatusBadge status={badge.status} label={badge.label} />
            {setName && <Tag icon={FolderOpen}>{setName}</Tag>}
            <span>{formatBytes(d.sizeBytes)}</span>
            {x && <span>{plural(x.sections.length, "section")}</span>}
            <span title={formatDate(d.createdAt)}>
              Uploaded {formatRelative(d.createdAt || "")}
            </span>
          </>
        }
        actions={
          <>
            <LoadPicker selection={selection} />
            <ButtonLink icon={FolderOpen} to={paths.dataDocument(d.id)}>
              Open in Connectors
            </ButtonLink>
          </>
        }
      />
      <div className="on-stack">
        <LoadNotice selection={selection} />
        {current || !selection.loads.length ? (
          <DocumentCallout
            d={d}
            live={live}
            loaded={loaded}
            docName={docName}
            setName={setName}
          />
        ) : selection.choice.kind === "other" ? (
          <Callout
            icon={FileClock}
            tone="muted"
            title="Documents are read into loads"
            text="Other data is what no load wrote, so it holds no documents. A load shows what was read from this one."
            action={
              <Button size="sm" onClick={() => selection.select("")}>
                Show the current load
              </Button>
            }
          />
        ) : (
          read &&
          graph.data &&
          !graph.data.stored && (
            <Callout
              icon={FileClock}
              tone="muted"
              title="This load didn’t read it"
              text="It was uploaded after the load, or reading it failed or was skipped there. The current load shows what was read from it."
              action={
                <Button size="sm" onClick={() => selection.select("")}>
                  Show the current load
                </Button>
              }
            />
          )
        )}
        <div className="dc-doc-grid">
          <Panel
            title={
              viewer.kind === "table"
                ? "Rows"
                : pdf && !shown && fileRead(d).state !== "ready"
                  ? "Pages"
                  : "Text"
            }
            subtitle={
              x
                ? "Names are underlined, quoted facts tinted, and what needs review marked."
                : current && read && graph.data && !graph.data.stored
                  ? "A load read it, but the ontology’s database doesn’t hold it now. Load the ontology again to mark what it found."
                  : undefined
            }
          >
            {shown && original.isLoading ? (
              <SkeletonText lines={8} />
            ) : shown ? (
              <DocumentText
                x={shown}
                types={types}
                markdown={viewer.kind === "markdown"}
                mdx={mdx}
                pages={pages}
                active={active}
                hovered={hovered}
                target={target}
                facts={facts.data}
                entity={(e) => link(paths.entity(o, e))}
                open={(t) => {
                  setTab(t.kind === "fact" ? "facts" : "review");
                  show(
                    t.kind === "fact"
                      ? `fact-${t.factKey}`
                      : `review-${t.review}`,
                  );
                }}
              />
            ) : read && !ontology.tenantId ? (
              <EmptyState
                size="sm"
                icon={FileText}
                title="You can’t read the ontology’s database"
                text="Its text and what was found in it live in a database of a project you can’t see. Ask a workspace admin for access to that project."
              />
            ) : read && startingOf(graph.error ?? graph.failureReason) ? (
              <DatabaseStarting queryKey={graphRead.queryKey} />
            ) : read && graph.isPending ? (
              <SkeletonText lines={8} />
            ) : read && graph.error ? (
              <p role="alert" className="on-error-text">
                Couldn’t read it from the database: {graph.error.message}{" "}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => void graph.refetch()}
                >
                  Retry
                </Button>
              </p>
            ) : d.status === "uploaded" ? (
              <DocumentView workspaceId={id} document={d} source={false} />
            ) : (
              <EmptyState
                size="sm"
                icon={FileText}
                title="Nothing to show"
                text={
                  d.status === "deleted"
                    ? "It was deleted, and the load you’re reading doesn’t hold its text."
                    : "Only an uploaded document shows what it holds."
                }
              />
            )}
          </Panel>
          <div className="dc-doc-side">
            <Tabs
              label="Document"
              items={[
                {
                  id: "facts",
                  label: "Facts",
                  count: x && new Set(x.evidence.map((e) => e.factKey)).size,
                },
                { id: "entities", label: "Entities", count: entities.size },
                { id: "review", label: "Needs review", count: reviews.length },
                { id: "details", label: "Details" },
              ]}
              value={tab}
              onChange={setTab}
            />
            <TabPanel id="facts" value={tab} idPrefix="tab">
              {!x?.evidence.length ? (
                <p className="on-muted">
                  {x
                    ? "No facts were read from it."
                    : "Facts show once it is read."}
                </p>
              ) : startingOf(facts.error ?? facts.failureReason) ? (
                <DatabaseStarting inline queryKey={factsRead.queryKey} />
              ) : facts.isPending ? (
                <SkeletonText lines={3} />
              ) : (
                <ul className="dc-doc-list" aria-label="Facts">
                  {facts.error && (
                    <li role="alert" className="on-error-text">
                      Couldn’t read the facts: {facts.error.message}
                    </li>
                  )}
                  {factList.map((f) => {
                    const q = f.quotes[0]!;
                    const item = `fact-${f.factKey}`;
                    return (
                      <li
                        key={f.factKey}
                        id={item}
                        className={active === item ? "is-active" : undefined}
                      >
                        <p className="dc-fact">
                          {f.from ? (
                            <Link to={link(paths.entity(o, f.from.id))}>
                              {f.from.name}
                            </Link>
                          ) : (
                            "?"
                          )}
                          <span className="dc-fact-link">
                            {f.link}
                            <ArrowRight size={12} aria-hidden />
                          </span>
                          {f.to ? (
                            <Link to={link(paths.entity(o, f.to.id))}>
                              {f.to.name}
                            </Link>
                          ) : (
                            "?"
                          )}
                        </p>
                        {f.quotes.map((e) => (
                          <button
                            key={spanAnchor(e.section, e.start)}
                            type="button"
                            className="dc-quote"
                            onClick={() => show(spanAnchor(e.section, e.start))}
                          >
                            “{e.quote}”
                          </button>
                        ))}
                        <small>
                          Section {q.section + 1}
                          {f.confidence !== undefined &&
                            ` · ${Math.round(f.confidence * 100)}% confident`}
                          {(f.evidenceCount || 0) > 1 &&
                            ` · ${f.evidenceCount} sources`}
                        </small>
                      </li>
                    );
                  })}
                </ul>
              )}
            </TabPanel>
            <TabPanel id="entities" value={tab} idPrefix="tab">
              {!entities.size ? (
                <p className="on-muted">
                  {x
                    ? "It names nothing the database holds."
                    : "Entities show once it is read."}
                </p>
              ) : (
                byType.map((type) => (
                  <section key={type} className="dc-doc-type" aria-label={type}>
                    <h3>
                      <span
                        className="dc-dot"
                        style={{
                          background: typeHue(types, type) || undefined,
                        }}
                        aria-hidden
                      />
                      {type}
                    </h3>
                    <ul className="dc-doc-list">
                      {[...entities.values()]
                        .filter((e) => e.type === type)
                        .map((e) => (
                          <li
                            key={e.id}
                            onMouseEnter={() => setHovered(e.id)}
                            onMouseLeave={() => setHovered("")}
                            onFocus={() => setHovered(e.id)}
                            onBlur={() => setHovered("")}
                          >
                            <p className="dc-fact">
                              <Link to={link(paths.entity(o, e.id))}>
                                {e.name}
                              </Link>
                              {[...e.by].map((by) => (
                                <Tag key={by}>{by}</Tag>
                              ))}
                            </p>
                            <small>
                              {plural(e.count, "mention")}
                              {e.surfaces.some((s) => s !== e.name) &&
                                ` · as “${e.surfaces.join("”, “")}”`}{" "}
                              ·{" "}
                              <button
                                type="button"
                                className="on-link"
                                onClick={() => show(e.anchor)}
                              >
                                Show in text
                              </button>
                            </small>
                            {(x?.mentions || [])
                              .filter(
                                (m) =>
                                  m.entity.id === e.id &&
                                  m.resolvedBy === "adjudicated",
                              )
                              .map((m) => (
                                <Link
                                  key={`${m.section}:${m.start}`}
                                  to={
                                    link(
                                      paths.document(o, documentId, "review"),
                                    ) +
                                    "#" +
                                    spanAnchor(m.section, m.start)
                                  }
                                >
                                  Review link
                                </Link>
                              ))}
                          </li>
                        ))}
                    </ul>
                  </section>
                ))
              )}
            </TabPanel>
            <TabPanel id="review" value={tab} idPrefix="tab">
              {!reviews.length ? (
                <p className="on-muted">
                  {x
                    ? "Nothing needs review."
                    : "Reviews show once it is read."}
                </p>
              ) : (
                <ul className="dc-doc-list" aria-label="Needs review">
                  {reviews.map((r, i) => {
                    const copy = reviewCopy(r.reason),
                      item = `review-${i}`;
                    return (
                      <li
                        key={item}
                        id={item}
                        className={cx(
                          "dc-review",
                          active === item && "is-active",
                        )}
                      >
                        <strong>
                          {copy.title}
                          {r.prop && `: ${r.prop}`}
                        </strong>
                        <p>{copy.text}</p>
                        {r.quote && (
                          <button
                            type="button"
                            className="dc-quote"
                            onClick={() => show(spanAnchor(r.section, r.start))}
                          >
                            “{r.quote}”
                          </button>
                        )}
                        <SettleReview
                          review={r}
                          link={link}
                          load={
                            selection.choice.kind === "load"
                              ? selection.choice.load
                              : undefined
                          }
                        />
                        <small>
                          Section {r.section + 1}
                          {r.suggestedLink &&
                            ` · suggested link: ${r.suggestedLink}`}
                          {r.confidence !== undefined &&
                            ` · ${Math.round(r.confidence * 100)}% confident`}
                        </small>
                      </li>
                    );
                  })}
                </ul>
              )}
              <SavedDecisions documentId={documentId} />
            </TabPanel>
            <TabPanel id="details" value={tab} idPrefix="tab">
              <KeyValue
                items={[
                  { label: "Media type", value: d.mediaType, mono: true },
                  { label: "Size", value: formatBytes(d.sizeBytes) },
                  {
                    label: "Uploaded",
                    value: d.createdAt && formatDate(d.createdAt),
                  },
                  {
                    label: "Read",
                    value:
                      d.extraction?.finishedAt &&
                      formatDate(d.extraction.finishedAt),
                  },
                  {
                    label: "Cost to read",
                    value: d.extraction && formatUsd(d.extraction.usdMicros),
                  },
                  { label: "Model", value: run?.model, mono: true },
                  { label: "Checked by", value: run?.verifier, mono: true },
                  {
                    label: "Prompt version",
                    value: run?.promptVersion,
                    mono: true,
                  },
                  { label: "Database", value: ontology.databaseName },
                  { label: "SHA-256", value: d.sha256, mono: true },
                  { label: "Document ID", value: d.id, mono: true },
                ]}
              />
            </TabPanel>
          </div>
        </div>
      </div>
    </Page>
  );
}
