import type {
  FoundryPropValue,
  FoundryReviewChoice,
  FoundryReviewDecision,
  FoundryReviewLabel,
  ListReviewLabelsResponse,
  ReviewDecision,
  ReviewDecisionResponse,
  ReviewDrawn,
  ReviewLabelResponse,
  ListReviewDecisionsResponse,
} from "../../src/shared/wfe";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_LOAD,
  FOUNDRY_NODES as N,
} from "../../src/shared/foundry-labels";
import { createHash } from "node:crypto";
import { fail, id, now, ok, type MockRoute } from "../http";
import {
  decisionFits,
  decisionTypes,
  decisionValue,
  decisions,
  holdsOne,
  isInt64,
  labelClaim,
  labels,
  saveLabel,
} from "../review-decisions";
import { tenantGraph } from "./databases";
import {
  advanceLoads,
  appliedOf,
  currentWithdraws,
  currentLoadId,
  currentLoadVersionId,
  forgetRelease,
  forgetValue,
  owePendingRelease,
  owePendingValue,
} from "./loads";
import { isScope, ontologyOf } from "./ontologies";
import { ontologyVersion } from "./ontology";
import { isRecord } from "../../src/shared/json";

// Review decisions, as the backend serves them
// (api/review_decisions.go): settling reads the
// review's identity from the current load's FoundryReview, checks it and
// the choice against the loaded version's ontology.toml, and saves one
// decision per identity with its value as the engine keeps it (trimmed
// strings, UTC datetimes); the list pages 100 at a time by id; a removed
// name is gone for later loads only. Making a version current drops
// the decisions it doesn't take (pruneDecisions). An approval of a held
// claim applies to the current load at once (./loads.ts owePendingRelease),
// and so does a chosen value, which a removal takes back at once
// (owePendingValue, forgetValue): each decision says what became of it
// there (`applied`). The mock sets no limit on how many values are decided.
// A rejection of a held claim is only saved: no load is told. A claim has an
// approval or a rejection, never both: settling one removes the other, and
// a claim the current load already wrote on an approval is not rejected.

const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}";
const PAGE = 100;

/**
 * A FoundryPropValue in the JSON form the WFE writes one: exactly one arm,
 * of its JSON kind, an int64 as its digits in a string. (Protobuf's JSON
 * also reads "NULL_VALUE" and an int64 as a number; the mock doesn't.)
 */
function propValue(v: unknown): v is FoundryPropValue {
  if (!isRecord(v) || Object.keys(v).length !== 1) return false;
  const [[arm, x]] = Object.entries(v) as [[string, unknown]];
  switch (arm) {
    case "null":
      return x === null;
    case "text":
      return typeof x === "string";
    case "integer":
      return typeof x === "string" && isInt64(x);
    case "number":
      return typeof x === "number";
    case "boolean":
      return typeof x === "boolean";
    default:
      return false;
  }
}

/** The mock enforces protobuf's oneofs, including an explicit false refusal. */
function valid(b: unknown): b is FoundryReviewChoice {
  if (!isRecord(b)) return false;
  for (const [arm, other] of [
    ["release", "reject"],
    ["reject", "release"],
  ] as const)
    if (arm in b)
      return (
        typeof (b[arm] as { statementHash?: unknown } | undefined)
          ?.statementHash === "string" &&
        !(other in b) &&
        !("resolve" in b) &&
        !("prop" in b)
      );
  if ("prop" in b) return !("resolve" in b) && propValue(b.prop);
  if (!isRecord(b.resolve)) return false;
  const r = b.resolve;
  return (
    (r.notInData === true && !("entityKey" in r)) ||
    (typeof r.entityKey === "string" && !!r.entityKey && !("notInData" in r))
  );
}

