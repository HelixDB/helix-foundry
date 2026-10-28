import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
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

const prod = process.env.NEXT_PUBLIC_APP_ENV?.trim().toLowerCase() === "prod";

export const metadata: Metadata = {
  title: "Helix Foundry",
  description:
    "Every system your company runs, matched into one set of objects you can explore and ask questions of. Hosted, or on your own machine.",
  icons: "/landing/favicon.svg",
  // The one page search engines may list, and only on prod.
  robots: { index: prod, follow: prod },
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
      <body>{children}</body>
    </html>
  );
}
