import { describe, expect, it } from "vitest";
import {
  activeSection,
  primaryNav,
  secondaryNav,
  sectionPanels,
  sectionSidebars,
} from "@/web/shell/nav";

// The shell's sections (src/web/shell/nav.ts): Ask first and open on /,
// the main page; Ask's threads in the sidebar under the sections, and the
// other sections' panels beside it.

describe("the sections", () => {
  it("lists Ask first, then Ontologies, the Explorer and Connectors", () => {
    expect(primaryNav.map((n) => [n.label, n.to])).toEqual([
      ["Analyst", "/ask"],
      ["Ontologies", "/ontology"],
      ["Explorer", "/explore"],
      ["Connectors", "/data"],
    ]);
    expect(secondaryNav.map((n) => n.label)).toEqual(["Settings"]);
  });
});

describe("activeSection", () => {
  it("opens Ask on /, the main page, and on its own pages", () => {
    expect(activeSection("/")).toBe("ask");
    expect(activeSection("/ask")).toBe("ask");
    expect(activeSection("/ask/ont_1")).toBe("ask");
  });

  it("marks the section a page is under, and none for any other path", () => {
    expect(activeSection("/ontology/ont_1/versions/v1")).toBe("ontology");
    expect(activeSection("/explore/ont_1/records")).toBe("explore");
    expect(activeSection("/data/connections/conn_1")).toBe("data");
    expect(activeSection("/settings/ai")).toBe("settings");
    expect(activeSection("/asks")).toBeUndefined();
    expect(activeSection("/nowhere")).toBeUndefined();
  });
});

describe("what a section shows beside the page", () => {
  it("puts Ask's threads in the sidebar, under the sections", () => {
    expect(Object.keys(sectionSidebars)).toEqual(["ask"]);
    expect(sectionSidebars.ask?.label).toBe("Analyst threads");
  });

  it("gives Connectors, Ontologies and the Explorer a panel beside the sidebar, and Ask none", () => {
    expect(
      Object.entries(sectionPanels)
        .map(([id, p]) => [id, p!.kind, p!.label])
        .sort(),
    ).toEqual([
      ["data", "component", "Connectors explorer"],
      ["explore", "page", "Explorer panel"],
      ["ontology", "component", "Ontologies"],
    ]);
    expect(sectionPanels.ask).toBeUndefined();
  });
});
