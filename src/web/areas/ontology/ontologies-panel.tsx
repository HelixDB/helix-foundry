// The Ontologies panel, the Ontologies section's panel beside the sidebar. The
// shell loads it apart from the Ontology pages, so it brings their styles.
import "./ontology.css";
import { useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import {
  Button,
  ButtonLink,
  SearchInput,
  SkeletonText,
  StatusDot,
  cx,
  formatNumber,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { childActive } from "../../shell/nav";
import type { NavChild } from "../../shell/types";
import { RefreshFailed, canManage, useWorkspace } from "../../ui";
import { useReviewSummary } from "../reviews/cards-api";
import { suggestionQueries } from "../suggestions/api";
import { loadVersion } from "./model";
import { lastLoadBadge } from "./parts";
import { ontologyQueries } from "./queries";
import { ontologyName } from "./scope";

// The workspace's ontologies, newest first, each with its database and how
// its last load went; the open one's row (aria-current) holds its pages,
// the page open marked too. Owners and admins create one from the head. A
// long list gets a filter, by name or database. A workspace with no ontology
// never shows the panel (it is onboarding, ../../main.tsx), and reads the
// same list, so only a filter leaves it empty.
//
// It reads everything from the URL and the query cache, so it can sit
// outside the page's routes. `onNavigate` runs on every link it follows
// (a phone's drawer closes on it).

// /ontology/{ontologyId}..., but not /ontology/new or /ontology/sample.
const ONTOLOGY_PAGE = /^\/ontology\/(?!(?:new|sample)(?:\/|$))([^/]+)/;
// /ontology/{ontologyId}/connections, and a document's page under it.
const CONNECTIONS_PAGE =
  /^\/ontology\/[^/]+\/(?:connections\/?$|documents\/[^/]+\/?$)/;
// From this many ontologies, the list gets a filter.
const LONG_LIST = 8;

export function OntologiesPanel({ onNavigate }: { onNavigate?: () => void }) {
  const { id: ws, role, grantee, partial, workflows } = useWorkspace(),
    { pathname } = useLocation(),
    manage = canManage(role);
  const match = ONTOLOGY_PAGE.exec(pathname),
    open = match ? decodeURIComponent(match[1]!) : "";
  const list = useQuery(ontologyQueries.list(ws)),
    own = useQuery({
      ...ontologyQueries.ontology(ws, open),
      enabled: !!open,
    }).data?.ontology;
  // Review counts what is open on both its tabs, and says each part: Facts'
  // cards to decide on the current load, as the review cards index counts
  // them (the summary, less the walk's own choices), and Ontology's
  // suggestions where audits run. A total waits for Facts' part; no grantee
  // reaches Review.
  const heal = useQuery({
      ...suggestionQueries.list(ws, open),
      enabled: !!open && !grantee,
    }),
    facts = useReviewSummary().get(open);
  const counted = facts && own?.currentLoad?.id === facts.loadId,
    factsOpen = counted ? (facts.open ?? 0) : undefined,
    // The index counts at most 10,000: more says so.
    more = !!(counted && facts.more),
    suggestionsOpen = heal.data?.enabled ? heal.data.findings?.length || 0 : 0;
  const reviewOpen = factsOpen === undefined ? 0 : factsOpen + suggestionsOpen;
  const reviewParts = [
    factsOpen &&
      (more
        ? `${formatNumber(factsOpen)}+ cards to decide`
        : `${plural(factsOpen, "card")} to decide`),
    suggestionsOpen && plural(suggestionsOpen, "ontology suggestion"),
  ]
    .filter(Boolean)
    .join(", ");
  // The open ontology's row, until the list names it (made in another tab).
  const listed = list.data?.ontologies || [],
    ontologies =
      own && !listed.some((o) => o.id === own.id) ? [own, ...listed] : listed;
  const [search, setSearch] = useState(""),
    term = search.trim().toLowerCase(),
    shown = ontologies.filter(
      (o) =>
        !term ||
        ontologyName(o).toLowerCase().includes(term) ||
        (o.databaseName || "").toLowerCase().includes(term),
    );
  // Overview, which lists its schema versions and data loads (their pages,
  // and generating, are its own), then the Explorer, its database's graph
  // and records (a page of its own, /explore, with its own panel; a
  // grantee's is Records), then Connections: the connections and documents
  // it reads (the Connectors area's), marked on a document's page too; then
  // Review: what its current load couldn't decide (Facts) and what audits
  // of it found the ontology lacks (Ontology); then Usage, its use cases
  // (its agents' tasks) and what their runs read and wrote, for anyone but a
  // grantee without the Usage permission. Ask, which reads a load, is a
  // section of its own.
  const overview: NavChild = {
    to: paths.ontology(open),
    label: "Overview",
    end: true,
    match: ["generate", "generations", "versions", "loads"].map(
      (p) => paths.ontology(open) + "/" + p,
    ),
  };
  const onOverview = childActive(pathname, overview),
    // Current on its page, marked on a document's.
    onConnections = open !== "" && CONNECTIONS_PAGE.test(pathname),
    onConnectionsList = onConnections && pathname.endsWith("/connections"),
    onReview = open !== "" && pathname === paths.review(open),
    onWorkflows =
      open !== "" &&
      (pathname === paths.workflows(open) ||
        pathname.startsWith(paths.workflows(open) + "/")),
    onWorkflowsList = onWorkflows && pathname === paths.workflows(open);
  return (
    <div className="on-panel">
      <div className="on-panel-head">
        <h2>Ontologies</h2>
        {manage && (
          <ButtonLink
            to={paths.newOntology()}
            variant="ghost"
            size="sm"
            icon={Plus}
            aria-label="New ontology"
            title="New ontology"
            aria-current={pathname === paths.newOntology() ? "page" : undefined}
            onClick={onNavigate}
          />
        )}
      </div>
      {(ontologies.length >= LONG_LIST || !!search) && (
        <SearchInput
          className="on-panel-filter"
          value={search}
          onChange={setSearch}
          label="Filter ontologies"
          placeholder="Filter"
        />
      )}
      {list.data && <RefreshFailed query={list} />}
      {list.isPending ? (
        <div className="on-panel-note" aria-busy="true">
          <SkeletonText lines={3} />
        </div>
      ) : !list.data ? (
        <p className="on-panel-note">
          <span role="alert">
            Couldn’t load the ontologies: {list.error?.message}
          </span>{" "}
          <Button size="sm" variant="ghost" onClick={() => void list.refetch()}>
            Retry
          </Button>
        </p>
      ) : !shown.length ? (
        <p className="on-panel-note">No ontologies match “{search.trim()}”.</p>
      ) : (
        <ul className="on-panel-list" aria-label="Ontologies">
          {shown.map((o) => {
            const here = o.id === open,
              load = lastLoadBadge(o);
            return (
              <li key={o.id}>
                <Link
                  to={paths.ontology(o.id)}
                  onClick={onNavigate}
                  className={cx("on-panel-row", here && "is-active")}
                  aria-current={here ? "true" : undefined}
                >
                  <span className="on-panel-name">{ontologyName(o)}</span>
                  {!grantee && (
                    <StatusDot status={load.status} label={load.label} />
                  )}
                  <small>
                    {grantee
                      ? `${partial.includes(o.id) ? "Part shared with you" : "Shared with you"}${o.currentLoad ? ` · ${loadVersion(o.currentLoad)}` : ""}`
                      : o.databaseName || o.tenantId || "Not visible to you"}
                  </small>
                </Link>
                {here && (
                  <ul className="hf-subnav" aria-label="Pages">
                    <li>
                      <Link
                        to={overview.to}
                        onClick={onNavigate}
                        className={cx(
                          "hf-subnav-link",
                          onOverview && "is-active",
                        )}
                        aria-current={onOverview ? "page" : undefined}
                      >
                        <span>{overview.label}</span>
                      </Link>
                    </li>
                    <li>
                      <Link
                        to={grantee ? paths.records(open) : paths.explore(open)}
                        onClick={onNavigate}
                        className="hf-subnav-link"
                      >
                        <span>Explorer</span>
                      </Link>
                    </li>
                    {/* A grantee reads only Analyst, the schema and Records. */}
                    {!grantee && (
                      <li>
                        <Link
                          to={paths.ontologyConnections(open)}
                          onClick={onNavigate}
                          className={cx(
                            "hf-subnav-link",
                            onConnections && "is-active",
                          )}
                          aria-current={onConnectionsList ? "page" : undefined}
                        >
                          <span>Connections</span>
                        </Link>
                      </li>
                    )}
                    {!grantee && (
                      <li>
                        <Link
                          to={paths.review(open)}
                          onClick={onNavigate}
                          className={cx(
                            "hf-subnav-link",
                            onReview && "is-active",
                          )}
                          aria-current={onReview ? "page" : undefined}
                          aria-label={
                            reviewOpen > 0
                              ? `Review: ${reviewParts}`
                              : undefined
                          }
                          title={reviewOpen > 0 ? reviewParts : undefined}
                        >
                          <span>Review</span>
                          {reviewOpen > 0 && (
                            <span className="hf-nav-badge" aria-hidden>
                              {formatNumber(reviewOpen)}
                              {more && "+"}
                            </span>
                          )}
                        </Link>
                      </li>
                    )}
                    {workflows && (
                      <li>
                        <Link
                          to={paths.workflows(open)}
                          onClick={onNavigate}
                          className={cx(
                            "hf-subnav-link",
                            onWorkflows && "is-active",
                          )}
                          aria-current={onWorkflowsList ? "page" : undefined}
                        >
                          <span>Usage</span>
                        </Link>
                      </li>
                    )}
                  </ul>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
