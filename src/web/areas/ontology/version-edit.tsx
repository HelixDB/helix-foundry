import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DatabaseZap, UserSearch } from "lucide-react";
import * as toml from "smol-toml";
import type * as wfe from "../../../shared/wfe";
import { Button, ButtonLink, Select, Tag } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { enrichmentQuery } from "../workspace/enrichment-api";
import {
  CREDITS_PER_PROFILE,
  PROFILE_DEFAULTS,
  WORK_HISTORY_DEFAULT,
  type ProfileField,
} from "./enrichment-fields";
import { cluesText, type AgentBundle } from "./model";
import { canManage } from "./parts";
import {
  CardFields,
  EnrichmentNotes,
  EvidenceFields,
  EvidenceWithheld,
} from "./version-parts";
import {
  HAND_EDITED,
  batchEnrichmentRefusal,
  canCreateRevision,
  clueOptions,
  createdProps,
  employerHistory,
  employerOptions,
  enrichedType,
  enrichmentDefaults,
  enrichmentForm,
  fieldOptions,
  handIdentifiers,
  historyLinks,
  readRevisionResult,
  editItem,
  editItems,
  identifierEdit,
  routeSetRefusal,
  stageEdit,
  stageEnrichment,
  undoEdit,
  validateEdit,
  type EditKind,
  type EnrichmentForm,
} from "./edit-model";
import {
  isLive,
  ontologyQueries,
  reviseOntologyVersion,
  setCurrentVersion,
} from "./queries";
import { useOntology } from "./scope";
import { documentSetQueries } from "../documents/documents-api";

/**
 * A revision this page started: its generation, and the batch it sent.
 * `save`: Save as version, whose clean preview creates the version itself.
 */
export type RevisionRun = { id: string; snapshot: string; save?: boolean };

/**
 * The identifiers a type had, as the version has them or as staged by hand,
 * before the Enrich people form staged the try now pending for it: taking
 * that try back restores them (`handIdentifiers`), which the edits alone
 * can't tell from those the try declared.
 */
export type TryBase = { type: string; identifiers: Record<string, string> };

/** Stored evidence is plain text, never model-provided markup or links. */
export function RevisionEvidence({
  generation: g,
}: {
  generation: wfe.OntologyGeneration;
}) {
  const result = readRevisionResult(g.revision?.resultJson),
    withheld = !!g.revision?.resultWithheld;
  // Revise fails a refusal, so a withheld result that succeeded revised.
  const revised = withheld
    ? g.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED"
    : result?.status === "revised";
  return (
    <section className="on-stack" aria-label="Revision result">
      <p role="status">
        {isLive(g)
          ? "Checking edits…"
          : result?.status === "refused"
            ? "Edits refused."
            : g.status === "ONTOLOGY_GENERATION_STATUS_FAILED"
              ? "Revision failed."
              : revised
                ? g.revision?.dryRun
                  ? "Preview ready. No version created."
                  : "Version created. Load it to apply these changes."
                : "Revision result unavailable."}
      </p>
      {withheld && <EvidenceWithheld />}
      {result?.rules && (
        <p>
          {result.rules.evaluated} of {result.rules.count} rules evaluated.
        </p>
      )}
      {g.error && (
        <p role="alert" className="hf-form-error">
          {g.error}
        </p>
      )}
      {result?.findings.some((f) => f.code === "budget") && (
        <p>
          Measuring these edits on the current load needed more than its budget,
          so none was accepted. Preview fewer rule and constraint changes at a
          time.
        </p>
      )}
      {result?.findings.map((finding, i) => (
        <div className="on-item" key={i}>
          <strong>
            Finding {i + 1}: {finding.code}
          </strong>
          <EvidenceFields value={finding} />
        </div>
      ))}
      {result?.cards.map((card, i) => (
        <div className="on-item" key={i}>
          <strong>
            {card.item} · {card.verdict}
          </strong>
          <CardFields card={card.card} />
        </div>
      ))}
    </section>
  );
}

/**
 * Per-item editing only. A preview authorizes exactly its immutable edit
 * batch. The staged edits, the run and the staged try's base are the
 * version page's, so they outlive this tab: a create keeps showing after a
 * look at another tab, and its edits are cleared once it has made a
 * version.
 */
