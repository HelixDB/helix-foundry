import "./viewers.css";
import { useMemo, useState } from "react";
import { ChevronRight } from "lucide-react";
import { Button, cx } from "../../kit";
import { JSON_PAGE, jsonChildren, jsonSummary, readJson } from "./model";

// A JSON document as a tree. Objects and arrays open and close; a closed
// one renders none of its children, and an open one its first JSON_PAGE,
// more a click away, so a large array never renders all at once. Loaded on
// its own, when a JSON document is shown.

export function JsonTree({ text }: { text: string }) {
  const json = useMemo(() => readJson(text), [text]);
  if (!json.ok)
    return (
      <p role="alert" className="on-error-text">
        It isn’t valid JSON ({json.error}). Its source shows it as uploaded.
      </p>
    );
  return (
    <ul className="dc-json" aria-label="Document text">
      <JsonNode value={json.value} depth={0} />
    </ul>
  );
}

function JsonNode({
  name,
  value,
  depth,
}: {
  // Its key in its parent; the root has none.
  name?: string;
  value: unknown;
  depth: number;
}) {
  const [open, setOpen] = useState(depth === 0),
    [shown, setShown] = useState(JSON_PAGE);
  const key = name !== undefined && <span className="dc-json-key">{name}</span>;
  if (value === null || typeof value !== "object")
    return (
      <li className="dc-json-leaf">
        {key}
        <span
          className={cx(
            "dc-json-value",
            `is-${value === null ? "null" : typeof value}`,
          )}
        >
          {jsonSummary(value)}
        </span>
      </li>
    );
  const children = open ? jsonChildren(value) : [];
  return (
    <li>
      <button
        type="button"
        className="dc-json-toggle"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <ChevronRight size={12} className="dc-json-chevron" aria-hidden />
        {key}
        <span className="dc-json-summary">{jsonSummary(value)}</span>
      </button>
      {open && (
        <ul>
          {children.slice(0, shown).map(([k, v]) => (
            <JsonNode key={k} name={k} value={v} depth={depth + 1} />
          ))}
          {children.length > shown && (
            <li>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setShown(shown + JSON_PAGE)}
              >
                Show {Math.min(JSON_PAGE, children.length - shown)} more of{" "}
                {(children.length - shown).toLocaleString("en-US")}
              </Button>
            </li>
          )}
        </ul>
      )}
    </li>
  );
}
