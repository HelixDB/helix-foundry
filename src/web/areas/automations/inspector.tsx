import { ArrowDownUp, Trash2 } from "lucide-react";
import { useState } from "react";
import type {
  AutomationAction,
  AutomationNode,
  AutomationNodeConfig,
  AutomationSpec,
  AutomationTrigger,
} from "../../../shared/wfe";
import {
  Button,
  Segmented,
  Select,
  formatDate,
  type SelectOption,
} from "../../kit";
import {
  KIND_HEADS,
  LIMITS,
  MAX_REVIEWS_WAITING,
  WEEKDAYS,
  actionText,
  edgesOutOf,
  here,
  nextFire,
  sameAction,
  setConfig,
  triggerOfKind,
  validZone,
  wholeNumber,
  type Weekday,
} from "./model";
import { nodeIcon, stepTitle } from "./parts";
import { zoneCity, zoneLabel } from "../../../shared/time-zone";
import type { useNames } from "./queries";

// The builder's inspector: what the selected step does, in plain words and
// pickers (a time, a day, a connection by its name), never JSON or a cron.
// Every change comes back as a new spec (onChange); Remove takes the step
// out, joining what came before it to what came after.

type Lists = ReturnType<typeof useNames>;
type Config<K extends AutomationNode["kind"]> = Extract<
  AutomationNodeConfig,
  { kind: K }
>["config"];

const TRIGGERS: SelectOption<AutomationTrigger["kind"]>[] = [
  { value: "schedule", label: "On a schedule" },
  { value: "sync_finished", label: "When a connection finishes syncing" },
  { value: "load_ready", label: "When a load of an ontology is ready" },
  { value: "document_arrived", label: "When documents arrive in a set" },
  { value: "generation_finished", label: "When a draft of an ontology ends" },
  { value: "reviews_waiting", label: "When reviews wait in an ontology" },
  { value: "manual", label: "Only when I run it" },
];
const OUTCOMES: SelectOption<"succeeded" | "failed" | "any">[] = [
  { value: "succeeded", label: "succeeds" },
  { value: "failed", label: "fails" },
  { value: "any", label: "ends either way" },
];
const DAYS: SelectOption<Weekday>[] = WEEKDAYS.map((d) => ({
  value: d,
  label: d.charAt(0).toUpperCase() + d.slice(1),
}));
const ZONES: string[] = (() => {
  const all = (
    Intl as { supportedValuesOf?: (key: "timeZone") => string[] }
  ).supportedValuesOf?.("timeZone");
  return all?.length ? all : ["UTC"];
})();
let zoneChoices: SelectOption[] | undefined;

/**
 * A time zone picker's options: `current`, the browser's, then every zone
 * this runtime names, each by its city and offset now ("New York ·
 * GMT−5"); the id is the value. Every zone's label is worked out once.
 */
export function zoneOptions(current: string): SelectOption[] {
  zoneChoices ??= ZONES.map((z) => ({
    value: z,
    label: zoneLabel(z) ?? zoneCity(z),
  }));
  const first = [...new Set([current, here()])].map((z) => ({
    value: z,
    label: zoneLabel(z) ?? zoneCity(z),
  }));
  return [
    ...first,
    ...zoneChoices.filter((o) => !first.some((f) => f.value === o.value)),
  ];
}

/** A target's picker: the workspace's of its kind, by name; one gone shows as gone. */
function targets(
  list: { id: string; name: string }[],
  current: string,
): SelectOption[] {
  const options = list.map((x) => ({ value: x.id, label: x.name }));
  return current && !list.some((x) => x.id === current)
    ? [{ value: current, label: "(gone)", disabled: true }, ...options]
    : options;
}

