import { describe, expect, it } from "vitest";
import type { FoundryEnrichment } from "@/shared/wfe";
import { shortDate, utcDay } from "@/web/areas/ontology/model";
import {
  enrichmentCredits,
  enrichmentLoadText,
  enrichmentQuery,
  enrichmentStatusText,
  planUnknown,
} from "@/web/areas/workspace/enrichment-api";

// Settings › People enrichment's words, and the load dialog's row
//.

const on: FoundryEnrichment = {
  availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE",
  enabled: true,
  monthlyCreditCap: "50000",
  creditsUsed: "8460",
  creditsReserved: "0",
  capResetsAt: "2026-11-01T00:00:00Z",
  enabledBy: "usr_ada",
  enabledAt: "2026-10-02T09:00:00Z",
};
const NOV_1 = utcDay(new Date(Date.UTC(2026, 10, 1)));

describe("the month's credits", () => {
  it("are what the cap leaves after charges and reservations, never below 0", () => {
    expect(enrichmentCredits({ ...on, creditsReserved: "12240" })).toEqual({
      cap: 50000,
      used: 8460,
      reserved: 12240,
      left: 29300,
    });
    expect(
      enrichmentCredits({ monthlyCreditCap: "20", creditsUsed: "40" }),
    ).toEqual({ cap: 20, used: 40, reserved: 0, left: 0 });
    expect(enrichmentCredits({})).toEqual({
      cap: 0,
      used: 0,
      reserved: 0,
      left: 0,
    });
  });
});

describe("the load dialog's row", () => {
  it("says what a load may spend", () => {
    expect(enrichmentLoadText(on)).toBe(
      "Looks up people earlier loads haven’t: 20 credits per profile found, 41,540 of 50,000 left this month. The first load takes longer and re-reads documents that mention people.",
    );
  });
  it("applies what was found before when no credits are left", () => {
    expect(enrichmentLoadText({ ...on, creditsUsed: "49990" })).toBe(
      `0 credits left this month: applies profiles found before. The cap resets on ${NOV_1}.`,
    );
    expect(
      enrichmentLoadText({ ...on, creditsUsed: "50000", capResetsAt: null }),
    ).toBe("0 credits left this month: applies profiles found before.");
  });
  it("is off without consent, before anything else", () => {
    expect(
      enrichmentLoadText({
        ...on,
        enabled: false,
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
      }),
    ).toBe("Off for this workspace: this load applies no profiles.");
    expect(enrichmentLoadText({})).toBe(
      "Off for this workspace: this load applies no profiles.",
    );
  });
  it("needs Pro for new lookups, and says when Helix pauses them", () => {
    expect(
      enrichmentLoadText({
        ...on,
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
      }),
    ).toBe(
      "Needs Foundry Pro for new lookups; profiles found before still apply.",
    );
    expect(
      enrichmentLoadText({
        ...on,
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
      }),
    ).toBe("Lookups are paused by Helix; profiles found before still apply.");
  });
  it("can't say whether lookups run while Helix can't read the plan", () => {
    const unknown =
      "Helix can’t read this workspace’s plan just now, so it can’t say whether this load looks people up; profiles found before still apply.";
    expect(
      enrichmentLoadText({
        ...on,
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED",
      }),
    ).toBe(unknown);
    // The WFE leaves the default value out.
    expect(enrichmentLoadText({ ...on, availability: undefined })).toBe(
      unknown,
    );
    expect(enrichmentLoadText({ enabled: false })).toBe(
      "Off for this workspace: this load applies no profiles.",
    );
  });
});

describe("the plan's availability", () => {
  it("is unknown while UNSPECIFIED or unset", () => {
    expect(
      planUnknown({
        availability: "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED",
      }),
    ).toBe(true);
    expect(planUnknown({})).toBe(true);
    for (const availability of [
      "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE",
      "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED",
      "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED",
    ] as const)
      expect(planUnknown({ availability })).toBe(false);
  });
});

describe("the setting in a line", () => {
  it("tells the owner the month's credits and who allowed it", () => {
    expect(
      enrichmentStatusText(
        { ...on, creditsReserved: "12240" },
        true,
        "Ada Lovelace",
      ),
    ).toBe(
      `8,460 of 50,000 credits used this month · 12,240 reserved by a running load · resets ${NOV_1} · Allowed by Ada Lovelace on ${shortDate(on.enabledAt)}`,
    );
    expect(
      enrichmentStatusText(
        { ...on, capResetsAt: undefined, enabledAt: null },
        true,
      ),
    ).toBe("8,460 of 50,000 credits used this month");
    expect(enrichmentStatusText({ ...on, enabled: false }, true)).toBe("");
  });
  it("tells admins and members whether it is on, and that only owners change it", () => {
    expect(enrichmentStatusText(on, false)).toBe(
      "On · cap 50,000 · 8,460 used. Only owners can change this.",
    );
    expect(enrichmentStatusText({ enabled: false }, false)).toBe(
      "Off. Only owners can change this.",
    );
  });
});

describe("the setting's query", () => {
  it("polls only while enriched values are being removed", () => {
    const q = enrichmentQuery("ws_1");
    expect(q.queryKey).toEqual(["ws_1", "foundry-enrichment"]);
    expect(q.refetchInterval({ state: { data: { purgesPending: 2 } } })).toBe(
      5000,
    );
    expect(q.refetchInterval({ state: { data: on } })).toBe(false);
    expect(q.refetchInterval({ state: {} })).toBe(false);
  });
});
