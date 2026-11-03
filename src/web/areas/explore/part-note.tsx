import { useReadsPart } from "./graph-reader";

/**
 * One quiet line on a page of an ontology its reader holds only part of: what
 * it shows is what the sources shared with them hold. Nothing for anyone else.
 */
export function PartNote() {
  return useReadsPart() ? (
    <p className="hf-part-note" role="note">
      You can read part of this ontology: what the sources shared with you hold.
      Some records and values may not appear.
    </p>
  ) : null;
}
