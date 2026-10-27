import "./workspace.css";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Cloud,
  LoaderCircle,
  Monitor,
  Server,
  SlidersHorizontal,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import type {
  FoundryAi,
  FoundryAiProvider,
  FoundryAiProviderKind,
  FoundryAiRates,
  FoundryAiRole,
  FoundryOllamaServer,
} from "../../../shared/wfe";
import {
  Button,
  ConfirmDialog,
  Panel,
  Select,
  SkeletonText,
  cx,
} from "../../kit";
import { sentence } from "../../api";
import { useWorkspace } from "../../ui";
import * as AI from "./ai-api";
import { OwnKeyGate } from "./ai-gate";
import { isProRequired } from "./pro-gate";

// Settings › AI models: where the workspace's drafts,
// document reads and questions run. Hosted, that is the default unless an
// owner brings a key (Foundry Pro or Max); a local install starts on the
// provider its .env names, else the one it found on its machine (it looks
// again as the page opens), or none. An owner's save is tested before it
// is used: the page reads the setting every 1.5 s until the test ends, and
// says so when it passes or why it failed (the setting in force stays).
// Owners see what runs as the form's choice; everyone else reads one line.

// The kinds this site has a form for, and the provider message each sends
// given the Azure endpoint typed (normalized, or empty until it is one) and
// the custom and Ollama servers' fields and the Bedrock region and FIPS
// choice. A kind the WFE offers beyond these waits for its layer's form.
const FORMS: Partial<
  Record<
    FoundryAiProviderKind,
    (endpoint: string, fields: CardFields) => FoundryAiProvider
  >
> = {
  FOUNDRY_AI_KIND_ANTHROPIC: () => ({ anthropic: {} }),
  FOUNDRY_AI_KIND_OPENAI: () => ({ openai: {} }),
  FOUNDRY_AI_KIND_AZURE_OPENAI: (endpoint) => ({
    azureOpenai: { endpoint: AI.azureEndpoint(endpoint) ?? "" },
  }),
  FOUNDRY_AI_KIND_OPENAI_COMPATIBLE: (_, { server }) =>
    AI.compatProvider(server),
  FOUNDRY_AI_KIND_OLLAMA: (_, { ollama: o }) => ({
    ollama: {
      baseUrl: AI.baseUrl(ollamaUrl(o)) ?? "",
      contextTokens: o.contextTokens.trim(),
    },
  }),
  FOUNDRY_AI_KIND_BEDROCK: (_, { bedrock: { region, fips } }) => ({
    bedrock: AI.bedrockProvider(region, fips),
  }),
};
const AZURE = "FOUNDRY_AI_KIND_AZURE_OPENAI",
  COMPAT = "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE",
  OLLAMA = "FOUNDRY_AI_KIND_OLLAMA",
  BEDROCK = "FOUNDRY_AI_KIND_BEDROCK";
// A custom server's fields before any are typed.
const NO_SERVER = AI.compatFields();
// An Ollama server as its card edits it: one it names, or the address typed
// for another, and the context window models load with.
type OllamaFields = {
  server: AI.OllamaServer;
  url: string;
  contextTokens: string;
};
const ollamaUrl = (o: OllamaFields) =>
  o.server === "other" ? o.url : AI.OLLAMA_SERVERS[o.server].url;
// What the Ollama card has read of the server it asked (`url`): whether it
// is reading its models, and what it said.
type OllamaListing = {
  url: string;
  loading: boolean;
  server?: FoundryOllamaServer;
};
// How long an Ollama server's address stays as typed before its models are
// read.
const TYPING_MS = 500;
// A Bedrock card's region (empty until one is chosen) and FIPS choice.
type Bedrock = { region: string; fips: boolean };
const NO_REGION: Bedrock = { region: "", fips: false };
// What the cards with fields of their own hold.
type CardFields = {
  server: AI.CompatFields;
  ollama: OllamaFields;
  bedrock: Bedrock;
};
type Picks = Record<FoundryAiRole, AI.ModelPick>;

export function AiTab() {
  const { id, role, notify } = useWorkspace(),
    qc = useQueryClient();
  const query = useQuery(AI.aiQuery(id));
  const ai = query.data;
  // A local install that finds its own default looks again once as the
  // page opens, so a model pulled or a key given since it started is used
  // without a restart. The setting is read already, so a failure changes
  // nothing; an answer is kept unless the setting changed meanwhile, when
  // it is read again.
  const looked = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!ai?.detected || role !== "owner" || looked.current === id) return;
    looked.current = id;
    const before = qc.getQueryData(AI.aiKey(id));
    AI.detectAi(id).then(
      (next) => {
        if (qc.getQueryData(AI.aiKey(id)) === before)
          qc.setQueryData(AI.aiKey(id), next);
        else void qc.invalidateQueries({ queryKey: AI.aiKey(id) });
      },
      () => {},
    );
  }, [ai, id, role, qc]);
  // The save this tab saw being tested: once it is the setting, say so.
  const watched = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!ai) return;
    if (ai.pending) {
      watched.current = ai.pending.id;
      return;
    }
    if (watched.current && ai.active?.id === watched.current)
      notify(`AI models now run on ${ai.active.label || "the new provider"}.`);
    watched.current = undefined;
  }, [ai, notify]);
  if (query.isPending)
    return (
      <Panel title="AI models">
        <SkeletonText lines={4} />
      </Panel>
    );
  if (!ai)
    return (
      <Panel title="AI models" padded={false}>
        <div className="wk-pad">
          <p role="alert" className="hf-form-error">
            Couldn’t read the AI settings. {query.error?.message}
          </p>
          <Button size="sm" onClick={() => void query.refetch()}>
            Retry
          </Button>
        </div>
      </Panel>
    );
  return role === "owner" ? (
    // Keyed by the setting and the default's kind: once a save becomes the
    // setting, or the install finds another default, the form starts
    // afresh from it.
    <OwnerForm
      key={`${ai.active?.id ?? "default"} ${ai.default?.kind ?? "none"}`}
      ai={ai}
    />
  ) : (
    <Status ai={ai} />
  );
}

/** Where AI runs, for everyone but owners, who see it as the form's choice. */
function Status({ ai }: { ai: FoundryAi }) {
  return (
    <Panel
      title="AI models"
      subtitle="Where drafts, document reads and questions run."
    >
      <p className="wk-note">{AI.statusText(ai, LOCAL)}</p>
      {AI.paused(ai) && (
        <p role="alert" className="hf-form-error">
          {AI.PAUSED}
        </p>
      )}
    </Panel>
  );
}

/**
 * For owners, above their choice: no provider yet, a pause, a save under
 * test, the last save's failed test.
 */
