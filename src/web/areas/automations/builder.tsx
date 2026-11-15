import {
  Suspense,
  lazy,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, Plus } from "lucide-react";
import type { AutomationSpec, FoundryAutomation } from "../../../shared/wfe";
import {
  Button,
  Dialog,
  EmptyState,
  Page,
  PageHeader,
  Segmented,
  SkeletonText,
  useMedia,
} from "../../kit";
import { sentence as asSentence } from "../../api";
import { useWorkspace } from "../../ui";
import { Inspector, RemoveStep } from "./inspector";
import {
  KIND_LABELS,
  LIMITS,
  TEMPLATES,
  addStep,
  automationRefusal,
  canonicalJson,
  incomplete,
  parseSpec,
  removeStep,
  sameSteps,
  sentence,
  type Kind,
  type TemplateId,
} from "./model";
import { Outline } from "./outline";
import { StepsPreview, here, stepTitle } from "./parts";
import { automationPaths as paths } from "./paths";
import {
  approveAutomation,
  automationQueries,
  createAutomation,
  keepAutomation,
  updateAutomation,
  useNames,
} from "./queries";

// The builder: an automation's graph on a canvas (canvas.tsx, lazy) beside
// an inspector of the selected step, or as an outline (outline.tsx), the
// same graph for keyboards and screen readers and the way to edit it on a
// phone, where the canvas only shows it. A new one starts from a template
// (?template=), its steps filled with the workspace's first connection,
// ontology or set. Saving shows exactly what will run unattended, its
// steps in words and what each Assistant step may do without asking, and
// its owner's click on Save and turn on is their approval (:approve);
// saved without it, it waits for that click on its page.

const AutomationCanvas = lazy(() => import("./canvas"));

type Draft = { name: string; description: string; spec: AutomationSpec };
const ADDABLE: Exclude<Kind, "trigger">[] = [
  "assistant",
  "action",
  "condition",
  "notify",
];

export function BuilderPage() {
  const { automationId = "" } = useParams(),
    [params] = useSearchParams(),
    { id: ws } = useWorkspace(),
    lists = useNames(ws);
  const editing = !!automationId;
  const existing = useQuery({
    ...automationQueries.automation(ws, automationId),
    enabled: editing,
    refetchInterval: false,
  });
  const template =
    TEMPLATES.find((t) => t.id === params.get("template")) ??
    TEMPLATES.find((t) => t.id === "blank")!;
  // Where the draft starts: the automation as saved, or the template with
  // the workspace's first of each target, once those are read.
  const start = useMemo<Draft | undefined>(() => {
    if (editing) {
      const a = existing.data?.automation;
      const read = a && parseSpec(a.specJson || "");
      return a && read?.ok
        ? {
            name: a.name || "",
            description: a.description || "",
            spec: read.spec,
          }
        : undefined;
    }
    if (!lists.ready) return undefined;
    return template.build({
      timeZone: here(),
      connector: lists.connectors[0],
      ontology: lists.ontologies[0],
      set: lists.sets[0],
    });
  }, [editing, existing.data, lists.ready, template]);
  const header = (
    <PageHeader
      title={editing ? "Edit automation" : "New automation"}
      breadcrumbs={[
        { label: "Automations", to: paths.automations() },
        ...(editing && existing.data
          ? [
              {
                label: existing.data.automation.name || "Automation",
                to: paths.automation(automationId),
              },
            ]
          : []),
        { label: editing ? "Edit" : "New" },
      ]}
    />
  );
  if (editing && existing.isError)
    return (
      <Page>
        {header}
        <EmptyState
          title="Couldn’t load the automation"
          text={existing.error.message}
          action={
            <Button onClick={() => void existing.refetch()}>Retry</Button>
          }
        />
      </Page>
    );
  if (editing && existing.data && !existing.data.automation.canEdit)
    return (
      <Page>
        {header}
        <EmptyState
          title="Only its owner changes this automation"
          text="It runs as them, so they decide its steps."
        />
      </Page>
    );
  if (!start)
    return (
      <Page>
        {header}
        <SkeletonText lines={8} />
      </Page>
    );
  return (
    <Builder
      key={automationId || template.id}
      start={start}
      existing={editing ? existing.data?.automation : undefined}
      template={template.id}
      lists={lists}
      header={header}
    />
  );
}

