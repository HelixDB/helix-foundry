import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LoaderCircle, Check, Monitor, Cloud, ArrowRight } from "lucide-react";
import { api, number } from "./api";
import { useWorkspace } from "./ui";
import { ChoiceCard } from "./choice-card";
import type {
  OnboardingState,
  Resource,
  ProviderSettings,
} from "../../../packages/shared/src";

// Polls faster while samples load, the AI describes the suggestion or the
// workspace builds, so the loading screens follow along.
const settling = (s?: OnboardingState) =>
  !!s?.sampleProgress ||
  (s?.ontologyRun?.data.onboardingSignature === s?.signature &&
    ["queued", "running"].includes(s?.ontologyRun?.data.status)) ||
  ["queued", "running"].includes(s?.buildRun?.data.status);
export function useSetup() {
  const { id } = useWorkspace();
  return useQuery({
    queryKey: [id, "onboarding"],
    queryFn: () => api<OnboardingState>(`/workspaces/${id}/onboarding`),
    refetchInterval: (q) => (settling(q.state.data) ? 1000 : 2500),
  });
}
export function useSetupAction() {
  const qc = useQueryClient();
  const { id } = useWorkspace();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const run = async (fn: () => Promise<unknown>) => {
    setPending(true);
    setError("");
    try {
      await fn();
      await qc.invalidateQueries({ queryKey: [id] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return { pending, error, run };
}
export function OperationProgress({
  operation,
  onRetry,
  onCancel,
}: {
  operation: Resource;
  onRetry?: () => void;
  onCancel?: () => void;
}) {
  const d = operation.data,
    p = d.progress || {},
    working = ["running", "queued"].includes(d.status),
    failed = ["failed", "canceled"].includes(d.status);
  return (
    <div className="setup-operation" aria-live="polite">
      <div className="operation-line">
        {working ? (
          <LoaderCircle size={15} className="spin" />
        ) : d.status === "succeeded" ? (
          <Check size={15} />
        ) : null}
        <strong>{operation.name}</strong>
        <span className="muted">
          {d.status === "succeeded"
            ? "Ready"
            : d.status === "canceled"
              ? "Stopped"
              : d.stage || d.status}
        </span>
      </div>
      {working && (
        <p className="muted">
          {p.message || d.events?.at(-1)?.message || "Waiting to start"}
          {p.records !== undefined
            ? ` · ${number(p.records)} records${p.relationships ? ` and ${number(p.relationships)} relationships` : ""} read`
            : p.bytes !== undefined
              ? ` · ${number(Math.round(p.bytes / 1024))} KB received`
              : ""}
        </p>
      )}
      {working && p.total > 0 && (
        <progress
          aria-label="Model download"
          value={p.completed || 0}
          max={p.total}
        />
      )}
      {failed && (
        <p role="alert" className="error-text">
          {d.error || "Progress has been saved. Retry when you’re ready."}
        </p>
      )}
      <div className="buttons">
        {failed && onRetry && (
          <button type="button" className="text-button" onClick={onRetry}>
            Retry
          </button>
        )}
        {working && onCancel && (
          <button type="button" className="text-button" onClick={onCancel}>
            Stop
          </button>
        )}
      </div>
    </div>
  );
}
// Docker Desktop shows its memory setting in decimal gigabytes.
const gigabytes = (bytes: number) => (bytes / 1e9).toFixed(bytes % 1e9 ? 1 : 0);
export function ProviderSetup({
  onContinue,
}: {
  onContinue?: (settings?: ProviderSettings) => void;
}) {
  const { id, role } = useWorkspace(),
    setup = useSetup(),
    action = useSetupAction();
  const q = useQuery({
    queryKey: [id, "provider"],
    queryFn: () => api(`/workspaces/${id}/provider`),
  });
  const [settings, setSettings] = useState<any>(null);
  useEffect(() => {
    if (q.data && setup.data && !settings) {
      const candidate = setup.data.providerJob?.data;
      setSettings({
        ...(candidate?.settings
          ? { ...candidate.settings, hasKey: candidate.hasKey }
          : q.data),
        apiKey: "",
      });
    }
  }, [q.data, setup.data, settings]);
  const s = settings || { provider: "local", model: "qwen3:4b" };
  const update = (k: string, v: any) => setSettings({ ...s, [k]: v });
  const job = setup.data?.providerJob,
    working = job && ["queued", "running"].includes(job.data.status);
  const owner = role === "owner";
  return (
    <form
      className="provider-setup"
      onSubmit={(event) => {
        event.preventDefault();
        if (onContinue) onContinue(owner ? s : undefined);
        else
          void action.run(async () => {
            await api(`/workspaces/${id}/onboarding/provider`, s);
            setSettings({ ...s, apiKey: "" });
          });
      }}
    >
      <fieldset
        disabled={!owner || action.pending || (!onContinue && !!working)}
        className="plain-fieldset"
      >
        <legend className="sr-only">Where should AI run?</legend>
        <div className="provider-options choice-options">
          {[
            ["local", "Local", "On your infrastructure."],
            [
              "claude",
              "Claude",
              "Use your Anthropic API key. Your schemas and data samples are sent to Anthropic.",
            ],
            [
              "openai",
              "OpenAI",
              "Use your OpenAI API key. Your schemas, data samples and answers are sent to OpenAI.",
            ],
            [
              "azure",
              "Azure OpenAI",
              "Use a deployment in your Azure subscription. Your schemas, data samples and answers are sent to that Azure resource.",
            ],
          ].map(([value, title, description]) => (
            <ChoiceCard
              key={value}
              name="provider"
              value={value}
              title={title}
              description={description}
              checked={s.provider === value}
              onChange={() =>
                setSettings({
                  ...s,
                  provider: value,
                  model:
                    value === "local"
                      ? "qwen3:4b"
                      : value === "claude"
                        ? "claude-sonnet-4-6"
                        : value === "openai"
                          ? "gpt-5.4-mini"
                          : "",
                  baseUrl: value === "local" ? q.data?.baseUrl : undefined,
                  apiKey: "",
                  hasKey: q.data?.provider === value && q.data?.hasKey,
                })
              }
              icon={
                value === "local" ? (
                  <Monitor className="provider-logo" aria-hidden="true" />
                ) : value === "azure" ? (
                  <Cloud className="provider-logo" aria-hidden="true" />
                ) : (
                  <img
                    className="provider-logo"
                    src={`/providers/${value}.svg`}
                    alt=""
                    aria-hidden="true"
                  />
                )
              }
            />
          ))}
        </div>
        {s.provider === "local" &&
          q.data?.localMemory &&
          !q.data.localMemory.sufficient && (
            <div className="notice" role="status">
              <div>
                <strong>Local AI may not fit in Docker’s memory</strong>
                <p>
                  Docker has {gigabytes(q.data.localMemory.totalBytes)} GB;
                  local AI next to your data needs at least{" "}
                  {gigabytes(q.data.localMemory.minimumBytes)} GB. When memory
                  runs out, questions fail or the database restarts. Choose
                  Claude or OpenAI, give Docker more memory, or run Ollama on
                  this computer instead of in Docker.
                </p>
              </div>
            </div>
          )}
        {s.provider === "azure" && (
          <>
            <label>
              Endpoint
              <input
                type="url"
                autoComplete="off"
                required={owner}
                placeholder="https://RESOURCE.openai.azure.com"
                value={s.baseUrl || ""}
                onChange={(e) =>
                  update("baseUrl", e.target.value.trim() || undefined)
                }
              />
            </label>
            <label>
              Deployment name
              <input
                type="text"
                autoComplete="off"
                required={owner}
                placeholder="gpt-5.4"
                value={s.model || ""}
                onChange={(e) => update("model", e.target.value.trim())}
              />
            </label>
            <p className="muted">
              Find the endpoint under Keys and Endpoint on your Azure OpenAI
              resource, and the deployment name under Deployments. Hands-free
              voice is not available with Azure OpenAI.
            </p>
          </>
        )}
        {s.provider !== "local" && (
          <label>
            API key
            <input
              type="password"
              autoComplete="off"
              required={owner && !s.hasKey}
              placeholder={
                s.hasKey
                  ? "Leave blank to keep your saved key"
                  : "Enter your provider API key"
              }
              value={s.apiKey || ""}
              onChange={(e) => update("apiKey", e.target.value)}
            />
          </label>
        )}
      </fieldset>
      {!owner && (
        <p className="muted">
          A workspace owner can configure AI. You can continue connecting data.
        </p>
      )}
      {action.error && (
        <p role="alert" className="error-box">
          {action.error}
        </p>
      )}
      <div className="setup-actions">
        {onContinue ? (
          <button
            className="button primary"
            type="submit"
            disabled={owner && (q.isPending || setup.isPending)}
          >
            Continue <ArrowRight size={15} />
          </button>
        ) : owner && !working ? (
          <button
            className="button primary"
            type="submit"
            disabled={action.pending}
          >
            {action.pending
              ? "Saving…"
              : setup.data?.providerReady
                ? "Test and save AI"
                : "Set up AI"}
          </button>
        ) : null}
      </div>
    </form>
  );
}
