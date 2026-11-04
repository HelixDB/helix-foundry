import type { MockRoute } from "../http";
import { accessRoutes, guarded } from "./access";
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
import { signInRoutes } from "./sign-ins";
import { loadRoutes } from "./loads";
import { ontologiesRoutes } from "./ontologies";
import { ontologyRoutes } from "./ontology";
import { questionRoutes } from "./questions";
import { suggestionRoutes } from "./suggestions";
import { usageRoutes } from "./usage";
import { workspaceRoutes } from "./workspaces";

// Every WFE route the mock serves. An area adds a module next to these
// (e.g. ./connectors.ts exporting connectorRoutes) and spreads it in here.
// Each route must also be on the /papi allowlist to be reachable from the app.
// Foundry's routes run source grants' access check first (./access.ts).
export const routes: MockRoute[] = [
  ...authRoutes,
  ...workspaceRoutes,
  ...billingRoutes,
  ...usageRoutes,
  ...dataRoutes,
  ...signInRoutes,
  ...eventSourceRoutes,
  ...dataLayoutRoutes,
  ...ontologiesRoutes,
  ...ontologyRoutes,
  ...documentRoutes,
  ...reviewRoutes,
  ...suggestionRoutes,
  ...loadRoutes,
  ...enrichmentRoutes,
  ...aiRoutes,
  ...questionRoutes,
  ...databaseRoutes,
  ...accessRoutes,
].map(guarded);
