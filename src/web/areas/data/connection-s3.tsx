import { useId } from "react";
import { Copy, KeyRound, ShieldCheck } from "lucide-react";
import type { ConnectorTable, S3ConnectorSetup } from "../../../shared/wfe";
import { Button, cx, plural } from "../../kit";
import { useWorkspace } from "../../ui";
import {
  formatBytes,
  keySummary,
  parseS3Uri,
  previewGrid,
  s3PermissionsPolicy,
  s3TrustPolicy,
  type ConnectionForm,
} from "./model";

// The S3 kind's parts of the connection form: where the files are, how
// Helix gets in (an IAM role it assumes with the workspace's external ID, or
// an access key), and a tested table's files, columns and preview.

type S3Form = Extract<ConnectionForm, { kind: "s3" }>;

function useCopy() {
  const { notify } = useWorkspace();
  return (text: string, what: string) =>
    navigator.clipboard
      .writeText(text)
      .then(() => notify(`Copied the ${what}.`))
      .catch(() => notify(`Couldn’t copy the ${what}.`, true));
}

// A JSON policy the customer pastes into AWS, with its copy button.
function Policy({ name, json }: { name: string; json: string }) {
  const copy = useCopy();
  return (
    <figure className="dc-policy" aria-label={name}>
      <pre>{json}</pre>
      <Button
        size="sm"
        variant="ghost"
        icon={Copy}
        aria-label={`Copy ${name}`}
        onClick={() => void copy(json, name)}
      >
        Copy
      </Button>
    </figure>
  );
}

export function S3Fields({
  form,
  setup,
  editing,
  storedKey,
  onChange,
}: {
  form: S3Form;
  // What a role must trust; missing where role access is off.
  setup?: S3ConnectorSetup | null;
  editing: boolean;
  // The stored connector reads with a key, so its secret may stay blank.
  storedKey: boolean;
  onChange: (patch: Partial<S3Form>) => void;
}) {
  const copy = useCopy(),
    ids = useId(),
    roleOn = !!setup?.principalArn,
    // AWS's IAM policies; an S3-compatible store has its own permissions.
    // A role is always AWS: the endpoint is a key's only.
    aws = form.access === "role" || !form.endpoint.trim(),
    permissions = s3PermissionsPolicy(
      form.bucket.trim() || "your-bucket",
      form.prefix,
    );
  // A region belongs to its bucket on AWS: a new bucket is detected again.
  const newBucket = aws ? { region: "" } : {};
  // A pasted s3:// URI (or one typed, once the field is left) fills bucket
  // and folder; it is not split while being typed.
  const splitUri = (value: string) => {
    const uri = parseS3Uri(value);
    if (uri) onChange({ ...uri, ...newBucket });
    return !!uri;
  };
  return (
    <>
      <label>
        Bucket
        <input
          required
          autoComplete="off"
          spellCheck={false}
          placeholder="acme-exports"
          value={form.bucket}
          onChange={(e) => onChange({ bucket: e.target.value, ...newBucket })}
          onPaste={(e) => {
            if (splitUri(e.clipboardData.getData("text"))) e.preventDefault();
          }}
          onBlur={(e) => void splitUri(e.target.value)}
        />
      </label>
      <label>
        Folder (optional)
        <input
          autoComplete="off"
          spellCheck={false}
          maxLength={1024}
          placeholder="Whole bucket"
          value={form.prefix}
          onChange={(e) => onChange({ prefix: e.target.value })}
        />
      </label>
      <fieldset className="span2 plain-fieldset">
        <legend>Access</legend>
        <div className="choice-options provider-options dc-access">
          {(
            [
              [
                "role",
                "IAM role (recommended)",
                "Helix assumes a role in your AWS account",
                ShieldCheck,
              ],
              [
                "keys",
                "Access key",
                "An IAM user’s key, or an S3-compatible store’s",
                KeyRound,
              ],
            ] as const
          ).map(([value, title, description, Icon]) => (
            <label
              key={value}
              className={cx("choice-card", form.access === value && "selected")}
            >
              <span className="choice-card-icon" aria-hidden="true">
                <Icon strokeWidth={1.5} />
              </span>
              <span className="choice-card-heading">
                <input
                  type="radio"
                  name="s3-access"
                  value={value}
                  checked={form.access === value}
                  disabled={value === "role" && !roleOn}
                  aria-label={title}
                  aria-describedby={`${ids}-${value}`}
                  onChange={() => onChange({ access: value })}
                />
                <strong>{title}</strong>
              </span>
              <span id={`${ids}-${value}`} className="choice-card-description">
                {description}
              </span>
            </label>
          ))}
        </div>
        {!roleOn && (
          <p className="muted small">
            IAM role access isn’t available here; use an access key.
          </p>
        )}
      </fieldset>
      {form.access === "role" && !setup ? (
        // A stored role where role access is off: shown, never tested.
        <label className="span2">
          Role ARN
          <input readOnly className="hf-mono" value={form.roleArn} />
        </label>
      ) : form.access === "role" && setup ? (
        <>
          <div className="span2">
            <label htmlFor={`${ids}-external-id`}>External ID</label>
            <div className="dc-copy-field">
              <input
                id={`${ids}-external-id`}
                readOnly
                className="hf-mono"
                value={setup.externalId || ""}
              />
              <Button
                size="sm"
                variant="ghost"
                icon={Copy}
                aria-label="Copy external ID"
                onClick={() => void copy(setup.externalId || "", "external ID")}
              >
                Copy
              </Button>
            </div>
            <small className="muted">
              Unique to this workspace. Your role’s trust policy must require
              it.
            </small>
          </div>
          <ol className="span2 dc-steps">
            <li>
              In AWS IAM, <strong>create a role</strong>, choose{" "}
              <strong>Custom trust policy</strong> and paste this:
              <Policy name="trust policy" json={s3TrustPolicy(setup)} />
            </li>
            <li>
              <strong>Add permissions</strong>: give the role this inline
              policy, which reads only{" "}
              {form.prefix.trim() ? "this folder" : "this bucket"}:
              <Policy name="permissions policy" json={permissions} />
            </li>
            <li>
              <label>
                Paste the role’s ARN
                <input
                  required
                  autoComplete="off"
                  spellCheck={false}
                  aria-label="Role ARN"
                  placeholder="arn:aws:iam::123456789012:role/helix-foundry"
                  value={form.roleArn}
                  onChange={(e) => onChange({ roleArn: e.target.value })}
                />
              </label>
            </li>
          </ol>
        </>
      ) : (
        <>
          {aws && (
            <div className="span2 muted small">
              Create an IAM user with this inline policy, then an access key for
              it:
              <Policy name="permissions policy" json={permissions} />
            </div>
          )}
          <label>
            Access key ID
            <input
              required
              autoComplete="off"
              spellCheck={false}
              placeholder="AKIA…"
              value={form.accessKeyId}
              onChange={(e) => onChange({ accessKeyId: e.target.value })}
            />
          </label>
          <label>
            Secret access key
            <input
              type="password"
              autoComplete="off"
              spellCheck={false}
              required={!(editing && storedKey)}
              placeholder={
                editing && storedKey ? "Saved — leave blank to keep" : ""
              }
              value={form.secretAccessKey}
              onChange={(e) => onChange({ secretAccessKey: e.target.value })}
            />
          </label>
        </>
      )}
      {/* Either access may need the region: a test asks for it when S3
          doesn't say where the bucket is. */}
      <details className="span2 disclosure" open={!aws || undefined}>
        <summary>Advanced</summary>
        <div className="form-grid">
          {form.access === "keys" && (
            <label className="span2">
              Endpoint (S3-compatible storage only)
              <input
                type="url"
                autoComplete="off"
                spellCheck={false}
                placeholder="https://<account>.r2.cloudflarestorage.com"
                value={form.endpoint}
                onChange={(e) => onChange({ endpoint: e.target.value })}
              />
            </label>
          )}
          <label className="span2">
            Region
            <input
              autoComplete="off"
              spellCheck={false}
              placeholder="Detected automatically"
              value={form.region}
              onChange={(e) => onChange({ region: e.target.value })}
            />
          </label>
        </div>
      </details>
    </>
  );
}

