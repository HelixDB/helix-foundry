// Runs in the browser before every page of the site. On the hosted stamps'
// landing page only, it starts Vercel BotID, which marks the reminder form's
// POST /remind so the route can tell a person from a bot (src/app/remind).
// NEXT_PUBLIC_BOTID is set only in a stamp's own build (next.config.ts), so
// the app, a local build, the mock and e2e never load it.
if (process.env.NEXT_PUBLIC_BOTID === "1" && location.pathname === "/")
  void import("botid/client/core").then(({ initBotId }) =>
    initBotId({ protect: [{ path: "/remind", method: "POST" }] }),
  );
