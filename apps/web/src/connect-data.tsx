import { useState } from "react";
import { Link } from "react-router-dom";
import { paths } from "./paths";
import { useQueryClient } from "@tanstack/react-query";
import {
  FileUp,
  Globe2,
  Braces,
  Database,
  Plus,
  Waypoints,
} from "lucide-react";
import { api, number } from "./api";
import { useWorkspace, Modal, DataTable } from "./ui";
import {
  useSetup,
  useSetupAction,
  OperationProgress,
} from "./setup-components";
import { ConnectionCard } from "./choice-card";
import { ConnectionForm } from "./connection-form";
import { connectionGroups, type ConnectionGroup } from "./connection-groups";
import type { Resource } from "../../../packages/shared/src";
import {
  hostingProviders,
  type HostingProvider,
} from "../../../packages/shared/src";
import { HostingConnect, providerNames } from "./hosting-connect";

const choices = [
  ["upload", "Upload files", "CSV, JSON, JSONL, or Parquet"],
  ["postgres", "PostgreSQL", "Import tables from your database."],
  ["mysql", "MySQL", "Import tables from your database."],
  ["neo4j", "Neo4j", "Import nodes and relationships from your graph."],
  ["sqlite", "SQLite", "Import tables from a .db or .sqlite file."],
  ["rest", "REST API", "Pull records from another service."],
  ["s3", "Blob storage", "S3 compatible storage"],
  ["api", "Ingestion API", "Send records from your application."],
];
// Business apps connected with an API key, shown beside the database hosts.
const appChoices = [
  ["stripe", "Stripe", "Customers and payments"],
  ["workos", "WorkOS", "Users and organizations"],
  ["posthog", "PostHog", "Product usage and events"],
];
const sourceIcons: Record<string, typeof FileUp> = {
  upload: FileUp,
  rest: Globe2,
  api: Braces,
  neo4j: Waypoints,
  sqlite: Database,
};

