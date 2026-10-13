import { expect, it } from "vitest";
import { focusDatasets } from "../apps/api/src/answer-context.js";

const dataset = (id: string, name: string, columns: string[], pad = 0) => ({
  id,
  name,
  description: "",
  columns: columns.map((c) => `${c} VARCHAR`),
  sample: [{ note: "x".repeat(pad) }],
});

it("sends every dataset while they fit the budget", () => {
  const all = [
    dataset("a", "orders", ["id"]),
    dataset("b", "customers", ["id"]),
  ];
  expect(focusDatasets(all, { text: "anything" }, 10_000)).toBe(all);
});

it("past the budget, sends the datasets the question is about, in their order", () => {
  const all = [
    dataset("u", "users_fct", ["user_id", "institution_id"], 400),
    dataset("x", "cdmast_raw", ["cdbal", "cdtype"], 400),
    dataset("i", "institution_fct", ["institution_id", "name"], 400),
    dataset("y", "lnhist_raw", ["lnamt"], 400),
  ];
  const focused = focusDatasets(
    all,
    { text: "Which institution fcts have the most users fcts?" },
    1_200,
  );
  expect(focused.map((d) => d.id)).toEqual(["u", "i"]);
});

it("keeps pinned datasets, then ranks names over columns over descriptions", () => {
  const all = [
    dataset("p", "previous_answer", ["anything"], 400),
    dataset("c", "ledger", ["balance"], 400),
    dataset("n", "balances", ["amount"], 400),
    dataset("z", "unrelated", ["other"], 400),
  ];
  const focused = focusDatasets(
    all,
    { text: "balance trend", pinned: ["p"] },
    1_100,
  );
  // The pinned one, then the name match; the column match no longer fits.
  expect(focused.map((d) => d.id)).toEqual(["p", "n"]);
});

it("falls back to the published types' datasets, then to what fits", () => {
  const all = [
    dataset("a", "alpha", ["one"], 400),
    dataset("b", "beta", ["two"], 400),
    dataset("c", "gamma", ["three"], 400),
  ];
  expect(
    focusDatasets(all, { text: "hello", preferred: ["c"] }, 1_000).map(
      (d) => d.id,
    ),
  ).toEqual(["c"]);
  expect(focusDatasets(all, { text: "hello" }, 1_000).map((d) => d.id)).toEqual(
    ["a", "b"],
  );
});

it("weighs a word by how few datasets use it, leaving weak matches out", () => {
  const all = [
    dataset("u", "users_fct", ["user_id"], 400),
    ...Array.from({ length: 6 }, (_, i) =>
      dataset(`f${i}`, `table${i}_fct`, ["other"], 400),
    ),
  ];
  // "fct" names every dataset, so only "users" tells them apart.
  expect(
    focusDatasets(all, { text: "How many users fcts?" }, 2_000).map(
      (d) => d.id,
    ),
  ).toEqual(["u"]);
});
