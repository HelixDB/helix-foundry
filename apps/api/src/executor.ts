import { config } from "./config.js";
import type {
  Execution,
  Materialized,
} from "../../../services/executor/src/engine.js";
import { duration, timeLimit } from "../../../services/executor/src/limits.js";
import type { DatasetProfile } from "../../../packages/shared/src/index.js";
import { postJson } from "./post.js";
import { AppError } from "./security.js";
// The executor answers within the job's time limit, including time queued
// behind other jobs, which it reads from the same settings. This is the time
// it has after that to stop a job and remove its scratch before replying.
const REPLY_MARGIN = 15000;
export async function execute(
  job: Execution & { mode: "materialize" },
): Promise<Materialized>;
export async function execute(job: Execution): Promise<{
  profile: DatasetProfile;
  version: string;
  rows: Record<string, any>[];
  // Ingest only: profile.fingerprint in the untagged format older executors
  // stored.
  legacyFingerprint?: string;
}>;
export async function execute(job: Execution) {
  const limit = timeLimit(job),
    signal = AbortSignal.timeout(limit.ms + REPLY_MARGIN);
  const r = await postJson(
    config.executor + "/execute",
    JSON.stringify(job),
    signal,
    {
      "x-internal-key": config.internalKey,
    },
  ).catch((e) => {
    if (!signal.aborted) throw e;
    throw new AppError(
      504,
      `The executor did not answer within its time limit (${duration(limit.ms)}, ${limit.setting})`,
    );
  });
  const ok = r.status >= 200 && r.status < 300;
  let result: any;
  try {
    result = JSON.parse(r.text);
  } catch {
    if (ok) throw new Error("Invalid executor response");
  }
  if (!ok) throw new AppError(422, result?.error || "Dataset execution failed");
  return result;
}
