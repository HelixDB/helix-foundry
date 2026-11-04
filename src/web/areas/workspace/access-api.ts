import type {
  CreateServiceCredentialRequest,
  CreateServiceCredentialResponse,
  FoundryAccessChange,
  FoundryAccessTarget,
  FoundryTeamRequest,
  FoundryTeamResponse,
  GetMyFoundryAccessResponse,
  ListFoundryAccessResponse,
  ListServiceCredentialsResponse,
  PreviewFoundryAccessChangeResponse,
  SetFoundryAccessGrantsRequest,
  SetFoundryAccessGrantsResponse,
  SetFoundryMemberRestrictedRequest,
  SetFoundryMemberRestrictedResponse,
} from "../../../shared/wfe";
import { api } from "../../api";
import type { Principal } from "./access-model";

// Settings › Access's WFE calls, and restricted mode's: the workspace's
// grants, teams and restricted members, who reads what through them, what
// a change would take away, agents' keys, and what the caller itself reads
// through grants. Hosted only: a local build has one owner.

const enc = encodeURIComponent;
const access = (ws: string) => `/v1/workspaces/${enc(ws)}/foundry/access`;
const keys = (ws: string) => `/v1/workspaces/${enc(ws)}/service-credentials`;

/** Every grant, team and restricted member, and who reads each ontology (owners and admins). */
export const accessQuery = (ws: string) => ({
  queryKey: [ws, "foundry-access"],
  queryFn: () => api<ListFoundryAccessResponse>(access(ws)),
});

/**
 * What the caller reads through grants: read once per workspace, as the
 * shell opens it, to tell a grantee (restricted to what grants give).
 */
export const myAccessQuery = (ws: string) => ({
  queryKey: [ws, "foundry-access", "me"],
  queryFn: () => api<GetMyFoundryAccessResponse>(`${access(ws)}/me`),
  // Grants change elsewhere: a read of a minute ago is fresh enough, and coming
  // back to the tab or a job's end (../ontology/scope.tsx) reads it again.
  staleTime: 60_000,
  refetchOnWindowFocus: "always" as const,
});

/** The workspace's agents' keys, revoked ones too (with revokedAt). */
export const keysQuery = (ws: string) => ({
  queryKey: [ws, "service-credentials"],
  queryFn: async () =>
    (await api<ListServiceCredentialsResponse>(keys(ws))).credentials || [],
});

/** Replaces a principal's grants; none takes them all away. */
export const setGrants = (
  ws: string,
  principal: Principal,
  targets: FoundryAccessTarget[],
) =>
  api<SetFoundryAccessGrantsResponse>(
    `${access(ws)}/grants`,
    { principal, targets } satisfies SetFoundryAccessGrantsRequest,
    "PUT",
  );

/** Who reads an ontology through grants now and wouldn't after `change`. */
export const previewChange = async (ws: string, change: FoundryAccessChange) =>
  (
    await api<PreviewFoundryAccessChangeResponse>(`${access(ws)}:preview`, {
      change,
    })
  ).losses || [];

/** Restricts a plain member to what grants give, or lifts it. */
export const setRestricted = (
  ws: string,
  userId: string,
  restricted: boolean,
) =>
  api<SetFoundryMemberRestrictedResponse>(
    `${access(ws)}/members/${enc(userId)}`,
    { restricted } satisfies SetFoundryMemberRestrictedRequest,
    "PUT",
  );

export const createTeam = (ws: string, team: FoundryTeamRequest) =>
  api<FoundryTeamResponse>(`${access(ws)}/teams`, team);
/** Renames a team and replaces its members. */
export const updateTeam = (ws: string, id: string, team: FoundryTeamRequest) =>
  api<FoundryTeamResponse>(`${access(ws)}/teams/${enc(id)}`, team, "PUT");
/** Deletes a team with its grants. */
export const deleteTeam = (ws: string, id: string) =>
  api<object>(`${access(ws)}/teams/${enc(id)}`, undefined, "DELETE");

/**
 * Makes an agent's key with no project grants: it reaches Foundry only
 * through source grants. Its token is in the answer once.
 */
export const createKey = (ws: string, name: string) =>
  api<CreateServiceCredentialResponse>(keys(ws), {
    name,
  } satisfies CreateServiceCredentialRequest);
/** Revokes a key; the WFE deletes its grants with it. */
export const revokeKey = (ws: string, id: string) =>
  api<object>(`${keys(ws)}/${enc(id)}`, undefined, "DELETE");
