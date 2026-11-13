// A demo of Customer contracts' workflows (MOCK_WORKFLOW_DEMO, or
// POST /__mock/config {"workflowDemo": true}): three workflows with 45 days
// of runs by two people and the Support agent's key, finished and assessed
// with the engine's assess_result.json shapes, so the Workflows pages' trends
// have something to show. Deterministic: the same `now` makes the same runs.

type Gap = { kind: string; what: string };
type Plan = {
  id: string;
  name: string;
  description: string;
  // Runs a day at the start and at the end of the 45 days.
  from: number;
  to: number;
  // How often the ontology helped, at the start and at the end.
  helpedFrom: number;
  helpedTo: number;
  starters: { user?: string; key?: string }[];
  requests: string[];
  gaps: Gap[];
  suggestions: string[];
};

const PLANS: Plan[] = [
  {
    id: "wf_demo_renewals",
    name: "Renewal desk",
    description:
      "Answers questions about upcoming renewals: term, price and who signed.",
    from: 1,
    to: 4,
    helpedFrom: 0.5,
    helpedTo: 0.86,
    starters: [
      { key: "sc_acme_support" },
      { key: "sc_acme_support" },
      { user: "usr_ada" },
    ],
    requests: [
      "Which contracts renew in the next 60 days?",
      "Who signed the Globex services agreement?",
      "What did Initech pay over the last quarter?",
      "Is Umbrella Holdings up for renewal this year?",
      "Which renewals still have an open invoice?",
      "What notice does Globex have to give before its renewal?",
    ],
    gaps: [
      {
        kind: "missing_property",
        what: "No renewal notice period on Contract",
      },
      {
        kind: "missing_link",
        what: "No link from Contract to the invoices it bills",
      },
      {
        kind: "missing_data",
        what: "Umbrella Holdings isn't in the customer table",
      },
      {
        kind: "wrong_data",
        what: "Contract terms disagree between two documents",
      },
    ],
    suggestions: [
      "Add a notice_period property to Contract",
      "Link Contract to Invoice through the Stripe subscription",
      "Add a rule flagging contracts that renew within 60 days",
    ],
  },
  {
    id: "wf_demo_health",
    name: "Customer health check",
    description:
      "Answers questions about a customer account: plan, spend, usage and open support issues.",
    from: 1,
    to: 3,
    helpedFrom: 0.62,
    helpedTo: 0.8,
    starters: [{ user: "usr_ada" }, { key: "sc_acme_support" }],
    requests: [
      "Which accounts look most at risk of churning right now?",
      "How much has Globex spent with us this year?",
      "Does Initech have any overdue invoices?",
      "Summarise last week's calls with Globex",
      "Which customers pay in GBP?",
    ],
    gaps: [
      { kind: "missing_property", what: "No health score on Customer" },
      { kind: "missing_data", what: "Support tickets aren't loaded" },
      { kind: "missing_type", what: "No Ticket type for support issues" },
    ],
    suggestions: [
      "Add a health_score property to Customer, from payments and usage",
      "Load the support tickets as a Ticket type",
      "Add a rule flagging customers with overdue invoices",
    ],
  },
  {
    id: "wf_demo_disputes",
    name: "Invoice disputes",
    description:
      "Explains a customer's invoices and what their contract says they owe.",
    from: 0,
    to: 2,
    helpedFrom: 0.78,
    helpedTo: 0.82,
    starters: [{ user: "usr_alan" }],
    requests: [
      "Why was Initech charged twice in March?",
      "Does the Globex invoice match its contract price?",
      "Which invoices differ from their contract's amount?",
    ],
    gaps: [
      {
        kind: "missing_link",
        what: "No link from Invoice to the Contract that sets its price",
      },
      {
        kind: "wrong_data",
        what: "Invoice currency differs from the contract's",
      },
    ],
    suggestions: [
      "Link Invoice to Contract",
      "Add a rule comparing each invoice's currency with its contract's",
    ],
  },
];

export const DEMO_DAYS = 45;

function random(seed: number) {
  let s = seed;
  return () => (s = (s * 9301 + 49297) % 233280) / 233280;
}

