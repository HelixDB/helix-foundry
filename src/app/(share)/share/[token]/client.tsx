"use client";

import dynamic from "next/dynamic";

// The page draws in the browser only, as the app does: its tiles are the
// app's (src/web), which never render on a server.
export const SharedDashboard = dynamic(
  () =>
    import("@/web/share/shared-dashboard").then((m) => m.SharedDashboardPage),
  { ssr: false },
);
