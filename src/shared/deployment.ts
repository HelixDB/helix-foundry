// Which Foundry this build is, fixed by `next build` (NEXT_PUBLIC_FOUNDRY_MODE,
// checked in next.config.ts):
// - unset: the hosted site. People sign in through WorkOS, workspaces have
//   members, and Foundry Pro is billed.
// - "local": a copy someone runs on their own machine. It serves only loopback
//   hosts, has one owner and no sign-in, team or billing, and /papi sends the
//   API a token shared with it (src/server/local.ts).
export const LOCAL = process.env.NEXT_PUBLIC_FOUNDRY_MODE === "local";
