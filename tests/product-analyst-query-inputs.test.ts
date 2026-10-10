import { it, expect } from "vitest";
import { queryInputs } from "../apps/api/src/query-inputs.js";
import { datasetTable } from "../packages/shared/src/index.js";
import type { Resource } from "../packages/shared/src/index.js";

// An answer's citations are its inputs. The model's declared inputs used to
// be trusted as they were, so a query over one table could cite four
// datasets, three of which it never read; a small model can even declare
// four datasets and read a fifth.
const dataset = (id: string, name: string) =>
  ({
    id,
    workspaceId: "w",
    kind: "dataset",
    name,
    description: "",
    revision: 1,
    createdAt: "",
    updatedAt: "",
    data: { activeVersion: "snap_" + id },
  }) as unknown as Resource;

const datasets = new Map<string, Resource>([
  ["source_1", dataset("source_1", "changelog_entries")],
  ["source_2", dataset("source_2", "Sessions processed")],
  ["dataset_upload_3", dataset("dataset_upload_3", "lever_ledger_nodes")],
  ["dataset_upload_4", dataset("dataset_upload_4", "eval_ledger_nodes")],
]);

it("drops declared datasets the SQL never names", () => {
  const sql =
    "SELECT project, count(*) FROM changelog_entries WHERE created_at >= '2026-01-01' GROUP BY 1";
  expect(
    queryInputs(
      sql,
      ["dataset_upload_3", "dataset_upload_4", "source_1"],
      datasets,
    ),
  ).toEqual(["source_1"]);
});

it("keeps a dataset named by its table, its name or an alias", () => {
  const byTable = `SELECT count(*) FROM ${datasetTable("source_2")}`;
  expect(queryInputs(byTable, ["source_1", "source_2"], datasets)).toEqual([
    "source_2",
  ]);
  const byQuotedName = 'SELECT count(*) FROM "Sessions processed" s';
  expect(queryInputs(byQuotedName, ["source_1", "source_2"], datasets)).toEqual(
    ["source_2"],
  );
  const byAlias = "SELECT count(*) FROM sessions_processed s";
  expect(queryInputs(byAlias, ["source_1", "source_2"], datasets)).toEqual([
    "source_2",
  ]);
});

it("counts a dataset table in a comma join, as the local model writes them", () => {
  const sql = `SELECT c.name, sum(o.amount) FROM ${datasetTable("source_1")} o, ${datasetTable("source_2")} c GROUP BY 1`;
  expect(queryInputs(sql, ["source_1"], datasets)).toEqual([
    "source_1",
    "source_2",
  ]);
});

it("adds a table the SQL names even when it was not declared", () => {
  const sql = `SELECT * FROM changelog_entries c JOIN ${datasetTable("source_2")} s ON s.id = c.id`;
  expect(queryInputs(sql, ["source_1"], datasets)).toEqual([
    "source_1",
    "source_2",
  ]);
});

it("does not match a dataset name that is only part of another word", () => {
  const sql = "SELECT * FROM changelog_entries_archive";
  expect(queryInputs(sql, ["source_1", "source_2"], datasets)).toEqual([
    "source_1",
    "source_2",
  ]);
});

it("resolves declared names and table names to dataset ids", () => {
  const sql = "SELECT * FROM changelog_entries";
  expect(
    queryInputs(sql, ["changelog_entries", datasetTable("source_1")], datasets),
  ).toEqual(["source_1"]);
});

it("cites only the table the SQL reads when the declaration is wrong", () => {
  const sql = `SELECT strftime('%Y-%m', created_at) AS month, count(*) AS count FROM ${datasetTable("source_1")} GROUP BY month`;
  expect(
    queryInputs(
      sql,
      ["dataset_upload_3", "dataset_upload_4", "source_2"],
      datasets,
    ),
  ).toEqual(["source_1"]);
});

it("does not take a dataset name used as a column or function for a table", () => {
  const withCount = new Map(datasets);
  withCount.set("source_9", dataset("source_9", "count"));
  const sql = "SELECT count(*) FROM changelog_entries";
  expect(queryInputs(sql, ["source_1"], withCount)).toEqual(["source_1"]);
});

it("falls back to the declaration when the SQL reads nothing recognisable", () => {
  const sql = "SELECT 1 FROM something_else";
  expect(queryInputs(sql, ["source_1", "source_2"], datasets)).toEqual([
    "source_1",
    "source_2",
  ]);
});
