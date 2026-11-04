// Workspace area: route components lazy-loaded by main.tsx.
import "./workspace.css";
import { Route, Routes, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { LOCAL } from "../../../shared/deployment";
import type { Workspace } from "../../../shared/wfe";
import { api } from "../../api";
import {
  KeyValue,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  TabPanel,
  Tabs,
  formatDate,
} from "../../kit";
import { paths } from "../../paths";
import { NotFound } from "../../shell/ErrorBoundary";
import { useWorkspace } from "../../ui";
import { AccessTab } from "./access";
import { AiTab } from "./ai";
import { BillingTab } from "./billing";
import { EnrichmentTab } from "./enrichment";
import { DASHBOARD_URL, MembersTab } from "./members";
import type { SettingsTab } from "./paths";
import { roleLabel } from "./people";
import { canManage } from "./plan";

// /settings: the workspace itself, read-only. Renaming and deleting stay in
// the Helix dashboard.
function GeneralTab() {
  const { id, role } = useWorkspace();
  const workspace = useQuery({
    queryKey: [id, "workspace"],
    queryFn: () => api<Workspace>(`/v1/workspaces/${encodeURIComponent(id)}`),
  });
  const w = workspace.data;
  return (
    <>
      <Panel title="Workspace">
        {workspace.isPending ? (
          <SkeletonText lines={4} />
        ) : workspace.error ? (
          <p role="alert" className="hf-form-error">
            Couldn’t load the workspace. {workspace.error.message}
          </p>
        ) : (
          <KeyValue
            columns={2}
            items={[
              { label: "Name", value: w?.displayName || w?.slug },
              { label: "Address", value: w?.slug, mono: true },
              { label: "Region", value: w?.region, mono: true },
              { label: "Your role", value: roleLabel[role] },
              {
                label: "Created",
                value:
                  w?.timestamps?.createdAt &&
                  formatDate(w.timestamps.createdAt, false),
              },
              { label: "Workspace ID", value: w?.id, mono: true },
            ]}
          />
        )}
      </Panel>
      {!LOCAL && (
        <p className="wk-note">
          Workspaces are shared with Helix Cloud. Rename or delete this one in
          the{" "}
          <a href={DASHBOARD_URL} target="_blank" rel="noreferrer">
            Helix dashboard
          </a>
          .
        </p>
      )}
    </>
  );
}

type Tab = "general" | SettingsTab;
const tabs: { id: Tab; label: string }[] = [
  { id: "general", label: "General" },
  { id: "ai", label: "AI models" },
  // A local build has no team, access, billing or People enrichment (its
  // lookups are a hosted service's).
  ...(LOCAL
    ? []
    : [
        { id: "members" as const, label: "Members" },
        { id: "access" as const, label: "Access" },
        { id: "billing" as const, label: "Plan & billing" },
        { id: "enrichment" as const, label: "People enrichment" },
      ]),
];

function SettingsPage({ tab }: { tab: Tab }) {
  const { name, role } = useWorkspace(),
    navigate = useNavigate();
  // Access is owners' and admins'.
  const shown = tabs.filter(
    (t) => t.id !== "access" || canManage(role) || tab === "access",
  );
  return (
    <Page>
      <PageHeader
        title="Settings"
        subtitle={name}
        documentTitle={tabs.find((t) => t.id === tab)!.label}
        tabs={
          <Tabs
            label="Settings"
            idPrefix="settings"
            items={shown}
            value={tab}
            onChange={(t) =>
              navigate(
                paths.settings(
                  t === "general" ? undefined : (t as SettingsTab),
                ),
              )
            }
          />
        }
      />
      <TabPanel
        id={tab}
        value={tab}
        idPrefix="settings"
        className="wk-settings"
      >
        {tab === "general" ? (
          <GeneralTab />
        ) : tab === "ai" ? (
          <AiTab />
        ) : tab === "members" ? (
          <MembersTab />
        ) : tab === "access" ? (
          <AccessTab />
        ) : tab === "billing" ? (
          <BillingTab />
        ) : (
          <EnrichmentTab />
        )}
      </TabPanel>
    </Page>
  );
}

// /settings, /settings/ai, /settings/members, /settings/access,
// /settings/billing (where Stripe Checkout returns) and
// /settings/enrichment; only /settings and /settings/ai in a local build.
export function SettingsRoutes() {
  return (
    <Routes>
      <Route index element={<SettingsPage tab="general" />} />
      <Route path="ai" element={<SettingsPage tab="ai" />} />
      {!LOCAL && (
        <>
          <Route path="members" element={<SettingsPage tab="members" />} />
          <Route path="access" element={<SettingsPage tab="access" />} />
          <Route path="billing" element={<SettingsPage tab="billing" />} />
          <Route
            path="enrichment"
            element={<SettingsPage tab="enrichment" />}
          />
        </>
      )}
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