function Notices({ ai }: { ai: FoundryAi }) {
  return (
    <>
      {AI.inForce(ai).source === "none" && (
        // A local install that looked for one says what it looked for.
        <p className="wk-note">{ai.detected?.summary || AI.NO_PROVIDER}</p>
      )}
      {AI.paused(ai) && (
        <p role="alert" className="hf-form-error">
          {AI.PAUSED}
        </p>
      )}
      {AI.testing(ai) && (
        <p role="status" className="wk-plan-confirming">
          <LoaderCircle size={14} className="hf-spin" aria-hidden />
          Testing {ai.pending!.label || "the provider"}: one tool call per task
          model and one structured answer…
        </p>
      )}
      {ai.lastFailed && (
        <p role="alert" className="hf-form-error">
          {AI.testFailureText(ai.lastFailed)} Your current settings stay active.
        </p>
      )}
    </>
  );
}

const defaultDisclosure = (ai: FoundryAi) => AI.defaultDisclosure(ai.default);

type Card = "default" | FoundryAiProviderKind;

/** The owner's choice of provider, its models and key, and its save. */
function OwnerForm({ ai }: { ai: FoundryAi }) {
  const { id, name, notify } = useWorkspace(),
    qc = useQueryClient();
  const now = AI.inForce(ai);
  // Ollama only in a local install, and first there: hosted, a native
  // Ollama server, which takes no key, would have to be public.
  const offers = (ai.offered || [])
    .flatMap((o) => {
      const kind = AI.PROVIDER_KINDS.find(
        (k) => k === o.kind && FORMS[k] && (LOCAL || k !== OLLAMA),
      );
      return kind ? [{ ...o, kind }] : [];
    })
    .sort((a, b) => Number(b.kind === OLLAMA) - Number(a.kind === OLLAMA));
  const activeKind =
    now.source === "workspace"
      ? AI.providerKind(now.config.provider)
      : undefined;
  // What a kind starts from when the WFE suggests models: those, else its
  // first priced model for every task; nothing for Azure or a custom server,
  // whose owner names each deployment or model, or Ollama, whose owner names
  // a model the server has.
  const suggestedFor = (kind: FoundryAiProviderKind): Picks | undefined => {
    if (kind === AZURE || kind === COMPAT || kind === OLLAMA) return undefined;
    const s = AI.rolePicks(offers.find((o) => o.kind === kind)?.suggested),
      first = AI.pricedModels(kind)[0]?.model || "";
    return Object.fromEntries(
      AI.ROLE_IDS.map((r) => [
        r,
        s[r].name ? s[r] : { name: first, table: {} },
      ]),
    ) as Picks;
  };
  // A kind starts from the setting's models when it is the setting's kind,
  // else from what the WFE suggests; an Azure deployment from the first model
  // Helix prices for it, a custom server's model with no cost counted, and
  // Ollama from the WFE's draft model, else the one a local install pulls,
  // at no cost.
  const startFor = (kind: FoundryAiProviderKind): Picks =>
    kind === activeKind && now.source === "workspace"
      ? AI.rolePicks(now.config.models)
      : (suggestedFor(kind) ??
        all(
          kind === COMPAT
            ? { name: "", free: {} }
            : kind === OLLAMA
              ? {
                  name:
                    offers.find((o) => o.kind === OLLAMA)?.suggested?.draft
                      ?.name || AI.OLLAMA_MODEL,
                  free: {},
                }
              : AI.pricedModels(kind)[0]
                ? { name: "", tableAs: AI.pricedModels(kind)[0]!.model }
                : { name: "", rates: {} },
        ));
  // The setting's card, else the default's, else the first kind offered.
  const first: Card =
    activeKind ?? (ai.default ? "default" : offers[0]?.kind) ?? "default";
  // The models a kind's fields start with: the first card's kind's, or the
  // first kind offered's while the default is chosen.
  const start = (() => {
    const k = first === "default" ? offers[0]?.kind : first;
    return k ? startFor(k) : AI.rolePicks();
  })();
  const perTaskFor = (k: Card, m: Picks) => {
    if (k === "default") return false;
    const s = suggestedFor(k);
    return !uniform(m) && !(s && same(m, s));
  };
  const [card, setCard] = useState<Card>(first),
    [models, setModels] = useState(start),
    [perTask, setPerTask] = useState(perTaskFor(first, start)),
    [endpoint, setEndpoint] = useState(
      now.source === "workspace" && activeKind === AZURE
        ? now.config.provider?.azureOpenai?.endpoint || ""
        : "",
    ),
    [server, setServer] = useState(
      activeKind === COMPAT && now.source === "workspace"
        ? AI.compatFields(now.config.provider)
        : NO_SERVER,
    ),
    [ollama, setOllama] = useState((): OllamaFields => {
      const o =
        now.source === "workspace" && activeKind === OLLAMA
          ? now.config.provider?.ollama
          : undefined;
      const server = o?.baseUrl ? AI.ollamaServer(o.baseUrl) : "bundled";
      return {
        server,
        url: server === "other" ? o?.baseUrl || "" : "",
        contextTokens:
          o?.contextTokens && o.contextTokens !== "0"
            ? o.contextTokens
            : String(AI.OLLAMA_CONTEXT.default),
      };
    }),
    [bedrock, setBedrock] = useState<Bedrock>(
      now.source === "workspace" && activeKind === BEDROCK
        ? {
            region: now.config.provider?.bedrock?.region || "",
            fips: !!now.config.provider?.bedrock?.fips,
          }
        : NO_REGION,
    ),
    [apiKey, setApiKey] = useState(""),
    [confirming, setConfirming] = useState(false),
    [pending, setPending] = useState(false),
    [error, setError] = useState<unknown>(null);
  const kind = card === "default" ? undefined : card,
    offer = offers.find((o) => o.kind === kind);

  // The Ollama server whose models the card lists: one it names at once,
  // another once its address is valid and stays as typed for TYPING_MS.
  const typedServer =
    ollama.server === "other" && !AI.endpointProblem(OLLAMA, ollama.url, LOCAL)
      ? AI.baseUrl(ollama.url)
      : undefined;
  const [settled, setSettled] = useState(typedServer);
  useEffect(() => {
    const t = window.setTimeout(() => setSettled(typedServer), TYPING_MS);
    return () => window.clearTimeout(t);
  }, [typedServer]);
  const asked =
    kind !== OLLAMA
      ? undefined
      : ollama.server === "other"
        ? typedServer === settled
          ? settled
          : undefined
        : AI.OLLAMA_SERVERS[ollama.server].url;
  const read = useQuery({
    ...AI.ollamaModelsQuery(id, asked || ""),
    enabled: LOCAL && !!asked,
  });
  const listing: OllamaListing | undefined = asked
    ? { url: asked, loading: read.isLoading, server: read.data }
    : undefined;
  // Its models follow the server's best (AI.ollamaBest) until the owner
  // chooses one: from the start, unless the setting is Ollama's, whose
  // models are kept, and again once another server than the setting's is
  // chosen.
  const settingServer =
    now.source === "workspace" && activeKind === OLLAMA
      ? AI.baseUrl(now.config.provider?.ollama?.baseUrl || "")
      : undefined;
  const [auto, setAuto] = useState(!settingServer);
  const best = AI.ollamaBest(listing?.server);
  useEffect(() => {
    if (kind !== OLLAMA || !auto || !best) return;
    setModels(all({ name: best, free: {} }));
    setPerTask(false);
  }, [kind, auto, best]);
  const editOllama = (o: OllamaFields) => {
    const url = AI.baseUrl(ollamaUrl(o));
    if (url !== AI.baseUrl(ollamaUrl(ollama))) {
      // Back on the setting's server: its models again.
      setAuto(url !== settingServer);
      if (url === settingServer) {
        const start = startFor(OLLAMA);
        setModels(start);
        setPerTask(perTaskFor(OLLAMA, start));
      }
    }
    setOllama(o);
  };

  const names = AI.roleModels(models);
  const choice: AI.Choice | undefined = kind && {
    kind,
    provider: FORMS[kind]!(endpoint, { server, ollama, bedrock }),
    models: names,
  };
  const reuse = !!choice && AI.canReuseKey(ai.active, choice),
    takesKey = !!offer && offer.key !== "FOUNDRY_AI_KEY_NONE",
    needsKey = offer?.key === "FOUNDRY_AI_KEY_REQUIRED" && !reuse;
  const endpointError =
    kind === AZURE && endpoint.trim()
      ? AI.endpointProblem(kind, endpoint, LOCAL)
      : undefined;
  // A custom server's key belongs to its base URL: another one clears the key
  // typed, and the setting's key stays with the setting's server.
  const editServer = (s: AI.CompatFields) => {
    if (AI.baseUrl(s.baseUrl) !== AI.baseUrl(server.baseUrl)) setApiKey("");
    setServer(s);
  };
  const keptElsewhere =
    kind === COMPAT &&
    activeKind === COMPAT &&
    !!ai.active?.hasKey &&
    !reuse &&
    ai.active?.label;
  // An Azure key belongs to its resource and draft deployment: another one
  // clears the key typed, and needs one.
  const editModels = (m: Picks) => {
    if (kind === AZURE && m.draft.name.trim() !== models.draft.name.trim())
      setApiKey("");
    if (kind === OLLAMA) setAuto(false);
    setModels(m);
  };
  const gated = !!kind && !!ai.proRequired;

  // What the change does to drafts and documents, for its confirmation.
  const readers = (c: Card): AI.Readers | undefined => {
    if (c !== "default")
      return {
        agent: AI.PROVIDERS[c].agent,
        endpoint: AI.idEndpoint(choice?.provider),
        extract: names.extract,
        verify: names.verify,
      };
    const k = AI.PROVIDER_KINDS.find((x) => x === ai.default?.kind);
    const m = AI.roleModels(ai.default?.models);
    return (
      k && {
        agent: AI.PROVIDERS[k].agent,
        extract: m.extract,
        verify: m.verify,
      }
    );
  };
  const from: AI.Readers | undefined =
    now.source === "workspace" && activeKind
      ? {
          agent: AI.PROVIDERS[activeKind].agent,
          endpoint: AI.idEndpoint(now.config.provider),
          ...pick(AI.roleModels(now.config.models)),
        }
      : now.source === "default"
        ? readers("default")
        : undefined;
  const to = readers(card);
  // Another Azure resource or custom server is a switch too.
  const switching =
    card === "default"
      ? now.source !== "default"
      : now.source !== "workspace" ||
        activeKind !== card ||
        from?.endpoint !== to?.endpoint;
  const reread = !!from && !!to && AI.documentsReread(from, to);
  const confirm = now.source !== "none" && (switching || reread);

  const choose = (c: Card) => {
    setCard(c);
    setApiKey("");
    setError(null);
    if (c === "default") return;
    const start = startFor(c);
    setModels(start);
    setPerTask(perTaskFor(c, start));
    if (c === OLLAMA)
      setAuto(
        !settingServer || AI.baseUrl(ollamaUrl(ollama)) !== settingServer,
      );
  };

  const save = async () => {
    setPending(true);
    setError(null);
    try {
      const next = await AI.saveAi(
        id,
        !choice
          ? { useDefault: {} }
          : {
              provider: choice.provider,
              models: Object.fromEntries(
                AI.ROLE_IDS.map((r) => [r, AI.sentPick(models[r])]),
              ),
              ...(apiKey.trim() && { apiKey: apiKey.trim() }),
            },
      );
      setApiKey("");
      setConfirming(false);
      qc.setQueryData(AI.aiKey(id), next);
      if (!choice)
        notify(`AI models now run on ${AI.runsOn("default", LOCAL)}.`);
    } catch (e) {
      setError(e);
    } finally {
      setPending(false);
    }
  };
  const failure = error ? (
    isProRequired(error) ? (
      <OwnKeyGate />
    ) : (
      <p role="alert" className="hf-form-error">
        {sentence((error as Error).message)}
      </p>
    )
  ) : null;
  const ready =
    !pending &&
    !gated &&
    (!kind ||
      (AI.ROLE_IDS.every((r) => !AI.modelProblem(kind, models[r])) &&
        !(kind === COMPAT
          ? (AI.endpointProblem(kind, server.baseUrl, LOCAL) ??
            AI.callsProblem(server.calls))
          : AI.endpointProblem(
              kind,
              kind === OLLAMA
                ? ollamaUrl(ollama)
                : kind === BEDROCK
                  ? bedrock.region
                  : endpoint,
              LOCAL,
            )) &&
        (kind !== OLLAMA || !AI.contextProblem(ollama.contextTokens)) &&
        (!needsKey || !!apiKey.trim()))) &&
    // The default in force already, unless a save waits to be tested:
    // choosing the default drops it.
    !(card === "default" && now.source === "default" && !AI.testing(ai));
  // An install that offers no kind this site has a form for, and has no
  // default, leaves nothing to choose.
  const none = !ai.default && !offers.length;

  return (
    <Panel
      title="Choose where AI runs"
      subtitle={
        LOCAL
          ? "A provider you choose is tested before Foundry uses it."
          : `Run ${name}’s AI on the default, or bring your own key. A key is tested before Foundry uses it.`
      }
      padded={false}
    >
      <form
        className="wk-pad wk-ai-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (!ready || none) return;
          if (confirm) setConfirming(true);
          else void save();
        }}
      >
        <Notices ai={ai} />
        <fieldset className="plain-fieldset" disabled={pending}>
          <legend className="hf-sr-only">Where AI runs</legend>
          <div className="choice-options provider-options">
            {ai.default && (
              <ChoiceCard
                title={LOCAL ? AI.INSTALL_DEFAULT : AI.HOUSE.title}
                description={
                  LOCAL
                    ? `${ai.detected?.summary || `${ai.default.label || "Its provider"}, from .env.`} ${defaultDisclosure(ai)}`
                    : AI.HOUSE.disclosure
                }
                icon={
                  LOCAL ? (
                    <Monitor strokeWidth={1.5} />
                  ) : (
                    <img src="/providers/claude.svg" alt="" />
                  )
                }
                checked={card === "default"}
                onChange={() => choose("default")}
              />
            )}
            {offers.map((o) => (
              <ChoiceCard
                key={o.kind}
                title={AI.PROVIDERS[o.kind].title}
                description={AI.disclosure(
                  o.kind,
                  FORMS[o.kind]!(o.kind === card ? endpoint : "", {
                    server: o.kind === card ? server : NO_SERVER,
                    ollama,
                    bedrock: o.kind === card ? bedrock : NO_REGION,
                  }),
                  o.kind === card ? Object.values(names) : [],
                )}
                icon={
                  AI.PROVIDERS[o.kind].logo ? (
                    <img src={AI.PROVIDERS[o.kind].logo} alt="" />
                  ) : o.kind === COMPAT ? (
                    <SlidersHorizontal strokeWidth={1.5} />
                  ) : o.kind === OLLAMA ? (
                    <Server strokeWidth={1.5} />
                  ) : (
                    <Cloud strokeWidth={1.5} />
                  )
                }
                checked={card === o.kind}
                onChange={() => choose(o.kind)}
              />
            ))}
          </div>
        </fieldset>
        {gated ? (
          <OwnKeyGate />
        ) : (
          kind && (
            <>
              {kind === AZURE && (
                <label className="hf-field">
                  <span>Endpoint</span>
                  <input
                    type="url"
                    inputMode="url"
                    autoComplete="off"
                    spellCheck={false}
                    required
                    disabled={pending}
                    placeholder="https://<resource>.openai.azure.com"
                    value={endpoint}
                    aria-invalid={!!endpointError}
                    onChange={(e) => {
                      if (
                        AI.azureEndpoint(e.target.value) !==
                        AI.azureEndpoint(endpoint)
                      )
                        setApiKey("");
                      setEndpoint(e.target.value);
                    }}
                  />
                  <small>
                    {endpointError ??
                      "The resource’s endpoint, from Keys and Endpoint in the Azure portal."}
                  </small>
                </label>
              )}
              {kind === COMPAT && (
                <CompatServer
                  server={server}
                  disabled={pending}
                  onServer={editServer}
                />
              )}
              {kind === OLLAMA && (
                <OllamaServer
                  value={ollama}
                  disabled={pending}
                  onChange={editOllama}
                />
              )}
              {kind === BEDROCK && (
                <Region
                  bedrock={bedrock}
                  models={Object.values(names)}
                  disabled={pending}
                  onChange={setBedrock}
                />
              )}
              <Models
                kind={kind}
                models={models}
                suggested={suggestedFor(kind)}
                perTask={perTask}
                listing={listing}
                disabled={pending}
                onModels={editModels}
                onPerTask={(on) => {
                  if (kind === OLLAMA) setAuto(false);
                  setPerTask(on);
                  if (!on && !uniform(models))
                    editModels(suggestedFor(kind) ?? all(models.draft));
                }}
              />
              {kind === OLLAMA && (
                <OllamaContext
                  value={ollama}
                  disabled={pending}
                  onChange={setOllama}
                />
              )}
              {kind === OLLAMA && (
                <p className="wk-note">
                  The test checks that the model calls Foundry’s tools and
                  answers in JSON, not how well it drafts: a small local model
                  may stop a draft early.
                </p>
              )}
              {takesKey && (
                <label className="hf-field">
                  <span>API key</span>
                  <input
                    type="password"
                    autoComplete="off"
                    spellCheck={false}
                    required={needsKey}
                    disabled={pending}
                    placeholder={
                      reuse
                        ? `Leave blank to keep ${ai.active?.keyHint || "the saved key"}`
                        : AI.PROVIDERS[kind].keyPlaceholder
                    }
                    value={apiKey}
                    onChange={(e) => setApiKey(e.target.value)}
                  />
                  <small>
                    {kind === COMPAT &&
                      (keptElsewhere
                        ? `The saved key is sent only to ${keptElsewhere}; enter it again if this server needs one. `
                        : "Optional: leave it blank for a server that takes none. ")}
                    Stored encrypted, and never shown again.
                  </small>
                </label>
              )}
            </>
          )
        )}
        {!confirming && failure}
        {none ? (
          <p className="wk-note">
            This install offers no AI provider to choose here.
          </p>
        ) : (
          <div className="hf-form-actions">
            <Button
              type="submit"
              variant="primary"
              pending={pending}
              disabled={!ready}
            >
              {card === "default"
                ? LOCAL
                  ? "Use the install default"
                  : "Use the default"
                : "Test and save"}
            </Button>
          </div>
        )}
      </form>
      {confirming && (
        <ConfirmDialog
          title="Change where AI runs?"
          description={[
            switching &&
              `New drafts, document reads and questions run on ${AI.runsOn(card, LOCAL, choice?.provider)}.`,
            reread &&
              (card === OLLAMA
                ? "Documents are read again by Ollama on their next load."
                : `Documents are read again at ${card === "default" ? (LOCAL ? "the install default’s" : "Helix’s") : "your"} cost on their next load.`),
          ]
            .filter(Boolean)
            .join(" ")}
          confirmLabel={card === "default" ? "Switch" : "Test and save"}
          pending={pending}
          onConfirm={save}
          onCancel={() => {
            setConfirming(false);
            setError(null);
          }}
        >
          {failure}
        </ConfirmDialog>
      )}
    </Panel>
  );
}

