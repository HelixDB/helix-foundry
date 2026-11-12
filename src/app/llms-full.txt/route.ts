import { agentText } from "@/server/agent-routes";

// llms.txt with the setup prompt and Foundry's docs pages whole.
export const GET = (request: Request) => agentText(request, "llms-full");
