import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Bot, Copy, KeyRound, Plus, Shapes, Users } from "lucide-react";
import type {
  CreateServiceCredentialResponse,
  FoundryPrincipal,
  FoundryTeam,
  ServiceCredential,
  WorkspaceMember,
} from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  Dialog,
  EmptyState,
  Panel,
  SkeletonText,
  SourceLogo,
  TabPanel,
  Tabs,
  Tag,
  connectorLabel,
  formatRelative,
  plural,
  useTabParam,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { shellRoles } from "../../workspaces";
import { connectorsQuery } from "../data/data-api";
import { documentSetQueries } from "../documents/documents-api";
import { setLabel } from "../documents/model";
import { ontologyQueries } from "../ontology/queries";
import {
  accessQuery,
  createKey,
  createTeam,
  deleteTeam,
  keysQuery,
  revokeKey,
  setGrants,
  setRestricted,
  updateTeam,
} from "./access-api";
import {
  grantKey,
  grantsOf,
  heldGrants,
  missingText,
  principalKey,
  readings,
  readsText,
  holdsRawAccess,
  newlyRestricted,
  restrictable,
  setOwners,
  sourceState,
  sourceTree,
  targetsOf,
  toggle,
  type Grant,
  type Principal,
} from "./access-model";
import { membersQuery } from "./members";
import { initials, roleLabel } from "./people";
import { canManage } from "./plan";

// Settings › Access (hosted, owners and admins): what agents, people and
// teams may read through source grants. A grant gives a connection (its
// tables now and later, and the sets its syncs write), one table, a
// document set, or People enrichment; whoever it is for reads the part of an
// ontology's current load those sources hold, and asks it questions, all of it
// once the load reads nothing else. Agents' keys reach Foundry only that way;
// a member restricted here sees only those ontologies, in Analyst, Ontologies
// and Records; a team's members hold its grants. Sources are the limit: giving
// a member or a team any restricts them, taking them away leaves them restricted
// (it never gives anyone more), and only lifting it, with none left, undoes it.
// Everyone else sees what they saw before. Ontologies says who each is shared
// with.

type Tab = "agents" | "people" | "teams" | "ontologies";
const TABS: { id: Tab; label: string }[] = [
  { id: "agents", label: "Agents" },
  { id: "people", label: "People" },
  { id: "teams", label: "Teams" },
  { id: "ontologies", label: "Ontologies" },
];

/** What every part of the page reads, with the names it shows. */
function useAccessData() {
  const { id } = useWorkspace();
  const access = useQuery(accessQuery(id)),
    ontologies = useQuery(ontologyQueries.list(id)),
    connectors = useQuery(connectorsQuery(id)),
    sets = useQuery(documentSetQueries.list(id)),
    members = useInfiniteQuery(membersQuery(id));
  // Every member, for names and the People and Teams lists.
  useEffect(() => {
    if (members.hasNextPage && !members.isFetchingNextPage)
      void members.fetchNextPage();
  }, [members.hasNextPage, members.isFetchingNextPage]);
  const people = useMemo(
    () => (members.data?.pages || []).flatMap((p) => p.members || []),
    [members.data],
  );
  const names = useMemo(() => {
    const ontology = new Map(
        (ontologies.data?.ontologies || []).map((o) => [o.id, o.name || o.id]),
      ),
      connection = new Map(
        (connectors.data?.connectors || []).map((c) => [
          c.id,
          c.name || connectorLabel(c.kind),
        ]),
      ),
      set = new Map((sets.data || []).map((s) => [s.id, setLabel(s)]));
    return {
      ontology: (o: string) => ontology.get(o) || "An ontology",
      connection: (c: string) => connection.get(c) || "A deleted connection",
      set: (s: string) => set.get(s) || "A deleted set",
    };
  }, [ontologies.data, connectors.data, sets.data]);
  const failed = access.error || connectors.error || sets.error;
  return {
    access,
    connectors,
    sets,
    members,
    people,
    names,
    loading: access.isPending || connectors.isPending || sets.isPending,
    failed,
    retry: () =>
      void Promise.all([
        access.refetch(),
        connectors.refetch(),
        sets.refetch(),
      ]),
  };
}
type AccessData = ReturnType<typeof useAccessData>;

