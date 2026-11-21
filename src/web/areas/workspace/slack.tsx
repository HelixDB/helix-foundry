import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  FoundrySlackInstall,
  FoundrySlackLink,
} from "../../../shared/wfe";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  KeyValue,
  Panel,
  SkeletonText,
  StatusBadge,
  Tag,
  formatDate,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { initials } from "./people";
import {
  SLACK_NOTICE,
  deleteSlackLink,
  disconnectSlack,
  installFailed,
  installFailure,
  slackKey,
  slackLinksQuery,
  slackQuery,
  startSlackInstall,
} from "./slack-api";

// Settings › Slack (hosted only): Helix's own Slack app, Helix Foundry,
// which answers people in a Slack workspace as Founder, each linked Slack
// user as their own Foundry user. Everyone sees whether it is added and
// their own link; owners and admins add it (Slack's page, then back through
// /slack/installed), see who is linked, unlink anyone and disconnect it.

const PURPOSE =
  "Ask Founder from Slack: mention @Helix Foundry in a channel or message it directly, and it answers from your Foundry data as you.";

export function SlackTab() {
  const { id } = useWorkspace();
  const query = useQuery(slackQuery(id));
  const view = query.data;
  if (query.isPending)
    return (
      <Panel title="Slack">
        <SkeletonText lines={3} />
      </Panel>
    );
  if (!view)
    return (
      <Panel title="Slack">
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read Slack’s setting. {query.error?.message}
          </p>
          <Button size="sm" onClick={() => void query.refetch()}>
            Retry
          </Button>
        </div>
      </Panel>
    );
  switch (view.kind) {
    case "unavailable":
      return (
        <Panel title="Slack">
          <p className="wk-note">
            Helix Foundry for Slack isn’t available here.
          </p>
        </Panel>
      );
    case "not_offered":
      return (
        <Panel title="Slack" subtitle={PURPOSE}>
          <p className="wk-note">
            Helix offers Helix Foundry for Slack to a few workspaces while Slack
            reviews the app. Ask Helix to add this one.
          </p>
        </Panel>
      );
    case "not_installed":
      return <NotInstalled canManage={view.canManage} />;
    case "installed":
      return (
        <>
          <Installed install={view.install} canManage={view.canManage} />
          <YourAccount install={view.install} link={view.myLink} />
          {view.canManage && <LinkedPeople />}
          <p className="wk-note">{SLACK_NOTICE}</p>
        </>
      );
  }
}

/** Add to Slack for owners and admins; who adds it, for members. */
function NotInstalled({ canManage }: { canManage: boolean }) {
  const { id } = useWorkspace();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const add = async () => {
    setPending(true);
    setError("");
    try {
      window.location.assign(await startSlackInstall(id));
    } catch (e) {
      installFailed(installFailure(e));
      setError(sentence((e as Error).message));
      setPending(false);
    }
  };
  return (
    <>
      <Panel title="Slack" subtitle={PURPOSE}>
        <div className="wk-pad">
          {canManage ? (
            <>
              <p className="wk-note">
                Slack asks you to allow Helix Foundry in your Slack workspace,
                then brings you back here. Each person then links their own
                Slack account from a message the app sends them.
              </p>
              <Button
                variant="primary"
                pending={pending}
                onClick={() => void add()}
              >
                Add to Slack
              </Button>
              {error && (
                <p role="alert" className="hf-form-error">
                  {error}
                </p>
              )}
            </>
          ) : (
            <p className="wk-note">
              An owner or admin adds Helix Foundry to Slack. Then message it
              there to link your Slack account.
            </p>
          )}
        </div>
      </Panel>
      <p className="wk-note">{SLACK_NOTICE}</p>
    </>
  );
}

