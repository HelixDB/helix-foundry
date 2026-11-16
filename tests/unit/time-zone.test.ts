import { describe, expect, it } from "vitest";
import {
  isTimeZone,
  zoneCity,
  zoneLabel,
  zoneOffset,
  zoneTime,
} from "@/shared/time-zone";
import { answerText, leftText, optionText } from "@/web/questions/model";

// Dates fixed on either side of daylight saving, so nothing depends on
// when the tests run.
const WINTER = new Date("2026-01-15T12:00:00Z"),
  SUMMER = new Date("2026-07-15T12:00:00Z");

describe("time zones in words", () => {
  it("knows the runtime's zones and UTC, nothing else", () => {
    expect(isTimeZone("America/New_York")).toBe(true);
    expect(isTimeZone("UTC")).toBe(true);
    for (const x of ["New York", "Mars/Olympus", "Local", "", "utc"])
      expect(isTimeZone(x), x).toBe(false);
  });

  it("says a zone's city, underscores as spaces", () => {
    expect(zoneCity("America/New_York")).toBe("New York");
    expect(zoneCity("America/Argentina/Buenos_Aires")).toBe("Buenos Aires");
    expect(zoneCity("UTC")).toBe("UTC");
  });

  it("says its offset at the date given, with a true minus sign", () => {
    expect(zoneOffset("America/New_York", WINTER)).toBe("GMT−5");
    expect(zoneOffset("America/New_York", SUMMER)).toBe("GMT−4");
    expect(zoneOffset("Asia/Kolkata", WINTER)).toBe("GMT+5:30");
    expect(zoneOffset("Europe/London", WINTER)).toBe("GMT");
  });

  it("labels a zone id by its city and offset; anything else is no zone", () => {
    expect(zoneLabel("America/New_York", WINTER)).toBe("New York · GMT−5");
    expect(zoneLabel("America/New_York", SUMMER)).toBe("New York · GMT−4");
    expect(zoneLabel("Europe/London", SUMMER)).toBe("London · GMT+1");
    expect(zoneLabel("America/Los_Angeles", WINTER)).toBe(
      "Los Angeles · GMT−8",
    );
    expect(zoneLabel("UTC", WINTER)).toBe("UTC");
    expect(zoneLabel("London (UK time)")).toBeUndefined();
    expect(zoneLabel("Weekly")).toBeUndefined();
  });

  it("says a time's zone as people say it", () => {
    expect(zoneTime("Europe/London")).toBe("London time");
    expect(zoneTime("America/New_York")).toBe("New York time");
    expect(zoneTime("UTC")).toBe("UTC");
  });
});

describe("question options", () => {
  it("shows a zone id in words and anything else as written", () => {
    expect(optionText("America/New_York")).toMatch(/^New York · GMT−[45]$/);
    expect(optionText("UTC")).toBe("UTC");
    expect(optionText("The company that pays")).toBe("The company that pays");
  });

  it("says chosen and recommended zones in words", () => {
    expect(answerText({ options: ["Europe/Berlin", "UTC"] })).toMatch(
      /^Berlin · GMT\+[12] and UTC$/,
    );
    expect(
      leftText({
        id: "q1",
        header: "",
        text: "Which?",
        items: [],
        blocking: false,
        ask: {
          kind: "choice",
          multiple: false,
          options: [
            { label: "UTC", description: "" },
            { label: "Europe/London", description: "" },
          ],
          recommended: "UTC",
        },
      }),
    ).toBe("Left to the agent: it recommends UTC");
  });
});
