import { Suspense, lazy, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Check,
  MoreHorizontal,
  Pause,
  Pencil,
  Play,
  Trash2,
} from "lucide-react";
import type {
  FoundryAutomation,
  FoundryAutomationRun,
  ListAutomationRunsResponse,
} from "../../../shared/wfe";
import { ApiError, api, sentence as asSentence } from "../../api";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  DataGrid,
  DropdownMenu,
  EmptyState,
  KeyValue,
  MenuItem,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  StatusBadge,
  TabPanel,
  Tabs,
  formatDate,
  formatRelative,
  plural,
  useNow,
  useTabParam,
} from "../../kit";
import { RefreshFailed, useWorkspace } from "../../ui";
import {
  eventText,
  isLiveRun,
  parseEvent,
  parseSpec,
  runUsd,
  sentence,
  stoppedText,
  triggerOf,
  triggerText,
  type Names,
} from "./model";
import { RUN_STATUS, StepsPreview, automationStatus, here } from "./parts";
import { automationPaths as paths } from "./paths";
import {
  ACTIVE_POLL_MS,
  RUN_POLL_MS,
  approveAutomation,
  automationQueries,
  deleteAutomation,
  keepAutomation,
  pauseAutomation,
  runAutomation,
  useNames,
} from "./queries";

// One automation: what it does in a sentence, why it isn't running (it
// waits for its owner's approval, or paused, by a person or after failing
// three times in a row), what it does unattended, and its runs. Its owner
// approves it here (what they read is what runs), runs it now, edits and
// resumes it; they, or an owner or admin, pause and delete it.

const AutomationCanvas = lazy(() => import("./canvas"));

/** A run's time, from start to end, in words. */
const took = (r: FoundryAutomationRun) => {
  const from = Date.parse(r.startedAt || ""),
    to = Date.parse(r.finishedAt || "");
  if (!Number.isFinite(from) || !Number.isFinite(to)) return "—";
  const s = Math.max(0, Math.round((to - from) / 1000));
  return s < 60 ? `${s} s` : `${Math.round(s / 60)} min`;
};

