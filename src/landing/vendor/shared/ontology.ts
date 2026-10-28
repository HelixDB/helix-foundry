// Area schemas for Ontology: object types, link types, actions and object views.
// Owned by the ontology area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index" would be circular.
// Type-only imports from "./index" are fine.
import { z } from "zod";

export const OntologySchema = z.object({
  name: z.string().min(1),
  datasetId: z.string(),
  primaryKey: z.string(),
  properties: z.array(z.string()),
  description: z.string().default(""),
});
export const RelationshipSchema = z.object({
  name: z.string(),
  fromType: z.string(),
  toType: z.string(),
  fromColumn: z.string(),
  toColumn: z.string(),
});