/**
 * A Bedrock card's region: one AWS lists for Bedrock, or another typed, and
 * the region's FIPS endpoint where AWS has one. It says that a cross-region
 * inference profile among the models may run them elsewhere, and that AWS
 * GovCloud and China have no global ones.
 */
function Region({
  bedrock,
  models,
  disabled,
  onChange,
}: {
  bedrock: Bedrock;
  models: readonly string[];
  disabled: boolean;
  onChange: (b: Bedrock) => void;
}) {
  // A region typed (a setting's the list lacks, too) stays typed, even one
  // the list has, so the field doesn't turn into the list as it is typed.
  const [typed, setTyped] = useState(
    !!bedrock.region &&
      !AI.BEDROCK_REGIONS.some((r) => r.region === bedrock.region),
  );
  const region = bedrock.region.trim();
  const problem = region
    ? AI.endpointProblem(BEDROCK, region, LOCAL)
    : undefined;
  const help =
    /^(us-gov|cn)-/.test(region) &&
    models.some((m) => m.trim().startsWith("global."))
      ? `Bedrock has no global inference profiles in ${region}: choose models it offers there.`
      : "Where Foundry calls Bedrock, under your AWS account. A cross-region inference profile, like global.… or eu.…, may run the models in other regions.";
  return (
    <>
      <label className="hf-field">
        <span>Region</span>
        <Select
          label="Region"
          value={typed ? OTHER : bedrock.region}
          placeholder="Choose a region"
          disabled={disabled}
          onChange={(v) => {
            setTyped(v === OTHER);
            onChange({ ...bedrock, region: v === OTHER ? "" : v });
          }}
          options={[
            ...AI.BEDROCK_REGIONS.map((r) => ({
              value: r.region,
              label: r.region,
              hint: r.name,
            })),
            {
              value: OTHER,
              label: "Another region",
              hint: "Enter its code",
            },
          ]}
        />
        {!typed && <small>{help}</small>}
      </label>
      {typed && (
        <label className="hf-field">
          <span>Region code</span>
          <input
            autoComplete="off"
            spellCheck={false}
            disabled={disabled}
            placeholder="us-east-1"
            value={bedrock.region}
            aria-invalid={!!problem}
            onChange={(e) => onChange({ ...bedrock, region: e.target.value })}
          />
          <small>{problem ?? help}</small>
        </label>
      )}
      {AI.fipsOffered(region) && (
        <label className="checkbox">
          <input
            type="checkbox"
            checked={bedrock.fips}
            disabled={disabled}
            onChange={(e) => onChange({ ...bedrock, fips: e.target.checked })}
          />
          Use the region’s FIPS endpoint
        </label>
      )}
    </>
  );
}

