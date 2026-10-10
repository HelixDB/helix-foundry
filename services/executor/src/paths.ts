import { resolve } from "node:path";
// DuckDB spills here. Its default, `.tmp` in the working directory, is not
// writable in the container. The executor owns everything under it.
export const scratchRoot = (root: string) => resolve(root, "scratch");
// A "materialize" job writes all its rows here, as JSON lines, for the worker
// to read once and delete.
export const resultFile = (root: string, scope: string, version: string) =>
  resolve(root, "results", scope, version + ".jsonl");
