import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { createHash } from "node:crypto";
import { createWriteStream } from "node:fs";
import { mkdir, open, rename, rm } from "node:fs/promises";
import { platform } from "node:os";
import { resolve, extname } from "node:path";
import { pipeline } from "node:stream/promises";
import { CronExpressionParser } from "cron-parser";
import { sourceSyncDefaults } from "./source-sync.js";
import { resolveHostedConfig } from "./hosting.js";
import {
  OnboardingProviderSchema,
  SourceSchema,
  OnboardingUpdateSchema,
  OnboardingImportSchema,
  OnboardingOutcomeSchema,
  OntologyConfirmSchema,
  type OnboardingState,
  type SuggestedOntology,
} from "../../../packages/shared/src/index.js";
import { transaction, resource, now, type Store, type Tx } from "./store.js";
import { assert, digest, seal, unseal, AppError } from "./security.js";
import { config } from "./config.js";
import { discover } from "./connectors.js";
import { providerSettings, retried } from "./assistant.js";
import { onboardingState, publicResource } from "./onboarding-state.js";
import { confirmOntology, ontologyGoal, suggestOntology } from "./discovery.js";

type Access = (req: any, role?: "owner" | "editor" | "viewer") => Promise<any>;
export function registerOnboarding(
  app: FastifyInstance,
  store: Store,
  access: Access,
) {
  const base = "/api/v1/workspaces/:w/onboarding";
  app.get(base, async (req) =>
    onboardingState(store, (await access(req)).scope),
  );
  app.patch(base, async (req) => {
    const a = await access(req, "editor"),
      body = OnboardingUpdateSchema.parse(req.body);
    for (const id of body.excludedIds || [])
      assert(
        await store.get(a.scope, id),
        "Selection not found in this workspace",
        404,
      );
    await transaction(store, a.scope, async (tx) => {
      const w = (await tx.get("workspace"))!;
      const old = w.data.onboarding || {};
      // A selection is a set: the same ids in another order change nothing.
      const set = (ids: string[] = []) =>
        JSON.stringify([...new Set(ids)].sort());
      const excludedChanged =
        body.excludedIds !== undefined &&
        set(old.excludedIds) !== set(body.excludedIds);
      const changed =
        (body.goal !== undefined && old.goal !== body.goal) || excludedChanged;
      tx.update(w, {
        ...w.data,
        onboarding: {
          ...old,
          ...body,
          ...(changed ? { buildRunId: null, recommendationRunId: null } : {}),
          ...(excludedChanged
            ? { ontologySuggestion: null, ontologyRunId: null, ontology: null }
            : {}),
        },
      });
    });
    return onboardingState(store, a.scope);
  });
  app.post(base + "/provider", async (req) => {
    const a = await access(req, "owner"),
      { advanceToData, ...settings } = OnboardingProviderSchema.parse(req.body);
    const old = await providerSettings(store, a.scope);
    const job = await transaction(store, a.scope, async (tx) => {
      const w = (await tx.get("workspace"))!,
        m = w.data.onboarding || {};
      const existing = m.providerJobId && (await tx.get(m.providerJobId));
      const selected = { ...settings };
      if (!selected.apiKey) {
        if (existing?.data.settings?.provider === selected.provider)
          selected.apiKey = unseal(existing.data.secret).apiKey;
        else if (selected.provider === old.provider)
          selected.apiKey = old.apiKey;
      }
      assert(
        selected.provider === "local" || !!selected.apiKey?.trim(),
        "Enter an API key for the selected provider.",
      );
      const fingerprint = digest(JSON.stringify(selected));
      if (
        existing?.data.fingerprint === fingerprint &&
        !["failed", "canceled"].includes(existing.data.status)
      ) {
        if (advanceToData)
          tx.update(w, { ...w.data, onboarding: { ...m, step: "data" } });
        return existing;
      }
      if (existing && ["running", "queued"].includes(existing.data.status))
        tx.update(existing, { ...existing.data, status: "canceled" });
      const job = tx.put(
        resource(a.scope, "job", "Prepare " + settings.provider, {
          type: "provider_prepare",
          status: "queued",
          secret: seal(selected),
          settings: { ...selected, apiKey: undefined },
          hasKey: !!selected.apiKey,
          fingerprint,
          actor: a.user.id,
          progress: { message: "Waiting to prepare AI" },
        }),
      );
      tx.update(w, {
        ...w.data,
        onboarding: {
          ...m,
          providerJobId: job.id,
          ...(advanceToData ? { step: "data" } : {}),
        },
      });
      return job;
    });
    return publicResource(job);
  });
  // Batch setup retains the existing one-source-per-table/file storage model.
  app.post(base + "/sources", async (req) => {
    const a = await access(req, "owner"),
      body = OnboardingImportSchema.parse(req.body);
    assert(body.kind !== "upload", "Use the upload endpoint");
    if (body.schedule) CronExpressionParser.parse(body.schedule, { tz: "UTC" });
    const opId = "connection_" + digest(body.operationId),
      fingerprint = digest(JSON.stringify(body));
    const prior = await store.get(a.scope, opId);
    if (prior) {
      assert(
        prior.data.fingerprint === fingerprint,
        "Operation was already used for different inputs",
        409,
      );
      return prior.data.result;
    }
    const resolved = await resolveHostedConfig(
      store,
      a.scope,
      body.kind,
      body.config,
    );
    const discovery: any = await testedConnection(body.kind, resolved.config);
    const selections = [
      ...new Map(body.selections.map((s) => [JSON.stringify(s), s])).values(),
    ];
    for (const s of selections) {
      if (body.kind === "mysql" || body.kind === "postgres")
        assert(
          discovery.tables?.some(
            (t: any) =>
              t.name === s.table &&
              (body.kind === "mysql" ||
                t.schema === (s.schema || body.config.schema || "public")),
          ),
          "Choose tables from the tested connection",
        );
      if (body.kind === "neo4j")
        assert(
          discovery.tables?.some(
            (t: any) => t.name === s.table && t.schema === s.schema,
          ),
          "Choose labels and relationship types from the tested connection",
        );
      if (["stripe", "workos", "posthog"].includes(body.kind))
        assert(
          discovery.tables?.some((t: any) => t.name === s.table),
          "Choose objects the API key can read",
        );
      if (body.kind === "s3")
        assert(
          discovery.tables?.some((t: any) => t.name === s.key),
          "Choose files from the tested bucket",
        );
    }
    return transaction(store, a.scope, async (tx) => {
      const existing = await tx.get(opId);
      if (existing) {
        assert(
          existing.data.fingerprint === fingerprint,
          "Operation was already used for different inputs",
          409,
        );
        return existing.data.result;
      }
      const result = [];
      for (const s of selections) {
        const source = tx.put(
          resource(
            a.scope,
            "source",
            // Each table, file, or object names its dataset; REST has neither.
            s.table || s.key || body.name,
            {
              kind: body.kind,
              connectionId: opId,
              connectionName: body.name,
              secret: seal({ ...resolved.config, ...s }),
              hostedProvider: resolved.provider,
              discovery,
              ...sourceSyncDefaults(body.kind, body.schedule),
            },
          ),
        );
        const job = tx.put(
          resource(a.scope, "job", source.name, {
            type: "sync",
            sourceId: source.id,
            status: "queued",
            actor: a.user.id,
            progress: { message: "Waiting to import" },
          }),
        );
        result.push({ source: publicResource(source), job });
      }
      tx.put(
        resource(
          a.scope,
          "operation",
          "Connection import",
          { fingerprint, result },
          opId,
        ),
      );
      return result;
    });
  });
  app.put(base + "/sources/:id", async (req: any) => {
    const a = await access(req, "owner"),
      body = SourceSchema.parse(req.body);
    const old = await store.get(a.scope, req.params.id);
    assert(old?.kind === "source", "Source not found", 404);
    assert(old.data.kind === body.kind, "Keep the existing connector type");
    if (body.schedule) CronExpressionParser.parse(body.schedule, { tz: "UTC" });
    const resolved = await resolveHostedConfig(
      store,
      a.scope,
      body.kind,
      body.config,
    );
    const discovery = await testedConnection(body.kind, resolved.config);
    return transaction(store, a.scope, async (tx) => {
      const source = (await tx.get(old.id))!;
      for (const job of await tx.list("job"))
        if (
          job.data.sourceId === source.id &&
          ["queued", "running"].includes(job.data.status)
        )
          tx.update(job, { ...job.data, status: "canceled" });
      tx.update(source, {
        ...source.data,
        secret: seal(resolved.config),
        hostedProvider: resolved.provider,
        discovery,
        ...sourceSyncDefaults(body.kind, body.schedule),
      });
      return tx.put(
        resource(a.scope, "job", source.name, {
          type: "sync",
          status: "queued",
          sourceId: source.id,
          actor: a.user.id,
        }),
      );
    });
  });
  app.post(base + "/uploads", async (req: any) => {
    const a = await access(req, "editor");
    const operationId = z
      .string()
      .min(8)
      .max(100)
      .parse(req.headers["idempotency-key"]);
    const jobId = "upload_" + digest(operationId);
    const part = await req.file();
    assert(part, "Choose a file");
    const ext = extname(part.filename).toLowerCase();
    assert(
      [".csv", ".json", ".jsonl", ".parquet"].includes(ext),
      "Choose CSV, JSON, JSONL or Parquet",
    );
    const dir = resolve(config.dataDir, "incoming", a.scope);
    await mkdir(dir, { recursive: true });
    const temporary = `${jobId}_${crypto.randomUUID()}.staging`,
      file = jobId + ext;
    const hash = createHash("sha256");
    part.file.on("data", (chunk: Buffer) => hash.update(chunk));
    try {
      await pipeline(
        part.file,
        createWriteStream(resolve(dir, temporary), { flags: "wx" }),
      );
      assert(!part.file.truncated, "File exceeds the 256 MB upload limit");
      const fingerprint = hash.digest("hex");
      const handle = await open(resolve(dir, temporary), "r+");
      try {
        await handle.sync();
      } finally {
        await handle.close();
      }
      // Content-addressed staging makes concurrent retries safe; metadata commits after the file is durable.
      const staged = fingerprint + ext;
      await rename(resolve(dir, temporary), resolve(dir, staged));
      // Windows cannot flush a read-only directory handle. File contents are synced above.
      if (platform() !== "win32") {
        const directory = await open(dir, "r");
        try {
          await directory.sync();
        } finally {
          await directory.close();
        }
      }
      return publicResource(
        await transaction(store, a.scope, async (tx) => {
          const old = await tx.get(jobId);
          if (old) {
            assert(
              old.data.fingerprint === fingerprint,
              "Upload operation was already used for a different file",
              409,
            );
            return old;
          }
          return tx.put(
            resource(
              a.scope,
              "job",
              part.filename.replace(/\.[^.]+$/, ""),
              {
                type: "upload",
                status: "queued",
                file: staged,
                fingerprint,
                datasetId: "dataset_" + jobId,
                actor: a.user.id,
                progress: { message: "File received. Waiting to import" },
              },
              jobId,
            ),
          );
        }),
      );
    } finally {
      await rm(resolve(dir, temporary), { force: true });
    }
  });
  app.post(base + "/imports/:id/:action", async (req: any) => {
    const a = await access(req, "editor"),
      action = z.enum(["retry", "cancel"]).parse(req.params.action);
    return transaction(store, a.scope, async (tx) => {
      const job = await tx.get(req.params.id);
      assert(
        job?.kind === "job" &&
          ["sync", "upload", "provider_prepare"].includes(job.data.type),
        "Setup operation not found",
        404,
      );
      if (job.data.type === "provider_prepare") {
        assert(a.role === "owner", "Only owners configure AI", 403);
        assert(
          (await tx.get("workspace"))?.data.onboarding?.providerJobId ===
            job.id,
          "A different provider was selected. Prepare the selected provider instead.",
          409,
        );
      }
      if (action === "cancel") {
        assert(
          job.data.status !== "succeeded",
          "This operation has completed",
          409,
        );
        return publicResource(
          tx.update(job, { ...job.data, status: "canceled" }),
        );
      }
      assert(
        ["failed", "canceled"].includes(job.data.status),
        "Only stopped operations can be retried",
        409,
      );
      return publicResource(
        tx.update(job, {
          ...job.data,
          status: "queued",
          attempts: 0,
          retryAt: 0,
          error: null,
        }),
      );
    });
  });
  for (const kind of ["recommendations", "build"] as const)
    app.post(base + "/" + kind, async (req) => {
      const a = await access(req, "editor");
      const body =
        kind === "build"
          ? OnboardingOutcomeSchema.parse(req.body)
          : {
              goal: "Suggest useful business outcomes for the connected data",
              parentId: undefined,
            };
      const state = await onboardingState(store, a.scope);
      assert(state.inputsReady, state.blockers.join(" "), 409);
      assert(
        state.providerReady,
        "Finish preparing AI before creating a view",
        409,
      );
      if (body.parentId) {
        const p = await store.get(a.scope, body.parentId);
        assert(p?.kind === "proposal", "Proposal not found", 404);
      }
      return transaction(store, a.scope, async (tx) => {
        const w = (await tx.get("workspace"))!,
          m = w.data.onboarding || {};
        // Revisions keep building from the confirmed ontology.
        const confirmed =
          kind === "build" && m.ontology?.signature === state.signature
            ? m.ontology
            : undefined;
        const run = await setupRun(tx, a, state, {
          kind,
          goal: body.goal,
          parentId: body.parentId,
          ontology: confirmed,
        });
        tx.update(w, {
          ...w.data,
          onboarding: {
            ...m,
            ...(kind === "build"
              ? { goal: body.goal, buildRunId: run.id, step: "review" }
              : { recommendationRunId: run.id }),
          },
        });
        return run;
      });
    });
  // The suggestion is computed here, deterministically; the AI only rewrites its
  // summary, descriptions and link names in a background run.
  app.post(base + "/ontology", async (req) => {
    const a = await access(req, "editor");
    const state = await onboardingState(store, a.scope);
    assert(!state.sampleProgress, "Sample data is still loading.", 409);
    assert(state.inputsReady, state.blockers.join(" "), 409);
    const stored = (await store.get(a.scope, "workspace"))?.data.onboarding
      ?.ontologySuggestion;
    const suggestion: SuggestedOntology =
      stored?.signature === state.signature
        ? stored
        : await suggestOntology(store, a.scope, {
            datasets: state.datasets,
            sources: state.sources,
            signature: state.signature,
          });
    await transaction(store, a.scope, async (tx) => {
      const w = (await tx.get("workspace"))!,
        m = w.data.onboarding || {};
      const current =
        m.ontologySuggestion?.signature === state.signature
          ? m.ontologySuggestion
          : suggestion;
      const linked = m.ontologyRunId && (await tx.get(m.ontologyRunId));
      const describe =
        state.providerReady &&
        !current.described &&
        linked?.data.onboardingSignature !== state.signature;
      // A finished run described an earlier copy of this suggestion (the data was
      // deselected and restored since), so the next attempt gets its own run.
      let runId =
          "setup_" + digest(JSON.stringify(["ontology", state.signature])),
        prior = await tx.get(runId);
      for (let n = 2; describe && prior && !working(prior); n++) {
        runId =
          "setup_" + digest(JSON.stringify(["ontology", state.signature, n]));
        prior = await tx.get(runId);
      }
      if (describe && !prior)
        tx.put(
          resource(
            a.scope,
            "run",
            "Describe the suggested ontology",
            {
              goal: "Describe the suggested ontology",
              intent: "build",
              authority: { userId: a.user.id, readOnly: false },
              status: "queued",
              stage: "queued",
              events: [],
              calls: 0,
              tokens: 0,
              task: "describe_ontology",
              datasetIds: state.datasets.map((d) => d.id),
              onboardingSignature: state.signature,
              ontologyHash: current.hash,
            },
            runId,
          ),
        );
      if (current !== m.ontologySuggestion || describe)
        tx.update(w, {
          ...w.data,
          onboarding: {
            ...m,
            ontologySuggestion: current,
            ...(describe ? { ontologyRunId: runId } : {}),
          },
        });
    });
    return onboardingState(store, a.scope);
  });
  app.post(base + "/ontology/confirm", async (req) => {
    const a = await access(req, "editor"),
      body = OntologyConfirmSchema.parse(req.body);
    const state = await onboardingState(store, a.scope);
    assert(state.inputsReady, state.blockers.join(" "), 409);
    // Building from a confirmed ontology is code; AI only described it.
    await transaction(store, a.scope, async (tx) => {
      const w = (await tx.get("workspace"))!,
        m = w.data.onboarding || {},
        suggestion = m.ontologySuggestion;
      assert(
        suggestion?.signature === state.signature &&
          suggestion.hash === body.hash,
        "The suggested ontology changed. Review it again.",
        409,
      );
      const value = confirmOntology(suggestion, body);
      const ontology = {
        signature: state.signature,
        hash: digest(JSON.stringify(value)),
        value,
        confirmedAt: now(),
        actor: a.user.id,
      };
      const goal = ontologyGoal(value);
      const run = await setupRun(tx, a, state, {
        kind: "build",
        goal,
        ontology,
      });
      tx.update(w, {
        ...w.data,
        onboarding: {
          ...m,
          ontology: {
            ...ontology,
            edits: { excluded: body.excluded, names: body.names },
          },
          goal,
          buildRunId: run.id,
          step: "review",
        },
      });
    });
    return onboardingState(store, a.scope);
  });
}

