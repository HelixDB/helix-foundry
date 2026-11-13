import { describe, expect, it } from "vitest";
import type { FoundryTeam, WorkspaceDirectory } from "../../src/shared/wfe";
import {
  directoryCounts,
  directoryView,
  issueGroups,
  providerName,
  stateView,
  syncText,
  teamsWithoutSources,
  withoutSourcesText,
} from "../../src/web/areas/workspace/directory-model";

const ago = () => "5m ago";
const synced = (name: string, id = name): FoundryTeam => ({
  id,
  name,
  directory: {
    directoryId: "d",
    groupId: `g_${id}`,
    provider: "Okta",
    leftOutUserIds: [],
  },
});

describe("directoryView", () => {
  it("hides the panel unless the workspace is entitled", () => {
    expect(directoryView(undefined)).toEqual({ kind: "hidden" });
    expect(directoryView({ workspaceId: "ws" })).toEqual({ kind: "hidden" });
  });
  it("offers Connect until a directory is connected", () => {
    expect(directoryView({ entitled: true })).toEqual({ kind: "none" });
  });
  it("names the provider, or your directory", () => {
    expect(
      directoryView({ entitled: true, directory: { provider: " Okta " } }),
    ).toEqual({ kind: "connected", provider: "Okta" });
    expect(directoryView({ entitled: true, directory: {} })).toEqual({
      kind: "connected",
      provider: "your directory",
    });
    expect(providerName("")).toBe("your directory");
  });
});

describe("stateView", () => {
  it("says what to do when the connection broke", () => {
    expect(stateView("DIRECTORY_STATE_LINKED")).toEqual({
      label: "Connected",
      tone: "ok",
    });
    for (const s of [
      "DIRECTORY_STATE_INVALID_CREDENTIALS",
      "DIRECTORY_STATE_UNLINKED",
    ] as const) {
      expect(stateView(s).tone).toBe("danger");
      expect(stateView(s).problem).toMatch(/Reconnect it in the setup guide/);
    }
    expect(stateView("DIRECTORY_STATE_VALIDATING").tone).toBe("pending");
    expect(stateView("DIRECTORY_STATE_DELETING").label).toBe("Being removed");
    expect(stateView(undefined).label).toBe("Unknown");
  });
  it("calls one that never linked unfinished, not broken", () => {
    expect(stateView("DIRECTORY_STATE_UNLINKED", false)).toEqual({
      label: "Setup not finished",
      tone: "pending",
      hint: "Finish connecting it in the setup guide. Its groups and people show here once it sends them.",
    });
    // Credentials that failed before it ever synced are still broken.
    expect(stateView("DIRECTORY_STATE_INVALID_CREDENTIALS", false).tone).toBe(
      "danger",
    );
  });
});

describe("syncText", () => {
  const d: WorkspaceDirectory = { entitled: true, directory: {} };
  it("says how the last sync went", () => {
    expect(
      syncText({ ...d, syncState: "DIRECTORY_SYNC_STATE_PENDING" }, ago),
    ).toEqual({ text: "Syncing…", tone: "pending" });
    expect(
      syncText(
        {
          ...d,
          syncState: "DIRECTORY_SYNC_STATE_FAILED",
          syncError: "WorkOS is down",
        },
        ago,
      ),
    ).toEqual({ text: "The last sync failed: WorkOS is down", tone: "danger" });
    expect(
      syncText({ ...d, syncState: "DIRECTORY_SYNC_STATE_FAILED" }, ago).text,
    ).toBe("The last sync failed.");
    expect(
      syncText({ ...d, lastSyncedAt: "2026-11-11T10:00:00Z" }, ago),
    ).toEqual({ text: "Synced 5m ago", tone: "ok" });
    expect(syncText(d, ago)).toEqual({
      text: "Not synced yet",
      tone: "pending",
    });
  });
  it("shows no failure for one that never linked", () => {
    const settingUp: WorkspaceDirectory = {
      entitled: true,
      directory: { state: "DIRECTORY_STATE_UNLINKED" },
    };
    // An older WFE reports it failed.
    expect(
      syncText(
        {
          ...settingUp,
          syncState: "DIRECTORY_SYNC_STATE_FAILED",
          syncError:
            "the directory is unlinked; reconnect it in the setup guide",
        },
        ago,
      ),
    ).toEqual({ text: "Not synced yet", tone: "pending" });
    expect(
      syncText({ ...settingUp, syncState: "DIRECTORY_SYNC_STATE_PENDING" }, ago)
        .text,
    ).toBe("Syncing…");
    // Once it synced, unlinking is a failure worth saying.
    expect(
      syncText(
        {
          ...settingUp,
          lastSyncedAt: "2026-11-11T10:00:00Z",
          syncState: "DIRECTORY_SYNC_STATE_FAILED",
          syncError: "the directory is unlinked",
        },
        ago,
      ).tone,
    ).toBe("danger");
  });
  it("counts people and groups", () => {
    expect(directoryCounts({ userCount: 1, groups: [{}] })).toBe(
      "1 person · 1 group",
    );
    expect(directoryCounts({ userCount: 1200, groups: [{}, {}] })).toBe(
      "1,200 people · 2 groups",
    );
    expect(directoryCounts({})).toBe("0 people · 0 groups");
  });
});

