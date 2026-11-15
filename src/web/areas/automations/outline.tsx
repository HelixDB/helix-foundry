import {
  CircleAlert,
  MoreHorizontal,
  Plus,
  Trash2,
  Unlink,
} from "lucide-react";
import type { AutomationSpec } from "../../../shared/wfe";
import {
  DropdownMenu,
  MenuItem,
  MenuLabel,
  MenuSeparator,
  cx,
} from "../../kit";
import {
  KIND_HEADS,
  KIND_LABELS,
  addStep,
  cannotConnect,
  connect,
  disconnect,
  edgesInto,
  edgesOutOf,
  incomplete,
  parseSpec,
  removeStep,
  type Kind,
  type Names,
} from "./model";
import { nodeIcon, stepTitle } from "./parts";

// An automation's graph as an outline: every step in the order a run takes
// them, what comes before and after each (a condition's by when met and
// otherwise), and, to whoever edits it, a menu to add a step after it, join
// it to another, cut a join or remove it. The same graph as the canvas, for
// keyboards and screen readers, and the place to edit it on a phone, where
// the canvas only shows it.

const ADDABLE: Exclude<Kind, "trigger">[] = [
  "assistant",
  "action",
  "condition",
  "notify",
];

export function Outline({
  spec,
  names,
  email,
  selected,
  onSelect,
  onChange,
}: {
  spec: AutomationSpec;
  names: Names;
  email: boolean;
  selected?: string;
  onSelect: (id: string) => void;
  // Unset, it only shows the graph.
  onChange?: (spec: AutomationSpec) => void;
}) {
  const read = parseSpec(spec);
  // A graph that doesn't read yet keeps the order it was drawn in.
  const order = read.ok ? read.order : spec.nodes.map((n) => n.id);
  const byId = new Map(spec.nodes.map((n) => [n.id, n]));
  const title = (id: string) => {
    const n = byId.get(id);
    return n ? stepTitle(n, names, email) : id;
  };
  // A step another names, cut short.
  const short = (id: string) => {
    const t = [...title(id)];
    return t.length > 60 ? t.slice(0, 59).join("").trimEnd() + "…" : t.join("");
  };
  const via = (branch?: string) =>
    branch === "true"
      ? " (when met)"
      : branch === "false"
        ? " (otherwise)"
        : "";
  return (
    <ol className="au-outline" aria-label="Steps">
      {order.map((id) => {
        const n = byId.get(id)!,
          Icon = nodeIcon(n),
          problem = incomplete(n),
          into = edgesInto(spec, id),
          outOf = edgesOutOf(spec, id);
        const joinable = spec.nodes.filter(
          (x) => !cannotConnect(spec, id, x.id),
        );
        return (
          <li
            key={id}
            className={cx("au-outline-step", id === selected && "is-selected")}
          >
            <button
              type="button"
              className="au-outline-main"
              aria-pressed={id === selected}
              onClick={() => onSelect(id)}
            >
              <span className="au-outline-icon" aria-hidden>
                <Icon size={14} />
              </span>
              <span className="au-outline-text">
                <span className="au-outline-kind">{KIND_HEADS[n.kind]}</span>
                <span>{title(id)}</span>
                {problem && (
                  <span className="au-outline-problem">
                    <CircleAlert size={12} aria-hidden /> {problem}
                  </span>
                )}
                {n.kind !== "trigger" && (
                  <span className="au-outline-links">
                    {into.length
                      ? `After: ${into.map((e) => short(e.from) + via(e.branch)).join("; ")}`
                      : "Not joined to any step before it"}
                  </span>
                )}
                {!!outOf.length && (
                  <span className="au-outline-links">
                    Then:{" "}
                    {outOf.map((e) => short(e.to) + via(e.branch)).join("; ")}
                  </span>
                )}
              </span>
            </button>
            {onChange && (
              <DropdownMenu
                label={`Change the step: ${title(id)}`}
                align="end"
                triggerClassName="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
                trigger={<MoreHorizontal size={16} aria-hidden />}
              >
                <MenuLabel>Add a step after it</MenuLabel>
                {ADDABLE.map((kind) => (
                  <MenuItem
                    key={kind}
                    icon={Plus}
                    disabled={
                      spec.nodes.length >= 50 ||
                      (n.kind === "condition" && outOf.length >= 2)
                    }
                    onSelect={() => {
                      const added = addStep(spec, kind, id);
                      onChange(added.spec);
                      onSelect(added.id);
                    }}
                  >
                    {KIND_LABELS[kind]}
                  </MenuItem>
                ))}
                {!!joinable.length && (
                  <>
                    <MenuSeparator />
                    <MenuLabel>Go on to</MenuLabel>
                    {joinable.map((x) => (
                      <MenuItem
                        key={x.id}
                        onSelect={() => onChange(connect(spec, id, x.id))}
                      >
                        {title(x.id)}
                      </MenuItem>
                    ))}
                  </>
                )}
                {!!outOf.length && (
                  <>
                    <MenuSeparator />
                    <MenuLabel>Stop going on to</MenuLabel>
                    {outOf.map((e) => (
                      <MenuItem
                        key={e.to}
                        icon={Unlink}
                        onSelect={() => onChange(disconnect(spec, id, e.to))}
                      >
                        {title(e.to) + via(e.branch)}
                      </MenuItem>
                    ))}
                  </>
                )}
                {n.kind !== "trigger" && (
                  <>
                    <MenuSeparator />
                    <MenuItem
                      icon={Trash2}
                      danger
                      onSelect={() => onChange(removeStep(spec, id))}
                    >
                      Remove this step
                    </MenuItem>
                  </>
                )}
              </DropdownMenu>
            )}
          </li>
        );
      })}
    </ol>
  );
}
