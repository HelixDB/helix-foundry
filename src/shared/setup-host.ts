// foundry.new: the address a user gives their own agent ("setup
// https://foundry.new") to set Foundry up with them. It points at the prod
// stamp: its `/` is the setup prompt as Markdown (src/app/setup.md), and
// every other path redirects to the product's site, which signs in
// (next.config.ts). No code here: next.config.ts imports it.

/** The host of the setup address. */
export const SETUP_HOST = "foundry.new";

/** Where the hosted site is when it is the product, not a stamp of it. */
export const PROD_ORIGIN = "https://foundry.helix-db.com";
