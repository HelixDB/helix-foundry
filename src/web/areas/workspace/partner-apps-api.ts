import type {
  ApprovePartnerAppAuthorizationRequest,
  ApprovePartnerAppAuthorizationResponse,
  GetPartnerAppAuthorizationResponse,
  ListPartnerAppInstallsResponse,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { api, refusalOf } from "../../api";
import type { PartnerRequest } from "./partner-apps-model";

// Connected apps' WFE calls (hosted only): an app's request as Helix reads
// it, approving it for a workspace, and the workspace's installs, listed and
// revoked. An install's sources are its credential's source grants, read
// and set through Settings › Access's routes (./access-api.ts).

const enc = encodeURIComponent;
const installs = (ws: string) =>
  `/v1/workspaces/${enc(ws)}/partner-apps/installs`;

/**
 * The app and what it asks for, as Helix checks `request`. Its state and
 * PKCE challenge stay in the browser (the URL would reach the site's
 * logs): Approve sends them in its body, and Cancel adds the state to the
 * deny URL itself (partner-apps-model's cancelUrl).
 */
export const authorizationQuery = (
  request: Pick<PartnerRequest, "clientId" | "redirectUri" | "scope">,
) => ({
  queryKey: [
    "partner-apps",
    "authorization",
    request.clientId,
    request.redirectUri,
    request.scope,
  ],
  queryFn: () =>
    api<GetPartnerAppAuthorizationResponse>(
      `/v1/partner-apps/authorization?${new URLSearchParams({
        clientId: request.clientId,
        redirectUri: request.redirectUri,
        scope: request.scope,
      })}`,
    ),
  retry: false,
  staleTime: Infinity,
});

/**
 * Approves the app for workspace `ws`: where Helix sends the browser back.
 * A refusal is reported before it is thrown.
 */
export async function approve(
  ws: string,
  body: ApprovePartnerAppAuthorizationRequest,
): Promise<string> {
  try {
    const { redirectUrl } = await api<ApprovePartnerAppAuthorizationResponse>(
      `/v1/workspaces/${enc(ws)}/partner-apps/authorization:approve`,
      body,
    );
    return redirectUrl || "";
  } catch (e) {
    trackEvent("partner_app_approve_refused", { reason: refusalOf(e) });
    throw e;
  }
}

/** The workspace's live installs, the most recently used first (owners and admins). */
export const installsQuery = (ws: string) => ({
  queryKey: [ws, "partner-apps", "installs"],
  queryFn: async () =>
    (await api<ListPartnerAppInstallsResponse>(installs(ws))).installs || [],
});

/** Revokes an install: its key, and with it its sources, end at once. */
export const revokeInstall = (ws: string, id: string) =>
  api<object>(`${installs(ws)}/${enc(id)}`, undefined, "DELETE");