/**
 * A kind's models: one for every task (or, where the WFE suggests them, the
 * recommended one per task), or one chosen per task. Azure OpenAI's are
 * deployments; Amazon Bedrock's are model or inference profile IDs.
 */
function Models({
  kind,
  models,
  suggested,
  perTask,
  listing,
  disabled,
  onModels,
  onPerTask,
}: {
  kind: FoundryAiProviderKind;
  models: Picks;
  suggested?: Picks;
  perTask: boolean;
  // Ollama's: what its server has.
  listing?: OllamaListing;
  disabled: boolean;
  onModels: (m: Picks) => void;
  onPerTask: (on: boolean) => void;
}) {
  const recommended = !!suggested && same(models, suggested),
    one = uniform(models);
  const Field =
    kind === AZURE
      ? Deployment
      : kind === COMPAT
        ? ServerModel
        : kind === OLLAMA
          ? OllamaModel
          : kind === BEDROCK
            ? BedrockModel
            : CatalogModel;
  return (
    <>
      {/* Ollama's fields start afresh for another server. */}
      {!perTask && (
        <Field
          key={listing?.url}
          kind={kind}
          title={kind === AZURE ? "Every task" : "Model"}
          pick={recommended ? undefined : (one ?? models.draft)}
          suggested={suggested}
          every
          listing={listing}
          disabled={disabled}
          onPick={(p) => onModels(p ? all(p) : suggested!)}
        />
      )}
      <label className="checkbox">
        <input
          type="checkbox"
          checked={perTask}
          disabled={disabled}
          onChange={(e) => onPerTask(e.target.checked)}
        />
        {kind === AZURE
          ? "Choose a deployment per task"
          : "Choose a model per task"}
      </label>
      {perTask &&
        AI.ROLE_IDS.map((r) => (
          <Field
            key={`${r} ${listing?.url ?? ""}`}
            kind={kind}
            title={AI.ROLES[r]}
            pick={models[r]}
            listing={listing}
            disabled={disabled}
            onPick={(p) => p && onModels({ ...models, [r]: p })}
          />
        ))}
    </>
  );
}