export function AgentEdit({
  bundle,
  version,
  edits,
  setEdits,
  run,
  setRun,
  tryBase,
  setTryBase,
}: {
  bundle: AgentBundle;
  version: wfe.OntologyVersion;
  edits: wfe.OntologyEdit[];
  setEdits: Dispatch<SetStateAction<wfe.OntologyEdit[]>>;
  run: RevisionRun | null;
  setRun: Dispatch<SetStateAction<RevisionRun | null>>;
  tryBase: TryBase | null;
  setTryBase: Dispatch<SetStateAction<TryBase | null>>;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  const qc = useQueryClient();
  // The Types tab's Edit enrichment opens a type: ?item=type.Person.
  const [params] = useSearchParams();
  const asked = params.get("item") || "";
  const typeItem = (item: string) =>
    bundle.ontology.ok &&
    bundle.ontology.value.types.some((t) => `type.${t.name}` === item)
      ? item
      : "";
  const [selected, setSelected] = useState(() => typeItem(asked));
  useEffect(() => {
    if (typeItem(asked)) setSelected(asked);
  }, [asked]);
  const [dirty, setDirty] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const query = useQuery({
    ...ontologyQueries.generation(ws, o, run?.id || ""),
    enabled: !!run,
  });
  const sets = useQuery(documentSetQueries.list(ws));
  const versions = useQuery(ontologyQueries.versions(ws, o));
  const settings = useQuery(enrichmentQuery(ws));
  const nextVersion =
    Math.max(0, ...(versions.data?.versions || []).map((v) => v.version || 0)) +
    1;
  const g = query.data?.generation;
  useEffect(() => {
    if (g && !isLive(g)) {
      void qc.invalidateQueries({
        queryKey: ontologyQueries.versions(ws, o).queryKey,
      });
      void qc.invalidateQueries({
        queryKey: ontologyQueries.generations(ws, o).queryKey,
      });
    }
    // A created version holds the batch now: staging it again would make
    // an identical one. Save as version's form closes with it.
    if (g?.resultVersionId && !g.revision?.dryRun) {
      setEdits([]);
      setTryBase(null);
      if (run?.save) setSelected("");
    }
    // Save as version: a clean preview of the batch creates it.
    if (run?.save && canCreateRevision(g, run.snapshot, edits))
      void start(false, edits, true);
  }, [g?.status, g?.id]);
  const busy = pending || isLive(g);
  if (!canManage(role)) return <p>Only owners and admins can edit versions.</p>;
  if (bundle.status !== "done")
    return <p>Finish this draft with the agent before editing it.</p>;
  if (
    !bundle.ontology.ok ||
    !bundle.mappings.ok ||
    !bundle.routes.ok ||
    !bundle.ontologyToml.trim()
  )
    return <p>Repair this draft with the agent before editing it.</p>;
  const items = editItems(bundle);
  const ready =
    !busy &&
    !dirty &&
    !query.error &&
    canCreateRevision(g, run?.snapshot || "", edits);
  async function start(
    dryRun: boolean,
    batch: wfe.OntologyEdit[] = edits,
    save = false,
  ) {
    if (
      pending ||
      (isLive(g) && !save) ||
      dirty ||
      !batch.length ||
      (!dryRun && !canCreateRevision(g, run?.snapshot || "", batch))
    )
      return;
    setPending(true);
    setError("");
    // Save as version keeps its preview showing until the create answers.
    if (!save) setRun(null);
    try {
      const response = await reviseOntologyVersion(ws, o, version.id, {
        edits: batch,
        dryRun,
      });
      if (!response.generation)
        throw new Error("No revision generation was returned. Preview again.");
      qc.setQueryData(
        ontologyQueries.generation(ws, o, response.generation.id).queryKey,
        response,
      );
      setRun({
        id: response.generation.id,
        snapshot: JSON.stringify(batch),
        save,
      });
      void qc.invalidateQueries({
        queryKey: ontologyQueries.generations(ws, o).queryKey,
      });
      void qc.invalidateQueries({
        queryKey: ontologyQueries.versions(ws, o).queryKey,
      });
    } catch (e) {
      setError((e as Error).message);
      if (save) setRun(null);
    } finally {
      setPending(false);
    }
  }
  const before = (type: string) =>
    tryBase?.type === type ? tryBase.identifiers : undefined;
  // The Enrich people form's batch: staged, then previewed and, if clean,
  // created. The form stays open until then, so a refusal can be fixed in
  // it. The type's identifiers without the try go with it, to take it back
  // to.
  const saveAsVersion = (type: string, batch: wfe.OntologyEdit[]) => {
    setTryBase({
      type,
      identifiers: handIdentifiers(bundle, edits, type, before(type)),
    });
    setEdits(batch);
    setRun(null);
    void start(true, batch, true);
  };
  // The pending edits each editor starts from: a change to them (a save
  // from the other editor, an undo) starts it afresh.
  const startsFrom = (ids: (id: string) => boolean) =>
    JSON.stringify(edits.filter((e) => ids(editItem(e))));
  return (
    <div className="on-stack">
      <h2>Edit version</h2>
      <p>
        Change document routes, identifiers, People enrichment, rules and
        constraints. Preview the edits, then create a new version. Ask the agent
        to change table bindings or the schema.
      </p>
      <fieldset disabled={busy} className="on-stack">
        <label className="hf-field">
          <span>Item to edit</span>
          <Select
            label="Item to edit"
            value={selected}
            onChange={(id) => {
              setSelected(id);
              setDirty(false);
            }}
            options={[
              { value: "", label: "Choose an item…" },
              ...Object.keys(items)
                .filter((id) =>
                  HAND_EDITED.includes(id.split(".")[0] as EditKind),
                )
                .map((id) => ({ value: id, label: id })),
              { value: "new:extraction", label: "Add document route" },
              { value: "new:rule", label: "Add rule" },
              { value: "new:constraint", label: "Add constraint" },
            ]}
          />
        </label>
        {selected && (
          <ItemEditor
            key={`${selected}:${startsFrom((id) => id === selected)}`}
            bundle={bundle}
            selected={selected}
            items={items}
            edits={edits}
            sets={
              sets.data && new Map(sets.data.map((x) => [x.id, x.name || ""]))
            }
            onDirty={() => {
              setDirty(true);
              setRun(null);
            }}
            onSave={(edit) => {
              setEdits((previous) => stageEdit(bundle, previous, edit));
              setDirty(false);
              setRun(null);
              setError("");
              // A type's card stays open: its Enrich people form would
              // lose what it hasn't saved.
              if (!selected.startsWith("type.")) setSelected("");
            }}
          />
        )}
        {selected.startsWith("type.") && (
          <EnrichPeople
            // Its own key: a sibling sharing ItemEditor's would leave a
            // stale editor mounted when the selection changes. The type's
            // put isn't in it, so staging its identifiers keeps the form's
            // choices.
            key={`enrich:${selected}:${startsFrom(
              (id) => id.startsWith("link.") || id.startsWith("enrichment."),
            )}`}
            bundle={bundle}
            versionId={version.id}
            type={selected.slice("type.".length)}
            edits={edits}
            before={before(selected.slice("type.".length))}
            settings={settings.data}
            nextVersion={nextVersion}
            blocked={dirty}
            onSave={(batch) =>
              saveAsVersion(selected.slice("type.".length), batch)
            }
            onUnstage={(batch) => {
              setEdits(batch);
              setTryBase(null);
              setRun(null);
              setError("");
            }}
          />
        )}
        {edits.length > 0 && (
          <section className="on-stack" aria-label="Pending edits">
            <h3>Pending edits</h3>
            {edits.map((e) => {
              const itemId = editItem(e);
              return (
                <div key={itemId} className="on-item">
                  <code>{itemId}</code> · {e.put ? "Put" : "Remove"}{" "}
                  <Button
                    size="sm"
                    onClick={() => {
                      setEdits((previous) =>
                        undoEdit(
                          bundle,
                          previous,
                          itemId,
                          tryBase?.identifiers,
                        ),
                      );
                      // Undoing the try's item takes it back, and undoing its
                      // type drops what was staged by hand.
                      if (
                        itemId.startsWith("enrichment.") ||
                        itemId === `type.${tryBase?.type}`
                      )
                        setTryBase(null);
                      setRun(null);
                    }}
                  >
                    Undo {itemId}
                  </Button>
                </div>
              );
            })}
          </section>
        )}
      </fieldset>
      {dirty && <p>Stage this item before previewing.</p>}
      {(error || query.error) && (
        <p role="alert" className="hf-form-error">
          {error || query.error?.message}
        </p>
      )}
      <div className="hf-form-actions">
        <Button
          disabled={!edits.length || busy || dirty}
          pending={pending}
          onClick={() => void start(true)}
        >
          Preview edits
        </Button>
        <Button disabled={!ready} onClick={() => void start(false)}>
          Create version
        </Button>
      </div>
      {run?.save && g ? (
        <SaveStatus generation={g} pending={pending} />
      ) : (
        run &&
        g && (
          <>
            <RevisionEvidence generation={g} />
            <ButtonLink to={paths.generation(o, g.id)} size="sm">
              View revision
            </ButtonLink>
            {g.resultVersionId && (
              <ButtonLink to={paths.version(o, g.resultVersionId)}>
                View new version
              </ButtonLink>
            )}
          </>
        )
      )}
    </div>
  );
}

/**
 * Save as version's progress: checking, then the version it saved with
 * Make current and load, or why it couldn't.
 */
function SaveStatus({
  generation: g,
  pending,
}: {
  generation: wfe.OntologyGeneration;
  pending: boolean;
}) {
  const { id: o } = useOntology();
  const result = readRevisionResult(g.revision?.resultJson);
  // Revise fails a refusal, so a withheld result that succeeded revised.
  if (
    pending ||
    isLive(g) ||
    (g.revision?.dryRun &&
      g.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" &&
      (g.revision.resultWithheld || result?.status === "revised"))
  )
    return <p role="status">Checking edits…</p>;
  if (g.resultVersionId) return <SavedVersion versionId={g.resultVersionId} />;
  // The engine's words, without its code: "current_title is already filled
  // by contacts.title."
  const finding =
    result?.findings.find(
      (f) => f.code !== "no_comparable_load" && f.code !== "Lint",
    ) ?? result?.findings[0];
  return (
    <div className="on-stack">
      <p role="alert" className="hf-form-error">
        Couldn’t save:{" "}
        {(finding?.message || g.error || "the revision failed.").replace(
          /^[A-Za-z]+: /,
          "",
        )}
      </p>
      <ButtonLink to={paths.generation(o, g.id)} size="sm">
        View revision
      </ButtonLink>
    </div>
  );
}

/** "Version 5 saved.", with Make current and load: set-current, then the load dialog. */
function SavedVersion({ versionId }: { versionId: string }) {
  const { id: ws, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    qc = useQueryClient(),
    navigate = useNavigate();
  const version = useQuery(ontologyQueries.version(ws, o, versionId));
  const [pending, setPending] = useState(false);
  const n = version.data?.version?.version;
  const makeCurrent = async () => {
    setPending(true);
    try {
      await setCurrentVersion(ws, o, {
        versionId,
        expectedCurrentVersionId: ontology.currentVersionId || "",
      });
      await qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
      // The new version's page opens its load dialog.
      navigate(paths.version(o, versionId), { state: { openLoad: true } });
    } catch (e) {
      const message = (e as Error).message;
      notify(message.charAt(0).toUpperCase() + message.slice(1), true);
      await qc.invalidateQueries({ queryKey: [ws, "ontology", o] });
    } finally {
      setPending(false);
    }
  };
  return (
    <div className="on-stack">
      <p role="status">Version {n ?? "?"} saved.</p>
      <div className="hf-form-actions">
        <Button
          variant="primary"
          icon={DatabaseZap}
          pending={pending}
          onClick={() => void makeCurrent()}
        >
          Make current and load
        </Button>
        <ButtonLink to={paths.version(o, versionId)}>
          View version {n ?? ""}
        </ButtonLink>
      </div>
    </div>
  );
}

/** "a, b and c". */
const listText = (items: string[]) =>
  items.length > 1
    ? `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`
    : items.join("");

/**
 * The Enrich people form on a type's card: the
 * props a person's email and LinkedIn URL come from, the profile fields
 * looked up into props (new or not), and the employers to link. It starts
 * from the pending edits, so a save the WFE refused keeps its choices.
 * Saving stages one merged type put, a new link and the item, and saves
 * them as a new version; switching it off removes the item only, and says
 * what stays, empty: the props it fills and its work history's link. Left as
 * the version has it, the form unstages what an earlier try staged (a save
 * the WFE refused), back to the type's identifiers before it (`before`), so
 * the pending edits never enrich what it reads as off; while the type's own
 * identifiers are unsaved (which keeps Preview off), once they are staged.
 * A file enriches one type: while another type is, the switch is off.
 * Hidden while Helix has lookups off.
 */
export function EnrichPeople({
  bundle,
  versionId,
  type,
  edits,
  before,
  settings,
  nextVersion,
  blocked,
  onSave,
  onUnstage,
}: {
  bundle: AgentBundle;
  versionId: string;
  type: string;
  edits: wfe.OntologyEdit[];
  // The type's identifiers before the pending try, if one is.
  before?: Readonly<Record<string, string>>;
  settings?: wfe.FoundryEnrichment;
  nextVersion: number;
  // Something else is unsaved: saving would lose it.
  blocked: boolean;
  onSave: (batch: wfe.OntologyEdit[]) => void;
  // The pending edits with what an earlier try staged for the type dropped.
  onUnstage: (batch: wfe.OntologyEdit[]) => void;
}) {
  // The version's item, and the item as the pending edits leave it.
  const saved = useMemo(() => enrichmentForm(bundle, type), [bundle, type]),
    staged = useMemo(
      () => enrichmentForm(bundle, type, edits),
      [bundle, type, edits],
    );
  const [form, setForm] = useState<EnrichmentForm>(staged);
  const [error, setError] = useState("");
  // As the version has it, nothing is left to save: an earlier try's edits
  // go, unless that would reset the type's unsaved identifiers; then once
  // they are staged.
  const held = useRef(false);
  const unstage = (next: EnrichmentForm) => {
    if (stageEnrichment(bundle, [], type, next).length) return;
    if (blocked) {
      held.current = true;
      return;
    }
    const unstaged = stageEnrichment(bundle, edits, type, next, before);
    if (JSON.stringify(unstaged) !== JSON.stringify(edits)) onUnstage(unstaged);
  };
  useEffect(() => {
    if (blocked || !held.current) return;
    held.current = false;
    unstage(form);
  }, [blocked]);
  const t = bundle.ontology.ok
    ? bundle.ontology.value.types.find((x) => x.name === type)
    : undefined;
  const existing = bundle.enrichments.ok
    ? bundle.enrichments.value.find((e) => e.type === type)
    : undefined;
  // What turning it off leaves, empty: the props it fills and its link.
  const kept = existing
    ? [
        ...existing.props.map(([p]) => p),
        ...(existing.workHistory ? [existing.workHistory.link] : []),
      ]
    : [];
  if (
    !t ||
    settings?.availability === "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED"
  )
    return null;
  const eligible = t.key.length > 0 && t.names.length > 0,
    other = enrichedType(bundle, edits),
    taken = other !== undefined && other !== type;
  const clues = clueOptions(bundle, type),
    created = createdProps(bundle, type),
    employers = employerOptions(bundle, type);
  const change = (next: EnrichmentForm) => {
    setForm(next);
    setError("");
    held.current = false;
    unstage(next);
  };
  // Saving would change the version.
  const changed = stageEnrichment(bundle, [], type, form).length > 0;
  const save = () => {
    if (form.on) {
      if (!form.clues.email && !form.clues.linkedinUrl)
        return setError("pick the field an email or LinkedIn URL comes from.");
      const props = form.fields.map((f) => f.prop);
      if (new Set(props).size !== props.length)
        return setError("each profile field needs a field of its own.");
    }
    const batch = stageEnrichment(bundle, edits, type, form, before);
    const problem = batch
      .map((e) => validateEdit(bundle, e, batch))
      .find(Boolean);
    if (problem) setError(problem.replace(/^[A-Za-z]+: /, ""));
    else onSave(batch);
  };
  const clueHint = (prop: string | undefined, other: string | undefined) =>
    !prop
      ? undefined
      : prop === other
        ? "Pick different fields."
        : !created.has(prop)
          ? `${prop} isn’t filled by any table.`
          : undefined;
  const clueSelect = (
    label: string,
    which: "email" | "linkedinUrl",
    hint?: string,
  ) => {
    if (!form.on) return null;
    const value = form.clues[which],
      other = form.clues[which === "email" ? "linkedinUrl" : "email"];
    return (
      <label className="hf-field">
        <span>{label}</span>
        <Select
          label={label}
          value={value || ""}
          onChange={(v) =>
            change({
              ...form,
              clues: { ...form.clues, [which]: v || undefined },
            })
          }
          options={[
            { value: "", label: "None" },
            ...clues.map((p) => ({ value: p, label: p })),
          ]}
        />
        {(clueHint(value, other) || hint) && (
          <small>{clueHint(value, other) || hint}</small>
        )}
      </label>
    );
  };
  // The default fields, each into the prop it would fill on its own, then
  // any other the item maps.
  const defaults = enrichmentDefaults(bundle, type).fields;
  const rows = [
    ...PROFILE_DEFAULTS.map((d) => ({
      ...d,
      prop: defaults.find((f) => f.field === d.field)?.prop ?? d.prop,
    })),
    ...(form.on ? form.fields : [])
      .filter((f) => !PROFILE_DEFAULTS.some((d) => d.field === f.field))
      .map((f) => ({ field: f.field, label: f.field, prop: f.prop })),
  ];
  const history = form.on ? form.workHistory : undefined,
    employer = employers.find((x) => x.type === history?.to),
    links = history ? historyLinks(bundle, type, history.to) : [];
  const newLink = !bundle.ontology.ok
    ? false
    : !bundle.ontology.value.links.some(
        (l) => l.name === WORK_HISTORY_DEFAULT.link,
      );
  return (
    <section className="on-item" aria-label={`People enrichment of ${type}`}>
      <header>
        <h3>
          {[
            type,
            t.key.length ? `key ${t.key.join(", ")}` : "no key",
            t.names.length ? `names ${t.names.join(", ")}` : "no names",
          ].join(" · ")}
        </h3>
        {existing && (
          <Tag tone="info" icon={UserSearch}>
            Enriched · {cluesText(existing)}
          </Tag>
        )}
        <label className="checkbox">
          <input
            type="checkbox"
            role="switch"
            checked={form.on}
            disabled={!eligible || taken}
            onChange={(e) =>
              change(
                !e.target.checked
                  ? { on: false }
                  : staged.on
                    ? staged
                    : saved.on
                      ? saved
                      : enrichmentDefaults(bundle, type),
              )
            }
          />
          Enrich people
        </label>
      </header>
      {!eligible && <p className="on-muted">Needs a key and a name field.</p>}
      {eligible && taken && (
        <p className="on-muted">
          Only one type can be enriched, and {other} is. Turn it off there
          first.
        </p>
      )}
      {existing && <EnrichmentNotes bundle={bundle} versionId={versionId} />}
      {settings && !settings.enabled && form.on && !existing && (
        <p className="on-muted">
          People enrichment is off for this workspace, so loads apply no
          profiles. An owner can allow it in{" "}
          <Link to={paths.settings("enrichment")} className="on-link">
            Settings
          </Link>
          .
        </p>
      )}
      {form.on ? (
        <>
          <p>
            Look up public work profiles of each {type} on context.dev.{" "}
            {CREDITS_PER_PROFILE} credits per profile found.
          </p>
          <fieldset className="plain-fieldset">
            <legend>Input fields</legend>
            {clueSelect(
              "Email comes from",
              "email",
              "Becomes an email identifier: documents can also find people by it.",
            )}
            {clueSelect("LinkedIn URL comes from", "linkedinUrl")}
          </fieldset>
          <fieldset className="plain-fieldset">
            <legend>Profile fields</legend>
            {rows.map((row) => {
              const chosen = form.fields.find((f) => f.field === row.field),
                prop = chosen?.prop || row.prop;
              const field = { field: row.field as ProfileField, prop };
              const set = (on: boolean, to = prop) =>
                change({
                  ...form,
                  fields: !on
                    ? form.fields.filter((f) => f.field !== row.field)
                    : chosen
                      ? form.fields.map((f) =>
                          f === chosen ? { ...f, prop: to } : f,
                        )
                      : [...form.fields, { ...field, prop: to }],
                });
              return (
                <div key={row.field} className="hf-form-actions">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={!!chosen}
                      onChange={(e) => set(e.target.checked)}
                    />
                    {row.label}
                  </label>
                  <Select
                    label={`${row.label} fills`}
                    value={prop}
                    disabled={!chosen}
                    onChange={(to) => set(true, to)}
                    options={fieldOptions(
                      bundle,
                      type,
                      form.clues,
                      row.prop,
                      prop,
                    )}
                  />
                </div>
              );
            })}
          </fieldset>
          <fieldset className="plain-fieldset">
            <legend>Work history</legend>
            {employers.length ? (
              <>
                <div className="hf-form-actions">
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={!!history}
                      onChange={(e) =>
                        change({
                          ...form,
                          // Ticked again: as staged, else as the version
                          // has it, else the defaults' employer.
                          workHistory: e.target.checked
                            ? (staged.on && staged.workHistory) ||
                              (saved.on && saved.workHistory) ||
                              employerHistory(bundle, type)
                            : undefined,
                        })
                      }
                    />
                    Link each employer
                  </label>
                  {history && (
                    <>
                      <span>with</span>
                      <Select
                        label="Link"
                        value={history.link ?? ""}
                        onChange={(link) =>
                          change({
                            ...form,
                            workHistory: { ...history, link },
                          })
                        }
                        options={[
                          ...(newLink ? [WORK_HISTORY_DEFAULT.link] : []),
                          ...links,
                        ].map((l) => ({
                          value: l,
                          label: links.includes(l) ? l : `${l} (new)`,
                        }))}
                      />
                      <span>to</span>
                      <Select
                        label="Employer type"
                        value={history.to}
                        onChange={(to) =>
                          change({
                            ...form,
                            workHistory: employerHistory(bundle, type, to),
                          })
                        }
                        options={employers.map((x) => ({
                          value: x.type,
                          label: x.type,
                        }))}
                      />
                      <span>matched by</span>
                      <Select
                        label="Matched by"
                        value={history.match}
                        onChange={(match) =>
                          change({
                            ...form,
                            workHistory: { ...history, match },
                          })
                        }
                        options={(employer?.match || []).map((m) => ({
                          value: m,
                          label: m,
                        }))}
                      />
                    </>
                  )}
                </div>
                {history && !history.link && (
                  <p className="on-muted">
                    No link from {type} to {history.to} can hold work history,
                    and {WORK_HISTORY_DEFAULT.link} is already taken.
                  </p>
                )}
                <p className="on-muted">
                  Only companies already in the graph are linked.
                </p>
              </>
            ) : (
              <p className="on-muted">
                No other type is known by a web domain to link employers to.
              </p>
            )}
          </fieldset>
        </>
      ) : (
        existing && (
          <p>
            Lookups stop from the next load.
            {kept.length > 0 &&
              ` ${listText(kept)} ${kept.length === 1 ? "stays" : "stay"}, empty.`}
          </p>
        )
      )}
      {error && (
        <p role="alert" className="hf-form-error">
          Couldn’t save: {error}
        </p>
      )}
      {changed && (
        <div className="hf-form-actions">
          <Button variant="primary" disabled={blocked} onClick={save}>
            Save as version {nextVersion}
          </Button>
        </div>
      )}
    </section>
  );
}

function ItemEditor({
  bundle,
  selected,
  items,
  edits,
  sets,
  onDirty,
  onSave,
}: {
  bundle: AgentBundle;
  selected: string;
  items: Record<string, string>;
  edits: wfe.OntologyEdit[];
  // The workspace’s document sets, id to name; undefined until read.
  sets: ReadonlyMap<string, string> | undefined;
  onDirty: () => void;
  onSave: (edit: wfe.OntologyEdit) => void;
}) {
  const isNew = selected.startsWith("new:"),
    kind = selected.split(isNew ? ":" : ".")[isNew ? 1 : 0] as EditKind;
  const [name, setName] = useState(
    isNew ? "" : selected.slice(selected.indexOf(".") + 1),
  );
  const previous = edits.find((e) => editItem(e) === selected)?.put;
  const [snippet, setSnippet] = useState(
    previous ? previous.toml : items[selected] || "",
  );
  const [identifiers, setIdentifiers] = useState<Record<string, string>>(() => {
    if (kind !== "type") return {};
    const doc = toml.parse(previous ? previous.toml : items[selected]!);
    return (doc.types as Record<string, any>)[name].identifiers || {};
  });
  const [error, setError] = useState("");
  const itemId = `${kind}.${name}`;
  // A type's put keeps the props pending People enrichment adds.
  const put: wfe.OntologyEdit =
    kind === "type"
      ? identifierEdit(bundle, name, identifiers, edits)
      : { put: { itemId, toml: snippet } };
  const save = (edit: wfe.OntologyEdit) => {
    if (isNew && Object.hasOwn(items, itemId)) {
      setError(
        "Choose a new item name, or select the existing item to edit it.",
      );
      return;
    }
    const problem =
      validateEdit(bundle, edit, edits) ??
      batchEnrichmentRefusal(bundle, stageEdit(bundle, edits, edit))?.message ??
      (kind !== "extraction" || !edit.put
        ? undefined
        : sets
          ? routeSetRefusal(edit, sets)
          : "The document sets are still loading. Stage the route again in a moment.");
    if (problem) setError(problem);
    else onSave(edit);
  };
  const type = bundle.ontology.ok
    ? bundle.ontology.value.types.find((t) => t.name === name)
    : undefined;
  return (
    <div className="on-stack">
      {isNew && (
        <label className="hf-field">
          <span>Item name</span>
          <input
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              onDirty();
            }}
          />
        </label>
      )}
      {kind === "type" ? (
        <>
          <p>
            Only identifiers change. Key properties match text only when listed
            here.
          </p>
          {type?.props.map((p) => (
            <label className="hf-field" key={p.name}>
              <span>Identifier for {p.name}</span>
              <Select
                label={`Identifier for ${p.name}`}
                value={identifiers[p.name] || ""}
                onChange={(k) => {
                  const next = { ...identifiers };
                  if (k) next[p.name] = k;
                  else delete next[p.name];
                  setIdentifiers(next);
                  onDirty();
                }}
                options={[
                  { value: "", label: "Not an identifier" },
                  ...(p.type === "integer"
                    ? ["number"]
                    : p.type === "string"
                      ? ["email", "code", "exact"]
                      : []
                  ).map((k) => ({ value: k, label: k })),
                ]}
              />
            </label>
          ))}
        </>
      ) : (
        <>
          <p>
            {kind === "extraction"
              ? 'Use one [extractions.name] item with documents = "all" or set ids, scoped types, properties and links.'
              : "Use one [rules.name] or [constraints.name] item with description and datalog. Constraints also need severity (block, warn or info). today, including today - 30d, is allowed only in rules."}
          </p>
          {kind === "extraction" && sets && (
            <p className="on-muted">
              Set ids:{" "}
              {[...sets].map(([id, name]) => `${name} = ${id}`).join(", ")}
            </p>
          )}
          <label className="hf-field">
            <span>Item TOML</span>
            <textarea
              rows={12}
              spellCheck={false}
              value={snippet}
              onChange={(e) => {
                setSnippet(e.target.value);
                onDirty();
              }}
            />
          </label>
        </>
      )}
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
      <div className="hf-form-actions">
        <Button onClick={() => save(put)}>Stage item</Button>
        {!isNew && kind !== "type" && (
          <Button onClick={() => save({ remove: { itemId } })}>
            Remove item
          </Button>
        )}
      </div>
    </div>
  );
}
