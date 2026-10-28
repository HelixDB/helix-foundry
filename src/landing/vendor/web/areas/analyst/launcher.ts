import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import { analystPaths } from "./paths";

// Entry points other areas use to reach the Analyst: starting a chat (Home ask
// box, command palette) and the chat list (sidebar history). A chat is a
// question and its follow-ups; its id is its first run's id.

export type ChatSummary = {
  id: string;
  // The first question.
  title: string;
  createdAt: string;
  updatedAt: string;
  turns: number;
  // The latest run's status: queued, running, succeeded, failed, canceled or
  // awaiting_review.
  status: string;
};
export type ChatList = { items: ChatSummary[] };

// GET /workspaces/:w/threads, newest first. The key sits under the workspace
// id, so the live event stream refreshes it when a run finishes.
export const chatListKey = (workspaceId: string) => [workspaceId, "threads"];
export const chatListQuery = (workspaceId: string) => ({
  queryKey: chatListKey(workspaceId),
  queryFn: () => api<ChatList>(`/workspaces/${workspaceId}/threads`),
});
export const chatIsActive = (c: Pick<ChatSummary, "status">) =>
  c.status === "queued" || c.status === "running";

export type AnalystAsk = {
  prompt: string;
  // Unused: asking opens the chat page, titled by the question. Kept so
  // existing callers still type-check.
  title?: string;
  // A dataset, object, run or proposal the question is scoped to. A run id
  // makes the question a follow-up in that run's chat.
  resourceId?: string;
};

type StartedRun = { id: string; data?: { threadId?: string } };

// Starts a chat (or continues one, given a run as resourceId) and opens it.
// Resolves to the chat id, or undefined when the question could not be sent;
// the error has been shown by then, so callers only keep the typed text.
export function useAskAnalyst() {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    navigate = useNavigate();
  return async ({
    prompt,
    resourceId,
  }: AnalystAsk): Promise<string | undefined> => {
    const goal = prompt.trim();
    let run: StartedRun;
    try {
      run = await api<StartedRun>(`/workspaces/${id}/assistant/runs`, {
        goal,
        intent: "auto",
        context: { page: window.location.pathname, resourceId },
      });
    } catch (e) {
      notify((e as Error).message, true);
      return undefined;
    }
    const chatId = run.data?.threadId || run.id,
      now = new Date().toISOString();
    // Show the chat in the sidebar straight away; the refetch confirms it.
    qc.setQueryData<ChatList>(chatListKey(id), (list) => {
      if (!list) return list;
      const existing = list.items.find((c) => c.id === chatId);
      const chat: ChatSummary = existing
        ? {
            ...existing,
            updatedAt: now,
            turns: existing.turns + 1,
            status: "queued",
          }
        : {
            id: chatId,
            title: goal,
            createdAt: now,
            updatedAt: now,
            turns: 1,
            status: "queued",
          };
      return {
        ...list,
        items: [chat, ...list.items.filter((c) => c.id !== chatId)],
      };
    });
    void qc.invalidateQueries({ queryKey: chatListKey(id) });
    // Run lists (search) include the new question.
    void qc.invalidateQueries({ queryKey: [id, "run"] });
    navigate(analystPaths.thread(chatId));
    return chatId;
  };
}
