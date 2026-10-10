import { resolve } from "node:path";
// DuckDB spills here. Its default, `.tmp` in the working directory, is not
// writable in the container. The executor owns everything under it.
export const scratchRoot = (root: string) => resolve(root, "scratch");
