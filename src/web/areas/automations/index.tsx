// Automations area: a person's graphs of steps Foundry runs as them when a
// schedule or an event fires (backend/automation), lazy-loaded by main.tsx's
// route table under /automations: the overview (the workspace's runs of the
// last days and its automations), a new one (the Assistant's conversation,
// or a template's draft on the canvas), and an automation on its full-page
// canvas, its runs coloured there. Every member sees every automation; what
// each may do the WFE says per automation.
import "./automations.css";
import { Suspense, lazy } from "react";
import { Link, Route, Routes, useNavigate } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Plus, Zap } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import type { FoundryAutomation } from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Section,
  Segmented,
  SkeletonText,
  StatusBadge,
  formatNumber,
  formatRelative,
  useNow,
  useStoredState,
} from "../../kit";
import { RefreshFailed, useWorkspace } from "../../ui";
import { membersQuery } from "../workspace/members";
import { AutomationPage } from "./editor";
import { automationsUnavailable, listTriggerText, usdText } from "./model";
import { NewAutomationPage } from "./new";
import { RUN_STATUS, Templates, automationStatus, here } from "./parts";
import { automationPaths as paths } from "./paths";
import { automationQueries, useNames } from "./queries";

const RunsChart = lazy(() => import("./runs-chart"));

export function AutomationsRoutes() {
  return (
    <Routes>
      <Route index element={<Overview />} />
      <Route path="new" element={<NewAutomationPage />} />
      <Route path=":automationId" element={<AutomationPage />} />
      <Route path=":automationId/runs/:runId" element={<AutomationPage />} />
    </Routes>
  );
}

const WINDOWS = [
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
];

/**
 * Who made an automation, in words: you, a member by their name (the
 * members list's first page, which hosted Foundry reads; a local build has
 * one owner), else another member.
 */
function useOwners() {
  const { id: ws, user } = useWorkspace();
  const members = useInfiniteQuery({ ...membersQuery(ws), enabled: !LOCAL });
  const names = new Map(
    (members.data?.pages[0]?.members || []).map((m) => [
      m.user?.id || "",
      m.user?.displayName || m.user?.email || "",
    ]),
  );
  return (id?: string) =>
    !id || id === user.id ? "You" : names.get(id) || "Another member";
}

/** One figure of the runs: what it counts, its number, and more in words. */
function Stat({
  label,
  value,
  note,
  tone,
}: {
  label: string;
  value: string;
  note?: string;
  tone?: "failed" | "waiting";
}) {
  return (
    <div className={tone ? `au-stat is-${tone}` : "au-stat"}>
      <dt className="au-stat-label">{label}</dt>
      <dd className="au-stat-value">{value}</dd>
      {note && <dd className="au-stat-note">{note}</dd>}
    </div>
  );
}

/**
 * /automations: the workspace's runs of the last 7 or 30 days (how many
 * succeeded, failed, wait for someone, what they spent, and by day), then
 * its automations; a row opens one on its canvas. With none yet, the
 * templates to start from.
 */
