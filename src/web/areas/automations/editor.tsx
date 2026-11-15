import {
  Suspense,
  lazy,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ChevronRight,
  CircleStop,
  ListTree,
  PanelRightClose,
  PanelRightOpen,
  Pencil,
  Play,
  X,
} from "lucide-react";
import type {
  AutomationNode,
  AutomationSpec,
  FoundryAutomation,
  FoundryAutomationRun,
} from "../../../shared/wfe";
import { ApiError, sentence as asSentence } from "../../api";
import {
  Button,
  ButtonLink,
  Dialog,
  EmptyState,
  SkeletonText,
  Tabs,
  cx,
  formatNumber,
  useMedia,
  useStoredState,
  useTitle,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { Inspector, RemoveStep } from "./inspector";
import {
  KIND_HEADS,
  LIMITS,
  TEMPLATES,
  addStep,
  here,
  incomplete,
  parseSpec,
  removeStep,
  stepStates,
  tidy,
  triggerOf,
  type Kind,
  type StepState,
  type TemplateId,
} from "./model";
import { Outline } from "./outline";
import { nodeIcon, stepTitle } from "./parts";
import { automationPaths as paths } from "./paths";
import {
  automationQueries,
  cancelAutomationRun,
  keepAutomation,
  pauseAutomation,
  runAutomation,
  updateAutomation,
  useNames,
} from "./queries";
import {
  ApproveDialog,
  ReviewDialog,
  changesOf,
  saveRefusal,
  type Draft,
} from "./review";
import { RunNotice, RunSteps, RunsList, StepOutcome, useRuns } from "./runs";
import { RunHead, SettingsPanel, Summary } from "./summary";

// An automation on its own page, as the Explorer lays its graph out: a slim
// bar (the way back, its name, renamed in place, the Editor, Runs and
// Settings tabs, its outline, Run now and the Live switch), the canvas
// filling everything under it, and an inspector as the right-hand sidebar
// that scrolls on its own, hidden and shown from the bar or ] and
// remembered: the automation's summary with nothing selected, the selected
// step's settings with one. Its owner edits the steps there (the canvas or
// the outline) and saves them through the review of what runs unattended;
// going Live approves the saved steps (or resumes it), going off pauses it.
// The Runs tab lists the runs beside the canvas, which colours each step by
// how the run open went, the inspector saying what each found. Settings
// holds its name, description, time zone, approval and deletion. A new one
// from a template (or from scratch) is a draft on the same page until its
// first save. On a phone the canvas only shows the steps, the inspector is
// a sheet, and the outline edits them.

const AutomationCanvas = lazy(() => import("./canvas"));

const TABS = ["editor", "runs", "settings"] as const;
type Tab = (typeof TABS)[number];
const INSPECTOR = "foundry.automations.inspector";

/** /automations/:id and /automations/:id/runs/:runId. */
export function AutomationPage() {
  const { automationId = "", runId } = useParams(),
    { id: ws } = useWorkspace();
  const query = useQuery(automationQueries.automation(ws, automationId));
  const a = query.data?.automation;
  if (!a)
    return (
      <Frame bar={<Crumbs name={query.isPending ? "Automation" : "Gone"} />}>
        <div className="au-frame">
          {query.isPending ? (
            <SkeletonText lines={6} />
          ) : (
            <EmptyState
              title={
                query.error instanceof ApiError && query.error.status === 404
                  ? "This automation is gone"
                  : "Couldn’t load the automation"
              }
              text={query.error?.message}
              action={
                <ButtonLink to={paths.automations()}>
                  All automations
                </ButtonLink>
              }
            />
          )}
        </div>
      </Frame>
    );
  return (
    <Editor
      key={automationId}
      automation={a}
      runId={runId}
      template="blank"
      refreshFailed={query.isRefetchError}
    />
  );
}

/** /automations/new?template=: a draft from a template, on the canvas, until its first save. */
export function DraftPage({ templateId }: { templateId: string }) {
  const { id: ws } = useWorkspace(),
    lists = useNames(ws);
  const template =
    TEMPLATES.find((t) => t.id === templateId) ??
    TEMPLATES.find((t) => t.id === "blank")!;
  // The template with the workspace's first of each target, once read.
  const start = useMemo<Draft | undefined>(
    () =>
      lists.ready
        ? template.build({
            timeZone: here(),
            connector: lists.connectors[0],
            ontology: lists.ontologies[0],
            set: lists.sets[0],
          })
        : undefined,
    [lists.ready, template],
  );
  if (!start)
    return (
      <Frame bar={<Crumbs name="New automation" />}>
        <div className="au-frame">
          <SkeletonText lines={6} />
        </div>
      </Frame>
    );
  return <Editor key={template.id} start={start} template={template.id} />;
}

/** The page's frame: the bar, then what fills the rest of the viewport. */
export function Frame({
  bar,
  children,
}: {
  bar: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="au-page">
      <header className="au-top">{bar}</header>
      {children}
    </div>
  );
}

/** The way back to the overview, and the name (a heading, or what renames it). */
export function Crumbs({
  name,
  children,
}: {
  name: string;
  children?: ReactNode;
}) {
  useTitle(name);
  return (
    <nav className="au-crumbs" aria-label="Breadcrumbs">
      <Link to={paths.automations()} className="au-crumb">
        Automations
      </Link>
      <ChevronRight size={14} aria-hidden className="au-crumb-sep" />
      {children ?? <h1 className="au-crumb-name">{name}</h1>}
    </nav>
  );
}

/** The name, renamed in place by its owner: Enter or leaving saves, Escape cancels. */
function NameInPlace({
  name,
  canRename,
  onSave,
}: {
  name: string;
  canRename: boolean;
  onSave: (name: string) => Promise<void>;
}) {
  const [editing, setEditing] = useState(false),
    [value, setValue] = useState(name),
    [saving, setSaving] = useState<string | null>(null);
  // Enter's or Escape's blur, as the field goes, must not finish again.
  const open = useRef(false);
  const finish = (keep: boolean) => {
    if (!open.current) return;
    open.current = false;
    setEditing(false);
    const next = value.trim();
    if (keep && next && next !== name) {
      setSaving(next);
      void onSave(next).finally(() => setSaving(null));
    }
  };
  if (editing)
    return (
      <input
        className="au-name-input"
        value={value}
        maxLength={LIMITS.name}
        autoFocus
        aria-label="Name"
        onFocus={(e) => e.currentTarget.select()}
        onChange={(e) => setValue(e.target.value)}
        onBlur={() => finish(true)}
        onKeyDown={(e) => {
          if (e.key === "Enter") finish(true);
          if (e.key === "Escape") finish(false);
        }}
      />
    );
  return (
    <span className="au-crumb-title">
      <h1 className="au-crumb-name">{saving ?? name}</h1>
      {canRename && (
        <button
          type="button"
          className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
          aria-label={`Rename ${name}`}
          title="Rename"
          onClick={() => {
            setValue(name);
            open.current = true;
            setEditing(true);
          }}
        >
          <Pencil size={13} aria-hidden />
        </button>
      )}
    </span>
  );
}

function Editor({
  automation: a,
  start,
  runId,
  template,
  refreshFailed,
}: {
  // The automation as saved, read again as it changes; none for a draft.
  automation?: FoundryAutomation;
  // A draft's first steps.
  start?: Draft;
  runId?: string;
  template: TemplateId;
  refreshFailed?: boolean;
}) {
  const { id: ws, user } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    lists = useNames(ws);
  const email = useQuery(automationQueries.list(ws)).data?.email ?? true;
  const phone = useMedia("(max-width: 760px)");
  const [params] = useSearchParams();
  const asked = params.get("tab") as Tab | null;
  // A draft has no runs yet.
  const tab: Tab = runId
    ? "runs"
    : asked && TABS.includes(asked) && (a || asked !== "runs")
      ? asked
      : "editor";
  const setTab = (next: string) =>
    navigate(
      a
        ? paths.automation(
            a.id,
            next === "runs" || next === "settings" ? next : undefined,
          )
        : "?" + new URLSearchParams({ template, tab: next }),
      { replace: !runId },
    );

  // What is saved, and what this page changes: a draft of its name,
  // description and steps, from the automation as it was when the first
  // change was made (`base`, which a save names, so it never writes over a
  // change made elsewhere: the WFE refuses it). A new one is all draft, its
  // one token making a retried create the same automation.
  const saved = useMemo<Draft | undefined>(() => {
    const read = a && parseSpec(a.specJson || "");
    return a && read?.ok
      ? {
          name: a.name || "",
          description: a.description || "",
          spec: read.spec,
        }
      : undefined;
  }, [a?.name, a?.description, a?.specJson]);
  const [draft, setDraft] = useState<Draft | null>(start ?? null),
    [base, setBase] = useState<FoundryAutomation | undefined>(undefined),
    [token] = useState(() => crypto.randomUUID());
  const current = draft ?? saved;
  const editable = !a || !!a.canEdit;
  const change = (next: Partial<Draft>) => {
    if (!current) return;
    if (a && !base) setBase(a);
    setDraft({ ...current, ...next });
  };
  const setSpec = (spec: AutomationSpec) => change({ spec });
  // The draft as it stands now, for a call that ends after it changed.
  const latest = useRef(draft);
  useEffect(() => {
    latest.current = draft;
  }, [draft]);
  const discard = () => {
    setDraft(null);
    setBase(undefined);
  };

  const [selected, setSelected] = useState<string | undefined>(
    () => start?.spec.nodes.find((n) => incomplete(n))?.id,
  );
  // Another tab or run shows other steps: nothing is selected there.
  const shownAt = useRef(`${tab}/${runId}`);
  useEffect(() => {
    if (shownAt.current === `${tab}/${runId}`) return;
    shownAt.current = `${tab}/${runId}`;
    setSelected(undefined);
  }, [tab, runId]);
  const [shown, setShown] = useStoredState(INSPECTOR, "shown"),
    [sheet, setSheet] = useState(false),
    [outline, setOutline] = useState(false);
  const inspectorOpen = phone ? sheet : shown === "shown";
  const toggleInspector = () =>
    phone
      ? setSheet((v) => !v)
      : setShown(shown === "shown" ? "hidden" : "shown");
  // ] hides and shows the inspector, as on the Explorer.
  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (
        e.key !== "]" ||
        e.metaKey ||
        e.ctrlKey ||
        t?.closest("input, textarea, select, [contenteditable]")
      )
        return;
      e.preventDefault();
      toggleInspector();
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  });
  const select = (id: string | undefined) => {
    setSelected(id);
    if (phone && id) setSheet(true);
  };

  const [reviewing, setReviewing] = useState(false),
    [approving, setApproving] = useState(false),
    [pending, setPending] = useState<string | null>(null),
    [error, setError] = useState(""),
    // Why the Live switch can't change, shown once tapped.
    [whyShown, setWhyShown] = useState(false);
  const act = async (which: string, call: () => Promise<FoundryAutomation>) => {
    setPending(which);
    setError("");
    try {
      keepAutomation(qc, ws, await call());
    } catch (e) {
      setError(saveRefusal(e));
    } finally {
      setPending(null);
    }
  };

  const runs = useRuns(a),
    run = useQuery({
      ...automationQueries.run(ws, a?.id || "", runId || ""),
      enabled: !!a && !!runId,
    }).data?.run;
  // The run open, as it ran: its steps and how each went, to colour them.
  const ran = useMemo(
    () => (run ? parseSpec(run.specJson || "") : undefined),
    [run],
  );
  const states = useMemo(
    () =>
      tab === "runs" && run && ran?.ok ? stepStates(ran.spec, run) : undefined,
    [tab, run, ran],
  );

  if (!current)
    return (
      <Frame bar={<Crumbs name={a?.name || "Automation"} />}>
        <div className="au-frame">
          <EmptyState
            title="Its steps can’t be read"
            text="Foundry stored steps this page doesn’t know. Reload to update Foundry."
          />
        </div>
      </Frame>
    );

  // What the canvas shows: the steps as they stand; on Runs, as the run
  // open ran them, each coloured by how it went, else as saved (a draft
  // hasn't run).
  const shownSpec =
    tab === "runs"
      ? runId && ran?.ok
        ? ran.spec
        : (saved?.spec ?? current.spec)
      : current.spec;
  const canEditSteps = editable && tab === "editor";
  const node = shownSpec.nodes.find((n) => n.id === selected);

  // Before a save: every step complete, the graph one a run can take, a name.
  const named = (id: string) => {
    const n = current.spec.nodes.find((x) => x.id === id);
    return n ? `“${stepTitle(n, lists.names, email)}”` : id;
  };
  const read = parseSpec(current.spec, named);
  // Compared as a save sends it: trimmed, and written as the WFE writes it.
  const dirty =
    !!draft &&
    (!a ||
      !!changesOf(base ?? a, {
        ...draft,
        spec: read.ok ? read.spec : draft.spec,
      }));
  const missing = current.spec.nodes.flatMap((n) => {
    const why = incomplete(n);
    return why ? [{ id: n.id, text: `${named(n.id)}: ${why}.` }] : [];
  });
  const problems = [
    ...missing,
    // Only what starts it: the graph's own refusal names nodes.
    ...(current.spec.nodes.length < 2
      ? [{ id: undefined, text: "Add a step after what starts it." }]
      : !missing.length && !read.ok
        ? [{ id: read.nodeId, text: asSentence(read.error) }]
        : []),
    ...(current.name.trim()
      ? []
      : [{ id: undefined, text: "Give it a name." }]),
  ];
  const add = (kind: Exclude<Kind, "trigger">) => {
    const spec = current.spec,
      at = spec.nodes.find((n) => n.id === selected);
    const after =
      at?.kind === "condition" &&
      spec.edges.filter((e) => e.from === at.id).length >= 2
        ? undefined
        : (at?.id ?? spec.nodes[spec.nodes.length - 1]?.id);
    const added = addStep(spec, kind, after);
    setSpec(added.spec);
    select(added.id);
  };

  const rename = async (name: string) => {
    if (!a) return change({ name });
    try {
      const renamed = await updateAutomation(ws, a.id, {
        expectedRevision: (base ?? a).revision || "",
        name,
      });
      keepAutomation(qc, ws, renamed);
      // A draft made before or during the rename goes on from the renamed
      // automation, with its new name and every change made meanwhile.
      if (latest.current) {
        setBase(renamed);
        setDraft((d) => d && { ...d, name });
      }
    } catch (e) {
      setError(saveRefusal(e));
    }
  };
  const runNow = async () => {
    if (!a) return;
    setPending("run");
    setError("");
    try {
      const r = await runAutomation(ws, a.id, crypto.randomUUID());
      void qc.invalidateQueries({ queryKey: [ws, "automations"] });
      navigate(paths.automationRun(a.id, r.id));
    } catch (e) {
      setError(saveRefusal(e));
    } finally {
      setPending(null);
    }
  };
  const live = a?.status === "active";
  const liveWhy = !a
    ? "Save it first"
    : dirty
      ? "Save or discard your changes first"
      : live
        ? a.canPause
          ? undefined
          : "Only its owner, or an owner or admin, pauses it"
        : a.canApprove || a.canResume
          ? undefined
          : a.status === "needs_approval" && a.canEdit
            ? "Approve it in Foundry to go live"
            : "Only its owner turns it on";
  const flip = (on: boolean) => {
    if (!a) return;
    if (!on) return void act("pause", () => pauseAutomation(ws, a.id, true));
    if (a.status === "needs_approval") setApproving(true);
    else void act("resume", () => pauseAutomation(ws, a.id, false));
  };

  const bar = (
    <>
      <Crumbs name={current.name || "Untitled automation"}>
        {a ? (
          <NameInPlace
            key={current.name}
            name={current.name || a.name || "Automation"}
            canRename={!!a.canEdit}
            onSave={rename}
          />
        ) : (
          <input
            className="au-name-input"
            value={current.name}
            maxLength={LIMITS.name}
            placeholder="Name it"
            aria-label="Name"
            onChange={(e) => change({ name: e.target.value })}
          />
        )}
      </Crumbs>
      <Tabs
        label="Automation"
        idPrefix="au"
        className="au-tabs"
        value={tab}
        onChange={setTab}
        items={[
          { id: "editor", label: "Editor" },
          {
            id: "runs",
            // The runs read so far, and a + while there are more: they are
            // read a page at a time.
            label: (
              <>
                Runs
                {a && runs.query.data && (
                  <span className="hf-tab-count au-tab-count">
                    {formatNumber(runs.all.length)}
                    {runs.query.hasNextPage && "+"}
                  </span>
                )}
              </>
            ),
            disabled: !a,
          },
          { id: "settings", label: "Settings" },
        ]}
      />
      <div className="au-top-tools">
        {(dirty || !a) && (
          <span className="au-unsaved">
            {a && <span className="au-muted">Unsaved changes</span>}
            {a && (
              <Button size="sm" onClick={discard}>
                Discard
              </Button>
            )}
            {!a && (
              <ButtonLink size="sm" to={paths.automations()}>
                Cancel
              </ButtonLink>
            )}
            <Button
              size="sm"
              variant="primary"
              disabled={!!problems.length}
              title={problems.length ? problems[0]!.text : undefined}
              onClick={() => setReviewing(true)}
            >
              Review and save
            </Button>
          </span>
        )}
        {a?.canRun && !dirty && (
          <Button
            size="sm"
            icon={Play}
            pending={pending === "run"}
            disabled={!!pending}
            onClick={() => void runNow()}
          >
            Run now
          </Button>
        )}
        {tab !== "settings" && (
          <button
            type="button"
            className={cx(
              "hf-btn hf-btn--ghost hf-btn--sm",
              outline && "is-active",
            )}
            aria-pressed={outline}
            title="The steps as a list, for keyboards and screen readers"
            onClick={() => setOutline((v) => !v)}
          >
            <ListTree size={15} aria-hidden />
            Outline
          </button>
        )}
        {/* Off while it can't change, but still focusable: a tap, a key
            or a screen reader learns why. React reports the tap's change
            though it was prevented, so onChange checks again. */}
        <label className="checkbox au-live" title={liveWhy}>
          <input
            type="checkbox"
            role="switch"
            checked={live}
            disabled={!!pending}
            aria-disabled={!!liveWhy || undefined}
            aria-describedby={liveWhy ? "au-live-why" : undefined}
            onClick={(e) => {
              if (!liveWhy) return;
              e.preventDefault();
              setWhyShown(true);
            }}
            onChange={(e) => !liveWhy && flip(e.target.checked)}
          />
          Live
          {liveWhy && (
            <span id="au-live-why" className="hf-sr-only">
              {liveWhy}
            </span>
          )}
        </label>
        <button
          type="button"
          className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
          aria-label={
            inspectorOpen ? "Hide the inspector" : "Show the inspector"
          }
          aria-pressed={inspectorOpen}
          aria-keyshortcuts="]"
          title={`${inspectorOpen ? "Hide" : "Show"} the inspector (])`}
          onClick={toggleInspector}
        >
          {inspectorOpen ? (
            <PanelRightClose size={16} aria-hidden />
          ) : (
            <PanelRightOpen size={16} aria-hidden />
          )}
        </button>
      </div>
    </>
  );

  const notes = (
    <>
      {whyShown && liveWhy && (
        <p className="au-strip is-muted" role="status">
          <span>{liveWhy}.</span>
          <button
            type="button"
            className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
            aria-label="Dismiss"
            onClick={() => setWhyShown(false)}
          >
            <X size={14} aria-hidden />
          </button>
        </p>
      )}
      {refreshFailed && (
        <p className="au-strip is-muted" role="status">
          Couldn’t read it again: what shows may be out of date.
        </p>
      )}
      {error && (
        <p className="au-strip is-danger" role="alert">
          <span>{error}</span>
          <button
            type="button"
            className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon"
            aria-label="Dismiss"
            onClick={() => setError("")}
          >
            <X size={14} aria-hidden />
          </button>
        </p>
      )}
    </>
  );

  // The inspector: the selected step, the run open, or the summary.
  const step = node && (
    <StepPanel
      node={node}
      spec={shownSpec}
      lists={lists}
      email={email}
      editable={canEditSteps}
      readOnly={
        a?.canEdit
          ? "Edit its steps in the Editor tab."
          : "Only its owner changes its steps: it runs as them."
      }
      onChange={setSpec}
      onRemove={() => {
        setSpec(removeStep(current.spec, node.id));
        setSelected(undefined);
      }}
      outcome={
        tab === "runs" && run ? (
          <StepOutcome
            node={node}
            step={run.nodes?.find((n) => n.nodeId === node.id)}
            run={run}
          />
        ) : undefined
      }
      onBack={() => setSelected(undefined)}
    />
  );
  const inspector =
    step ??
    (tab === "runs" && a && runId ? (
      run && ran ? (
        <RunPanel
          automation={a}
          run={run}
          order={ran.ok ? ran.order : []}
          nodes={ran.ok ? ran.spec.nodes : []}
          states={states}
          lists={lists}
          email={email}
          onSelect={select}
        />
      ) : (
        <SkeletonText lines={5} />
      )
    ) : (
      <Summary
        automation={a}
        draft={current}
        saved={saved}
        dirty={dirty}
        mine={!a || a.ownerUserId === user.id}
        runs={runs.all}
        problems={problems}
        lists={lists}
        email={email}
        onSelect={select}
        onApprove={() => setApproving(true)}
        onRuns={() => setTab("runs")}
      />
    ));

  const stage =
    tab === "settings" ? (
      <SettingsPanel
        automation={a}
        draft={current}
        editable={editable}
        onChange={change}
        onApprove={() => setApproving(true)}
        lists={lists}
        email={email}
      />
    ) : outline ? (
      <div className="au-outline-stage">
        <Outline
          spec={shownSpec}
          names={lists.names}
          email={email}
          selected={selected}
          onSelect={select}
          {...(canEditSteps && { onChange: setSpec })}
        />
      </div>
    ) : (
      <Suspense fallback={<div className="au-flow" aria-busy="true" />}>
        <AutomationCanvas
          key={tab === "runs" ? `run:${runId || ""}` : "steps"}
          spec={shownSpec}
          names={lists.names}
          email={email}
          states={states}
          selected={selected}
          onSelect={select}
          {...(canEditSteps &&
            !phone && {
              onChange: setSpec,
              onAdd: add,
              onTidy: () => setSpec(tidy(current.spec)),
            })}
          label={
            states
              ? "Its steps on a canvas, coloured by how each went"
              : "Steps on a canvas"
          }
        />
      </Suspense>
    );

  return (
    <div className="au-page">
      <header className="au-top">{bar}</header>
      {notes}
      <div className="au-body">
        {tab === "runs" && a && (
          <aside className="au-runs" aria-label="Its runs">
            <RunsList
              automation={a}
              runs={runs}
              open={runId}
              trigger={saved ? triggerOf(saved.spec).config : undefined}
              names={lists.names}
            />
          </aside>
        )}
        <div
          className={cx("au-stage", tab === "settings" && "is-page")}
          role="tabpanel"
          id={`au-panel-${tab}`}
          aria-labelledby={`au-tab-${tab}`}
        >
          {stage}
        </div>
        {!phone && inspectorOpen && (
          <aside className="au-side" aria-label="Inspector">
            {inspector}
          </aside>
        )}
      </div>
      {phone && sheet && (
        <Dialog
          title={node ? "Step" : current.name || "Automation"}
          variant="drawer"
          onClose={() => setSheet(false)}
        >
          <div className="au-sheet">{inspector}</div>
        </Dialog>
      )}
      {reviewing && read.ok && (
        <ReviewDialog
          draft={{ ...current, spec: read.spec }}
          base={a ? (base ?? a) : undefined}
          token={token}
          onBase={(saved) => {
            if (a) setBase(saved);
          }}
          template={template}
          lists={lists}
          email={email}
          onClose={() => setReviewing(false)}
          onSaved={(saved) => {
            keepAutomation(qc, ws, saved);
            setReviewing(false);
            discard();
            if (!a) navigate(paths.automation(saved.id), { replace: true });
          }}
        />
      )}
      {approving && a && saved && (
        <ApproveDialog
          automation={a}
          spec={saved.spec}
          lists={lists}
          email={email}
          onClose={() => setApproving(false)}
          onApproved={(approved) => {
            keepAutomation(qc, ws, approved);
            setApproving(false);
          }}
        />
      )}
    </div>
  );
}

