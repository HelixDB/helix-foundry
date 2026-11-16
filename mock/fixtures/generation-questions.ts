import type {
  AgentDraftQuestion,
  OntologyGenerationQuestion,
} from "../../src/shared/wfe";

// What the mock's ontology agent asks before it drafts (the mock's
// `ontologyQuestions`), in the engine's own shape (turn_result's Question,
// held to the vendored schema by tests/unit/agent-contract.test.ts): a choice
// of one with a recommended option, a choice of several, an answer in words
// it suggests, and a question it may decide itself, whose text looks like
// markup (the pages show it as text).
export const ENGINE_QUESTIONS: AgentDraftQuestion[] = [
  {
    id: "Q1",
    items: ["type.Customer"],
    header: "Customers",
    text: "Is a customer the company that pays, or each person who signs in?",
    options: [
      {
        label: "The company that pays",
        description:
          "One customer per billing account; the people who sign in are its contacts.",
      },
      {
        label: "Each person who signs in",
        description: "One customer per login, even within the same company.",
      },
    ],
    multiple: false,
    default: "The company that pays",
    blocking: true,
  },
  {
    id: "Q2",
    items: ["rule.renewal_at_risk"],
    header: "Risk signals",
    text: "Which signals should mark a renewal as at risk?",
    options: [
      {
        label: "Unpaid invoices",
        description: "An invoice past its due date.",
      },
      { label: "Falling usage", description: null },
      {
        label: "An open support ticket",
        description: "A ticket open for more than a week.",
      },
      { label: "No contact in 60 days", description: null },
    ],
    multiple: true,
    default: "Unpaid invoices",
    blocking: true,
  },
  {
    id: "Q3",
    items: ["rule.renewal_at_risk"],
    header: "At-risk window",
    text: "How many days before a contract ends does its renewal count as at risk?",
    options: [],
    multiple: false,
    default: "90 days",
    blocking: true,
  },
  {
    id: "Q4",
    items: [],
    header: "Cancelled orders",
    text: "Should orders whose status is <b>cancelled</b> stay in the ontology?",
    options: [{ label: "Keep them" }, { label: "Leave them out" }],
    multiple: false,
    default: null,
    blocking: false,
  },
];

// What it asks with `ontologyQuestions: "time_zone"`: one choice whose
// labels are IANA time zone ids, as the engine may write them; the pages
// show them in words and send the id back.
export const ZONE_QUESTIONS: AgentDraftQuestion[] = [
  {
    id: "Q1",
    items: [],
    header: "Dates",
    text: "Which time zone are the dates in your invoices written in?",
    options: [
      { label: "Europe/London", description: null },
      { label: "America/New_York", description: null },
      { label: "America/Los_Angeles", description: null },
      { label: "UTC", description: null },
    ],
    multiple: false,
    default: "America/New_York",
    blocking: true,
  },
];

/**
 * A question of the engine's as the WFE gives it
 * (model.OntologyQuestion): its `default` is the recommended answer.
 */
export const wfeQuestion = (
  q: AgentDraftQuestion,
): OntologyGenerationQuestion => ({
  id: q.id,
  header: q.header || "",
  text: q.text,
  options: (q.options || []).map((o) => ({
    label: o.label,
    description: o.description || "",
  })),
  multiple: !!q.multiple,
  recommended: q.default || "",
  blocking: q.blocking,
  items: [...q.items],
});
