// Which Foundry this build is, fixed by `next build` (NEXT_PUBLIC_FOUNDRY_MODE,
// checked in next.config.ts):
// - unset: the hosted site. People sign in through WorkOS, workspaces have
//   members, and Foundry Pro is billed.
// - "local": a copy someone runs on their own machine. It serves only loopback
//   hosts, has one owner and no sign-in, team or billing, and /papi sends the
//   API a token shared with it (src/server/local.ts).
export const LOCAL = process.env.NEXT_PUBLIC_FOUNDRY_MODE === "local";

// Whether this build has Founder (areas/agent; NEXT_PUBLIC_FOUNDRY_ASSISTANT,
// set in next.config.ts: local builds and the mock, not the hosted stamps
// until their backend serves it). With it, a member who isn't a grantee asks
// Founder on /ask; the Analyst answers under it.
export const FOUNDER = !!process.env.NEXT_PUBLIC_FOUNDRY_ASSISTANT;