const text = (v: unknown) => (typeof v === "string" ? v : "");
/** The decision for the same identity, which settling again replaces. */
const sameIdentity = (a: ReviewDecision, b: ReviewDecision) =>
  a.kind === "release" || a.kind === "reject" || a.kind === "withdraw"
    ? b.kind === a.kind &&
      a.docId === b.docId &&
      a.claimKey === b.claimKey &&
      a.claimSection === b.claimSection &&
      a.claimStart === b.claimStart &&
      a.claimEnd === b.claimEnd
    : a.kind === "resolve"
      ? b.kind === "resolve" &&
        a.type === b.type &&
        a.docId === b.docId &&
        a.normName === b.normName
      : b.kind === "prop" &&
        a.type === b.type &&
        a.entityKey === b.entityKey &&
        a.prop === b.prop;

/**
 * Saves `decision`, replacing the one of its identity, as settling again
 * does. A value is the exception, as in the backend's SettleFoundryValue:
 * the engine keeps a decision once, by its id, so another value for a
 * property is a new decision under a new id, and the same value chosen
 * again is the decision that stands, with its id, its time and who made it.
 * So is an approval of the statement it approved, as the backend keeps the
 * time a person first approved what they read: of a link's approvals the
 * last decided is its decision (contract 4.14.0).
 */
function save(
  ontology: string,
  decision: ReviewDecision,
  user: string,
  drawn?: ReviewDrawn,
): ReviewDecisionResponse {
  const own = decisions.get(ontology) || [];
  const previous = own.find((d) => sameIdentity(d, decision));
  const value = previous?.kind === "prop" && decision.kind === "prop";
  if (
    (value &&
      JSON.stringify(previous.value) === JSON.stringify(decision.value)) ||
    (previous?.kind === "release" &&
      decision.kind === "release" &&
      previous.statementHash === decision.statementHash)
  )
    return { decision: previous! };
  const saved = {
    ...decision,
    id: (!value && previous?.id) || id("decision"),
    decidedByUserId: user,
    decidedAt: now(),
    ...(drawn ? { drawn } : {}),
  };
  decisions.set(ontology, [
    ...own.filter((d) => d !== previous && d.id !== saved.id),
    saved,
  ]);
  return { decision: saved };
}