describe("issueGroups", () => {
  it("says each kind once, most pressing first, and leaves out the rest", () => {
    const groups = issueGroups(
      [
        { kind: "DIRECTORY_ISSUE_KIND_OWNER_KEPT", displayName: "Olive Owner" },
        { kind: "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED", email: "bea@x.org" },
        { kind: "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED", displayName: "Al" },
        { kind: "DIRECTORY_ISSUE_KIND_LEFT_OUT_RAW_ACCESS", displayName: "A" },
        { kind: "DIRECTORY_ISSUE_KIND_UNSPECIFIED" },
      ],
      "Okta",
    );
    expect(groups).toEqual([
      {
        kind: "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED",
        summary:
          "2 people in Okta have no Helix account yet. If this lasts, contact Helix support.",
        details: ["bea@x.org", "Al"],
      },
      {
        kind: "DIRECTORY_ISSUE_KIND_OWNER_KEPT",
        summary:
          "Olive Owner was removed in Okta but stays, because the directory never removes an owner.",
        details: [],
      },
    ]);
  });
  it("words every kind", () => {
    const one = (kind: Parameters<typeof issueGroups>[0][number]["kind"]) =>
      issueGroups([{ kind, displayName: "Ada", groupName: "Eng" }], "Okta")[0]
        ?.summary;
    expect(one("DIRECTORY_ISSUE_KIND_NO_EMAIL")).toBe(
      "1 person has no email in Okta, so they can’t join.",
    );
    expect(one("DIRECTORY_ISSUE_KIND_PENDING_INVITATION")).toBe(
      "1 person was emailed an invitation and joins once they accept it.",
    );
    expect(one("DIRECTORY_ISSUE_KIND_TOO_MANY_GROUPS")).toBe(
      "Okta has more than 500 groups, so only the first 500 by name are synced.",
    );
    expect(one("DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE")).toBe(
      "Eng has more than 10,000 people, so it syncs without members.",
    );
    expect(
      issueGroups(
        [
          { kind: "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE", groupName: "A" },
          { kind: "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE", groupName: "B" },
          { kind: "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE" },
        ],
        "your directory",
      )[0].summary,
    ).toBe(
      "A, B and A group have more than 10,000 people each, so they sync without members.",
    );
    expect(
      issueGroups(
        [
          { kind: "DIRECTORY_ISSUE_KIND_OWNER_KEPT", displayName: "Olive" },
          { kind: "DIRECTORY_ISSUE_KIND_OWNER_KEPT", email: "o@x.org" },
        ],
        "Okta",
      )[0],
    ).toEqual({
      kind: "DIRECTORY_ISSUE_KIND_OWNER_KEPT",
      summary:
        "2 owners were removed in Okta but stay, because the directory never removes an owner.",
      details: ["Olive", "o@x.org"],
    });
  });
});

describe("teamsWithoutSources", () => {
  it("lists synced teams that hold no grant", () => {
    const teams = [
      synced("Engineering"),
      synced("Finance"),
      { id: "manual", name: "Manual" },
    ];
    expect(
      teamsWithoutSources(teams, [
        { principal: { kind: "KIND_TEAM", id: "Finance" } },
        { principal: { kind: "KIND_USER", id: "Engineering" } },
      ]).map((t) => t.name),
    ).toEqual(["Engineering"]);
  });
  it("says who reads nothing yet", () => {
    expect(withoutSourcesText([synced("Engineering")], "Okta")).toBe(
      "Engineering was synced from Okta but reads nothing yet, so being on it limits no one. Choose what it reads with Sources.",
    );
    expect(
      withoutSourcesText([synced("Engineering"), synced("Finance")], "Okta"),
    ).toBe(
      "Engineering and Finance were synced from Okta but read nothing yet, so being on them limits no one. Choose what each reads with Sources.",
    );
    expect(
      withoutSourcesText(
        ["A", "B", "C", "D", "E"].map((n) => synced(n)),
        "Okta",
      ),
    ).toMatch(/^A, B and 3 more teams were synced/);
  });
});
