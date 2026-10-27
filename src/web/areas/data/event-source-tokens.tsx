import { useEffect, useRef, useState } from "react";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Copy, KeyRound } from "lucide-react";
import type {
  Connector,
  CreateFoundryEventSourceTokenResponse,
  FoundryEventSourceToken,
} from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  DataGrid,
  formatDate,
  formatRelative,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { membersQuery } from "../workspace/members";
import {
  createEventToken,
  eventTokensQuery,
  revokeEventToken,
} from "./data-api";
import { canManage } from "./model";

// An API source's send tokens, after helix-foundry's Ingestion API token
// (connection-form.tsx): owners and admins make one, shown once in a
// read-only field with Copy, and revoke them; members see the list. A token
// is in this component's state only, from its create until Done: the WFE
// keeps its hash and lists its prefix.

/** The WFE's cap on a source's live tokens. */
const MAX_LIVE = 10;
// Set per stamp in next.config.ts: empty where API sources take pieces only
// over the Helix MCP (prod, local mode).
const OVER_HTTP = !!process.env.NEXT_PUBLIC_HELIX_EVENTS_URL;
const COL = 150;

export function EventSourceTokens({
  source,
  focusCreate = false,
}: {
  source: Connector;
  // Focus "Create a send token" when a dialog opens on it (?token=new).
  focusCreate?: boolean;
}) {
  const { id, role, user, notify } = useWorkspace(),
    manage = canManage(role),
    qc = useQueryClient(),
    root = useRef<HTMLDivElement>(null);
  // The kit's dialog focuses [data-autofocus] once, as it opens. A dialog
  // that opened on a source still loading (EventSourceSendDialog in a new
  // tab) renders this later, so it focuses the button itself. Rendered with
  // the dialog, this runs before the dialog opens, and does nothing.
  useEffect(() => {
    if (focusCreate)
      root.current?.querySelector<HTMLElement>("[data-autofocus]")?.focus();
  }, []);
  const tokens = useQuery(eventTokensQuery(id, source.id)),
    // Who made each, by name: the first page of Settings › Members.
    members = useInfiniteQuery(membersQuery(id));
  const [made, setMade] = useState<CreateFoundryEventSourceTokenResponse>(),
    [creating, setCreating] = useState(false),
    [error, setError] = useState(""),
    [revoking, setRevoking] = useState<FoundryEventSourceToken>(),
    [revokePending, setRevokePending] = useState(false);
  const list = tokens.data?.tokens || [],
    live = list.filter((t) => !t.revokedAt).length;
  const maker = (userId?: string) => {
    if (userId && userId === user.id) return "You";
    const u = members.data?.pages
      .flatMap((p) => p.members || [])
      .find((m) => m.user?.id === userId)?.user;
    return u?.displayName || u?.email || "—";
  };
  const refresh = () =>
    qc.invalidateQueries({
      queryKey: eventTokensQuery(id, source.id).queryKey,
    });
  const create = async () => {
    setCreating(true);
    setError("");
    try {
      setMade(await createEventToken(id, source.id));
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setCreating(false);
    }
  };
  const revoke = async (t: FoundryEventSourceToken) => {
    setRevokePending(true);
    try {
      await revokeEventToken(id, source.id, t.id);
      // The token just shown can be revoked before Done.
      if (made?.token.id === t.id) setMade(undefined);
      await refresh();
      notify("Send token revoked");
      setRevoking(undefined);
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setRevokePending(false);
    }
  };
  const copy = (secret: string) =>
    navigator.clipboard
      .writeText(secret)
      .then(() => notify("Copied the send token."))
      .catch(() => notify("Couldn’t copy the send token.", true));
  return (
    <div className="dc-event-tokens" ref={root}>
      <p>
        An app or agent that sends on its own uses a send token as its Helix MCP
        bearer. It can only send pieces to this source.
      </p>
      {manage && !made && (
        <div className="dc-event-actions">
          <Button
            icon={KeyRound}
            pending={creating}
            disabled={live >= MAX_LIVE}
            data-autofocus={focusCreate ? "" : undefined}
            onClick={() => void create()}
          >
            Create a send token
          </Button>
          {live >= MAX_LIVE && (
            <small className="muted">
              A source has at most {MAX_LIVE} live tokens: revoke one to make
              another.
            </small>
          )}
        </div>
      )}
      {!manage && (
        <p className="muted small">
          An owner or admin of the workspace makes send tokens.
        </p>
      )}
      {error && (
        <p role="alert" className="error-box">
          {error}
        </p>
      )}
      {made && (
        <div
          className="dc-event-secret"
          role="group"
          aria-label="New send token"
        >
          <label htmlFor={`token-${made.token.id}`}>
            Copy it now: Foundry shows it once. Put it in your{" "}
            {OVER_HTTP ? "app’s or agent’s" : "agent’s"} environment as{" "}
            <code>HELIX_EVENT_TOKEN</code>, never in a chat.
          </label>
          <div className="dc-copy-field">
            <input
              id={`token-${made.token.id}`}
              readOnly
              type="password"
              autoComplete="off"
              className="hf-mono"
              value={made.secret}
            />
            <Button
              size="sm"
              variant="ghost"
              icon={Copy}
              aria-label="Copy send token"
              onClick={() => void copy(made.secret)}
            >
              Copy
            </Button>
          </div>
          <small className="muted">
            Anyone with this token can add pieces to this source and replace any
            piece in it by its id. Give each app or agent that sends on its own
            a source of its own.
          </small>
          <div className="dc-event-actions">
            <Button size="sm" onClick={() => setMade(undefined)}>
              Done
            </Button>
          </div>
        </div>
      )}
      <DataGrid<FoundryEventSourceToken>
        label="Send tokens"
        loading={tokens.isPending}
        rows={list}
        getRowKey={(t) => t.id}
        maxHeight={260}
        emptyText={
          tokens.error
            ? `Couldn’t load the send tokens: ${tokens.error.message}`
            : "No send tokens yet."
        }
        columns={[
          {
            key: "prefix",
            label: "Token",
            width: COL,
            render: (t) => <span className="hf-mono">{t.prefix}…</span>,
          },
          {
            key: "createdByUserId",
            label: "Made by",
            width: COL,
            render: (t) => (
              <span className="dc-clip">{maker(t.createdByUserId)}</span>
            ),
          },
          {
            key: "createdAt",
            label: "Made",
            type: "timestamp",
            width: COL,
            render: (t) => (
              <span title={formatDate(t.createdAt)}>
                {t.createdAt ? formatRelative(t.createdAt) : "—"}
              </span>
            ),
          },
          {
            key: "lastUsedAt",
            label: "Last used",
            type: "timestamp",
            width: COL,
            render: (t) =>
              t.lastUsedAt ? (
                <span title={formatDate(t.lastUsedAt)}>
                  {formatRelative(t.lastUsedAt)}
                </span>
              ) : (
                "Never"
              ),
          },
          {
            key: "revokedAt",
            label: "Revoked",
            width: COL,
            sortable: false,
            render: (t) =>
              t.revokedAt ? (
                <span title={formatDate(t.revokedAt)}>
                  {formatRelative(t.revokedAt)}
                </span>
              ) : manage ? (
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Revoke ${t.prefix}…`}
                  onClick={() => setRevoking(t)}
                >
                  Revoke
                </Button>
              ) : (
                "Live"
              ),
          },
        ]}
      />
      {revoking && (
        <ConfirmDialog
          title={`Revoke ${revoking.prefix}…?`}
          description="Apps and agents sending with it are refused from now on. This can’t be undone: make a new token for them."
          confirmLabel="Revoke token"
          tone="danger"
          pending={revokePending}
          onCancel={() => setRevoking(undefined)}
          onConfirm={() => revoke(revoking)}
        />
      )}
    </div>
  );
}
