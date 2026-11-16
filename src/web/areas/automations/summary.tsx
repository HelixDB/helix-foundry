import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { Check, CircleAlert, Trash2, Zap } from "lucide-react";
import type {
  AutomationNodeConfig,
  FoundryAutomation,
  FoundryAutomationRun,
} from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  KeyValue,
  Select,
  StatusBadge,
  StatusDot,
  formatDate,
  formatRelative,
  plural,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { zoneOptions } from "./inspector";
import {
  LIMITS,
  eventText,
  here,
  nextFire,
  parseEvent,
  parseSpec,
  sentence,
  setConfig,
  stoppedText,
  triggerOf,
  triggerText,
  usdText,
  validZone,
  type Names,
} from "./model";
import { OntologyImpact } from "./impact";
import { RUN_STATUS, StepsPreview, automationStatus } from "./parts";
import { automationPaths as paths } from "./paths";
import { automationQueries, deleteAutomation, type useNames } from "./queries";
import type { Draft } from "./review";
import { took } from "./runs";

// What the canvas page's inspector says with nothing selected (the
// automation in plain words: what it does, why it isn't running, what runs
// unattended, its last runs), of the run open, and its Settings tab.

type Lists = ReturnType<typeof useNames>;
type Problem = { id?: string; text: string };

const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
/** What starts it, in words. */
const startsText = (spec: Draft["spec"], names: Names) => {
  const t = triggerOf(spec).config;
  return t.kind === "manual"
    ? "only when it is run by hand"
    : t.kind === "schedule"
      ? triggerText(t, names)
      : `when ${triggerText(t, names)}`;
};

/** Why its Save waits: what each problem names opens its step. */
export function Problems({
  problems,
  onSelect,
}: {
  problems: Problem[];
  onSelect: (id: string) => void;
}) {
  if (!problems.length) return null;
  return (
    <ul className="au-problems" aria-label="Before you save">
      {problems.map((p, i) => (
        <li key={i}>
          <CircleAlert size={14} aria-hidden />
          {p.id ? (
            <button
              type="button"
              className="au-link"
              onClick={() => onSelect(p.id!)}
            >
              {p.text}
            </button>
          ) : (
            p.text
          )}
        </li>
      ))}
    </ul>
  );
}

/**
 * The automation in plain words, with nothing selected: what it does, why
 * it isn't running (and its owner's approval, which turns it on), what runs
 * unattended, when it starts and runs next, whose it is, and its last runs.
 * A draft's says what its save waits for. What runs unattended, and when it
 * starts, are what is saved: changes run only once saved.
 */