type FieldProps = {
  kind: FoundryAiProviderKind;
  // The field's name: a task's, or the one for every task.
  title: string;
  // Undefined while the recommended model per task is chosen.
  pick?: AI.ModelPick;
  suggested?: Picks;
  // Whether it sets every task's model.
  every?: boolean;
  // Ollama's: what its server has.
  listing?: OllamaListing;
  disabled: boolean;
  onPick: (p?: AI.ModelPick) => void;
};

// The Select values that aren't a model or a region: neither has spaces.
const RECOMMENDED = "",
  OTHER = " other";
const priceOptions = (kind: FoundryAiProviderKind) =>
  AI.pricedModels(kind).map((r) => ({
    value: r.model,
    label: r.model,
    hint: AI.priceText(r),
  }));

/** A model from Helix's price table, or another one with its prices. */
function CatalogModel({
  kind,
  title,
  pick,
  suggested,
  every,
  disabled,
  onPick,
}: FieldProps) {
  const listed =
    !!pick &&
    "table" in pick &&
    AI.pricedModels(kind).some((r) => r.model === pick.name);
  const value = !pick ? RECOMMENDED : listed ? pick.name : OTHER;
  // Another model is priced by its rates, even one saved by its table's row
  // that the table no longer has.
  const other =
    pick && ("rates" in pick ? pick : { name: pick.name, rates: {} });
  const named = other?.name.trim()
    ? AI.nameProblem(kind, other.name)
    : undefined;
  return (
    <div role="group" aria-label={title} className="wk-ai-model">
      <label className="hf-field">
        <span>{title}</span>
        <Select
          label={title}
          value={value}
          disabled={disabled}
          onChange={(v) =>
            onPick(
              v === RECOMMENDED
                ? undefined
                : v === OTHER
                  ? { name: "", rates: {} }
                  : { name: v, table: {} },
            )
          }
          options={[
            ...(suggested
              ? [{ value: RECOMMENDED, label: "Recommended for each task" }]
              : []),
            ...priceOptions(kind),
            {
              value: OTHER,
              label: "Another model",
              hint: "Enter its name and prices",
            },
          ]}
        />
        {every && value !== OTHER && (
          <small>
            {!pick
              ? AI.ROLE_IDS.map(
                  (r) => `${AI.ROLES[r]}: ${suggested![r].name}`,
                ).join(" · ")
              : `Every task runs on it: ${AI.pickPriceText(kind, pick)}.`}
          </small>
        )}
      </label>
      {value === OTHER && other && (
        <>
          <label className="hf-field">
            <span>Model name</span>
            <input
              autoComplete="off"
              spellCheck={false}
              disabled={disabled}
              value={other.name}
              aria-invalid={!!named}
              onChange={(e) => onPick({ ...other, name: e.target.value })}
            />
            {named && <small>{named}</small>}
          </label>
          <Rates
            pick={other}
            problem={AI.ratesProblem(other.rates)}
            disabled={disabled}
            onPick={onPick}
          />
        </>
      )}
    </div>
  );
}

