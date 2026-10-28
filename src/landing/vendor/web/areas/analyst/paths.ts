// Analyst area URLs, merged into `paths` (apps/web/src/paths.ts). Owned by the
// analyst area; do not import ../../paths here (it imports this file).
const enc = encodeURIComponent;
export const analystPaths = {
  analyst: () => "/analyst",
  thread: (threadId: string) => "/analyst/" + enc(threadId),
};
