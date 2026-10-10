import { config } from "./config.js";
import type { Execution } from "../../../services/executor/src/engine.js";
import type { DatasetProfile } from "../../../packages/shared/src/index.js";
import { AppError } from "./security.js";
export async function execute(job: Execution): Promise<{
  profile: DatasetProfile;
  version: string;
  rows: Record<string, any>[];
  // Ingest only: profile.fingerprint in the untagged format older executors
  // stored.
  legacyFingerprint?: string;
}> {
  const r = await fetch(config.executor + "/execute", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-internal-key": config.internalKey,
    },
    body: JSON.stringify(job),
    signal: AbortSignal.timeout(125000),
  });
  const result = (await r.json()) as any;
  if (!r.ok)
    throw new AppError(422, result.error || "Dataset execution failed");
  return result;
}
