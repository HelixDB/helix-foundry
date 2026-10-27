import { useMemo, useState, type ReactNode } from "react";
import { Check, ChevronRight, Copy } from "lucide-react";

import { cn } from "./ui";

import { classifyValue } from "./lib/normalize";
import type { IndexKind, JsonValue, ValueKind } from "./lib/types";
import { useCopyFeedback } from "./query/useCopyFeedback";
import { MONO, TEXT_BUTTON } from "./ui";

/** Short names for index kinds, shown as badges next to indexed properties. */
export const INDEX_BADGE: Record<IndexKind, string> = {
  equality: "eq",
  unique: "unique",
  range: "range",
  vector: "vector",
  full_text: "text",
};

const INDEX_TITLE: Record<IndexKind, string> = {
  equality: "Equality index: exact matches are fast",
  unique: "Unique index: one node per value",
  range: "Range index: comparisons and ordering are fast",
  vector: "Vector index: similarity search",
  full_text: "Full-text index: BM25 text search",
};

export function IndexBadges({ kinds }: { kinds: readonly IndexKind[] }) {
  if (kinds.length === 0) return null;
  return (
    <>
      {kinds.map((kind) => (
        <span key={kind} title={INDEX_TITLE[kind]} className="ex-index-badge">
          {INDEX_BADGE[kind]}
        </span>
      ))}
    </>
  );
}

export interface PropertyRow {
  key: string;
  value: JsonValue;
  indexes?: readonly IndexKind[];
}

/** A node's or edge's properties with typed rendering and copy on every row. */
export function PropertyList({
  rows,
  empty,
}: {
  rows: PropertyRow[];
  empty?: ReactNode;
}) {
  const { copied, copy } = useCopyFeedback();
  if (rows.length === 0) {
    return <p className="ex-empty-note">{empty ?? "No properties."}</p>;
  }
  return (
    <dl className="ex-props">
      {rows.map((row) => (
        <div key={row.key} className="ex-prop">
          <dt>
            <span
              className={cn(MONO, "ex-prop-key ex-truncate")}
              title={row.key}
            >
              {row.key}
            </span>
            <IndexBadges kinds={row.indexes ?? []} />
            <button
              type="button"
              onClick={() => copy(row.key, copyText(row.value))}
              aria-label={`Copy ${row.key}`}
              title={`Copy ${row.key}`}
              className={cn("ex-prop-copy", copied === row.key && "is-shown")}
            >
              {copied === row.key ? (
                <Check aria-hidden="true" size={14} className="ex-ok" />
              ) : (
                <Copy aria-hidden="true" size={14} />
              )}
            </button>
          </dt>
          <dd>
            <PropertyValue value={row.value} />
          </dd>
        </div>
      ))}
    </dl>
  );
}

function copyText(value: JsonValue): string {
  return typeof value === "string" ? value : JSON.stringify(value, null, 2);
}

const CLAMP_CHARS = 220;
const CLAMP_LINES = 3;
const INLINE_ARRAY_ITEMS = 8;
const INLINE_ARRAY_CHARS = 120;

export function PropertyValue({ value }: { value: JsonValue }) {
  const kind = classifyValue(value);
  switch (kind) {
    case "null":
      return <span className={cn(MONO, "ex-dim")}>null</span>;
    case "boolean":
      return (
        <span className={cn("ex-bool", value ? "is-true" : "is-false")}>
          {value ? "true" : "false"}
        </span>
      );
    case "integer":
    case "float":
      return (
        <span className={cn(MONO, "ex-num ex-value")}>{String(value)}</span>
      );
    case "datetime":
      return <DateTimeValue value={value as string} />;
    case "string":
      return <StringValue value={value as string} />;
    case "vector":
      return <VectorValue value={value as number[]} />;
    case "array":
    case "object":
      return <JsonValueBlock value={value} kind={kind} />;
  }
}