export function Inspector({
  node,
  spec,
  lists,
  email,
  onChange,
}: {
  node: AutomationNode;
  spec: AutomationSpec;
  lists: Lists;
  email: boolean;
  onChange: (spec: AutomationSpec) => void;
}) {
  const set = (config: AutomationNodeConfig) =>
    onChange(setConfig(spec, node.id, config));
  const Icon = nodeIcon(node);
  return (
    <div className="au-inspector">
      <div className="au-inspector-head">
        <Icon size={15} aria-hidden />
        <h2>{KIND_HEADS[node.kind]}</h2>
      </div>
      {node.kind === "trigger" && (
        <TriggerFields
          t={node.config}
          lists={lists}
          onChange={(config) => set({ kind: "trigger", config })}
        />
      )}
      {node.kind === "assistant" && (
        <AssistantFields
          c={node.config}
          lists={lists}
          onChange={(config) => set({ kind: "assistant", config })}
        />
      )}
      {node.kind === "action" && (
        <ActionFields
          a={node.config}
          lists={lists}
          label="What it does"
          onChange={(config) => set({ kind: "action", config })}
        />
      )}
      {node.kind === "condition" && (
        <ConditionFields
          c={node.config}
          spec={spec}
          id={node.id}
          lists={lists}
          email={email}
          onChange={(config) => set({ kind: "condition", config })}
          onSwap={() =>
            onChange({
              ...spec,
              edges: spec.edges.map((e) =>
                e.from === node.id && e.branch
                  ? { ...e, branch: e.branch === "true" ? "false" : "true" }
                  : e,
              ),
            })
          }
        />
      )}
      {node.kind === "notify" && (
        <>
          <label className="hf-field">
            <span>Subject (optional)</span>
            <input
              value={node.config.message}
              maxLength={LIMITS.message}
              placeholder="e.g. Your weekly summary"
              onChange={(e) =>
                set({ kind: "notify", config: { message: e.target.value } })
              }
            />
          </label>
          <p className="au-hint">
            {email
              ? "It emails you this, with what Founder’s steps before it found. Links are taken out of their words."
              : "This Foundry sends no email: the run records what Founder’s steps found instead."}
          </p>
        </>
      )}
    </div>
  );
}

/** The inspector's Remove, under it; never for the trigger. */
/**
 * A whole-number field. Its text is the person's while they type, so it can
 * be emptied and retyped; only a number commits (`wholeNumber`, clamped),
 * and leaving the field shows the number kept.
 */
function WholeNumber({
  value,
  min,
  max,
  onChange,
}: {
  value: number;
  min: number;
  max?: number;
  onChange: (n: number) => void;
}) {
  const [text, setText] = useState<string>();
  return (
    <input
      type="number"
      inputMode="numeric"
      min={min}
      max={max}
      value={text ?? value}
      onChange={(e) => {
        setText(e.target.value);
        const n = wholeNumber(e.target.value, min, max);
        if (n !== undefined) onChange(n);
      }}
      onBlur={() => setText(undefined)}
    />
  );
}

export function RemoveStep({ onRemove }: { onRemove: () => void }) {
  return (
    <Button variant="ghost" size="sm" icon={Trash2} onClick={onRemove}>
      Remove this step
    </Button>
  );
}

