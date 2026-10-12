import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { Bundle, Resource } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { paths } from "../../paths";
import { useResources, useWorkspace } from "../../ui";
import { Page, PageHeader } from "../../kit";

type Definitions = { activeGeneration: string | null; bundle: Bundle };

export function OntologyDesigner() {
  const { id } = useWorkspace();
  return <OntologyEditorSession key={id} />;
}

function OntologyEditorSession() {
  const { id } = useWorkspace();
  const definitions = useQuery({
    queryKey: [id, "ontology-definitions"],
    queryFn: () => api<Definitions>(`/workspaces/${id}/ontology/definitions`),
    refetchOnWindowFocus: false,
  });
  const [baseline, setBaseline] = useState<Definitions>(),
    [editorEpoch, setEditorEpoch] = useState(0);
  useEffect(() => {
    if (!baseline && definitions.data) setBaseline(definitions.data);
  }, [baseline, definitions.data]);
  const reload = async () => {
    const result = await definitions.refetch();
    if (result.error) throw result.error;
    if (!result.data) throw new Error("Published definitions unavailable");
    setBaseline(result.data);
    setEditorEpoch((epoch) => epoch + 1);
  };
  return (
    <Page>
      <PageHeader
        title="Ontology designer"
        meta={<Link to={paths.ontology()}>Ontology</Link>}
      />
      <p className="muted">
        Map datasets to business objects and connect them by matching columns.
        Create a tested review before publishing. Other accepted definitions
        remain in the proposal.
      </p>
      {baseline ? (
        <MappingEditor key={editorEpoch} current={baseline} reload={reload} />
      ) : definitions.error ? (
        <p role="alert">{definitions.error.message}</p>
      ) : (
        <p>Loading accepted definitions…</p>
      )}
    </Page>
  );
}

