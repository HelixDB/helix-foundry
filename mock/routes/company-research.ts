import type {
  DraftGoalRequest,
  DraftGoalResponse,
  FoundryCompanyResearch,
  FoundryResearchQuestion,
  ResearchCompanyResponse,
} from "../../src/shared/wfe";
import { isRecord } from "../../src/shared/json";
import {
  companyDomain,
  registrableDomain,
} from "../../src/web/areas/ontology/model";
import {
  fail,
  id,
  now,
  ok,
  type GrpcCode,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { config, isReply, membership, onReset } from "../state";
import { aiRefusal, foundryPrecondition } from "./ai";
import { manages } from "./workspaces";

// The new-ontology wizard's company research, as the WFE's
// ResearchFoundryCompany and DraftFoundryGoal answer it, without reading any
// site or calling a model:
// - Both need an owner or admin, then the workspace's AI (aiRefusal:
//   FOUNDRY_AI_NOT_CONFIGURED, FOUNDRY_PRO_REQUIRED).
// - research: the domain normalised as the backend does (scheme, path and
//   port dropped, then reduced to its registrable domain as the client's
//   registrableDomain reads it: eu.globex.com is globex.com); one that isn't
//   a domain, an IP address or a free-mail provider's (gmail.com,
//   outlook.com, …) is INVALID_ARGUMENT. A domain containing "unreadable"
//   answers fetched=false with no sources; containing "fail" UNAVAILABLE
//   with reason FOUNDRY_RESEARCH_FAILED; else the company is its first label
//   title-cased ("globex-industries.com" is Globex Industries), with a
//   2-sentence summary, suggestedName "<Company> operations", its homepage
//   and /about as sources, and 4 fixed questions with best guesses and
//   options.
// - draft-goal: NOT_FOUND for research unknown or another workspace's;
//   INVALID_ARGUMENT for an answer that isn't a question id and a string,
//   one naming no question of it, an answer over 500 characters or data
//   over 12,000; else a goal (at most 2,000 characters) from the summary,
//   the answers in the questions' order and the first lines of data, and
//   the research's suggestedName.
// - `POST /__mock/config {"companyResearchMs": n}`: each call takes n ms
//   (600). `{"companyResearchOutcome": "fail" | "budget" | "rate_limit"}`:
//   every call fails as the model failing (UNAVAILABLE,
//   FOUNDRY_RESEARCH_FAILED), the monthly spend being out
//   (FAILED_PRECONDITION, FOUNDRY_RESEARCH_BUDGET) or the hourly cap
//   (RESOURCE_EXHAUSTED).

type Knobs = {
  companyResearchMs?: number;
  companyResearchOutcome?: "fail" | "budget" | "rate_limit";
};
const knobs = config as typeof config & Knobs;

const researched = new Map<
  string,
  { workspaceId: string; research: FoundryCompanyResearch }
>();

/** An error with the ErrorInfo reason the WFE gives it. */
function reasoned(code: GrpcCode, ws: string, reason: string, message: string) {
  const reply = fail(code, message);
  return {
    ...reply,
    body: {
      ...(reply.body as object),
      details: (
        foundryPrecondition(ws, reason, message).body as { details: unknown[] }
      ).details,
    },
  };
}

/**
 * A domain as the backend normalises it, or why it isn't one.
 *
 * @example
 * normalizeDomain(" https://www.Globex.com:443/about?x=1 "); // "globex.com"
 * normalizeDomain("shop.acme.co.uk"); // "acme.co.uk"
 * normalizeDomain("gmail.com"); // { refused: "…" }
 */
export function normalizeDomain(raw: string): string | { refused: string } {
  const domain = registrableDomain(
    raw
      .trim()
      .replace(/^[a-z][a-z0-9+.-]*:\/\//i, "")
      .replace(/[/?#].*$/, "")
      .replace(/:\d*$/, ""),
  );
  if (!domain)
    return { refused: "enter your company’s domain, such as acme.com" };
  if (!companyDomain("x@" + domain))
    return {
      refused: `${domain} is a personal mail provider: enter your company’s domain`,
    };
  return domain;
}

const QUESTIONS = (company: string): FoundryResearchQuestion[] => [
  {
    id: "q1",
    question: "Who will ask this ontology questions, and what for?",
    bestGuess: `${company}’s operations and finance teams, to see how customers, orders and revenue connect.`,
    options: ["Leadership", "Sales and account managers", "Support", "Finance"],
  },
  {
    id: "q2",
    question: "Which questions matter most?",
    bestGuess: "Which customers are growing or at risk, and why.",
    options: ["Revenue by customer", "Churn risk", "Pipeline health"],
  },
  {
    id: "q3",
    question: "When two sources describe a customer, which one is right?",
    bestGuess: "The billing system for accounts; the CRM for contacts.",
    options: ["Billing", "CRM", "The product database"],
  },
  {
    id: "q4",
    question: "What does “active” mean for a customer here?",
    bestGuess: "Paid an invoice in the last 90 days.",
    options: [
      "Paid in the last 90 days",
      "Logged in this month",
      "Has an open contract",
    ],
  },
];

function caller(req: MockRequest): MockReply | undefined {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
  return (
    aiRefusal(m.workspaceId) ||
    (knobs.companyResearchOutcome === "fail"
      ? reasoned(
          "UNAVAILABLE",
          m.workspaceId,
          "FOUNDRY_RESEARCH_FAILED",
          "the model couldn’t finish the research; try again",
        )
      : knobs.companyResearchOutcome === "budget"
        ? reasoned(
            "FAILED_PRECONDITION",
            m.workspaceId,
            "FOUNDRY_RESEARCH_BUDGET",
            "this workspace has spent this month’s allowance for company research",
          )
        : knobs.companyResearchOutcome === "rate_limit"
          ? fail(
              "RESOURCE_EXHAUSTED",
              "too many company researches this hour; try again later",
            )
          : undefined)
  );
}

const paced = async (reply: () => MockReply) => {
  const ms = knobs.companyResearchMs ?? 600;
  if (ms > 0) await new Promise((r) => setTimeout(r, ms));
  return reply();
};

const PATH = "/v1/workspaces/{workspaceId}/foundry/company-research" as const;

export const companyResearchRoutes: MockRoute[] = [
  {
    method: "POST",
    path: PATH,
    handle: (req) =>
      paced(() => {
        const refused = caller(req);
        if (refused) return refused;
        const ws = req.params.workspaceId!,
          body = (req.body || {}) as { domain?: unknown };
        const domain = normalizeDomain(
          typeof body.domain === "string" ? body.domain : "",
        );
        if (typeof domain !== "string")
          return fail("INVALID_ARGUMENT", domain.refused);
        if (domain.includes("fail"))
          return reasoned(
            "UNAVAILABLE",
            ws,
            "FOUNDRY_RESEARCH_FAILED",
            "the model couldn’t finish the research; try again",
          );
        const unreadable = domain.includes("unreadable"),
          company = domain
            .split(".")[0]!
            .split("-")
            .filter(Boolean)
            .map((w) => w[0]!.toUpperCase() + w.slice(1))
            .join(" ");
        const research: FoundryCompanyResearch = {
          id: id("res"),
          domain,
          companyName: company,
          summary: unreadable
            ? `${company} is a business at ${domain}; its site couldn’t be read, so this is a guess from its name. It likely sells to other businesses.`
            : `${company} sells software that helps teams run their operations. Its customers are mid-sized companies that pay a monthly subscription.`,
          suggestedName: `${company} operations`,
          questions: QUESTIONS(company),
          sources: unreadable
            ? []
            : [`https://${domain}/`, `https://${domain}/about`],
          fetched: !unreadable,
          createdAt: now(),
        };
        researched.set(research.id, { workspaceId: ws, research });
        return ok({ research } satisfies ResearchCompanyResponse);
      }),
  },
  {
    method: "POST",
    path: `${PATH}/{researchId}:draft-goal`,
    handle: (req) =>
      paced(() => {
        const refused = caller(req);
        if (refused) return refused;
        const found = researched.get(req.params.researchId!);
        if (!found || found.workspaceId !== req.params.workspaceId)
          return fail("NOT_FOUND", "company research not found");
        const { research } = found,
          body = (req.body || {}) as Partial<DraftGoalRequest>,
          answers = Array.isArray(body.answers) ? body.answers : [],
          data = typeof body.data === "string" ? body.data : "";
        const questions = research.questions || [];
        if (
          !answers.every(
            (a: unknown) =>
              isRecord(a) &&
              typeof a.questionId === "string" &&
              typeof a.answer === "string",
          )
        )
          return fail(
            "INVALID_ARGUMENT",
            "answers: each is a questionId and an answer, both strings",
          );
        const unknown = answers.find(
          (a) => !questions.some((q) => q.id === a.questionId),
        );
        if (unknown)
          return fail(
            "INVALID_ARGUMENT",
            `answers: no question ${JSON.stringify(unknown.questionId)} in this research`,
          );
        if (answers.some((a) => [...a.answer!].length > 500))
          return fail(
            "INVALID_ARGUMENT",
            "an answer is at most 500 characters",
          );
        if ([...data].length > 12_000)
          return fail("INVALID_ARGUMENT", "data is at most 12,000 characters");
        const said = questions.flatMap((q) => {
          const a = answers.find((x) => x.questionId === q.id)?.answer?.trim();
          return a ? [`${q.question} ${a}`] : [];
        });
        const reads = data
          .split("\n")
          .map((l) => l.trim())
          .filter((l) => l.startsWith("**") || l.startsWith("- "))
          .slice(0, 3);
        const goal = [
          `${research.companyName} uses this ontology to answer its team’s questions about its business. ${research.summary || ""}`.trim(),
          "",
          ...said,
          ...(reads.length ? ["", "It reads:", ...reads] : []),
        ].join("\n");
        return ok({
          goal: [...goal].slice(0, 2000).join(""),
          name: research.suggestedName,
        } satisfies DraftGoalResponse);
      }),
  },
];

onReset(() => {
  researched.clear();
  delete knobs.companyResearchMs;
  delete knobs.companyResearchOutcome;
});
