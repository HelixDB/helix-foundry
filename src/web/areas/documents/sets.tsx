import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { FoundryDocument, FoundryDocumentSet } from "../../../shared/wfe";
import { Button, Dialog, Select, SkeletonText } from "../../kit";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import {
  createDocumentSet,
  documentSetQueries,
  documentsChanged,
  moveDocument,
  updateDocumentSet,
} from "./documents-api";
import { useFileMappings } from "./mappings";
import {
  MAX_SET_NAME,
  andList,
  kindText,
  readersText,
  readsText,
  setNameProblem,
} from "./model";

// Document sets are the workspace's folders of documents: each document is
// in one, a version reads the sets of its scope (picked when it was
// generated), and its routes say what it reads from each. Owners and admins
// create a set, describe one (the ontology agent reads the description),
// and move a document between sets; the Data tree renames and deletes them
// in place. A connector's own sets (FoundryDocumentSet.connector) are its
// syncs': none of this changes them, and no upload goes into one.

/** The workspace's ontologies' names, by id, for what reads a set. */
export function useOntologyNames() {
  const { id } = useWorkspace();
  const list = useQuery(ontologyQueries.list(id));
  const names = new Map(
    (list.data?.ontologies || []).map((o) => [o.id, o.name || o.id]),
  );
  return (ontologyId = "") => names.get(ontologyId) || "an ontology";
}

/** Creates a set, or describes and renames one. */
export function SetDialog({
  set,
  sets,
  onClose,
}: {
  // The set to edit; none creates one.
  set?: FoundryDocumentSet;
  sets: readonly FoundryDocumentSet[];
  onClose: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [name, setName] = useState(set?.name || ""),
    [description, setDescription] = useState(set?.description || ""),
    [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  const problem = setNameProblem(name, sets, set?.id);
  return (
    <Dialog
      title={set ? `Edit ${set.name}` : "New document set"}
      description={
        set
          ? undefined
          : "A folder of documents read together. A generation reads the sets you pick, and its routes say what each one’s documents fill."
      }
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setPending(true);
          setError("");
          const body = { name: name.trim(), description: description.trim() };
          try {
            if (set) await updateDocumentSet(id, set.id, body);
            else await createDocumentSet(id, body);
            await qc.invalidateQueries({ queryKey: [id, "document-sets"] });
            notify(set ? `Saved ${body.name}.` : `Created ${body.name}.`);
            onClose();
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label className="hf-field">
          <span>Name</span>
          <input
            required
            data-autofocus
            maxLength={MAX_SET_NAME * 2}
            placeholder="e.g. Contracts"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {name.trim() && problem && <small>{problem}</small>}
        </label>
        <label className="hf-field">
          <span>Description</span>
          <textarea
            rows={3}
            maxLength={1000}
            placeholder="Optional. What its documents are, for the agent that drafts the next version."
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!!problem}
          >
            {set ? "Save" : "Create set"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/**
 * Moves an upload to another set people made: a connector's sets take
 * none. The loads that read it keep what they read; every ontology that
 * reads the new set reads it there. A table file a current version maps
 * that leaves the version's sets fails its next load until the version is
 * revised: the dialog says so before the move.
 */
export function MoveDialog({
  document: d,
  onClose,
}: {
  document: FoundryDocument;
  onClose: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    name = useOntologyNames();
  const sets = useQuery(documentSetQueries.list(id));
  const others = (sets.data || []).filter(
    (s) => !s.connector && s.id !== d.setId,
  );
  const [to, setTo] = useState(""),
    [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  const target = others.find((s) => s.id === to) || others[0];
  // The versions mapping it that don't read the set it would move to.
  const breaks = useFileMappings(d).filter(
    (m) =>
      m.mapping?.kind === "mapped" &&
      !!target &&
      !(m.version?.documentSetIds || []).includes(target.id),
  );
  return (
    <Dialog
      title={`Move ${d.name}`}
      description="Loads that read it keep what they read until their next load."
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (!target) return;
          setPending(true);
          setError("");
          try {
            const moved = await moveDocument(id, d.id, target.id);
            await documentsChanged(qc, id);
            notify(
              `Moved ${d.name} to ${target.name}. ${kindText(moved.document, target.name || "") ?? readsText(moved.reads || [], { name, setName: target.name || "", readers: target.readBy || [] })}`,
            );
            onClose();
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        {sets.isPending ? (
          <SkeletonText lines={1} />
        ) : !others.length ? (
          <p className="on-muted">
            There is no other set. Create one in Connectors first.
          </p>
        ) : (
          <label className="hf-field">
            <span>Set</span>
            <Select
              label="Set"
              autoFocus
              value={target?.id ?? ""}
              onChange={setTo}
              options={others.map((s) => ({
                value: s.id,
                label: s.name || "",
              }))}
            />
            {target && (
              <small>
                {kindText(d, target.name || "") ??
                  readersText(target.name || "", target.readBy || [], name)}
              </small>
            )}
          </label>
        )}
        {breaks.length > 0 && (
          <p role="alert" className="hf-form-error">
            {andList(
              breaks.map(
                (m) => `${name(m.ontology.id)}’s version ${m.version?.version}`,
              ),
            )}{" "}
            {breaks.length === 1 ? "maps" : "map"} this file and{" "}
            {breaks.length === 1 ? "doesn’t" : "don’t"} read {target?.name}: the
            next load fails until the version is revised to stop mapping it, or
            the file is moved back.
          </p>
        )}
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!target}
          >
            Move
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
