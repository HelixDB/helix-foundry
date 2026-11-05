import type { CannotAnswerKind } from "./wfe";

/** A known gap kind; untrusted or absent values carry no classification. */
export function cannotAnswerKind(value: unknown): CannotAnswerKind | undefined {
  switch (value) {
    case "uncovered":
    case "after_as_of":
    case "undeclared_relation":
    case "undeclared_property":
    case "absent":
      return value;
    default:
      return undefined;
  }
}
