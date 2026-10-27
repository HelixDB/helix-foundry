import { useMemo } from "react";
import { plural } from "../../kit";
import { paths } from "../../paths";
import { useOntology } from "./scope";
import type { Graph } from "./model";
import type { Sources } from "./sources";
import {
  OntologySchema,
  type SchemaEntity,
  type SchemaLink,
} from "./suggested-ontology";

// A version's graph as the suggested-ontology schema: every type as a card,
// with its links drawn and named.

const cardinality: Record<string, string> = {
  one_to_one: "one to one",
  one_to_many: "one to many",
  many_to_one: "many to one",
  many_to_many: "many to many",
};

export function GraphView({
  graph,
  sources,
  versionId,
  tab,
}: {
  graph: Graph;
  sources: Sources;
  versionId: string;
  // The tab that lists the types, for the cards' links.
  tab: string;
}) {
  const { id: o } = useOntology();
  const entities: SchemaEntity[] = useMemo(
    () =>
      graph.nodes.map((n) => {
        const members = n.sources.map(sources.member);
        return {
          id: n.name,
          name: n.name,
          description: n.description,
          kind: "record",
          key: n.key.join(", "),
          // A Definition v1 entity is its table's rows; an agent type's
          // count isn't known before ingest (one table can make several).
          rows: n.sources.reduce(
            (sum, s) =>
              sum + (s.kind === "table" ? sources.resolve(s)?.rows || 0 : 0),
            0,
          ),
          members,
          properties: n.props.map((p) => p.name),
        };
      }),
    // sources.member changes identity each render; sources.key tracks its data.
    [graph, sources.key],
  );
  const links: SchemaLink[] = useMemo(
    () =>
      graph.edges.map((e) => ({
        id: e.name,
        name: e.name,
        from: e.from,
        to: e.to,
        facts:
          e.cardinality === "one" || e.cardinality === "many"
            ? `${e.cardinality}${e.required ? " · required" : ""}`
            : cardinality[e.cardinality] || e.cardinality,
        description: e.description || undefined,
        code: e.via,
      })),
    [graph],
  );
  return (
    <div className="on-graph">
      <div className="on-graph-bar">
        <span className="on-graph-count">
          {plural(graph.nodes.length, "type")} ·{" "}
          {plural(graph.edges.length, "link")}
        </span>
      </div>
      <OntologySchema
        entities={entities}
        links={links}
        entityHref={(e) =>
          paths.version(o, versionId, tab) + "#type-" + encodeURIComponent(e.id)
        }
      />
    </div>
  );
}
