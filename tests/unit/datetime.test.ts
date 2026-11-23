import { describe, expect, it } from "vitest";
import { helixDateTime } from "@/shared/datetime";

// helixDateTime reads RFC 3339 as helix-db's DateTime::parse_rfc3339 does
// (chrono's parse_from_rfc3339, then timestamp_millis) and renders it as a
// read renders a DateTime.

describe("helixDateTime", () => {
  it("keeps the doc comment's examples", () => {
    expect(helixDateTime("2026-03-14T09:30:00+01:00")).toBe(
      "2026-03-14T08:30:00.000Z",
    );
    expect(helixDateTime("2026-03-14t08:30:00.25z")).toBe(
      "2026-03-14T08:30:00.250Z",
    );
    expect(helixDateTime("2026-03-14 08:30:00.0009Z")).toBe(
      "2026-03-14T08:30:00.000Z",
    );
    expect(helixDateTime("2016-12-31T23:59:60Z")).toBe(
      "2017-01-01T00:00:00.000Z",
    );
    expect(helixDateTime("2026-03-14")).toBeUndefined();
    expect(helixDateTime(" 2026-03-14T08:30:00Z")).toBeUndefined();
  });

  it("renders an instant in UTC to the millisecond, its other digits dropped", () => {
    for (const [raw, helix] of [
      ["2026-03-14T08:30:00Z", "2026-03-14T08:30:00.000Z"],
      ["2026-03-14T08:30:00.000Z", "2026-03-14T08:30:00.000Z"],
      ["2026-03-14T08:30:00.5Z", "2026-03-14T08:30:00.500Z"],
      ["2026-03-14T08:30:00.25Z", "2026-03-14T08:30:00.250Z"],
      ["2026-03-14T08:30:00.123Z", "2026-03-14T08:30:00.123Z"],
      ["2026-03-14T08:30:00.1239Z", "2026-03-14T08:30:00.123Z"],
      ["2026-03-14T08:30:00.999999Z", "2026-03-14T08:30:00.999Z"],
      ["2026-03-14T08:30:00.123456789123Z", "2026-03-14T08:30:00.123Z"],
      ["2026-03-14T08:30:00.000001-00:00", "2026-03-14T08:30:00.000Z"],
      ["2026-12-31T23:30:00-01:00", "2027-01-01T00:30:00.000Z"],
      ["2026-03-14T00:30:00+01:30", "2026-03-13T23:00:00.000Z"],
      ["2026-03-14T07:30:00−01:00", "2026-03-14T08:30:00.000Z"],
      ["2026-03-14 09:30:00+01:00", "2026-03-14T08:30:00.000Z"],
      ["2024-02-29T00:00:00Z", "2024-02-29T00:00:00.000Z"],
      ["2026-03-14T08:30:00+23:59", "2026-03-13T08:31:00.000Z"],
    ])
      expect(helixDateTime(raw!), raw).toBe(helix);
  });

  it("floors before 1970, as timestamp_millis does", () => {
    expect(helixDateTime("1969-12-31T23:59:59.9995Z")).toBe(
      "1969-12-31T23:59:59.999Z",
    );
    expect(helixDateTime("1969-12-31T23:59:59.0001Z")).toBe(
      "1969-12-31T23:59:59.000Z",
    );
    expect(helixDateTime("1900-01-01T00:00:00Z")).toBe(
      "1900-01-01T00:00:00.000Z",
    );
  });

  it("reads a leap second as the next second, its fraction kept", () => {
    expect(helixDateTime("2017-01-01T00:59:60.5+01:00")).toBe(
      "2017-01-01T00:00:00.500Z",
    );
    expect(helixDateTime("2026-03-14T08:30:60Z")).toBe(
      "2026-03-14T08:31:00.000Z",
    );
  });

  it("holds Helix's years, 0000 to 9999 in UTC, after the offset", () => {
    expect(helixDateTime("0000-01-01T00:00:00Z")).toBe(
      "0000-01-01T00:00:00.000Z",
    );
    expect(helixDateTime("0000-01-01T01:00:00+01:00")).toBe(
      "0000-01-01T00:00:00.000Z",
    );
    expect(helixDateTime("9999-12-31T23:59:59.999999Z")).toBe(
      "9999-12-31T23:59:59.999Z",
    );
    expect(helixDateTime("10000-01-01T00:00:00Z")).toBeUndefined();
    for (const past of [
      "0000-01-01T00:30:00+01:00",
      "9999-12-31T23:30:00-01:00",
      // The next second of the last one is past it.
      "9999-12-31T23:59:60Z",
    ])
      expect(helixDateTime(past), past).toBeUndefined();
  });

  it("refuses what chrono doesn't read as RFC 3339", () => {
    for (const bad of [
      "2026-03-14",
      "2026-03-14T08:30:00",
      "2026-03-14T08:30Z",
      "2026-02-30T00:00:00Z",
      "2025-02-29T00:00:00Z",
      "2026-00-01T00:00:00Z",
      "2026-13-01T00:00:00Z",
      "2026-03-00T00:00:00Z",
      "2026-03-14T24:00:00Z",
      "2026-03-14T08:60:00Z",
      "2026-03-14T08:30:61Z",
      "2026-03-14T08:30:00+24:00",
      "2026-03-14T08:30:00+01:60",
      "2026-03-14_08:30:00Z",
      "2026-03-14T08:30:00.Z",
      "2026-03-14T08:30:00+0100",
      "2026-03-14T08:30:00 Z",
      "2026-3-14T08:30:00Z",
      "+2026-03-14T08:30:00Z",
      "2026-03-14T08:30:00Z ",
      "08:30Z",
      "today",
      "",
    ])
      expect(helixDateTime(bad), bad).toBeUndefined();
  });

  it("gives what it reads back unchanged", () => {
    for (const raw of [
      "2026-03-14T08:30:00.000Z",
      "1969-07-20T20:17:40.123Z",
      "0000-01-01T00:00:00.000Z",
      "9999-12-31T23:59:59.999Z",
    ])
      expect(helixDateTime(raw)).toBe(raw);
  });
});
