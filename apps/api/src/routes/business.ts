import type { FastifyInstance } from "fastify";
import { z } from "zod";
import type { Store } from "../store.js";
import {
  addKnowledge,
  describeBusiness,
  dismissSuggestion,
  insights,
  removeKnowledge,
} from "../industry.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

const DescribeSchema = z.object({ description: z.string().max(200) });
const KnowledgeSchema = z.object({ text: z.string().min(1).max(300) });
export const businessSchemas: RequestSchemas = {
  "PATCH /api/v1/workspaces/:w/business": DescribeSchema,
  "POST /api/v1/workspaces/:w/business/knowledge": KnowledgeSchema,
  "POST /api/v1/workspaces/:w/business/suggestions/:id/dismiss": z.object({}),
};

// What Foundry believes about the business: the industry's playbook applied to
// this workspace (docs/industries). Reading it never asks anything; the owner
// can describe the business, add what they know, or set a suggestion aside.
export function registerBusinessRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access } = deps;
  const base = "/api/v1/workspaces/:w/business";
  app.get(base, async (req) => {
    const a = await access(req);
    return insights(store, a.scope, a.user.id);
  });
  app.patch(base, async (req) => {
    const a = await access(req, "editor"),
      body = DescribeSchema.parse(req.body);
    await describeBusiness(store, a.scope, body.description, a.user.id);
    return insights(store, a.scope, a.user.id);
  });
  app.post(base + "/knowledge", async (req) => {
    const a = await access(req, "editor"),
      body = KnowledgeSchema.parse(req.body);
    await addKnowledge(store, a.scope, body.text);
    return insights(store, a.scope, a.user.id);
  });
  app.delete(base + "/knowledge/:id", async (req) => {
    const a = await access(req, "editor");
    await removeKnowledge(store, a.scope, (req.params as any).id);
    return insights(store, a.scope, a.user.id);
  });
  app.post(base + "/suggestions/:id/dismiss", async (req) => {
    const a = await access(req, "editor");
    await dismissSuggestion(store, a.scope, (req.params as any).id);
    return insights(store, a.scope, a.user.id);
  });
}
