import { describe, expect, it } from "vitest";
import {
  FOUNDRY_EDGE_LABELS,
  FOUNDRY_EDGE_PREFIX,
  FOUNDRY_LOAD,
  FOUNDRY_NODE_LABELS,
  FOUNDRY_NODE_PREFIX,
  FOUNDRY_NODES,
  FOUNDRY_PROP_PREFIX,
  isFoundryLabel,
  isFoundryStamp,
} from "@/shared/foundry-labels";

// Foundry's own labels in an ontology's database: the engine writes these
// (oa crates/extraction/src/store/mod.rs) and refuses ontology names with
// their prefixes.

describe("Foundry's labels", () => {
  it("are the engine's layout, each prefixed", () => {
    expect(FOUNDRY_NODE_LABELS).toEqual([
      "FoundryDocument",
      "FoundrySection",
      "FoundryMention",
      "FoundryEvidence",
      "FoundryChunk",
      "FoundryReview",
      "FoundrySwept",
      "FoundryViolation",
      "FoundryTx",
      "FoundryVersion",
      "FoundryLoad",
    ]);
    expect(FOUNDRY_EDGE_LABELS).toEqual([
      "FOUNDRY_IN_DOCUMENT",
      "FOUNDRY_IN_SECTION",
      "FOUNDRY_MENTION_OF",
      "FOUNDRY_FROM_SECTION",
      "FOUNDRY_FROM_CHUNK",
      "FOUNDRY_CONCERNS",
      "FOUNDRY_ABOUT",
    ]);
    for (const l of FOUNDRY_NODE_LABELS) {
      expect(l.startsWith(FOUNDRY_NODE_PREFIX)).toBe(true);
      expect(isFoundryLabel("node", l)).toBe(true);
    }
    for (const l of FOUNDRY_EDGE_LABELS) {
      expect(l.startsWith(FOUNDRY_EDGE_PREFIX)).toBe(true);
      expect(isFoundryLabel("edge", l)).toBe(true);
    }
  });

  it("name the engine's transactions and versions", () => {
    expect(FOUNDRY_NODES.Tx).toBe("FoundryTx");
    expect(FOUNDRY_NODES.Version).toBe("FoundryVersion");
    // Every label list the Explorer offers spreads FOUNDRY_NODE_LABELS.
    expect(FOUNDRY_NODE_LABELS).toContain("FoundryTx");
    expect(FOUNDRY_NODE_LABELS).toContain("FoundryVersion");
  });

  it("stamp what a load writes with prefixed props", () => {
    expect(FOUNDRY_PROP_PREFIX).toBe("foundry_");
    expect(FOUNDRY_LOAD.startsWith(FOUNDRY_PROP_PREFIX)).toBe(true);
  });

  it("stamp everything but Foundry's own, which keep all but the load", () => {
    for (const key of ["foundry_tx", "foundry_part", "foundry_later"]) {
      expect(isFoundryStamp("node", "Customer", key), key).toBe(true);
      expect(isFoundryStamp("edge", "billed_to", key), key).toBe(true);
      expect(isFoundryStamp("node", "FoundryTx", key), key).toBe(false);
      expect(isFoundryStamp("edge", "FOUNDRY_ABOUT", key), key).toBe(false);
      // A label is Foundry's by its side's prefix.
      expect(isFoundryStamp("edge", "FoundryTx", key), key).toBe(true);
    }
    for (const [target, label] of [
      ["node", "Customer"],
      ["node", "FoundryTx"],
      ["edge", "FOUNDRY_ABOUT"],
    ] as const)
      expect(isFoundryStamp(target, label, FOUNDRY_LOAD)).toBe(true);
    for (const key of ["foundry", "Foundry_tx", "my_foundry_tx", "tx"])
      expect(isFoundryStamp("node", "Customer", key), key).toBe(false);
  });

  it("are told by their prefix, per side", () => {
    expect(isFoundryLabel("node", "FoundryMention")).toBe(true);
    expect(isFoundryLabel("node", "FoundryLater")).toBe(true);
    expect(isFoundryLabel("edge", "FOUNDRY_LATER")).toBe(true);
    expect(isFoundryLabel("edge", "signed_with")).toBe(false);
    // What the labels were called before is the ontology's to use.
    for (const l of ["Document", "Mention", "Chunk"])
      expect(isFoundryLabel("node", l)).toBe(false);
    for (const l of ["MENTION_OF", "ABOUT", "CONCERNS"])
      expect(isFoundryLabel("edge", l)).toBe(false);
    // The prefix is case-sensitive and per side.
    expect(isFoundryLabel("node", "foundryDocument")).toBe(false);
    expect(isFoundryLabel("node", "FOUNDRY_ABOUT")).toBe(false);
    expect(isFoundryLabel("edge", "FoundryDocument")).toBe(false);
    expect(isFoundryLabel("edge", "Foundry_ABOUT")).toBe(false);
  });
});