function Overview() {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate(),
    now = useNow(60_000),
    lists = useNames(ws),
    ownerOf = useOwners();
  const [windowDays, setWindowDays] = useStoredState(
    "foundry.automations.days",
    "7",
  );
  const days = windowDays === "30" ? 30 : 7;
  const query = useQuery(automationQueries.overview(ws, days, here()));
  const header = (
    <PageHeader
      title="Automations"
      subtitle="Steps Foundry runs for you on a schedule, or when a sync, a load or new documents arrive: syncs, loads, the Assistant’s checks, and emails."
      actions={
        query.data?.canCreate && (
          <ButtonLink to={paths.newAutomation()} variant="primary" icon={Plus}>
            New automation
          </ButtonLink>
        )
      }
    />
  );
  if (query.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={6} />
      </Page>
    );
  if (!query.data)
    return (
      <Page>
        {header}
        {automationsUnavailable(query.error) ? (
          <EmptyState
            icon={Zap}
            title="Automations aren’t available here"
            text="They are off for this Foundry."
          />
        ) : (
          <EmptyState
            icon={Zap}
            title="Couldn’t load the automations"
            text={query.error?.message}
            action={<Button onClick={() => void query.refetch()}>Retry</Button>}
          />
        )}
      </Page>
    );
  const automations = query.data.automations || [],
    summary = query.data.summary,
    totals = summary?.totals || {};
  if (!automations.length)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Zap}
          title="No automations yet"
          text="Describe what should happen and the Assistant sets it up, or start from a template. Nothing runs until you approve it."
          action={
            query.data.canCreate && (
              <ButtonLink
                to={paths.newAutomation()}
                variant="primary"
                icon={Plus}
              >
                New automation
              </ButtonLink>
            )
          }
        />
        <Section
          title="Start from a template"
          description="Pick one close to what you want; you can change every step before it runs."
          className="au-templates-section"
        >
          <Templates lists={lists} />
        </Section>
      </Page>
    );
  const waiting = totals.waiting || 0;
  return (
    <Page>
      {header}
      <RefreshFailed query={query} />
      <section className="au-overview" aria-label="Runs">
        <div className="au-overview-head">
          <h2>Runs</h2>
          <Segmented
            label="Runs of the last"
            value={String(days)}
            options={WINDOWS}
            onChange={setWindowDays}
          />
        </div>
        <dl className="au-stats">
          <Stat
            label="Runs"
            value={formatNumber(totals.runs || 0)}
            note={`in the last ${days} days`}
          />
          <Stat
            label="Succeeded"
            value={formatNumber(totals.succeeded || 0)}
            note={
              totals.runs
                ? `${Math.round(((totals.succeeded || 0) / totals.runs) * 100)}% of runs`
                : undefined
            }
          />
          <Stat
            label="Failed"
            value={formatNumber(totals.failed || 0)}
            tone={totals.failed ? "failed" : undefined}
          />
          <Stat
            label="Waiting for someone"
            value={formatNumber(waiting)}
            note="actions its owner runs or dismisses"
            tone={waiting ? "waiting" : undefined}
          />
          <Stat
            label="Spend"
            value={usdText(totals.usdMicros)}
            note="by its Assistant steps"
          />
        </dl>
        {!!summary?.byDay?.length && (
          <Suspense fallback={<div className="au-chart" aria-busy="true" />}>
            <RunsChart days={summary.byDay} />
          </Suspense>
        )}
      </section>
      <DataGrid<FoundryAutomation>
        className="au-grid"
        label="Automations"
        rows={automations}
        getRowKey={(a) => a.id}
        onRowClick={(a) => navigate(paths.automation(a.id))}
        rowHeight={60}
        columns={[
          {
            key: "name",
            label: "Automation",
            type: "string",
            render: (a) => (
              <span className="au-list-name">
                <Link className="au-list-link" to={paths.automation(a.id)}>
                  {a.name}
                </Link>
                <span className="au-muted">
                  {listTriggerText(a.triggerKind, a.triggerTarget, lists.names)}
                </span>
              </span>
            ),
          },
          {
            key: "status",
            label: "Status",
            type: "string",
            width: 190,
            sortValue: (a) => automationStatus(a).label,
            render: (a) => {
              const s = automationStatus(a);
              return <StatusBadge size="sm" status={s.dot} label={s.label} />;
            },
          },
          {
            key: "runs",
            label: "Runs",
            type: "number",
            width: 80,
            sortValue: (a) => a.stats?.runs || 0,
            render: (a) => formatNumber(a.stats?.runs || 0),
          },
          {
            key: "lastRun",
            label: "Last run",
            type: "timestamp",
            width: 190,
            sortValue: (a) => a.lastRun?.createdAt || "",
            render: (a) =>
              a.lastRun ? (
                <span className="au-list-run">
                  <StatusBadge
                    size="sm"
                    status={RUN_STATUS[a.lastRun.status || "queued"].status}
                    label={RUN_STATUS[a.lastRun.status || "queued"].label}
                  />
                  <span className="au-muted">
                    {a.lastRun.createdAt
                      ? formatRelative(a.lastRun.createdAt, now)
                      : ""}
                  </span>
                </span>
              ) : (
                <span className="au-muted">Never</span>
              ),
          },
          {
            key: "lastFailedAt",
            label: "Last failed run",
            type: "timestamp",
            width: 140,
            sortValue: (a) => a.lastFailedAt || "",
            render: (a) =>
              a.lastFailedAt ? (
                formatRelative(a.lastFailedAt, now)
              ) : (
                <span className="au-muted">—</span>
              ),
          },
          {
            key: "owner",
            label: "Created by",
            type: "string",
            width: 150,
            sortValue: (a) => ownerOf(a.ownerUserId),
            render: (a) => ownerOf(a.ownerUserId),
          },
          {
            key: "updatedAt",
            label: "Edited",
            type: "timestamp",
            width: 120,
            sortValue: (a) => a.updatedAt || "",
            render: (a) =>
              a.updatedAt ? formatRelative(a.updatedAt, now) : "—",
          },
        ]}
      />
    </Page>
  );
}
