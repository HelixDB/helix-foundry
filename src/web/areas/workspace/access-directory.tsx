import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FolderSync } from "lucide-react";
import type {
  CreateDirectoryPortalLinkRequest,
  CreateDirectoryPortalLinkResponse,
  FoundryAccessGrant,
  FoundryTeam,
  WorkspaceDirectory,
} from "../../../shared/wfe";
import { ApiError, api } from "../../api";
import { Button, Panel, Tag, formatRelative } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  directoryCounts,
  directoryView,
  issueGroups,
  stateView,
  syncText,
  syncing,
  teamsWithoutSources,
  withoutSourcesText,
} from "./directory-model";
import { accessQuery } from "./access-api";
import { DASHBOARD_URL } from "./members";

// Directory sync on Settings › Access › Teams: an owner connects the
// company's directory (Okta, Microsoft Entra ID…) through WorkOS's Admin
// Portal, each of its groups becomes a team here, and the panel says how the
// sync is doing. Admins see it without its buttons. Roles from groups stay in
// the Helix dashboard, which the panel links to. Hosted only, as Access is.

const enc = encodeURIComponent;
const base = (ws: string) => `/v1/workspaces/${enc(ws)}/directory`;

/** The workspace's directory; polls while a sync runs. */
export const directoryQuery = (ws: string) => ({
  queryKey: [ws, "directory"],
  queryFn: () => api<WorkspaceDirectory>(base(ws)),
  // A workspace that can't have one (or a WFE without the route) answers
  // 4xx: the panel isn't for it, and asking again changes nothing.
  retry: (n: number, e: unknown) =>
    !(e instanceof ApiError && e.status < 500) && n < 2,
  refetchInterval: (q: { state: { data?: WorkspaceDirectory } }) =>
    q.state.data && syncing(q.state.data) ? 5000 : false,
});

const settingsUrl = (slug: string) => `${DASHBOARD_URL}/${enc(slug)}/settings`;

