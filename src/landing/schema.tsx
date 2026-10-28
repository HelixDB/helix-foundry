import { useLayoutEffect, useRef } from "react";
import { SuggestedOntologyView } from "./vendor/web/suggested-ontology";
import type { Resource, SuggestedOntology } from "./vendor/shared";
import data from "./data/loopwell-ontology.json";
import { attach } from "./field/field";

// The schema view from setup and the Ontology page, read-only, showing the
// ontology Foundry suggests for the Loopwell sample data (11 tables in
// PlanetScale, WorkOS, Stripe, PostHog and S3). The field draws its dotted
// ground, so its dots can arrive from the hero's graph, and marks the figure
// it is rendered in live once they have.
export function Schema() {
  const wrap = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    attach(
      "schema",
      wrap.current!.querySelector<HTMLElement>(".so-stage"),
      wrap.current!.closest("figure"),
    );
    return () => attach("schema", null);
  }, []);
  return (
    <div ref={wrap}>
      <SuggestedOntologyView
        readOnly
        ontology={data.ontology as unknown as SuggestedOntology}
        datasets={data.datasets as unknown as Resource[]}
      />
    </div>
  );
}