/** The selected step: its settings (read only unless its owner edits the steps), and in a run, what it did. */
function StepPanel({
  node,
  spec,
  lists,
  email,
  editable,
  readOnly,
  onChange,
  onRemove,
  outcome,
  onBack,
}: {
  node: AutomationNode;
  spec: AutomationSpec;
  lists: ReturnType<typeof useNames>;
  email: boolean;
  editable: boolean;
  // Why it can't be changed here, said when it can't.
  readOnly: string;
  onChange: (spec: AutomationSpec) => void;
  onRemove: () => void;
  outcome?: ReactNode;
  onBack: () => void;
}) {
  const Icon = nodeIcon(node);
  return (
    <div className="au-side-body">
      <button type="button" className="au-side-back" onClick={onBack}>
        <ChevronRight size={13} aria-hidden className="au-flip" />
        Back to the summary
      </button>
      {outcome !== undefined ? (
        <div className="au-inspector">
          <div className="au-inspector-head">
            <Icon size={15} aria-hidden />
            <h2>{KIND_HEADS[node.kind]}</h2>
          </div>
          <p className="au-inspector-sum">
            {stepTitle(node, lists.names, email)}
          </p>
          {outcome ?? (
            <p className="au-hint">The run didn’t reach this step.</p>
          )}
        </div>
      ) : (
        <>
          <fieldset className="au-fieldset" disabled={!editable}>
            <Inspector
              node={node}
              spec={spec}
              lists={lists}
              email={email}
              onChange={onChange}
            />
          </fieldset>
          {editable && node.kind !== "trigger" && (
            <RemoveStep onRemove={onRemove} />
          )}
          {!editable && <p className="au-hint">{readOnly}</p>}
        </>
      )}
    </div>
  );
}

