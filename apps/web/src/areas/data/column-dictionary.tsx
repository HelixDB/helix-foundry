import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../../api";
import { useAction, useWorkspace } from "../../ui";
import { Button, Dialog, Skeleton } from "../../kit";
import type {
  ColumnDictionary,
  ColumnDefinition,
} from "../../../../../packages/shared/src";

export function ColumnDictionaryEditor({ datasetId }: { datasetId: string }) {
  const { id, role } = useWorkspace(),
    [editing, setEditing] = useState(false);
  const query = useQuery({
    queryKey: [id, "column-dictionary", datasetId],
    queryFn: () =>
      api<ColumnDictionary>(
        `/workspaces/${id}/datasets/${datasetId}/dictionary`,
      ),
  });
  return (
    <section className="dc-dictionary" aria-label="Column dictionary">
      <h3>Column dictionary</h3>
      <p className="muted">
        Describe what columns mean and their units. The Analyst receives these
        owner definitions as context; they do not convert values or change
        imported data.
      </p>
      {query.isPending ? (
        <Skeleton height={80} />
      ) : query.error ? (
        <p role="alert">{query.error.message}</p>
      ) : (
        <>
          <dl>
            {query.data.columns.map((column) => {
              const definition = query.data.definitions.find(
                (d) => d.column === column.name,
              );
              return (
                <div key={column.name}>
                  <dt>
                    <code>{column.name}</code> <small>{column.type}</small>
                  </dt>
                  <dd>
                    {definition?.meaning || "No definition"}
                    {definition?.unit && (
                      <span> · Unit: {definition.unit}</span>
                    )}
                  </dd>
                </div>
              );
            })}
          </dl>
          {role !== "viewer" && (
            <Button
              pending={query.isFetching}
              onClick={async () => {
                const latest = await query.refetch();
                if (latest.data && !latest.error) setEditing(true);
              }}
            >
              Edit column dictionary
            </Button>
          )}
        </>
      )}
      {editing && query.data && (
        <DictionaryForm
          dictionary={query.data}
          onClose={() => setEditing(false)}
        />
      )}
    </section>
  );
}
function DictionaryForm({
  dictionary,
  onClose,
}: {
  dictionary: ColumnDictionary;
  onClose: () => void;
}) {
  const { id } = useWorkspace(),
    action = useAction();
  const [snapshot] = useState(dictionary);
  const names = new Set(snapshot.columns.map((c) => c.name));
  const [definitions, setDefinitions] = useState<ColumnDefinition[]>([
    ...snapshot.columns.map(
      (c) =>
        snapshot.definitions.find((d) => d.column === c.name) || {
          column: c.name,
          meaning: "",
          unit: "",
        },
    ),
    ...snapshot.definitions.filter((d) => !names.has(d.column)),
  ]);
  const change = (column: string, field: "meaning" | "unit", value: string) =>
    setDefinitions((rows) =>
      rows.map((d) => (d.column === column ? { ...d, [field]: value } : d)),
    );
  const save = async () => {
    const result = await action.run(
      () =>
        api<ColumnDictionary>(
          `/workspaces/${id}/datasets/${snapshot.datasetId}/dictionary`,
          {
            revision: snapshot.revision,
            datasetRevision: snapshot.datasetRevision,
            definitions: definitions.filter(
              (d) => d.meaning.trim() || d.unit.trim(),
            ),
          },
          "PUT",
        ),
      "Column dictionary saved",
    );
    if (result) onClose();
  };
  return (
    <Dialog
      title="Edit column dictionary"
      onClose={onClose}
      footer={
        <>
          <Button disabled={action.pending} onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" pending={action.pending} onClick={save}>
            Save dictionary
          </Button>
        </>
      }
    >
      <p>
        Definitions stay with this dataset through refreshes. Removed columns
        keep their definitions but are omitted from Analyst context until they
        return. Clear both fields to remove a definition.
      </p>
      <div className="dc-dictionary-form">
        {definitions.map((d) => (
          <fieldset key={d.column}>
            <legend>
              {d.column}
              {!names.has(d.column) && " · Not in the current snapshot"}
            </legend>
            <label>
              Meaning for {d.column}
              <textarea
                maxLength={1000}
                rows={2}
                disabled={action.pending}
                value={d.meaning}
                onChange={(e) => change(d.column, "meaning", e.target.value)}
              />
            </label>
            <label>
              Unit for {d.column}
              <input
                maxLength={80}
                disabled={action.pending}
                value={d.unit}
                placeholder="For example: cents, USD, milliseconds"
                onChange={(e) => change(d.column, "unit", e.target.value)}
              />
            </label>
          </fieldset>
        ))}
      </div>
    </Dialog>
  );
}
