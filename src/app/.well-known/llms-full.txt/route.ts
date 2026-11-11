import { agentText } from "@/server/agent-routes";

// /llms-full.txt, where some scanners look for it.
export const GET = (request: Request) => agentText(request, "llms-full");