export const reviewRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${BASE}/reviews/{reviewId}:settle`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const choice: unknown = req.body?.decision;
      if (!valid(choice))
        return fail(
          "INVALID_ARGUMENT",
          "choose a candidate, not in data, or a property value",
        );
      const drawn: unknown = req.body?.drawn;
      if (
        drawn !== undefined &&
        drawn !== "" &&
        drawn !== "ranked" &&
        drawn !== "random"
      )
        return fail(
          "INVALID_ARGUMENT",
          `drawn must be ranked or random, not "${String(drawn)}"`,
        );
      const draw = drawn || undefined;
      const load = currentLoadId(s.o.id);
      if (!load)
        return fail(
          "FAILED_PRECONDITION",
          "load the ontology before settling a review",
        );
      const graph = tenantGraph(s.o.tenantId);
      // Only the current load can be settled; old loads remain read-only.
      const found = [...graph.nodes.values()].filter(
        (n) =>
          n.label === N.Review &&
          n.props.review_key === req.params.reviewId &&
          n.props[FOUNDRY_LOAD] === load,
      );
      if (found.length !== 1)
        return fail("NOT_FOUND", "review not found in the current load");
      const r = found[0]!.props;
      // A held claim names no type: the engine reads what it states from its
      // document. One held as conflicting with what its link holds is
      // settled so only on a link that holds one value (contract 4.14.0).
      if (
        text(r.reason) === "conflict" &&
        !holdsOne(
          ontologyVersion(s.o.id, currentLoadVersionId(s.o.id) || "")
            ?.definition,
          text(r.suggested_link),
        )
      )
        return fail(
          "FAILED_PRECONDITION",
          "this claim conflicts with a property of a fact the graph holds; only a conflict on a link that holds one value can be approved",
        );
      if (
        ["low_confidence", "unverified", "conflict"].includes(text(r.reason))
      ) {
        const read = (choice.release ?? choice.reject)?.statementHash;
        if (read === undefined)
          return fail(
            "INVALID_ARGUMENT",
            "this review requires a release or a reject decision",
          );
        const docId = text(r.doc_id),
          claimKey = text(r.claim_key),
          link = text(r.suggested_link),
          stored = text(r.statement);
        if (!docId || !/^[0-9a-f]{64}$/.test(claimKey) || !link || !stored)
          return fail(
            "FAILED_PRECONDITION",
            "this review is missing its identity; start a new load before settling it",
          );
        const statementHash = createHash("sha256")
          .update(`${link}\u001f${stored}`)
          .digest("hex");
        if (read !== statementHash)
          return fail(
            "FAILED_PRECONDITION",
            "this claim reads differently now; reload the review and decide on what it says",
          );
        // The claim's decisions: at its place, under its key or of what was
        // read there under another (a rebuild moves keys), as the engine
        // binds a release from contract 4.14.0 and the backend keeps them.
        // One of its kind of what was read here stands, the first decided,
        // and takes the claim's key; every other gives way, and an approval
        // the load has not written leaves its list too.
        const claim = {
          docId,
          claimKey,
          claimSection: Number(r.section),
          claimStart: Number(r.start),
          claimEnd: Number(r.end),
        };
        const kind = choice.reject ? "reject" : "release";
        const own = decisions.get(s.o.id) || [];
        const mine = own.filter(
          (
            d,
          ): d is Extract<
            FoundryReviewDecision,
            { kind: "release" | "reject" }
          > =>
            (d.kind === "release" || d.kind === "reject") &&
            d.docId === claim.docId &&
            d.claimSection === claim.claimSection &&
            d.claimStart === claim.claimStart &&
            d.claimEnd === claim.claimEnd &&
            (d.claimKey === claim.claimKey ||
              d.statementHash === statementHash),
        );
        const same = mine.filter((d) => d.kind === kind);
        const stands =
          same
            .filter((d) => d.statementHash === statementHash)
            .sort(
              (a, b) =>
                (a.decidedAt ?? "").localeCompare(b.decidedAt ?? "") ||
                a.id.localeCompare(b.id),
            )[0] ?? same.find((d) => d.claimKey === claim.claimKey);
        for (const d of mine)
          if (d !== stands && d.kind === "release") forgetRelease(s.o.id, d.id);
        decisions.set(s.o.id, [
          ...own.filter((d) => !mine.some((m) => m.id === d.id)),
          ...(stands ? [{ ...stands, claimKey: claim.claimKey }] : []),
        ]);
        const { decision: saved } = save(
          s.o.id,
          { kind, type: "", ...claim, statementHash },
          req.principal.user.id,
          draw,
        );
        if (saved.kind === "release") owePendingRelease(s.o.id, saved);
        // A decision of the random slice is a label of the claim too, with
        // the score its review was read at, unless this person re-checked
        // the claim: that word stands.
        const rechecked = (labels.get(s.o.id) || []).some(
          (l) =>
            l.drawn === "recheck" &&
            l.decidedByUserId === req.principal.user.id &&
            l.docId === claim.docId &&
            l.claimSection === claim.claimSection &&
            l.claimStart === claim.claimStart &&
            l.claimEnd === claim.claimEnd &&
            l.claimKey === claim.claimKey,
        );
        if (draw === "random" && !rechecked)
          saveLabel(s.o.id, {
            id: id("label"),
            drawn: "random",
            ...claim,
            statementHash,
            link,
            ...(typeof r.confidence === "number"
              ? { confidence: r.confidence }
              : {}),
            verdict: kind === "release" ? "right" : "wrong",
            decisionId: saved.id,
            loadId: load,
            decidedByUserId: req.principal.user.id,
            decidedAt: now(),
          });
        return ok({
          decision: { ...saved, applied: appliedOf(s.o.id, saved) },
        } satisfies ReviewDecisionResponse);
      }
      const type = text(r.type);
      if (!type)
        return fail(
          "FAILED_PRECONDITION",
          "this review is missing its identity; start a new load before settling it",
        );
      const types = decisionTypes(
        ontologyVersion(s.o.id, currentLoadVersionId(s.o.id) || "")?.definition,
      );
      const t = types[type];
      if (!t)
        return fail(
          "FAILED_PRECONDITION",
          "the review type is not in the loaded version",
        );
      let decision: ReviewDecision;
      switch (text(r.reason)) {
        case "unresolved":
        case "ambiguous":
        case "collision":
        case "identifier_conflict":
        case "adjudicated":
        case "legal_form":
        case "part_name":
        case "own_name":
          if (!choice.resolve)
            return fail(
              "INVALID_ARGUMENT",
              "this review requires a resolve decision",
            );
          decision = {
            kind: "resolve",
            type,
            docId: text(r.doc_id),
            normName: text(r.norm_name),
            ...(choice.resolve.entityKey === undefined
              ? {}
              : { entityKey: choice.resolve.entityKey }),
          };
          break;
        case "prop_conflict": {
          if (choice.prop === undefined)
            return fail(
              "INVALID_ARGUMENT",
              "this review requires a prop decision",
            );
          const prop = text(r.prop);
          if (!(prop in t.props))
            return fail(
              "FAILED_PRECONDITION",
              "the property is not in the loaded version",
            );
          const value = decisionValue(t.props[prop]!, choice.prop);
          if (value === undefined)
            return fail(
              "INVALID_ARGUMENT",
              "the chosen value does not match the property's type",
            );
          decision = {
            kind: "prop",
            type,
            entityKey: text(r.entity_key),
            prop,
            value,
          };
          break;
        }
        default:
          return fail("INVALID_ARGUMENT", "this review kind cannot be settled");
      }
      if (
        decision.kind === "resolve"
          ? !decision.docId || !decision.normName
          : !decision.entityKey || !decision.prop
      )
        return fail(
          "FAILED_PRECONDITION",
          "the review is missing its decision identity",
        );
      // The chosen entity, or a property's entity, must be in this load.
      const key = decision.entityKey;
      const identity = t.key.length ? "entity_key" : "norm_name";
      if (
        key !== undefined &&
        [...graph.nodes.values()].filter(
          (n) =>
            n.label === type &&
            n.props[identity] === key &&
            n.props[FOUNDRY_LOAD] === load,
        ).length !== 1
      )
        return fail(
          "INVALID_ARGUMENT",
          "the chosen entity is not in this load",
        );
      // The backend checks again under the ontology's lock: a load of another
      // version may have become current since the review was read.
      if (!decisionFits({ ...decision, id: "" }, types))
        return fail(
          "FAILED_PRECONDITION",
          "the current load's version no longer has this review's type or property; reload the review and try again",
        );
      const { decision: stored } = save(
        s.o.id,
        decision,
        req.principal.user.id,
        draw,
      );
      if (stored.kind === "prop") owePendingValue(s.o.id, stored);
      const applied = appliedOf(s.o.id, stored);
      return ok({
        decision: applied ? { ...stored, applied } : stored,
      } satisfies ReviewDecisionResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/review-decisions`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const token = req.query.get("pageToken") || "";
      if (token.length > 26)
        return fail("INVALID_ARGUMENT", "invalid page_token");
      // Approvals whose job's step has passed are applied by now.
      advanceLoads(s.o.id);
      // Keyset paging: every decision after the token's id, and, for a
      // client that asks, the current load's withdraws, approvals taken back
      // from it (a client before them gets none, as the backend's does).
      const after = [
        ...(decisions.get(s.o.id) || []),
        ...(req.query.get("includeWithdraws") === "true"
          ? currentWithdraws(s.o.id)
          : []),
      ]
        .filter((d) => d.id > token)
        .toSorted((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
      const page = after.slice(0, PAGE);
      return ok({
        decisions: page.map((d) => {
          const applied = appliedOf(s.o.id, d);
          return applied ? { ...d, applied } : d;
        }),
        nextPageToken: after.length > PAGE ? page.at(-1)!.id : "",
      } satisfies ListReviewDecisionsResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/review-decisions/{id}`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const own = decisions.get(s.o.id) || [];
      if (!own.some((d) => d.id === req.params.id))
        return fail("NOT_FOUND", "review decision not found");
      // One the current load has not written is taken back from it too, and
      // a chosen value from every load that holds it.
      forgetRelease(s.o.id, req.params.id!);
      forgetValue(s.o.id, req.params.id!);
      decisions.set(
        s.o.id,
        own.filter((d) => d.id !== req.params.id),
      );
      return ok();
    },
  },
  {
    method: "POST",
    path: `${BASE}/review-labels`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const b = isRecord(req.body) ? req.body : {};
      const user = req.principal.user.id;
      if (b.verdict !== "right" && b.verdict !== "wrong")
        return fail("INVALID_ARGUMENT", "verdict must be right or wrong");
      if (b.drawn !== "random" && b.drawn !== "recheck")
        return fail("INVALID_ARGUMENT", "drawn must be random or recheck");
      const load = currentLoadId(s.o.id);
      if (!load)
        return fail(
          "FAILED_PRECONDITION",
          "load the ontology before labelling a claim",
        );
      const subjects = ["fact", "reviewId", "decisionId", "labelId"].filter(
        (k) => b[k] !== undefined,
      );
      if (subjects.length !== 1)
        return fail(
          "INVALID_ARGUMENT",
          "a label is of a fact, a held claim, a decision or a label",
        );
      const subject = subjects[0]!;
      // A draw is of a claim the load holds; a re-check of another's word.
      const random = b.drawn === "random";
      if (random !== (subject === "fact" || subject === "reviewId"))
        return fail(
          "INVALID_ARGUMENT",
          random
            ? "a decision or a label is re-checked, never drawn"
            : "a claim the load holds is labelled when drawn at random",
        );
      const base = {
        id: id("label"),
        drawn: b.drawn,
        verdict: b.verdict,
        loadId: load,
        decidedByUserId: user,
        decidedAt: now(),
      } as const;
      const graph = tenantGraph(s.o.tenantId);
      const own = labels.get(s.o.id) || [];
      let label: FoundryReviewLabel;
      if (subject === "fact") {
        const f = isRecord(b.fact) ? b.fact : {};
        const [section, start, end] = [f.section ?? 0, f.start ?? 0, f.end];
        if (
          typeof f.docId !== "string" ||
          !f.docId ||
          typeof f.factKey !== "string" ||
          !f.factKey ||
          f.factKey.length > 128 ||
          ![section, start, end].every(
            (n) => typeof n === "number" && Number.isSafeInteger(n) && n >= 0,
          ) ||
          (start as number) >= (end as number)
        )
          return fail(
            "INVALID_ARGUMENT",
            "a fact is its document, section, byte range and fact key",
          );
        // The quote as the current load holds it: its link and score.
        const quote = [...graph.nodes.values()].find((n) => {
          if (
            n.label !== N.Evidence ||
            n.props[FOUNDRY_LOAD] !== load ||
            n.props.fact_key !== f.factKey ||
            n.props.doc_id !== f.docId ||
            n.props.start !== start ||
            n.props.end !== end
          )
            return false;
          const at = (graph.outgoing.get(n.id) || [])
            .map((e) => graph.edges.get(e)!)
            .find((e) => e.label === E.FROM_SECTION)?.to;
          return (
            at !== undefined && graph.nodes.get(at)?.props.ordinal === section
          );
        });
        if (!quote || typeof quote.props.link !== "string")
          return fail(
            "NOT_FOUND",
            "the current load holds no such quote of the fact",
          );
        label = {
          ...base,
          docId: f.docId,
          claimSection: section as number,
          claimStart: start as number,
          claimEnd: end as number,
          factKey: f.factKey,
          link: quote.props.link,
          ...(typeof quote.props.confidence === "number"
            ? { confidence: quote.props.confidence }
            : {}),
        };
      } else if (subject === "reviewId") {
        const found = [...graph.nodes.values()].filter(
          (n) =>
            n.label === N.Review &&
            n.props.review_key === b.reviewId &&
            n.props[FOUNDRY_LOAD] === load,
        );
        if (found.length !== 1)
          return fail("NOT_FOUND", "review not found in the current load");
        const r = found[0]!.props;
        if (
          !["low_confidence", "unverified", "conflict"].includes(text(r.reason))
        )
          return fail(
            "FAILED_PRECONDITION",
            "only a held claim is labelled so",
          );
        const link = text(r.suggested_link),
          stored = text(r.statement);
        if (!text(r.doc_id) || !link || !stored)
          return fail(
            "FAILED_PRECONDITION",
            "this review is missing its identity; start a new load before labelling it",
          );
        label = {
          ...base,
          docId: text(r.doc_id),
          claimSection: Number(r.section),
          claimStart: Number(r.start),
          claimEnd: Number(r.end),
          claimKey: text(r.claim_key),
          statementHash: createHash("sha256")
            .update(`${link}\u001f${stored}`)
            .digest("hex"),
          link,
          ...(typeof r.confidence === "number"
            ? { confidence: r.confidence }
            : {}),
        };
      } else if (subject === "decisionId") {
        const d = (decisions.get(s.o.id) || []).find(
          (d) => d.id === b.decisionId,
        );
        if (!d) return fail("NOT_FOUND", "review decision not found");
        if (d.kind !== "release" && d.kind !== "reject")
          return fail(
            "FAILED_PRECONDITION",
            "only a decision on a held claim is re-checked",
          );
        if (d.decidedByUserId === user)
          return fail(
            "FAILED_PRECONDITION",
            "a re-check is another person's word, not the decision's own",
          );
        const random = own.find(
          (l) => l.decisionId === d.id && l.drawn === "random",
        );
        label = {
          ...base,
          docId: d.docId,
          claimSection: d.claimSection,
          claimStart: d.claimStart,
          claimEnd: d.claimEnd,
          claimKey: d.claimKey,
          statementHash: d.statementHash,
          decisionId: d.id,
          against: d.kind === "release" ? "right" : "wrong",
          ...(random?.link ? { link: random.link } : {}),
          ...(random?.confidence !== undefined
            ? { confidence: random.confidence }
            : {}),
        };
      } else {
        const l = own.find((l) => l.id === b.labelId);
        if (!l) return fail("NOT_FOUND", "review label not found");
        if (l.drawn !== "random")
          return fail(
            "FAILED_PRECONDITION",
            "only a label drawn at random is re-checked",
          );
        if (l.decidedByUserId === user)
          return fail(
            "FAILED_PRECONDITION",
            "a re-check is another person's word, not the label's own",
          );
        label = {
          ...base,
          docId: l.docId,
          claimSection: l.claimSection,
          claimStart: l.claimStart,
          claimEnd: l.claimEnd,
          ...(l.claimKey
            ? { claimKey: l.claimKey, statementHash: l.statementHash }
            : { factKey: l.factKey }),
          ...(l.link ? { link: l.link } : {}),
          ...(l.confidence !== undefined ? { confidence: l.confidence } : {}),
          against: l.verdict,
        };
      }
      // One label per person and claim: neither kind replaces the other.
      const other = own.find(
        (l) =>
          labelClaim(l) === labelClaim(label) &&
          l.decidedByUserId === user &&
          l.drawn !== label.drawn,
      );
      if (other)
        return fail(
          "FAILED_PRECONDITION",
          other.drawn === "random"
            ? "you have labelled this claim already; a re-check is another person's"
            : "you have re-checked this claim; that word stands",
        );
      return ok({
        label: saveLabel(s.o.id, label),
      } satisfies ReviewLabelResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/review-labels`,
    handle: (req) => {
      const s = ontologyOf(req, false);
      if (!isScope(s)) return s;
      const token = req.query.get("pageToken") || "";
      if (token.length > 26)
        return fail("INVALID_ARGUMENT", "invalid page_token");
      const after = (labels.get(s.o.id) || [])
        .filter((l) => l.id > token)
        .toSorted((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
      const page = after.slice(0, PAGE);
      return ok({
        labels: page,
        nextPageToken: after.length > PAGE ? page.at(-1)!.id : "",
      } satisfies ListReviewLabelsResponse);
    },
  },
];
