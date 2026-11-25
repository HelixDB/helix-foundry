import { LOCAL } from "../../../shared/deployment";

/**
 * Whether this build offers connected apps (partner integrations reading a
 * workspace through Helix's OAuth): the hosted dev stamp and the mock, never
 * prod (until Helix turns it on there) or a local build, which has no Helix
 * to register apps with (next.config.ts NEXT_PUBLIC_FOUNDRY_APPS).
 */
export const partnerApps =
  !LOCAL && process.env.NEXT_PUBLIC_FOUNDRY_APPS === "1";