/**
 * An Azure deployment: its name, and the model it runs for Helix's prices,
 * or its own prices.
 */
function Deployment({
  kind,
  title,
  pick,
  every,
  disabled,
  onPick,
}: FieldProps) {
  const p = pick ?? { name: "", rates: {} };
  const named = p.name.trim() ? AI.nameProblem(kind, p.name) : undefined;
  return (
    <div role="group" aria-label={title} className="wk-ai-model">
      <label className="hf-field">
        <span>{every ? "Deployment" : `${title} deployment`}</span>
        <input
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          placeholder="The deployment’s name in Azure"
          value={p.name}
          aria-invalid={!!named}
          onChange={(e) => onPick({ ...p, name: e.target.value })}
        />
        {named && <small>{named}</small>}
      </label>
      <label className="hf-field">
        <span>{every ? "Priced as" : `${title} priced as`}</span>
        <Select
          label={every ? "Priced as" : `${title} priced as`}
          value={"tableAs" in p ? p.tableAs : OTHER}
          disabled={disabled}
          onChange={(v) =>
            onPick(
              v === OTHER
                ? { name: p.name, rates: {} }
                : { name: p.name, tableAs: v },
            )
          }
          options={[
            ...priceOptions(kind),
            {
              value: OTHER,
              label: "Enter prices",
              hint: "For a model Helix has no price for",
            },
          ]}
        />
        {!("rates" in p) && (
          <small>
            The model the deployment runs, for Helix’s prices
            {AI.pickPriceText(kind, p) && `: ${AI.pickPriceText(kind, p)}`}.
          </small>
        )}
      </label>
      {"rates" in p && (
        <Rates
          pick={p}
          problem={AI.ratesProblem(p.rates)}
          disabled={disabled}
          onPick={onPick}
        />
      )}
    </div>
  );
}

// The cost Select's values for a custom server's model.
const FREE = "free",
  RATES = "rates";

/**
 * A custom server's model: its name or alias, and its own prices or no cost
 * counted (Helix's table has no row for a model on someone else's server).
 */
function ServerModel({ kind, title, pick, disabled, onPick }: FieldProps) {
  const p = pick ?? { name: "", free: {} };
  const named = p.name.trim() ? AI.nameProblem(kind, p.name) : undefined;
  return (
    <div role="group" aria-label={title} className="wk-ai-model">
      <label className="hf-field">
        <span>Model name</span>
        <input
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          placeholder="Model ID or routing alias"
          value={p.name}
          aria-invalid={!!named}
          onChange={(e) => onPick({ ...p, name: e.target.value })}
        />
        {named && <small>{named}</small>}
      </label>
      <label className="hf-field">
        <span>Cost</span>
        <Select
          label="Cost"
          value={"rates" in p ? RATES : FREE}
          disabled={disabled}
          onChange={(v) =>
            onPick(
              v === RATES
                ? { name: p.name, rates: {} }
                : { name: p.name, free: {} },
            )
          }
          options={[
            {
              value: FREE,
              label: "Don’t track cost",
              hint: "For a server whose calls cost you nothing per token",
            },
            {
              value: RATES,
              label: "Enter prices",
              hint: "What the server bills, for Foundry’s cost limits",
            },
          ]}
        />
        {!("rates" in p) && (
          <small>
            No cost is counted, so a run’s cost limit doesn’t stop it; its step
            and time limits do.
          </small>
        )}
      </label>
      {"rates" in p && (
        <Rates
          pick={p}
          problem={AI.ratesProblem(p.rates)}
          disabled={disabled}
          onPick={onPick}
        />
      )}
    </div>
  );
}

/**
 * A custom OpenAI-compatible server: where it answers, the API it speaks,
 * and how many calls it takes at once. Hosted, it must be https on a public
 * address; locally, a server on this computer is host.docker.internal.
 */
function CompatServer({
  server,
  disabled,
  onServer,
}: {
  server: AI.CompatFields;
  disabled: boolean;
  onServer: (s: AI.CompatFields) => void;
}) {
  const urlError = server.baseUrl.trim()
      ? AI.endpointProblem(COMPAT, server.baseUrl, LOCAL)
      : undefined,
    callsError = AI.callsProblem(server.calls);
  return (
    <>
      <label className="hf-field">
        <span>Base URL</span>
        <input
          type="url"
          inputMode="url"
          autoComplete="off"
          spellCheck={false}
          required
          disabled={disabled}
          placeholder={
            LOCAL
              ? "http://host.docker.internal:1234/v1"
              : "https://llm.example.com/v1"
          }
          value={server.baseUrl}
          aria-invalid={!!urlError}
          onChange={(e) => onServer({ ...server, baseUrl: e.target.value })}
        />
        <small>
          {urlError ??
            (AI.versionless(server.baseUrl)
              ? "Most servers answer under /v1: add it unless yours says otherwise."
              : LOCAL
                ? "Including /v1. A server on this computer is host.docker.internal, not localhost: LM Studio http://host.docker.internal:1234/v1, vLLM :8000/v1, Ollama :11434/v1."
                : "The server’s OpenAI-compatible API, including /v1. It must be https on a public address.")}
        </small>
      </label>
      <label className="hf-field">
        <span>API format</span>
        <Select
          label="API format"
          value={server.api}
          disabled={disabled}
          onChange={(api) => onServer({ ...server, api })}
          options={(
            Object.keys(AI.COMPAT_APIS) as (keyof typeof AI.COMPAT_APIS)[]
          ).map((api) => ({
            value: api,
            label: AI.COMPAT_APIS[api].title,
            hint: AI.COMPAT_APIS[api].hint,
          }))}
        />
        <small>
          The format the server supports. Its models must call tools and answer
          in structured JSON; small local models may stop drafts early.
        </small>
      </label>
      <label className="hf-field">
        <span>Calls at once</span>
        <input
          inputMode="numeric"
          autoComplete="off"
          disabled={disabled}
          value={server.calls}
          aria-invalid={!!callsError}
          onChange={(e) => onServer({ ...server, calls: e.target.value })}
        />
        <small>
          {callsError ??
            "How many calls Foundry sends the server at once. Lower it for a server on one GPU."}
        </small>
      </label>
    </>
  );
}

