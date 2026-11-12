import { agentText } from "@/server/agent-routes";

// What Foundry is to an agent, and Foundry's docs as links (llmstxt.org).
export const GET = (request: Request) => agentText(request, "llms");
