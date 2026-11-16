import { useQuery, type QueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import type {
  ApproveAutomationRequest,
  AutomationResponse,
  AutomationRunResponse,
  CreateAutomationRequest,
  FoundryAutomation,
  ListAutomationRunsResponse,
  ListAutomationSuggestionsResponse,
  ListAutomationsResponse,
  RunAutomationRequest,
  UpdateAutomationRequest,
} from "../../../shared/wfe";
import { api, refusalOf } from "../../api";
import { trackEvent, type AutomationCall } from "../../analytics";
import { connectorsQuery } from "../data/data-api";
import { documentSetQueries } from "../documents/documents-api";
import { setLabel } from "../documents/model";
import { dashboardQueries } from "../dashboards/queries";
import { ontologyQueries } from "../ontology/queries";
import {
  ASSESSED_WITHIN_MS,
  assessing,
  automationRefusal,
  canonicalJson,
  isLiveRun,
  parseSpec,
  placeNodes,
  type Names,
  type Places,
  type TemplateId,
} from "./model";

// The automations' WFE calls (src/shared/wfe.ts "Automations"), and the
// analytics each reports once the WFE accepts it, or why it refused: never
// a name, an instruction or a target.

const enc = encodeURIComponent;
const at = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry/automations`;

/** A live run's page, and an automation whose run is live, read again this often. */
export const RUN_POLL_MS = 1500;
/** An active automation's page, whose trigger may fire a run any time. */
export const ACTIVE_POLL_MS = 5000;
/** Suggestions the Assistant is writing, read again this often. */
export const SUGGESTIONS_POLL_MS = 2500;

export const automationQueries = {
  // Every automation of the workspace (at most 50), each with its newest
  // run, without specs; read again while a run is live.
  list: (ws: string) => ({
    queryKey: [ws, "automations"],
    queryFn: () => api<ListAutomationsResponse>(at(ws)),
    refetchInterval: (q: { state: { data?: ListAutomationsResponse } }) =>
      q.state.data?.automations?.some((a) => isLiveRun(a.lastRun))
        ? RUN_POLL_MS
        : false,
  }),
  // The overview's: every automation with its runs of the last `days` days
  // (by the days of `zone`) and their sum by day. Read again while one is
  // live (a trigger may fire) or a run is, never as often as a run's page:
  // each read also counts the runs.
  overview: (ws: string, days: number, zone: string) => ({
    queryKey: [ws, "automations", { days, zone }],
    queryFn: () =>
      api<ListAutomationsResponse>(
        `${at(ws)}?${new URLSearchParams({ statsDays: String(days), timeZone: zone })}`,
      ),
    refetchInterval: (q: { state: { data?: ListAutomationsResponse } }) =>
      q.state.data?.automations?.some(
        (a) => a.status === "active" || isLiveRun(a.lastRun),
      )
        ? ACTIVE_POLL_MS
        : false,
  }),
  // One automation with its spec and revision; read again while a run of
  // it is live, for a minute after one ended (its ontologies' assessments
  // come in), and while it is active (a trigger may fire).
  automation: (ws: string, id: string) => ({
    queryKey: [ws, "automations", id],
    queryFn: () => api<AutomationResponse>(`${at(ws)}/${enc(id)}`),
    refetchInterval: (q: { state: { data?: AutomationResponse } }) => {
      const a = q.state.data?.automation,
        ended = Date.parse(a?.lastRun?.finishedAt || "");
      return isLiveRun(a?.lastRun) || Date.now() - ended < ASSESSED_WITHIN_MS
        ? RUN_POLL_MS
        : a?.status === "active"
          ? ACTIVE_POLL_MS
          : false;
    },
  }),
  // Its runs, newest first: the first page (50).
  runs: (ws: string, id: string) => ({
    queryKey: [ws, "automations", id, "runs"],
    queryFn: () => api<ListAutomationRunsResponse>(`${at(ws)}/${enc(id)}/runs`),
  }),
  // One run with its steps; read again until it ends.
  run: (ws: string, id: string, runId: string) => ({
    queryKey: [ws, "automations", id, "runs", runId],
    queryFn: () =>
      api<AutomationRunResponse>(`${at(ws)}/${enc(id)}/runs/${enc(runId)}`),
    // Read again while it is live, and until the ontologies it used are
    // assessed.
    refetchInterval: (q: { state: { data?: AutomationRunResponse } }) =>
      isLiveRun(q.state.data?.run) || assessing(q.state.data?.run)
        ? RUN_POLL_MS
        : false,
  }),
  // The new automation page's suggestions for an ontology's current
  // version; read again while the Assistant writes them.
  suggestions: (ws: string, ontologyId: string) => ({
    queryKey: [ws, "automations", "suggestions", ontologyId],
    queryFn: () =>
      api<ListAutomationSuggestionsResponse>(
        `/v1/workspaces/${enc(ws)}/foundry/automation-suggestions?${new URLSearchParams({ ontologyId })}`,
      ),
    refetchInterval: (q: {
      state: { data?: ListAutomationSuggestionsResponse };
    }) => (q.state.data?.generating ? SUGGESTIONS_POLL_MS : false),
  }),
};

/** Puts a changed automation where its page reads it, and has the lists read again. */
export function keepAutomation(
  qc: QueryClient,
  ws: string,
  a: FoundryAutomation,
) {
  qc.setQueryData(automationQueries.automation(ws, a.id).queryKey, {
    automation: a,
  } satisfies AutomationResponse);
  void qc.invalidateQueries({ queryKey: [ws, "automations"] });
}

/** A call that reports its action once accepted, and why it was refused. */
async function reported<T>(
  action: AutomationCall,
  call: () => Promise<T>,
): Promise<T> {
  return call().catch((e: unknown) => {
    trackEvent("automation_refused", { action, reason: refusalOf(e) });
    throw e;
  });
}

/** Makes an automation, waiting for its owner's approval; the same client token returns the one made. */
export async function createAutomation(
  ws: string,
  body: CreateAutomationRequest,
  template: TemplateId,
): Promise<FoundryAutomation> {
  const { automation } = await reported("create", () =>
    api<AutomationResponse>(at(ws), body),
  );
  trackEvent("automation_created", { template });
  return automation;
}

/** Changes an automation as it was at `expectedRevision`; a new spec waits for approval again unless only its layout moved. */
export async function updateAutomation(
  ws: string,
  id: string,
  body: UpdateAutomationRequest,
): Promise<FoundryAutomation> {
  const { automation } = await reported("edit", () =>
    api<AutomationResponse>(`${at(ws)}/${enc(id)}`, body, "PATCH"),
  );
  trackEvent("automation_edited", { steps: body.specJson !== undefined });
  return automation;
}

/**
 * Steps moved on the canvas, saved on their own: `places` put onto the
 * automation as saved (the newest its page has read), so only where its
 * steps are drawn changes and the WFE keeps its approval (it compares the
 * steps without their places). Never a draft's other changes: those wait
 * for Review and save. Changed elsewhere meanwhile, it reads it again and
 * puts the places onto that, once. Resolves to the automation saved and the
 * revision the places went onto, or undefined when they are its places
 * already (or its steps can't be read). Reports no analytics: a drag is no
 * edit.
 */
export async function saveLayout(
  qc: QueryClient,
  ws: string,
  id: string,
  places: Places,
): Promise<{ automation: FoundryAutomation; onto: string } | undefined> {
  const key = automationQueries.automation(ws, id).queryKey;
  const place = async (a: FoundryAutomation) => {
    const read = parseSpec(a.specJson || "");
    if (!read.ok) return undefined;
    const specJson = canonicalJson(placeNodes(read.spec, places));
    if (specJson === canonicalJson(read.spec)) return undefined;
    const { automation } = await api<AutomationResponse>(
      `${at(ws)}/${enc(id)}`,
      {
        expectedRevision: a.revision || "",
        specJson,
      } satisfies UpdateAutomationRequest,
      "PATCH",
    );
    // Its lists hold no places: only its page reads it again.
    qc.setQueryData(key, { automation } satisfies AutomationResponse);
    return { automation, onto: a.revision || "" };
  };
  const reread = async () => {
    const fresh = await api<AutomationResponse>(`${at(ws)}/${enc(id)}`);
    qc.setQueryData(key, fresh);
    return fresh.automation;
  };
  const known = qc.getQueryData<AutomationResponse>(key)?.automation;
  try {
    return await place(known ?? (await reread()));
  } catch (e) {
    if (automationRefusal(e) !== "FOUNDRY_AUTOMATION_CHANGED") throw e;
    return place(await reread());
  }
}

/** Its owner's approval of the spec they read, at `expectedRevision`: it becomes active. */
export async function approveAutomation(
  ws: string,
  id: string,
  expectedRevision: string,
): Promise<FoundryAutomation> {
  const { automation } = await reported("approve", () =>
    api<AutomationResponse>(`${at(ws)}/${enc(id)}:approve`, {
      expectedRevision,
    } satisfies ApproveAutomationRequest),
  );
  trackEvent("automation_approved");
  return automation;
}

/** Pauses or resumes an automation. */
export async function pauseAutomation(
  ws: string,
  id: string,
  pause: boolean,
): Promise<FoundryAutomation> {
  const { automation } = await reported(pause ? "pause" : "resume", () =>
    api<AutomationResponse>(
      `${at(ws)}/${enc(id)}:${pause ? "pause" : "resume"}`,
      {},
    ),
  );
  trackEvent(pause ? "automation_paused" : "automation_resumed");
  return automation;
}

/** Runs it now, by hand; the same client token returns the run it queued. */
export async function runAutomation(
  ws: string,
  id: string,
  clientToken: string,
) {
  const { run } = await reported("run", () =>
    api<AutomationRunResponse>(`${at(ws)}/${enc(id)}:run`, {
      clientToken,
    } satisfies RunAutomationRequest),
  );
  trackEvent("automation_run_started");
  return run;
}

/** Deletes an automation, its runs and their steps. */
export async function deleteAutomation(ws: string, id: string) {
  await reported("delete", () =>
    api(`${at(ws)}/${enc(id)}`, undefined, "DELETE"),
  );
  trackEvent("automation_deleted");
}

/** Cancels a live run, with the step it was on. */
export async function cancelAutomationRun(
  ws: string,
  id: string,
  runId: string,
) {
  const { run } = await reported("cancel", () =>
    api<AutomationRunResponse>(
      `${at(ws)}/${enc(id)}/runs/${enc(runId)}:cancel`,
      {},
    ),
  );
  trackEvent("automation_run_cancelled");
  return run;
}

/**
 * What the workspace's connections, ontologies, document sets and
 * dashboards are called, from the lists the other areas read (each one
 * call): a step names its targets by them. `ready` once all have answered.
 */
export function useNames(ws: string): {
  names: Names;
  ready: boolean;
  connectors: { id: string; name: string }[];
  ontologies: { id: string; name: string }[];
  sets: { id: string; name: string }[];
  dashboards: { id: string; name: string; published: boolean }[];
} {
  const connectors = useQuery(connectorsQuery(ws)),
    ontologies = useQuery(ontologyQueries.list(ws)),
    sets = useQuery(documentSetQueries.list(ws)),
    dashboards = useQuery(dashboardQueries.list(ws));
  const ready =
    !connectors.isPending &&
    !ontologies.isPending &&
    !sets.isPending &&
    !dashboards.isPending;
  // Kept while the lists' data are the same (a poll that read nothing new
  // keeps its data), so a canvas the names feed isn't laid out again on
  // every render: it would drop the step being dragged.
  return useMemo(() => {
    const lists = {
      connectors: (connectors.data?.connectors || []).map((c) => ({
        id: c.id,
        name: c.name || c.kind || c.id,
      })),
      ontologies: (ontologies.data?.ontologies || []).map((o) => ({
        id: o.id,
        name: o.name || o.id,
      })),
      sets: (sets.data || []).map((s) => ({ id: s.id, name: setLabel(s) })),
      dashboards: (dashboards.data?.dashboards || []).map((d) => ({
        id: d.id,
        name: d.name || d.id,
        published: !!d.share,
      })),
    };
    const of = (list: { id: string; name: string }[]) => (id: string) =>
      list.find((x) => x.id === id)?.name;
    return {
      ...lists,
      names: {
        connector: of(lists.connectors),
        ontology: of(lists.ontologies),
        set: of(lists.sets),
        dashboard: of(lists.dashboards),
      },
      ready,
    };
  }, [connectors.data, ontologies.data, sets.data, dashboards.data, ready]);
}
