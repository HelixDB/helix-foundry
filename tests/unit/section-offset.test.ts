import { describe, expect, it } from "vitest";
import {
  SECTION_MIN_HEIGHT,
  SECTION_OFFSET_DEFAULT,
  clampSectionOffset,
  maxSectionOffset,
  storedSectionOffset,
} from "@/web/shell/section-offset";

// The line above a section's sidebar content: never over the sections' links,
// and never so low the content loses its height.

describe("clampSectionOffset", () => {
  const room = SECTION_MIN_HEIGHT + 100;

  it("keeps an offset that fits", () => {
    expect(clampSectionOffset(0, room)).toBe(0);
    expect(clampSectionOffset(40, room)).toBe(40);
    expect(clampSectionOffset(100, room)).toBe(100);
  });

  it("stops at the sections", () => {
    expect(clampSectionOffset(-16, room)).toBe(0);
  });

  it("leaves the content its height", () => {
    expect(maxSectionOffset(room)).toBe(100);
    expect(clampSectionOffset(101, room)).toBe(100);
    expect(room - clampSectionOffset(5000, room)).toBe(SECTION_MIN_HEIGHT);
  });

  it("has no offset to give in a room at or under the content's height", () => {
    for (const short of [SECTION_MIN_HEIGHT, SECTION_MIN_HEIGHT - 1, 0]) {
      expect(maxSectionOffset(short)).toBe(0);
      expect(clampSectionOffset(SECTION_OFFSET_DEFAULT, short)).toBe(0);
    }
  });

  it("is a whole number of pixels, as a pointer's position may not be", () => {
    expect(clampSectionOffset(40.4, room)).toBe(40);
  });
});

describe("storedSectionOffset", () => {
  it("reads a pixel count", () => {
    expect(storedSectionOffset("0")).toBe(0);
    expect(storedSectionOffset("120")).toBe(120);
  });

  it("reads anything else as the default", () => {
    for (const bad of ["", " ", "soon", "12px", "-4", "NaN", "Infinity", "{}"])
      expect(storedSectionOffset(bad)).toBe(SECTION_OFFSET_DEFAULT);
  });

  it("leaves a huge one to the clamp", () => {
    expect(clampSectionOffset(storedSectionOffset("1e9"), 300)).toBe(
      300 - SECTION_MIN_HEIGHT,
    );
  });
});
