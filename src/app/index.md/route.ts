import { agentText } from "@/server/agent-routes";

// The landing page in Markdown, which / serves a client that asks for it
// (src/proxy.ts).
export const GET = (request: Request) => agentText(request, "index");
