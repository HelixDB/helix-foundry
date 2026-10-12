import { ProviderSetup } from "./setup-components";
import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiTokenMetadata } from "../../../packages/shared/src";
import {
  Copy,
  Download,
  Globe,
  KeyRound,
  LoaderCircle,
  Monitor,
  Settings,
  ShieldCheck,
  Sparkles,
  Terminal,
} from "lucide-react";
import { useWorkspace, useAction, PageHeader } from "./ui";
import { Button, ConfirmDialog } from "./kit";
import { api } from "./api";
export default function SettingsPage() {
  const { id, role } = useWorkspace(),
    action = useAction(),
    [tab, setTab] = useState(
      new URLSearchParams(location.search).get("tab") || "models",
    ),
    [issued, setIssued] = useState<{ token: string; id: string } | null>(null),
    [revoking, setRevoking] = useState<ApiTokenMetadata | null>(null);
  const owner = role === "owner";
  const tokens = useQuery({
    queryKey: [id, "developer-tokens"],
    enabled: owner && tab === "developer",
    queryFn: () =>
      api<{ items: ApiTokenMetadata[] }>(`/workspaces/${id}/tokens`),
  });
  useEffect(() => setIssued(null), [id]);
  return (
    <>
      <PageHeader title="Workspace settings" description="" />
      <div className="tabs standalone">
        {[
          ["models", "AI & models"],
          ["developer", "Developer tools"],
        ].map(([value, label]) => (
          <button
            key={value}
            className={tab === value ? "active" : ""}
            onClick={() => setTab(value)}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === "models" ? (
        <section className="reading-width">
          <ProviderSetup />
        </section>
      ) : (
        <>
          <section className="panel settings-panel">
            <div className="panel-title">
              <h3>
                <Terminal size={18} /> Developer access
              </h3>
              <a href="/api/docs" target="_blank">
                Open API docs ↗
              </a>
            </div>
            <form
              className="pad"
              onSubmit={(e) => {
                e.preventDefault();
                const f = new FormData(e.currentTarget);
                action.run(async () => {
                  const r = await api(`/workspaces/${id}/tokens`, {
                    name: f.get("name"),
                    readOnly: f.get("readOnly") === "on",
                  });
                  setIssued(r);
                });
              }}
            >
              <label>
                Token name
                <input
                  name="name"
                  required
                  placeholder="Local development"
                  disabled={!owner}
                />
              </label>
              <label className="checkbox">
                <input
                  name="readOnly"
                  type="checkbox"
                  defaultChecked
                  disabled={!owner}
                />{" "}
                Read-only access
              </label>
              <button className="button" disabled={!owner}>
                <KeyRound size={15} /> Create API token
              </button>
              {issued && (
                <div className="copy-box">
                  <code>{issued.token}</code>
                  <button
                    type="button"
                    className="button small"
                    onClick={() => navigator.clipboard.writeText(issued.token)}
                  >
                    <Copy size={14} />
                  </button>
                </div>
              )}
              <p className="muted">
                Tokens are scoped to this workspace. Copy once and store
                securely.
              </p>
              <pre className="sql-block">{`import { FoundryClient } from '@helix-foundry/sdk';\n\nconst foundry = new FoundryClient({\n  baseUrl: '${location.origin}',\n  workspaceId: '${id}',\n  token: process.env.FOUNDRY_TOKEN,\n});\n\nconst datasets = await foundry.list('dataset');`}</pre>
            </form>
          </section>
          {owner && (
            <section
              className="panel settings-panel"
              aria-label="Active API tokens"
            >
              <div className="pad">
                <h3>Active API tokens</h3>
                <p className="muted">
                  Revoke access when an integration is no longer needed. Stored
                  token values are never shown again.
                </p>
                {tokens.isPending ? (
                  <p>Loading tokens…</p>
                ) : tokens.error ? (
                  <p role="alert">{tokens.error.message}</p>
                ) : tokens.data?.items.length ? (
                  <ul className="simple-list">
                    {tokens.data.items.map((t) => (
                      <li className="simple-row" key={t.id}>
                        <div>
                          <strong>{t.name}</strong>
                          <p>
                            {t.readOnly ? "Read-only" : "Read and write"}
                            {" · "}
                            Created {new Date(t.createdAt).toLocaleDateString()}
                          </p>
                        </div>
                        <div>
                          <Button
                            size="sm"
                            variant="danger"
                            disabled={action.pending}
                            aria-label={`Revoke ${t.name}`}
                            onClick={() => setRevoking(t)}
                          >
                            Revoke
                          </Button>
                        </div>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p>No active API tokens.</p>
                )}
              </div>
              {revoking && (
                <ConfirmDialog
                  title={`Revoke “${revoking.name}”?`}
                  description="Integrations that use this token will stop working. This can’t be undone."
                  confirmLabel="Revoke token"
                  tone="danger"
                  pending={action.pending}
                  onConfirm={() =>
                    action.run(async () => {
                      await api(
                        `/workspaces/${id}/tokens/${revoking.id}`,
                        {},
                        "DELETE",
                      );
                      if (issued?.id === revoking.id) setIssued(null);
                      setRevoking(null);
                    }, "API token revoked")
                  }
                  onCancel={() => setRevoking(null)}
                />
              )}
            </section>
          )}
          <section className="panel settings-panel">
            <div className="pad">
              <h3>Portable definitions</h3>
              <p className="muted">
                Export published definitions without credentials. Imports become
                validated proposals.
              </p>
              <a
                className="button"
                href={`/api/v1/workspaces/${id}/definitions/export`}
                download="foundry-definitions.json"
              >
                <Download size={15} /> Export definitions
              </a>
            </div>
          </section>
        </>
      )}
    </>
  );
}
