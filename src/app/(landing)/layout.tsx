import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { PROD, PROD_ORIGIN } from "@/server/agent-context";
import { Telemetry } from "@/web/telemetry";
// The landing page's own stylesheets, before the figures' (src/landing). A
// root layout of its own, so none of the app's global styles reach it, and
// going between the two is a full page load.
import "@/landing/styles/base.css";
import "@/landing/styles/sections.css";
import "@/landing/styles/schema.css";
import "@/landing/styles/explorer.css";
import "@/landing/styles/scene.css";
import "@/landing/styles/connect.css";
import "@/landing/styles/analyst.css";

const description =
  "Every system your company runs, matched into one ontology that you and your AI agents explore and ask questions of, each answer citing its sources. Hosted, or on your own machine.";

export const metadata: Metadata = {
  // Every stamp's canonical page is prod's.
  metadataBase: new URL(PROD_ORIGIN),
  title: "Helix Foundry",
  description,
  icons: "/landing/favicon.svg",
  // The one page search engines may list, and only on prod.
  robots: { index: PROD, follow: PROD },
  // The same page in Markdown, for agents (src/app/index.md).
  alternates: { canonical: "/", types: { "text/markdown": "/index.md" } },
  openGraph: {
    type: "website",
    url: "/",
    siteName: "Helix Foundry",
    title: "Helix Foundry",
    description,
  },
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  themeColor: "#0e0e0e",
};

export default function LandingLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" data-theme="dark">
      <head>
        <link rel="preconnect" href="https://fonts.googleapis.com" />
        <link
          rel="preconnect"
          href="https://fonts.gstatic.com"
          crossOrigin=""
        />
        {/* eslint-disable-next-line @next/next/no-page-custom-font -- this
            root layout is the landing page's whole document. */}
        <link
          rel="stylesheet"
          href="https://fonts.googleapis.com/css2?family=Geist:wght@300..700&family=Geist+Mono:wght@400..600&display=swap"
        />
      </head>
      <body>
        {children}
        <Telemetry />
      </body>
    </html>
  );
}
