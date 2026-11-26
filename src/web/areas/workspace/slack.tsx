import { useState } from "react";
import { Link } from "react-router-dom";
import {
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";
import { Bot, Globe, ListChecks, UserRound } from "lucide-react";
import type {
  FoundrySlackInstall,
  FoundrySlackLink,
} from "../../../shared/wfe";
import { LOCAL } from "../../../shared/deployment";
import { sentence } from "../../api";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  EmptyState,
  KeyValue,
  Panel,
  SkeletonText,
  StatusBadge,
  Tag,
  formatDate,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { connectorKindsQuery } from "../data/data-api";
import { ChoiceCard } from "./choice-card";
import { initials } from "./people";
import { relayQuery, type RelayView } from "./relay-api";
import { RelayLinks } from "./relay-links";
import { AllowedPeople, ApproveFromSlack, RelayPanel } from "./slack-local";
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
  updateSlackBotScope,
  updateSlackMode,
  type SlackBotScope,
  type SlackMode,
  type SlackView,
} from "./slack-api";

// Settings › Slack: Helix's own Slack app, Helix Foundry,
// which answers people in a Slack workspace as Founder: each linked Slack
// user as their own Foundry user, or, in bot mode, anyone there as one
// shared bot, a workspace key that reads everything in the workspace, or
// only the sources Settings › Access gives it, and whose proposed changes
// owners and admins run as themselves. Everyone sees whether it is added,
// its mode, what the bot reads and their own link; owners and admins add it
// (Slack's page, then back through /slack/installed), switch its mode
// (behind a confirmation), choose what the bot reads (widening it behind a
// confirmation), see who is linked, unlink anyone and disconnect it.
// The install says when it lacks permissions what Helix offers here needs
// (owners and admins add it again), and, where a Slack connection may read
// through it (the Slack kind's setup), the connection that does or the way
// to make one, that no Slack admin or owner added it, and when Slack held
// back the team's events. Owners and admins also see the Foundry installs
// on people's computers linked to the workspace through Helix's relay, and
// revoke them (./relay-links.tsx).
//
// A local build's (./slack-local.tsx) reaches Slack only through Helix's
// relay: its owner links it to Helix first, then adds the app, links their
// own Slack account as the owner, chooses whether it approves changes from
// Slack, and allows Slack users who asked to read and ask as them. It has
// no shared bot.

const PURPOSE =
  "Ask Founder from Slack: mention @Founder in a channel or message it directly, and it answers from your Foundry data as you.";
const LOCAL_PURPOSE =
  "Ask Founder from Slack: mention @Founder in a channel or message it directly, and it answers from this Foundry’s data, through Helix.";
const LOCAL_NOTICE =
  "Links in its answers open on the computer running this Foundry.";
const BOT_PURPOSE: Record<SlackBotScope, string> = {
  workspace:
    "Ask Founder from Slack: anyone in your Slack workspace mentions @Founder in a channel or messages it directly, and the shared bot answers from everything in this workspace.",
  sources:
    "Ask Founder from Slack: anyone in your Slack workspace mentions @Founder in a channel or messages it directly, and the shared bot answers from the sources you give it.",
};
/** The message shortcut (backend slack.CallbackAskAbout), in one line. */
const SHORTCUT =
  "On any message, More actions › Ask Founder about this asks what Foundry knows about it.";

/** The two modes as their cards say them. */
const MODES = {
  per_person: {
    title: "Per person",
    description:
      "Each person links their Slack account, and Helix Foundry answers them as their own Foundry user, with what they may read and do.",
    icon: <UserRound strokeWidth={1.5} />,
  },
  bot: {
    title: "Shared bot",
    description:
      "Anyone in your Slack workspace can ask; the bot reads everything in this workspace, or only the sources you choose; answers in a channel reach everyone in it, guests included; owners and admins approve changes.",
    icon: <Bot strokeWidth={1.5} />,
  },
} as const;

export function SlackTab() {
  const { id } = useWorkspace();
  // A local build's link to Helix, which its Slack goes through.
  const relay = useQuery({ ...relayQuery(id), enabled: LOCAL }).data,
    linked = relay?.kind === "linked" ? relay : null;
  const query = useQuery({
    ...slackQuery(id),
    // A local install learns of an install Helix finished from Helix's
    // next answer to it, within half a minute.
    refetchInterval: (q) =>
      LOCAL && linked && q.state.data?.kind === "not_installed" ? 5000 : false,
  });
  if (!LOCAL) return <SlackSettings view={query} />;
  return (
    <>
      <RelayPanel />
      <SlackSettings view={query} linked={linked} />
    </>
  );
}

