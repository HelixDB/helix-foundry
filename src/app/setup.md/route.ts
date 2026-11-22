import { agentText } from "@/server/agent-routes";

// The setup prompt for an agent, as Markdown: what https://foundry.new is.
export const GET = (request: Request) => agentText(request, "setup");
