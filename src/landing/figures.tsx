"use client";

import dynamic from "next/dynamic";
import { useWide } from "./phone";

// The landing page's figures and the field behind them, in the browser only:
// they read the screen (the theme, reduced motion, sizes) as they load and
// render. The Analyst brings the app's chat and charts with it, so it is its
// own chunk like the others. A phone (./phone.ts) gets neither the field nor
// the figures that need room or the field: the schema, whose dots the field
// draws, the Explorer, whose records it draws, and the setup scene. They
// mount, and their chunks load, only on a wider screen.
const SchemaFigure = dynamic(() => import("./schema").then((m) => m.Schema), {
  ssr: false,
});
const ExplorerFigure = dynamic(
  () => import("./explorer").then((m) => m.Explorer),
  { ssr: false },
);
const SceneFigure = dynamic(() => import("./scene").then((m) => m.Scene), {
  ssr: false,
});

export function Schema() {
  return useWide() ? <SchemaFigure /> : null;
}
export function Explorer() {
  return useWide() ? <ExplorerFigure /> : null;
}
export function Scene() {
  return useWide() ? <SceneFigure /> : null;
}

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
