"use client";

import dynamic from "next/dynamic";

// The landing page's figures and the field behind them, in the browser only:
// they read the screen (the theme, reduced motion, sizes) as they load and
// render. The Analyst brings the app's chat and charts with it, so it is its
// own chunk like the others.
export const Schema = dynamic(() => import("./schema").then((m) => m.Schema), {
  ssr: false,
});
export const Explorer = dynamic(
  () => import("./explorer").then((m) => m.Explorer),
  { ssr: false },
);
export const Scene = dynamic(() => import("./scene").then((m) => m.Scene), {
  ssr: false,
});
export const Analyst = dynamic(
  () => import("./analyst").then((m) => m.Analyst),
  { ssr: false },
);
export const Connect = dynamic(
  () => import("./connect").then((m) => m.Connect),
  { ssr: false },
);
export const Behaviours = dynamic(
  () => import("./behaviours").then((m) => m.Behaviours),
  { ssr: false },
);
