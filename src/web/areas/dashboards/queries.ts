import type {
  AddDashboardQuestionVisualRequest,
  CreateDashboardRequest,
  DashboardLinkExpiry,
  DashboardResponse,
  DashboardSpec,
  FoundryDashboard,
  FoundryDashboardSettings,
  FoundryDashboardSettingsResponse,
  ListDashboardsResponse,
  PinDashboardRequest,
  PublishDashboardRequest,
  UpdateDashboardRequest,
  UpdateDashboardSettingsRequest,
} from "../../../shared/wfe";
import type { QueryClient } from "@tanstack/react-query";
import { api, refusalOf } from "../../api";
import { trackEvent, type DashboardAction } from "../../analytics";
import { readGraph } from "../explore/graph-read";
import { tileDraw, type LoadRead, type TileDraw } from "./model";

// The dashboards' WFE calls (src/shared/wfe.ts "Dashboards"), and the
// analytics each reports once the WFE accepts it, or why it refused: never
// a name, a title or a query. A tile is read as the viewer through its
// load's :read (explore/graph-read.ts readGraph), the tiles of one load in
// one call, cached under the ontology's reads so a job's end refreshes them.

const enc = encodeURIComponent;
const at = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry/dashboards`;
const settingsAt = (ws: string) =>
  `/v1/workspaces/${enc(ws)}/foundry/dashboard-settings`;

export const dashboardQueries = {
  // The workspace's dashboard settings: whether public links are on.
  settings: (ws: string) => ({
    queryKey: [ws, "dashboard-settings"],
    queryFn: async (): Promise<FoundryDashboardSettings> =>
      (await api<FoundryDashboardSettingsResponse>(settingsAt(ws))).settings ||
      {},
  }),
  // Every dashboard the caller sees (at most 200), pinned first, without specs.
  list: (ws: string) => ({
    queryKey: [ws, "dashboards"],
    queryFn: () => api<ListDashboardsResponse>(at(ws)),
  }),
  // One dashboard with its spec and revision.
  dashboard: (ws: string, id: string) => ({
    queryKey: [ws, "dashboards", id],
    queryFn: () => api<DashboardResponse>(`${at(ws)}/${enc(id)}`),
  }),
  // The tiles of one load, read once: each tile's drawing, by id.
  read: (ws: string, read: LoadRead) => ({
    queryKey: [
      ws,
      "ontology",
      read.ontologyId,
      "read",
      read.loadId,
      "dashboard",
      read.tiles.map((t) => [t.id, t.query]),
    ],
    queryFn: async ({
      signal,
    }: {
      signal?: AbortSignal;
    }): Promise<Map<string, TileDraw>> => {
      const outcomes = await readGraph(
        ws,
        read.ontologyId,
        read.loadId,
        read.tiles.map((t) => ({ query: t.query })),
        { signal },
      );
      return new Map(
        read.tiles.map((t, i) => [t.id, tileDraw(outcomes[i], t.query)]),
      );
    },
    retry: false,
    staleTime: 30_000,
  }),
};

/** Puts a changed dashboard where every page reads it, and has the list read again. */
export function keepDashboard(
  qc: QueryClient,
  ws: string,
  d: FoundryDashboard,
) {
  qc.setQueryData(dashboardQueries.dashboard(ws, d.id).queryKey, {
    dashboard: d,
  } satisfies DashboardResponse);
  void qc.invalidateQueries({
    queryKey: dashboardQueries.list(ws).queryKey,
    exact: true,
  });
}

/** A call that reports its action once accepted, and why it was refused. */
async function reported<T>(
  action: DashboardAction,
  call: () => Promise<T>,
): Promise<T> {
  return call().catch((e: unknown) => {
    trackEvent("dashboard_refused", { action, reason: refusalOf(e) });
    throw e;
  });
}

/** Makes a dashboard; the same client token returns the one made. */
export async function createDashboard(
  ws: string,
  body: CreateDashboardRequest,
  from: "analyst",
): Promise<FoundryDashboard> {
  const { dashboard } = await reported("create", () =>
    api<DashboardResponse>(at(ws), body),
  );
  trackEvent("dashboard_created", { from });
  return dashboard;
}

/** Changes a dashboard as it was at `expectedRevision`: its name, description or spec. */
export async function updateDashboard(
  ws: string,
  id: string,
  body: Omit<UpdateDashboardRequest, "specJson"> & { spec?: DashboardSpec },
): Promise<FoundryDashboard> {
  const { spec, ...rest } = body;
  const { dashboard } = await reported("edit", () =>
    api<DashboardResponse>(
      `${at(ws)}/${enc(id)}`,
      {
        ...rest,
        ...(spec ? { specJson: JSON.stringify(spec) } : {}),
      } satisfies UpdateDashboardRequest,
      "PATCH",
    ),
  );
  trackEvent("dashboard_edited", { tiles: !!spec });
  return dashboard;
}

/** Deletes a dashboard and its public link. */
export async function deleteDashboard(ws: string, id: string) {
  await reported("delete", () =>
    api(`${at(ws)}/${enc(id)}`, undefined, "DELETE"),
  );
  trackEvent("dashboard_deleted");
}

/** Pins or unpins a dashboard for the whole workspace (owners and admins). */
export async function pinDashboard(
  ws: string,
  id: string,
  pinned: boolean,
  expectedRevision: string,
): Promise<FoundryDashboard> {
  const { dashboard } = await reported("pin", () =>
    api<DashboardResponse>(`${at(ws)}/${enc(id)}:pin`, {
      pinned,
      expectedRevision,
    } satisfies PinDashboardRequest),
  );
  trackEvent(pinned ? "dashboard_pinned" : "dashboard_unpinned");
  return dashboard;
}

/** An Analyst answer's visual as a tile of its own, last. */
export async function addQuestionVisual(
  ws: string,
  id: string,
  body: AddDashboardQuestionVisualRequest,
): Promise<FoundryDashboard> {
  const { dashboard } = await reported("add_visual", () =>
    api<DashboardResponse>(`${at(ws)}/${enc(id)}:add-question-visual`, body),
  );
  trackEvent("dashboard_visual_added", {
    load: body.pinLoad ? "pinned" : "current",
  });
  return dashboard;
}

/**
 * Publishes the dashboard as it is at `expectedRevision`: a snapshot at its
 * public link, ending as `expiry` says (none keeps a live link's own).
 */
export async function publishDashboard(
  ws: string,
  id: string,
  expectedRevision: string,
  first: boolean,
  expiry?: DashboardLinkExpiry,
): Promise<FoundryDashboard> {
  const { dashboard } = await reported("publish", () =>
    api<DashboardResponse>(`${at(ws)}/${enc(id)}:publish`, {
      expectedRevision,
      ...(expiry ? { expiry } : {}),
    } satisfies PublishDashboardRequest),
  );
  trackEvent("dashboard_published", { first });
  return dashboard;
}

/** Takes the public link down: the snapshot is deleted, and the link shows nothing. */
export async function unpublishDashboard(
  ws: string,
  id: string,
): Promise<FoundryDashboard> {
  const { dashboard } = await reported("unpublish", () =>
    api<DashboardResponse>(`${at(ws)}/${enc(id)}:unpublish`, {}),
  );
  trackEvent("dashboard_unpublished");
  return dashboard;
}

/** Turns the workspace's public dashboard links on or off (owners only). */
export async function setPublicLinks(
  ws: string,
  publicLinks: boolean,
): Promise<FoundryDashboardSettings> {
  const { settings } = await api<FoundryDashboardSettingsResponse>(
    settingsAt(ws),
    { publicLinks } satisfies UpdateDashboardSettingsRequest,
    "PATCH",
  );
  trackEvent("dashboard_public_links_set", { on: publicLinks });
  return settings || {};
}
