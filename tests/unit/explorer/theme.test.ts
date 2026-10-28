import { describe, expect, it } from "vitest";

import {
  CANVAS_THEME,
  createLabelColors,
  LABEL_PALETTE,
  labelColorAt,
  mixHex,
  withAlpha,
} from "@/web/areas/explore/explorer/lib/theme";

// The explorer's canvas colors are the kit's tokens, light and dark, and its
// label colors the kit's categorical hues (src/web/workspace.css).

describe("LABEL_PALETTE", () => {
  it("has eight distinct hues per theme, none the ink selection draws in", () => {
    for (const theme of ["light", "dark"] as const) {
      const palette = LABEL_PALETTE[theme];
      expect(palette).toHaveLength(8);
      expect(new Set(palette).size).toBe(8);
      for (const color of palette) expect(color).toMatch(/^#[0-9A-F]{6}$/);
    }
    expect(LABEL_PALETTE.light).not.toContain(CANVAS_THEME.selection);
  });
});

describe("createLabelColors", () => {
  it("assigns colors in registration order and never changes them", () => {
    const colors = createLabelColors();
    colors.register(["Order", "User"]);
    expect(colors.colorFor("Order")).toBe(LABEL_PALETTE.light[0]);
    expect(colors.colorFor("User")).toBe(LABEL_PALETTE.light[1]);
    colors.register(["Account", "Order"]);
    expect(colors.colorFor("Account")).toBe(LABEL_PALETTE.light[2]);
    expect(colors.colorFor("Order")).toBe(LABEL_PALETTE.light[0]);
  });

  it("gives unregistered labels the next free slot on first use", () => {
    const colors = createLabelColors();
    colors.register(["A"]);
    expect(colors.colorFor("Z")).toBe(LABEL_PALETTE.light[1]);
    expect(colors.colorFor("Z")).toBe(LABEL_PALETTE.light[1]);
  });

  it("keeps labels past the palette distinct from their first-lap twin", () => {
    const colors = createLabelColors();
    const labels = Array.from({ length: 17 }, (_, i) => `L${i}`);
    colors.register(labels);
    expect(colors.colorFor("L8")).not.toBe(colors.colorFor("L0"));
    expect(colors.colorFor("L16")).not.toBe(colors.colorFor("L8"));
  });
});

describe("color helpers", () => {
  it("cycles with lighter and deeper laps, per theme", () => {
    expect(labelColorAt(3)).toBe(LABEL_PALETTE.light[3]);
    expect(labelColorAt(3, "dark")).toBe(LABEL_PALETTE.dark[3]);
    expect(labelColorAt(11)).toBe(
      mixHex(LABEL_PALETTE.light[3], "#FFFFFF", 0.35),
    );
    expect(labelColorAt(19)).toBe(
      mixHex(LABEL_PALETTE.light[3], "#000000", 0.22),
    );
    expect(labelColorAt(24)).toBe(LABEL_PALETTE.light[0]);
    expect(labelColorAt(-4)).toBe(LABEL_PALETTE.light[0]);
  });

  it("mixes and adds alpha", () => {
    expect(mixHex("#000000", "#FFFFFF", 0.5)).toBe("#808080");
    expect(mixHex("nope", "#FFFFFF", 0.5)).toBe("nope");
    expect(withAlpha("#FF0000", 0.25)).toBe("rgba(255, 0, 0, 0.25)");
    expect(withAlpha("red", 0.25)).toBe("red");
  });

  it("reads the light theme without a document, every key a color or an alpha", () => {
    expect(CANVAS_THEME.background).toBe("#FBFBFB");
    for (const [key, value] of Object.entries(CANVAS_THEME))
      expect(value, key).toEqual(
        typeof value === "number"
          ? expect.any(Number)
          : expect.stringMatching(/^#[0-9A-F]{6}$/),
      );
  });
});