function TriggerFields({
  t,
  lists,
  onChange,
}: {
  t: Config<"trigger">;
  lists: Lists;
  onChange: (t: AutomationTrigger) => void;
}) {
  const zone = "time_zone" in t ? t.time_zone : here();
  return (
    <>
      <label className="hf-field">
        <span>Starts</span>
        <Select
          label="Starts"
          value={t.kind}
          options={TRIGGERS}
          onChange={(kind) => onChange(triggerOfKind(kind, t, here()))}
        />
      </label>
      {t.kind === "schedule" && (
        <>
          <div className="hf-field">
            <span>Every</span>
            <Segmented
              label="Every"
              value={t.every}
              options={[
                { value: "hour", label: "Hour" },
                { value: "day", label: "Day" },
                { value: "week", label: "Week" },
              ]}
              onChange={(every) =>
                onChange(
                  every === "hour"
                    ? { kind: "schedule", every, minute: 0, time_zone: zone }
                    : every === "day"
                      ? {
                          kind: "schedule",
                          every,
                          at: "at" in t ? t.at : "09:00",
                          time_zone: zone,
                        }
                      : {
                          kind: "schedule",
                          every: "week",
                          weekday: "monday",
                          at: "at" in t ? t.at : "09:00",
                          time_zone: zone,
                        },
                )
              }
            />
          </div>
          {t.every === "hour" && (
            <label className="hf-field">
              <span>Minutes past the hour</span>
              <WholeNumber
                min={0}
                max={59}
                value={t.minute}
                onChange={(minute) => onChange({ ...t, minute })}
              />
            </label>
          )}
          {t.every === "week" && (
            <label className="hf-field">
              <span>On</span>
              <Select
                label="Day of the week"
                value={t.weekday}
                options={DAYS}
                onChange={(weekday) => onChange({ ...t, weekday })}
              />
            </label>
          )}
          {t.every !== "hour" && (
            <label className="hf-field">
              <span>At</span>
              <input
                type="time"
                value={t.at}
                required
                onChange={(e) =>
                  /^\d\d:\d\d$/.test(e.target.value) &&
                  onChange({ ...t, at: e.target.value })
                }
              />
            </label>
          )}
          <label className="hf-field">
            <span>Time zone</span>
            <Select
              label="Time zone"
              value={t.time_zone}
              options={zoneOptions(t.time_zone)}
              onChange={(time_zone) => onChange({ ...t, time_zone })}
            />
          </label>
          {validZone(t.time_zone) && (
            <p className="au-hint">
              Next:{" "}
              {formatDate(new Date(nextFire(t, Date.now())).toISOString())}
              {t.time_zone === here() ? "" : " your time"}.
            </p>
          )}
        </>
      )}
      {t.kind === "sync_finished" && (
        <>
          <label className="hf-field">
            <span>Connection</span>
            <Select
              label="Connection"
              value={t.connector_id}
              placeholder="Choose a connection…"
              options={targets(lists.connectors, t.connector_id)}
              onChange={(connector_id) => onChange({ ...t, connector_id })}
            />
          </label>
          <label className="hf-field">
            <span>When its sync</span>
            <Select
              label="When its sync"
              value={t.outcome}
              options={OUTCOMES}
              onChange={(outcome) => onChange({ ...t, outcome })}
            />
          </label>
        </>
      )}
      {(t.kind === "load_ready" ||
        t.kind === "generation_finished" ||
        t.kind === "reviews_waiting") && (
        <label className="hf-field">
          <span>Ontology</span>
          <Select
            label="Ontology"
            value={t.ontology_id}
            placeholder="Choose an ontology…"
            options={targets(lists.ontologies, t.ontology_id)}
            onChange={(ontology_id) => onChange({ ...t, ontology_id })}
          />
        </label>
      )}
      {t.kind === "generation_finished" && (
        <label className="hf-field">
          <span>When the draft</span>
          <Select
            label="When the draft"
            value={t.outcome}
            options={OUTCOMES}
            onChange={(outcome) => onChange({ ...t, outcome })}
          />
        </label>
      )}
      {t.kind === "reviews_waiting" && (
        <label className="hf-field">
          <span>Cards to review, at least</span>
          <WholeNumber
            min={1}
            max={MAX_REVIEWS_WAITING}
            value={t.at_least}
            onChange={(at_least) => onChange({ ...t, at_least })}
          />
        </label>
      )}
      {t.kind === "document_arrived" && (
        <label className="hf-field">
          <span>Document set</span>
          <Select
            label="Document set"
            value={t.set_id}
            placeholder="Choose a document set…"
            options={targets(lists.sets, t.set_id)}
            onChange={(set_id) => onChange({ ...t, set_id })}
          />
        </label>
      )}
      {t.kind === "manual" && (
        <p className="au-hint">
          It never starts on its own: you run it from its page.
        </p>
      )}
      {t.kind === "reviews_waiting" && (
        <p className="au-hint">
          Runs when at least this many cards wait in the ontology’s current
          load, once its reviews are sorted, including the load that is current
          when you turn it on. It runs again for a new load, or after the count
          drops below this and rises past it again.
        </p>
      )}
      {t.kind !== "schedule" &&
        t.kind !== "manual" &&
        t.kind !== "reviews_waiting" && (
          <p className="au-hint">
            Foundry checks every half minute or so, and starts it a couple of
            minutes after what it waits for ends. Several at once start it once.
          </p>
        )}
    </>
  );
}