/** The install: its Slack workspace, who added it and when, and Disconnect. */
function Installed({
  install,
  canManage,
}: {
  install: FoundrySlackInstall;
  canManage: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [confirm, setConfirm] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const team = install.teamName || install.teamId || "Slack";
  const disconnect = async () => {
    setPending(true);
    setError("");
    try {
      qc.setQueryData(slackKey(id), await disconnectSlack(id));
      void qc.invalidateQueries({ queryKey: slackKey(id) });
      setConfirm(false);
      notify(`Helix Foundry is disconnected from ${team}`);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  const added = install.updatedAt || install.createdAt;
  return (
    <Panel
      title="Slack"
      subtitle={PURPOSE}
      actions={
        canManage && (
          <Button size="sm" variant="danger" onClick={() => setConfirm(true)}>
            Disconnect
          </Button>
        )
      }
    >
      <KeyValue
        columns={2}
        items={[
          {
            label: "Slack workspace",
            value: (
              <>
                {team} <StatusBadge status="healthy" label="Added" size="sm" />
              </>
            ),
          },
          { label: "Added by", value: install.installedByName },
          { label: "Added", value: added && formatDate(added, false) },
        ]}
      />
      {confirm && (
        <ConfirmDialog
          title={`Disconnect ${team}?`}
          description="Helix Foundry is removed from the Slack workspace and stops answering there. Everyone’s links are deleted, and so are the Slack threads it answered in Founder."
          confirmLabel="Disconnect"
          tone="danger"
          pending={pending}
          onConfirm={disconnect}
          onCancel={() => {
            setConfirm(false);
            setError("");
          }}
        >
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </ConfirmDialog>
      )}
    </Panel>
  );
}

/** The caller's own Slack account in the install, with Unlink. */
function YourAccount({
  install,
  link,
}: {
  install: FoundrySlackInstall;
  link: FoundrySlackLink | null;
}) {
  const [confirm, setConfirm] = useState(false);
  return (
    <Panel title="Your Slack account">
      <div className="wk-pad">
        {link ? (
          <>
            <p className="wk-note">
              Linked
              {link.createdAt ? ` on ${formatDate(link.createdAt, false)}` : ""}
              : Helix Foundry answers your Slack account (
              <span className="hf-mono">{link.slackUserId}</span>) as you.
            </p>
            <Button size="sm" onClick={() => setConfirm(true)}>
              Unlink
            </Button>
          </>
        ) : (
          <p className="wk-note">
            Not linked. Message @Helix Foundry in {install.teamName || "Slack"}:
            it sends you a link that connects your Slack account to your Foundry
            account.
          </p>
        )}
      </div>
      {confirm && link?.slackUserId && (
        <UnlinkDialog
          slackUserId={link.slackUserId}
          own
          onDone={() => setConfirm(false)}
        />
      )}
    </Panel>
  );
}

/** Confirms unlinking a Slack account, the caller's own or someone's. */
function UnlinkDialog({
  slackUserId,
  own,
  name,
  onDone,
}: {
  slackUserId: string;
  own: boolean;
  name?: string;
  onDone: () => void;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const unlink = async () => {
    setPending(true);
    setError("");
    try {
      await deleteSlackLink(id, slackUserId);
      await qc.invalidateQueries({ queryKey: slackKey(id) });
      notify(
        own
          ? "Your Slack account is unlinked"
          : `${name || "Their"} Slack account is unlinked`,
      );
      onDone();
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  return (
    <ConfirmDialog
      title={own ? "Unlink your Slack account?" : `Unlink ${name || "them"}?`}
      description={
        own
          ? "Helix Foundry stops answering you in Slack until you link again from a message it sends you."
          : "Helix Foundry stops answering them in Slack until they link again."
      }
      confirmLabel="Unlink"
      tone="danger"
      pending={pending}
      onConfirm={unlink}
      onCancel={onDone}
    >
      {error && (
        <p role="alert" className="hf-form-error">
          {error}
        </p>
      )}
    </ConfirmDialog>
  );
}

/** Who is linked, for owners and admins, each with Unlink. */
function LinkedPeople() {
  const { id, user } = useWorkspace();
  const links = useQuery(slackLinksQuery(id));
  const [unlinking, setUnlinking] = useState<FoundrySlackLink | null>(null);
  return (
    <Panel
      title="Linked people"
      subtitle="Who Helix Foundry answers in Slack, each as their own Foundry user."
      padded={false}
    >
      {links.isPending ? (
        <div className="wk-pad">
          <SkeletonText lines={2} />
        </div>
      ) : links.error ? (
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read who is linked. {links.error.message}
          </p>
          <Button size="sm" onClick={() => void links.refetch()}>
            Retry
          </Button>
        </div>
      ) : links.data.length ? (
        <ul className="wk-members" aria-label="Linked people" data-replay-block>
          {links.data.map((l) => {
            const name = l.userName || l.userId || "Someone";
            return (
              <li key={l.slackUserId} className="wk-member">
                <span className="hf-avatar" aria-hidden>
                  {initials(name)}
                </span>
                <div className="wk-member-text">
                  <strong>
                    {name}
                    {l.userId === user.id && <Tag>You</Tag>}
                  </strong>
                  <small>
                    Slack user <span className="hf-mono">{l.slackUserId}</span>
                  </small>
                </div>
                {l.createdAt && (
                  <small className="wk-member-joined">
                    Linked {formatDate(l.createdAt, false)}
                  </small>
                )}
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Unlink ${name}`}
                  onClick={() => setUnlinking(l)}
                >
                  Unlink
                </Button>
              </li>
            );
          })}
        </ul>
      ) : (
        <EmptyState
          size="sm"
          title="No one is linked yet"
          text="People link their Slack accounts from the message Helix Foundry sends them when they first message it."
        />
      )}
      {unlinking?.slackUserId && (
        <UnlinkDialog
          slackUserId={unlinking.slackUserId}
          own={unlinking.userId === user.id}
          name={unlinking.userName}
          onDone={() => setUnlinking(null)}
        />
      )}
    </Panel>
  );
}
