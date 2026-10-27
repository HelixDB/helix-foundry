import type {
  CreateWorkspaceRequest,
  InviteWorkspaceMemberRequest,
  ListWorkspaceMembersResponse,
  ListWorkspacesResponse,
  Workspace,
  WorkspaceMember,
  WorkspaceRole,
} from "../../src/shared/wfe";
import { fail, id, now, ok, type MockRoute } from "../http";
import {
  config,
  db,
  displayName,
  isReply,
  membership,
  onReset,
  type Membership,
} from "../state";

// The caller's workspaces, their members and invitations, as the WFE serves
// them, with the Helix backend's slug and invitation rules.

// Pending invitations, newest last. The WFE keeps them in WorkOS.
export const invitations: {
  id: string;
  workspaceId: string;
  email: string;
  role: WorkspaceRole;
  invitedBy: string;
}[] = [];

// Alan is a plain member of Acme: he can read it but not manage it.
// `POST /__mock/config {"alanRole": "WORKSPACE_ROLE_ADMIN"}` makes him an
// admin until the next reset, for what admins (not owners) see: role changes
// stay in the dashboard, so the mock has no route for one.
export const MEMBER_EMAIL = "alan@example.com";
const knobs = config as typeof config & { alanRole?: WorkspaceRole };
onReset(() => {
  invitations.length = 0;
  delete knobs.alanRole;
  const at = "2026-10-02T09:00:00.000Z";
  db.users.set("usr_alan", {
    id: "usr_alan",
    workosId: "user_01ALAN",
    email: MEMBER_EMAIL,
    firstName: "Alan",
    lastName: "Turing",
    createdAt: at,
  });
  db.memberships.push({
    id: "mem_alan_acme",
    workspaceId: "ws_acme",
    userId: "usr_alan",
    get role(): WorkspaceRole {
      return knobs.alanRole || "WORKSPACE_ROLE_MEMBER";
    },
    createdAt: at,
  });
});

/** Owners and admins hold workspace.members.manage and workspace.billing.manage. */
export const manages = (m: Membership) =>
  m.role === "WORKSPACE_ROLE_OWNER" || m.role === "WORKSPACE_ROLE_ADMIN";

const ROLES = new Set<WorkspaceRole>([
  "WORKSPACE_ROLE_OWNER",
  "WORKSPACE_ROLE_ADMIN",
  "WORKSPACE_ROLE_MEMBER",
]);
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

function member(m: Membership): WorkspaceMember {
  const user = db.users.get(m.userId)!;
  return {
    id: m.id,
    workspaceId: m.workspaceId,
    role: m.role,
    user: {
      id: user.id,
      email: user.email,
      displayName: displayName(user),
      timestamps: { createdAt: user.createdAt, updatedAt: user.createdAt },
    },
    timestamps: { createdAt: m.createdAt, updatedAt: m.createdAt },
  };
}

const RESERVED = new Set([
  "access",
  "account",
  "api",
  "auth",
  "billing-success",
  "blog",
  "content",
  "create-workspace",
  "explorer",
  "invitations",
  "invited",
  "login",
  "papi",
  "personal",
  "privacy",
  "terms",
  "visualizer",
]);

function slugProblem(slug: string): string | null {
  if (!slug) return "is required";
  if (slug.length > 64) return "must be at most 64 characters";
  if (RESERVED.has(slug)) return `"${slug}" is reserved`;
  return /^[a-z0-9][a-z0-9_-]*$/.test(slug)
    ? null
    : "must start with a lowercase letter or number and contain only lowercase letters, numbers, hyphens, or underscores";
}