/** Every action a Founder step may be allowed, from the workspace's targets. */
function choices(lists: Lists): AutomationAction[] {
  return [
    ...lists.connectors.map(
      (c): AutomationAction => ({
        operation: "sync_connector",
        connector_id: c.id,
      }),
    ),
    ...lists.ontologies.map(
      (o): AutomationAction => ({
        operation: "create_load",
        ontology_id: o.id,
      }),
    ),
    ...lists.dashboards
      .filter((d) => d.published)
      .map(
        (d): AutomationAction => ({
          operation: "publish_dashboard",
          dashboard_id: d.id,
        }),
      ),
  ];
}

function AssistantFields({
  c,
  lists,
  onChange,
}: {
  c: Config<"assistant">;
  lists: Lists;
  onChange: (c: Config<"assistant">) => void;
}) {
  const allow = c.allow || [];
  const offered = [
    ...choices(lists),
    ...allow.filter((a) => !choices(lists).some((x) => sameAction(x, a))),
  ];
  return (
    <>
      <label className="hf-field">
        <span>What should it do?</span>
        <textarea
          rows={6}
          maxLength={LIMITS.instruction}
          placeholder="e.g. Find the invoices more than 30 days overdue and summarise them by customer."
          value={c.instruction}
          onChange={(e) => onChange({ ...c, instruction: e.target.value })}
        />
        <small>
          {[...c.instruction].length.toLocaleString()} of{" "}
          {LIMITS.instruction.toLocaleString()} characters
        </small>
      </label>
      <p className="au-hint">
        Write it as you would ask a colleague. Ask a yes-or-no question to
        follow it with an “If” step, or ask how many to compare a count.
      </p>
      <fieldset className="au-allow">
        <legend>It may do these without asking you</legend>
        {!offered.length && (
          <p className="au-hint">
            This workspace has nothing it could sync, load or refresh yet.
          </p>
        )}
        {offered.map((a) => {
          const on = allow.some((x) => sameAction(x, a));
          const text = actionText(a, lists.names);
          return (
            <label
              key={a.operation + ":" + JSON.stringify(a)}
              className="au-check"
            >
              <input
                type="checkbox"
                checked={on}
                disabled={!on && allow.length >= LIMITS.allow}
                onChange={() => {
                  const next = on
                    ? allow.filter((x) => !sameAction(x, a))
                    : [...allow, a];
                  onChange({
                    instruction: c.instruction,
                    ...(next.length && { allow: next }),
                  });
                }}
              />
              <span>{text.charAt(0).toUpperCase() + text.slice(1)}</span>
            </label>
          );
        })}
        <p className="au-hint">
          Anything else it prepares waits for you as a card on the run’s page,
          to run or dismiss.
        </p>
      </fieldset>
    </>
  );
}

const OPERATIONS: SelectOption<AutomationAction["operation"]>[] = [
  { value: "sync_connector", label: "Sync a connection" },
  { value: "create_load", label: "Load an ontology’s current version" },
  {
    value: "publish_dashboard",
    label: "Refresh a dashboard’s public snapshot",
  },
];