export function DirectoryPanel() {
  const { id, slug, role, notify } = useWorkspace(),
    qc = useQueryClient(),
    owner = role === "owner";
  const [opening, setOpening] = useState(false),
    // Set once the setup guide opened: until a directory shows, the panel
    // reads again every 5 s, so connecting in that tab shows here.
    [awaiting, setAwaiting] = useState(false);
  const q = useQuery({
    ...directoryQuery(id),
    refetchInterval: (query: { state: { data?: WorkspaceDirectory } }) => {
      const d = query.state.data;
      return d && (syncing(d) || (awaiting && !d.directory)) ? 5000 : false;
    },
  });
  const sync = useMutation({
    mutationFn: () => api<WorkspaceDirectory>(`${base(id)}:sync`, {}),
    onSuccess: (d) => qc.setQueryData(directoryQuery(id).queryKey, d),
    onError: (e: Error) => notify(`Couldn’t start a sync. ${e.message}`, true),
  });
  // The portal link is single use and runs out in minutes, so each click
  // asks for one. The tab opens inside the click, before the link comes, so
  // a popup blocker lets it through.
  const openPortal = async () => {
    const tab = window.open("", "_blank");
    setOpening(true);
    try {
      const { link } = await api<CreateDirectoryPortalLinkResponse>(
        `${base(id)}/portal-link`,
        {
          returnUrl: window.location.href,
        } satisfies CreateDirectoryPortalLinkRequest,
      );
      if (!link) throw new Error("The setup guide gave no link.");
      if (tab) {
        tab.opener = null;
        tab.location.href = link;
      } else window.location.assign(link);
      setAwaiting(true);
    } catch (e) {
      tab?.close();
      notify(`Couldn’t open the setup guide. ${(e as Error).message}`, true);
    } finally {
      setOpening(false);
    }
  };

  // A sync that lands (the directory connected, or synced again) changes the
  // teams: read them again.
  const synced = q.data?.lastSyncedAt,
    seen = useRef(synced);
  useEffect(() => {
    if (synced === seen.current) return;
    seen.current = synced;
    void qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
  }, [synced, id, qc]);

  if (q.isPending) return null;
  if (q.isError)
    return q.error instanceof ApiError && q.error.status < 500 ? null : (
      <p className="wk-note" role="alert">
        Couldn’t read directory sync. {(q.error as Error).message}
      </p>
    );
  const d = q.data,
    view = directoryView(d);
  if (view.kind === "hidden") return null;
  const roles = (
    <p className="wk-note">
      Roles from groups are set in the{" "}
      <a href={settingsUrl(slug)} target="_blank" rel="noreferrer">
        Helix dashboard’s Settings
      </a>
      .
    </p>
  );

  if (view.kind === "none")
    return (
      <Panel
        title="Directory sync"
        subtitle="Connect your company’s directory, such as Okta or Microsoft Entra ID, and each of its groups becomes a team here, with the group’s members. People your IT admin assigns join the workspace, and leave when they’re unassigned."
        actions={
          owner && (
            <Button
              variant="primary"
              size="sm"
              icon={FolderSync}
              disabled={opening}
              onClick={openPortal}
            >
              {opening ? "Opening…" : "Connect your directory"}
            </Button>
          )
        }
      >
        <p className="wk-note">
          {owner
            ? "The setup guide opens in a new tab, with steps for your provider. Your IT admin can do this step."
            : "Only workspace owners can connect a directory."}
        </p>
      </Panel>
    );

  const conn = d.directory!,
    state = stateView(conn.state),
    last = syncText(d, (at) => formatRelative(at)),
    issues = issueGroups(d.issues || [], view.provider),
    busy = syncing(d) || sync.isPending;
  return (
    <Panel
      title="Directory sync"
      subtitle="Each group of your directory is a team here, with the group’s members. People your IT admin assigns join the workspace, and leave when they’re unassigned."
      actions={
        owner && (
          <>
            <Button size="sm" disabled={busy} onClick={() => sync.mutate()}>
              {busy ? "Syncing…" : "Sync now"}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={opening}
              onClick={openPortal}
            >
              {opening ? "Opening…" : "Open setup guide"}
            </Button>
          </>
        )
      }
    >
      <div className="wk-directory" aria-label="Directory status">
        <p>
          <strong>
            {conn.provider?.trim() || "Directory"}
            {conn.name ? ` · ${conn.name}` : ""}
          </strong>
          <Tag
            tone={
              state.tone === "ok"
                ? "success"
                : state.tone === "danger"
                  ? "danger"
                  : "warning"
            }
          >
            {state.label}
          </Tag>
        </p>
        {state.problem && (
          <p className="wk-directory-problem" role="alert">
            {state.problem}
          </p>
        )}
        <p
          className={
            last.tone === "danger" ? "wk-directory-problem" : "wk-note"
          }
        >
          {last.text} · {directoryCounts(d)}
        </p>
        {issues.length > 0 && (
          <ul className="wk-directory-issues" aria-label="Needs attention">
            {issues.map((i) => (
              <li key={i.kind}>
                {i.summary}
                {i.details.length > 0 && <small>{i.details.join(", ")}</small>}
              </li>
            ))}
          </ul>
        )}
        {roles}
        {!owner && (
          <p className="wk-note">
            Only workspace owners can sync or change the directory.
          </p>
        )}
      </div>
    </Panel>
  );
}

/**
 * Asks for sources for synced teams that hold none, above the teams. It
 * names the provider from the teams themselves, so it needs no directory read.
 */
export function NoSourcesNudge({
  teams,
  grants,
  onSources,
}: {
  teams: readonly FoundryTeam[];
  grants: readonly FoundryAccessGrant[];
  onSources: (t: FoundryTeam) => void;
}) {
  const bare = teamsWithoutSources(teams, grants);
  if (!bare.length) return null;
  const provider = bare[0].directory?.provider?.trim() || "your directory";
  return (
    <div className="wk-directory-nudge" role="status">
      <p>{withoutSourcesText(bare, provider)}</p>
      <Button size="sm" variant="primary" onClick={() => onSources(bare[0])}>
        Choose what {bare[0].name || "it"} reads
      </Button>
    </div>
  );
}