export const workspaceRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/workspaces",
    handle: ({ query, principal: { user } }) => {
      // Pages of page.size (50 when unset or over 200, as the backend); the
      // token is an offset here, opaque to the app.
      const rawSize = query.get("page.size") || "0",
        rawToken = query.get("page.token") || "0";
      if (!/^\d+$/.test(rawSize) || !/^\d+$/.test(rawToken))
        return fail("INVALID_ARGUMENT", "invalid page");
      const size =
          Number(rawSize) > 0 && Number(rawSize) <= 200 ? Number(rawSize) : 50,
        start = Number(rawToken);
      const mine = new Set(
        db.memberships
          .filter((m) => m.userId === user.id)
          .map((m) => m.workspaceId),
      );
      const all = [...db.workspaces.values()].filter((w) => mine.has(w.id));
      return ok({
        workspaces: all.slice(start, start + size),
        nextPageToken:
          start + size < all.length ? String(start + size) : undefined,
      } satisfies ListWorkspacesResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces",
    handle: ({ body, principal: { user } }) => {
      const req = (body || {}) as Partial<CreateWorkspaceRequest>;
      const slug = (req.slug || "").trim().toLowerCase();
      const problem = slugProblem(slug);
      if (problem) return fail("INVALID_ARGUMENT", `slug: ${problem}`);
      const name = req.displayName?.trim();
      if (req.displayName !== undefined && !name)
        return fail("INVALID_ARGUMENT", "display_name: is required");
      if (!req.region) return fail("INVALID_ARGUMENT", "region is required");
      if (!config.allowedRegions.includes(req.region))
        return fail(
          "INVALID_ARGUMENT",
          `region "${req.region}" is not enabled`,
        );
      if ([...db.workspaces.values()].some((w) => w.slug === slug))
        return fail("ALREADY_EXISTS", `workspace "${slug}" already exists`);
      const at = now();
      const workspace: Workspace = {
        id: id("ws"),
        slug,
        displayName: name || slug,
        region: req.region,
        status: "RESOURCE_STATUS_ACTIVE",
        timestamps: { createdAt: at, updatedAt: at },
      };
      db.workspaces.set(workspace.id, workspace);
      db.memberships.push({
        id: id("mem"),
        workspaceId: workspace.id,
        userId: user.id,
        role: "WORKSPACE_ROLE_OWNER",
        createdAt: at,
      });
      return ok(workspace);
    },
  },
  {
    method: "GET",
    path: "/v1/workspaces/{id}",
    handle: ({ params, principal: { user } }) => {
      const m = membership(params.id!, user.id);
      return isReply(m) ? m : ok(db.workspaces.get(m.workspaceId));
    },
  },
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/members",
    handle: ({ params, query, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      // Pages of page.size (default 50, at most 200), as the backend pages;
      // the token is an offset here, opaque to the app.
      const rawSize = query.get("page.size") || "0",
        rawToken = query.get("page.token") || "0";
      if (!/^\d+$/.test(rawSize) || !/^\d+$/.test(rawToken))
        return fail("INVALID_ARGUMENT", "invalid page");
      const size = Math.min(Number(rawSize) || 50, 200),
        start = Number(rawToken);
      const all = db.memberships.filter((x) => x.workspaceId === m.workspaceId);
      const page = all.slice(start, start + size);
      return ok({
        members: page.map(member),
        nextPageToken:
          start + size < all.length ? String(start + size) : undefined,
      } satisfies ListWorkspaceMembersResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/members:invite",
    handle: ({ params, body, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      const req = (body || {}) as Partial<InviteWorkspaceMemberRequest>;
      const email = (req.email || "").trim().toLowerCase();
      if (!EMAIL.test(email))
        return fail("INVALID_ARGUMENT", "a valid email is required");
      if (!req.role || !ROLES.has(req.role))
        return fail("INVALID_ARGUMENT", "role is required");
      if (
        req.role === "WORKSPACE_ROLE_OWNER" &&
        m.role !== "WORKSPACE_ROLE_OWNER"
      )
        return fail(
          "PERMISSION_DENIED",
          "workspace ownership management permission required",
        );
      const workspaceId = m.workspaceId;
      if (
        db.memberships.some(
          (x) =>
            x.workspaceId === workspaceId &&
            db.users.get(x.userId)?.email === email,
        )
      )
        return fail(
          "ALREADY_EXISTS",
          `${email} is already a member of this workspace`,
        );
      if (
        invitations.some(
          (i) => i.workspaceId === workspaceId && i.email === email,
        )
      )
        return fail(
          "ALREADY_EXISTS",
          `a pending invitation already exists for ${email}`,
        );
      invitations.push({
        id: id("inv"),
        workspaceId,
        email,
        role: req.role,
        invitedBy: user.id,
      });
      return ok({});
    },
  },
];
