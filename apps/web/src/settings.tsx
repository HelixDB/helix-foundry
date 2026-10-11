import { ProviderSetup } from "./setup-components";
import { BusinessSettings } from "./business-settings";
import { useState } from "react";
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
import { api } from "./api";
export default function SettingsPage() {
  const { id, role } = useWorkspace(),
    action = useAction(),
    [tab, setTab] = useState(
      new URLSearchParams(location.search).get("tab") || "models",
    ),
    [apiToken, setApiToken] = useState("");
  const owner = role === "owner";
  return (
    <>
      <PageHeader title="Workspace settings" description="" />
      <div className="tabs standalone">
        {[
          ["models", "AI & models"],
          ["business", "Your business"],
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
      ) : tab === "business" ? (
        <section className="reading-width">
          <BusinessSettings />
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
                  setApiToken(r.token);
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
              {apiToken && (
                <div className="copy-box">
                  <code>{apiToken}</code>
                  <button
                    type="button"
                    className="button small"
                    onClick={() => navigator.clipboard.writeText(apiToken)}
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
