import "./ui.css";
import { createContext, useContext, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { CircleAlert, LoaderCircle } from "lucide-react";
import { Button } from "./kit";

// The open workspace and the signed-in user, for every page in the shell.
export type WorkspaceRole = "owner" | "admin" | "member";
export type ShellWorkspace = {
  id: string;
  slug: string;
  name: string;
  role: WorkspaceRole;
};
export type ShellUser = { id: string; name: string; email: string };

export const WorkspaceContext = createContext({
  id: "",
  slug: "",
  name: "",
  role: "member" as WorkspaceRole,
  user: { id: "", name: "", email: "" } as ShellUser,
  // The workspace has no ontology yet: the shell offers nothing but the
  // onboarding (main.tsx's OnboardingRoutes).
  onboarding: false,
  notify: (_text: string, _error?: boolean) => {},
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
