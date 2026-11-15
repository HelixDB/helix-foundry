import "@xyflow/react/dist/base.css";
import "./automations.css";
import { memo, useEffect, useMemo } from "react";
import {
  Background,
  BackgroundVariant,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useNodesState,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import { CircleAlert } from "lucide-react";
import type { AutomationNode, AutomationSpec } from "../../../shared/wfe";
import { StatusDot, cx } from "../../kit";
import { useCurrentTheme } from "../../shell/theme";
import {
  KIND_HEADS,
  cannotConnect,
  connect,
  disconnect,
  incomplete,
  moveNode,
  removeStep,
  type Names,
  type StepState,
} from "./model";
import { STEP_STATUS, nodeIcon, stepTitle } from "./parts";

// An automation's graph on a canvas (React Flow, @xyflow/react), lazy-loaded
// so the rest of the app never carries it: its steps as cards, top to
// bottom, joined by their edges (a condition's labelled Yes and No). In the
// builder its person drags steps, joins them from a step's lower handle to
// another's upper one, and selects one to change it in the inspector; read
// only (an automation's page, a run's, a phone) it pans, zooms and selects.
// A run's page colours each step by how it went. Its colours are the kit's
// tokens (automations.css); the outline (outline.tsx) is the same graph for
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
      {node.kind !== "trigger" && (
        <Handle type="target" position={Position.Top} />
      )}
      <div className="au-node-head">
        <Icon size={13} aria-hidden />
        <span>{KIND_HEADS[node.kind]}</span>
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
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
});
const NODE_TYPES = { step: StepCard };

export type CanvasProps = {
  spec: AutomationSpec;
  names: Names;
  email: boolean;
  // Each step's state in a run, to colour them.
  states?: ReadonlyMap<string, StepState>;
  selected?: string;
  onSelect?: (id: string | undefined) => void;
  // Set, the person edits the graph: every change comes back as a new spec.
  // A join the graph can't take (model.ts cannotConnect) isn't offered.
  onChange?: (spec: AutomationSpec) => void;
  label: string;
};

function Canvas({
  spec,
  names,
  email,
  states,
  selected,
  onSelect,
  onChange,
  label,
}: CanvasProps) {
  const theme = useCurrentTheme();
  const editable = !!onChange;
  const derived = useMemo<StepNode[]>(
    () =>
      spec.nodes.map((n) => ({
        id: n.id,
        type: "step",
        position: n.position,
        selected: n.id === selected,
        deletable: editable && n.kind !== "trigger",
        ariaLabel: `${KIND_HEADS[n.kind]}: ${stepTitle(n, names, email)}`,
        data: {
          node: n,
          title: stepTitle(n, names, email),
          ...(editable && { problem: incomplete(n) }),
          ...(states && { state: states.get(n.id) }),
        },
      })),
    [spec, names, email, states, selected, editable],
  );
  // Dragging moves the canvas's own copy; the spec takes the place it ends at.
  const [nodes, setNodes, onNodesChange] = useNodesState<StepNode>(derived);
  useEffect(() => setNodes(derived), [derived, setNodes]);
  const edges = useMemo<Edge[]>(
    () =>
      spec.edges.map((e) => ({
        id: `${e.from}->${e.to}`,
        source: e.from,
        target: e.to,
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
          label: e.branch === "true" ? "Yes" : "No",
          labelBgPadding: [6, 3] as [number, number],
          labelBgBorderRadius: 4,
        }),
      })),
    [spec, states, editable],
  );
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
        fitViewOptions={{ padding: 0.2, maxZoom: 1 }}
        minZoom={0.25}
        maxZoom={1.5}
        nodesDraggable={editable}
        nodesConnectable={editable}
        elementsSelectable={!!onSelect}
        deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
        onNodesChange={onNodesChange}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        onPaneClick={() => onSelect?.(undefined)}
        onNodeDragStop={(_, n) => onChange?.(moveNode(spec, n.id, n.position))}
        onConnect={(c) => onChange?.(connect(spec, c.source, c.target))}
        isValidConnection={(c) =>
          editable && !cannotConnect(spec, c.source, c.target)
        }
        onDelete={({ nodes: gone, edges: cut }) =>
          onChange?.(
            gone.reduce(
              (s, n) => removeStep(s, n.id),
              cut.reduce((s, e) => disconnect(s, e.source, e.target), spec),
            ),
          )
        }
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1.2} />
        <Controls showInteractive={false} />
      </ReactFlow>
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
