import { useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { LOCAL } from "../../../shared/deployment";
import type { FoundryAccessChange } from "../../../shared/wfe";
import { ConfirmDialog } from "../../kit";
import { useWorkspace } from "../../ui";
import { ontologyQueries } from "../ontology/queries";
import { accessQuery, keysQuery, previewChange } from "./access-api";
import { lossLines } from "./access-model";
import { membersQuery } from "./members";
import { canManage } from "./plan";

// Before a change that can take away what someone reads through source
// grants (loading or making current a version, deleting a connection,
// moving a document), owners and admins are told who stops reading which
// ontology, or keeps only part of it, and choose to go on. Hosted, and only
// once the workspace grants anything: a workspace without grants loses nothing.

/**
 * `check(change)` resolves true to go on: nothing would be lost, or there
 * is nothing to ask (a local build, someone who can't make the change, a
 * workspace without grants, a preview that couldn't be read: the warning
 * is advice, never a gate); else it shows `dialog` and resolves with the
 * choice. Render `dialog` where the action is.
 */
export function useAccessCheck(): {
  check: (change: FoundryAccessChange) => Promise<boolean>;
  dialog: ReactNode;
} {
  const { id, role } = useWorkspace(),
    qc = useQueryClient();
  const [asking, setAsking] = useState<{
    lines: string[];
    answer: (go: boolean) => void;
  } | null>(null);
  const check = async (change: FoundryAccessChange) => {
    if (LOCAL || !canManage(role)) return true;
    let lines: string[];
    try {
      const access = await qc.fetchQuery({
        ...accessQuery(id),
        staleTime: 30_000,
      });
      if (!access.grants?.length) return true;
      const losses = await previewChange(id, change);
      if (!losses.length) return true;
      // Names: every member, the agents' keys, the teams and ontologies.
      const [members, keys, ontologies] = await Promise.all([
        qc.fetchInfiniteQuery({ ...membersQuery(id), pages: 20 }),
        qc.fetchQuery(keysQuery(id)),
        qc.fetchQuery(ontologyQueries.list(id)),
      ]);
      const name = new Map<string, string>([
        ...members.pages
          .flatMap((p) => p.members || [])
          .map(
            (m) =>
              [
                `KIND_USER:${m.user?.id}`,
                m.user?.displayName || m.user?.email || "A member",
              ] as const,
          ),
        ...keys.map(
          (k) =>
            [`KIND_SERVICE_CREDENTIAL:${k.id}`, k.name || "An agent"] as const,
        ),
        ...(access.teams || []).map(
          (t) => [`KIND_TEAM:${t.id}`, t.name || "A team"] as const,
        ),
      ]);
      const ontology = new Map(
        (ontologies.ontologies || []).map((o) => [o.id, o.name || o.id]),
      );
      lines = lossLines(
        losses,
        (p) => name.get(`${p.kind}:${p.id}`) || "Someone",
        (o) => ontology.get(o) || "An ontology",
      );
    } catch {
      return true;
    }
    return new Promise<boolean>((resolve) =>
      setAsking({
        lines,
        answer: (go) => {
          setAsking(null);
          resolve(go);
        },
      }),
    );
  };
  const dialog = asking && (
    <ConfirmDialog
      title="Some people will stop reading"
      description="Their sources won’t cover everything these ontologies read after this, so these agents, people and teams lose them, or keep only the part their sources hold:"
      confirmLabel="Continue"
      onConfirm={() => asking.answer(true)}
      onCancel={() => asking.answer(false)}
    >
      <ul aria-label="Who stops reading" data-replay-block>
        {asking.lines.map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ul>
    </ConfirmDialog>
  );
  return { check, dialog };
}
