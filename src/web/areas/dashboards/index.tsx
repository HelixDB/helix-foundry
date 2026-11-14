// Dashboards area: the workspace's dashboards, each tiles of typed queries
// over ontologies' loads drawn as the Analyst draws an answer's visuals,
// lazy-loaded by main.tsx's route table under /dashboards. Every member
// sees every dashboard (a grantee only the pinned ones) and reads each tile
// as themselves; what they may change, the WFE says per dashboard.
import "./dashboards.css";
import { useEffect, useState } from "react";
import {
  Link,
  Route,
  Routes,
  useNavigate,
  useSearchParams,
} from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LayoutDashboard, Plus } from "lucide-react";
import type { FoundryDashboard } from "../../../shared/wfe";
import {
  Button,
  DataGrid,
  Dialog,
  EmptyState,
  Page,
  PageHeader,
  SkeletonText,
  Tag,
  formatRelative,
  useNow,
} from "../../kit";
import { RefreshFailed, useWorkspace } from "../../ui";
import { DashboardPage } from "./dashboard";
import { MAX_DESCRIPTION, MAX_NAME, dashboardName } from "./model";
import { dashboardPaths as paths } from "./paths";
import { createDashboard, dashboardQueries, keepDashboard } from "./queries";

export function DashboardsRoutes() {
  return (
    <Routes>
      <Route index element={<DashboardsPage />} />
      <Route path=":dashboardId" element={<DashboardPage />} />
    </Routes>
  );
}

/** /dashboards: every dashboard the viewer sees, pinned first; `?new=1` opens New dashboard. */
function DashboardsPage() {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate(),
    now = useNow(60_000),
    [params, setParams] = useSearchParams();
  const list = useQuery(dashboardQueries.list(ws));
  const [creating, setCreating] = useState(false);
  // The sidebar's New dashboard opens it on this page, open already or not.
  const asked = params.get("new") === "1";
  useEffect(() => {
    if (!asked) return;
    setCreating(true);
    setParams(
      (p) => {
        p.delete("new");
        return p;
      },
      { replace: true },
    );
  }, [asked, setParams]);
  const canCreate = !!list.data?.canCreate;
  const header = (
    <PageHeader
      title="Dashboards"
      subtitle="Tables and charts of your ontologies’ data, read as each viewer may read it."
      actions={
        canCreate && (
          <Button
            variant="primary"
            icon={Plus}
            onClick={() => setCreating(true)}
          >
            New dashboard
          </Button>
        )
      }
    />
  );
  const dialog = creating && canCreate && (
    <NewDashboardDialog
      onClose={() => setCreating(false)}
      onCreated={(d) => navigate(paths.dashboard(d.id))}
    />
  );
  if (list.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={5} />
      </Page>
    );
  if (!list.data)
    return (
      <Page>
        {header}
        <EmptyState
          icon={LayoutDashboard}
          title="Couldn’t load the dashboards"
          text={list.error?.message}
          action={<Button onClick={() => void list.refetch()}>Retry</Button>}
        />
      </Page>
    );
  const dashboards = list.data.dashboards || [];
  return (
    <Page>
      {header}
      <RefreshFailed query={list} />
      {!dashboards.length ? (
        <EmptyState
          icon={LayoutDashboard}
          title="No dashboards yet"
          text={
            canCreate
              ? "Make one here, pin a chart or table from an Analyst answer, or ask the Assistant to build one for some data."
              : "An owner or admin pins dashboards for the workspace here."
          }
        />
      ) : (
        <DataGrid<FoundryDashboard>
          className="db-grid-list"
          label="Dashboards"
          rows={dashboards}
          getRowKey={(d) => d.id}
          onRowClick={(d) => navigate(paths.dashboard(d.id))}
          rowHeight={60}
          columns={[
            {
              key: "name",
              label: "Dashboard",
              type: "string",
              render: (d) => (
                <span className="db-list-name">
                  <Link className="db-list-link" to={paths.dashboard(d.id)}>
                    {d.name}
                  </Link>
                  {d.description && (
                    <span className="db-list-muted" title={d.description}>
                      {d.description}
                    </span>
                  )}
                </span>
              ),
            },
            {
              key: "state",
              label: "Shared",
              type: "string",
              width: 200,
              sortValue: (d) => (d.pinned ? "a" : "b") + (d.share ? "a" : "b"),
              render: (d) => (
                <span className="db-list-tags">
                  {d.pinned && <Tag tone="accent">Pinned</Tag>}
                  {d.share && <Tag tone="info">Public link</Tag>}
                  {!d.pinned && !d.share && (
                    <span className="db-list-muted">—</span>
                  )}
                </span>
              ),
            },
            {
              key: "updatedAt",
              label: "Edited",
              type: "timestamp",
              width: 160,
              sortValue: (d) => d.updatedAt || "",
              render: (d) =>
                d.updatedAt ? formatRelative(d.updatedAt, now) : "—",
            },
          ]}
        />
      )}
      {dialog}
    </Page>
  );
}

/** New dashboard: a name and description; its tiles come from the Analyst or the Assistant. */
function NewDashboardDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (d: FoundryDashboard) => void;
}) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient();
  const [name, setName] = useState(""),
    [description, setDescription] = useState(""),
    [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [token] = useState(() => crypto.randomUUID());
  const valid = dashboardName(name);
  return (
    <Dialog title="New dashboard" size="sm" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (!valid) return;
          setPending(true);
          setError("");
          try {
            const d = await createDashboard(
              ws,
              {
                name: valid,
                description: description.trim(),
                clientToken: token,
              },
              "page",
            );
            keepDashboard(qc, ws, d);
            onCreated(d);
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label className="hf-field">
          <span>Name</span>
          <input
            required
            data-autofocus
            maxLength={MAX_NAME}
            placeholder="e.g. Revenue this quarter"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <label className="hf-field">
          <span>Description (optional)</span>
          <textarea
            rows={3}
            maxLength={MAX_DESCRIPTION}
            placeholder="e.g. What the sales team checks every Monday"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!valid}
          >
            Create dashboard
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