/** A tested file table in a line: "2 files · 1.2 GB · 8 columns · Key id". */
export function fileSummary(t: ConnectorTable) {
  const columns = (t.columns || []).length;
  return [
    plural(t.fileCount || 0, "file"),
    formatBytes(t.bytes),
    columns ? plural(columns, "column") : "columns after the first sync",
    keySummary(t),
  ].join(" · ");
}

/** A tested file table's typed columns and its preview rows, on demand. */
export function FileTableDetails({ table }: { table: ConnectorTable }) {
  const columns = table.columns || [],
    grid = previewGrid(table);
  return (
    <details className="dc-file-details">
      <summary>Columns and preview</summary>
      {columns.length ? (
        <ul className="dc-file-columns" aria-label={`${table.name} columns`}>
          {columns.map((c) => (
            <li key={c.name}>
              <span className="hf-mono">{c.name}</span>
              <small>
                {c.type || "—"}
                {c.nullable && " · nullable"}
              </small>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted small">Columns after the first sync.</p>
      )}
      {grid.rows.length ? (
        <div className="table-scroll dc-preview">
          <table aria-label={`${table.name} preview`}>
            <thead>
              <tr>
                {grid.columns.map((c) => (
                  <th key={c}>{c}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {grid.rows.map((row, i) => (
                <tr key={i}>
                  {row.map((cell, j) =>
                    cell === null ? (
                      <td key={j}>
                        <span className="muted">null</span>
                      </td>
                    ) : (
                      <td key={j}>{cell}</td>
                    ),
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="muted small">Preview after the first sync.</p>
      )}
    </details>
  );
}