/**
 * A model the Ollama server has: one it lists (those Foundry can run first,
 * each with its size; one it can't, with why, not to be chosen), or another
 * by name, such as one not pulled yet. While the list is read, the model
 * chosen stays chosen; a server that can't list its models says why. It
 * runs at no cost counted.
 */
function OllamaModel({
  kind,
  title,
  pick,
  every,
  listing,
  disabled,
  onPick,
}: FieldProps) {
  const name = pick?.name ?? "",
    trimmed = name.trim();
  const choices = AI.ollamaChoices(listing?.server),
    problem = listing?.server?.problem;
  const chosen = choices.find((c) => c.name === name);
  // A name typed stays typed, even one the server lists, so the field
  // doesn't turn into the list as it is typed.
  const [typed, setTyped] = useState(false);
  const reading = !!listing?.loading && !!name && !chosen;
  const value = typed || !(chosen || reading) ? OTHER : name;
  const named = trimmed ? AI.nameProblem(kind, name) : undefined;
  const cloud = AI.ollamaCloud(name)
    ? "Ollama runs this model on ollama.com, so what it is sent leaves this machine. The bundled server runs no cloud models."
    : undefined;
  // The server listed its models, and this one isn't among them.
  const unpulled =
    !!listing?.server &&
    !problem &&
    !!trimmed &&
    !choices.some((c) => c.name === trimmed || c.name === `${trimmed}:latest`);
  const hint =
    value === OTHER
      ? named ||
        problem ||
        chosen?.unusable ||
        cloud ||
        (unpulled
          ? `The server hasn’t pulled ${trimmed}. Pull it there with ollama pull ${trimmed} before testing it.`
          : every
            ? `A model the server has pulled that calls tools, like ${AI.OLLAMA_MODEL}. No cost is counted.`
            : undefined)
      : chosen?.unusable ||
        problem ||
        cloud ||
        (reading
          ? "Reading the models the server has…"
          : every
            ? "No cost is counted."
            : undefined);
  return (
    <div role="group" aria-label={title} className="wk-ai-model">
      <label className="hf-field">
        <span>{title}</span>
        <Select
          label={title}
          value={value}
          disabled={disabled}
          onChange={(v) => {
            setTyped(v === OTHER);
            onPick({ name: v === OTHER ? "" : v, free: {} });
          }}
          options={[
            ...(reading ? [{ value: name, label: name }] : []),
            ...choices.map((c) => ({
              value: c.name,
              label: c.label,
              hint: c.unusable || undefined,
              disabled: !!c.unusable,
            })),
            { value: OTHER, label: "Another model", hint: "Enter its name" },
          ]}
        />
        {value !== OTHER && hint && <small>{hint}</small>}
      </label>
      {value === OTHER && (
        <label className="hf-field">
          <span>Model name</span>
          <input
            autoComplete="off"
            spellCheck={false}
            disabled={disabled}
            placeholder={AI.OLLAMA_MODEL}
            value={name}
            aria-invalid={!!named}
            onChange={(e) => {
              setTyped(true);
              onPick({ name: e.target.value, free: {} });
            }}
          />
          {hint && <small>{hint}</small>}
        </label>
      )}
    </div>
  );
}

const OLLAMA_SERVER_OPTIONS: {
  value: AI.OllamaServer;
  label: string;
  hint: string;
}[] = [
  {
    value: "bundled",
    label: AI.OLLAMA_SERVERS.bundled.label,
    hint: AI.OLLAMA_SERVERS.bundled.url,
  },
  {
    value: "host",
    label: AI.OLLAMA_SERVERS.host.label,
    hint: AI.OLLAMA_SERVERS.host.url,
  },
  { value: "other", label: "Other", hint: "Enter its address" },
];

/** Where the Ollama server is: one the card names, or another's address. */
function OllamaServer({
  value,
  disabled,
  onChange,
}: {
  value: OllamaFields;
  disabled: boolean;
  onChange: (v: OllamaFields) => void;
}) {
  const urlError = value.url.trim()
    ? AI.endpointProblem(OLLAMA, value.url, LOCAL)
    : undefined;
  return (
    <>
      <label className="hf-field">
        <span>Server</span>
        <Select
          label="Server"
          value={value.server}
          disabled={disabled}
          onChange={(server) => onChange({ ...value, server })}
          options={OLLAMA_SERVER_OPTIONS}
        />
        <small>
          {
            {
              bundled:
                "The Ollama that runs with Foundry, from the compose file’s ollama profile: docker compose --profile ollama up -d.",
              host: `Ollama installed on this computer; ${AI.OLLAMA_LINUX_HOST}`,
              other: "An Ollama server this computer can reach.",
            }[value.server]
          }
        </small>
      </label>
      {value.server === "other" && (
        <label className="hf-field">
          <span>Server address</span>
          <input
            type="url"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
            required
            disabled={disabled}
            placeholder="http://gpu.example.lan:11434"
            value={value.url}
            aria-invalid={!!urlError}
            onChange={(e) => onChange({ ...value, url: e.target.value })}
          />
          {urlError && <small>{urlError}</small>}
        </label>
      )}
    </>
  );
}

/** The context window an Ollama server loads models with. */
function OllamaContext({
  value,
  disabled,
  onChange,
}: {
  value: OllamaFields;
  disabled: boolean;
  onChange: (v: OllamaFields) => void;
}) {
  const problem = AI.contextProblem(value.contextTokens);
  return (
    <label className="hf-field">
      <span>Context window</span>
      <input
        inputMode="numeric"
        autoComplete="off"
        disabled={disabled}
        value={value.contextTokens}
        aria-invalid={!!problem}
        onChange={(e) => onChange({ ...value, contextTokens: e.target.value })}
      />
      <small>{problem ?? AI.OLLAMA_CONTEXT_NOTE}</small>
    </label>
  );
}

/**
 * A Bedrock model: one from Helix's price table, or another model or
 * inference profile by its ID. Another ID is priced by the table where it
 * has a row (a cross-region profile by its model's), else as the table
 * model it runs (an application inference profile's) or at its own rates.
 */
