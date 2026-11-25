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
import { canManage, useWorkspace } from "../../ui";
import { AccessTab } from "./access";
import { AiTab } from "./ai";
import { BillingTab } from "./billing";
import { EnrichmentTab } from "./enrichment";
import { publicLinks } from "../dashboards/public-links";
import { SharingTab } from "./sharing";
import { SlackTab } from "./slack";
import { SlackInstalledPage } from "./slack-installed";
import { SlackLinkPage } from "./slack-link";
import { RelayLinkPage } from "./relay-link";
import { partnerApps } from "./partner-apps";
import { PartnerAppsTab } from "./partner-apps-tab";
import { PartnerAuthorizePage } from "./partner-authorize";
import { DASHBOARD_URL, MembersTab } from "./members";
import type { SettingsTab } from "./paths";
import { roleLabel } from "./people";

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
  // lookups are a hosted service's). Its Slack goes through Helix's relay.
  ...(LOCAL
    ? []
    : [
        { id: "members" as const, label: "Members" },
        { id: "access" as const, label: "Access" },
        // Partner apps reading the workspace, where the build offers them.
        ...(partnerApps
          ? [{ id: "apps" as const, label: "Connected apps" }]
          : []),
        { id: "billing" as const, label: "Plan & billing" },
        { id: "enrichment" as const, label: "People enrichment" },
      ]),
  { id: "slack" as const, label: "Slack" },
  // Public dashboard links, where the build has them (never a local one).
  ...(publicLinks ? [{ id: "sharing" as const, label: "Sharing" }] : []),
];

function SettingsPage({ tab }: { tab: Tab }) {
  const { name, role, grantee } = useWorkspace(),
    navigate = useNavigate();
  // Access and Connected apps are owners' and admins'. A grantee reaches
  // only Slack, to see the install and unlink their own account
  // (granteeRedirect).
  const shown = tabs.filter((t) =>
    grantee
      ? t.id === "slack"
      : (t.id !== "access" && t.id !== "apps") ||
        canManage(role) ||
        tab === t.id,
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
        ) : tab === "apps" ? (
          <PartnerAppsTab />
        ) : tab === "billing" ? (
          <BillingTab />
        ) : tab === "sharing" ? (
          <SharingTab />
        ) : tab === "slack" ? (
          <SlackTab />
        ) : (
          <EnrichmentTab />
        )}
      </TabPanel>
    </Page>
  );
}

// /settings, /settings/ai, /settings/members, /settings/access,
// /settings/apps where the build offers connected apps,
// /settings/billing (where Stripe Checkout returns) and
// /settings/enrichment, /settings/slack, and /settings/sharing where the
// build has public dashboard links; only /settings, /settings/ai and
// /settings/slack in a local build.
export function SettingsRoutes() {
  return (
    <Routes>
      <Route index element={<SettingsPage tab="general" />} />
      <Route path="ai" element={<SettingsPage tab="ai" />} />
      {!LOCAL && (
        <>
          <Route path="members" element={<SettingsPage tab="members" />} />
          <Route path="access" element={<SettingsPage tab="access" />} />
          {partnerApps && (
            <Route path="apps" element={<SettingsPage tab="apps" />} />
          )}
          <Route path="billing" element={<SettingsPage tab="billing" />} />
          <Route
            path="enrichment"
            element={<SettingsPage tab="enrichment" />}
          />
        </>
      )}
      <Route path="slack" element={<SettingsPage tab="slack" />} />
      {publicLinks && (
        <Route path="sharing" element={<SettingsPage tab="sharing" />} />
      )}
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}

// /slack/installed, where Slack returns from adding Helix Foundry (hosted:
// Helix finishes a local install's), and /slack/link, the link it sends a
// person in Slack.
export function SlackRoutes() {
  return (
    <Routes>
      {!LOCAL && <Route path="installed" element={<SlackInstalledPage />} />}
      <Route path="link" element={<SlackLinkPage />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}

// /relay/link (hosted): a local Foundry install asks to be linked to one of
// the caller's workspaces (main.tsx mounts it outside a local build).
export function RelayRoutes() {
  return (
    <Routes>
      <Route path="link" element={<RelayLinkPage />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}

// /oauth/authorize (hosted, where the build offers connected apps): a
// partner app asks to read one of the caller's workspaces (main.tsx mounts
// it, while onboarding too).
export function OAuthRoutes() {
  return (
    <Routes>
      <Route path="authorize" element={<PartnerAuthorizePage />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