function MappingEditor({
  current,
  reload,
}: {
  current: Definitions;
  reload: () => Promise<void>;
}) {
  const { id, role } = useWorkspace(),
    navigate = useNavigate();
  const datasets = useResources("dataset", { limit: 500 });
  const [types, setTypes] = useState(current.bundle.ontology),
    [relationships, setRelationships] = useState(current.bundle.relationships),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const columns = (datasetId: string): string[] =>
    datasets.data?.items
      .find((d: Resource) => d.id === datasetId)
      ?.data.profile.columns.map((c: any) => c.name) || [];
  const typeColumns = (name: string) =>
    columns(types.find((t) => t.name === name)?.datasetId || "");
  const updateType = (
    index: number,
    value: Partial<Bundle["ontology"][number]>,
  ) => setTypes(types.map((t, i) => (i === index ? { ...t, ...value } : t)));
  const updateRelationship = (
    index: number,
    value: Partial<Bundle["relationships"][number]>,
  ) =>
    setRelationships(
      relationships.map((r, i) => (i === index ? { ...r, ...value } : r)),
    );
  const incomplete =
    !types.length ||
    types.some(
      (t) =>
        !t.name.trim() || !t.datasetId || !t.primaryKey || !t.properties.length,
    ) ||
    relationships.some(
      (r) =>
        !r.name.trim() ||
        !r.fromType ||
        !r.toType ||
        !r.fromColumn ||
        !r.toColumn,
    );
  return (
    <section aria-label="Ontology mappings" className="ontology-designer">
      {datasets.error && <p role="alert">{datasets.error.message}</p>}
      <p>
        Accepted generation:{" "}
        <code>{current.activeGeneration || "No publication yet"}</code>
      </p>
      <h2>Object types</h2>
      {types.map((t, index) => (
        <fieldset key={index} aria-label={`Object type ${index + 1}`}>
          <legend>Object type {index + 1}</legend>
          <label>
            Object type name
            <input
              value={t.name}
              disabled={role === "viewer"}
              onChange={(e) => updateType(index, { name: e.target.value })}
            />
          </label>
          <label>
            Description
            <input
              value={t.description}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateType(index, { description: e.target.value })
              }
            />
          </label>
          <label>
            Dataset
            <select
              value={t.datasetId}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateType(index, {
                  datasetId: e.target.value,
                  primaryKey: "",
                  properties: [],
                })
              }
            >
              <option value="">Choose a dataset</option>
              {datasets.data?.items.map((d: Resource) => (
                <option key={d.id} value={d.id}>
                  {d.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Primary key
            <select
              value={t.primaryKey}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateType(index, {
                  primaryKey: e.target.value,
                  properties: [...new Set([...t.properties, e.target.value])],
                })
              }
            >
              <option value="">Choose a unique column</option>
              {columns(t.datasetId).map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
          <fieldset>
            <legend>Properties</legend>
            {columns(t.datasetId).map((c) => (
              <label className="mapping-property" key={c}>
                <input
                  type="checkbox"
                  checked={t.properties.includes(c)}
                  disabled={role === "viewer" || c === t.primaryKey}
                  onChange={(e) =>
                    updateType(index, {
                      properties: e.target.checked
                        ? [...t.properties, c]
                        : t.properties.filter((p) => p !== c),
                    })
                  }
                />
                {c}
              </label>
            ))}
          </fieldset>
          {role !== "viewer" && (
            <button
              onClick={() => setTypes(types.filter((_, i) => i !== index))}
            >
              Remove object type {index + 1}
            </button>
          )}
        </fieldset>
      ))}
      {role !== "viewer" && (
        <button
          disabled={types.length >= 20}
          onClick={() =>
            setTypes([
              ...types,
              {
                name: "",
                description: "",
                datasetId: "",
                primaryKey: "",
                properties: [],
              },
            ])
          }
        >
          Add object type
        </button>
      )}
      <h2>Relationships</h2>
      <p className="muted">
        A link connects records where the selected source and target columns
        match. Renaming or removing a type may require updating its links and
        existing actions; validation reports missing references.
      </p>
      {relationships.map((r, index) => (
        <fieldset key={index} aria-label={`Relationship ${index + 1}`}>
          <legend>Relationship {index + 1}</legend>
          <label>
            Relationship name
            <input
              value={r.name}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateRelationship(index, { name: e.target.value })
              }
            />
          </label>
          <label>
            From object type
            <select
              value={r.fromType}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateRelationship(index, {
                  fromType: e.target.value,
                  fromColumn: "",
                })
              }
            >
              <option value="">Choose a type</option>
              {types
                .filter((t) => t.name)
                .map((t) => (
                  <option key={t.name} value={t.name}>
                    {t.name}
                  </option>
                ))}
            </select>
          </label>
          <label>
            From column
            <select
              value={r.fromColumn}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateRelationship(index, { fromColumn: e.target.value })
              }
            >
              <option value="">Choose a column</option>
              {typeColumns(r.fromType).map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
          <label>
            To object type
            <select
              value={r.toType}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateRelationship(index, {
                  toType: e.target.value,
                  toColumn: "",
                })
              }
            >
              <option value="">Choose a type</option>
              {types
                .filter((t) => t.name)
                .map((t) => (
                  <option key={t.name} value={t.name}>
                    {t.name}
                  </option>
                ))}
            </select>
          </label>
          <label>
            To column
            <select
              value={r.toColumn}
              disabled={role === "viewer"}
              onChange={(e) =>
                updateRelationship(index, { toColumn: e.target.value })
              }
            >
              <option value="">Choose a column</option>
              {typeColumns(r.toType).map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
          {role !== "viewer" && (
            <button
              onClick={() =>
                setRelationships(relationships.filter((_, i) => i !== index))
              }
            >
              Remove relationship {index + 1}
            </button>
          )}
        </fieldset>
      ))}
      {role !== "viewer" && (
        <button
          disabled={relationships.length >= 30}
          onClick={() =>
            setRelationships([
              ...relationships,
              {
                name: "",
                fromType: "",
                toType: "",
                fromColumn: "",
                toColumn: "",
              },
            ])
          }
        >
          Add relationship
        </button>
      )}
      <div className="buttons">
        {role !== "viewer" && (
          <button
            disabled={pending || incomplete}
            onClick={async () => {
              setPending(true);
              setError("");
              try {
                const p = await api(`/workspaces/${id}/ontology/draft`, {
                  expectedGeneration: current.activeGeneration,
                  ontology: types,
                  relationships,
                });
                navigate(paths.proposals(p.id));
              } catch (e) {
                setError((e as Error).message);
              } finally {
                setPending(false);
              }
            }}
          >
            Create mapping review
          </button>
        )}
        <button
          disabled={pending}
          onClick={async () => {
            setPending(true);
            setError("");
            try {
              await reload();
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setPending(false);
            }
          }}
        >
          Reload accepted definitions
        </button>
      </div>
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
