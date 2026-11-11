import { agentJson } from "@/server/agent-routes";

// Where the Helix MCP is and how it signs in.
export const GET = (request: Request) => agentJson(request, "mcp");
