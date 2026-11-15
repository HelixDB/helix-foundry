import { GeistMono } from "geist/font/mono";
import { GeistSans } from "geist/font/sans";
import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { THEME_BACKGROUNDS, THEME_BOOT } from "@/shared/theme";
// The app's tokens and kit, without its shell: a published dashboard's page
// is drawn as the app draws a dashboard, light or dark as the viewer's
// browser is.
import "@/web/style.css";
import "@/web/minimal.css";
import "@/web/workspace.css";
import "@/web/kit/kit.css";

// A root layout of its own: no app, no session, no analytics (its address
// carries the link's token, which nothing on it may send anywhere), and no
// referrer from it or anything it loads.
export const metadata: Metadata = {
  title: "Shared dashboard · Helix Foundry",
  robots: { index: false, follow: false },
  referrer: "no-referrer",
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  themeColor: [
    {
      media: "(prefers-color-scheme: light)",
      color: THEME_BACKGROUNDS.light,
    },
    { media: "(prefers-color-scheme: dark)", color: THEME_BACKGROUNDS.dark },
  ],
};

export default function ShareLayout({ children }: { children: ReactNode }) {
  return (
    <html
      lang="en"
      className={`${GeistSans.variable} ${GeistMono.variable}`}
      suppressHydrationWarning
    >
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_BOOT }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
