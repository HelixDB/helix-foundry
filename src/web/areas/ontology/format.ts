import type { OntologyVersion } from "../../../shared/wfe";

// A version's stored format. Kept apart from model.ts so the shell (sidebar,
// palette) can name formats without loading the TOML parser.

export type Format = "foundry_v1" | "agent_bundle";

/**
 * A version's format: the WFE's `format`, else the definition's own `format`
 * field, else Definition v1 (versions from before formats existed).
 *
 * @example
 * versionFormat({ id: "v", format: "ONTOLOGY_FORMAT_AGENT_BUNDLE" }); // "agent_bundle"
 * versionFormat({ id: "v" }); // "foundry_v1"
 */
export function versionFormat(v: OntologyVersion): Format {
  if (v.format === "ONTOLOGY_FORMAT_AGENT_BUNDLE") return "agent_bundle";
  if (v.format === "ONTOLOGY_FORMAT_FOUNDRY_V1") return "foundry_v1";
  return v.definition?.format === "agent_bundle"
    ? "agent_bundle"
    : "foundry_v1";
}
export const formatLabel: Record<Format, string> = {
  foundry_v1: "Definition v1",
  agent_bundle: "Agent draft",
};
