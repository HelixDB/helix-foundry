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
import { HELPER_COMMANDS, RUNTIMES, accountText, helperAccount } from "./model";
import {
  RUNTIME_POLL_MS,
  agentQueries,
  pairAgentHelper,
  revokeAgentHelper,
  setAgentRuntime,
} from "./queries";

// Settings › AI models on a local install, for its owner: where the
// workspace's new Assistant threads run (the workspace's AI model, or the
// owner's Claude subscription through a helper on their computer; Codex is
// shown, not offered), a one-time pairing code with the command that
// spends it, and the install's helpers: whether each runs, what its CLI is
// signed in to, and Revoke. The page reads them every 3 s while a code
// waits for its helper or the chosen runtime's helper isn't running, else
// every 30 s. No helper token is ever shown: Foundry never sends one.

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
    "Not available yet: Codex can’t yet be limited to Foundry’s tools alone.",
};

/** A pairing code shown, with the helpers there were when it was made. */
type Pairing = FoundryAgentHostPairingResponse & { before: string[] };

export function AssistantRuntimePanel() {
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient();
  const [pairing, setPairing] = useState<Pairing>();
  const [choosing, setChoosing] = useState<AgentRuntime>();
  const query = useQuery({
    ...agentQueries.runtime(ws),
    refetchInterval: (q: { state: { data?: FoundryAgentRuntime } }) =>
      pairing || waiting(q.state.data) ? RUNTIME_POLL_MS : SLOW_POLL_MS,
  });
  const data = query.data;
  // A helper the code paired: say so, and put the code away.
  useEffect(() => {
    if (!pairing || !data) return;
    const paired = (data.hosts || []).find(
      (h) => !pairing.before.includes(h.id),
    );
    if (!paired) return;
    setPairing(undefined);
    trackEvent("agent_helper_paired");
    notify(`Paired ${paired.name || "a helper"}.`);
  }, [data, pairing]);

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
  const choose = async (next: AgentRuntime) => {
    if (next === runtime) return;
    setChoosing(next);
    try {
      qc.setQueryData(
        agentQueries.runtime(ws).queryKey,
        await setAgentRuntime(ws, next),
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
      setPairing({ ...made, before: hosts.map((h) => h.id) });
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
        <fieldset className="plain-fieldset" disabled={!!choosing}>
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
                disabled={r === "codex"}
                onChange={() => void choose(r)}
              />
            ))}
          </div>
        </fieldset>
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
        A helper runs on the computer where you’re signed in to Claude Code, and
        runs the Assistant’s turns there with Foundry’s tools alone.
      </p>
      {pairing && (
        <PairingCode
          pairing={pairing}
          onAgain={() => void pair()}
          onCancel={onCancelPairing}
        />
      )}
      {runtime !== "api" &&
        !(data.online || []).includes(runtime) &&
        !pairing &&
        hosts.length > 0 && <StartHelper runtime={runtime} />}
      {hosts.length > 0 ? (
        <ul className="wk-members" aria-label="Helpers">
          {hosts.map((h) => (
            <HelperRow key={h.id} host={h} />
          ))}
        </ul>
      ) : (
        !pairing && (
          <p className="wk-note">
            No helper is paired yet. Pair one to run new threads on your Claude
            subscription.
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
  pairing: Pairing;
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
        <strong>1.</strong> On the computer where Claude Code is signed in, run
        this. It pairs one helper, once, in the next{" "}
        {minutes === 1 ? "minute" : `${minutes} minutes`}.
      </p>
      <Command text={pairing.command || ""} label="pair command" />
      <p className="ag-pairing-step">
        <strong>2.</strong> Keep it running at every login (or run it now with{" "}
        <code>run</code> in place of <code>install</code>):
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
function HelperRow({ host }: { host: FoundryAgentHost }) {
  const { id: ws, notify } = useWorkspace(),
    qc = useQueryClient(),
    now = useNow();
  const [revoking, setRevoking] = useState(false),
    [pending, setPending] = useState(false);
  const name = host.name || "Unnamed helper";
  const accounts = (host.runtimes || []).map((r) =>
    accountText(helperAccount(host, r), r),
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
            label={host.online ? "Running" : "Not running"}
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
          description="It stops running the Assistant’s turns at once, and a turn it is running ends. To run more, pair it again."
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
