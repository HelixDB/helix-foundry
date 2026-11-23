import { LOCAL } from "@/shared/deployment";

/** Foundry's landing page as Markdown, on helix-website (its home). */
const LANDING_MARKDOWN = "https://www.helix-db.com/index.md";

// `/` goes to the landing page on helix-website (src/app/(app)/page.tsx), so
// its Markdown is helix-website's too: without this route the app's
// catch-all answered /index.md with the SPA's HTML. A local build has no
// landing page and serves no crawler.
export const GET = () =>
  LOCAL
    ? new Response("Not found", { status: 404 })
    : Response.redirect(LANDING_MARKDOWN, 307);
