import type {
  FoundryHealFinding,
  HealFindingResponse,
  ListHealFindingsResponse,
} from "../../src/shared/wfe";
import { fail, now, ok, type MockRoute } from "../http";
import { config, onReset } from "../state";
import { ACME_GENERAL } from "./documents";
import { currentLoadId, currentLoadVersionId } from "./loads";
import { isScope, ontologyOf } from "./ontologies";

// Self-healing suggestions, as the backend serves them
// (api/heal_findings.go): an ontology with a current load has had it
// audited, and lists its open findings by support (backed by two documents,
// or by rows), with its latest run; a manager dismisses an open one, which
// then leaves the list. Each finding's JSON is the engine's Finding
// (audit_result.json `$defs/Finding`), which the unit tests hold to the
// vendored schema.
//
// Knobs, through POST /__mock/config: `healEnabled: false` (a Foundry
// whose foundry.heal.enabled is off: no audit runs), `healRun: "running" |
// "failed" | "incomplete"` (the current load's audit, from then on, hasn't
// completed: the list keeps an earlier load's findings, if one was
// audited), `healDismiss: "superseded"` (an audit supersedes the finding
// as it is dismissed, which then fails) or `"fails"` (the backend fails
// it: the finding stays open), and `healRowsMeasured: false` (the load's
// report kept no rows, so the complete audit didn't check them).
type Knobs = {
  healEnabled?: boolean;
  // null clears it: the current load's audit completes when next asked.
  healRun?: "running" | "failed" | "incomplete" | null;
  healDismiss?: "superseded" | "fails";
  healRowsMeasured?: boolean;
};
const knobs = config as typeof config & Knobs;
onReset(() => {
  delete knobs.healEnabled;
  delete knobs.healRun;
  delete knobs.healDismiss;
  delete knobs.healRowsMeasured;
});

const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}";
const SHOWN = 20;

/** A fingerprint as the engine writes one: 64 lowercase hex digits. */
const fingerprint = (digit: string) => digit.repeat(64);

/** The engine's findings an audit of a seeded load gives, by support. */
export const SUGGESTION_FINDINGS: {
  finding: object;
  support: number;
  sources: number;
}[] = [
  {
    support: 12,
    sources: 5,
    finding: {
      fingerprint: fingerprint("a"),
      signal: {
        kind: "unmatched",
        canonical: "supplier_of",
        set: ACME_GENERAL,
        backing: {
          count: 12,
          sources: 5,
          examples: [
            {
              doc_id: "doc_acme_msa",
              quote: "Globex supplies Acme with steel under this agreement.",
            },
            {
              doc_id: "doc_acme_sow",
              quote: "Initech is Acme's supplier of packaging.",
            },
            {
              doc_id: "doc_acme_renewal",
              quote: "Globex will continue to supply Acme.",
            },
            { doc_id: "doc_acme_order", quote: "Parts supplied by Initech." },
          ],
        },
      },
      proposal: {
        proposal: "route",
        set: ACME_GENERAL,
        links: ["supplies"],
        target: { count: "unmatched", canonicals: ["supplier_of"] },
      },
    },
  },
  {
    support: 4,
    sources: 3,
    finding: {
      fingerprint: fingerprint("b"),
      signal: {
        kind: "unresolved",
        type: "Customer",
        norm_name: "globex corp",
        now: { resolves: "none" },
        backing: {
          count: 4,
          sources: 3,
          examples: [
            {
              doc_id: "doc_acme_msa",
              quote: "Globex Corp signed on behalf of its affiliates.",
            },
          ],
        },
      },
      proposal: null,
    },
  },
  {
    support: 3,
    sources: 0,
    finding: {
      fingerprint: fingerprint("c"),
      signal: {
        kind: "row_errors",
        mapping: "customers",
        errors: { count: 3, examples: ["row 14: customer_id is empty"] },
      },
      proposal: {
        proposal: "draft",
        brief: { brief: "mapping", mapping: "customers" },
        target: { count: "row_errors", mapping: "customers" },
      },
    },
  },
];

