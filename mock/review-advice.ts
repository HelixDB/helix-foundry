import type {
  FoundryReviewChoice,
  FoundryReviewModelAdvice,
} from "../src/shared/wfe";
import { normalizeName } from "../src/web/areas/explore/naming";
import { reviewValues } from "../src/web/areas/reviews/model";
import type { Card, Review } from "./review-cards";

// Founder's model's advice on review cards, as the backend's graph worker
// stores it (backend/reviewadvice, workers/foundry-graph-worker
// review_advice.go) and the index serves it: deterministic, at no cost, on
// every open name or value card of the decide lane with two choices or
// more, whatever the rules say. The site's gate (src/web/areas/reviews/
// advice.ts advise) decides what shows: only where the rules abstain, and
// only while the card offers what the advice was offered.
//
// - a name: the candidate whose stored name shares the most words with the
//   document's name (the first on a tie);
// - a value: the value of the last quote stating one.
//
// Its rationale is a template, `rationale` when given (an e2e's text), and
// it cites the quote it stands on.

/**
 * The model's advice on `card`, from its review as the load holds it
 * (`review`), or undefined when it has none to give.
 *
 * @example
 * modelAdviceOf(nameCard, review); // { choice: { resolve: { entityKey: "scus_2" } }, offered: [...], rationale: "…", cites: [...] }
 */
export function modelAdviceOf(
  card: Card,
  review: Review | undefined,
  rationale?: string,
): FoundryReviewModelAdvice | undefined {
  if (!review || card.lane !== "decide") return undefined;
  const span = (doc: string, s: number, start: number, end: number) => ({
    docId: doc,
    span: { section: String(s), start: String(start), end: String(end) },
  });
  if (card.kind === "resolve") {
    const keys = [...new Set((review.candidates ?? []).filter(Boolean))];
    const words = new Set(normalizeName(review.norm_name ?? "").split(" "));
    const shared = (key: string) => {
      const name = review.concerned?.find((e) => e.key === key)?.props?.name;
      return typeof name === "string"
        ? normalizeName(name)
            .split(" ")
            .filter((w) => words.has(w)).length
        : 0;
    };
    const best = keys.reduce<string | undefined>(
      (a, k) => (a === undefined || shared(k) > shared(a) ? k : a),
      undefined,
    );
    const { section, start, end } = review;
    if (
      !best ||
      section == null ||
      start == null ||
      end == null ||
      end <= start
    )
      return undefined;
    const name = review.concerned?.find((e) => e.key === best)?.props?.name;
    return {
      choice: { resolve: { entityKey: best } },
      offered: [
        ...keys.map(
          (k): FoundryReviewChoice => ({ resolve: { entityKey: k } }),
        ),
        { resolve: { notInData: true } },
      ],
      rationale:
        rationale ??
        `${typeof name === "string" ? name : "This record"} shares the most words with the name the document uses.`,
      cites: [span(card.docId, section, start, end)],
    };
  }
  if (card.kind !== "prop") return undefined;
  const quotes = reviewValues(review.values);
  const values = new Map(quotes.map((q) => [JSON.stringify(q.value), q.value]));
  const last = quotes.at(-1);
  if (values.size < 2 || !last) return undefined;
  return {
    choice: { prop: last.value },
    offered: [...values.values()].map(
      (value): FoundryReviewChoice => ({ prop: value }),
    ),
    rationale: rationale ?? "The latest quote states this value.",
    cites: [span(last.documentId, last.section, last.start, last.end)],
  };
}
