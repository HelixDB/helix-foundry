import type {
  DirectoryIssue,
  DirectoryIssueKind,
  DirectoryState,
  FoundryAccessGrant,
  FoundryTeam,
  WorkspaceDirectory,
} from "../../../shared/wfe";

// Directory sync as Settings › Access › Teams shows it (SCIM through WorkOS,
// the host's): whether the workspace can have a directory, how it is doing,
// what needs attention, and which synced teams read nothing yet. Pure; the
// panel is access-directory.tsx. The Helix dashboard words the same states
// (helix-website's workspace-directory.ts): keep them in step.

/** What the panel shows: nothing, Connect, or the directory's status. */
export type DirectoryView =
  | { kind: "hidden" }
  | { kind: "none" }
  | { kind: "connected"; provider: string };

/**
 * A stamp where directory sync is off (`entitled` false) shows nothing, as a
 * read that can't be made (a member, an older WFE) does.
 */
export function directoryView(
  directory: WorkspaceDirectory | undefined,
): DirectoryView {
  if (!directory?.entitled) return { kind: "hidden" };
  if (!directory.directory) return { kind: "none" };
  return {
    kind: "connected",
    provider: providerName(directory.directory.provider),
  };
}

/**
 * Whether a failed read of the directory means the panel isn't for this
 * workspace: refused (403), or a WFE without the route (404, 501). Any other
 * failure (a 429, a 5xx, a timeout) is said, with Retry, never taken for
 * "no directory sync here".
 */
export const directoryNotOffered = (status: number | undefined) =>
  status === 403 || status === 404 || status === 501;

/** The provider's name for copy, or "your directory" when the WFE sends none. */
export const providerName = (provider: string | undefined) =>
  provider?.trim() || "your directory";

export type StateView = {
  label: string;
  tone: "ok" | "pending" | "danger";
  /** What to do, when someone has to act. */
  problem?: string;
  /** What comes next, when nothing is wrong. */
  hint?: string;
};

/**
 * The directory's state in words. `synced` says whether it ever synced: one
 * that never linked was chosen in the setup guide and not finished, which is
 * no breakage.
 */
export function stateView(
  state: DirectoryState | undefined,
  synced = true,
): StateView {
  switch (state) {
    case "DIRECTORY_STATE_LINKED":
      return { label: "Connected", tone: "ok" };
    case "DIRECTORY_STATE_VALIDATING":
      return { label: "Checking the connection", tone: "pending" };
    case "DIRECTORY_STATE_INVALID_CREDENTIALS":
      return {
        label: "Credentials stopped working",
        tone: "danger",
        problem:
          "Helix can no longer read your directory. Reconnect it in the setup guide.",
      };
    case "DIRECTORY_STATE_UNLINKED":
      if (!synced)
        return {
          label: "Setup not finished",
          tone: "pending",
          hint: "Finish connecting it in the setup guide. Its groups and people show here once it sends them.",
        };
      return {
        label: "Disconnected",
        tone: "danger",
        problem:
          "Your directory stopped sending changes. Reconnect it in the setup guide.",
      };
    case "DIRECTORY_STATE_DELETING":
      return { label: "Being removed", tone: "pending" };
    default:
      return { label: "Unknown", tone: "pending" };
  }
}

export const syncing = (d: WorkspaceDirectory) =>
  d.syncState === "DIRECTORY_SYNC_STATE_PENDING";

/** The last sync in words: "Synced 5m ago", "Syncing…", or why it failed. */
export function syncText(
  d: WorkspaceDirectory,
  ago: (at: string) => string,
): { text: string; tone: "ok" | "pending" | "danger" } {
  if (syncing(d)) return { text: "Syncing…", tone: "pending" };
  // One that never linked can't have synced: an older WFE's failure for it
  // ("the directory is unlinked") says nothing the state doesn't.
  if (!d.lastSyncedAt && d.directory?.state === "DIRECTORY_STATE_UNLINKED")
    return { text: "Not synced yet", tone: "pending" };
  if (d.syncState === "DIRECTORY_SYNC_STATE_FAILED") {
    const why = d.syncError?.trim();
    return {
      text: why ? `The last sync failed: ${why}` : "The last sync failed.",
      tone: "danger",
    };
  }
  const when = d.lastSyncedAt && ago(d.lastSyncedAt);
  return when
    ? { text: `Synced ${when}`, tone: "ok" }
    : { text: "Not synced yet", tone: "pending" };
}

const count = (n: number, one: string, many: string) =>
  `${n.toLocaleString("en")} ${n === 1 ? one : many}`;

/** "5 people · 3 groups". */
export const directoryCounts = (d: WorkspaceDirectory) =>
  `${count(d.userCount ?? 0, "person", "people")} · ${count(
    (d.groups ?? []).length,
    "group",
    "groups",
  )}`;