/** "Reads Customer contracts, part of Renewals", or that it reads nothing yet. */
function readsLine(data: AccessData, principal: Principal) {
  return readsText(
    readings(
      data.access.data || {},
      heldGrants(data.access.data || {}, principal),
      principal,
    ),
    data.names.ontology,
  );
}

// What being restricted takes, said wherever saving restricts someone.
const restrictionCost =
  "They lose the rest of each ontology’s schema, other connections and other documents, and taking these sources away later doesn’t bring those back: that takes lifting the restriction under People.";

/** One source of the editor: its box, and what it is. */
function SourceCheck({
  state,
  onToggle,
  children,
  coveredBy,
}: {
  state: "chosen" | "covered" | "off";
  onToggle: () => void;
  children: ReactNode;
  coveredBy?: string;
}) {
  return (
    <label
      className="checkbox wk-access-source"
      title={state === "covered" ? `Shared with ${coveredBy}` : undefined}
    >
      <input
        type="checkbox"
        checked={state !== "off"}
        disabled={state === "covered"}
        onChange={onToggle}
      />
      {children}
    </label>
  );
}

/**
 * A principal's sources: the workspace's connections (each with its tables
 * and the sets its syncs write), the sets people made and People
 * enrichment, saved as one PUT; then the ontologies its grants (a member's
 * teams' too) cover, and what each other one lacks.
 */
