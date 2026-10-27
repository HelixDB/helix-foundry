"use client";

import dynamic from "next/dynamic";

// The app runs in the browser only: no server rendering, as in helix-foundry.
const FoundryApp = dynamic(() => import("@/web/main"), { ssr: false });

export function ClientOnly() {
  return <FoundryApp />;
}
