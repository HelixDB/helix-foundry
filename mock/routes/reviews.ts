import type {
  FoundryPropValue,
  FoundryReviewChoice,
  ReviewDecision,
  ReviewDecisionResponse,
  ListReviewDecisionsResponse,
} from "../../src/shared/wfe";
import {
  FOUNDRY_LOAD,
  FOUNDRY_NODES as N,
} from "../../src/shared/foundry-labels";
import { fail, id, now, ok, type MockRoute } from "../http";
import {
  decisionFits,
  decisionTypes,
  decisionValue,
  decisions,
  isInt64,
} from "../review-decisions";
import { tenantGraph } from "./databases";
import { currentLoadId, currentLoadVersionId } from "./loads";
import { isScope, ontologyOf } from "./ontologies";
import { ontologyVersion } from "./ontology";

// Review decisions, as the backend serves them
// (api/review_decisions.go): settling reads the
// review's identity from the current load's FoundryReview, checks it and
// the choice against the loaded version's ontology.toml, and saves one
// decision per identity with its value as the engine keeps it (trimmed
// strings, UTC datetimes); the list pages 100 at a time by id; a removed
// decision is gone for later loads only. Making a version current drops
// the decisions it doesn't take (pruneDecisions).

const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}";
const PAGE = 100;

const isObject = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
/**
 * A FoundryPropValue in the JSON form the WFE writes one: exactly one arm,
 * of its JSON kind, an int64 as its digits in a string. (Protobuf's JSON
 * also reads "NULL_VALUE" and an int64 as a number; the mock doesn't.)
 */
function propValue(v: unknown): v is FoundryPropValue {
  if (!isObject(v) || Object.keys(v).length !== 1) return false;
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
  if (!isObject(b)) return false;
  if ("prop" in b) return !("resolve" in b) && propValue(b.prop);
  if (!isObject(b.resolve)) return false;
  const r = b.resolve;
  return (
    (r.notInData === true && !("entityKey" in r)) ||
    (typeof r.entityKey === "string" && !!r.entityKey && !("notInData" in r))
  );
}

const text = (v: unknown) => (typeof v === "string" ? v : "");
/** The decision for the same identity, which settling again replaces. */
const sameIdentity = (a: ReviewDecision, b: ReviewDecision) =>
  a.kind === "resolve"
    ? b.kind === "resolve" &&
      a.type === b.type &&
      a.docId === b.docId &&
      a.normName === b.normName
    : b.kind === "prop" &&
      a.type === b.type &&
      a.entityKey === b.entityKey &&
      a.prop === b.prop;

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
      const own = decisions.get(s.o.id) || [];
      const previous = own.find((d) => sameIdentity(d, decision));
      const saved = {
        ...decision,
        id: previous?.id || id("decision"),
        decidedByUserId: req.principal.user.id,
        decidedAt: now(),
      };
      decisions.set(s.o.id, [...own.filter((d) => d.id !== saved.id), saved]);
      return ok({ decision: saved } satisfies ReviewDecisionResponse);
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
      // Keyset paging: every decision after the token's id.
      const after = (decisions.get(s.o.id) || [])
        .filter((d) => d.id > token)
        .toSorted((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
      const page = after.slice(0, PAGE);
      return ok({
        decisions: page,
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
      decisions.set(
        s.o.id,
        own.filter((d) => d.id !== req.params.id),
      );
      return ok();
    },
  },
];
