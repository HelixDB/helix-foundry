import { useState } from "react";
import ObjectActionForm from "./object-action-form";
import { Network, Plus, Sparkles } from "lucide-react";
import { Link } from "react-router-dom";
import { useWorkspace, useResources, PageHeader, Empty, Modal } from "./ui";
import { PublishedSchema } from "./areas/ontology/schema";
import { OntologyUpdate } from "./ontology-update";
import type { Resource } from "../../../packages/shared/src";
export function OntologyPage() {
  const { role } = useWorkspace(),
    types = useResources("objectType"),
    datasets = useResources("dataset"),
    proposals = useResources("proposal"),
    [creating, setCreating] = useState(false);
  const list = [...(types.data?.items || [])],
    proposal = proposals.data?.items.find(
      (p: Resource) => p.data.publishedGeneration === list[0]?.data.generation,
    ),
    relations = proposal?.data.bundle.relationships || [];
  list.sort((a: Resource, b: Resource) =>
    relations.some((r: any) => r.fromType === a.name && r.toType === b.name)
      ? -1
      : relations.some((r: any) => r.fromType === b.name && r.toType === a.name)
        ? 1
        : 0,
  );
  return (
    <OntologyUpdate
      published={proposal}
      header={
        <PageHeader
          eyebrow="THE SHAPE OF YOUR BUSINESS"
          title="Ontology"
          description="Real-world objects and relationships, grounded in your data."
        >
          {role !== "viewer" && types.data?.items.length > 0 && (
            <button className="button" onClick={() => setCreating(true)}>
              <Plus size={15} /> Workspace action
            </button>
          )}
          <Link className="button" to="/proposals">
            <Sparkles size={16} /> Review suggestions
          </Link>
        </PageHeader>
      }
    >
      {list.length ? (
        // The data model: types and how they relate. Records themselves are
        // in the Explorer.
        <div className="ontology-schema">
          <PublishedSchema
            types={list}
            relationships={relations}
            previews={proposal?.data.validation?.previews || {}}
            datasets={datasets.data?.items || []}
          />
        </div>
      ) : (
        <div className="panel">
          <Empty
            icon={Network}
            title="Give your data a shared language"
            text="Foundry can suggest the customers, orders, and relationships hidden in your datasets. Publish a tested proposal to bring the model to life."
          />
        </div>
      )}
      {creating && (
        <Modal title="Workspace action" onClose={() => setCreating(false)}>
          <ObjectActionForm types={list} onDone={() => setCreating(false)} />
        </Modal>
      )}
    </OntologyUpdate>
  );
}