const working = (r: { data: Record<string, any> }) =>
  ["queued", "running"].includes(r.data.status);

// Setup runs are keyed by their inputs, so repeating a request reuses its run.
async function setupRun(
  tx: Tx,
  a: { scope: string; user: { id: string } },
  state: OnboardingState,
  opts: {
    kind: "recommendations" | "build";
    goal: string;
    parentId?: string;
    ontology?: { hash: string; value: SuggestedOntology };
  },
) {
  const runId =
    "setup_" +
    digest(
      JSON.stringify([
        opts.kind,
        state.signature,
        opts.goal,
        opts.parentId || "",
        ...(opts.ontology ? [opts.ontology.hash] : []),
      ]),
    );
  const existing = await tx.get(runId);
  // Confirming again restarts a stopped or failed build, or one left waiting for
  // the former review page, from its checkpoints and as the confirming editor.
  if (
    existing?.data.ontology &&
    ["awaiting_review", "failed", "canceled"].includes(existing.data.status)
  )
    return tx.update(existing, {
      ...retried(existing.data, "Building your workspace again"),
      authority: { userId: a.user.id, readOnly: false },
    });
  return (
    existing ||
    tx.put(
      resource(
        a.scope,
        "run",
        opts.goal.slice(0, 100),
        {
          goal: opts.goal,
          parentId: opts.parentId,
          intent: "build",
          authority: { userId: a.user.id, readOnly: false },
          status: "queued",
          stage: "queued",
          events: [],
          calls: 0,
          tokens: 0,
          task:
            opts.kind === "recommendations"
              ? "recommend_outcomes"
              : "onboarding_build",
          datasetIds: state.datasets.map((d) => d.id),
          onboardingSignature: state.signature,
          ...(opts.ontology
            ? {
                ontology: opts.ontology.value,
                ontologyHash: opts.ontology.hash,
              }
            : {}),
        },
        runId,
      ),
    )
  );
}

export async function testedConnection(
  kind: string,
  config: Record<string, any>,
) {
  try {
    return await discover(kind, config);
  } catch (error) {
    if (error instanceof AppError) throw error;
    const message = error instanceof Error ? error.message : "";
    throw new AppError(
      400,
      /certificate|TLS|SSL/i.test(message)
        ? "TLS verification failed. Check the CA certificate and server name, then test again."
        : /password|denied|auth/i.test(message)
          ? "Authentication failed. Check the username, password and read permissions, then test again."
          : "Could not connect. Check the host, port and connection settings, then test again.",
    );
  }
}
