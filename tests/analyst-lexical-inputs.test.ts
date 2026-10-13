import { expect, it } from "vitest";
import { queryInputs } from "../apps/api/src/query-inputs.js";
import { datasetTable, type Resource } from "../packages/shared/src/index.js";

const dataset = (id: string, name: string) =>
  ({
    id,
    name,
    workspaceId: "w",
    kind: "dataset",
    description: "",
    revision: 1,
    createdAt: "",
    updatedAt: "",
    data: { activeVersion: "snapshot_" + id },
  }) as Resource;
const datasets = new Map([
  ["source_1", dataset("source_1", "changelog_entries")],
  ["source_2", dataset("source_2", "Sessions processed")],
]);

it.each([
  "'d_source_2'",
  "'JOIN sessions_processed'",
  "'it''s JOIN sessions_processed'",
  String.raw`E'it\'s JOIN sessions_processed'`,
  "$$JOIN sessions_processed d_source_2$$",
  "$note$ 'JOIN sessions_processed' $$d_source_2$$ $note$",
  String.raw`E'first'
'it\'s JOIN sessions_processed'`,
  String.raw`E'first' -- JOIN sessions_processed d_source_2
'it\'s JOIN sessions_processed'`,
  String.raw`E'first'
-- JOIN sessions_processed d_source_2
'it\'s JOIN sessions_processed'`,
  "$é$JOIN sessions_processed d_source_2$é$",
  "'first'\n'JOIN sessions_processed'",
])("does not cite literal text %s as another dataset", (literal) => {
  const sql = `SELECT ${literal} AS note FROM changelog_entries`;
  expect(queryInputs(sql, ["source_1", "source_2"], datasets)).toEqual([
    "source_1",
  ]);
});

it.each([
  "-- JOIN sessions_processed d_source_2\n",
  "-- JOIN sessions_processed d_source_2\r\n",
  "/* JOIN sessions_processed d_source_2 */",
  "/* ignored /* JOIN sessions_processed */ d_source_2 */",
])("does not cite comment text %s as another dataset", (comment) => {
  const sql = `SELECT * FROM changelog_entries ${comment}`;
  expect(queryInputs(sql, ["source_1", "source_2"], datasets)).toEqual([
    "source_1",
  ]);
});

it("keeps a real table reference immediately after a comment", () => {
  expect(
    queryInputs(
      "SELECT * FROM changelog_entries c JOIN/* d_not_a_table */sessions_processed s ON c.id=s.id",
      ["source_1"],
      datasets,
    ),
  ).toEqual(["source_1", "source_2"]);
});

it("keeps a real canonical table after a literal containing comment syntax", () => {
  expect(
    queryInputs(
      `SELECT '/* -- */' AS note FROM ${datasetTable("source_2")}`,
      ["source_1", "source_2"],
      datasets,
    ),
  ).toEqual(["source_2"]);
});

it("does not treat the escape-string prefix as a declared dataset name", () => {
  const special = new Map(datasets);
  special.set("source_3", dataset("source_3", "e"));
  expect(
    queryInputs(
      "SELECT E'plain text' AS note FROM changelog_entries",
      ["source_1", "source_3"],
      special,
    ),
  ).toEqual(["source_1"]);
});

it("keeps quoted identifiers containing literal and comment punctuation", () => {
  const special = new Map(datasets);
  special.set("source_3", dataset("source_3", "customer'--records"));
  expect(
    queryInputs('SELECT * FROM "customer\'--records"', ["source_1"], special),
  ).toEqual(["source_3"]);
});

it("preserves quoted readable names and comma joins", () => {
  expect(
    queryInputs(
      'SELECT * FROM changelog_entries c, "Sessions processed" s',
      ["source_1"],
      datasets,
    ),
  ).toEqual(["source_1", "source_2"]);
});

it("preserves the declared-input fallback for an unrecognised construct", () => {
  expect(
    queryInputs(
      "WITH p AS (PIVOT_WIDER sessions_processed ON kind) SELECT * FROM p",
      ["source_2"],
      datasets,
    ),
  ).toEqual(["source_2"]);
  expect(
    queryInputs(
      "SELECT 1 FROM something_else",
      ["source_1", "source_2"],
      datasets,
    ),
  ).toEqual(["source_1", "source_2"]);
});

it.each([
  String.raw`E'first'
'it\'s JOIN sessions_processed'`,
  "$é$JOIN sessions_processed d_source_2$é$",
])("keeps a real join after the complete literal %s", (literal) => {
  const sql = `SELECT ${literal} AS note FROM changelog_entries c JOIN sessions_processed s ON c.id=s.id`;
  expect(queryInputs(sql, ["source_1"], datasets)).toEqual([
    "source_1",
    "source_2",
  ]);
});
