import localFont from "next/font/local";

/**
 * Helix Mono (github.com/HelixDB/helix-mono, OFL 1.1: fonts/helix-mono/OFL.txt),
 * self-hosted by next/font: every piece of the app's text. Its variable,
 * `--font-helix-mono`, feeds the font tokens (`--hf-font-*`, workspace.css);
 * a root layout puts it on <html>.
 */
export const helixMono = localFont({
  src: [
    {
      path: "./fonts/helix-mono/HelixMono-ExtraLight.woff2",
      weight: "200",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-ExtraLightItalic.woff2",
      weight: "200",
      style: "italic",
    },
    {
      path: "./fonts/helix-mono/HelixMono-Light.woff2",
      weight: "300",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-LightItalic.woff2",
      weight: "300",
      style: "italic",
    },
    {
      path: "./fonts/helix-mono/HelixMono-Regular.woff2",
      weight: "400",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-Italic.woff2",
      weight: "400",
      style: "italic",
    },
    {
      path: "./fonts/helix-mono/HelixMono-Medium.woff2",
      weight: "500",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-MediumItalic.woff2",
      weight: "500",
      style: "italic",
    },
    {
      path: "./fonts/helix-mono/HelixMono-SemiBold.woff2",
      weight: "600",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-SemiBoldItalic.woff2",
      weight: "600",
      style: "italic",
    },
    {
      path: "./fonts/helix-mono/HelixMono-Bold.woff2",
      weight: "700",
      style: "normal",
    },
    {
      path: "./fonts/helix-mono/HelixMono-BoldItalic.woff2",
      weight: "700",
      style: "italic",
    },
  ],
  variable: "--font-helix-mono",
  display: "swap",
});
