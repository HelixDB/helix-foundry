// Every in-app URL is built through `paths`. Each area owns its helpers in
// areas/<area>/paths.ts; this file only merges them and keeps the core ones.
import { dataPaths } from "./areas/data/paths";
import { ontologyPaths } from "./areas/ontology/paths";
import { analystPaths } from "./areas/analyst/paths";
import { workspacePaths } from "./areas/workspace/paths";

export const paths = {
  home: () => "/",
  onboarding: () => "/onboarding",
  ...dataPaths,
  ...ontologyPaths,
  ...analystPaths,
  ...workspacePaths,
};