/** The app's panels as the workspace's Slack has it. */
function SlackSettings({
  view: query,
  linked,
}: {
  view: UseQueryResult<SlackView>;
  linked?: Extract<RelayView, { kind: "linked" }> | null;
}) {
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
        <>
          <Panel title="Slack" subtitle={PURPOSE}>
            <p className="wk-note">
              Helix offers Helix Foundry for Slack to a few workspaces while
              Slack reviews the app. Ask Helix to add this one.
            </p>
          </Panel>
          {!LOCAL && view.canManage && <RelayLinks />}
        </>
      );
    case "not_installed":
      return (
        <>
          <NotInstalled canManage={view.canManage} blocked={LOCAL && !linked} />
          {!LOCAL && view.canManage && <RelayLinks />}
        </>
      );
    case "installed":
      return (
        <>
          <Installed
            install={view.install}
            mode={view.mode}
            canManage={view.canManage}
          />
          {!LOCAL && (
            <IdentityMode
              install={view.install}
              mode={view.mode}
              canManage={view.canManage}
            />
          )}
          {!LOCAL && view.mode.kind === "bot" && (
            <SharedBot
              team={view.install.teamName || "your Slack workspace"}
              bot={view.mode}
              canManage={view.canManage}
            />
          )}
          <YourAccount
            install={view.install}
            mode={view.mode}
            canManage={view.canManage}
            link={view.myLink}
          />
          {linked && <ApproveFromSlack relay={linked} />}
          {view.canManage &&
            (LOCAL ? <AllowedPeople /> : <LinkedPeople mode={view.mode} />)}
          {!LOCAL && view.canManage && <RelayLinks />}
          <p className="wk-note">
            {SLACK_NOTICE}
            {LOCAL && ` ${LOCAL_NOTICE}`}
          </p>
        </>
      );
  }
}

/**
 * Add to Slack for owners and admins; who adds it, for members. A local
 * install adds it through Helix: `blocked` until linked to Helix.
 */