function StringValue({ value }: { value: string }) {
  const [expanded, setExpanded] = useState(false);
  const long =
    value.length > CLAMP_CHARS || value.split("\n").length > CLAMP_LINES;
  if (value === "")
    return <span className="ex-dim ex-italic">empty string</span>;
  return (
    <div>
      <p className={cn("ex-string", long && !expanded && "is-clamped")}>
        {value}
      </p>
      {long && (
        <button
          type="button"
          onClick={() => setExpanded((open) => !open)}
          className={TEXT_BUTTON}
        >
          {expanded
            ? "Show less"
            : `Show more · ${value.length.toLocaleString("en-US")} characters`}
        </button>
      )}
    </div>
  );
}

const DATE_FORMAT = new Intl.DateTimeFormat("en-US", {
  dateStyle: "medium",
  timeStyle: "short",
});
const RELATIVE = new Intl.RelativeTimeFormat("en-US", { numeric: "auto" });
const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
];

export function relativeTime(date: Date, now = Date.now()): string {
  const seconds = (date.getTime() - now) / 1000;
  for (const [unit, size] of UNITS) {
    if (Math.abs(seconds) >= size)
      return RELATIVE.format(Math.round(seconds / size), unit);
  }
  return "just now";
}

function DateTimeValue({ value }: { value: string }) {
  const date = new Date(value);
  return (
    <span className="ex-value" title={value}>
      {DATE_FORMAT.format(date)}
      <span className="ex-dim"> · {relativeTime(date)}</span>
    </span>
  );
}

function VectorValue({ value }: { value: number[] }) {
  const { copied, copy } = useCopyFeedback();
  const preview = value
    .slice(0, 4)
    .map((item) => (Number.isInteger(item) ? String(item) : item.toFixed(3)))
    .join(", ");
  return (
    <div className="ex-vector">
      <span className="ex-value-chip">
        vector · {value.length.toLocaleString("en-US")} dims
      </span>
      <span className={cn(MONO, "ex-dim ex-truncate")}>[{preview}, …]</span>
      <button
        type="button"
        onClick={() => copy("vector", JSON.stringify(value))}
        className={TEXT_BUTTON}
      >
        {copied === "vector" ? (
          <Check aria-hidden="true" size={14} className="ex-ok" />
        ) : (
          <Copy aria-hidden="true" size={14} />
        )}
        {copied === "vector" ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

function isScalar(value: JsonValue): boolean {
  return value === null || typeof value !== "object";
}

function JsonValueBlock({
  value,
  kind,
}: {
  value: JsonValue;
  kind: ValueKind;
}) {
  const [open, setOpen] = useState(false);
  const text = useMemo(() => JSON.stringify(value, null, 2), [value]);

  if (
    Array.isArray(value)
      ? value.length === 0
      : Object.keys(value as object).length === 0
  ) {
    return (
      <span className="ex-dim">
        {kind === "array" ? "Empty array" : "Empty object"}{" "}
        <span className={MONO}>{kind === "array" ? "[]" : "{}"}</span>
      </span>
    );
  }

  // Short lists of scalars read best as chips.
  if (
    Array.isArray(value) &&
    value.length > 0 &&
    value.length <= INLINE_ARRAY_ITEMS &&
    value.every(isScalar) &&
    JSON.stringify(value).length <= INLINE_ARRAY_CHARS
  ) {
    return (
      <div className="ex-chips">
        {value.map((item, index) => (
          <span key={index} className={cn(MONO, "ex-value-chip")}>
            {item === null ? "null" : String(item)}
          </span>
        ))}
      </div>
    );
  }

  const summary =
    kind === "array"
      ? `${(value as JsonValue[]).length.toLocaleString("en-US")} ${(value as JsonValue[]).length === 1 ? "item" : "items"}`
      : `${Object.keys(value as object).length} ${Object.keys(value as object).length === 1 ? "key" : "keys"}`;
  return (
    <div>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((current) => !current)}
        className={TEXT_BUTTON}
      >
        <ChevronRight
          aria-hidden="true"
          size={14}
          className={cn("ex-chevron-right", open && "is-open")}
        />
        {kind === "array" ? "Array" : "Object"} · {summary}
      </button>
      {open && (
        <pre className={cn(MONO, "ex-json")}>
          <code className={MONO}>{text}</code>
        </pre>
      )}
    </div>
  );
}
