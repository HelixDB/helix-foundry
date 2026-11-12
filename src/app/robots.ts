import type { MetadataRoute } from "next";
import { PROD, PROD_ORIGIN } from "@/server/agent-context";
import { LOCAL } from "@/shared/deployment";

// robots.txt. On prod, crawlers, AI ones included, may read the landing page,
// its Markdown, the agent files and what the landing page loads; everything
// else is the signed-in app. Every other build (dev, the mock, a local
// install) asks them to read nothing.
export default function robots(): MetadataRoute.Robots {
  if (!PROD || LOCAL) return { rules: { userAgent: "*", disallow: "/" } };
  return {
    rules: {
      userAgent: "*",
      allow: [
        "/$",
        "/index.md",
        "/llms.txt",
        "/llms-full.txt",
        "/.well-known/",
        "/landing/",
        "/connectors/",
        "/providers/",
        "/_next/static/",
      ],
      disallow: "/",
    },
    sitemap: `${PROD_ORIGIN}/sitemap.xml`,
  };
}