function GrantsDialog({
  data,
  principal,
  name,
  onClose,
}: {
  data: AccessData;
  principal: Principal;
  name: string;
  onClose: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const access = data.access.data || {},
    saved = grantsOf(access.grants || [], principal);
  const [chosen, setChosen] = useState<Grant[]>(saved),
    [pending, setPending] = useState(false),
    [confirming, setConfirming] = useState(false),
    [error, setError] = useState("");
  const tree = sourceTree(
      data.connectors.data?.connectors || [],
      data.sets.data || [],
    ),
    owners = setOwners(data.sets.data || []);
  const key = (gs: readonly Grant[]) =>
    targetsOf(gs, owners)
      .map((t) => JSON.stringify(t))
      .sort()
      .join();
  const dirty = key(chosen) !== key(saved);
  const flip = (g: Grant) => setChosen((c) => toggle(c, g));
  const state = (g: Grant, owner?: string) => sourceState(chosen, g, owner);
  const teams =
    principal.kind === "KIND_USER"
      ? (access.teams || []).filter((t) =>
          t.memberUserIds?.includes(principal.id),
        )
      : [];
  const all = readings(access, heldGrants(access, principal), principal),
    reads = all.filter((r) => r.reach !== "none"),
    lacks = all.filter((r) => r.reach === "none");
  const save = async () => {
    setPending(true);
    setError("");
    try {
      await setGrants(id, principal, targetsOf(chosen, owners));
      await qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
      notify(`Saved what ${name} can read.`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setConfirming(false);
      setPending(false);
    }
  };
  // Sources are the limit: giving them restricts whoever isn't yet, so say so first.
  const restricting =
      chosen.length > 0 ? newlyRestricted(access, data.people, principal) : [],
    // A team's members not read yet: it can't name them, but still asks.
    unnamed =
      chosen.length > 0 &&
      principal.kind === "KIND_TEAM" &&
      (data.members.isPending ||
        !!data.members.error ||
        data.members.hasNextPage);
  return (
    <>
      {confirming && (
        <ConfirmDialog
          title={
            principal.kind !== "KIND_TEAM"
              ? `Restrict ${name}?`
              : unnamed
                ? `Restrict the members of ${name}?`
                : `Restrict ${plural(restricting.length, "member")} of ${name}?`
          }
          description={
            principal.kind !== "KIND_TEAM"
              ? `They’ll see only what these sources hold. ${restrictionCost}`
              : `${unnamed ? "Its members" : restricting.map(nameOf).join(", ")} will see only what the team’s sources hold. ${restrictionCost}`
          }
          confirmLabel="Save and restrict"
          pending={pending}
          onConfirm={() => void save()}
          onCancel={() => setConfirming(false)}
        />
      )}
      <Dialog
        title={`Sources for ${name}`}
        description="What these sources hold of an ontology’s current load is shared; all of it, once everything it reads is chosen here."
        size="lg"
        onClose={onClose}
        footer={
          <>
            <Button onClick={onClose}>Close</Button>
            <Button
              variant="primary"
              pending={pending}
              disabled={!dirty}
              onClick={() =>
                restricting.length > 0 || unnamed
                  ? setConfirming(true)
                  : void save()
              }
            >
              Save
            </Button>
          </>
        }
      >
        <div className="wk-access-grants" data-replay-block>
          <fieldset className="wk-access-sources">
            <legend>Sources</legend>
            {tree.connections.length > 0 && (
              <ul aria-label="Connections">
                {tree.connections.map((c) => {
                  const connection: Grant = {
                    kind: "connector",
                    connectorId: c.id,
                  };
                  return (
                    <li key={c.id}>
                      <SourceCheck
                        state={state(connection)}
                        onToggle={() => flip(connection)}
                      >
                        <span aria-hidden>
                          <SourceLogo source={c.kind} size={18} />
                        </span>
                        <span>
                          {c.name}
                          <small> · all of it, now and later</small>
                        </span>
                      </SourceCheck>
                      {(c.tables.length > 0 || c.sets.length > 0) && (
                        <ul
                          className="wk-access-nested"
                          aria-label={`${c.name} tables and sets`}
                        >
                          {c.tables.map((t) => {
                            const table: Grant = {
                              kind: "table",
                              connectorId: c.id,
                              table: t,
                            };
                            return (
                              <li key={grantKey(table)}>
                                <SourceCheck
                                  state={state(table)}
                                  coveredBy={c.name}
                                  onToggle={() => flip(table)}
                                >
                                  <span className="hf-mono">{t}</span>
                                </SourceCheck>
                              </li>
                            );
                          })}
                          {c.sets.map((s) => {
                            const set: Grant = { kind: "set", setId: s.id };
                            return (
                              <li key={s.id}>
                                <SourceCheck
                                  state={state(set, c.id)}
                                  coveredBy={c.name}
                                  onToggle={() => flip(set)}
                                >
                                  {c.name} / {s.name}
                                </SourceCheck>
                              </li>
                            );
                          })}
                        </ul>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
            {tree.sets.length > 0 && (
              <ul aria-label="Document sets">
                {tree.sets.map((s) => {
                  const set: Grant = { kind: "set", setId: s.id };
                  return (
                    <li key={s.id}>
                      <SourceCheck
                        state={state(set)}
                        onToggle={() => flip(set)}
                      >
                        {s.name}
                        <small> · documents</small>
                      </SourceCheck>
                    </li>
                  );
                })}
              </ul>
            )}
            <ul aria-label="Other sources">
              <li>
                <SourceCheck
                  state={state({ kind: "enrichment" })}
                  onToggle={() => flip({ kind: "enrichment" })}
                >
                  People enrichment
                  <small> · profiles a load looked up</small>
                </SourceCheck>
              </li>
            </ul>
          </fieldset>
          <section className="wk-access-reads" aria-labelledby="access-reads">
            <h4 id="access-reads">Can read</h4>
            {reads.length ? (
              <ul aria-labelledby="access-reads" className="wk-access-lacks">
                {reads.map((r) => (
                  <li key={r.ontologyId}>
                    {r.reach === "all" ? (
                      data.names.ontology(r.ontologyId)
                    ) : (
                      <>
                        <strong>{data.names.ontology(r.ontologyId)}</strong>
                        <small>
                          Part of it: not{" "}
                          {missingText(r.missing, data.names).join(", ")}
                        </small>
                      </>
                    )}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="wk-note">
                Nothing yet: no ontology’s current load reads any of what is
                saved.
              </p>
            )}
            {lacks.length > 0 && (
              <>
                <h4 id="access-lacks">Can’t read yet</h4>
                <ul aria-labelledby="access-lacks" className="wk-access-lacks">
                  {lacks.map((r) => (
                    <li key={r.ontologyId}>
                      <strong>{data.names.ontology(r.ontologyId)}</strong>
                      <small>
                        {r.missing.uncoverable
                          ? "Load again to share"
                          : `Reads ${missingText(r.missing, data.names).join(", ")}`}
                      </small>
                    </li>
                  ))}
                </ul>
              </>
            )}
            <p className="wk-note">
              Ontologies show here once they’re loaded.
              {teams.length > 0 &&
                ` Also holds what ${teams.map((t) => t.name).join(", ")} ${teams.length > 1 ? "are" : "is"} given.`}
            </p>
          </section>
        </div>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </Dialog>
    </>
  );
}

/** A row of a list of principals: who, what they read, and their actions. */
function Row({
  icon,
  title,
  tags,
  lines,
  actions,
}: {
  icon: ReactNode;
  title: string;
  tags?: ReactNode;
  lines: ReactNode;
  actions: ReactNode;
}) {
  return (
    <li className="wk-member" aria-label={title}>
      <span className="hf-avatar" aria-hidden>
        {icon}
      </span>
      <div className="wk-member-text">
        <strong>
          {title}
          {tags}
        </strong>
        <small>{lines}</small>
      </div>
      <div className="wk-access-actions">{actions}</div>
    </li>
  );
}

// ---------------------------------------------------------------- agents

function NewKeyDialog({ onClose }: { onClose: () => void }) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [name, setName] = useState(""),
    [made, setMade] = useState<CreateServiceCredentialResponse>(),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const copy = (token: string) =>
    navigator.clipboard
      .writeText(token)
      .then(() => notify("Copied the key."))
      .catch(() => notify("Couldn’t copy the key.", true));
  return (
    <Dialog
      title={
        made ? `Key for ${made.credential?.name || name}` : "New agent key"
      }
      description={
        made
          ? "Copy it now: Foundry shows it once."
          : "A key reads only the ontologies its sources cover, through the Helix MCP."
      }
      size="sm"
      onClose={onClose}
    >
      {made ? (
        <div className="wk-pad wk-access-token" data-replay-block>
          <label className="hf-field">
            <span>Key</span>
            <div className="wk-access-copy">
              <input
                readOnly
                type="password"
                autoComplete="off"
                className="hf-mono"
                aria-label="Agent key"
                value={made.token || ""}
              />
              <Button
                size="sm"
                variant="ghost"
                icon={Copy}
                aria-label="Copy agent key"
                onClick={() => void copy(made.token || "")}
              >
                Copy
              </Button>
            </div>
            <small>
              Put it in your agent’s environment, never in a chat. Then choose
              its sources.
            </small>
          </label>
          <div className="hf-form-actions">
            <Button variant="primary" data-autofocus onClick={onClose}>
              Done
            </Button>
          </div>
        </div>
      ) : (
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            setPending(true);
            setError("");
            try {
              setMade(await createKey(id, name.trim()));
              await qc.invalidateQueries({ queryKey: keysQuery(id).queryKey });
            } catch (err) {
              setError((err as Error).message);
            } finally {
              setPending(false);
            }
          }}
        >
          <label className="hf-field">
            <span>Name</span>
            <input
              required
              data-autofocus
              autoComplete="off"
              maxLength={100}
              placeholder="Support agent"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
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
              disabled={!name.trim()}
            >
              Create key
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

function AgentsPanel({ data }: { data: AccessData }) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const keys = useQuery(keysQuery(id));
  const [making, setMaking] = useState(false),
    [editing, setEditing] = useState<ServiceCredential>(),
    [revoking, setRevoking] = useState<ServiceCredential>(),
    [pending, setPending] = useState(false);
  const live = (keys.data || []).filter((k) => !k.revokedAt);
  const revoke = async (k: ServiceCredential) => {
    setPending(true);
    try {
      await revokeKey(id, k.id);
      await Promise.all([
        qc.invalidateQueries({ queryKey: keysQuery(id).queryKey }),
        qc.invalidateQueries({ queryKey: accessQuery(id).queryKey }),
      ]);
      notify(`Revoked ${k.name || "the key"}.`);
      setRevoking(undefined);
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  const principal = (k: ServiceCredential): Principal => ({
    kind: "KIND_SERVICE_CREDENTIAL",
    id: k.id,
  });
  return (
    <>
      <Panel
        title="Agents"
        subtitle="Keys your agents use through the Helix MCP. A key reads and asks only the ontologies its sources cover."
        actions={
          <Button
            variant="primary"
            size="sm"
            icon={Plus}
            onClick={() => setMaking(true)}
          >
            New key
          </Button>
        }
        padded={false}
      >
        {keys.isPending ? (
          <div className="wk-pad">
            <SkeletonText lines={2} />
          </div>
        ) : keys.error ? (
          <div className="wk-pad">
            <p role="alert" className="hf-form-error">
              {keys.error.message}
            </p>
            <Button size="sm" onClick={() => void keys.refetch()}>
              Retry
            </Button>
          </div>
        ) : live.length ? (
          <ul className="wk-members" aria-label="Agent keys" data-replay-block>
            {live.map((k) => (
              <Row
                key={k.id}
                icon={<Bot size={15} />}
                title={k.name || k.id}
                lines={
                  <>
                    <span className="hf-mono">{k.tokenPrefix}…</span>
                    {k.timestamps?.createdAt &&
                      ` · made ${formatRelative(k.timestamps.createdAt)}`}
                    {" · "}
                    {readsLine(data, principal(k))}
                  </>
                }
                actions={
                  <>
                    <Button
                      size="sm"
                      data-track="access-sources"
                      aria-label={`Sources for ${k.name || k.id}`}
                      onClick={() => setEditing(k)}
                    >
                      Sources
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Revoke ${k.name || k.id}`}
                      onClick={() => setRevoking(k)}
                    >
                      Revoke
                    </Button>
                  </>
                }
              />
            ))}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={KeyRound}
            title="No agent keys yet"
            text="Make one for each agent, then choose what it reads."
          />
        )}
      </Panel>
      {making && <NewKeyDialog onClose={() => setMaking(false)} />}
      {editing && (
        <GrantsDialog
          data={data}
          principal={principal(editing)}
          name={editing.name || editing.id}
          onClose={() => setEditing(undefined)}
        />
      )}
      {revoking && (
        <ConfirmDialog
          title={`Revoke ${revoking.name || "this key"}?`}
          description="An agent using it stops reaching Helix at once, and its sources go with it."
          confirmLabel="Revoke"
          tone="danger"
          pending={pending}
          onConfirm={() => revoke(revoking)}
          onCancel={() => setRevoking(undefined)}
        />
      )}
    </>
  );
}

// ---------------------------------------------------------------- people

const roleOf = (m: WorkspaceMember) =>
  shellRoles[m.role || "WORKSPACE_ROLE_UNSPECIFIED"] ?? "member";
const rank = { owner: 0, admin: 1, member: 2 };
const nameOf = (m: WorkspaceMember) =>
  m.user?.displayName || m.user?.email || "Unknown user";

function RestrictSwitch({
  member,
  restricted,
  holdsSources,
}: {
  member: WorkspaceMember;
  restricted: boolean;
  // Holding sources is what restricts them, so it can't be lifted until they go.
  holdsSources: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    [pending, setPending] = useState(false),
    // Shown as asked at once; back as it was if the WFE refuses.
    [shown, setShown] = useState(restricted);
  useEffect(() => setShown(restricted), [restricted]);
  const can = restrictable(member);
  const name = nameOf(member);
  const held = restricted && holdsSources,
    why = held
      ? "Holds sources, their own or a team’s. Take those away first."
      : can.ok
        ? undefined
        : can.why;
  const flip = async (on: boolean) => {
    setPending(true);
    setShown(on);
    try {
      await setRestricted(id, member.user?.id || "", on);
      await qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
      notify(
        on
          ? `${name} now sees only what their sources cover.`
          : `${name} sees everything again.`,
      );
    } catch (e) {
      setShown(restricted);
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  return (
    <label className="checkbox wk-access-switch" title={why}>
      <input
        type="checkbox"
        role="switch"
        data-track="access-restrict"
        aria-label={`Restrict ${name}`}
        aria-description={why}
        checked={shown}
        disabled={pending || held || (!can.ok && !restricted)}
        onChange={(e) => void flip(e.target.checked)}
      />
      Restricted
    </label>
  );
}

function PeoplePanel({ data }: { data: AccessData }) {
  const { user } = useWorkspace(),
    [editing, setEditing] = useState<WorkspaceMember>();
  const restricted = new Set(data.access.data?.restrictedUserIds || []);
  const list = [...data.people].sort(
    (a, b) =>
      rank[roleOf(a)] - rank[roleOf(b)] || nameOf(a).localeCompare(nameOf(b)),
  );
  const principal = (m: WorkspaceMember): Principal => ({
    kind: "KIND_USER",
    id: m.user?.id || "",
  });
  return (
    <>
      <Panel
        title="People"
        subtitle="Giving a member sources restricts them to what those hold, in Analyst, Ontologies and Records, and hides everything else. Members who aren’t restricted see everything."
        padded={false}
      >
        {data.members.isPending ? (
          <div className="wk-pad">
            <SkeletonText lines={3} />
          </div>
        ) : data.members.error ? (
          <div className="wk-pad">
            <p role="alert" className="hf-form-error">
              {data.members.error.message}
            </p>
            <Button size="sm" onClick={() => void data.members.refetch()}>
              Retry
            </Button>
          </div>
        ) : (
          <ul className="wk-members" aria-label="People" data-replay-block>
            {list.map((m, i) => {
              const role = roleOf(m),
                can = restrictable(m),
                // A restriction applies only while their role is member.
                isRestricted =
                  role === "member" && restricted.has(m.user?.id || ""),
                held = heldGrants(data.access.data || {}, principal(m));
              return (
                <Row
                  key={m.id || m.user?.id || i}
                  icon={initials(nameOf(m))}
                  title={nameOf(m)}
                  tags={
                    <>
                      {m.user?.id === user.id && <Tag>You</Tag>}
                      <Tag tone={role === "member" ? "neutral" : "accent"}>
                        {roleLabel[role]}
                      </Tag>
                      {isRestricted && holdsRawAccess(m) && (
                        <Tag tone="warning">Raw access</Tag>
                      )}
                    </>
                  }
                  lines={
                    isRestricted ? (
                      <>
                        {readsLine(data, principal(m))}
                        {holdsRawAccess(m) &&
                          ". Also writes a project or queries its databases outside Foundry, which this doesn’t limit: take that away in the Helix dashboard."}
                      </>
                    ) : can.ok ? (
                      "Sees everything"
                    ) : (
                      can.why
                    )
                  }
                  actions={
                    role === "member" && (
                      <>
                        <RestrictSwitch
                          member={m}
                          restricted={isRestricted}
                          holdsSources={held.length > 0}
                        />
                        <Button
                          size="sm"
                          data-track="access-sources"
                          aria-label={`Sources for ${nameOf(m)}`}
                          // One who can't be confined is given none, but can lose what they hold.
                          disabled={!can.ok && held.length === 0}
                          title={can.ok ? undefined : can.why}
                          onClick={() => setEditing(m)}
                        >
                          Sources
                        </Button>
                      </>
                    )
                  }
                />
              );
            })}
          </ul>
        )}
      </Panel>
      {editing && (
        <GrantsDialog
          data={data}
          principal={principal(editing)}
          name={nameOf(editing)}
          onClose={() => setEditing(undefined)}
        />
      )}
    </>
  );
}

// ---------------------------------------------------------------- teams

function TeamDialog({
  data,
  team,
  onClose,
}: {
  data: AccessData;
  // The team to rename and change, or none for a new one.
  team?: FoundryTeam;
  onClose: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [name, setName] = useState(team?.name || ""),
    [members, setMembers] = useState(new Set(team?.memberUserIds || [])),
    [pending, setPending] = useState(false),
    [confirming, setConfirming] = useState(false),
    [error, setError] = useState("");
  const people = [...data.people].sort((a, b) =>
    nameOf(a).localeCompare(nameOf(b)),
  );
  // Joining a team that holds sources restricts whoever isn't yet.
  const access = data.access.data || {},
    joining =
      team &&
      grantsOf(access.grants || [], {
        kind: "KIND_TEAM",
        id: team.id || "",
      }).length > 0
        ? people.filter((m) => {
            const userId = m.user?.id || "";
            return (
              m.role === "WORKSPACE_ROLE_MEMBER" &&
              members.has(userId) &&
              !team.memberUserIds?.includes(userId) &&
              !access.restrictedUserIds?.includes(userId)
            );
          })
        : [];
  const save = async () => {
    setPending(true);
    setError("");
    const body = { name: name.trim(), memberUserIds: [...members] };
    try {
      if (team) await updateTeam(id, team.id || "", body);
      else await createTeam(id, body);
      await qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
      notify(team ? `Saved ${body.name}.` : `Made ${body.name}.`);
      onClose();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setConfirming(false);
      setPending(false);
    }
  };
  return (
    <>
      {confirming && (
        <ConfirmDialog
          title={
            joining.length === 1
              ? `Restrict ${nameOf(joining[0]!)}?`
              : `Restrict ${plural(joining.length, "member")}?`
          }
          description={`${joining.map(nameOf).join(", ")} will see only what ${team?.name || "the team"}’s sources hold. ${restrictionCost}`}
          confirmLabel="Save and restrict"
          pending={pending}
          onConfirm={() => void save()}
          onCancel={() => setConfirming(false)}
        />
      )}
      <Dialog
        title={team ? `Edit ${team.name}` : "New team"}
        description="A team’s members read what the team is given. Once it has sources, anyone on it is restricted to what they hold, and stays so after leaving."
        size="sm"
        onClose={onClose}
      >
        <form
          data-replay-block
          onSubmit={(e) => {
            e.preventDefault();
            if (joining.length > 0) setConfirming(true);
            else void save();
          }}
        >
          <label className="hf-field">
            <span>Name</span>
            <input
              required
              data-autofocus
              autoComplete="off"
              maxLength={120}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <fieldset className="wk-access-sources">
            <legend>Members</legend>
            <ul aria-label="Team members">
              {people.map((m) => {
                const userId = m.user?.id || "";
                return (
                  <li key={userId}>
                    <label className="checkbox wk-access-source">
                      <input
                        type="checkbox"
                        checked={members.has(userId)}
                        onChange={() =>
                          setMembers((s) => {
                            const next = new Set(s);
                            if (!next.delete(userId)) next.add(userId);
                            return next;
                          })
                        }
                      />
                      {nameOf(m)}
                    </label>
                  </li>
                );
              })}
            </ul>
          </fieldset>
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
              disabled={!name.trim()}
            >
              {team ? "Save" : "Create team"}
            </Button>
          </div>
        </form>
      </Dialog>
    </>
  );
}

function TeamsPanel({ data }: { data: AccessData }) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [making, setMaking] = useState(false),
    [changing, setChanging] = useState<FoundryTeam>(),
    [editing, setEditing] = useState<FoundryTeam>(),
    [deleting, setDeleting] = useState<FoundryTeam>(),
    [pending, setPending] = useState(false);
  const teams = data.access.data?.teams || [];
  const person = new Map(data.people.map((m) => [m.user?.id || "", nameOf(m)]));
  const remove = async (t: FoundryTeam) => {
    setPending(true);
    try {
      await deleteTeam(id, t.id || "");
      await qc.invalidateQueries({ queryKey: accessQuery(id).queryKey });
      notify(`Deleted ${t.name}.`);
      setDeleting(undefined);
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  const principal = (t: FoundryTeam): Principal => ({
    kind: "KIND_TEAM",
    id: t.id || "",
  });
  return (
    <>
      <Panel
        title="Teams"
        subtitle="Give a team sources once, and every member of it can read what they cover."
        actions={
          <Button
            variant="primary"
            size="sm"
            icon={Plus}
            onClick={() => setMaking(true)}
          >
            New team
          </Button>
        }
        padded={false}
      >
        {teams.length ? (
          <ul className="wk-members" aria-label="Teams" data-replay-block>
            {teams.map((t) => {
              const who = (t.memberUserIds || []).map(
                (u) => person.get(u) || "A former member",
              );
              return (
                <Row
                  key={t.id}
                  icon={<Users size={15} />}
                  title={t.name || "Team"}
                  lines={`${who.length ? who.join(", ") : "No members"} · ${readsLine(data, principal(t))}`}
                  actions={
                    <>
                      <Button
                        size="sm"
                        data-track="access-sources"
                        aria-label={`Sources for ${t.name}`}
                        onClick={() => setEditing(t)}
                      >
                        Sources
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`Edit ${t.name}`}
                        onClick={() => setChanging(t)}
                      >
                        Edit
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`Delete ${t.name}`}
                        onClick={() => setDeleting(t)}
                      >
                        Delete
                      </Button>
                    </>
                  }
                />
              );
            })}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Users}
            title="No teams yet"
            text="Group members to give them sources together."
          />
        )}
      </Panel>
      {(making || changing) && (
        <TeamDialog
          data={data}
          team={changing}
          onClose={() => {
            setMaking(false);
            setChanging(undefined);
          }}
        />
      )}
      {editing && (
        <GrantsDialog
          data={data}
          principal={principal(editing)}
          name={editing.name || "the team"}
          onClose={() => setEditing(undefined)}
        />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          description={`Its ${plural(deleting.memberUserIds?.length || 0, "member")} stop reading what it was given. Anyone it restricted stays restricted, reading only what else they hold, until that’s lifted under People.`}
          confirmLabel="Delete team"
          tone="danger"
          pending={pending}
          onConfirm={() => remove(deleting)}
          onCancel={() => setDeleting(undefined)}
        />
      )}
    </>
  );
}

// ---------------------------------------------------------------- ontologies

/** Each loaded ontology, and who its current load is shared with: all of it, or part. */
function OntologiesPanel({ data }: { data: AccessData }) {
  const { id } = useWorkspace(),
    keys = useQuery(keysQuery(id));
  const named = new Map<string, string>([
    ...data.people.map(
      (m) =>
        [
          principalKey({ kind: "KIND_USER", id: m.user?.id }),
          nameOf(m),
        ] as const,
    ),
    ...(keys.data || []).map(
      (k) =>
        [
          principalKey({ kind: "KIND_SERVICE_CREDENTIAL", id: k.id }),
          k.name || k.id,
        ] as const,
    ),
    ...(data.access.data?.teams || []).map(
      (t) =>
        [
          principalKey({ kind: "KIND_TEAM", id: t.id }),
          t.name || "A team",
        ] as const,
    ),
  ]);
  const who = (ps: readonly FoundryPrincipal[] = []) =>
    ps.map((p) => named.get(principalKey(p)) || "Someone").join(", ");
  const ontologies = data.access.data?.ontologies || [];
  return (
    <Panel
      title="Ontologies"
      subtitle="Who each ontology’s current load is shared with, through their own sources. A team’s members read what it is given."
      padded={false}
    >
      {ontologies.length ? (
        <ul className="wk-members" aria-label="Ontologies" data-replay-block>
          {ontologies.map((o) => {
            const name = data.names.ontology(o.ontologyId || "");
            return (
              <li key={o.ontologyId} className="wk-member" aria-label={name}>
                <span className="hf-avatar" aria-hidden>
                  <Shapes size={15} />
                </span>
                <div className="wk-member-text">
                  <strong>{name}</strong>
                  {o.readers?.length ? (
                    <small>Shared with {who(o.readers)}</small>
                  ) : null}
                  {o.partialReaders?.length ? (
                    <small>Part of it: {who(o.partialReaders)}</small>
                  ) : null}
                  {!o.readers?.length && !o.partialReaders?.length && (
                    <small>Not shared with anyone</small>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      ) : (
        <EmptyState
          size="sm"
          icon={Shapes}
          title="No ontology is loaded yet"
          text="An ontology shows here once it has a current load."
        />
      )}
    </Panel>
  );
}

export function AccessTab() {
  const { role } = useWorkspace();
  return canManage(role) ? (
    <AccessTabs />
  ) : (
    <p className="wk-note">Only owners and admins manage who reads what.</p>
  );
}

function AccessTabs() {
  const [tab, setTab] = useTabParam(TABS.map((t) => t.id));
  const data = useAccessData();
  return (
    <>
      <Tabs
        label="Access"
        idPrefix="access"
        items={TABS}
        value={tab}
        onChange={setTab}
      />
      <TabPanel id={tab} value={tab} idPrefix="access" className="wk-access">
        {data.loading ? (
          <SkeletonText lines={4} />
        ) : data.failed ? (
          <div className="wk-pad">
            <p role="alert" className="hf-form-error">
              Couldn’t load who reads what. {data.failed.message}
            </p>
            <Button size="sm" onClick={data.retry}>
              Retry
            </Button>
          </div>
        ) : tab === "agents" ? (
          <AgentsPanel data={data} />
        ) : tab === "people" ? (
          <PeoplePanel data={data} />
        ) : tab === "ontologies" ? (
          <OntologiesPanel data={data} />
        ) : (
          <TeamsPanel data={data} />
        )}
      </TabPanel>
    </>
  );
}
