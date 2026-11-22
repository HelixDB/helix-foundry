import type { MetadataRoute } from "next";
import { PROD } from "@/server/agent-context";
import { LOCAL } from "@/shared/deployment";

// robots.txt. On prod, crawlers, AI ones included, may read the agent files,
// `/` and its Markdown alone: here they go to the landing page on
// helix-website (src/app/(app)/page.tsx, src/app/index.md), and on
// foundry.new, whose robots.txt redirects here, `/` is the setup prompt,
// which agents some users point at it fetch only where robots.txt allows.
// Everything else is the signed-in app. Every other build (dev, the mock, a
// local install) asks them to read nothing.
export default function robots(): MetadataRoute.Robots {
  if (!PROD || LOCAL) return { rules: { userAgent: "*", disallow: "/" } };
  return {
    rules: {
      userAgent: "*",
      allow: [
        "/$",
        "/index.md",
        "/setup.md",
        "/llms.txt",
        "/llms-full.txt",
        "/.well-known/",
        "/favicon.svg",
      ],
      disallow: "/",
    },
  };
}
