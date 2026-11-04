import "./ontology.css";
import { useQuery } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { Button, connectorLabel } from "../../kit";
import { useWorkspace } from "../../ui";
import { connectorKindsQuery, connectorsQuery } from "../data/data-api";
import { isEventSource, setFolders } from "../data/model";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { setLabel } from "../documents/model";
import { promptDocuments, promptEventSource } from "./goal-prompt";
import { isEventSourceSet } from "./model";
import { onboardingPrompt, type OnboardingOntology } from "./onboarding-prompt";
import { canManage } from "./parts";
import { isSampleConnector, isSampleSet } from "./sample";

// Set per stamp in next.config.ts.
export const MCP_URL =
  process.env.NEXT_PUBLIC_HELIX_MCP_URL || "https://mcp.helix-db.com/mcp";
// Empty on a stamp without the HTTP endpoint (prod, local mode): no HTTP.
const EVENTS_URL = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL || "";
const WHAT =
  "Copy a prompt into Claude Code, Codex or another agent, and it does the setup with you.";

/**
 * "Set up with your agent", for owners and admins: copies the onboarding
 * prompt (./onboarding-prompt.ts) for their own agent, which carries the
 * setup on from where it is: from creating an ontology without `ontology`,
 * else from its setup's step. It describes the workspace's connectors, API
 * sources, document sets (never an API source's own, which the WFE takes
 * only as the source's extraction pick) and the kinds not connected (never
 * API sources, which the prompt makes its own way), read with the Data
 * pages' queries. Of these the sample company's connector and set are left
 * out, as the sample's data is no source of the user's own ontology, unless
 * `keepSample`: the sample's own. A sentence saying what it does and the
 * button, for an `.on-card` to lay out (Get started's); `compact`, the button
 * alone, for a panel's header.
 */
export function AgentSetup({
  ontology,
  compact = false,
  keepSample = false,
}: {
  ontology?: OnboardingOntology;
  compact?: boolean;
  keepSample?: boolean;
}) {
  // Both hosted and local owners can continue setup through their MCP.
  const { id, name, role, notify } = useWorkspace(),
    manage = canManage(role);
  const connectors = useQuery({ ...connectorsQuery(id), enabled: manage }),
    sets = useQuery({ ...documentSetQueries.list(id), enabled: manage }),
    documents = useQuery({ ...documentQueries.workspace(id), enabled: manage }),
    kinds = useQuery({ ...connectorKindsQuery(id), enabled: manage });
  if (!manage) return null;
  const list = (connectors.data?.connectors || []).filter(
    (c) => keepSample || !isSampleConnector(c),
  );
  const copy = () =>
    navigator.clipboard
      .writeText(
        onboardingPrompt({
          origin: window.location.origin,
          local: LOCAL,
          mcpUrl: MCP_URL,
          eventsUrl: EVENTS_URL,
          workspace: { id, name },
          ontology: ontology ?? null,
          connectors: list
            .filter((c) => !isEventSource(c.kind))
            .map((c) => ({
              id: c.id,
              name: c.name || connectorLabel(c.kind),
              kind: connectorLabel(c.kind),
              tables: c.tables || [],
            })),
          eventSources: list
            .filter((c) => isEventSource(c.kind))
            .map((c) => ({ id: c.id, ...promptEventSource(c) })),
          sets: setFolders(
            (sets.data || []).filter(
              (s) => !isEventSourceSet(s) && (keepSample || !isSampleSet(s)),
            ),
            documents.data || [],
          ).map((f) => ({
            id: f.set.id,
            name: setLabel(f.set),
            description: f.set.description,
            // A connector's set holds text only, none of it in the uploads.
            ...(f.set.connector
              ? { documents: f.set.documents || 0, names: [], files: [] }
              : promptDocuments(f.documents)),
          })),
          kinds: (kinds.data?.kinds || [])
            .filter(
              (k) =>
                !isEventSource(k.kind) && !list.some((c) => c.kind === k.kind),
            )
            .map((k) => ({
              kind: k.kind,
              label: k.displayName || connectorLabel(k.kind),
              available: k.available !== false,
            })),
        }),
      )
      .then(() => notify("Copied the prompt: paste it into your agent."))
      .catch(() => notify("Couldn’t copy the prompt.", true));
  const button = (
    <Button
      size="sm"
      icon={Copy}
      disabled={[connectors, sets, documents, kinds].some((q) => q.isPending)}
      title={compact ? WHAT : undefined}
      onClick={copy}
    >
      {compact ? "Set up with your agent" : "Copy prompt"}
    </Button>
  );
  return compact ? (
    button
  ) : (
    <>
      <div className="on-card-text">
        <p>{WHAT}</p>
      </div>
      {button}
    </>
  );
}
