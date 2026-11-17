import { LOCAL } from "../../../shared/deployment";

/**
 * Whether this build offers dashboards' public links: never a local one, nor
 * one without the door (NEXT_PUBLIC_HELIX_EVENTS_URL) the share page reads.
 * The WFE says so per dashboard too (canPublish, publishBlock) and per
 * workspace (its dashboard settings' available).
 */
export const publicLinks = !LOCAL && !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;
