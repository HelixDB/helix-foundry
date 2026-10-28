import { useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Shapes, Trash2 } from "lucide-react";
import type { ListOntologiesResponse } from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  IconButton,
  Page,
  PageHeader,
  SkeletonText,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { canManage } from "./parts";
import { deleteOntology, ontologyQueries } from "./queries";
import { lastOntology, ontologyName, useOntology } from "./scope";

// /ontology opens an ontology (OpenOntology), as /explore and /ask do: the
// workspace's ontologies are the section's panel (./ontologies-panel.tsx),
// each bound to one Helix database; /ontology/new creates one
// (./new-ontology.tsx). Deleting an ontology deletes its loads from its
// database and leaves the rest.

/**
 * /ontology, /explore and /ask: the section's page of the ontology last
 * opened in this browser while it still exists, else of the workspace's
 * first ontology (in the list's order). It replaces its own entry, so Back
 * skips it, and only while the address is still its own: a link followed
 * while the list loads isn't taken back. A workspace without one is
 * onboarding, which main.tsx sends to /ontology/new, as this does while the
 * routes catch up.
 */
export function OpenOntology({
  title,
  to,
}: {
  title: string;
  to: (ontologyId: string) => string;
}) {
  const { id: ws } = useWorkspace();
  const list = useQuery(ontologyQueries.list(ws));
  const ontologies = list.data?.ontologies || [],
    last = lastOntology(ws);
  const open = ontologies.find((o) => o.id === last) ?? ontologies[0];
  const target = open ? to(open.id) : list.data ? paths.newOntology() : null;
  const { pathname } = useLocation(),
    navigate = useNavigate();
  // The router renders a followed link's route in a transition, so this page
  // can still render after the address has moved on: compare the address.
  useEffect(() => {
    if (target && window.location.pathname === pathname)
      navigate(target, { replace: true });
  }, [target, pathname, navigate]);
  if (target) return null;
  return (
    <Page>
      <PageHeader title={title} />
      {list.error ? (
        <EmptyState
          icon={Shapes}
          title="Couldn’t load the ontologies"
          text={list.error.message}
          action={<Button onClick={() => void list.refetch()}>Retry</Button>}
        />
      ) : (
        <SkeletonText lines={4} />
      )}
    </Page>
  );
}

/**
 * Delete in an ontology's header, for owners and admins: Foundry's rows,
 * and what its loads wrote into the database (removed in the background);
 * the database's other data stays.
 */
export function DeleteOntology() {
  const { id, role, notify } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const [confirming, setConfirming] = useState(false),
    [pending, setPending] = useState(false);
  if (!canManage(role)) return null;
  const run = async () => {
    setPending(true);
    try {
      await deleteOntology(id, o);
      notify(
        `${ontologyName(ontology)} is deleted. Its loads are leaving the database; your own data stays.`,
      );
      setConfirming(false);
      // /ontology opens the list's first, which mustn't be this one, even
      // from a read that was under way.
      await qc.cancelQueries({ queryKey: [id, "ontologies"] });
      qc.setQueryData<ListOntologiesResponse>(
        ontologyQueries.list(id).queryKey,
        (l) => l && { ontologies: l.ontologies?.filter((x) => x.id !== o) },
      );
      navigate(paths.ontologies());
      await qc.invalidateQueries({ queryKey: [id, "ontologies"] });
      qc.removeQueries({ queryKey: [id, "ontology", o] });
    } catch (e) {
      const message = (e as Error).message;
      notify(message.charAt(0).toUpperCase() + message.slice(1), true);
      setConfirming(false);
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <IconButton
        label="Delete ontology"
        icon={Trash2}
        onClick={() => setConfirming(true)}
      />
      {confirming && (
        <ConfirmDialog
          title={`Delete ${ontologyName(ontology)}?`}
          description={`Foundry forgets this ontology: its versions, generations and loads, and deletes what its loads wrote from ${ontology.databaseName || "its database"}, with any edges added to those nodes. The database stays in Helix with your own data, and your documents stay in Connectors.`}
          confirmLabel="Delete ontology"
          tone="danger"
          pending={pending}
          onConfirm={run}
          onCancel={() => setConfirming(false)}
        />
      )}
    </>
  );
}
