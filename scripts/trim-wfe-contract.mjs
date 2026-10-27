// Trims contract/wfe.swagger.json to what the site calls: the WFE routes in
// src/server/papi-allowlist.ts and the mock's routes (mock/routes, which also
// serve the sign-in the server calls directly), each with only those methods,
// and the definitions they reach. The rest of hosted Helix's API stays out of this
// repository. scripts/sync-contract.sh runs it after each sync; run it alone
// as `node scripts/trim-wfe-contract.mjs`.
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import process from "node:process";

const file = "contract/wfe.swagger.json";
const swagger = JSON.parse(readFileSync(file, "utf8"));
const sources = [
  "src/server/papi-allowlist.ts",
  ...readdirSync("mock/routes")
    .filter((f) => f.endsWith(".ts"))
    .map((f) => `mock/routes/${f}`),
].map((f) => readFileSync(f, "utf8"));
const allowed = new Map();
for (const [, method, path] of sources
  .join("\n")
  .matchAll(/method:\s*"(GET|POST|PUT|PATCH|DELETE)",\s*path:\s*"([^"]+)"/g))
  allowed.set(path, [...(allowed.get(path) ?? []), method.toLowerCase()]);

const paths = {};
for (const [path, methods] of Object.entries(swagger.paths ?? {})) {
  const keep = Object.fromEntries(
    Object.entries(methods).filter(([m]) => allowed.get(path)?.includes(m)),
  );
  if (Object.keys(keep).length) paths[path] = keep;
}

// Every definition a kept operation reaches, following $refs.
const definitions = swagger.definitions ?? {};
const reached = new Set();
const walk = (node) => {
  if (Array.isArray(node)) return node.forEach(walk);
  if (!node || typeof node !== "object") return;
  for (const [key, value] of Object.entries(node)) {
    if (key === "$ref" && typeof value === "string") {
      const name = value.replace("#/definitions/", "");
      if (!reached.has(name) && definitions[name]) {
        reached.add(name);
        walk(definitions[name]);
      }
    } else walk(value);
  }
};
walk(paths);

const tags = new Set(
  Object.values(paths).flatMap((ms) =>
    Object.values(ms).flatMap((op) => op.tags ?? []),
  ),
);
const trimmed = {
  ...swagger,
  tags: (swagger.tags ?? []).filter((t) => tags.has(t.name)),
  paths,
  definitions: Object.fromEntries(
    Object.keys(definitions)
      .filter((d) => reached.has(d))
      .map((d) => [d, definitions[d]]),
  ),
};
writeFileSync(file, JSON.stringify(trimmed, null, 2) + "\n");
process.stdout.write(
  `contract/wfe.swagger.json trimmed to ${Object.keys(paths).length} paths and ${reached.size} definitions\n`,
);
