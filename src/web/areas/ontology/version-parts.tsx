import { useEffect, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { KeyRound, ShieldAlert } from "lucide-react";
import { SourceLogo, Tag, formatNumber } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { enrichmentQuery } from "../workspace/enrichment-api";
import {
  confidenceText,
  lastLoadText,
  readEnrichmentClues,
  type AgentBundle,
  type SourceRef,
} from "./model";
import { Callout } from "./parts";
import { ontologyQueries } from "./queries";
import { useOntology } from "./scope";
import type { Sources } from "./sources";

// Building blocks both formats' tabs use: type cards, property lists and the
// line naming a synced table.

/** Scrolls to the element the URL's #hash names, e.g. a type's card. */
export function useHashScroll() {
  const { hash } = useLocation();
  useEffect(() => {
    if (!hash) return;
    document
      .getElementById(decodeURIComponent(hash.slice(1)))
      ?.scrollIntoView({ block: "start" });
  }, [hash]);
}

/** A synced table: its connector's logo and name, the table and its rows. */
export function SourceLine({
  sources,
  at,
}: {
  sources: Sources;
  at?: SourceRef;
}) {
  const info = at && sources.resolve(at);
  if (!at || !info)
    return (
      <span className="on-source">
        <SourceLogo source="upload" size={18} />
        <span>
          {at?.kind === "table" ? (
            <>
              {at.table} <small className="hf-mono">{at.connectorId}</small>
            </>
          ) : sources.pending ? (
            "Loading…"
          ) : (
            <>
              Unknown source{" "}
              {at && <small className="hf-mono">{at.sourceId}</small>}
            </>
          )}
        </span>
      </span>
    );
  return (
    <span className="on-source">
      <SourceLogo source={info.kind} size={18} />
      <span>
        {info.connector} <span className="on-source-sep">·</span>{" "}
        <span className="hf-mono">{info.table}</span>
        {at.kind === "agent" && at.object && (
          <span className="hf-mono"> {at.object}</span>
        )}
        {info.rows !== undefined && (
          <small> · {formatNumber(info.rows)} rows</small>
        )}
      </span>
    </span>
  );
}

export type PropRow = {
  name: string;
  type: string;
  description?: string;
  key?: boolean;
  // A second tag, e.g. "Name" for the props text names a type by.
  note?: string;
};
export function PropList({ props }: { props: PropRow[] }) {
  if (!props.length) return <p className="on-muted">No properties.</p>;
  return (
    <table className="on-props">
      <thead>
        <tr>
          <th scope="col">Property</th>
          <th scope="col">Type</th>
          <th scope="col">
            <span className="hf-sr-only">Notes</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {props.map((p) => (
          <tr key={p.name}>
            <th scope="row" className="hf-mono">
              {p.name}
            </th>
            <td>{p.type || "—"}</td>
            <td>
              <span className="on-prop-notes">
                {p.key && (
                  <Tag tone="accent" icon={KeyRound}>
                    Key
                  </Tag>
                )}
                {p.note && <Tag>{p.note}</Tag>}
                {p.description && <span>{p.description}</span>}
              </span>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export type TypeCardItem = {
  name: string;
  description: string;
  badges?: ReactNode;
  props: PropRow[];
  sources: SourceRef[];
  // Below the properties, e.g. the type's links.
  extra?: ReactNode;
  // Why the agent proposed it.
  why?: string;
};
export function TypeCards({
  items,
  sources,
}: {
  items: TypeCardItem[];
  sources: Sources;
}) {
  useHashScroll();
  return (
    <div className="on-types">
      {items.map((t) => (
        <section
          key={t.name}
          className="on-item on-type"
          id={`type-${t.name}`}
          aria-labelledby={`type-${t.name}-name`}
        >
          <header>
            <h3 id={`type-${t.name}-name`}>{t.name}</h3>
            {t.badges}
          </header>
          {t.description && <p>{t.description}</p>}
          <PropList props={t.props} />
          {t.sources.length > 0 && (
            <ul className="on-type-sources" aria-label={`${t.name} sources`}>
              {t.sources.map((s, i) => (
                <li key={i}>
                  <SourceLine sources={sources} at={s} />
                </li>
              ))}
            </ul>
          )}
          {t.extra}
          {t.why && <p className="on-why">{t.why}</p>}
        </section>
      ))}
    </div>
  );
}

// What an evidence card wasn't measured on (EvidenceCard.unmeasured).
const UNMEASURED = new Map<unknown, string>([
  [
    "rows",
    "Not measured on every table: the dry run’s sample was missing rows it needs, from a mapping that can fill it or, for a rule, rows that state what it derived about its subject.",
  ],
  ["documents", "Not measured on documents: the preview had none for it."],
  [
    "rows, documents",
    "Not measured on every table or on documents: the dry run’s sample was missing rows it needs, and the preview had no documents for it.",
  ],
  [
    "load",
    "Not measured: no load of a version with the same types, links and properties. A constraint is capped at warn.",
  ],
  [
    "entities",
    "Not measured on every entity: some of the load’s entities were over the read budget, so a constraint is capped at warn.",
  ],
]);

/**
 * In place of evidence measured on the ontology's load (a revision's result,
 * a revised version's cards), which the WFE withholds from one who can't
 * query its database.
 */
export function EvidenceWithheld() {
  return (
    <Callout
      icon={ShieldAlert}
      tone="muted"
      title="Evidence hidden"
      text="It was measured on the ontology’s load, and you can’t read the ontology’s database. Ask a workspace admin for access to its project."
    />
  );
}

/** An evidence card as text, with what it wasn't measured on in words. */
export function CardFields({ card }: { card: unknown }) {
  const unmeasured =
    card && typeof card === "object" && "unmeasured" in card
      ? UNMEASURED.get(card.unmeasured)
      : undefined;
  return (
    <>
      {unmeasured && <p>{unmeasured}</p>}
      <EvidenceFields value={card} />
    </>
  );
}

/** Preserve untyped engine fields, including unknown verdicts and measured counts. */
export function EvidenceFields({ value }: { value: unknown }) {
  const fields: [string, unknown][] =
    value && typeof value === "object" && !Array.isArray(value)
      ? Object.entries(value)
      : [["Result", value]];
  return (
    <dl className="hf-kv">
      {fields.map(([key, field]) => (
        <div key={key}>
          <dt>{key}</dt>
          <dd>{typeof field === "string" ? field : JSON.stringify(field)}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * What a version's People enrichment item says beyond its fields: why the
 * agent suggested it and how sure it is (or that a hand-added item isn't
 * measured), who the newest load of the version could look up, and that
 * loads apply no profiles while the workspace hasn't allowed it.
 */
export function EnrichmentNotes({
  bundle,
  versionId,
}: {
  bundle: AgentBundle;
  versionId: string;
}) {
  const { id } = useWorkspace(),
    { id: o } = useOntology();
  const loads = useQuery(ontologyQueries.loads(id, o)),
    settings = useQuery(enrichmentQuery(id));
  const item = bundle.enrichments.ok ? bundle.enrichments.value[0] : undefined;
  if (!item) return null;
  const draft = bundle.items[`enrichment.${item.name}`];
  const last = lastLoadText(loads.data?.loads || [], versionId);
  return (
    <>
      {draft?.status === "proposed" && draft.rationale && (
        <p className="on-why">Why: {draft.rationale}</p>
      )}
      <p className="on-muted">
        {confidenceText(readEnrichmentClues(draft?.clues))}
      </p>
      {last && <p className="on-muted">{last}</p>}
      {settings.data && !settings.data.enabled && (
        <p className="on-muted">
          People enrichment is off for this workspace, so loads apply no
          profiles. An owner can allow it in{" "}
          <Link to={paths.settings("enrichment")} className="on-link">
            Settings
          </Link>
          .
        </p>
      )}
    </>
  );
}
