import type {
  CreateWorkspaceRequest,
  ListWorkspacesResponse,
  WhoamiResponse,
  Workspace,
  WorkspaceRole as WfeRole,
} from "../shared/wfe";
import { ApiError, api } from "./api";
import { readStorage, writeStorage } from "./kit";
import type { ShellUser, ShellWorkspace, WorkspaceRole } from "./ui";

// The signed-in user and their workspaces: GET /v1/whoami (memberships and
// roles) joined with GET /v1/workspaces (names). The open workspace is
// remembered per browser, as in helix-foundry.

const KEY = "foundry.workspace";

export const whoamiQuery = {
  queryKey: ["whoami"],
  queryFn: () =>
    api<WhoamiResponse>("/v1/whoami", undefined, "GET", undefined, {
      signIn: false,
    }),
  retry: false,
} as const;
// Every page of them: the WFE answers at most 200 a page, newest first.
export const workspacesQuery = {
  queryKey: ["workspaces"],
  queryFn: async (): Promise<ListWorkspacesResponse> => {
    const workspaces: Workspace[] = [];
    let token = "";
    do {
      const q = new URLSearchParams({ "page.size": "200" });
      if (token) q.set("page.token", token);
      const page = await api<ListWorkspacesResponse>(`/v1/workspaces?${q}`);
      workspaces.push(...(page.workspaces || []));
      token = page.nextPageToken || "";
    } while (token);
    return { workspaces };
  },
} as const;

export const shellRoles: Record<WfeRole, WorkspaceRole> = {
  WORKSPACE_ROLE_OWNER: "owner",
  WORKSPACE_ROLE_ADMIN: "admin",
  WORKSPACE_ROLE_MEMBER: "member",
  WORKSPACE_ROLE_UNSPECIFIED: "member",
};

export function shellUser({ user }: WhoamiResponse): ShellUser {
  const email = user.email || "";
  return { id: user.id, email, name: user.displayName || email || "You" };
}

/** The user's workspaces with their role in each, in the WFE's order. */
export function shellWorkspaces(
  me: WhoamiResponse,
  list: Workspace[],
): ShellWorkspace[] {
  const role = new Map(
    (me.workspaces || []).map((m) => [m.workspaceId, m.role]),
  );
  return list
    .filter((w) => w.status !== "RESOURCE_STATUS_DELETED")
    .map((w) => ({
      id: w.id,
      slug: w.slug || w.id,
      name: w.displayName || w.slug || w.id,
      role:
        shellRoles[role.get(w.id) || "WORKSPACE_ROLE_UNSPECIFIED"] ?? "member",
    }));
}

export const chosenWorkspace = () => readStorage(KEY);

/**
 * Opens a workspace. Switching is client-side today: a Foundry session works
 * on every workspace the user belongs to. When sessions become scoped to one
 * workspace, the token swap goes here,
 * before the new workspace is remembered.
 */
export function chooseWorkspace(id: string) {
  writeStorage(KEY, id);
}

/** A workspace slug from its name: "Acme Inc." becomes "acme-inc". */
export function slugify(name: string) {
  return name
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64)
    .replace(/-+$/, "");
}

/**
 * Creates a workspace in the stamp's default region. Its slug comes from the
 * name and is unique across Helix, so when it is taken (ALREADY_EXISTS) or
 * reserved, the workspace is created once more with a short random suffix:
 * "acme" becomes "acme-7k2f", and the name stays as typed.
 */
export async function createWorkspace(name: string) {
  const create = (slug: string) =>
    api<Workspace>("/v1/workspaces", {
      slug,
      displayName: name.trim(),
      region: process.env.NEXT_PUBLIC_DEFAULT_WORKSPACE_REGION || "us-east-2",
    } satisfies CreateWorkspaceRequest);
  const slug = slugify(name);
  try {
    return await create(slug);
  } catch (e) {
    const unusable =
      e instanceof ApiError &&
      (e.status === 409 || (e.status === 400 && /is reserved/.test(e.message)));
    if (!unusable) throw e;
    const suffix = Array.from(
      crypto.getRandomValues(new Uint8Array(4)),
      (b) => "abcdefghijklmnopqrstuvwxyz0123456789"[b % 36],
    ).join("");
    return create(`${slug.slice(0, 59).replace(/[-_]+$/, "")}-${suffix}`);
  }
}
