import type { MetadataRoute } from "next";
import { PROD, PROD_ORIGIN } from "@/server/agent-context";
import { LOCAL } from "@/shared/deployment";

// sitemap.xml: the pages crawlers may list, which is the landing page, on
// prod only.
export default function sitemap(): MetadataRoute.Sitemap {
  if (!PROD || LOCAL) return [];
  return [{ url: `${PROD_ORIGIN}/`, changeFrequency: "weekly", priority: 1 }];
}
