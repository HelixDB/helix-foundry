import { ArrowRight, Copy, KeyRound, ListChecks } from "lucide-react";
import { Button, DataGrid, EmptyState, Tag } from "../../kit";
import { useWorkspace } from "../../ui";
import { foundryGraph, type FoundryView } from "./model";
import type { Sources } from "./sources";
import { TypeCards } from "./version-parts";

// The tabs of a Definition v1 version: entities on one synced table each,
// relationships joining their properties, SQL rules and constraints, and the
// definition as stored.

const cardinality: Record<string, string> = {
  one_to_one: "One to one",
  one_to_many: "One to many",
  many_to_one: "Many to one",
  many_to_many: "Many to many",
};

export function FoundryEntities({
  view,
  sources,
}: {
  view: FoundryView;
  sources: Sources;
}) {
  if (!view.entities.length)
    return <EmptyState title="No entities" text="This version defines none." />;
  const graph = foundryGraph(view);
  return (
    <TypeCards
      sources={sources}
      items={view.entities.map((e, i) => ({
        name: e.name,
        description: e.description || "",
        badges: e.primary_key ? (
          <Tag tone="accent" icon={KeyRound}>
            {e.primary_key}
          </Tag>
        ) : undefined,
        props: [
          ...(e.primary_key &&
          !e.properties.some((p) => p.name === e.primary_key)
            ? [{ name: e.primary_key, type: "", key: true }]
            : []),
          ...e.properties.map((p) => ({
            ...p,
            key: p.name === e.primary_key,
          })),
        ],
        sources: graph.nodes[i]!.sources,
      }))}
    />
  );
}

export function FoundryRelationships({ view }: { view: FoundryView }) {
  return (
    <DataGrid
      label="Relationships"
      rows={view.relationships}
      getRowKey={(r) => r.name}
      maxHeight={640}
      emptyText="This version defines no relationships."
      columns={[
        {
          key: "name",
          label: "Relationship",
          width: 180,
          render: (r) => <span className="hf-mono">{r.name}</span>,
        },
        {
          key: "from",
          label: "From → to",
          width: 220,
          render: (r) => (
            <span className="on-endpoints">
              {r.from} <ArrowRight size={12} aria-label="to" /> {r.to}
            </span>
          ),
        },
        {
          key: "from_property",
          label: "Joins on",
          width: 275,
          render: (r) =>
            r.from_property && r.to_property ? (
              <span className="hf-mono on-cell-text">
                {r.from}.{r.from_property} = {r.to}.{r.to_property}
              </span>
            ) : (
              "—"
            ),
        },
        {
          key: "cardinality",
          label: "Cardinality",
          width: 140,
          render: (r) =>
            cardinality[r.cardinality || ""] || r.cardinality || "—",
        },
      ]}
    />
  );
}

export function FoundryRules({ view }: { view: FoundryView }) {
  if (!view.rules.length)
    return (
      <EmptyState
        icon={ListChecks}
        title="No rules"
        text="This version derives nothing from its entities."
      />
    );
  return (
    <div className="on-groups">
      <section className="on-group">
        <p className="on-muted">
          Each rule is a SQL condition on its entity’s rows.
        </p>
        {view.rules.map((r) => (
          <section key={r.name} className="on-item">
            <header>
              <h3 className="hf-mono">{r.name}</h3>
              {r.entity ? <Tag>{r.entity}</Tag> : <Tag>Whole ontology</Tag>}
            </header>
            {r.description && <p>{r.description}</p>}
            <pre className="on-code">
              <code>{r.expression}</code>
            </pre>
          </section>
        ))}
      </section>
    </div>
  );
}

const kindLabel: Record<string, string> = {
  unique: "Unique",
  not_null: "Not null",
  reference: "Reference",
  check: "Check",
};
export function FoundryConstraints({ view }: { view: FoundryView }) {
  return (
    <DataGrid
      label="Constraints"
      rows={view.constraints}
      getRowKey={(r) => r.name}
      maxHeight={640}
      emptyText="This version defines no constraints."
      columns={[
        {
          key: "name",
          label: "Constraint",
          width: 180,
          render: (r) => <span className="hf-mono">{r.name}</span>,
        },
        { key: "entity", label: "Entity", width: 140 },
        {
          key: "kind",
          label: "Kind",
          width: 120,
          render: (r) => <Tag>{kindLabel[r.kind || ""] || r.kind || "—"}</Tag>,
        },
        {
          key: "properties",
          label: "Properties",
          // A list, shown as text: a json column would take twice the room.
          type: "string",
          width: 200,
          render: (r) => (
            <span className="hf-mono on-cell-text">
              {r.properties.join(", ") || "—"}
            </span>
          ),
        },
        {
          key: "expression",
          label: "Check",
          width: 220,
          render: (r) =>
            r.expression ? (
              <span className="hf-mono on-cell-text" title={r.expression}>
                {r.expression}
              </span>
            ) : (
              "—"
            ),
        },
      ]}
    />
  );
}

/** The definition as stored, for copying into another tool. */
export function FoundryJson({
  definition,
}: {
  definition: Record<string, unknown>;
}) {
  const { notify } = useWorkspace();
  const json = JSON.stringify(definition, null, 2);
  return (
    <div className="on-toml">
      <div className="on-graph-bar">
        <span className="on-muted">definition.json</span>
        <Button
          size="sm"
          icon={Copy}
          onClick={() =>
            navigator.clipboard
              .writeText(json)
              .then(() => notify("Copied the definition."))
              .catch(() => notify("Couldn’t copy the definition.", true))
          }
        >
          Copy
        </Button>
      </div>
      <pre className="on-code on-code--file" aria-label="definition.json">
        <code>{json}</code>
      </pre>
    </div>
  );
}
