import type { MockRoute } from "../http";
import { accessRoutes, directoryRoutes, guarded } from "./access";
import { agentRoutes, whileOn } from "./agent";
import { agentHostRoutes } from "./agent-hosts";
import { aiRoutes } from "./ai";
import { authRoutes } from "./auth";
import { automationRoutes } from "./automations";
import { automationSuggestionRoutes } from "./automation-suggestions";
import { billingRoutes } from "./billing";
import { dataRoutes } from "./data";
import { dashboardRoutes } from "./dashboards";
import { dataLayoutRoutes } from "./data-layout";
import { databaseRoutes } from "./databases";
import { documentRoutes } from "./documents";
import { enrichmentRoutes } from "./enrichment";
import { eventSourceRoutes } from "./event-sources";
import { feedbackRoutes } from "./feedback";
import { reviewRoutes } from "./reviews";
import { signInRoutes } from "./sign-ins";
import { loadRoutes } from "./loads";
import { ontologiesRoutes } from "./ontologies";
import { ontologyRoutes } from "./ontology";
import { questionRoutes } from "./questions";
import { suggestionRoutes } from "./suggestions";
import { usageRoutes } from "./usage";
import { workflowRoutes } from "./workflows";
import { workspaceRoutes } from "./workspaces";

// Every WFE route the mock serves. An area adds a module next to these
// (e.g. ./connectors.ts exporting connectorRoutes) and spreads it in here.
// Each route must also be on the /papi allowlist to be reachable from the app.
// Foundry's routes run source grants' access check first (./access.ts).
export const routes: MockRoute[] = [
  ...authRoutes,
  ...feedbackRoutes,
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
  ...workflowRoutes,
  ...loadRoutes,
  ...enrichmentRoutes,
  ...aiRoutes,
  ...questionRoutes,
  ...whileOn([...agentRoutes, ...agentHostRoutes]),
  ...dashboardRoutes,
  ...automationRoutes,
  ...automationSuggestionRoutes,
  ...databaseRoutes,
  ...accessRoutes,
  ...directoryRoutes,
].map(guarded);
