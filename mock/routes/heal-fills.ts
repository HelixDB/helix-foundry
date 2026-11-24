import type { FoundryHealFinding } from "../../src/shared/wfe";
import { config, onReset } from "../state";

// A finding's Add this link, as the mock keeps it: making a version current
// with a finding's fingerprint (fillFinding, ./ontology.ts's set-current)
// moves the current load to it, shown moving the first time the list
// (./suggestions.ts) is read and moved after, unless `healMove:
// "needs_load"`, which the version then needs. Each relationship no link
// covers carries a state; the others none. Apart from ./suggestions.ts,
// which ./ontology.ts can't import without a cycle. With `healFill: true` a
// moved load then fills the documents behind the finding: applying the
// next read, fixed after.
type Knobs = { healMove?: "needs_load"; healFill?: boolean };
const knobs = config as typeof config & Knobs;

// A finding's move, by ontology and fingerprint: the version it moves the
// current load to, and how often the list has shown it.
type Fill = { versionId: string; version: number; shown: number };
const fills = new Map<string, Fill>();
onReset(() => {
  delete knobs.healMove;
  delete knobs.healFill;
  fills.clear();
});

/** Records a finding's Add this link: the current load moves to the version. */
export function fillFinding(
  ontologyId: string,
  fingerprint: string,
  versionId: string,
  version: number,
) {
  fills.set(`${ontologyId}/${fingerprint}`, { versionId, version, shown: 0 });
}

/** A listed finding with its state: its move, else open for a relationship no link covers. */
export function withState(
  ontologyId: string,
  f: FoundryHealFinding,
): FoundryHealFinding {
  if (f.kind !== "unmatched") return f;
  const fill = fills.get(`${ontologyId}/${f.fingerprint}`);
  if (!fill) return { ...f, state: { open: {} } };
  const version = { versionId: fill.versionId, version: String(fill.version) };
  if (knobs.healMove === "needs_load")
    return {
      ...f,
      state: {
        needsLoad: {
          ...version,
          why: [{ kind: "type_added", item: "types.Deal" }],
        },
      },
    };
  fill.shown++;
  if (fill.shown === 1) return { ...f, state: { moving: version } };
  const canonical = (
    JSON.parse(f.findingJson) as { signal: { canonical: string } }
  ).signal.canonical;
  const linksAdded = [canonical];
  if (knobs.healFill && fill.shown >= 3) {
    const filling = {
      ...version,
      linksAdded,
      docsOwed: "2",
      docsDone: fill.shown === 3 ? "1" : "2",
      docsLeft: "0",
      reviewsResolved: fill.shown === 3 ? "3" : "5",
      factsAdded: fill.shown === 3 ? "2" : "4",
      spentUsd: fill.shown === 3 ? 0.01 : 0.02,
      capUsd: 5,
    };
    return {
      ...f,
      state: fill.shown === 3 ? { applying: filling } : { fixed: filling },
    };
  }
  return { ...f, state: { moved: { ...version, linksAdded } } };
}
