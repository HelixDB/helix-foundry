import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowLeft,
  ArrowRight,
  ExternalLink,
  FileText,
  Network,
  Table2,
  UserSearch,
} from "lucide-react";
import {
  Button,
  ButtonLink,
  Dialog,
  KeyValue,
  SkeletonText,
  SourceLogo,
  Tag,
  formatDate,
  formatNumber,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { useDocumentNames } from "../documents/documents-api";
import { spanAnchor } from "../documents/model";
import {
  factGroups,
  mentionsByDocument,
  parseEntityId,
  provenanceText,
  snippet,
  valueText,
  type EntityRow,
  type Evidence,
} from "../explore/model";
import type { Scope } from "../explore/load-scope";
import type { Naming } from "../explore/naming";
import { readQueries, startingOf } from "../explore/reads";
import { DatabaseStarting } from "../explore/starting";
import { useTenantReads } from "../explore/tenant-reads";
import { ENRICHMENT_SOURCE } from "../ontology/enrichment-fields";
import { useOntology } from "../ontology/scope";
import { useSources } from "../ontology/sources";
import { detailProps, fieldName } from "./model";

// One record of the ontology's database, in a drawer over Records: its
// properties, its links out and in by link name with the other ends'
// readable names, and where it comes from (the table rows that state it,
// the documents that name it, and the rows and quotes behind each link).
// It reads the entity page's reads (../explore/reads.ts), so the two agree.

// The quotes shown per document; its page lists the rest.
const QUOTES = 3;

export function RecordDetail({
  entityId,
  naming,
  scope,
  link,
  onClose,
  onOpen,
}: {
  entityId: string;
  naming: Naming;
  // The load Records reads, and how its links carry it.
  scope: Scope;
  link: (path: string) => string;
  onClose: () => void;
  // Opens another record in the drawer.
  onOpen: (other: EntityRow) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate();
  const identity = parseEntityId(entityId);
  const tenant = ontology.tenantId || "";
  const { read } = useTenantReads();
  const entityRead = readQueries.entity(
    ws,
    o,
    read,
    scope,
    identity || { type: "", key: "" },
    naming,
  );
  const query = useQuery({
    ...entityRead,
    enabled: !!tenant && !!identity,
  });
  const page = query.data;
  const evidenceRead = readQueries.evidence(
    ws,
    o,
    read,
    scope,
    identity || { type: "", key: "" },
    page?.facts,
  );
  const evidence = useQuery({
    ...evidenceRead,
    enabled: !!identity && !!page?.facts.some((f) => f.factKey),
  });
  // Its documents may be a connector's, read by id.
  const docName = useDocumentNames(ws, "A document"),
    sources = useSources();
  const e = page?.entity;

  const title = e?.name || "Record";
  if (startingOf(query.error ?? query.failureReason))
    return (
      <Dialog
        title="Record"
        variant="drawer"
        onClose={onClose}
        className="rx-detail"
      >
        <DatabaseStarting queryKey={entityRead.queryKey} />
      </Dialog>
    );
  if (!identity || (page && !e) || query.error)
    return (
      <Dialog
        title="Record unavailable"
        variant="drawer"
        onClose={onClose}
        className="rx-detail"
      >
        <p className="on-muted">
          {!identity
            ? "That isn’t a record’s address: the link may be cut short."
            : query.error
              ? `Couldn’t read it: ${query.error.message}`
              : "It isn’t in the ontology’s database: nothing loaded holds it, or the database changed outside Foundry."}
        </p>
        {query.error && (
          <Button onClick={() => void query.refetch()}>Retry</Button>
        )}
      </Dialog>
    );

  const t = e ? naming.types.get(e.type) : undefined;
  const key = identity && "key" in identity ? identity.key : undefined;
  const evidenceItem = (ev: Evidence, i: number) => {
    if (ev.kind === "row" && ev.source === ENRICHMENT_SOURCE)
      return (
        <li key={i} className="ex-evidence">
          <UserSearch size={16} aria-hidden />
          <span>
            People enrichment · profile{" "}
            <code className="hf-mono">
              {(ev.rowKey || "unknown").split("/")[0]}
            </code>
          </span>
        </li>
      );
    if (ev.kind === "row") {
      const s = sources.byAgentId(ev.source);
      return (
        <li key={i} className="ex-evidence">
          <SourceLogo source={s?.kind || "upload"} size={16} />
          <span>
            {s ? (
              <Link to={s.to}>
                {s.connector} · {s.table}
              </Link>
            ) : (
              "A table"
            )}{" "}
            row <code className="hf-mono">{ev.rowKey}</code>
          </span>
        </li>
      );
    }
    return (
      <li key={i} className="ex-evidence">
        <FileText size={16} aria-hidden />
        <span>
          <q className="ex-quote">{ev.quote}</q>{" "}
          <small>
            section {ev.section + 1} ·{" "}
            <Link
              to={link(
                paths.document(o, ev.documentId, "facts") +
                  "#" +
                  spanAnchor(ev.section, ev.start),
              )}
            >
              Open in {docName(ev.documentId)}
            </Link>
          </small>
        </span>
      </li>
    );
  };

  return (
    <Dialog
      title={title}
      description={
        e ? (
          <>
            {e.type}
            {key && key !== e.name ? (
              <>
                {" · "}
                <code className="hf-mono">{key}</code>
              </>
            ) : null}
          </>
        ) : undefined
      }
      variant="drawer"
      onClose={onClose}
      className="rx-detail"
    >
      {!e || !page ? (
        <SkeletonText lines={8} />
      ) : (
        <div className="rx-detail-body">
          <div className="rx-detail-actions">
            <Button
              icon={Network}
              onClick={() => navigate(link(paths.explore(o, e.dbId)))}
            >
              Open in graph
            </Button>
            <ButtonLink
              variant="ghost"
              icon={ExternalLink}
              to={link(paths.entity(o, e.id))}
            >
              Open its page
            </ButtonLink>
          </div>

          <section aria-label="Properties">
            <h3>Properties</h3>
            <KeyValue
              items={detailProps(naming, t, e.type, e.props).map(
                ([label, value]) => ({
                  label: fieldName(label),
                  value: valueText(value),
                }),
              )}
            />
          </section>

          <section aria-label="Links">
            <h3>Links</h3>
            {!page.facts.length ? (
              <p className="on-muted">
                Nothing in your tables or documents links it to another record.
              </p>
            ) : (
              <>
                {page.truncated.facts && (
                  <p className="on-muted">
                    It has more links than this reads: the first 200 each way
                    show here. The graph shows the rest.
                  </p>
                )}
                {startingOf(evidence.error ?? evidence.failureReason) ? (
                  <DatabaseStarting inline queryKey={evidenceRead.queryKey} />
                ) : (
                  evidence.error && (
                    <p role="alert" className="on-error-text">
                      Couldn’t read what states these links:{" "}
                      {evidence.error.message}
                    </p>
                  )
                )}
                {factGroups(page.facts).map((g) => (
                  <section
                    key={g.link + g.direction}
                    className="ex-group rx-links"
                    aria-label={`${g.link} ${g.direction === "out" ? "to" : "from"}`}
                  >
                    <h4>
                      {g.direction === "out" ? (
                        <>
                          {g.link} <ArrowRight size={14} aria-hidden />
                        </>
                      ) : (
                        <>
                          <ArrowLeft size={14} aria-hidden /> {g.link}
                        </>
                      )}
                      <small>{plural(g.facts.length, "record")}</small>
                    </h4>
                    <ul className="ex-facts">
                      {g.facts.map((f, i) => (
                        <li key={f.factKey || i}>
                          <div className="ex-fact-head">
                            {f.other ? (
                              <button
                                type="button"
                                className="rx-other"
                                onClick={() => onOpen(f.other!)}
                              >
                                {f.other.name}
                              </button>
                            ) : (
                              <span className="on-muted">
                                A record it no longer holds
                              </span>
                            )}
                            {f.other && (
                              <span className="on-muted">{f.other.type}</span>
                            )}
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
                          </div>
                          <ul
                            className="ex-evidence-list"
                            aria-label="Evidence"
                          >
                            {evidence.isPending && f.factKey ? (
                              <li className="on-muted">
                                Reading the evidence…
                              </li>
                            ) : (
                              (evidence.data?.get(f.factKey || "") || []).map(
                                evidenceItem,
                              )
                            )}
                          </ul>
                        </li>
                      ))}
                    </ul>
                  </section>
                ))}
              </>
            )}
          </section>

          <section aria-label="Where it comes from">
            <h3>Where it comes from</h3>
            {!page.rows.length && !page.mentions.length ? (
              <p className="on-muted">
                No table row or document states it: a link made it.
              </p>
            ) : (
              <ul className="ex-evidence-list">
                {page.rows.map((r, i) => {
                  if (r.source === ENRICHMENT_SOURCE)
                    return (
                      <li key={`r${i}`} className="ex-evidence">
                        <UserSearch size={16} aria-hidden />
                        <span>
                          People enrichment · profile{" "}
                          <code className="hf-mono">
                            {(r.rowKey || "unknown").split("/")[0]}
                          </code>
                        </span>
                      </li>
                    );
                  const s = sources.byAgentId(r.source);
                  const synced = Date.parse(r.snapshot || "");
                  return (
                    <li key={`r${i}`} className="ex-evidence">
                      <SourceLogo source={s?.kind || "upload"} size={16} />
                      <span>
                        {s ? (
                          <Link to={s.to}>
                            {s.connector} · {s.table}
                          </Link>
                        ) : (
                          "A table"
                        )}{" "}
                        row <code className="hf-mono">{r.rowKey}</code>
                        {r.snapshot && (
                          <small>
                            {" "}
                            · synced{" "}
                            {Number.isNaN(synced)
                              ? r.snapshot
                              : formatDate(r.snapshot)}
                          </small>
                        )}
                      </span>
                    </li>
                  );
                })}
                {mentionsByDocument(page.mentions).flatMap((d) => [
                  ...d.mentions.slice(0, QUOTES).map((m, i) => {
                    const s = snippet(m.text, m.start, m.end);
                    return (
                      <li key={`${d.documentId}${i}`} className="ex-evidence">
                        <FileText size={16} aria-hidden />
                        <span>
                          <q className="ex-quote">
                            {s.before}
                            <strong>{s.match}</strong>
                            {s.after}
                          </q>{" "}
                          <small>
                            section {m.section + 1} ·{" "}
                            <Link
                              to={link(
                                paths.document(o, d.documentId, "entities") +
                                  "#" +
                                  spanAnchor(m.section, m.start),
                              )}
                            >
                              Open in {docName(d.documentId)}
                            </Link>
                          </small>
                        </span>
                      </li>
                    );
                  }),
                  // The rest are on its page, not left out unsaid.
                  d.mentions.length > QUOTES && (
                    <li key={`${d.documentId}more`} className="ex-evidence">
                      <FileText size={16} aria-hidden />
                      <small>
                        {formatNumber(d.mentions.length - QUOTES)} more in{" "}
                        {docName(d.documentId)} ·{" "}
                        <Link to={link(paths.entity(o, e.id, "mentions"))}>
                          See all
                        </Link>
                      </small>
                    </li>
                  ),
                ])}
                {page.truncated.mentions && (
                  <li className="ex-evidence">
                    <FileText size={16} aria-hidden />
                    <small>
                      Documents name it more often than this reads ·{" "}
                      <Link to={link(paths.entity(o, e.id, "mentions"))}>
                        See more
                      </Link>
                    </small>
                  </li>
                )}
              </ul>
            )}
          </section>
        </div>
      )}
    </Dialog>
  );
}