export function AutomationPage() {
  const { automationId = "" } = useParams(),
    { id: ws, user } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    now = useNow(30_000),
    lists = useNames(ws);
  const query = useQuery(automationQueries.automation(ws, automationId));
  const email = useQuery(automationQueries.list(ws)).data?.email ?? true;
  const a = query.data?.automation;
  const runs = useInfiniteQuery({
    queryKey: automationQueries.runs(ws, automationId).queryKey,
    queryFn: ({ pageParam }) =>
      api<ListAutomationRunsResponse>(
        `/v1/workspaces/${encodeURIComponent(ws)}/foundry/automations/${encodeURIComponent(automationId)}/runs` +
          (pageParam ? `?pageToken=${encodeURIComponent(pageParam)}` : ""),
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextPageToken || undefined,
    refetchInterval: isLiveRun(a?.lastRun)
      ? RUN_POLL_MS
      : a?.status === "active"
        ? ACTIVE_POLL_MS
        : false,
  });
  const [tab, setTab] = useTabParam(["runs", "steps"]);
  const [pending, setPending] = useState<string | null>(null),
    [error, setError] = useState(""),
    [deleting, setDeleting] = useState(false),
    [token, setToken] = useState(() => crypto.randomUUID());
  const crumbs = [{ label: "Automations", to: paths.automations() }];
  if (query.isPending)
    return (
      <Page>
        <PageHeader title="Automation" breadcrumbs={crumbs} />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!a)
    return (
      <Page>
        <PageHeader title="Automation" breadcrumbs={crumbs} />
        <EmptyState
          title={
            query.error instanceof ApiError && query.error.status === 404
              ? "This automation is gone"
              : "Couldn’t load the automation"
          }
          text={query.error?.message}
          action={
            <ButtonLink to={paths.automations()}>All automations</ButtonLink>
          }
        />
      </Page>
    );
  const mine = a.ownerUserId === user.id,
    read = parseSpec(a.specJson || ""),
    status = automationStatus(a),
    stopped = stoppedText(a, mine);
  const act = async (which: string, call: () => Promise<FoundryAutomation>) => {
    setPending(which);
    setError("");
    try {
      keepAutomation(qc, ws, await call());
    } catch (e) {
      setError(asSentence((e as Error).message));
    } finally {
      setPending(null);
    }
  };
  const runNow = async () => {
    setPending("run");
    setError("");
    try {
      const run = await runAutomation(ws, a.id, token);
      setToken(crypto.randomUUID());
      void qc.invalidateQueries({ queryKey: [ws, "automations"] });
      navigate(paths.automationRun(a.id, run.id));
    } catch (e) {
      setError(asSentence((e as Error).message));
    } finally {
      setPending(null);
    }
  };
  const allRuns = runs.data?.pages.flatMap((p) => p.runs || []) || [];
  const lastFailed = allRuns.find((r) => r.status === "failed");
  return (
    <Page>
      <PageHeader
        title={a.name || "Automation"}
        subtitle={a.description || undefined}
        breadcrumbs={[...crumbs, { label: a.name || "Automation" }]}
        meta={<StatusBadge status={status.dot} label={status.label} />}
        actions={
          <>
            {a.canRun && (
              <Button
                variant="primary"
                icon={Play}
                pending={pending === "run"}
                disabled={!!pending}
                onClick={() => void runNow()}
              >
                Run now
              </Button>
            )}
            {a.canResume && (
              <Button
                icon={Play}
                pending={pending === "resume"}
                disabled={!!pending}
                onClick={() =>
                  void act("resume", () => pauseAutomation(ws, a.id, false))
                }
              >
                Turn on
              </Button>
            )}
            {a.canPause && (
              <Button
                icon={Pause}
                pending={pending === "pause"}
                disabled={!!pending}
                onClick={() =>
                  void act("pause", () => pauseAutomation(ws, a.id, true))
                }
              >
                Pause
              </Button>
            )}
            {a.canEdit && (
              <ButtonLink to={paths.editAutomation(a.id)} icon={Pencil}>
                Edit
              </ButtonLink>
            )}
            {a.canDelete && (
              <DropdownMenu
                label="More actions"
                align="end"
                triggerClassName="hf-btn hf-btn--secondary hf-btn--md hf-btn--icon"
                trigger={<MoreHorizontal size={16} aria-hidden />}
              >
                <MenuItem
                  icon={Trash2}
                  danger
                  onSelect={() => setDeleting(true)}
                >
                  Delete
                </MenuItem>
              </DropdownMenu>
            )}
          </>
        }
      />
      <RefreshFailed query={query} />
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      {stopped && (
        <div
          className={
            a.status === "needs_approval" ? "au-callout" : "au-callout is-muted"
          }
          role="status"
        >
          <p>{stopped}</p>
          {a.pausedReason === "failures" && lastFailed?.error && (
            <p>The last one failed because: {lastFailed.error}</p>
          )}
          {a.canApprove && read.ok && (
            <>
              <p>These steps run unattended, as you, each time it starts:</p>
              <StepsPreview
                spec={read.spec}
                names={lists.names}
                email={email}
                label="Steps to approve"
              />
              <Button
                variant="primary"
                icon={Check}
                pending={pending === "approve"}
                disabled={!!pending}
                onClick={() =>
                  void act("approve", () =>
                    approveAutomation(ws, a.id, a.revision || ""),
                  )
                }
              >
                Approve and turn on
              </Button>
            </>
          )}
        </div>
      )}
      {a.status === "active" &&
        a.backoffUntil &&
        Date.parse(a.backoffUntil) > now && (
          <div className="au-callout is-muted" role="status">
            <p>
              Its last{" "}
              {a.failures && a.failures > 1 ? plural(a.failures, "run") : "run"}{" "}
              failed, so it waits until {formatDate(a.backoffUntil)} before
              starting on its own again. After 3 failures in a row it pauses.
            </p>
          </div>
        )}
      <Panel className="au-summary">
        <p className="au-sentence">
          {read.ok ? sentence(read.spec, lists.names, email, here()) : ""}
        </p>
        <KeyValue
          columns={3}
          items={[
            {
              label: "Starts",
              value: read.ok
                ? capital(startsText(read.spec, lists.names))
                : "—",
            },
            {
              label: "Next run",
              value: a.nextRunAt
                ? formatDate(a.nextRunAt)
                : a.status === "active"
                  ? "When it is next started"
                  : "—",
            },
            {
              label: "Owner",
              value: mine ? "You" : "Another member",
              hint: mine ? undefined : "It runs as them.",
            },
          ]}
        />
      </Panel>
      <Tabs
        label="Automation"
        value={tab}
        onChange={setTab}
        items={[
          { id: "runs", label: "Runs" },
          { id: "steps", label: "Steps" },
        ]}
      />
      <TabPanel id="runs" value={tab}>
        {runs.isPending ? (
          <SkeletonText lines={4} />
        ) : !allRuns.length ? (
          <EmptyState
            size="sm"
            title="No runs yet"
            text={
              a.status === "active"
                ? "It runs each time it starts. You can also run it now."
                : a.canRun
                  ? "Run it now to try it."
                  : "It hasn’t run yet."
            }
          />
        ) : (
          <>
            <DataGrid<FoundryAutomationRun>
              className="au-grid"
              label="Runs"
              rows={allRuns}
              getRowKey={(r) => r.id}
              onRowClick={(r) => navigate(paths.automationRun(a.id, r.id))}
              columns={[
                {
                  key: "status",
                  label: "Outcome",
                  type: "string",
                  width: 150,
                  render: (r) => (
                    <StatusBadge
                      size="sm"
                      status={RUN_STATUS[r.status || "queued"].status}
                      label={RUN_STATUS[r.status || "queued"].label}
                    />
                  ),
                },
                {
                  key: "event",
                  label: "What started it",
                  type: "string",
                  render: (r) => (
                    <Link
                      className="au-list-link"
                      to={paths.automationRun(a.id, r.id)}
                    >
                      {eventText(
                        parseEvent(r.triggerEventJson),
                        read.ok ? triggerOf(read.spec).config : undefined,
                        lists.names,
                      )}
                    </Link>
                  ),
                },
                {
                  key: "createdAt",
                  label: "Started",
                  type: "timestamp",
                  width: 150,
                  sortValue: (r) => r.createdAt || "",
                  render: (r) =>
                    r.createdAt ? formatRelative(r.createdAt, now) : "—",
                },
                {
                  key: "took",
                  label: "Took",
                  type: "string",
                  width: 90,
                  render: took,
                },
                {
                  key: "usd",
                  label: "Cost",
                  type: "number",
                  width: 90,
                  sortValue: (r) => runUsd(r.usdMicros),
                  render: (r) => `$${runUsd(r.usdMicros).toFixed(2)}`,
                },
              ]}
            />
            {runs.hasNextPage && (
              <Button
                className="au-more"
                pending={runs.isFetchingNextPage}
                onClick={() => void runs.fetchNextPage()}
              >
                Show older runs
              </Button>
            )}
          </>
        )}
      </TabPanel>
      <TabPanel id="steps" value={tab}>
        {read.ok ? (
          <div className="au-steps">
            <Suspense fallback={<div className="au-flow" aria-busy="true" />}>
              <AutomationCanvas
                spec={read.spec}
                names={lists.names}
                email={email}
                label="Its steps on a canvas"
              />
            </Suspense>
            <StepsPreview spec={read.spec} names={lists.names} email={email} />
          </div>
        ) : (
          <p className="au-hint">Its steps can’t be read.</p>
        )}
      </TabPanel>
      {deleting && (
        <ConfirmDialog
          title={`Delete “${a.name}”?`}
          description="Its runs go with it, and a run still going stops. Nothing it already did is undone."
          confirmLabel="Delete"
          tone="danger"
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            await deleteAutomation(ws, a.id);
            setDeleting(false);
            qc.removeQueries({
              queryKey: automationQueries.automation(ws, a.id).queryKey,
            });
            await qc.invalidateQueries({ queryKey: [ws, "automations"] });
            navigate(paths.automations(), { replace: true });
          }}
        />
      )}
    </Page>
  );
}

const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
/** What starts it, in words. */
const startsText = (spec: Parameters<typeof triggerOf>[0], names: Names) => {
  const t = triggerOf(spec).config;
  return t.kind === "manual"
    ? "only when it is run by hand"
    : t.kind === "schedule"
      ? triggerText(t, names, here())
      : `when ${triggerText(t, names, here())}`;
};
