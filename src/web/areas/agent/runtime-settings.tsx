import "./agent.css";
import { useEffect, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Laptop, LoaderCircle, Plus, Sparkles } from "lucide-react";
import type {
  AgentHelperRuntime,
  AgentRuntime,
  FoundryAgentHost,
  FoundryAgentHostPairingResponse,
  FoundryAgentRuntime,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { sentence } from "../../api";
import {
  Button,
  ConfirmDialog,
  Panel,
  SkeletonText,
  StatusBadge,
  formatRelative,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { ChoiceCard } from "../workspace/choice-card";
import {
  CODEX_UPDATE,
  HELPER_COMMANDS,
  RUNTIMES,
  accountText,
  agentRefusal,
  helperAccount,
  threadRuntime,
  type ThreadRuntime,
} from "./model";
import {
  RUNTIME_POLL_MS,
  agentQueries,
  pairAgentHelper,
  revokeAgentHelper,
  setAgentRuntime,
} from "./queries";

// Settings › AI models on a local install, for its owner: where the
// workspace's new Assistant threads run (the workspace's AI model, or the
// owner's Claude or ChatGPT subscription through a helper on their
// computer; choosing Codex the first time shows what it runs with, their
// own Codex setup, and the owner confirms it once), a one-time pairing code
// with the command that spends it, then, once a helper paired, how to start it (pairing starts
// nothing) until it runs, and the install's helpers: whether each runs,
// what its CLIs are signed in to (Codex with its version: one older than
// Foundry runs says to update it), and Revoke. The page reads them every 3 s
// while a code waits for its helper, a helper paired waits to start, or the
// chosen runtime's helper isn't running, else every 30 s. Nothing shows
// while the Assistant is off. No helper token is ever shown: Foundry never
// sends one.

const SLOW_POLL_MS = 30_000;

const ICONS: Record<AgentRuntime, ReactNode> = {
  api: <Sparkles strokeWidth={1.5} />,
  claude_code: <img src="/providers/claude.svg" alt="" />,
  // OpenAI's mark is drawn in currentColor, which an <img> takes as black:
  // as a mask it takes the card's color, in either theme.
  codex: <span className="ag-mark-openai" />,
};
const DESCRIPTIONS: Record<AgentRuntime, string> = {
  api: "The provider above. Its turns count toward the workspace’s Assistant budget.",
  claude_code:
    "Claude Code on your computer, signed in to your Claude plan, through Foundry’s helper. It costs the workspace nothing; check that your plan’s terms allow it.",
  codex:
    "Codex on your computer, signed in to your ChatGPT plan, through Foundry’s helper. It runs with your own Codex setup, its shell off. It costs the workspace nothing; its turns count against your ChatGPT plan.",
};

/** What Codex runs with, as the owner confirms it and the settings say it after. */
const CODEX_TERMS = [
  "Codex runs with your own Codex setup, including your MCP servers, plugins and apps. A tool of theirs that asks for approval is declined; one that doesn’t ask can run in the Assistant’s turns.",
  "Foundry turns off Codex’s shell, sub-agents, web search and image viewer, and declines any command or file change it asks for. The Assistant reads your documents and synced rows, which could carry instructions meant to mislead it: this keeps them from running commands or changing files on your computer. A tool of your own setup that doesn’t ask can still be called if a document tells the model to, and can read or send whatever it reaches.",
  "The Assistant’s turns count against your ChatGPT plan’s Codex limits. Check that your plan’s terms allow it.",
] as const;

/**
 * Pairing, a closed set: a code shown and waiting for its helper (with the
 * helpers there were when it was made), then the helper it paired, waiting
 * to be started.
 */
type Pairing =
  | ({ kind: "code"; before: string[] } & FoundryAgentHostPairingResponse)
  | { kind: "paired"; hostId: string; name: string };

export function AssistantRuntimePanel() {
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient();
  const [pairing, setPairing] = useState<Pairing>();
  const [choosing, setChoosing] = useState<AgentRuntime>();
  // Codex chosen the first time: what it runs with, to confirm.
  const [confirming, setConfirming] = useState(false);
  const query = useQuery({
    ...agentQueries.runtime(ws),
    refetchInterval: (q: { state: { data?: FoundryAgentRuntime } }) =>
      pairing || waiting(q.state.data) ? RUNTIME_POLL_MS : SLOW_POLL_MS,
  });
  const data = query.data;
  // A helper the code paired: say so, and how to start it; once it runs
  // (or is revoked), put that away.
  useEffect(() => {
    if (!pairing || !data) return;
    const hosts = data.hosts || [];
    if (pairing.kind === "paired") {
      const host = hosts.find((h) => h.id === pairing.hostId);
      if (!host || host.online) setPairing(undefined);
      if (host?.online) notify(`${pairing.name} is running.`);
      return;
    }
    const paired = hosts.find((h) => !pairing.before.includes(h.id));
    if (!paired) return;
    const name = paired.name || "a helper";
    setPairing(
      paired.online ? undefined : { kind: "paired", hostId: paired.id, name },
    );
    trackEvent("agent_helper_paired");
    notify(`Paired ${name}.`);
  }, [data, pairing]);

  if (agentRefusal(query.error) === "FOUNDRY_AGENT_DISABLED") return null;
  if (query.isPending)
    return (
      <Panel title="The Assistant">
        <SkeletonText lines={3} />
      </Panel>
    );
  if (!data)
    return (
      <Panel title="The Assistant" padded={false}>
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read where the Assistant runs.{" "}
            {query.error && sentence(query.error.message)}
          </p>
          <Button size="sm" onClick={() => void query.refetch()}>
            Retry
          </Button>
        </div>
      </Panel>
    );

  const runtime = data.runtime ?? "api",
    hosts = data.hosts || [];
  // One choice saves at a time; the radios stay focusable meanwhile (a
  // disabled one would drop the keyboard's focus).
  const choose = async (next: AgentRuntime, confirmCodex = false) => {
    if (next === runtime || choosing) return;
    if (next === "codex" && !data.codexConfirmedAt && !confirmCodex)
      return setConfirming(true);
    setChoosing(next);
    try {
      qc.setQueryData(
        agentQueries.runtime(ws).queryKey,
        await setAgentRuntime(ws, next, confirmCodex),
      );
      notify(`New Assistant threads run on ${RUNTIMES[next].on}.`);
    } catch (e) {
      notify(sentence((e as Error).message), true);
    } finally {
      setChoosing(undefined);
    }
  };
  const pair = async () => {
    try {
      const made = await pairAgentHelper(ws);
      setPairing({ kind: "code", ...made, before: hosts.map((h) => h.id) });
    } catch (e) {
      notify(sentence((e as Error).message), true);
    }
  };
  return (
    <Panel
      title="The Assistant"
      subtitle="Where new Assistant threads run. A thread keeps running where it started."
      padded={false}
    >
      <div className="wk-pad wk-ai-form">
        <fieldset
          className="plain-fieldset"
          aria-busy={!!choosing || undefined}
        >
          <legend className="hf-sr-only">Where the Assistant runs</legend>
          <div className="choice-options provider-options">
            {(["api", "claude_code", "codex"] as const).map((r) => (
              <ChoiceCard
                key={r}
                name="assistant-runtime"
                title={RUNTIMES[r].title}
                description={DESCRIPTIONS[r]}
                icon={ICONS[r]}
                checked={(choosing ?? runtime) === r}
                onChange={() => void choose(r)}
              />
            ))}
          </div>
        </fieldset>
        {runtime === "codex" && (
          <CodexTerms confirmedAt={data.codexConfirmedAt || undefined} />
        )}
        {confirming && (
          <ConfirmDialog
            title="Run the Assistant on your Codex?"
            description="New Assistant threads run on the Codex you’re signed in to on your computer, through Foundry’s helper."
            confirmLabel="Use Codex"
            pending={choosing === "codex"}
            onCancel={() => setConfirming(false)}
            onConfirm={async () => {
              await choose("codex", true);
              setConfirming(false);
            }}
          >
            <ul className="ag-codex-terms">
              {CODEX_TERMS.map((t) => (
                <li key={t}>{t}</li>
              ))}
            </ul>
          </ConfirmDialog>
        )}
        {(runtime !== "api" || hosts.length > 0 || pairing) && (
          <Helpers
            runtime={runtime}
            data={data}
            pairing={pairing}
            onPair={pair}
            onCancelPairing={() => setPairing(undefined)}
          />
        )}
      </div>
    </Panel>
  );
}

/** What Codex runs with, while it is chosen: as the owner confirmed it. */
function CodexTerms({ confirmedAt }: { confirmedAt?: string }) {
  const now = useNow();
  return (
    <section className="ag-codex-note" aria-label="What Codex runs with">
      <ul className="ag-codex-terms">
        {CODEX_TERMS.map((t) => (
          <li key={t}>{t}</li>
        ))}
      </ul>
      {confirmedAt && (
        <p className="wk-note">
          You confirmed this {formatRelative(confirmedAt, now)}.
        </p>
      )}
    </section>
  );
}

/** Whether the chosen runtime's helper isn't running: read it often. */
const waiting = (d?: FoundryAgentRuntime) =>
  !!d?.runtime && d.runtime !== "api" && !(d.online || []).includes(d.runtime);

function Helpers({
  runtime,
  data,
  pairing,
  onPair,
  onCancelPairing,
}: {
  runtime: AgentRuntime;
  data: FoundryAgentRuntime;
  pairing?: Pairing;
  onPair: () => Promise<void>;
  onCancelPairing: () => void;
}) {
  const hosts = data.hosts || [];
  const [making, setMaking] = useState(false);
  const pair = async () => {
    setMaking(true);
    await onPair();
    setMaking(false);
  };
  return (
    <section className="ag-helpers" aria-labelledby="ag-helpers-title">
      <div className="ag-helpers-head">
        <h4 id="ag-helpers-title">Helpers</h4>
        {!pairing && (
          <Button
            size="sm"
            variant={hosts.length ? "secondary" : "primary"}
            icon={Plus}
            pending={making}
            onClick={() => void pair()}
          >
            Pair a helper
          </Button>
        )}
      </div>
      <p className="wk-note">
        A helper runs on the computer where you’re signed in to Claude Code or
        Codex, and runs the Assistant’s turns there: Claude Code with Foundry’s
        tools alone, Codex with its shell off. Each keeps a thread’s session on
        that computer, with what Foundry’s tools answered, until the thread is
        deleted or the helper revoked.
      </p>
      {pairing?.kind === "code" && (
        <PairingCode
          pairing={pairing}
          onAgain={() => void pair()}
          onCancel={onCancelPairing}
        />
      )}
      {pairing?.kind === "paired" && (
        <StartPaired name={pairing.name} onDone={onCancelPairing} />
      )}
      {runtime !== "api" &&
        !(data.online || []).includes(runtime) &&
        !pairing &&
        hosts.length > 0 &&
        (["outdated", "unstarted"].includes(
          threadRuntime(undefined, data).state,
        ) ? (
          <UpdateCodex where={threadRuntime(undefined, data)} />
        ) : (
          <StartHelper runtime={runtime} />
        ))}
      {hosts.length > 0 ? (
        <ul className="wk-members" aria-label="Helpers">
          {hosts.map((h) => (
            <HelperRow key={h.id} host={h} min={data.codexMinVersion} />
          ))}
        </ul>
      ) : (
        !pairing && (
          <p className="wk-note">
            No helper is paired yet. Pair one to run new threads on your Claude
            or ChatGPT subscription.
          </p>
        )
      )}
    </section>
  );
}

/**
 * The one-time code's command, its expiry, and the command that keeps the
 * helper running once paired. Left out of session replay.
 */
function PairingCode({
  pairing,
  onAgain,
  onCancel,
}: {
  pairing: Extract<Pairing, { kind: "code" }>;
  onAgain: () => void;
  onCancel: () => void;
}) {
  const now = useNow(1000),
    expires = Date.parse(pairing.expiresAt || ""),
    left = Number.isFinite(expires) ? expires - now : 0;
  if (left <= 0)
    return (
      <div className="ag-pairing" role="status">
        <p className="wk-note">
          This code expired before a helper used it. Make another.
        </p>
        <div className="ag-pairing-actions">
          <Button size="sm" onClick={onAgain}>
            Make another code
          </Button>
          <Button size="sm" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </div>
    );
  const minutes = Math.ceil(left / 60_000);
  return (
    <div className="ag-pairing" data-replay-block>
      <p className="ag-pairing-step">
        <strong>1.</strong> On the computer where Claude Code or Codex is signed
        in, run this. It pairs one helper, once, in the next{" "}
        {minutes === 1 ? "minute" : `${minutes} minutes`}.
      </p>
      <Command text={pairing.command || ""} label="pair command" />
      <p className="ag-pairing-step">
        <strong>2.</strong> Then start it. This keeps it running at every login
        (or run it now with <code>run</code> in place of <code>install</code>):
      </p>
      <Command text={HELPER_COMMANDS.install} label="install command" />
      <div className="ag-pairing-actions">
        <p className="ag-note" role="status">
          <LoaderCircle size={14} className="hf-spin" aria-hidden />
          Waiting for the helper to pair…
        </p>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </div>
  );
}

/**
 * The helper a code just paired, until it runs: pairing starts nothing, so
 * say how to start it.
 */
function StartPaired({ name, onDone }: { name: string; onDone: () => void }) {
  return (
    <div className="ag-pairing" role="status">
      <p className="ag-pairing-step">
        Paired {name}. Now start it on that computer. This keeps it running at
        every login:
      </p>
      <Command text={HELPER_COMMANDS.install} label="install command" />
      <p className="ag-pairing-step">Or run it in a terminal there:</p>
      <Command text={HELPER_COMMANDS.run} label="run command" />
      <div className="ag-pairing-actions">
        <p className="ag-note">
          <LoaderCircle size={14} className="hf-spin" aria-hidden />
          Waiting for it to start…
        </p>
        <Button size="sm" variant="ghost" onClick={onDone}>
          Close
        </Button>
      </div>
    </div>
  );
}

/** What to run when the chosen runtime's helper isn't running. */
function StartHelper({ runtime }: { runtime: AgentHelperRuntime }) {
  return (
    <div className="ag-pairing" role="status">
      <p className="ag-pairing-step">
        No helper running {RUNTIMES[runtime].cli} is online, so new threads
        can’t be sent. Start it on its computer:
      </p>
      <Command text={HELPER_COMMANDS.run} label="run command" />
      <p className="ag-pairing-step">Or keep it running at every login:</p>
      <Command text={HELPER_COMMANDS.install} label="install command" />
    </div>
  );
}

/** What to do when the chosen runtime's helper runs a Codex too old to run turns on, or one that didn't start. */
function UpdateCodex({ where }: { where: ThreadRuntime }) {
  const o = where.helper;
  return where.state === "unstarted" ? (
    <div className="ag-pairing" role="status">
      <p className="ag-pairing-step">
        The Codex on {o?.host ?? "your computer"} didn’t start, so new threads
        can’t be sent. To see why, run this there; the helper tries it again
        every 30 seconds:
      </p>
      <Command text="codex --version" label="check command" />
    </div>
  ) : (
    <div className="ag-pairing" role="status">
      <p className="ag-pairing-step">
        The Codex on {o?.host ?? "your computer"}
        {o?.version ? ` is ${o.version},` : ""} older than{" "}
        {o?.min || "Foundry runs"}
        {o?.min ? ", the oldest Foundry runs" : ""}, so new threads can’t be
        sent. Update it there, and the helper runs it within a minute:
      </p>
      <Command text={CODEX_UPDATE} label="update command" />
    </div>
  );
}

/** A command to copy: never a token. */
export function Command({ text, label }: { text: string; label: string }) {
  const { notify } = useWorkspace();
  return (
    <figure className="ag-command" aria-label={label}>
      <pre>{text}</pre>
      <Button
        size="sm"
        variant="ghost"
        icon={Copy}
        aria-label={`Copy ${label}`}
        onClick={() =>
          void navigator.clipboard
            .writeText(text)
            .then(() => notify("Copied the command."))
            .catch(() => notify("Couldn’t copy the command.", true))
        }
      >
        Copy
      </Button>
    </figure>
  );
}

/**
 * A paired helper: whether it runs, what its CLI is signed in to, when it
 * was last seen, and Revoke.
 */
function HelperRow({ host, min }: { host: FoundryAgentHost; min?: string }) {
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient(),
    now = useNow();
  const [revoking, setRevoking] = useState(false),
    [pending, setPending] = useState(false);
  const name = host.name || "Unnamed helper";
  // What it runs, and a Codex it offers that is too old to run.
  const runtimes = [
    ...new Set([
      ...(host.runtimes || []),
      // An account is one of a runtime it offers: one it doesn't run (an
      // outdated Codex, or one that didn't start) still says why.
      ...(host.accounts || []).flatMap((a) => (a.runtime ? [a.runtime] : [])),
    ]),
  ];
  const accounts = runtimes.map((r) =>
    accountText(helperAccount(host, r), r, min),
  );
  const seen = host.lastSeenAt ? formatRelative(host.lastSeenAt, now) : "";
  return (
    <li className="wk-member ag-helper" aria-label={name}>
      <span className="hf-avatar" aria-hidden>
        <Laptop size={15} />
      </span>
      <div className="wk-member-text">
        <strong>
          {name}
          <StatusBadge
            size="sm"
            status={host.online ? "healthy" : "paused"}
            label={
              host.online
                ? "Running"
                : host.lastSeenAt
                  ? "Not running"
                  : "Not started"
            }
          />
        </strong>
        <small>
          {[
            ...accounts.filter((a) => a.ok).map((a) => a.text),
            host.online ? "" : seen && `last seen ${seen}`,
            host.version && `v${host.version}`,
            host.platform,
          ]
            .filter(Boolean)
            .join(" · ")}
        </small>
        {accounts
          .filter((a) => !a.ok)
          .map((a) => (
            <small key={a.text} className="ag-helper-problem">
              {a.text}.
            </small>
          ))}
      </div>
      <div className="wk-access-actions">
        <Button
          size="sm"
          variant="ghost"
          aria-label={`Revoke ${name}`}
          onClick={() => setRevoking(true)}
        >
          Revoke
        </Button>
      </div>
      {revoking && (
        <ConfirmDialog
          title={`Revoke ${name}?`}
          description="It stops running the Assistant’s turns at once, and a turn it is running ends. Next time it reaches Foundry, it deletes the sessions its Claude Code or Codex kept of its threads. To run more, pair it again."
          confirmLabel="Revoke"
          tone="danger"
          pending={pending}
          onCancel={() => setRevoking(false)}
          onConfirm={async () => {
            setPending(true);
            try {
              await revokeAgentHelper(ws, host.id);
              await qc.invalidateQueries({
                queryKey: agentQueries.runtime(ws).queryKey,
              });
              notify(`Revoked ${name}.`);
              setRevoking(false);
            } catch (e) {
              notify(sentence((e as Error).message), true);
            } finally {
              setPending(false);
            }
          }}
        />
      )}
    </li>
  );
}
