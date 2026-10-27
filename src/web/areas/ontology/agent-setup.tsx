import "./ontology.css";
import { useQuery } from "@tanstack/react-query";
import { Bot, Copy } from "lucide-react";
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
import { Callout, canManage } from "./parts";

// Set per stamp in next.config.ts.
const MCP_URL =
  process.env.NEXT_PUBLIC_HELIX_MCP_URL || "https://mcp.helix-db.com/mcp";
// Empty on a stamp without the HTTP endpoint (prod, local mode): no HTTP.
const EVENTS_URL = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL || "";
const WHAT =
  "Copy a prompt for Claude Code, Codex or another agent with the Helix MCP. It takes the setup on from here, and asks you before each change.";

/**
 * "Set up with your agent", for owners and admins: copies the onboarding
 * prompt (./onboarding-prompt.ts) for their own agent, which carries the
 * setup on from where it is: from creating an ontology without `ontology`,
 * else from its setup's step. It describes the workspace's connectors, API
 * sources, document sets (never an API source's own, which the WFE takes
 * only as the source's extraction pick) and the kinds not connected (never
 * API sources, which the prompt makes its own way), read with the Data
 * pages' queries. A card saying what it does; `compact`, the button alone,
 * for a panel's header.
 */
export function AgentSetup({
  ontology,
  compact = false,
}: {
  ontology?: OnboardingOntology;
  compact?: boolean;
}) {
  // A local build has no Helix MCP: its workspace isn't in Helix Cloud.
  const { id, name, role, notify } = useWorkspace(),
    manage = !LOCAL && canManage(role);
  const connectors = useQuery({ ...connectorsQuery(id), enabled: manage }),
    sets = useQuery({ ...documentSetQueries.list(id), enabled: manage }),
    documents = useQuery({ ...documentQueries.workspace(id), enabled: manage }),
    kinds = useQuery({ ...connectorKindsQuery(id), enabled: manage });
  if (!manage) return null;
  const list = connectors.data?.connectors || [];
  const copy = () =>
    navigator.clipboard
      .writeText(
        onboardingPrompt({
          origin: window.location.origin,
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
            (sets.data || []).filter((s) => !isEventSourceSet(s)),
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
    <Callout
      icon={Bot}
      tone="muted"
      title="Set up with your agent"
      text={WHAT}
      action={button}
    />
  );
}
