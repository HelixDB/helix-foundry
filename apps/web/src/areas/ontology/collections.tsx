import { useState, useDeferredValue } from "react";
import { useQuery } from "@tanstack/react-query";
import type {
  Resource,
  RecordCollection,
  RecordCollectionDetail,
} from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace, useAction } from "../../ui";
import { paths } from "../../paths";
import {
  Page,
  PageHeader,
  Button,
  ButtonLink,
  Dialog,
  ConfirmDialog,
  Skeleton,
  SearchInput,
} from "../../kit";

export function RecordCollectionsPage() {
  const { id, role } = useWorkspace(),
    action = useAction();
  const [selected, setSelected] = useState<string | null>(null);
  const [form, setForm] = useState<RecordCollection | "new" | null>(null);
  const [deleting, setDeleting] = useState<RecordCollection | null>(null);
  const [adding, setAdding] = useState<RecordCollection | null>(null);
  const list = useQuery({
    queryKey: [id, "record-collections"],
    queryFn: () =>
      api<{ items: RecordCollection[] }>(
        `/workspaces/${id}/record-collections`,
      ),
  });
  const detail = useQuery({
    queryKey: [id, "record-collection", selected],
    enabled: !!selected,
    queryFn: () =>
      api<RecordCollectionDetail>(
        `/workspaces/${id}/record-collections/${selected}`,
      ),
  });
  const editable = role !== "viewer";
  const update = (collection: RecordCollection, logicalIds: string[]) =>
    action.run(
      () =>
        api<RecordCollection>(
          `/workspaces/${id}/record-collections/${collection.id}`,
          { revision: collection.revision, name: collection.name, logicalIds },
          "PUT",
        ),
      "Collection saved",
    );
  return (
    <Page>
      <PageHeader
        title="Record collections"
        subtitle="Keep a named list of records to return to. Collections follow record identities across publications."
        actions={
          <>
            <ButtonLink to={paths.explorer()}>Explorer</ButtonLink>
            {editable && (
              <Button variant="primary" onClick={() => setForm("new")}>
                New collection
              </Button>
            )}
          </>
        }
      />
      <div className="rc-layout">
        <section aria-label="Collections" className="rc-list">
          {list.isPending ? (
            <Skeleton height={100} />
          ) : list.error ? (
            <p role="alert">{list.error.message}</p>
          ) : list.data.items.length ? (
            list.data.items.map((c) => (
              <Button
                key={c.id}
                variant={selected === c.id ? "secondary" : "ghost"}
                onClick={() => setSelected(c.id)}
              >
                <span>{c.name}</span>
                <small>{c.logicalIds.length} records</small>
              </Button>
            ))
          ) : (
            <p className="muted">No collections yet.</p>
          )}
        </section>
        <section aria-label="Collection records" className="rc-detail">
          {!selected ? (
            <p className="muted">Choose a collection to see its records.</p>
          ) : detail.isPending ? (
            <Skeleton height={160} />
          ) : detail.error ? (
            <p role="alert">{detail.error.message}</p>
          ) : (
            <>
              <div className="rc-heading">
                <h2>{detail.data.name}</h2>
                {editable && (
                  <div>
                    <Button
                      disabled={action.pending}
                      onClick={async () => {
                        const q = await detail.refetch();
                        if (q.data && !q.error) setAdding(q.data);
                      }}
                    >
                      Add record
                    </Button>
                    <Button
                      disabled={action.pending}
                      onClick={async () => {
                        const q = await detail.refetch();
                        if (q.data && !q.error) setForm(q.data);
                      }}
                    >
                      Rename
                    </Button>
                    <Button
                      variant="ghost"
                      disabled={action.pending}
                      onClick={async () => {
                        const q = await detail.refetch();
                        if (q.data && !q.error) setDeleting(q.data);
                      }}
                    >
                      Delete collection
                    </Button>
                  </div>
                )}
              </div>
              <p className="muted">
                {detail.data.members.length} records · Current published records
                are shown. Unavailable records stay in the collection until
                removed.
              </p>
              <ul className="rc-members">
                {detail.data.members.map((member) => (
                  <li key={member.logicalId}>
                    <div>
                      {member.available ? (
                        <ButtonLink
                          to={paths.object(member.logicalId, member.type!)}
                        >
                          {member.name}
                        </ButtonLink>
                      ) : (
                        <>
                          <strong>Record unavailable</strong>
                          <code>{member.logicalId}</code>
                        </>
                      )}
                      {member.type && <small>{member.type}</small>}
                    </div>
                    {editable && (
                      <Button
                        size="sm"
                        pending={action.pending}
                        aria-label={`Remove ${member.name || member.logicalId}`}
                        onClick={() =>
                          update(
                            detail.data,
                            detail.data.logicalIds.filter(
                              (x) => x !== member.logicalId,
                            ),
                          )
                        }
                      >
                        Remove
                      </Button>
                    )}
                  </li>
                ))}
              </ul>
              {!detail.data.members.length && (
                <p>
                  No records in this collection. Add one from the current
                  workspace.
                </p>
              )}
            </>
          )}
        </section>
      </div>
      {form && (
        <CollectionNameForm
          key={form === "new" ? "new" : form.id}
          collection={form === "new" ? null : form}
          onClose={() => setForm(null)}
          onSaved={(c) => {
            setSelected(c.id);
            setForm(null);
          }}
        />
      )}
      {adding && (
        <AddCollectionRecord
          collection={adding}
          onClose={() => setAdding(null)}
        />
      )}
      {deleting && (
        <ConfirmDialog
          title="Delete collection"
          description={`Delete ${deleting.name}? Its underlying records and source data remain available.`}
          confirmLabel="Delete collection"
          tone="danger"
          pending={action.pending}
          onCancel={() => setDeleting(null)}
          onConfirm={async () => {
            const result = await action.run(
              () =>
                api(
                  `/workspaces/${id}/record-collections/${deleting.id}`,
                  { revision: deleting.revision },
                  "DELETE",
                ),
              "Collection deleted",
            );
            if (result) {
              setDeleting(null);
              if (selected === deleting.id) setSelected(null);
            }
          }}
        />
      )}
    </Page>
  );
}
function CollectionNameForm({
  collection,
  onClose,
  onSaved,
}: {
  collection: RecordCollection | null;
  onClose: () => void;
  onSaved: (c: RecordCollection) => void;
}) {
  const { id } = useWorkspace(),
    action = useAction();
  const [name, setName] = useState(collection?.name || "");
  const save = async () => {
    const result = await action.run(
      () =>
        api<RecordCollection>(
          `/workspaces/${id}/record-collections${collection ? "/" + collection.id : ""}`,
          collection
            ? {
                revision: collection.revision,
                name,
                logicalIds: collection.logicalIds,
              }
            : { name },
          collection ? "PUT" : "POST",
        ),
      "Collection saved",
    );
    if (result) onSaved(result);
  };
  return (
    <Dialog
      title={collection ? "Rename collection" : "New collection"}
      onClose={onClose}
      footer={
        <>
          <Button disabled={action.pending} onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!name.trim()}
            pending={action.pending}
            onClick={save}
          >
            Save collection
          </Button>
        </>
      }
    >
      <label>
        Collection name
        <input
          data-autofocus
          maxLength={120}
          value={name}
          disabled={action.pending}
          onChange={(e) => setName(e.target.value)}
        />
      </label>
      <p className="muted">
        Up to 40 collections per workspace, with up to 200 records each.
      </p>
    </Dialog>
  );
}
function AddCollectionRecord({
  collection,
  onClose,
}: {
  collection: RecordCollection;
  onClose: () => void;
}) {
  const { id } = useWorkspace(),
    action = useAction();
  const [search, setSearch] = useState("");
  const term = useDeferredValue(search.trim());
  const results = useQuery({
    queryKey: [id, "collection-record-search", term],
    enabled: !!term,
    queryFn: () =>
      api<{ items: Resource[]; total: number }>(
        `/workspaces/${id}/resources/object?${new URLSearchParams({ search: term, limit: "8" })}`,
      ),
  });
  const ready = term === search.trim() && !results.isFetching;
  return (
    <Dialog
      title={`Add record to ${collection.name}`}
      onClose={onClose}
      footer={
        <Button disabled={action.pending} onClick={onClose}>
          Cancel
        </Button>
      }
    >
      <SearchInput
        label="Find collection record"
        placeholder="Find a record"
        value={search}
        onChange={setSearch}
      />
      {collection.logicalIds.length >= 200 && (
        <p role="alert">
          This collection has reached its 200-record limit. Remove a record
          before adding another.
        </p>
      )}
      {results.error ? (
        <p role="alert">{results.error.message}</p>
      ) : !search.trim() ? (
        <p className="muted">
          Search the current workspace's published records.
        </p>
      ) : !ready ? (
        <p role="status">Finding records…</p>
      ) : (
        <>
          <ul className="rc-add-results">
            {results.data?.items.map((r) => (
              <li key={r.id}>
                <div>
                  <strong>{r.data.label || r.name}</strong>
                  <small>{r.data.type}</small>
                </div>
                <Button
                  size="sm"
                  disabled={
                    action.pending ||
                    collection.logicalIds.includes(r.data.logicalId) ||
                    collection.logicalIds.length >= 200
                  }
                  onClick={async () => {
                    const result = await action.run(
                      () =>
                        api(
                          `/workspaces/${id}/record-collections/${collection.id}`,
                          {
                            revision: collection.revision,
                            name: collection.name,
                            logicalIds: [
                              ...collection.logicalIds,
                              r.data.logicalId,
                            ],
                          },
                          "PUT",
                        ),
                      "Record added",
                    );
                    if (result) onClose();
                  }}
                >
                  {collection.logicalIds.includes(r.data.logicalId)
                    ? "Already added"
                    : "Add"}
                </Button>
              </li>
            ))}
          </ul>
          <p role="status">
            {results.data?.total
              ? `Showing ${results.data.items.length} of ${results.data.total} matching records`
              : "No records match."}
          </p>
        </>
      )}
      <p className="muted">
        A collection keeps record identities, not a frozen copy of their values.
        If another editor changes this collection, cancel and reopen to use its
        latest revision.
      </p>
    </Dialog>
  );
}
