import { isRecord } from "../../../shared/json";

// Installing Helix Foundry's GitHub App: the step a GitHub connector that
// signed in needs before it reads anything, since GitHub chooses an app's
// repositories when it is installed, never when someone signs in. Foundry
// sends the user to the app's install page, remembering in this browser the
// connection it is for and where they were; the app's Setup URL
// (INSTALL_RETURN_PATH) brings them back, and that connection syncs.

/** Where Helix Foundry's GitHub App's Setup URL points, on each stamp's site. */
export const INSTALL_RETURN_PATH = "/connect/github";
export const INSTALL_KEY = "foundry.github.install.v1";
/** How long a remembered install waits for GitHub to come back. */
export const INSTALL_WAIT_MS = 60 * 60_000;

// The backend opens every message that says to install the app with these
// words (backend/connector/github.go, installWhere), and the mock's match.
const INSTALL_PROBLEM = /Helix Foundry can[’']t see /;

/**
 * Whether Foundry's account of why a signed-in GitHub connector can't read
 * (a sign-in's discovery problem, a sync's error) is that the app isn't
 * installed where its repositories are.
 */
export const needsGitHubInstall = (text?: string) =>
  !!text && INSTALL_PROBLEM.test(text);

/**
 * What the app can't see, without the backend's own directions to install
 * it (its link, "then Sync now"), which the install step's button replaces.
 */
export function installReason(problem: string) {
  const at = problem.search(/: install /);
  return (at < 0 ? problem : problem.slice(0, at)).trim().replace(/[.]$/, "");
}

/** A connection's install the user left for GitHub to make. */
export type PendingInstall = {
  workspaceId: string;
  connectorId: string;
  // Where to go back to, an app path (with ?workspace=), and its router
  // state: the connection's page, or the wizard or Generate that started
  // the sign-in, with its picks.
  to: string;
  state?: Record<string, unknown>;
  at: number;
};

/** Where an install is remembered: this browser's, across its tabs. */
export function installStorage(): Storage | null {
  try {
    return localStorage;
  } catch {
    return null;
  }
}

/** `path` in `workspaceId`, which the app opens when it isn't the current one. */
export function inWorkspace(path: string, workspaceId: string) {
  const url = new URL(path, "https://foundry.invalid");
  url.searchParams.set("workspace", workspaceId);
  return url.pathname + url.search + url.hash;
}

// Only a path of this app: never another origin's.
const appPath = (v: unknown): v is string =>
  typeof v === "string" && v.startsWith("/") && !v.startsWith("//");

/** Remembers the install about to be made, replacing any earlier one. */
export function rememberInstall(
  storage: Storage | null,
  install: Omit<PendingInstall, "at">,
  now = Date.now(),
) {
  try {
    storage?.setItem(INSTALL_KEY, JSON.stringify({ ...install, at: now }));
  } catch {
    // Without storage, GitHub's return lands on the page that says what next.
  }
}

/**
 * The install this browser left for, once: it is forgotten as it is read,
 * and one older than INSTALL_WAIT_MS, or malformed, is none.
 */
export function takeInstall(
  storage: Storage | null,
  now = Date.now(),
): PendingInstall | null {
  let raw: string | null = null;
  try {
    raw = storage?.getItem(INSTALL_KEY) ?? null;
    storage?.removeItem(INSTALL_KEY);
  } catch {
    return null;
  }
  try {
    const v: unknown = JSON.parse(raw || "null");
    if (!v || typeof v !== "object") return null;
    const e = v as Record<string, unknown>;
    const { workspaceId, connectorId, to, at } = e;
    if (
      typeof workspaceId !== "string" ||
      !workspaceId ||
      typeof connectorId !== "string" ||
      !connectorId ||
      !appPath(to) ||
      typeof at !== "number" ||
      now - at > INSTALL_WAIT_MS ||
      at > now
    )
      return null;
    const state = isRecord(e.state) ? e.state : undefined;
    return { workspaceId, connectorId, to, at, ...(state ? { state } : {}) };
  } catch {
    return null;
  }
}

/**
 * What GitHub says it did (its Setup URL's setup_action): installed the
 * app, changed an installation's repositories, or, for an organization
 * member who isn't an owner, asked its owners to install it.
 */
export type InstallAction = "install" | "update" | "request";

export function installAction(search: URLSearchParams): InstallAction {
  const action = search.get("setup_action");
  return action === "update" || action === "request" ? action : "install";
}
