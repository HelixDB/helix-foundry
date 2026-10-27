import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { History } from "lucide-react";
import {
  Button,
  DataGrid,
  EmptyState,
  Section,
  StatusBadge,
  Tag,
  formatDate,
  formatRelative,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import { formatLabel, versionFormat } from "./format";
import { generationTitle, versionRows } from "./model";
import { GenerationBadge, MakeCurrent, sourceLabel } from "./parts";
import { ontologyQueries } from "./queries";
import { useOntology } from "./scope";

// The overview's Schema versions section (#versions): every version, newest
// first, named by the generation that drafted it, among the generations that
// failed to draft one; the live one is the overview's callout. A version is
// immutable; making one current is the only change, and rolling back is
// making an older one current again. A failed generation's row opens its
// page, which says why. Nothing shows until there is a row (setup says how
// to draft the first) or the versions fail to load.

export function VersionsSection() {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate();
  const versions = useQuery(ontologyQueries.versions(id, o)),
    generations = useQuery(ontologyQueries.generations(id, o));
  // The versions are the page; the generations only name them and add the
  // failed ones, so without them the versions still list, and can still be
  // made current.
  const currentId = ontology.currentVersionId || "";
  const rows = versionRows(
    versions.data?.versions || [],
    generations.data?.generations || [],
  ).map((r) =>
    r.kind === "version"
      ? {
          row: r,
          key: r.version.id,
          number: r.version.version,
          isCurrent: r.version.id === currentId,
          // Without its generation (not listed, or older than the list's
          // 100), what made it.
          titleText: r.generation
            ? generationTitle(r.generation)
            : sourceLabel(r.version.source),
          formatText: formatLabel[versionFormat(r.version)],
          createdAt: r.version.createdAt,
          to: paths.version(o, r.version.id),
        }
      : {
          // No number or format: blank cells sort last.
          row: r,
          key: r.generation.id,
          number: undefined,
          isCurrent: false,
          titleText: generationTitle(r.generation),
          formatText: undefined,
          createdAt: r.generation.createdAt,
          to: paths.generation(o, r.generation.id),
        },
  );
  const failed = !versions.data && !!versions.error;
  if (!failed && !rows.length) return null;
  return (
    <div id="versions">
      <Section
        id="on-versions"
        title="Schema versions"
        description="Each generation drafts a new version of this ontology’s types, links and rules. Making an older one current rolls back."
      >
        {versions.data && <RefreshFailed query={versions} />}
        {versions.data && <RefreshFailed query={generations} />}
        {failed ? (
          <EmptyState
            icon={History}
            title="Couldn’t load the versions"
            text={versions.error?.message}
            action={
              <Button onClick={() => void versions.refetch()}>Retry</Button>
            }
          />
        ) : (
          <DataGrid
            label="Ontology versions"
            loading={generations.isPending}
            rows={rows}
            getRowKey={(r) => r.key}
            rowHeight={44}
            onRowClick={(r) => navigate(r.to)}
            columns={[
              {
                key: "number",
                label: "Version",
                width: 130,
                type: "integer",
                align: "left",
                render: (r) =>
                  r.row.kind === "version" ? (
                    <Link
                      to={r.to}
                      className="on-row-link"
                      onClick={(e) => e.stopPropagation()}
                    >
                      Version {r.number ?? "?"}
                    </Link>
                  ) : (
                    <span className="on-muted">—</span>
                  ),
              },
              {
                key: "isCurrent",
                label: "Status",
                width: 130,
                render: (r) =>
                  r.row.kind === "failed" ? (
                    <GenerationBadge generation={r.row.generation} size="sm" />
                  ) : r.isCurrent ? (
                    <StatusBadge status="succeeded" label="Current" size="sm" />
                  ) : (
                    <span className="on-muted">—</span>
                  ),
              },
              {
                key: "titleText",
                label: "Title",
                width: 260,
                // A version's link is its number; a failed generation's,
                // its title, with why it failed.
                render: (r) =>
                  r.row.kind === "failed" ? (
                    <Link
                      to={r.to}
                      className="on-row-link on-cell-text"
                      title={[r.titleText, r.row.generation.error]
                        .filter(Boolean)
                        .join(": ")}
                      onClick={(e) => e.stopPropagation()}
                    >
                      {r.titleText}
                      {r.row.generation.error && (
                        <small> · {r.row.generation.error}</small>
                      )}
                    </Link>
                  ) : (
                    <span className="on-cell-text" title={r.titleText}>
                      {r.titleText}
                    </span>
                  ),
              },
              {
                key: "formatText",
                label: "Format",
                width: 150,
                render: (r) =>
                  r.formatText ? (
                    <Tag>{r.formatText}</Tag>
                  ) : (
                    <span className="on-muted">—</span>
                  ),
              },
              {
                key: "createdAt",
                label: "Created",
                width: 150,
                type: "timestamp",
                render: (r) => (
                  <span title={formatDate(r.createdAt)}>
                    {formatRelative(r.createdAt || "")}
                  </span>
                ),
              },
              {
                key: "actions",
                label: <span className="hf-sr-only">Actions</span>,
                // No type icon: the column holds buttons, not values.
                className: "on-actions",
                width: 185,
                sortable: false,
                render: (r) =>
                  r.row.kind === "version" &&
                  versions.isSuccess && (
                    <span onClick={(e) => e.stopPropagation()}>
                      <MakeCurrent
                        version={{ ...r.row.version, isCurrent: r.isCurrent }}
                        currentId={currentId}
                        variant="secondary"
                        size="sm"
                      />
                    </span>
                  ),
              },
            ]}
          />
        )}
      </Section>
    </div>
  );
}
