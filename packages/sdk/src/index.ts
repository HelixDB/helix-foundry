import type {
  Bundle,
  Resource,
  ProviderSettings,
  OnboardingProvider,
  OnboardingState,
  OnboardingUpdate,
  OnboardingImport,
  SourceSyncState,
  HostingProvider,
  HostingSelection,
  HostingOptions,
  HostedConnection,
  PlanetScaleToken,
} from "@helix-foundry/shared";
export type {
  Bundle,
  Resource,
  ProviderSettings,
  OnboardingProvider,
  OnboardingState,
  OnboardingUpdate,
  OnboardingImport,
  SourceSyncState,
  HostingProvider,
  HostingSelection,
  HostingOptions,
  HostedConnection,
  PlanetScaleToken,
};
export class FoundryError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}
export class FoundryClient {
  hostingProviders() {
    return this.request("/hosting");
  }
  hostingAuthorize(
    provider: HostingProvider,
    returnTo: "onboarding" | "sources" = "onboarding",
  ) {
    return this.request<{ url: string }>(`/hosting/${provider}/authorize`, {
      returnTo,
    });
  }
  hostingOptions(provider: HostingProvider, selection: HostingSelection) {
    return this.request<HostingOptions>(
      `/hosting/${provider}/options`,
      selection,
    );
  }
  hostingConnect(provider: HostingProvider, selection: HostingSelection) {
    return this.request<HostedConnection>(
      `/hosting/${provider}/connect`,
      selection,
    );
  }
  hostingToken(provider: "neon" | "supabase", apiKey: string) {
    return this.request(`/hosting/${provider}/token`, { apiKey });
  }
  hostingServiceToken(credentials: PlanetScaleToken) {
    return this.request("/hosting/planetscale/token", credentials);
  }
  private base: string;
  private headers: Record<string, string>;
  constructor(
    private options: { baseUrl: string; workspaceId: string; token: string },
  ) {
    this.base =
      options.baseUrl.replace(/\/$/, "") +
      "/api/v1/workspaces/" +
      encodeURIComponent(options.workspaceId);
    this.headers = {
      authorization: "Bearer " + options.token,
      "content-type": "application/json",
    };
  }
  private async request<T = any>(
    path: string,
    body?: unknown,
    method?: string,
  ): Promise<T> {
    const r = await fetch(this.base + path, {
      method: method || (body === undefined ? "GET" : "POST"),
      headers: this.headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const data = (await r.json()) as any;
    if (!r.ok) throw new FoundryError(r.status, data.error);
    return data;
  }
  onboarding() {
    return this.request<OnboardingState>("/onboarding");
  }
  updateOnboarding(update: OnboardingUpdate) {
    return this.request<OnboardingState>("/onboarding", update, "PATCH");
  }
  prepareProvider(settings: OnboardingProvider) {
    return this.request<Resource>("/onboarding/provider", settings);
  }
  importSources(input: OnboardingImport) {
    return this.request<{ source: Resource; job: Resource }[]>(
      "/onboarding/sources",
      input,
    );
  }
  correctSource(
    id: string,
    source: {
      name: string;
      kind:
        | "mysql"
        | "postgres"
        | "rest"
        | "s3"
        | "stripe"
        | "workos"
        | "posthog";
      config: Record<string, unknown>;
      schedule?: string | null;
    },
  ) {
    return this.request<Resource>(
      "/onboarding/sources/" + encodeURIComponent(id),
      source,
      "PUT",
    );
  }
  async uploadForSetup(file: Blob, name: string, operationId: string) {
    const body = new FormData();
    body.set("file", file, name);
    const response = await fetch(this.base + "/onboarding/uploads", {
      method: "POST",
      headers: {
        authorization: this.headers.authorization,
        "idempotency-key": operationId,
      },
      body,
    });
    const result = await response.json();
    if (!response.ok) throw new FoundryError(response.status, result.error);
    return result as Resource;
  }
  recommendOutcomes() {
    return this.request<Resource>("/onboarding/recommendations", {});
  }
  prepareOutcome(goal: string, parentId?: string) {
    return this.request<Resource>("/onboarding/build", { goal, parentId });
  }
  retryImport(id: string) {
    return this.request<Resource>(
      "/onboarding/imports/" + encodeURIComponent(id) + "/retry",
      {},
    );
  }
  cancelImport(id: string) {
    return this.request<Resource>(
      "/onboarding/imports/" + encodeURIComponent(id) + "/cancel",
      {},
    );
  }
  list(
    kind: string,
    options: { offset?: number; limit?: number; search?: string } = {},
  ) {
    return this.request<{ items: Resource[]; total: number }>(
      "/resources/" +
        encodeURIComponent(kind) +
        "?" +
        new URLSearchParams(
          Object.entries(options)
            .filter(([, v]) => v !== undefined)
            .map(([k, v]) => [k, String(v)]),
        ),
    );
  }
  get(kind: string, id: string) {
    return this.request<Resource>(
      "/resources/" + encodeURIComponent(kind) + "/" + encodeURIComponent(id),
    );
  }
  ingest(name: string, rows: Record<string, unknown>[], datasetId?: string) {
    return this.request<Resource>("/ingest", { name, rows, datasetId });
  }
  createSource(source: {
    name: string;
    kind:
      | "postgres"
      | "mysql"
      | "rest"
      | "s3"
      | "stripe"
      | "workos"
      | "posthog";
    config: Record<string, unknown>;
    schedule?: string;
  }) {
    return this.request<Resource>("/sources", source);
  }
  sync(sourceId: string) {
    return this.request<Resource>("/sources/" + sourceId + "/sync", {});
  }
  build(goal: string, parentId?: string) {
    return this.request<Resource>("/assistant/runs", { goal, parentId });
  }
  cancel(runId: string) {
    return this.request("/assistant/runs/" + runId + "/cancel", {});
  }
  validate(proposalId: string) {
    return this.request<Resource>("/proposals/" + proposalId + "/validate", {});
  }
  revise(proposalId: string, bundle: Bundle) {
    return this.request<Resource>("/proposals/" + proposalId, bundle, "PUT");
  }
  publish(proposalId: string, hash: string) {
    return this.request<Resource>("/proposals/" + proposalId + "/publish", {
      hash,
    });
  }
  reject(proposalId: string) {
    return this.request("/proposals/" + proposalId + "/reject", {});
  }
  query(inputs: string[], sql: string) {
    return this.request("/query", { inputs, sql });
  }
  runPipeline(id: string) {
    return this.request<Resource>("/pipelines/" + id + "/run", {});
  }
  neighbors(objectId: string) {
    return this.request<{ ids: string[] }>(
      "/objects/" + objectId + "/neighbors",
    );
  }
  previewAction(objectId: string, values: Record<string, unknown>) {
    return this.request("/actions/preview", { objectId, values });
  }
  executeAction(action: {
    objectId: string;
    values: Record<string, unknown>;
    revision: number;
    idempotencyKey: string;
    revert?: boolean;
  }) {
    return this.request("/actions/execute", action);
  }
  exportDefinitions() {
    return this.request<{ version: 1; proposals: Bundle[] }>(
      "/definitions/export",
    );
  }
  importDefinitions(definitions: { version: 1; proposals: Bundle[] }) {
    return this.request("/definitions/import", definitions);
  }
  requestAssistance(
    goal: string,
    context?: { page?: string; resourceId?: string },
  ) {
    return this.request<Resource>("/assistant/runs", {
      goal,
      intent: "auto",
      context,
    });
  }
  record(logicalId: string) {
    return this.request<{
      record: Resource;
      related: Resource[];
      history: Resource[];
    }>("/records/" + encodeURIComponent(logicalId));
  }
  ask(question: string) {
    return this.request<Resource>("/assistant/runs", {
      goal: question,
      intent: "answer",
    });
  }
  resume(runId: string) {
    return this.request<Resource>("/assistant/runs/" + runId + "/resume", {});
  }
  saveAnalysis(runId: string) {
    return this.request<Resource>(
      "/assistant/runs/" + runId + "/save-analysis",
      {},
    );
  }
  testSource(source: {
    name: string;
    kind:
      | "postgres"
      | "mysql"
      | "rest"
      | "s3"
      | "stripe"
      | "workos"
      | "posthog";
    config: Record<string, unknown>;
  }) {
    return this.request("/sources/test", source);
  }
  provider() {
    return this.request<Omit<ProviderSettings, "apiKey"> & { hasKey: boolean }>(
      "/provider",
    );
  }
  setProvider(settings: ProviderSettings) {
    return this.request("/provider", settings, "PUT");
  }
  previewDataset(id: string, offset = 0, limit = 50) {
    return this.request("/datasets/" + id + "/preview", { offset, limit });
  }
  draftPipeline(definition: Bundle["pipelines"][number]) {
    return this.request<Resource>("/pipelines/draft", definition);
  }
  previewChange(
    action:
      | {
          operation: "create";
          objectType: string;
          values: Record<string, unknown>;
        }
      | {
          operation: "link" | "unlink";
          name: string;
          from: string;
          to: string;
        },
  ) {
    return this.request("/actions/change/preview", action);
  }
  executeChange(
    preview: { action: unknown; hash: string },
    idempotencyKey: string,
  ) {
    return this.request("/actions/change/execute", {
      ...preview,
      idempotencyKey,
    });
  }
  rollback() {
    return this.request("/rollback", {});
  }
  async *events(signal?: AbortSignal) {
    const r = await fetch(this.base + "/events", {
      headers: this.headers,
      signal,
    });
    if (!r.ok || !r.body)
      throw new FoundryError(r.status, "Event stream unavailable");
    const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
    let pending = "";
    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        pending += value;
        let idx;
        while ((idx = pending.indexOf("\n\n")) >= 0) {
          const event = pending.slice(0, idx);
          pending = pending.slice(idx + 2);
          if (event.startsWith("data: ")) yield JSON.parse(event.slice(6));
        }
      }
    } finally {
      await reader.cancel();
    }
  }
}
