import { agentText } from "@/server/agent-routes";

// /llms.txt, where some scanners look for it.
export const GET = (request: Request) => agentText(request, "llms");