function BedrockModel({
  kind,
  title,
  pick,
  suggested,
  every,
  disabled,
  onPick,
}: FieldProps) {
  const listed = (p?: AI.ModelPick) =>
    !!p &&
    "table" in p &&
    AI.pricedModels(kind).some((r) => r.model === p.name);
  // An ID typed stays typed, even one the table lists, so the field doesn't
  // turn into the list as it is typed.
  const [typed, setTyped] = useState(!!pick && !listed(pick));
  const value = !pick
    ? RECOMMENDED
    : typed || !listed(pick)
      ? OTHER
      : pick.name;
  const row =
    pick && "table" in pick ? AI.pricedRow(kind, pick.name.trim()) : undefined;
  // An ID the table can't price is priced by its rates, even one saved by a
  // row the table no longer has.
  const other =
    pick && ("table" in pick && !row ? { name: pick.name, rates: {} } : pick);
  const named = other?.name.trim()
    ? AI.nameProblem(kind, other.name)
    : undefined;
  // A new ID keeps the price chosen, unless the table prices it.
  const rename = (p: AI.ModelPick, name: string): AI.ModelPick =>
    AI.pricedRow(kind, name.trim())
      ? { name, table: {} }
      : "tableAs" in p
        ? { name, tableAs: p.tableAs }
        : { name, rates: "rates" in p ? p.rates : {} };
  const pricedAs = every ? "Priced as" : `${title} priced as`;
  return (
    <div role="group" aria-label={title} className="wk-ai-model">
      <label className="hf-field">
        <span>{title}</span>
        <Select
          label={title}
          value={value}
          disabled={disabled}
          onChange={(v) => {
            setTyped(v === OTHER);
            onPick(
              v === RECOMMENDED
                ? undefined
                : v === OTHER
                  ? { name: "", rates: {} }
                  : { name: v, table: {} },
            );
          }}
          options={[
            ...(suggested
              ? [{ value: RECOMMENDED, label: "Recommended for each task" }]
              : []),
            ...priceOptions(kind),
            {
              value: OTHER,
              label: "Another model ID",
              hint: "A model or inference profile",
            },
          ]}
        />
        {every && value !== OTHER && (
          <small>
            {!pick
              ? AI.ROLE_IDS.map(
                  (r) => `${AI.ROLES[r]}: ${suggested![r].name}`,
                ).join(" · ")
              : `Every task runs on it: ${AI.pickPriceText(kind, pick)}.`}
          </small>
        )}
      </label>
      {value === OTHER && other && (
        <>
          <label className="hf-field">
            <span>Model ID</span>
            <input
              autoComplete="off"
              spellCheck={false}
              disabled={disabled}
              placeholder="us.anthropic.claude-…"
              value={other.name}
              aria-invalid={!!named}
              onChange={(e) => onPick(rename(other, e.target.value))}
            />
            <small>
              {named ??
                "It must support tool use. Inference-profile IDs work, like us.… or global.…"}
            </small>
          </label>
          {row ? (
            <p className="wk-note">
              Priced as {row.model}, from Helix’s prices: {AI.priceText(row)}.
            </p>
          ) : (
            <>
              <label className="hf-field">
                <span>{pricedAs}</span>
                <Select
                  label={pricedAs}
                  value={"tableAs" in other ? other.tableAs : OTHER}
                  disabled={disabled}
                  onChange={(v) =>
                    onPick(
                      v === OTHER
                        ? { name: other.name, rates: {} }
                        : { name: other.name, tableAs: v },
                    )
                  }
                  options={[
                    ...priceOptions(kind),
                    {
                      value: OTHER,
                      label: "Enter prices",
                      hint: "For a model Helix has no price for",
                    },
                  ]}
                />
                {"tableAs" in other && (
                  <small>
                    The model the ID runs, for Helix’s prices
                    {AI.pickPriceText(kind, other) &&
                      `: ${AI.pickPriceText(kind, other)}`}
                    .
                  </small>
                )}
              </label>
              {"rates" in other && (
                <Rates
                  pick={other}
                  problem={AI.ratesProblem(other.rates)}
                  disabled={disabled}
                  onPick={onPick}
                />
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}

/** A model's own prices, in dollars per million tokens. */
function Rates({
  pick,
  problem,
  disabled,
  onPick,
}: {
  pick: AI.ModelPick & { rates: FoundryAiRates };
  problem?: string;
  disabled: boolean;
  onPick: (p: AI.ModelPick) => void;
}) {
  const fields: [keyof FoundryAiRates, string][] = [
    ["inputUsdPerMtok", "Input"],
    ["outputUsdPerMtok", "Output"],
    ["cachedInputUsdPerMtok", "Cached input"],
  ];
  const typed = fields.some(([f]) => (pick.rates[f] || "").trim());
  return (
    <div className="wk-ai-rates">
      {fields.map(([f, label]) => (
        <label key={f} className="hf-field">
          <span>{label}</span>
          <input
            inputMode="decimal"
            autoComplete="off"
            placeholder={f === "cachedInputUsdPerMtok" ? "Optional" : "0.00"}
            disabled={disabled}
            value={pick.rates[f] || ""}
            onChange={(e) =>
              onPick({ ...pick, rates: { ...pick.rates, [f]: e.target.value } })
            }
          />
        </label>
      ))}
      <small>
        {typed && problem
          ? problem
          : "Dollars per million tokens, as the provider bills them."}
      </small>
    </div>
  );
}

const all = (p: AI.ModelPick) =>
  Object.fromEntries(AI.ROLE_IDS.map((r) => [r, p])) as Picks;
const uniform = (m: Picks) =>
  AI.ROLE_IDS.every((r) => AI.samePick(m[r], m.draft)) ? m.draft : undefined;
const same = (a: Picks, b: Picks) =>
  AI.ROLE_IDS.every((r) => AI.samePick(a[r], b[r]));
const pick = (m: Record<FoundryAiRole, string>) => ({
  extract: m.extract,
  verify: m.verify,
});

/** helix-foundry's ChoiceCard (apps/web/src/choice-card.tsx): one radio. */
function ChoiceCard({
  title,
  description,
  icon,
  checked,
  onChange,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  checked: boolean;
  onChange: () => void;
}) {
  const descriptionId = useId();
  return (
    <label className={cx("choice-card", checked && "selected")}>
      <span className="choice-card-icon" aria-hidden="true">
        {icon}
      </span>
      <span className="choice-card-heading">
        <input
          type="radio"
          name="provider"
          checked={checked}
          onChange={onChange}
          aria-label={title}
          aria-describedby={descriptionId}
        />
        <strong>{title}</strong>
      </span>
      <span id={descriptionId} className="choice-card-description">
        {description}
      </span>
    </label>
  );
}