function Builder({
  start,
  existing,
  template,
  lists,
  header,
}: {
  start: Draft;
  existing?: FoundryAutomation;
  template: TemplateId;
  lists: ReturnType<typeof useNames>;
  header: ReactNode;
}) {
  const { id: ws } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const list = useQuery(automationQueries.list(ws)),
    email = list.data?.email ?? true;
  const phone = useMedia("(max-width: 760px)");
  const [draft, setDraft] = useState(start),
    [selected, setSelected] = useState<string | undefined>(
      () =>
        start.spec.nodes.find((n) => incomplete(n))?.id ??
        start.spec.nodes[0]?.id,
    ),
    [view, setView] = useState<"canvas" | "outline">("canvas"),
    [reviewing, setReviewing] = useState(false);
  // On a phone the canvas only shows the graph: the outline edits it.
  useEffect(() => {
    if (phone) setView("outline");
  }, [phone]);
  const spec = draft.spec,
    setSpec = (next: AutomationSpec) => setDraft((d) => ({ ...d, spec: next }));
  const node = spec.nodes.find((n) => n.id === selected);
  const name = (id: string) => {
    const n = spec.nodes.find((x) => x.id === id);
    return n ? `“${stepTitle(n, lists.names, email)}”` : id;
  };
  const missing = spec.nodes.flatMap((n) => {
    const why = incomplete(n);
    return why ? [{ id: n.id, why }] : [];
  });
  const read = parseSpec(spec, name);
  const unchanged =
    !!existing &&
    draft.name.trim() === existing.name &&
    draft.description.trim() === (existing.description || "") &&
    canonicalJson(spec) === existing.specJson;
  const problems = [
    ...missing.map((m) => ({ id: m.id, text: `${name(m.id)}: ${m.why}.` })),
    ...(!missing.length && !read.ok
      ? [{ id: read.nodeId, text: asSentence(read.error) }]
      : []),
    ...(draft.name.trim() ? [] : [{ id: undefined, text: "Give it a name." }]),
  ];
  const add = (kind: Exclude<Kind, "trigger">) => {
    const after =
      node?.kind === "condition" &&
      spec.edges.filter((e) => e.from === node.id).length >= 2
        ? undefined
        : (node?.id ?? spec.nodes[spec.nodes.length - 1]?.id);
    const added = addStep(spec, kind, after);
    setSpec(added.spec);
    setSelected(added.id);
  };
  return (
    <Page width="full" className="au-builder-page">
      {header}
      <div className="au-builder-fields">
        <label className="hf-field">
          <span>Name</span>
          <input
            value={draft.name}
            maxLength={LIMITS.name}
            placeholder="e.g. Monday summary"
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
          />
        </label>
        <label className="hf-field">
          <span>Description (optional)</span>
          <input
            value={draft.description}
            maxLength={LIMITS.description}
            placeholder="e.g. What the team reads every Monday"
            onChange={(e) =>
              setDraft((d) => ({ ...d, description: e.target.value }))
            }
          />
        </label>
      </div>
      <p className="au-sentence" aria-live="polite">
        {sentence(spec, lists.names, email, here())}
      </p>
      <div className="au-builder">
        <section className="au-builder-main" aria-label="Steps">
          <div className="au-builder-bar">
            <div className="au-add" role="group" aria-label="Add a step">
              {ADDABLE.map((kind) => (
                <Button
                  key={kind}
                  size="sm"
                  icon={Plus}
                  disabled={spec.nodes.length >= LIMITS.nodes}
                  onClick={() => add(kind)}
                >
                  {KIND_LABELS[kind]}
                </Button>
              ))}
            </div>
            <Segmented
              label="Show the steps as"
              value={view}
              options={[
                { value: "canvas", label: "Canvas" },
                { value: "outline", label: "Outline" },
              ]}
              onChange={(v) => setView(v as "canvas" | "outline")}
            />
          </div>
          {view === "canvas" ? (
            <Suspense fallback={<div className="au-flow" aria-busy="true" />}>
              <AutomationCanvas
                spec={spec}
                names={lists.names}
                email={email}
                selected={selected}
                onSelect={setSelected}
                {...(!phone && { onChange: setSpec })}
                label="Steps on a canvas"
              />
              {!phone && (
                <p className="au-hint">
                  Drag a step to move it. Join two steps from the lower dot of
                  one to the upper dot of the next. Select a join and press
                  Delete to cut it.
                </p>
              )}
            </Suspense>
          ) : (
            <Outline
              spec={spec}
              names={lists.names}
              email={email}
              selected={selected}
              onSelect={setSelected}
              onChange={setSpec}
            />
          )}
        </section>
        <aside className="au-builder-side" aria-label="Selected step">
          {node ? (
            <>
              <Inspector
                node={node}
                spec={spec}
                lists={lists}
                email={email}
                onChange={setSpec}
              />
              {node.kind !== "trigger" && (
                <RemoveStep
                  onRemove={() => {
                    setSpec(removeStep(spec, node.id));
                    setSelected(undefined);
                  }}
                />
              )}
            </>
          ) : (
            <p className="au-hint">Select a step to change what it does.</p>
          )}
        </aside>
      </div>
      <div className="au-builder-foot">
        {!!problems.length && (
          <ul className="au-problems" aria-label="Before you save">
            {problems.map((p, i) => (
              <li key={i}>
                <CircleAlert size={14} aria-hidden />
                {p.id ? (
                  <button
                    type="button"
                    className="au-link"
                    onClick={() => setSelected(p.id)}
                  >
                    {p.text}
                  </button>
                ) : (
                  p.text
                )}
              </li>
            ))}
          </ul>
        )}
        <div className="au-builder-actions">
          <Button
            onClick={() =>
              navigate(
                existing ? paths.automation(existing.id) : paths.automations(),
              )
            }
          >
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!!problems.length || unchanged}
            onClick={() => setReviewing(true)}
          >
            Review and save
          </Button>
        </div>
      </div>
      {reviewing && read.ok && (
        <ReviewDialog
          draft={{ ...draft, spec: read.spec }}
          existing={existing}
          template={template}
          lists={lists}
          email={email}
          onClose={() => setReviewing(false)}
          onSaved={(a) => {
            keepAutomation(qc, ws, a);
            navigate(paths.automation(a.id));
          }}
        />
      )}
    </Page>
  );
}

