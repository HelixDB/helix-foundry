// Automations area: a person's graphs of steps Foundry runs as them when a
// schedule or an event fires (backend/automation), lazy-loaded by main.tsx's
// route table under /automations: the list and the templates a new one
// starts from, the builder, an automation's page (its state, approval and
// runs) and a run's page (each step's outcome on the canvas). Every member
// sees every automation; what each may do the WFE says per automation.
import "./automations.css";
import { Link, Route, Routes, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Plus, Zap } from "lucide-react";
import type { FoundryAutomation } from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Section,
  SkeletonText,
  StatusBadge,
  formatDate,
  formatRelative,
  useNow,
} from "../../kit";
import { RefreshFailed, useWorkspace } from "../../ui";
import { AutomationPage } from "./automation";
import { BuilderPage } from "./builder";
import {
  TEMPLATES,
  automationRefusal,
  listTriggerText,
  type Template,
} from "./model";
import { RUN_STATUS, automationStatus } from "./parts";
import { automationPaths as paths } from "./paths";
import { automationQueries, useNames } from "./queries";
import { RunPage } from "./run";

export function AutomationsRoutes() {
  return (
    <Routes>
      <Route index element={<AutomationsPage />} />
      <Route path="new" element={<BuilderPage />} />
      <Route path=":automationId" element={<AutomationPage />} />
      <Route path=":automationId/edit" element={<BuilderPage />} />
      <Route path=":automationId/runs/:runId" element={<RunPage />} />
    </Routes>
  );
}

const NEEDS: Record<Template["needs"][number], string> = {
  connector: "a connection",
  ontology: "an ontology",
  set: "a document set",
};

/** The templates a new automation starts from, each saying what it needs that the workspace lacks. */
function Templates({ lists }: { lists: ReturnType<typeof useNames> }) {
  const has = {
    connector: !!lists.connectors.length,
    ontology: !!lists.ontologies.length,
    set: !!lists.sets.length,
  };
  return (
    <ul className="au-templates" aria-label="Templates">
      {TEMPLATES.map((t) => {
        const lacks = t.needs.filter((n) => !has[n]);
        return (
          <li key={t.id}>
            <Link className="au-template" to={paths.newAutomation(t.id)}>
              <strong>{t.title}</strong>
              <span>{t.description}</span>
              {lists.ready && !!lacks.length && (
                <small>
                  Needs {lacks.map((n) => NEEDS[n]).join(" and ")}, which this
                  workspace hasn’t yet.
                </small>
              )}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** /automations: the workspace's automations, and the templates a new one starts from. */
function AutomationsPage() {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate(),
    now = useNow(60_000),
    lists = useNames(ws);
  const list = useQuery(automationQueries.list(ws));
  const header = (
    <PageHeader
      title="Automations"
      subtitle="Steps Foundry runs for you on a schedule, or when a sync, a load or new documents arrive: syncs, loads, the Assistant’s checks, and emails."
      actions={
        list.data?.canCreate && (
          <ButtonLink to={paths.newAutomation()} variant="primary" icon={Plus}>
            New automation
          </ButtonLink>
        )
      }
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
        {automationRefusal(list.error) === "FOUNDRY_AUTOMATIONS_DISABLED" ? (
          <EmptyState
            icon={Zap}
            title="Automations aren’t available here"
            text="They are off for this Foundry."
          />
        ) : (
          <EmptyState
            icon={Zap}
            title="Couldn’t load the automations"
            text={list.error?.message}
            action={<Button onClick={() => void list.refetch()}>Retry</Button>}
          />
        )}
      </Page>
    );
  const automations = list.data.automations || [];
  return (
    <Page>
      {header}
      <RefreshFailed query={list} />
      {!!automations.length && (
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
                    {listTriggerText(
                      a.triggerKind,
                      a.triggerTarget,
                      lists.names,
                    )}
                  </span>
                </span>
              ),
            },
            {
              key: "status",
              label: "State",
              type: "string",
              width: 190,
              sortValue: (a) => automationStatus(a).label,
              render: (a) => {
                const s = automationStatus(a);
                return <StatusBadge size="sm" status={s.dot} label={s.label} />;
              },
            },
            {
              key: "lastRun",
              label: "Last run",
              type: "timestamp",
              width: 200,
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
              key: "nextRunAt",
              label: "Next run",
              type: "timestamp",
              width: 170,
              sortValue: (a) => a.nextRunAt || "",
              render: (a) =>
                a.nextRunAt ? (
                  formatDate(a.nextRunAt)
                ) : (
                  <span className="au-muted">—</span>
                ),
            },
          ]}
        />
      )}
      <Section
        title={
          automations.length
            ? "Start another from a template"
            : "Start from a template"
        }
        description={
          automations.length
            ? undefined
            : "Pick one close to what you want; you can change every step before it runs. Or ask the Assistant to set one up for you."
        }
        className="au-templates-section"
      >
        <Templates lists={lists} />
      </Section>
    </Page>
  );
}
