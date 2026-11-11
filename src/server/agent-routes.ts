import { landingMarkdown } from "@/landing/markdown";
import { LOCAL } from "@/shared/deployment";
import {
  PROD,
  SOURCE,
  aiCatalog,
  docsText,
  llmsFullTxt,
  llmsTxt,
  mcpManifest,
  setupPrompt,
  siteOf,
  type Site,
} from "./agent-context";

// The answers of the site's agent routes (src/app/llms.txt, llms-full.txt,
// index.md and .well-known): a 200 in the format asked for, readable from
// any origin and cached by the CDN for 5 minutes. A local build serves none
// of them: it has no landing page and serves no crawler. Off prod they say
// noindex, as robots.txt does.

const headers = (contentType: string) => ({
  "Access-Control-Allow-Origin": "*",
  "Cache-Control": "public, s-maxage=300, stale-while-revalidate=86400",
  "Content-Type": contentType,
  ...(PROD ? {} : { "X-Robots-Tag": "noindex" }),
});

const notFound = () => new Response("Not found", { status: 404 });

const TEXT = {
  llms: {
    type: "text/plain; charset=utf-8",
    body: async (site: Site) => llmsTxt(site, await docsText("/llms.txt")),
  },
  "llms-full": {
    type: "text/plain; charset=utf-8",
    body: async (site: Site) =>
      llmsFullTxt(
        site,
        ...(await Promise.all([
          docsText("/llms.txt"),
          docsText("/llms-full.txt"),
        ])),
      ),
  },
  index: {
    type: "text/markdown; charset=utf-8",
    body: async (site: Site) =>
      landingMarkdown(site.origin, SOURCE, setupPrompt(site)),
  },
} as const;

/** One of the agent pages, as text. */
export async function agentText(
  request: Request,
  page: keyof typeof TEXT,
): Promise<Response> {
  if (LOCAL) return notFound();
  const { type, body } = TEXT[page];
  return new Response(await body(siteOf(request)), { headers: headers(type) });
}

const JSON_DOCS = {
  mcp: { type: "application/json", body: mcpManifest },
  "ai-catalog": { type: "application/ai-catalog+json", body: aiCatalog },
} as const;

/** One of the agent manifests, as JSON. */
export function agentJson(
  request: Request,
  doc: keyof typeof JSON_DOCS,
): Response {
  if (LOCAL) return notFound();
  const { type, body } = JSON_DOCS[doc];
  return new Response(JSON.stringify(body(siteOf(request)), null, 2), {
    headers: headers(type),
  });
}
