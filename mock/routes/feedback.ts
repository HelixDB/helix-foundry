import type {
  FeedbackCategory,
  SendFeedbackRequest,
  SendFeedbackResponse,
} from "../../src/shared/wfe";
import { fail, id, now, ok, type MockRoute } from "../http";
import { isReply, membership, onReset } from "../state";

// The WFE's SendFeedback (POST /v1/feedback), with its rules: the backend's
// RecordMcpFeedback stores it (the site's source, the page the person was
// on) and tells Helix's team in Slack with a Linear issue. A named category,
// a message of 1 to 4096 bytes of UTF-8, a page of at most 255 bytes that
// starts "/" with no control character, a workspace the caller belongs to
// (else NOT_FOUND), and at most 20 an hour a person, shared with what their
// agents send through the MCP. The mock tells no one: GET /__mock/feedback
// lists what it holds.

const PER_HOUR = 20;
const HOUR_MS = 3_600_000;
const CATEGORIES: readonly FeedbackCategory[] = [
  "FEEDBACK_CATEGORY_BUG",
  "FEEDBACK_CATEGORY_MISSING_CAPABILITY",
  "FEEDBACK_CATEGORY_CONFUSING_RESULT",
  "FEEDBACK_CATEGORY_OTHER",
  "FEEDBACK_CATEGORY_CONNECTOR_REQUEST",
];

export type MockFeedback = Required<Omit<SendFeedbackRequest, "workspaceId">> & {
  id: string;
  userId: string;
  workspaceId: string | null;
  createdAt: string;
};

const received: MockFeedback[] = [];
onReset(() => void received.splice(0));

/** What the mock received, oldest first (GET /__mock/feedback). */
export const mockFeedback = (): readonly MockFeedback[] => received;

export const feedbackRoutes: MockRoute[] = [
  {
    method: "POST",
    path: "/v1/feedback",
    handle: ({ body, principal: { user } }) => {
      const req = (body ?? {}) as Partial<SendFeedbackRequest>,
        message = typeof req.message === "string" ? req.message : "",
        page = typeof req.page === "string" ? req.page : "",
        workspaceId =
          typeof req.workspaceId === "string" ? req.workspaceId : "";
      // In the WFE's order: the category and workspace, then the backend's
      // message and page.
      if (!CATEGORIES.includes(req.category as FeedbackCategory))
        return fail(
          "INVALID_ARGUMENT",
          "category is bug, missing_capability, confusing_result, other or connector_request",
        );
      if (workspaceId && isReply(membership(workspaceId, user.id)))
        return fail("NOT_FOUND", "workspace not found");
      if (!message.trim() || Buffer.byteLength(message) > 4096)
        return fail("INVALID_ARGUMENT", "message is 1 to 4096 bytes of UTF-8");
      if (
        page &&
        (!page.startsWith("/") ||
          Buffer.byteLength(page) > 255 ||
          // Go's unicode.IsControl: C0, DEL and C1.
          // eslint-disable-next-line no-control-regex
          /[\u0000-\u001f\u007f-\u009f]/.test(page))
      )
        return fail(
          "INVALID_ARGUMENT",
          "page is a path starting with / of at most 255 bytes, without control characters",
        );
      const since = Date.now() - HOUR_MS;
      if (
        received.filter(
          (f) => f.userId === user.id && Date.parse(f.createdAt) > since,
        ).length >= PER_HOUR
      )
        return fail(
          "RESOURCE_EXHAUSTED",
          `at most ${PER_HOUR} feedback messages an hour; try again later`,
        );
      const feedback: MockFeedback = {
        id: id("fb"),
        userId: user.id,
        workspaceId: workspaceId || null,
        category: req.category as FeedbackCategory,
        message,
        page,
        createdAt: now(),
      };
      received.push(feedback);
      return ok({ id: feedback.id } satisfies SendFeedbackResponse);
    },
  },
];
