import { useQuery } from "@tanstack/react-query";
import { api } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { SuggestedOntologyView } from "../../suggested-ontology";
import type {
  BusinessInsights,
  OnboardingState,
  Resource,
  SuggestedEntity,
  SuggestedLink,
  SuggestedOntology,
} from "../../../../../packages/shared/src";

type Relationship = {
  name: string;
  fromType: string;
  toType: string;
  fromColumn: string;
  toColumn: string;
};

// The published object types and relationships in the shape of the setup
// suggestion, so the Ontology page draws the same schema. Merged sources,
// activity streams and plain-language link names come from the ontology
// confirmed in setup, when there is one; what is drawn is what was published.
export function publishedOntology(
  types: Resource[],
  relationships: Relationship[],
  previews: Record<string, any>,
  datasets: Resource[],
  confirmed?: SuggestedOntology,
): SuggestedOntology {
  const byName = new Map((confirmed?.entities || []).map((e) => [e.name, e]));
  const dataset = new Map(datasets.map((d) => [d.id, d]));
  const entities: SuggestedEntity[] = types.map((t) => {
    const c = byName.get(t.name),
      d = dataset.get(t.data.datasetId),
      rows = t.data.objectCount ?? c?.rows ?? 0;
    return {
      id: t.data.typeId || t.id,
      name: t.name,
      description: c?.description || t.description || "",
      kind: "record",
      anchorDatasetId: c?.anchorDatasetId || t.data.datasetId,
      key: t.data.primaryKey,
      rows,
      members: c?.members || [
        {
          datasetId: t.data.datasetId,
          datasetName: d?.name || t.name,
          source: d?.data.sampleSource || "",
          key: t.data.primaryKey,
          rows,
          matched: rows,
          via: null,
        },
      ],
      properties: t.data.properties || [],
    };
  });
  const idOf = new Map(entities.map((e) => [e.name, e.id]));
  // Activity streams feed metrics rather than records, so they are not types.
  const streams = (confirmed?.entities || []).filter(
    (e) => e.kind === "activity" && !idOf.has(e.name),
  );
  for (const e of streams) idOf.set(e.name, e.id);
  const confirmedName = new Map(
    (confirmed?.entities || []).map((e) => [e.id, e.name]),
  );
  const typeOf = new Map(types.map((t) => [t.name, t]));
  const links: SuggestedLink[] = relationships.flatMap((r, i) => {
    const from = idOf.get(r.fromType),
      to = idOf.get(r.toType);
    if (!from || !to) return [];
    const c = confirmed?.links.find(
      (l) =>
        confirmedName.get(l.from) === r.fromType &&
        confirmedName.get(l.to) === r.toType,
    );
    const counts = previews["relation:" + r.name]?.rows?.[0];
    return [
      {
        id: "r" + i,
        name: c?.name || r.name,
        from,
        to,
        fromDatasetId: typeOf.get(r.fromType)?.data.datasetId || "",
        fromColumn: r.fromColumn,
        toDatasetId: typeOf.get(r.toType)?.data.datasetId || "",
        toColumn: r.toColumn,
        totalRows: Number(counts?.total ?? c?.totalRows ?? 0),
        matchedRows: Number(counts?.matched ?? c?.matchedRows ?? 0),
        caseInsensitive: c?.caseInsensitive ?? false,
        evidence: c?.evidence ?? "name",
      },
    ];
  });
  for (const l of confirmed?.links || [])
    if (streams.some((e) => e.id === l.from || e.id === l.to)) {
      const from = idOf.get(confirmedName.get(l.from) || ""),
        to = idOf.get(confirmedName.get(l.to) || "");
      if (from && to) links.push({ ...l, from, to });
    }
  return {
    signature: "",
    hash: "",
    summary: "",
    described: true,
    entities: [...entities, ...streams],
    links,
    notes: (confirmed?.notes || []).filter((n) => n.kind === "merged"),
    unplaced: [],
  };
}

export function PublishedSchema({
  types,
  relationships,
  previews,
  datasets,
}: {
  types: Resource[];
  relationships: Relationship[];
  previews: Record<string, any>;
  datasets: Resource[];
}) {
  const { id } = useWorkspace();
  const setup = useQuery({
    queryKey: [id, "onboarding"],
    queryFn: () => api<OnboardingState>(`/workspaces/${id}/onboarding`),
    staleTime: 60000,
  });
  const confirmed = setup.data?.buildRun?.data.ontology as
    | SuggestedOntology
    | undefined;
  const ontology = publishedOntology(
    types,
    relationships,
    previews,
    datasets,
    confirmed,
  );
  // The industry playbook's view of this data, worked out in the background.
  const business = useQuery({
    queryKey: [id, "business"],
    queryFn: () => api<BusinessInsights>(`/workspaces/${id}/business`),
    staleTime: 60000,
  });
  const b = business.data;
  const typeIds = new Set(types.map((t) => t.data.typeId || t.id));
  return (
    <SuggestedOntologyView
      business={
        b?.industry
          ? {
              checks: b.checks
                .filter((c) => c.kind === "definition" || !c.ok)
                .map((c) => c.text),
              suggestions: b.suggestions,
            }
          : undefined
      }
      readOnly
      ontology={ontology}
      datasets={datasets}
      entityHref={(e) =>
        typeIds.has(e.id)
          ? paths.objectType(e.id)
          : e.kind === "activity"
            ? paths.dataset(e.anchorDatasetId)
            : undefined
      }
    />
  );
}