/**
 * What the owner approves: the automation in a sentence and every step as
 * it will run unattended, as them. Save and turn on approves it; Save only
 * keeps it waiting for approval. An edit that only moved steps keeps the
 * approval it had.
 */
function ReviewDialog({
  draft,
  existing,
  template,
  lists,
  email,
  onClose,
  onSaved,
}: {
  draft: Draft;
  existing?: FoundryAutomation;
  template: TemplateId;
  lists: ReturnType<typeof useNames>;
  email: boolean;
  onClose: () => void;
  onSaved: (a: FoundryAutomation) => void;
}) {
  const { id: ws } = useWorkspace();
  const [pending, setPending] = useState<"on" | "save" | null>(null),
    [error, setError] = useState(""),
    [token] = useState(() => crypto.randomUUID());
  const save = async (turnOn: boolean) => {
    setPending(turnOn ? "on" : "save");
    setError("");
    try {
      const specJson = canonicalJson(draft.spec);
      let a = existing
        ? await updateAutomation(ws, existing.id, {
            expectedRevision: existing.revision || "",
            ...(draft.name.trim() !== existing.name && {
              name: draft.name.trim(),
            }),
            ...(draft.description.trim() !== (existing.description || "") && {
              description: draft.description.trim(),
            }),
            ...(specJson !== existing.specJson && { specJson }),
          })
        : await createAutomation(
            ws,
            {
              name: draft.name.trim(),
              description: draft.description.trim(),
              specJson,
              clientToken: token,
            },
            template,
          );
      if (turnOn && a.status === "needs_approval")
        a = await approveAutomation(ws, a.id, a.revision || "");
      onSaved(a);
    } catch (e) {
      setError(
        automationRefusal(e) === "FOUNDRY_AUTOMATION_CHANGED"
          ? "It was changed elsewhere since you opened it. Go back to its page to see the change, then edit it again."
          : automationRefusal(e) === "FOUNDRY_AUTOMATION_LIMIT"
            ? "This workspace has as many automations as it may keep (50). Delete one first."
            : asSentence((e as Error).message),
      );
    } finally {
      setPending(null);
    }
  };
  // Only moved, renamed or described again, it keeps the approval it had.
  const before = existing && parseSpec(existing.specJson || "");
  const approved =
    !!before?.ok &&
    sameSteps(before.spec, draft.spec) &&
    existing?.status !== "needs_approval";
  return (
    <Dialog
      title="Review what runs unattended"
      size="lg"
      onClose={onClose}
      footer={
        approved ? (
          <>
            <Button onClick={onClose}>Back</Button>
            <Button
              variant="primary"
              pending={pending === "save"}
              disabled={!!pending}
              onClick={() => void save(false)}
            >
              Save
            </Button>
          </>
        ) : (
          <>
            <Button onClick={onClose}>Back</Button>
            <Button
              pending={pending === "save"}
              disabled={!!pending}
              onClick={() => void save(false)}
            >
              Save, turn on later
            </Button>
            <Button
              variant="primary"
              pending={pending === "on"}
              disabled={!!pending}
              onClick={() => void save(true)}
            >
              Save and turn on
            </Button>
          </>
        )
      }
    >
      <div className="au-review">
        <p className="au-sentence">
          {sentence(draft.spec, lists.names, email, here())}
        </p>
        <StepsPreview
          spec={draft.spec}
          names={lists.names}
          email={email}
          label="Steps that run unattended"
        />
        <p className="au-review-note">
          {approved
            ? "Its steps are the ones you approved: saving keeps it as it is."
            : "It runs as you, with what you may do when it runs. Turning it on approves exactly these steps; any change to them waits for your approval again. Its Assistant steps spend the workspace’s automations budget."}
        </p>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
    </Dialog>
  );
}
