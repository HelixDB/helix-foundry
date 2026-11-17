import type { MetadataRoute } from "next";
import { PROD } from "@/server/agent-context";
import { LOCAL } from "@/shared/deployment";

// robots.txt. On prod, crawlers, AI ones included, may read the agent files;
// everything else is the signed-in app, and `/` goes to the landing page on
// helix-website (src/proxy.ts). Every other build (dev, the mock, a local
// install) asks them to read nothing.
export default function robots(): MetadataRoute.Robots {
  if (!PROD || LOCAL) return { rules: { userAgent: "*", disallow: "/" } };
  return {
    rules: {
      userAgent: "*",
      allow: ["/llms.txt", "/llms-full.txt", "/.well-known/", "/favicon.svg"],
      disallow: "/",
    },
  };
}