/** A demo run: a stored run's fields, without the mock's bookkeeping. */
export type DemoRun = {
  id: string;
  workflowId: string;
  input: string;
  output?: string;
  startedAt: string;
  finishedAt?: string;
  startedByUserId?: string;
  startedByServiceCredentialId?: string;
  reads: number;
  questions: number;
  verdict?: string;
  assessmentJson?: string;
  usdMicros: number;
  queued?: boolean;
};

/** The demo's workflows and their runs, oldest first, as of `now`. */
export function workflowDemo(now: Date) {
  const r = random(17);
  const pick = <T>(xs: readonly T[]) => xs[Math.floor(r() * xs.length)]!;
  const runs: DemoRun[] = [];
  const today = Date.UTC(
    now.getUTCFullYear(),
    now.getUTCMonth(),
    now.getUTCDate(),
  );
  for (const p of PLANS)
    for (let d = DEMO_DAYS - 1; d >= 0; d--) {
      const progress = 1 - d / (DEMO_DAYS - 1);
      const n = Math.max(
        0,
        Math.round((p.from + (p.to - p.from) * progress) * (0.5 + r())),
      );
      for (let i = 0; i < n; i++) {
        const started =
          today - d * 86_400_000 + Math.floor((8 + r() * 10) * 3_600_000);
        if (started > now.getTime() - 60_000) continue;
        const who = pick(p.starters);
        const helped =
          r() < p.helpedFrom + (p.helpedTo - p.helpedFrom) * progress;
        const silent = !helped && r() < 0.45; // the agent never called the ontology
        const verdict = helped
          ? r() < 0.6
            ? "essential"
            : "helpful"
          : silent
            ? "unused"
            : r() < 0.85
              ? "marginal"
              : "misleading";
        const reads = silent ? 0 : Math.floor(r() * 4) + (helped ? 1 : 0);
        const questions = silent ? 0 : Math.floor(r() * 2) + (reads ? 0 : 1);
        const claims = 3 + Math.floor(r() * 9);
        const backed = helped
          ? 0.8 + r() * 0.2
          : silent
            ? r() * 0.3
            : 0.4 + r() * 0.4;
        const gaps = p.gaps.filter(() => r() < (helped ? 0.3 : 0.6));
        const suggestions = p.suggestions.filter(
          () => r() < (helped ? 0.25 : 0.5),
        );
        const id = `wfr_demo_${p.id.slice(8)}_${runs.length.toString().padStart(4, "0")}`;
        const open = d === 0 && i === n - 1 && r() < 0.6;
        const queued = d === 0 && !open && i === n - 2;
        runs.push({
          id,
          workflowId: p.id,
          input: pick(p.requests),
          ...(!open && {
            output: "An answer drawn from the ontology's reads.",
          }),
          startedAt: new Date(started).toISOString(),
          ...(!open && {
            finishedAt: new Date(
              started + 40_000 + r() * 120_000,
            ).toISOString(),
          }),
          ...(who.user
            ? { startedByUserId: who.user }
            : { startedByServiceCredentialId: who.key }),
          reads,
          questions,
          ...(!open &&
            !queued && {
              verdict,
              assessmentJson: JSON.stringify({
                verdict,
                summary: helped
                  ? "The ontology answered the request from what it holds."
                  : silent
                    ? "The agent answered without calling the ontology."
                    : "The ontology answered part of the request.",
                claims: Array.from({ length: claims }, (_, c) => ({
                  text: `Claim ${c + 1} of the answer.`,
                  support:
                    c < Math.round(claims * backed)
                      ? "supported"
                      : r() < 0.15
                        ? "contradicted"
                        : "unsupported",
                  calls: reads + questions ? [1] : [],
                })),
                gaps: gaps.map((g) => ({
                  ...g,
                  calls: reads + questions ? [1] : [],
                })),
                suggestions,
              }),
            }),
          usdMicros: open || queued ? 0 : 18_000 + Math.floor(r() * 16_000),
          ...(queued && { queued: true }),
        });
      }
    }
  return {
    workflows: PLANS.map((p) => ({
      id: p.id,
      name: p.name,
      description: p.description,
    })),
    runs,
  };
}
