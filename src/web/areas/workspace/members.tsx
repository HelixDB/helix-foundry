import { useState } from "react";
import { infiniteQueryOptions, useInfiniteQuery } from "@tanstack/react-query";
import { UserPlus, Users } from "lucide-react";
import type {
  InviteWorkspaceMemberRequest,
  ListWorkspaceMembersResponse,
  WorkspaceMember,
  WorkspaceRole as WfeRole,
} from "../../../shared/wfe";
import { api } from "../../api";
import {
  Button,
  Dialog,
  EmptyState,
  Panel,
  Select,
  SkeletonText,
  Tag,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { shellRoles } from "../../workspaces";
import { initials, roleLabel } from "./people";
import { canManage } from "./plan";

// Settings → Members: who is in the workspace, read-only, and inviting by
// email. Role changes and removals stay in the Helix dashboard; the WFE sends
// the invitation from WorkOS and it lands on the dashboard to accept.

const PAGE_SIZE = 50;
/** The workspace's members, a page at a time. */
export const membersQuery = (workspaceId: string) =>
  infiniteQueryOptions({
    queryKey: [workspaceId, "members"],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api<ListWorkspaceMembersResponse>(
        `/v1/workspaces/${encodeURIComponent(workspaceId)}/members?page.size=${PAGE_SIZE}` +
          (pageParam ? "&page.token=" + encodeURIComponent(pageParam) : ""),
      ),
    getNextPageParam: (last) => last.nextPageToken || undefined,
  });

// A role this build does not know reads as the least privileged one.
const roleOf = (m: WorkspaceMember) =>
  shellRoles[m.role || "WORKSPACE_ROLE_UNSPECIFIED"] ?? "member";
// The Helix dashboard of this stamp (next.config.ts).
export const DASHBOARD_URL =
  process.env.NEXT_PUBLIC_HELIX_DASHBOARD_URL || "https://dev.app.helix-db.com";
const rank = { owner: 0, admin: 1, member: 2 };

// Roles Foundry invites as. Inviting an owner is an ownership change, which
// stays in the dashboard with the other role changes.
const inviteRoles: { value: WfeRole; label: string; hint: string }[] = [
  {
    value: "WORKSPACE_ROLE_MEMBER",
    label: "Member",
    hint: "Sees everything in the workspace.",
  },
  {
    value: "WORKSPACE_ROLE_ADMIN",
    label: "Admin",
    hint: "Also connects data, invites people and manages billing.",
  },
];

function InviteDialog({ onClose }: { onClose: () => void }) {
  const { id, name, notify } = useWorkspace();
  const [email, setEmail] = useState(""),
    [role, setRole] = useState<WfeRole>("WORKSPACE_ROLE_MEMBER"),
    [error, setError] = useState(""),
    [pending, setPending] = useState(false);
  return (
    <Dialog
      title={`Invite to ${name}`}
      description="They’ll get an email from Helix with a link to join."
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setPending(true);
          setError("");
          const to = email.trim();
          try {
            await api(
              `/v1/workspaces/${encodeURIComponent(id)}/members:invite`,
              {
                email: to,
                role,
              } satisfies InviteWorkspaceMemberRequest,
            );
            notify(`Invitation sent to ${to}`);
            onClose();
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label className="hf-field">
          <span>Email</span>
          <input
            type="email"
            required
            data-autofocus
            autoComplete="off"
            placeholder="name@company.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
        <label className="hf-field">
          <span>Role</span>
          <Select
            label="Role"
            value={role}
            onChange={setRole}
            options={inviteRoles.map(({ value, label }) => ({ value, label }))}
          />
          <small>{inviteRoles.find((r) => r.value === role)?.hint}</small>
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!email.trim()}
          >
            Send invitation
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function MemberRow({ member, me }: { member: WorkspaceMember; me: string }) {
  const user = member.user,
    email = user?.email || "",
    display = user?.displayName || email || "Unknown user",
    role = roleOf(member),
    joined = member.timestamps?.createdAt;
  return (
    <li className="wk-member">
      <span className="hf-avatar" aria-hidden>
        {initials(display)}
      </span>
      <div className="wk-member-text">
        <strong>
          {display}
          {user?.id === me && <Tag>You</Tag>}
        </strong>
        {email && email !== display && <small>{email}</small>}
      </div>
      {joined && (
        <small className="wk-member-joined">
          Joined {formatDate(joined, false)}
        </small>
      )}
      <Tag tone={role === "member" ? "neutral" : "accent"}>
        {roleLabel[role]}
      </Tag>
    </li>
  );
}

export function MembersTab() {
  const { id, name, role, user } = useWorkspace(),
    [inviting, setInviting] = useState(false);
  const members = useInfiniteQuery(membersQuery(id));
  // Owners first, then admins, then members, each by name; pages load in the
  // WFE's order, so a later page can add to any group.
  const list = (members.data?.pages || [])
    .flatMap((p) => p.members || [])
    .sort(
      (a, b) =>
        rank[roleOf(a)] - rank[roleOf(b)] ||
        (a.user?.displayName || a.user?.email || "").localeCompare(
          b.user?.displayName || b.user?.email || "",
        ),
    );
  const manager = canManage(role);
  return (
    <>
      <Panel
        title="Members"
        subtitle={`People in ${name} share its data, ontology and plan.`}
        actions={
          manager && (
            <Button
              variant="primary"
              size="sm"
              icon={UserPlus}
              onClick={() => setInviting(true)}
            >
              Invite people
            </Button>
          )
        }
        padded={false}
        footer={
          members.hasNextPage && (
            <Button
              size="sm"
              variant="ghost"
              pending={members.isFetchingNextPage}
              onClick={() => void members.fetchNextPage()}
            >
              Show more
            </Button>
          )
        }
      >
        {members.isPending ? (
          <div className="wk-pad">
            <SkeletonText lines={3} />
          </div>
        ) : members.error ? (
          <div className="wk-pad">
            <p role="alert" className="hf-form-error">
              {members.error.message}
            </p>
            <Button size="sm" onClick={() => void members.refetch()}>
              Retry
            </Button>
          </div>
        ) : list.length ? (
          <ul className="wk-members" aria-label="Members">
            {list.map((m, i) => (
              <MemberRow
                key={m.id || m.user?.id || i}
                member={m}
                me={user.id}
              />
            ))}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Users}
            title="No members yet"
            text="Invite the people you work with."
          />
        )}
      </Panel>
      <p className="wk-note">
        {!manager && "Only owners and admins can invite people. "}
        Roles are changed and people removed in the{" "}
        <a href={DASHBOARD_URL} target="_blank" rel="noreferrer">
          Helix dashboard
        </a>
        .
      </p>
      {inviting && <InviteDialog onClose={() => setInviting(false)} />}
    </>
  );
}
