import { afterEach, describe, expect, it, vi } from "vitest";
import {
  activeSection,
  allSections,
  footNav,
  granteeNav,
  linkedSections,
  navSection,
  primaryNav,
  secondaryNav,
  sectionPanels,
  sectionSidebars,
} from "@/web/shell/nav";

// The shell's sections (src/web/shell/nav.ts): Ask first and open on /,
// the main page; Ask's threads in the sidebar under the sections, and the
// other sections' panels beside it.

describe("the sections", () => {
  it("lists Ask first, then Dashboards, Workflows and Ontologies, with Connectors at the foot", () => {
    expect(primaryNav.map((n) => [n.label, n.to])).toEqual([
      ["Analyst", "/ask"],
      ["Dashboards", "/dashboards"],
      ["Workflows", "/workflows"],
      ["Ontologies", "/ontology"],
    ]);
    expect(footNav.map((n) => [n.label, n.to])).toEqual([
      ["Connectors", "/data"],
    ]);
    expect(secondaryNav.map((n) => n.label)).toEqual(["Settings"]);
    // The palette and the fallback list them in the sidebar's order.
    expect(linkedSections.map((n) => n.id)).toEqual([
      "ask",
      "dashboards",
      "automations",
      "ontology",
      "data",
      "settings",
    ]);
  });

  it("gives a grantee Analyst, Dashboards and Ontologies", () => {
    expect(granteeNav.map((n) => [n.id, n.label])).toEqual([
      ["ask", "Analyst"],
      ["dashboards", "Dashboards"],
      ["ontology", "Ontologies"],
    ]);
  });

  it("keeps the Explorer a section without a link, shown as Ontologies", () => {
    expect(allSections.map((n) => n.id)).toContain("explore");
    expect(navSection("explore")).toBe("ontology");
    expect(navSection("data")).toBe("data");
    expect(navSection(undefined)).toBeUndefined();
  });
});

describe("on a build with Founder", () => {
  // The build is read as the module loads (src/shared/deployment.ts).
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

  it("names Ask Founder, a grantee's too, with the agent's icon, and its sidebar's threads Threads", async () => {
    vi.resetModules();
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_ASSISTANT", "1");
    const nav = await import("@/web/shell/nav");
    const { Bot } = await import("lucide-react");
    expect(nav.primaryNav[0]).toMatchObject({
      id: "ask",
      label: "Founder",
      icon: Bot,
      to: "/ask",
      match: ["/ask"],
    });
    expect(nav.granteeNav.map((n) => [n.id, n.label, n.to])).toEqual([
      ["ask", "Founder", "/ask"],
      ["dashboards", "Dashboards", "/dashboards"],
      ["ontology", "Ontologies", "/ontology"],
    ]);
    expect(nav.sectionSidebars.ask?.label).toBe("Threads");
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
    expect(activeSection("/workflows/a/runs/r")).toBe("automations");
    expect(activeSection("/asks")).toBeUndefined();
    expect(activeSection("/nowhere")).toBeUndefined();
  });
});

describe("what a section shows beside the page", () => {
  it("puts Ask's threads, the dashboards and the workflows in the sidebar, under the sections", () => {
    expect(Object.keys(sectionSidebars).sort()).toEqual([
      "ask",
      "automations",
      "dashboards",
    ]);
    expect(sectionSidebars.ask?.label).toBe("Analyst threads");
    expect(sectionSidebars.dashboards?.label).toBe("Dashboards");
    expect(sectionSidebars.automations?.label).toBe("Workflows");
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