function NotInstalled({
  canManage,
  blocked = false,
}: {
  canManage: boolean;
  blocked?: boolean;
}) {
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
      <Panel title="Slack" subtitle={LOCAL ? LOCAL_PURPOSE : PURPOSE}>
        <div className="wk-pad">
          {canManage ? (
            <>
              <p className="wk-note">
                {blocked
                  ? "Link this Foundry to Helix first: Slack reaches it through Helix."
                  : LOCAL
                    ? "Slack asks you to allow Helix Foundry in your Slack workspace, then Helix brings you back here. Then message it in Slack from that account: it sends you a link that makes it this Foundry’s owner there. Added it already? It shows here within half a minute of Helix passing it on."
                    : "Slack asks you to allow Helix Foundry in your Slack workspace, then brings you back here. Each person then links their own Slack account from a message the app sends them."}
              </p>
              <Button
                variant="primary"
                pending={pending}
                disabled={blocked}
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
  mode,
  canManage,
}: {
  install: FoundrySlackInstall;
  mode: SlackMode;
  canManage: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [confirm, setConfirm] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const team = install.teamName || install.teamId || "Slack";
  // Where a Slack connection may read through the app (foundry.slack.feed).
  const feed = useQuery({
    ...connectorKindsQuery(id),
    retry: false,
  }).data?.kinds?.find((k) => k.kind === "slack")?.slack;
  const missing = install.missingScopes || [];
  const [reinstalling, setReinstalling] = useState(false);
  const reinstall = async () => {
    setReinstalling(true);
    setError("");
    try {
      window.location.assign(await startSlackInstall(id));
    } catch (e) {
      installFailed(installFailure(e));
      setError(sentence((e as Error).message));
      setReinstalling(false);
    }
  };
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
      subtitle={
        LOCAL
          ? LOCAL_PURPOSE
          : mode.kind === "bot"
            ? BOT_PURPOSE[mode.scope]
            : PURPOSE
      }
      actions={
        canManage && (
          <>
            {!!missing.length && (
              <Button
                size="sm"
                variant="primary"
                pending={reinstalling}
                onClick={() => void reinstall()}
              >
                Reinstall
              </Button>
            )}
            <Button size="sm" variant="danger" onClick={() => setConfirm(true)}>
              Disconnect
            </Button>
          </>
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
                {team}{" "}
                {missing.length ? (
                  <StatusBadge
                    status="stale"
                    label="Needs new permissions"
                    size="sm"
                  />
                ) : (
                  <StatusBadge status="healthy" label="Added" size="sm" />
                )}
              </>
            ),
          },
          {
            label: "Added by",
            value: install.installedByName && (
              <span data-replay-block>{install.installedByName}</span>
            ),
          },
          { label: "Added", value: added && formatDate(added, false) },
        ]}
      />
      <div className="wk-pad">
        <p className="wk-note">{SHORTCUT}</p>
        {!!missing.length && (
          <p className="wk-note">
            Helix Foundry needs new Slack permissions ({missing.join(", ")}) for
            what it does in Slack.{" "}
            {canManage
              ? "Reinstall it to grant them; it keeps answering in Slack meanwhile."
              : "An owner or admin reinstalls it to grant them."}
          </p>
        )}
        {feed && !install.installerAdmin && (
          <p className="wk-note">
            Installed by a non-admin: syncing its channels needs a Slack
            workspace admin or owner to add it.
          </p>
        )}
        {feed &&
          (feed.inUseByConnectorId ? (
            <p className="wk-note">
              Its channels sync into{" "}
              <Link to={paths.connection(feed.inUseByConnectorId)}>
                a Slack connection
              </Link>
              .
            </p>
          ) : (
            canManage && (
              <p className="wk-note">
                <Link to="/data?add=slack">Sync its channels</Link> into Foundry
                as a Slack connection.
              </p>
            )
          ))}
        {install.eventsLimitedAt && (
          <p className="wk-note">
            Slack held back some of {team}’s events on{" "}
            {formatDate(install.eventsLimitedAt)}: it sends an app at most
            30,000 an hour per workspace, so some messages may have been missed.
          </p>
        )}
        {error && !confirm && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
      </div>
      {confirm && (
        <ConfirmDialog
          title={`Disconnect ${team}?`}
          description={`Helix Foundry is removed from the Slack workspace and stops answering there. ${LOCAL ? "Your link and the people you allowed are" : "Everyone’s links are"} deleted, and so are the Slack threads it answered in Founder.${mode.kind === "bot" ? " The shared bot’s key is revoked." : ""}`}
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

/**
 * Whom the install answers as: per person or the shared bot. Owners and
 * admins switch it behind a confirmation; members read it.
 */
function IdentityMode({
  install,
  mode,
  canManage,
}: {
  install: FoundrySlackInstall;
  mode: SlackMode;
  canManage: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [asking, setAsking] = useState<SlackMode["kind"]>(),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const team = install.teamName || "your Slack workspace";
  const change = async () => {
    if (!asking) return;
    setPending(true);
    setError("");
    try {
      qc.setQueryData(slackKey(id), await updateSlackMode(id, asking));
      void qc.invalidateQueries({ queryKey: slackKey(id) });
      notify(
        asking === "bot"
          ? "The shared bot is on"
          : `Helix Foundry answers each linked person in ${team} as themselves`,
      );
      setAsking(undefined);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  const cancel = () => {
    setAsking(undefined);
    setError("");
  };
  return (
    <Panel title="Who it answers as">
      <div className="wk-pad wk-ai-form">
        {canManage ? (
          <fieldset className="plain-fieldset" disabled={pending}>
            <legend className="hf-sr-only">Who Helix Foundry answers as</legend>
            <div className="choice-options provider-options">
              {(["per_person", "bot"] as const).map((k) => (
                <ChoiceCard
                  key={k}
                  name="slack-mode"
                  title={MODES[k].title}
                  description={MODES[k].description}
                  icon={MODES[k].icon}
                  checked={(asking ?? mode.kind) === k}
                  onChange={() => setAsking(k === mode.kind ? undefined : k)}
                />
              ))}
            </div>
          </fieldset>
        ) : (
          <p className="wk-note">
            {MODES[mode.kind].title}: {MODES[mode.kind].description}
          </p>
        )}
      </div>
      {asking === "bot" && (
        <ConfirmDialog
          title={`Turn on the shared bot in ${team}?`}
          description="Anyone in your Slack workspace can ask Helix Foundry without linking, and the bot answers them all alike. It reads everything in this Foundry workspace, so anyone in the Slack workspace can ask it about all of it; you can limit it to sources you choose once it is on. Answers in a channel reach everyone in it, guests included. Changes it prepares wait for an owner or admin, who reviews them in Slack and runs them as themselves."
          confirmLabel="Turn on"
          pending={pending}
          onConfirm={change}
          onCancel={cancel}
        >
          {error && (
            <p role="alert" className="hf-form-error">
              {error}
            </p>
          )}
        </ConfirmDialog>
      )}
      {asking === "per_person" && (
        <ConfirmDialog
          title="Turn off the shared bot?"
          description="Helix Foundry answers only linked people again, each as their own Foundry user. The bot’s key is revoked with its sources, and the Slack threads it answered in are deleted from Founder."
          confirmLabel="Turn off"
          tone="danger"
          pending={pending}
          onConfirm={change}
          onCancel={cancel}
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

/** What the shared bot can see, as its choices say it. */
const SCOPES = {
  workspace: {
    title: "Everything in this workspace",
    description:
      "Every ontology, its data and documents, as a member reads them. Anyone in the Slack workspace can ask it about everything in this Foundry workspace.",
    icon: <Globe strokeWidth={1.5} />,
  },
  sources: {
    title: "Only the sources I choose",
    description:
      "Only the sources you give its key in Settings › Access; nothing until you choose them.",
    icon: <ListChecks strokeWidth={1.5} />,
  },
} as const;

/**
 * The shared bot: what it can see (owners and admins choose, widening it to
 * the whole workspace behind a confirmation; members read it), its key,
 * whether it still works, and, while it reads only its sources, the way to
 * them in Settings › Access for owners and admins.
 */
function SharedBot({
  team,
  bot,
  canManage,
}: {
  team: string;
  bot: Extract<SlackMode, { kind: "bot" }>;
  canManage: boolean;
}) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient();
  const [widening, setWidening] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const choose = async (scope: SlackBotScope) => {
    setPending(true);
    setError("");
    try {
      qc.setQueryData(slackKey(id), await updateSlackBotScope(id, scope));
      void qc.invalidateQueries({ queryKey: slackKey(id) });
      notify(
        scope === "workspace"
          ? "The shared bot reads everything in this workspace"
          : "The shared bot reads only the sources you choose",
      );
      setWidening(false);
    } catch (e) {
      setError(sentence((e as Error).message));
    } finally {
      setPending(false);
    }
  };
  return (
    <Panel
      title="Shared bot"
      actions={
        canManage &&
        bot.scope === "sources" &&
        bot.credentialId &&
        !bot.broken && (
          <ButtonLink
            size="sm"
            variant="primary"
            to={paths.keySources(bot.credentialId)}
          >
            Choose the bot’s sources
          </ButtonLink>
        )
      }
    >
      <KeyValue
        columns={2}
        items={[
          { label: "Key", value: bot.credentialName || "Slack bot" },
          {
            label: "Status",
            value: bot.broken ? (
              <StatusBadge status="failed" label="Can’t answer" size="sm" />
            ) : (
              <StatusBadge status="healthy" label="Answering" size="sm" />
            ),
          },
        ]}
      />
      <div className="wk-pad wk-ai-form">
        {canManage ? (
          <fieldset className="plain-fieldset" disabled={pending}>
            <legend>What the bot can see</legend>
            <div className="choice-options provider-options">
              {(["workspace", "sources"] as const).map((k) => (
                <ChoiceCard
                  key={k}
                  name="slack-bot-scope"
                  title={SCOPES[k].title}
                  description={SCOPES[k].description}
                  icon={SCOPES[k].icon}
                  checked={(widening ? "workspace" : bot.scope) === k}
                  onChange={() => {
                    if (k === bot.scope) return setWidening(false);
                    if (k === "workspace") return setWidening(true);
                    void choose(k);
                  }}
                />
              ))}
            </div>
          </fieldset>
        ) : (
          <p className="wk-note">
            What the bot can see: {SCOPES[bot.scope].title}.{" "}
            {SCOPES[bot.scope].description}
          </p>
        )}
        {error && !widening && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        {bot.broken ? (
          <p role="alert" className="hf-form-error">
            The bot’s key was revoked, deleted, expired or given database
            permissions, so it can’t answer.{" "}
            {canManage
              ? "Turn bot mode off and on again to give it a new key."
              : "An owner or admin turns bot mode off and on again to give it a new key."}
          </p>
        ) : (
          <p className="wk-note">
            {bot.scope === "workspace"
              ? `Anyone in ${team} can ask it about everything in this Foundry workspace, and its answers in a channel reach everyone in it.`
              : "It reads only the sources its key is given in Settings › Access, none until an owner or admin chooses them."}{" "}
            Changes it prepares wait in Slack for an owner or admin, who reviews
            them and runs them as themselves.
          </p>
        )}
      </div>
      {widening && (
        <ConfirmDialog
          title="Let the shared bot see everything?"
          description={`The bot reads every ontology, its data and documents in this Foundry workspace, whatever its key is given in Settings › Access. Anyone in ${team} can ask it about all of it, and its answers in a channel reach everyone in it, guests included.`}
          confirmLabel="Let it see everything"
          pending={pending}
          onConfirm={() => void choose("workspace")}
          onCancel={() => {
            setWidening(false);
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
  mode,
  canManage,
  link,
}: {
  install: FoundrySlackInstall;
  mode: SlackMode;
  canManage: boolean;
  link: FoundrySlackLink | null;
}) {
  const [confirm, setConfirm] = useState(false);
  const team = install.teamName || "Slack";
  return (
    <Panel title="Your Slack account">
      <div className="wk-pad">
        {link ? (
          <>
            <p className="wk-note">
              Linked
              {link.createdAt ? ` on ${formatDate(link.createdAt, false)}` : ""}
              {LOCAL ? (
                <>
                  : Helix Foundry answers your Slack account (
                  <span className="hf-mono">{link.slackUserId}</span>) as this
                  Foundry’s owner.
                </>
              ) : mode.kind === "per_person" ? (
                <>
                  : Helix Foundry answers your Slack account (
                  <span className="hf-mono">{link.slackUserId}</span>) as you.
                </>
              ) : (
                <>
                  : Helix Foundry knows your Slack account (
                  <span className="hf-mono">{link.slackUserId}</span>) is you.
                  {canManage
                    ? " You review and run the shared bot’s changes in Slack as yourself."
                    : " The shared bot answers you as it answers everyone."}
                </>
              )}
            </p>
            <Button size="sm" onClick={() => setConfirm(true)}>
              Unlink
            </Button>
          </>
        ) : LOCAL ? (
          <p className="wk-note">
            Not linked. Message @Founder in {team} from the Slack account that
            added it: it sends you a link that makes that account this Foundry’s
            owner in Slack.
          </p>
        ) : mode.kind === "bot" && !canManage ? (
          <p className="wk-note">
            Not linked, and no need: the shared bot answers anyone in {team}.
          </p>
        ) : mode.kind === "bot" ? (
          <p className="wk-note">
            Not linked. Click Review on a change the shared bot prepares in{" "}
            {team}: Helix Foundry sends you a link that connects your Slack
            account to your Foundry account, so you can run its changes as
            yourself.
          </p>
        ) : (
          <p className="wk-note">
            Not linked. Message @Founder in {team}: it sends you a link that
            connects your Slack account to your Foundry account.
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
function LinkedPeople({ mode }: { mode: SlackMode }) {
  const { id, user } = useWorkspace();
  const links = useQuery({
    ...slackLinksQuery(id),
    select: (r) => r.links,
  });
  const [unlinking, setUnlinking] = useState<FoundrySlackLink | null>(null);
  return (
    <Panel
      title="Linked people"
      subtitle={
        mode.kind === "bot"
          ? "Who Helix Foundry knows in Slack. Owners and admins among them review and run the shared bot’s changes."
          : "Who Helix Foundry answers in Slack, each as their own Foundry user."
      }
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
          text={
            mode.kind === "bot"
              ? "Owners and admins link their Slack accounts from the message Helix Foundry sends them when they first click Review on a change the shared bot prepares."
              : "People link their Slack accounts from the message Helix Foundry sends them when they first message it."
          }
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
