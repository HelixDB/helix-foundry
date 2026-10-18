import RecordPicker from "./record-picker";
import { useState } from "react";
import { useWorkspace, useAction, DataTable } from "./ui";
import { api } from "./api";
import type { Resource } from "../../../packages/shared/src";
export default function ObjectActionForm({
  types,
  onDone,
  initialRecord,
}: {
  types: Resource[];
  initialRecord?: Resource;
  onDone: () => void;
}) {
  const { id } = useWorkspace(),
    action = useAction();
  const [operation, setOperation] = useState(initialRecord ? "link" : "create"),
    [typeId, setTypeId] = useState(types[0]?.id || ""),
    [values, setValues] = useState<Record<string, unknown>>({}),
    [from, setFrom] = useState(initialRecord?.id || ""),
    [to, setTo] = useState(""),
    [name, setName] = useState("Related to"),
    [preview, setPreview] = useState<any>(null);
  const type = types.find((t) => t.id === typeId),
    change =
      operation === "create"
        ? { operation, objectType: type?.name, values }
        : operation === "merge"
          ? { operation: "merge" as const, primaryId: from, secondaryId: to }
          : { operation, name, from, to };
  const invalidate = () => setPreview(null);
  return (
    <div className="pad">
      <label>
        Action
        <select
          value={operation}
          onChange={(e) => {
            setOperation(e.target.value);
            invalidate();
          }}
        >
          <option value="create">Create a record</option>
          <option value="link">Create a relationship</option>
          <option value="unlink">Remove a relationship</option>
          <option value="merge">Merge a record into another</option>
        </select>
      </label>
      {operation === "create" ? (
        <>
          <label>
            Record type
            <select
              value={typeId}
              onChange={(e) => {
                setTypeId(e.target.value);
                setValues({});
                invalidate();
              }}
            >
              {types.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>
          </label>
          <div className="form-grid">
            {type?.data.properties.map((field: string) => (
              <label key={field}>
                {field}
                {field === type.data.primaryKey ? " · required key" : ""}
                <input
                  value={String(values[field] ?? "")}
                  onChange={(e) => {
                    setValues((v) => ({ ...v, [field]: e.target.value }));
                    invalidate();
                  }}
                />
              </label>
            ))}
          </div>
        </>
      ) : operation === "merge" ? (
        <>
          <div className="form-grid">
            <RecordPicker
              label="Kept record"
              value={from}
              initial={initialRecord}
              onChange={(v) => {
                setFrom(v);
                invalidate();
              }}
            />
            <RecordPicker
              label="Merged record"
              value={to}
              onChange={(v) => {
                setTo(v);
                invalidate();
              }}
            />
          </div>
          <p>
            The merged record's relationships move to the kept one, which keeps
            its values; the merged record disappears from the explorer.
          </p>
        </>
      ) : (
        <>
          <label>
            Relationship name
            <input
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                invalidate();
              }}
            />
          </label>
          <div className="form-grid">
            {[
              ["From object", from, setFrom],
              ["To object", to, setTo],
            ].map(([label, value, set]: any) => (
              <RecordPicker
                key={label}
                label={label.replace("object", "record")}
                value={value}
                initial={initialRecord}
                onChange={(v) => {
                  set(v);
                  invalidate();
                }}
              />
            ))}
          </div>
        </>
      )}
      {preview && (
        <div className="notice">
          <div>
            <strong>Review the affected records</strong>
            {operation === "create" ? (
              <DataTable rows={[preview.evidence.after]} />
            ) : operation === "merge" ? (
              <p>
                {preview.evidence.primaryName} keeps its values
                {preview.evidence.rewiredRelations > 0
                  ? `, along with ${preview.evidence.rewiredRelations} ${
                      preview.evidence.rewiredRelations === 1
                        ? "relationship"
                        : "relationships"
                    } moved from ${preview.evidence.secondaryName}`
                  : ""}
                ; {preview.evidence.secondaryName} disappears from the explorer.
              </p>
            ) : (
              <p>
                {preview.evidence.fromName} → {preview.evidence.toName}
                <br />
                {name}: {preview.evidence.before ? "present" : "absent"} →{" "}
                {preview.evidence.after ? "present" : "absent"}
              </p>
            )}
          </div>
        </div>
      )}
      <div className="modal-actions">
        <button
          className="button"
          disabled={action.pending}
          onClick={() =>
            action.run(async () =>
              setPreview(
                await api(`/workspaces/${id}/actions/change/preview`, change),
              ),
            )
          }
        >
          Preview changes
        </button>
        {preview && (
          <button
            className="button primary"
            disabled={action.pending}
            onClick={() =>
              action.run(async () => {
                await api(`/workspaces/${id}/actions/change/execute`, {
                  action: preview.action,
                  hash: preview.hash,
                  idempotencyKey: crypto.randomUUID(),
                });
                onDone();
              }, "Action recorded")
            }
          >
            Apply changes
          </button>
        )}
      </div>
    </div>
  );
}