export function Summary({
  automation: a,
  draft,
  saved,
  dirty,
  mine,
  runs,
  problems,
  lists,
  email,
  onSelect,
  onApprove,
  onRuns,
}: {
  automation?: FoundryAutomation;
  // The steps as they stand, and as saved (none for a new one).
  draft: Draft;
  saved?: Draft;
  dirty: boolean;
  mine: boolean;
  runs: FoundryAutomationRun[];
  problems: Problem[];
  lists: Lists;
  email: boolean;
  onSelect: (id: string) => void;
  onApprove: () => void;
  onRuns: () => void;
}) {
  const now = useNow(30_000);
  const status = a && automationStatus(a),
    stopped = a && stoppedText(a, mine),
    lastFailed = runs.find((r) => r.status === "failed"),
    runsNow = a && saved ? saved.spec : draft.spec,
    trigger = triggerOf(runsNow).config;
  return (
    <div className="au-side-body">
      <div className="au-summary-head">
        <span className="au-summary-icon" aria-hidden>
          <Zap size={15} />
        </span>
        <h2>{draft.name || "Untitled automation"}</h2>
      </div>
      {status && <StatusBadge status={status.dot} label={status.label} />}
      <p className="au-summary-text">
        {sentence(draft.spec, lists.names, email)}
      </p>
      {draft.description && (
        <p className="au-summary-note">{draft.description}</p>
      )}
      <Problems problems={problems} onSelect={onSelect} />
      {a && stopped && (
        <div
          className={
            a.status === "needs_approval" ? "au-callout" : "au-callout is-muted"
          }
          role="status"
        >
          <p>{stopped}</p>
          {a.pausedReason === "failures" && lastFailed?.error && (
            <p>The last one failed because: {lastFailed.error}</p>
          )}
          {a.canApprove && (
            <Button
              size="sm"
              variant="primary"
              icon={Check}
              onClick={onApprove}
            >
              Review and approve
            </Button>
          )}
        </div>
      )}
      {a?.status === "active" &&
        a.backoffUntil &&
        Date.parse(a.backoffUntil) > now && (
          <div className="au-callout is-muted" role="status">
            <p>
              Its last{" "}
              {a.failures && a.failures > 1 ? plural(a.failures, "run") : "run"}{" "}
              failed, so it waits until {formatDate(a.backoffUntil)} before
              starting on its own again. After 3 failures in a row it pauses.
            </p>
          </div>
        )}
      <section className="au-side-section" aria-label="What runs unattended">
        <h3>What runs unattended</h3>
        <StepsPreview spec={runsNow} names={lists.names} email={email} />
        {a && dirty && (
          <p className="au-hint">
            As saved: your changes run once you save them.
          </p>
        )}
      </section>
      {a && (
        <KeyValue
          className="au-side-facts"
          items={[
            {
              label: "Starts",
              value: capital(startsText(runsNow, lists.names)),
            },
            {
              label: "Next run",
              value: a.nextRunAt
                ? formatDate(a.nextRunAt)
                : a.status === "active"
                  ? "When it is next started"
                  : "—",
            },
            {
              label: "Owner",
              value: mine ? "You" : "Another member",
              hint: mine ? undefined : "It runs as them.",
            },
          ]}
        />
      )}
      {a && <OntologyImpact automation={a} />}
      {a && (
        <section className="au-side-section" aria-label="Last runs">
          <h3>Last runs</h3>
          {runs.length ? (
            <>
              <ul className="au-last-runs">
                {runs.slice(0, 5).map((r) => {
                  const s = RUN_STATUS[r.status || "queued"];
                  return (
                    <li key={r.id}>
                      <Link to={paths.automationRun(a.id, r.id)}>
                        <StatusDot status={s.status} label={s.label} />
                        <span>
                          {eventText(
                            parseEvent(r.triggerEventJson),
                            trigger,
                            lists.names,
                          )}
                        </span>
                        <small>
                          {r.createdAt ? formatRelative(r.createdAt, now) : ""}
                        </small>
                      </Link>
                    </li>
                  );
                })}
              </ul>
              <button type="button" className="au-link" onClick={onRuns}>
                All runs
              </button>
            </>
          ) : (
            <p className="au-hint">
              {a.status === "active"
                ? "None yet: it runs each time it starts."
                : "None yet."}
            </p>
          )}
        </section>
      )}
    </div>
  );
}

/** The run open, in the inspector's head: its outcome, what started it, when, how long and what it cost. */
export function RunHead({
  run,
  automation,
  lists,
}: {
  run: FoundryAutomationRun;
  automation: FoundryAutomation;
  lists: Lists;
}) {
  const s = RUN_STATUS[run.status || "queued"],
    read = parseSpec(automation.specJson || "");
  return (
    <>
      <div className="au-summary-head">
        <h2>Run</h2>
        <StatusBadge status={s.status} label={s.label} />
      </div>
      <KeyValue
        className="au-side-facts"
        items={[
          {
            label: "What started it",
            value: eventText(
              parseEvent(run.triggerEventJson),
              read.ok ? triggerOf(read.spec).config : undefined,
              lists.names,
            ),
          },
          {
            label: "Started",
            value: run.createdAt ? formatDate(run.createdAt) : "—",
          },
          { label: "Took", value: took(run) },
          {
            label: "Cost",
            value: usdText(run.usdMicros),
            hint: "What its Founder steps spent.",
          },
        ]}
      />
    </>
  );
}

/**
 * The Settings tab: its name and description and its schedule's time zone
 * (a draft's, saved with its steps), its approval, and deleting it.
 */