export type IssueGroup = {
  kind: DirectoryIssueKind;
  summary: string;
  /** Who or what it concerns, one a line; empty when the summary names them. */
  details: string[];
};

// Most pressing first. LEFT_OUT_RAW_ACCESS isn't here: each synced team
// says whom it leaves out.
const ISSUE_ORDER: readonly DirectoryIssueKind[] = [
  "DIRECTORY_ISSUE_KIND_NO_EMAIL",
  "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED",
  "DIRECTORY_ISSUE_KIND_PENDING_INVITATION",
  "DIRECTORY_ISSUE_KIND_OWNER_KEPT",
  "DIRECTORY_ISSUE_KIND_TOO_MANY_GROUPS",
  "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE",
];

const unique = (xs: readonly string[]) => [...new Set(xs)];
const personOf = (i: DirectoryIssue) =>
  i.displayName?.trim() || i.email?.trim() || "Someone";
const listed = (xs: readonly string[]) =>
  xs.length <= 1
    ? (xs[0] ?? "")
    : `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`;

function issueGroup(
  kind: DirectoryIssueKind,
  issues: readonly DirectoryIssue[],
  provider: string,
): IssueGroup | undefined {
  const people = unique(issues.map(personOf)),
    n = people.length,
    groups = unique(issues.map((i) => i.groupName?.trim() || "A group"));
  switch (kind) {
    case "DIRECTORY_ISSUE_KIND_NO_EMAIL":
      return {
        kind,
        summary: `${count(n, "person has", "people have")} no email in ${provider}, so they can’t join.`,
        details: people,
      };
    case "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED":
      return {
        kind,
        summary: `${count(n, "person", "people")} in ${provider} ${n === 1 ? "has" : "have"} no Helix account yet. If this lasts, contact Helix support.`,
        details: people,
      };
    case "DIRECTORY_ISSUE_KIND_PENDING_INVITATION":
      return {
        kind,
        summary: `${count(n, "person was", "people were")} emailed an invitation and ${n === 1 ? "joins" : "join"} once they accept it.`,
        details: people,
      };
    case "DIRECTORY_ISSUE_KIND_OWNER_KEPT":
      return n === 1
        ? {
            kind,
            summary: `${people[0]} was removed in ${provider} but stays, because the directory never removes an owner.`,
            details: [],
          }
        : {
            kind,
            summary: `${count(n, "owner was", "owners were")} removed in ${provider} but stay, because the directory never removes an owner.`,
            details: people,
          };
    case "DIRECTORY_ISSUE_KIND_TOO_MANY_GROUPS":
      return {
        kind,
        summary: `${provider} has more than 500 groups, so only the first 500 by name are synced.`,
        details: [],
      };
    case "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE":
      return {
        kind,
        summary:
          groups.length === 1
            ? `${groups[0]} has more than 10,000 people, so it syncs without members.`
            : `${listed(groups)} have more than 10,000 people each, so they sync without members.`,
        details: [],
      };
    default:
      return undefined;
  }
}

/** The directory's issues in words, one entry per kind, most pressing first. */
export function issueGroups(
  issues: readonly DirectoryIssue[],
  provider: string,
): IssueGroup[] {
  const out: IssueGroup[] = [];
  for (const kind of ISSUE_ORDER) {
    const of = issues.filter((i) => i.kind === kind);
    const g = of.length ? issueGroup(kind, of, provider) : undefined;
    if (g)
      out.push({
        ...g,
        summary: g.summary.charAt(0).toUpperCase() + g.summary.slice(1),
      });
  }
  return out;
}

/**
 * Synced teams that hold no sources yet: until they do, being on one limits
 * no one, so the Teams tab asks for them first.
 */
export function teamsWithoutSources(
  teams: readonly FoundryTeam[],
  grants: readonly FoundryAccessGrant[],
): FoundryTeam[] {
  const holding = new Set(
    grants
      .filter((g) => g.principal?.kind === "KIND_TEAM")
      .map((g) => g.principal?.id),
  );
  return teams.filter((t) => t.directory && !holding.has(t.id));
}

/** The nudge's line for them: "Engineering and Finance read nothing yet…". */
export function withoutSourcesText(
  teams: readonly FoundryTeam[],
  provider: string,
): string {
  const names = teams.map((t) => t.name || "A team");
  const who =
    names.length <= 3
      ? listed(names)
      : `${names.slice(0, 2).join(", ")} and ${names.length - 2} more teams`;
  const one = names.length === 1;
  return `${who} ${one ? "was" : "were"} synced from ${provider} but ${one ? "reads" : "read"} nothing yet, so being on ${one ? "it" : "them"} limits no one. Choose what ${one ? "it reads" : "each reads"} with Sources.`;
}