type Stored = { findings: FoundryHealFinding[]; runAt: string; loadId: string };
// By ontology: its findings once its current load was audited.
const audited = new Map<string, Stored>();
onReset(() => audited.clear());

/**
 * The ontology's findings, its current load audited when first asked,
 * unless `healRun` holds the audit off: then those of the load audited
 * before, if any.
 */
function auditOf(ontologyId: string): Stored | undefined {
  const loadId = currentLoadId(ontologyId);
  if (!loadId) return undefined;
  const have = audited.get(ontologyId);
  if (have?.loadId === loadId || knobs.healRun) return have;
  const fresh: Stored = {
    loadId,
    runAt: now(),
    findings: SUGGESTION_FINDINGS.map(({ finding, support, sources }) => ({
      fingerprint: (finding as { fingerprint: string }).fingerprint,
      kind: (finding as { signal: { kind: string } }).signal.kind,
      status: "open",
      support: String(support),
      sources: String(sources),
      loadId,
      baseVersionId: currentLoadVersionId(ontologyId) || "",
      findingJson: JSON.stringify(finding),
    })),
  };
  // A dismissal holds across loads (below twice its support).
  for (const f of fresh.findings) {
    const was = have?.findings.find((x) => x.fingerprint === f.fingerprint);
    if (was?.status === "dismissed")
      Object.assign(f, {
        status: "dismissed",
        dismissedByUserId: was.dismissedByUserId,
        supportAtDismissal: was.supportAtDismissal,
        findingJson: was.findingJson,
      });
  }
  audited.set(ontologyId, fresh);
  return fresh;
}

export const suggestionRoutes: MockRoute[] = [
  {
    method: "GET",
    path: `${BASE}/heal-findings`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const enabled = knobs.healEnabled !== false;
      if (!enabled) return ok({ enabled } satisfies ListHealFindingsResponse);
      const a = auditOf(s.o.id),
        current = currentLoadId(s.o.id);
      const held = knobs.healRun &&
        current && {
          id: "run_" + current,
          loadId: current,
          status: knobs.healRun,
          pages: 0,
          cuts: 0,
          startedAt: now(),
          ...(knobs.healRun !== "running" && { finishedAt: now() }),
        };
      if (!a)
        return ok({
          enabled,
          ...(held && { lastRun: held }),
        } satisfies ListHealFindingsResponse);
      const open = a.findings
        .filter(
          (f) =>
            f.status === "open" &&
            (f.sources === "0" || Number(f.sources) >= 2),
        )
        .toSorted((x, y) => Number(y.support) - Number(x.support))
        .slice(0, SHOWN);
      const complete = {
        id: "run_" + a.loadId,
        loadId: a.loadId,
        status: "complete",
        pages: 1,
        cuts: 0,
        rowsMeasured: knobs.healRowsMeasured !== false,
        startedAt: a.runAt,
        finishedAt: a.runAt,
      };
      return ok({
        enabled,
        findings: open,
        lastRun: held || complete,
        lastCompleteRun: complete,
      } satisfies ListHealFindingsResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/heal-findings/{fingerprint}:dismiss`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const f = auditOf(s.o.id)?.findings.find(
        (x) => x.fingerprint === req.params.fingerprint,
      );
      if (!f) return fail("NOT_FOUND", "finding not found");
      if (knobs.healDismiss === "fails")
        return fail("INTERNAL", "the database went away");
      if (f.status === "open" && knobs.healDismiss === "superseded")
        f.status = "superseded";
      // Dismissing a dismissed finding again is no error.
      if (f.status === "dismissed")
        return ok({ finding: f } satisfies HealFindingResponse);
      if (f.status !== "open")
        return fail(
          "FAILED_PRECONDITION",
          `the finding is ${f.status}, not open`,
        );
      Object.assign(f, {
        status: "dismissed",
        dismissedByUserId: req.principal.user.id,
        supportAtDismissal: f.support,
      });
      return ok({ finding: f } satisfies HealFindingResponse);
    },
  },
];