export function SettingsPanel({
  automation: a,
  draft,
  editable,
  onChange,
  onApprove,
  lists,
  email,
}: {
  automation?: FoundryAutomation;
  draft: Draft;
  editable: boolean;
  onChange: (next: Partial<Draft>) => void;
  onApprove: () => void;
  lists: Lists;
  email: boolean;
}) {
  const { id: ws, user } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const [deleting, setDeleting] = useState(false);
  const trigger = triggerOf(draft.spec),
    t = trigger.config,
    saved = a && parseSpec(a.specJson || "");
  const status = a && automationStatus(a),
    stopped = a && stoppedText(a, a.ownerUserId === user.id);
  return (
    <div className="au-settings">
      <section className="au-settings-section" aria-labelledby="au-about">
        <h2 id="au-about">About</h2>
        <label className="hf-field">
          <span>Name</span>
          <input
            value={draft.name}
            maxLength={LIMITS.name}
            readOnly={!editable}
            placeholder="e.g. Monday summary"
            onChange={(e) => onChange({ name: e.target.value })}
          />
        </label>
        <label className="hf-field">
          <span>Description (optional)</span>
          <textarea
            rows={3}
            value={draft.description}
            maxLength={LIMITS.description}
            readOnly={!editable}
            placeholder="e.g. What the team reads every Monday"
            onChange={(e) => onChange({ description: e.target.value })}
          />
        </label>
      </section>
      <section className="au-settings-section" aria-labelledby="au-zone">
        <h2 id="au-zone">Time zone</h2>
        {t.kind === "schedule" ? (
          <>
            <label className="hf-field">
              <span>Its schedule keeps the time of</span>
              <Select
                label="Time zone"
                value={t.time_zone}
                disabled={!editable}
                options={zoneOptions(t.time_zone)}
                onChange={(time_zone) =>
                  onChange({
                    spec: setConfig(draft.spec, trigger.id, {
                      kind: "trigger",
                      config: { ...t, time_zone },
                    } as AutomationNodeConfig),
                  })
                }
              />
            </label>
            {validZone(t.time_zone) && (
              <p className="au-hint">
                Next:{" "}
                {formatDate(new Date(nextFire(t, Date.now())).toISOString())}
                {t.time_zone === here() ? "" : " your time"}. A new time zone
                changes when it runs: it waits for approval again once saved.
              </p>
            )}
          </>
        ) : (
          <p className="au-hint">
            Only a schedule keeps a time zone: this one starts{" "}
            {startsText(draft.spec, lists.names)}.
          </p>
        )}
      </section>
      {a && status && saved?.ok && (
        <section className="au-settings-section" aria-labelledby="au-approval">
          <h2 id="au-approval">Approval</h2>
          <div className="au-approval">
            <StatusBadge status={status.dot} label={status.label} />
            <span className="au-hint">
              {a.approvedAt && a.status !== "needs_approval"
                ? `Its steps were approved ${formatDate(a.approvedAt)}.`
                : (stopped ?? "")}
            </span>
          </div>
          <p className="au-hint">
            It runs only the steps its owner approved, as them. Any change to
            its steps waits for their approval again; moving them on the canvas,
            renaming or describing it doesn’t.
          </p>
          <StepsPreview
            spec={saved.spec}
            names={lists.names}
            email={email}
            label="Approved steps"
          />
          {a.canApprove && (
            <Button variant="primary" icon={Check} onClick={onApprove}>
              Review and approve
            </Button>
          )}
        </section>
      )}
      {a?.canDelete && (
        <section className="au-settings-section" aria-labelledby="au-delete">
          <h2 id="au-delete">Delete</h2>
          <p className="au-hint">
            Its runs go with it, and a run still going stops. Nothing it already
            did is undone.
          </p>
          <Button
            icon={Trash2}
            variant="danger"
            onClick={() => setDeleting(true)}
          >
            Delete automation
          </Button>
        </section>
      )}
      {deleting && a && (
        <ConfirmDialog
          title={`Delete “${a.name}”?`}
          description="Its runs go with it, and a run still going stops. Nothing it already did is undone."
          confirmLabel="Delete"
          tone="danger"
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            await deleteAutomation(ws, a.id);
            setDeleting(false);
            qc.removeQueries({
              queryKey: automationQueries.automation(ws, a.id).queryKey,
            });
            await qc.invalidateQueries({ queryKey: [ws, "automations"] });
            navigate(paths.automations(), { replace: true });
          }}
        />
      )}
    </div>
  );
}
