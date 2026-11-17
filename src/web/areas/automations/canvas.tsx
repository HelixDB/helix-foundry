import "@xyflow/react/dist/base.css";
import "./automations.css";
import { memo, useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  ConnectionMode,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useNodesState,
  useReactFlow,
  useViewport,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import {
  AlignVerticalDistributeCenter,
  ChevronDown,
  CircleAlert,
  Hand,
  Maximize,
  MousePointer2,
  Plus,
} from "lucide-react";
import type { AutomationNode, AutomationSpec } from "../../../shared/wfe";
import {
  DropdownMenu,
  MenuItem,
  MenuSeparator,
  StatusDot,
  cx,
} from "../../kit";
import { useCurrentTheme } from "../../shell/theme";
import {
  KIND_HEADS,
  KIND_LABELS,
  LIMITS,
  cannotConnect,
  connect,
  edgeSides,
  incomplete,
  removeSelection,
  type Box,
  type Kind,
  type Names,
  type Places,
  type Side,
  type StepState,
} from "./model";
import { KIND_ICONS, STEP_STATUS, nodeIcon, stepTitle } from "./parts";

// An automation's graph on a canvas (React Flow, @xyflow/react), lazy-loaded
// so the rest of the app never carries it: its steps as cards, top to
// bottom, joined by their edges (a condition's labelled When met and
// Otherwise). It fills the automation's page under its bar. Editable, its
// person drags steps (onMove: where they are drawn, which saves on its own),
// joins them from a handle on any side of a step to any side of another,
// and selects one to change it in the inspector; read only (a
// run's colours, a member's view, a phone) it pans, zooms and selects. A
// An edge leaves and arrives by the sides its steps face (model.ts
// edgeSides), so steps side by side join straight across. A
// toolbar floats at its foot: the zoom, fit, pan or select, and, editable,
// Add step (A) and Tidy. Its colours are the kit's tokens
// (automations.css); the outline (outline.tsx) is the same graph for
// keyboards and screen readers, and on a phone the place to edit it.

type StepData = {
  node: AutomationNode;
  title: string;
  problem?: string;
  state?: StepState;
};
type StepNode = Node<StepData, "step">;

const StepCard = memo(function StepCard({
  data,
  selected,
}: NodeProps<StepNode>) {
  const { node, title, problem, state } = data,
    Icon = nodeIcon(node);
  return (
    <div
      className={cx(
        "au-node",
        `au-node--${node.kind}`,
        state && `au-node--${state}`,
        selected && "is-selected",
      )}
    >
      {/* A handle on each side, each where joins leave and arrive (the
          flow's loose connection mode): an edge picks its sides. */}
      {SIDES.map((side) => (
        <Handle
          key={side}
          id={side}
          type="source"
          position={SIDE_POSITIONS[side]}
        />
      ))}
      <div className="au-node-head">
        <span className="au-node-icon" aria-hidden>
          <Icon size={13} />
        </span>
        <span className="au-node-kind">{KIND_HEADS[node.kind]}</span>
        {state && (
          <StatusDot
            status={STEP_STATUS[state].status}
            label={STEP_STATUS[state].label}
          />
        )}
      </div>
      <p className="au-node-text">{title}</p>
      {problem && (
        <p className="au-node-problem">
          <CircleAlert size={12} aria-hidden />
          {problem}
        </p>
      )}
    </div>
  );
});
const NODE_TYPES = { step: StepCard };

const SIDES: readonly Side[] = ["top", "right", "bottom", "left"];
const SIDE_POSITIONS: Record<Side, Position> = {
  top: Position.Top,
  right: Position.Right,
  bottom: Position.Bottom,
  left: Position.Left,
};
// A card's size until the canvas has measured it (automations.css .au-node).
const CARD = { width: 248, height: 84 };

const ADDABLE: Exclude<Kind, "trigger">[] = [
  "assistant",
  "action",
  "condition",
  "notify",
];
const ZOOMS = [0.5, 0.75, 1, 1.5];
const FIT = { padding: 0.2, maxZoom: 1 };

export type CanvasProps = {
  spec: AutomationSpec;
  names: Names;
  email: boolean;
  // Each step's state in a run, to colour them.
  states?: ReadonlyMap<string, StepState>;
  selected?: string;
  onSelect?: (id: string | undefined) => void;
  // Set, the person edits the graph: every change to its steps comes back as
  // a new spec. A join the graph can't take (model.ts cannotConnect) isn't
  // offered.
  onChange?: (spec: AutomationSpec) => void;
  // Set with onChange: steps dragged to new places (every step a drag
  // moved), apart from changes to the steps.
  onMove?: (places: Places) => void;
  // Set with onChange: Add step's kinds, after the selected step.
  onAdd?: (kind: Exclude<Kind, "trigger">) => void;
  // Set with onChange: the steps laid out anew (model.ts tidy).
  onTidy?: () => void;
  label: string;
};

/** The toolbar at the canvas's foot: zoom, fit, pan or select, add, tidy. */
function Toolbar({
  mode,
  onMode,
  onAdd,
  onTidy,
  full,
}: {
  mode: "select" | "pan";
  onMode: (mode: "select" | "pan") => void;
  onAdd?: CanvasProps["onAdd"];
  onTidy?: CanvasProps["onTidy"];
  // The graph holds as many steps as it may.
  full: boolean;
}) {
  const flow = useReactFlow(),
    { zoom } = useViewport(),
    add = useRef<HTMLDivElement>(null);
  // A, outside a field, opens Add step.
  useEffect(() => {
    if (!onAdd) return;
    const key = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (
        e.key.toLowerCase() !== "a" ||
        e.metaKey ||
        e.ctrlKey ||
        e.altKey ||
        t?.closest("input, textarea, select, [contenteditable], [role=menu]") ||
        document.querySelector("dialog[open]")
      )
        return;
      e.preventDefault();
      add.current?.querySelector<HTMLButtonElement>("button")?.click();
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [onAdd]);
  return (
    <div className="au-toolbar" role="toolbar" aria-label="Canvas">
      <DropdownMenu
        label={`Zoom: ${Math.round(zoom * 100)}%`}
        side="top"
        triggerClassName="hf-btn hf-btn--ghost hf-btn--sm au-zoom"
        trigger={
          <>
            {Math.round(zoom * 100)}%
            <ChevronDown size={13} aria-hidden />
          </>
        }
      >
        <MenuItem onSelect={() => void flow.zoomIn({ duration: 150 })}>
          Zoom in
        </MenuItem>
        <MenuItem onSelect={() => void flow.zoomOut({ duration: 150 })}>
          Zoom out
        </MenuItem>
        <MenuSeparator />
        {ZOOMS.map((z) => (
          <MenuItem
            key={z}
            checked={Math.abs(zoom - z) < 0.01}
            onSelect={() => void flow.zoomTo(z, { duration: 150 })}
          >
            {z * 100}%
          </MenuItem>
        ))}
        <MenuSeparator />
        <MenuItem onSelect={() => void flow.fitView({ ...FIT, duration: 150 })}>
          Fit the steps in view
        </MenuItem>
      </DropdownMenu>
      <button
        type="button"
        className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
        aria-label="Fit the steps in view"
        title="Fit the steps in view"
        onClick={() => void flow.fitView({ ...FIT, duration: 150 })}
      >
        <Maximize size={15} aria-hidden />
      </button>
      <span className="au-toolbar-sep" aria-hidden />
      <button
        type="button"
        className={cx(
          "hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon",
          mode === "select" && "is-active",
        )}
        aria-label="Select: drag to select steps"
        title="Select: drag to select steps"
        aria-pressed={mode === "select"}
        onClick={() => onMode("select")}
      >
        <MousePointer2 size={15} aria-hidden />
      </button>
      <button
        type="button"
        className={cx(
          "hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon",
          mode === "pan" && "is-active",
        )}
        aria-label="Pan: drag to move the canvas"
        title="Pan: drag to move the canvas"
        aria-pressed={mode === "pan"}
        onClick={() => onMode("pan")}
      >
        <Hand size={15} aria-hidden />
      </button>
      {onAdd && (
        <>
          <span className="au-toolbar-sep" aria-hidden />
          <div ref={add}>
            <DropdownMenu
              label="Add step"
              side="top"
              triggerClassName="hf-btn hf-btn--ghost hf-btn--sm au-add-step"
              trigger={
                <>
                  <Plus size={15} aria-hidden />
                  Add step
                  <kbd className="au-kbd" aria-hidden>
                    A
                  </kbd>
                </>
              }
            >
              {ADDABLE.map((kind) => (
                <MenuItem
                  key={kind}
                  icon={KIND_ICONS[kind]}
                  disabled={full}
                  onSelect={() => onAdd(kind)}
                >
                  {KIND_LABELS[kind]}
                </MenuItem>
              ))}
            </DropdownMenu>
          </div>
        </>
      )}
      {onTidy && (
        <button
          type="button"
          className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
          aria-label="Tidy the layout"
          title="Tidy the layout"
          onClick={() => {
            onTidy();
            // Once the canvas has the new places.
            requestAnimationFrame(() =>
              requestAnimationFrame(
                () => void flow.fitView({ ...FIT, duration: 200 }),
              ),
            );
          }}
        >
          <AlignVerticalDistributeCenter size={15} aria-hidden />
        </button>
      )}
    </div>
  );
}

function Canvas({
  spec,
  names,
  email,
  states,
  selected,
  onSelect,
  onChange,
  onMove,
  onAdd,
  onTidy,
  label,
}: CanvasProps) {
  const theme = useCurrentTheme();
  const editable = !!onChange;
  const [mode, setMode] = useState<"select" | "pan">(
    editable ? "select" : "pan",
  );
  const derived = useMemo<StepNode[]>(
    () =>
      spec.nodes.map((n) => ({
        id: n.id,
        type: "step",
        position: n.position,
        selected: n.id === selected,
        deletable: editable && n.kind !== "trigger",
        ariaLabel: `${KIND_HEADS[n.kind]}: ${stepTitle(n, names, email, "brief")}`,
        data: {
          node: n,
          title: stepTitle(n, names, email),
          ...(editable && { problem: incomplete(n) }),
          ...(states && { state: states.get(n.id) }),
        },
      })),
    [spec, names, email, states, selected, editable],
  );
  // Dragging moves the canvas's own copy; the spec takes the place it ends
  // at. A spec that comes in mid-drag (a save of an earlier move landing)
  // waits for its end, so the step under the pointer never jumps back.
  const [nodes, setNodes, onNodesChange] = useNodesState<StepNode>(derived);
  const dragging = useRef(false);
  useEffect(() => {
    if (!dragging.current) setNodes(derived);
  }, [derived, setNodes]);
  // Each edge by the sides its steps face, where the canvas has them now (a
  // drag redraws them as it goes).
  const boxes = useMemo(
    () =>
      new Map<string, Box>(
        nodes.map((n) => [
          n.id,
          {
            ...n.position,
            width: n.measured?.width ?? CARD.width,
            height: n.measured?.height ?? CARD.height,
          },
        ]),
      ),
    [nodes],
  );
  const edges = useMemo<Edge[]>(
    () =>
      spec.edges.map((e) => {
        const from = boxes.get(e.from),
          to = boxes.get(e.to),
          sides = from && to ? edgeSides(from, to) : undefined;
        return {
          id: `${e.from}->${e.to}`,
          source: e.from,
          target: e.to,
          ...(sides && {
            sourceHandle: sides.source,
            targetHandle: sides.target,
          }),
          deletable: editable,
          markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16 },
          className: cx(
            "au-edge",
            e.branch && `au-edge--${e.branch}`,
            states?.get(e.from) === "succeeded" &&
              states?.get(e.to) !== "waiting" &&
              states?.get(e.to) !== "skipped" &&
              "is-taken",
          ),
          ...(e.branch && {
            label: e.branch === "true" ? "When met" : "Otherwise",
            labelBgPadding: [6, 3] as [number, number],
            labelBgBorderRadius: 4,
          }),
        };
      }),
    [spec, states, editable, boxes],
  );
  const select = mode === "select";
  return (
    <div
      className={cx("au-flow", editable && "is-editable")}
      role="region"
      aria-label={label}
    >
      <ReactFlow<StepNode, Edge>
        nodes={nodes}
        edges={edges}
        nodeTypes={NODE_TYPES}
        colorMode={theme}
        fitView
        fitViewOptions={FIT}
        minZoom={0.25}
        maxZoom={1.5}
        nodesDraggable={editable}
        nodesConnectable={editable}
        connectionMode={ConnectionMode.Loose}
        elementsSelectable={!!onSelect}
        // Select: a drag on the pane selects, the wheel or two fingers pan;
        // pan: a drag on the pane moves it.
        panOnDrag={select ? [1, 2] : true}
        selectionOnDrag={select}
        panOnScroll={select}
        zoomOnPinch
        deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
        onNodesChange={onNodesChange}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        onPaneClick={() => onSelect?.(undefined)}
        onNodeDragStart={() => {
          dragging.current = true;
        }}
        onNodeDragStop={(_, _node, moved) => {
          dragging.current = false;
          if (onMove)
            onMove(Object.fromEntries(moved.map((n) => [n.id, n.position])));
          else setNodes(derived);
        }}
        onConnect={(c) => onChange?.(connect(spec, c.source, c.target))}
        isValidConnection={(c) =>
          editable && !cannotConnect(spec, c.source, c.target)
        }
        onDelete={({ nodes: gone, edges: cut }) =>
          onChange?.(
            removeSelection(
              spec,
              gone.map((n) => n.id),
              cut.map((e) => ({ from: e.source, to: e.target })),
            ),
          )
        }
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1.2} />
      </ReactFlow>
      <Toolbar
        mode={mode}
        onMode={setMode}
        onAdd={editable ? onAdd : undefined}
        onTidy={editable ? onTidy : undefined}
        full={spec.nodes.length >= LIMITS.nodes}
      />
    </div>
  );
}

/** The canvas in its own React Flow context. */
export default function AutomationCanvas(props: CanvasProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  );
}
