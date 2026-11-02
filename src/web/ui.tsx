import "./ui.css";
import { createContext, useContext, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { CircleAlert, LoaderCircle } from "lucide-react";
import type { ResourceScope } from "../shared/wfe";
import { Button } from "./kit";

// The open workspace and the signed-in user, for every page in the shell.
export type WorkspaceRole = "owner" | "admin" | "member";
export type ShellWorkspace = {
  id: string;
  slug: string;
  name: string;
  role: WorkspaceRole;
  // A member's query access to the workspace's projects (whoami's
  // projectAccess.queryRead; unset reads as none), undefined when it isn't
  // known: owners and admins, or a WFE that doesn't say.
  queryRead?: ResourceScope;
};
export type ShellUser = { id: string; name: string; email: string };

/** The open workspace as every page reads it (useWorkspace). */
export type WorkspaceValue = ShellWorkspace & {
  user: ShellUser;
  // The workspace has no ontology yet: the shell offers nothing but the
  // onboarding (main.tsx's OnboardingRoutes).
  onboarding: boolean;
  // The user reads only through source grants (a member an owner or admin
  // restricted, GET .../foundry/access/me): the shell offers Analyst and
  // the ontologies those grants cover, read only, and nothing else.
  grantee: boolean;
  // The ontologies whose current loads the user's source grants hold any of:
  // what a grantee, or a member who can't query an ontology's database,
  // reads (../areas/workspace/access-model.ts readAccess).
  granted: readonly string[];
  // Those of them the grants hold only some of (readsPart).
  partial: readonly string[];
  notify: (text: string, error?: boolean) => void;
};

export const WorkspaceContext = createContext<WorkspaceValue>({
  id: "",
  slug: "",
  name: "",
  role: "member",
  user: { id: "", name: "", email: "" },
  onboarding: false,
  grantee: false,
  granted: [],
  partial: [],
  notify: () => {},
});
export const useWorkspace = () => useContext(WorkspaceContext);

// Runs a change, refreshes every query and reports the outcome in a toast.
export function useAction() {
  const qc = useQueryClient(),
    { notify } = useWorkspace(),
    [pending, setPending] = useState(false);
  return {
    pending,
    run: async <T,>(fn: () => Promise<T>, message?: string) => {
      setPending(true);
      try {
        const result = await fn();
        await qc.invalidateQueries();
        if (message) notify(message);
        return result;
      } catch (e) {
        notify((e as Error).message, true);
        return undefined;
      } finally {
        setPending(false);
      }
    },
  };
}

export function Loading() {
  return (
    <div className="loading">
      <LoaderCircle className="spin" size={22} /> Loading…
    </div>
  );
}

/**
 * Under a page whose query failed to refetch: the page keeps what it last
 * loaded (TanStack keeps `data` and sets `error`) and says so, with Retry.
 * Pages show their error state only when there is no data at all.
 */
export function RefreshFailed({
  query,
}: {
  query: { error: Error | null; refetch: () => unknown };
}) {
  if (!query.error) return null;
  return (
    <p role="status" className="hf-refresh-failed">
      <CircleAlert size={14} aria-hidden /> Couldn’t refresh.{" "}
      <Button size="sm" variant="ghost" onClick={() => void query.refetch()}>
        Retry
      </Button>
    </p>
  );
}
