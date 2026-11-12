import { agentJson } from "@/server/agent-routes";

// What an agent can read here, by type.
export const GET = (request: Request) => agentJson(request, "ai-catalog");