function ActionFields({
  a,
  lists,
  label,
  onChange,
}: {
  a: Config<"action">;
  lists: Lists;
  label: string;
  onChange: (a: AutomationAction) => void;
}) {
  const published = lists.dashboards.filter((d) => d.published);
  return (
    <>
      <label className="hf-field">
        <span>{label}</span>
        <Select
          label={label}
          value={a.operation}
          options={OPERATIONS.filter(
            (o) =>
              o.value !== "publish_dashboard" ||
              published.length ||
              a.operation === "publish_dashboard",
          )}
          onChange={(operation) =>
            onChange(
              operation === "sync_connector"
                ? { operation, connector_id: "" }
                : operation === "create_load"
                  ? { operation, ontology_id: "" }
                  : { operation, dashboard_id: "" },
            )
          }
        />
      </label>
      {a.operation === "sync_connector" && (
        <label className="hf-field">
          <span>Connection</span>
          <Select
            label="Connection"
            value={a.connector_id}
            placeholder="Choose a connection…"
            options={targets(lists.connectors, a.connector_id)}
            onChange={(connector_id) => onChange({ ...a, connector_id })}
          />
        </label>
      )}
      {a.operation === "create_load" && (
        <>
          <label className="hf-field">
            <span>Ontology</span>
            <Select
              label="Ontology"
              value={a.ontology_id}
              placeholder="Choose an ontology…"
              options={targets(lists.ontologies, a.ontology_id)}
              onChange={(ontology_id) => onChange({ ...a, ontology_id })}
            />
          </label>
          <p className="au-hint">
            It loads whichever version is current when it runs, within the
            workspace’s load budget.
          </p>
        </>
      )}
      {a.operation === "publish_dashboard" && (
        <label className="hf-field">
          <span>Dashboard</span>
          <Select
            label="Dashboard"
            value={a.dashboard_id}
            placeholder="Choose a published dashboard…"
            options={targets(published, a.dashboard_id)}
            onChange={(dashboard_id) => onChange({ ...a, dashboard_id })}
          />
        </label>
      )}
      <p className="au-hint">It runs without asking you, as you.</p>
    </>
  );
}

function ConditionFields({
  c,
  spec,
  id,
  lists,
  email,
  onChange,
  onSwap,
}: {
  c: Config<"condition">;
  spec: AutomationSpec;
  id: string;
  lists: Lists;
  email: boolean;
  onChange: (c: Config<"condition">) => void;
  onSwap: () => void;
}) {
  const leads = (branch: "true" | "false") => {
    const to = edgesOutOf(spec, id).find((e) => e.branch === branch);
    const n = to && spec.nodes.find((x) => x.id === to.to);
    return n ? stepTitle(n, lists.names, email, "brief") : "Stop";
  };
  return (
    <>
      <div className="hf-field">
        <span>It checks</span>
        <Segmented
          label="It checks"
          value={c.check}
          options={[
            { value: "answer", label: "Founder’s answer" },
            { value: "count", label: "A count" },
          ]}
          onChange={(check) =>
            onChange(
              check === "answer"
                ? { check: "answer", equals: "yes" }
                : { check: "count", op: "at_least", value: 1 },
            )
          }
        />
      </div>
      {c.check === "answer" ? (
        <label className="hf-field">
          <span>Goes on when the answer is</span>
          <Select
            label="Goes on when the answer is"
            value={c.equals}
            options={[
              { value: "yes", label: "Yes" },
              { value: "no", label: "No" },
            ]}
            onChange={(equals) => onChange({ ...c, equals })}
          />
        </label>
      ) : (
        <div className="au-row">
          <label className="hf-field">
            <span>Goes on when the count is</span>
            <Select
              label="Goes on when the count is"
              value={c.op}
              options={[
                { value: "at_least", label: "at least" },
                { value: "at_most", label: "at most" },
                { value: "equals", label: "exactly" },
              ]}
              onChange={(op) => onChange({ ...c, op })}
            />
          </label>
          <label className="hf-field">
            <span>Number</span>
            <WholeNumber
              min={0}
              value={c.value}
              onChange={(value) => onChange({ ...c, value })}
            />
          </label>
        </div>
      )}
      <p className="au-hint">
        {c.check === "answer"
          ? "It reads the yes or no of the Founder step before it."
          : "It reads the count of the step before it: Founder’s, the documents that arrived, the rows a sync wrote, or the cards waiting for review."}
      </p>
      <dl className="au-branches">
        <dt>When met</dt>
        <dd>{leads("true")}</dd>
        <dt>Otherwise</dt>
        <dd>{leads("false")}</dd>
      </dl>
      <Button size="sm" icon={ArrowDownUp} onClick={onSwap}>
        Swap the two ways
      </Button>
    </>
  );
}
