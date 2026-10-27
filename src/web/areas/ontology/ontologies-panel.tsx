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
} from "../../kit";
import { paths } from "../../paths";
import { childActive } from "../../shell/nav";
import type { NavChild } from "../../shell/types";
import { RefreshFailed, useWorkspace } from "../../ui";
import { OntologyDocumentsTree } from "../documents/explorer";
import { canManage, lastLoadBadge } from "./parts";
import { ontologyQueries } from "./queries";
import { ontologyName } from "./scope";

// The workspace's ontologies, newest first, each with its database and how
// its last load went; the open one's row (aria-current) holds its pages,
// the page open marked too. Owners and admins create one from the head. A
// long list gets a filter, by name or database. A workspace with no ontology
// never shows the panel (it is onboarding, ../../main.tsx), and reads the
// same list, so only a filter leaves it empty. Under its Documents link, its
// documents are a file system: the sets it reads and their documents
// (../documents/explorer.tsx).
//
// It reads everything from the URL and the query cache, so it can sit
// outside the page's routes. `onNavigate` runs on every link it follows
// (a phone's drawer closes on it).

// /ontology/{ontologyId}..., but not /ontology/new.
const ONTOLOGY_PAGE = /^\/ontology\/(?!new(?:\/|$))([^/]+)/;
// /ontology/{ontologyId}/documents, and /{documentId} under it.
const DOCUMENTS_PAGE = /^\/ontology\/([^/]+)\/documents(?:\/([^/]+))?\/?$/;
// From this many ontologies, the list gets a filter.
const LONG_LIST = 8;

export function OntologiesPanel({ onNavigate }: { onNavigate?: () => void }) {
  const { id: ws, role } = useWorkspace(),
    { pathname } = useLocation(),
    manage = canManage(role);
  const match = ONTOLOGY_PAGE.exec(pathname),
    open = match ? decodeURIComponent(match[1]!) : "",
    // On its documents pages: the document open, if one is.
    documents = DOCUMENTS_PAGE.exec(pathname),
    documentId = documents?.[2] ? decodeURIComponent(documents[2]) : undefined;
  const list = useQuery(ontologyQueries.list(ws)),
    own = useQuery({
      ...ontologyQueries.ontology(ws, open),
      enabled: !!open,
    }).data?.ontology;
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
  // and generating, are its own), then Documents: what it reads of the
  // workspace's documents (the Connectors area's), counted once the ontology
  // is read, the list's page, with the documents as a file system under it.
  // The Explorer and Ask, which read a load, are sections of their own.
  const overview: NavChild = {
    to: paths.ontology(open),
    label: "Overview",
    end: true,
    match: ["generate", "generations", "versions", "loads"].map(
      (p) => paths.ontology(open) + "/" + p,
    ),
  };
  const onOverview = childActive(pathname, overview),
    // Marked on its documents pages; current on the list (on a document's
    // page, the document's row is).
    onDocuments = !!documents && open !== "",
    onDocumentsList = onDocuments && !documentId;
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
                  <StatusDot status={load.status} label={load.label} />
                  <small>
                    {o.databaseName || o.tenantId || "Not visible to you"}
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
                        to={paths.documents(open)}
                        onClick={onNavigate}
                        className={cx(
                          "hf-subnav-link",
                          onDocuments && "is-active",
                        )}
                        aria-current={onDocumentsList ? "page" : undefined}
                      >
                        <span>Documents</span>
                        {own && (
                          <span className="on-panel-count">
                            {own.counts?.documents || 0}
                          </span>
                        )}
                      </Link>
                      <OntologyDocumentsTree
                        key={open}
                        ontologyId={open}
                        documentId={documentId}
                        onNavigate={onNavigate}
                      />
                    </li>
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
