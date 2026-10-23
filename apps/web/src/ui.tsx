import React, {
  createContext,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Database,
  LoaderCircle,
  X,
  MessageSquare,
  MoreHorizontal,
} from "lucide-react";
import { api } from "./api";
import { useTitle } from "./kit/util";
import { formatInterval, isInterval } from "../../../packages/shared/src";
export type AskContext = {
  resourceId?: string;
  title?: string;
  prompt?: string;
  page?: string;
};
export const WorkspaceContext = createContext({
  id: "",
  name: "",
  ask: (_context: AskContext = {}) => {},
  role: "viewer",
  user: { id: "", name: "", email: "" },
  notify: (_s: string, _error?: boolean) => {},
});
export const useWorkspace = () => useContext(WorkspaceContext);
export function useResources(
  kind: string,
  params: Record<string, string | number> = {},
) {
  const { id } = useWorkspace();
  return useQuery({
    queryKey: [id, kind, params],
    queryFn: () =>
      api(
        `/workspaces/${id}/resources/${kind}?${new URLSearchParams(Object.entries(params).map(([k, v]) => [k, String(v)]))}`,
      ),
    refetchInterval: kind === "run" ? 3000 : kind === "dataset" ? 15000 : false,
  });
}
export function useAction() {
  const qc = useQueryClient(),
    { notify } = useWorkspace(),
    [pending, setPending] = useState(false);
  return {
    pending,
    run: async (fn: () => Promise<any>, message?: string) => {
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
export function Badge({
  children,
  tone = "neutral",
}: {
  children: React.ReactNode;
  tone?: string;
}) {
  return <span className={"badge " + tone}>{children}</span>;
}
export function Empty({
  icon: Icon = Database,
  title,
  text,
  children,
}: {
  icon?: any;
  title: string;
  text: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="empty">
      <div className="empty-icon">
        <Icon size={26} />
      </div>
      <h3>{title}</h3>
      <p>{text}</p>
      {children}
    </div>
  );
}
export function Loading() {
  return (
    <div className="loading">
      <LoaderCircle className="spin" size={22} /> Loading…
    </div>
  );
}
export function PageHeader({
  eyebrow,
  title,
  description,
  children,
}: {
  eyebrow?: string;
  title: string;
  description?: string;
  children?: React.ReactNode;
}) {
  useTitle(title);
  return (
    <div className="page-heading">
      <div>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      <div className="heading-actions">{children}</div>
    </div>
  );
}
export function DataTable({ rows }: { rows: Record<string, any>[] }) {
  if (!rows.length) return <p className="muted pad">No rows to display.</p>;
  const columns = Object.keys(rows[0]);
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            {columns.map((k) => (
              <th key={k}>{k}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {columns.map((k) => (
                <td key={k}>
                  {r[k] === null ? (
                    <span className="muted">null</span>
                  ) : isInterval(r[k]) ? (
                    formatInterval(r[k])
                  ) : typeof r[k] === "object" ? (
                    JSON.stringify(r[k])
                  ) : (
                    String(r[k])
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
export function Modal({
  title,
  children,
  onClose,
  className = "",
}: {
  title: string;
  children: React.ReactNode;
  onClose: () => void;
  className?: string;
}) {
  const dialog = useRef<HTMLDialogElement>(null),
    close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const el = dialog.current!,
      previous = document.activeElement as HTMLElement | null;
    el.showModal();
    const cancel = (e: Event) => {
      e.preventDefault();
      close.current();
    };
    el.addEventListener("cancel", cancel);
    return () => {
      el.removeEventListener("cancel", cancel);
      el.close();
      previous?.focus();
    };
  }, []);
  return (
    <dialog
      ref={dialog}
      className={"modal " + className}
      aria-label={title}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="modal-head">
        <h2>{title}</h2>
        <button className="icon-btn" onClick={onClose} aria-label="Close">
          <X size={18} />
        </button>
      </div>
      {children}
    </dialog>
  );
}
export function AskButton({ resourceId, title, prompt }: AskContext) {
  const { ask } = useWorkspace();
  return (
    <button
      className="button"
      onClick={() => ask({ resourceId, title, prompt })}
    >
      <MessageSquare size={15} />
      Ask
    </button>
  );
}
export function Menu({
  label = "More",
  children,
}: {
  label?: string;
  children: React.ReactNode;
}) {
  return (
    <details className="menu">
      <summary aria-label={label}>
        <MoreHorizontal size={18} />
      </summary>
      <div className="menu-content">{children}</div>
    </details>
  );
}
export const fieldName = (s: string) =>
  s
    .replace(/_/g, " ")
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .replace(/^./, (c) => c.toUpperCase());