export function ConnectData({
  onImported,
  onClose,
  highlight = [],
}: {
  onImported?: () => void;
  onClose?: () => void;
  highlight?: readonly string[];
}) {
  const { id, role } = useWorkspace(),
    setup = useSetup(),
    qc = useQueryClient(),
    action = useSetupAction();
  const [kind, setKind] = useState<string | null>(null);
  // Highlighted cards light up in the order they appear on the page.
  const lit = (value: string) =>
    highlight.includes(value) &&
    [
      ...hostingProviders,
      ...appChoices.map((c) => c[0]),
      ...choices.map((c) => c[0]),
    ]
      .filter((v) => highlight.includes(v))
      .indexOf(value);
  const [hostingProvider, setHostingProvider] =
    useState<HostingProvider | null>(() => {
      const query = new URLSearchParams(window.location.search),
        provider = query.get("hosting");
      return query.get("workspace") === id &&
        hostingProviders.includes(provider as HostingProvider)
        ? (provider as HostingProvider)
        : null;
    });
  const [adding, setAdding] = useState(false);
  const [correcting, setCorrecting] = useState<Resource | null>(null);
  const state = setup.data;
  const groups = connectionGroups(state);
  const current = groups.filter((group) => group.kind === kind);
  const open = (value: string) => {
    setKind(value);
    setAdding(!groups.some((group) => group.kind === value));
    setCorrecting(null);
  };
  const close = () => {
    setKind(null);
    setAdding(false);
    setCorrecting(null);
    setHostingProvider(null);
    if (new URLSearchParams(window.location.search).has("hosting"))
      window.history.replaceState(null, "", window.location.pathname);
    onClose?.();
  };
  const finish = async () => {
    await qc.invalidateQueries({ queryKey: [id] });
    close();
    onImported?.();
  };
  const remove = (group: ConnectionGroup) =>
    action.run(() =>
      api(
        `/workspaces/${id}/onboarding`,
        {
          excludedIds: [
            ...new Set([
              ...(state?.excludedIds || []),
              ...group.sources.map((r) => r.id),
              ...group.datasets.map((r) => r.id),
              ...group.imports.map((r) => r.id),
            ]),
          ],
        },
        "PATCH",
      ),
    );
  const importAction = (job: Resource, verb: string) =>
    action.run(() =>
      api(`/workspaces/${id}/onboarding/imports/${job.id}/${verb}`, {}),
    );
  const waiting =
    state?.imports.filter((job) =>
      ["queued", "running"].includes(job.data.status),
    ).length || 0;
  const needsAttention = groups.filter(
    (group) =>
      group.imports.some((job) =>
        ["failed", "canceled"].includes(job.data.status),
      ) ||
      group.datasets.some(
        (d) => d.data.pendingVersion || !d.data.profile?.rows,
      ) ||
      (group.sources.length > 0 &&
        !group.datasets.length &&
        !group.imports.length),
  );
  if (role === "viewer")
    return <p>An editor can add data to this workspace.</p>;
  const picker = (
    <>
      <div
        className="connection-options choice-options"
        role="group"
        aria-label="Choose your database host or app"
      >
        {hostingProviders.map((provider) => (
          <ConnectionCard
            key={provider}
            title={providerNames[provider]}
            description="Connect your account"
            disabled={role !== "owner"}
            highlighted={lit(provider)}
            count={
              groups.filter((g) =>
                g.sources.some((s) => s.data.hostedProvider === provider),
              ).length
            }
            onClick={() => setHostingProvider(provider)}
            icon={
              <img
                className="connection-icon"
                src={`/connectors/${provider}.svg`}
                alt=""
              />
            }
          />
        ))}
        {appChoices.map(([value, title, description]) => (
          <ConnectionCard
            key={value}
            title={title}
            description={
              role === "owner"
                ? description
                : "A workspace owner can connect this source"
            }
            disabled={role !== "owner"}
            highlighted={lit(value)}
            count={groups.filter((group) => group.kind === value).length}
            onClick={() => open(value)}
            icon={
              <img
                className="connection-icon"
                src={`/connectors/${value}.svg`}
                alt=""
              />
            }
          />
        ))}
      </div>
      <hr className="connection-divider" />
      <p className="setup-intro" id="manual-sources-label">
        Or add data manually
      </p>
      <div
        className="connection-options choice-options"
        role="group"
        aria-labelledby="manual-sources-label"
      >
        {choices.map(([value, title, description]) => {
          const Icon = sourceIcons[value];
          const disabled =
            !["upload", "api"].includes(value) && role !== "owner";
          return (
            <ConnectionCard
              key={value}
              title={title}
              description={
                disabled
                  ? "A workspace owner can connect this source"
                  : description
              }
              count={groups.filter((group) => group.kind === value).length}
              disabled={disabled}
              highlighted={lit(value)}
              onClick={() => open(value)}
              icon={
                Icon ? (
                  <Icon />
                ) : (
                  <img
                    className={
                      value === "mysql"
                        ? "connection-wordmark"
                        : "connection-icon"
                    }
                    src={
                      value === "mysql"
                        ? "/connectors/mysql-wordmark.svg"
                        : `/connectors/${value === "postgres" ? "postgresql" : value}.svg`
                    }
                    alt=""
                  />
                )
              }
            />
          );
        })}
      </div>
    </>
  );
  return (
    <div className="connect-data">
      {!onClose && picker}
      {!onClose &&
        (needsAttention.length > 0 ? (
          <p className="connection-notice" role="status">
            {needsAttention.length === 1
              ? "A source needs attention."
              : `${needsAttention.length} sources need attention.`}{" "}
            <button
              className="text-button"
              onClick={() => open(needsAttention[0].kind)}
            >
              Review
            </button>
          </p>
        ) : waiting > 0 ? (
          <p className="connection-notice" role="status">
            Importing your data… You can add another source.
          </p>
        ) : null)}
      {(kind || hostingProvider || onClose) && (
        <Modal
          title={
            hostingProvider
              ? providerNames[hostingProvider]
              : kind
                ? [...appChoices, ...choices].find(
                    ([value]) => value === kind,
                  )![1]
                : "Where is your data?"
          }
          onClose={close}
        >
          <div className="pad connection-dialog">
            {hostingProvider ? (
              <HostingConnect
                key={hostingProvider}
                provider={hostingProvider}
                onImported={finish}
                onDirect={(kind) => {
                  setHostingProvider(null);
                  open(kind);
                }}
              />
            ) : !kind ? (
              picker
            ) : adding || correcting ? (
              <>
                {current.length > 0 && (
                  <button
                    className="text-button back-link"
                    onClick={() => {
                      setAdding(false);
                      setCorrecting(null);
                    }}
                  >
                    ← Added connections
                  </button>
                )}
                <ConnectionForm
                  key={correcting?.id || kind}
                  kind={kind}
                  correctSource={correcting || undefined}
                  onImported={finish}
                />
                {kind === "api" && (
                  <button className="button primary" onClick={close}>
                    Done
                  </button>
                )}
              </>
            ) : (
              <>
                <div className="connection-list">
                  {current.map((group) => (
                    <section className="connection-entry" key={group.id}>
                      <div className="connection-entry-heading">
                        <strong>{group.name}</strong>
                        <button
                          className="text-button"
                          disabled={action.pending}
                          onClick={() => remove(group)}
                        >
                          Remove
                        </button>
                      </div>
                      {group.sources.some((s) => s.data.sync) && (
                        <p className="muted small" role="status">
                          {group.kind === "sqlite"
                            ? "Imported from the uploaded file. Upload a new copy to refresh it."
                            : group.sources.every(
                                  (s) => s.data.sync?.mode === "cdc",
                                )
                              ? group.sources.some(
                                  (s) => s.data.sync?.status === "retrying",
                                )
                                ? "Reconnecting live sync. Your imported data is still available."
                                : "Database changes sync automatically."
                              : group.sources.some(
                                    (s) => s.data.sync?.mode === "snapshot",
                                  )
                                ? "Data refreshes automatically."
                                : "Checking live sync support…"}
                        </p>
                      )}
                      {group.sources.some((s) => s.data.sync?.reason) && (
                        <details className="disclosure">
                          <summary>Sync details</summary>
                          {[
                            ...new Set(
                              group.sources
                                .map((s) => s.data.sync?.reason)
                                .filter(Boolean),
                            ),
                          ].map((reason) => (
                            <p key={String(reason)} className="muted small">
                              {String(reason)}
                            </p>
                          ))}
                        </details>
                      )}
                      {group.imports
                        .filter((job) => job.data.status !== "succeeded")
                        .map((job) => (
                          <div key={job.id}>
                            <OperationProgress
                              operation={job}
                              onRetry={() => importAction(job, "retry")}
                              onCancel={() => importAction(job, "cancel")}
                            />
                            {["failed", "canceled"].includes(job.data.status) &&
                              role === "owner" &&
                              job.data.sourceId && (
                                <button
                                  className="text-button"
                                  onClick={() =>
                                    setCorrecting(
                                      group.sources.find(
                                        (s) => s.id === job.data.sourceId,
                                      ) || null,
                                    )
                                  }
                                >
                                  Correct connection
                                </button>
                              )}
                          </div>
                        ))}
                      {!group.imports.length &&
                        !group.datasets.length &&
                        group.sources.map((source) => (
                          <button
                            key={source.id}
                            className="button"
                            disabled={action.pending}
                            onClick={() =>
                              action.run(() =>
                                api(
                                  `/workspaces/${id}/sources/${source.id}/sync`,
                                  {},
                                ),
                              )
                            }
                          >
                            Start import
                          </button>
                        ))}
                      {group.datasets.map((dataset) => (
                        <details className="disclosure" key={dataset.id}>
                          <summary>
                            {dataset.name} ·{" "}
                            {number(dataset.data.profile?.rows)} records
                          </summary>
                          {dataset.data.pendingVersion && (
                            <p>
                              <Link to={paths.dataset(dataset.id)}>
                                Review schema change
                              </Link>
                            </p>
                          )}
                          {!dataset.data.profile?.rows && (
                            <p className="error-text">
                              No records to analyze. Add records or remove this
                              source.
                            </p>
                          )}
                          <DataTable
                            rows={dataset.data.profile?.sample || []}
                          />
                        </details>
                      ))}
                    </section>
                  ))}
                </div>
                <button
                  className="button primary"
                  onClick={() => setAdding(true)}
                >
                  <Plus size={15} />
                  {kind === "upload"
                    ? "Upload more files"
                    : kind === "api"
                      ? "Ingestion instructions"
                      : "Add another connection"}
                </button>
              </>
            )}
            {action.error && (
              <p className="error-box" role="alert">
                {action.error}
              </p>
            )}
          </div>
        </Modal>
      )}
    </div>
  );
}