/** The run open: what started it, how it went, and each step, which opens on the canvas. */
function RunPanel({
  automation,
  run,
  order,
  nodes,
  states,
  lists,
  email,
  onSelect,
}: {
  automation: FoundryAutomation;
  run: FoundryAutomationRun;
  order: string[];
  nodes: AutomationNode[];
  states?: ReadonlyMap<string, StepState>;
  lists: ReturnType<typeof useNames>;
  email: boolean;
  onSelect: (id: string) => void;
}) {
  const { id: ws } = useWorkspace(),
    qc = useQueryClient();
  const [cancelling, setCancelling] = useState(false),
    [error, setError] = useState("");
  return (
    <div className="au-side-body">
      <RunHead run={run} automation={automation} lists={lists} />
      {run.canCancel && (
        <Button
          size="sm"
          icon={CircleStop}
          pending={cancelling}
          onClick={async () => {
            setCancelling(true);
            setError("");
            try {
              qc.setQueryData(
                automationQueries.run(ws, automation.id, run.id).queryKey,
                { run: await cancelAutomationRun(ws, automation.id, run.id) },
              );
              void qc.invalidateQueries({ queryKey: [ws, "automations"] });
            } catch (e) {
              setError(asSentence((e as Error).message));
            } finally {
              setCancelling(false);
            }
          }}
        >
          Cancel run
        </Button>
      )}
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      <RunNotice run={run} />
      {states ? (
        <RunSteps
          run={run}
          order={order}
          nodes={nodes}
          states={states}
          names={lists.names}
          email={email}
          onSelect={onSelect}
        />
      ) : run.status === "skipped" ? (
        <p className="au-hint">It was skipped, so none of its steps ran.</p>
      ) : (
        <p className="au-hint">Its steps can’t be read.</p>
      )}
    </div>
  );
}
