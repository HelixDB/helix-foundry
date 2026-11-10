import { describe, expect, it } from "vitest";
import {
  INSTALL_KEY,
  INSTALL_WAIT_MS,
  inWorkspace,
  installAction,
  installReason,
  needsGitHubInstall,
  rememberInstall,
  takeInstall,
} from "@/web/areas/data/github-install";

// The install step of a GitHub connector that signed in: knowing the
// backend's install problems, remembering the install across GitHub's
// install page, and reading where GitHub's Setup URL says it came from.

/** A Storage over a Map, as the browser's localStorage behaves. */
function memoryStorage(): Storage {
  const m = new Map<string, string>();
  return {
    get length() {
      return m.size;
    },
    clear: () => m.clear(),
    getItem: (k) => m.get(k) ?? null,
    key: (i) => [...m.keys()][i] ?? null,
    removeItem: (k) => void m.delete(k),
    setItem: (k, v) => void m.set(k, String(v)),
  };
}

// The backend's words (backend/connector/github.go), pinned there by
// connector/sign_in_test.go.
const NONE =
  "Helix Foundry can't see any repository this GitHub sign-in reads: install Helix Foundry's GitHub App on an account or organization (https://github.com/apps/helix-foundry/installations/new), then Sync now";
const NAMED =
  "Helix Foundry can't see the GitHub repository initech/ledger: install Helix Foundry's GitHub App on initech (https://github.com/apps/helix-foundry/installations/new), then Sync now";

describe("needsGitHubInstall", () => {
  it("knows the backend's install problems", () => {
    expect(needsGitHubInstall(NONE)).toBe(true);
    expect(needsGitHubInstall(NAMED)).toBe(true);
    // As a page shows it, with a typographic apostrophe.
    expect(needsGitHubInstall(NAMED.replace("can't", "can’t"))).toBe(true);
  });
  it("leaves other problems alone", () => {
    expect(
      needsGitHubInstall(
        "Helix Foundry is installed on more than 100 repositories this sign-in reads; list the ones to sync",
      ),
    ).toBe(false);
    expect(needsGitHubInstall("GitHub rejected this token")).toBe(false);
    expect(needsGitHubInstall("")).toBe(false);
    expect(needsGitHubInstall(undefined)).toBe(false);
  });
});

describe("installReason", () => {
  it("keeps what the app can't see, without the backend's directions", () => {
    expect(installReason(NONE)).toBe(
      "Helix Foundry can't see any repository this GitHub sign-in reads",
    );
    expect(installReason(NAMED)).toBe(
      "Helix Foundry can't see the GitHub repository initech/ledger",
    );
    expect(installReason("Helix Foundry can't see acme/web.")).toBe(
      "Helix Foundry can't see acme/web",
    );
  });
});

describe("a remembered install", () => {
  const install = {
    workspaceId: "ws_1",
    connectorId: "conn_1",
    to: "/ontology/new?step=sources&connected=conn_1&workspace=ws_1",
    state: { picks: { connectorIds: ["conn_1"] } },
  };

  it("is taken once", () => {
    const storage = memoryStorage();
    rememberInstall(storage, install, 1_000);
    expect(takeInstall(storage, 2_000)).toEqual({ ...install, at: 1_000 });
    expect(takeInstall(storage, 2_000)).toBeNull();
  });

  it("is none once it waited too long, or from the future", () => {
    const storage = memoryStorage();
    rememberInstall(storage, install, 1_000);
    expect(takeInstall(storage, 1_000 + INSTALL_WAIT_MS + 1)).toBeNull();
    rememberInstall(storage, install, 5_000);
    expect(takeInstall(storage, 1_000)).toBeNull();
  });

  it("goes back only to this app's paths", () => {
    const storage = memoryStorage();
    for (const to of ["https://evil.example/", "//evil.example/x", "data"]) {
      rememberInstall(storage, { ...install, to }, 1_000);
      expect(takeInstall(storage, 2_000)).toBeNull();
    }
  });

  it("is none when malformed, or without storage", () => {
    const storage = memoryStorage();
    storage.setItem(INSTALL_KEY, "{");
    expect(takeInstall(storage)).toBeNull();
    storage.setItem(
      INSTALL_KEY,
      JSON.stringify({ ...install, connectorId: "" }),
    );
    expect(takeInstall(storage)).toBeNull();
    expect(takeInstall(null)).toBeNull();
    expect(() => rememberInstall(null, install)).not.toThrow();
  });

  it("drops state that isn't an object", () => {
    const storage = memoryStorage();
    rememberInstall(storage, { ...install, state: [1] as never }, 1_000);
    expect(takeInstall(storage, 2_000)).not.toHaveProperty("state");
  });
});

describe("inWorkspace", () => {
  it("names the workspace, keeping the path's own query", () => {
    expect(inWorkspace("/data/connections/conn_1", "ws_1")).toBe(
      "/data/connections/conn_1?workspace=ws_1",
    );
    expect(
      inWorkspace("/ontology/new?step=sources&workspace=old", "ws_1"),
    ).toBe("/ontology/new?step=sources&workspace=ws_1");
  });
});

describe("installAction", () => {
  it("reads GitHub's setup_action, an install when it says nothing else", () => {
    const action = (q: string) => installAction(new URLSearchParams(q));
    expect(action("installation_id=1&setup_action=install")).toBe("install");
    expect(action("installation_id=1&setup_action=update")).toBe("update");
    expect(action("setup_action=request")).toBe("request");
    expect(action("")).toBe("install");
    expect(action("setup_action=other")).toBe("install");
  });
});
