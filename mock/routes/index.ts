import type { MockRoute } from "../http";
import { aiRoutes } from "./ai";
import { authRoutes } from "./auth";
import { billingRoutes } from "./billing";
import { dataRoutes } from "./data";
import { dataLayoutRoutes } from "./data-layout";
import { databaseRoutes } from "./databases";
import { documentRoutes } from "./documents";
import { enrichmentRoutes } from "./enrichment";
import { eventSourceRoutes } from "./event-sources";
import { reviewRoutes } from "./reviews";
import { loadRoutes } from "./loads";
import { ontologiesRoutes } from "./ontologies";
import { ontologyRoutes } from "./ontology";
import { questionRoutes } from "./questions";
import { usageRoutes } from "./usage";
import { workspaceRoutes } from "./workspaces";

// Every WFE route the mock serves. An area adds a module next to these
// (e.g. ./connectors.ts exporting connectorRoutes) and spreads it in here.
// Each route must also be on the /papi allowlist to be reachable from the app.
export const routes: MockRoute[] = [
  ...authRoutes,
  ...workspaceRoutes,
  ...billingRoutes,
  ...usageRoutes,
  ...dataRoutes,
  ...eventSourceRoutes,
  ...dataLayoutRoutes,
  ...ontologiesRoutes,
  ...ontologyRoutes,
  ...documentRoutes,
  ...reviewRoutes,
  ...loadRoutes,
  ...enrichmentRoutes,
  ...aiRoutes,
  ...questionRoutes,
  ...databaseRoutes,
];
