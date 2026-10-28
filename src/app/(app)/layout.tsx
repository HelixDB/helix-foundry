import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { THEME_BACKGROUNDS, THEME_BOOT } from "@/shared/theme";
// Global styles, in helix-foundry's order (apps/web/src/main.tsx).
import "@/web/style.css";
import "@/web/minimal.css";
import "@/web/workspace.css";
import "@/web/kit/kit.css";
import "@/web/shell/shell.css";

export const metadata: Metadata = {
  title: "Helix Foundry",
  description:
    "Turn company data into a working intelligence workspace. Powered by HelixDB.",
  robots: { index: false, follow: false },
};

// System follows these; shell/theme.ts rewrites them for an explicit choice.
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

// THEME_BOOT sets data-theme on <html> before first paint, so the server's
// <html> differs from the DOM React hydrates: suppressHydrationWarning.
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_BOOT }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
