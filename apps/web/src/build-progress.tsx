import { api } from "./api";
import { OperationProgress, useSetupAction } from "./setup-components";
import type { Resource } from "../../../packages/shared/src";
// Plain words for the build's stages: a short label and what is happening.
const buildStage = (stage = ""): [string, string] | undefined =>
  stage.startsWith("dashboards_repair")
    ? ["Fixing analytics", "AI is fixing charts whose queries failed"]
    : (
        {
          queued: ["Waiting to start", "Your build starts in a moment"],
          ontology: [
            "Building records",
            "Turning the confirmed ontology into records and relationships",
          ],
          dashboards: [
            "Preparing analytics",
            "AI is choosing charts for your data",
          ],
          validation: [
            "Checking queries",
            "Running every query against your data",
          ],
          publishing: ["Publishing", "Saving records and relationships"],
          failed: ["Couldn’t finish", ""],
        } as Record<string, [string, string]>
      )[stage];
// The build that follows a confirmed ontology, with Retry and Stop.
export function BuildProgress({
  workspaceId: id,
  run,
  action,
}: {
  workspaceId: string;
  run: Resource;
  action: ReturnType<typeof useSetupAction>;
}) {
  const stage = buildStage(run.data.stage);
  return (
    <OperationProgress
      operation={{
        ...run,
        name: "Records",
        data: {
          ...run.data,
          stage: stage?.[0] || run.data.stage,
          // Publishing also counts the records and relationships it has read.
          progress: stage?.[1]
            ? {
                message: stage[1],
                ...(run.data.stage === "publishing" && {
                  records: run.data.progress?.records,
                  relationships: run.data.progress?.relationships,
                }),
              }
            : undefined,
        },
      }}
      onRetry={() =>
        action.run(() =>
          api(`/workspaces/${id}/assistant/runs/${run.id}/resume`, {}),
        )
      }
      // Publication finishes once it starts.
      onCancel={
        run.data.stage === "publishing"
          ? undefined
          : () =>
              action.run(() =>
                api(`/workspaces/${id}/assistant/runs/${run.id}/cancel`, {}),
              )
      }
    />
  );
}
