import { readFileSync } from "node:fs";
import Ajv2020 from "ajv/dist/2020";

// The ontology agent's Read and Ask JSON Schemas and its Probe result
// (contract/ontology_agent, vendored by `pnpm contract:sync` beside the WFE's
// swagger), compiled once.
// The mock refuses reads the engine would refuse, and the unit tests hold
// every mock answer and fixture to the engine's shapes, so a schema that
// changes under the website fails CI.

const SCHEMAS = [
  "read_request",
  "read_result",
  "ask_request",
  "ask_event",
  "ask_result",
  "probe_result",
] as const;
export type AgentSchema =
  | (typeof SCHEMAS)[number]
  | `${(typeof SCHEMAS)[number]}#/$defs/${string}`;

const file = (name: string) =>
  readFileSync(
    new URL(`../contract/ontology_agent/${name}`, import.meta.url),
    "utf8",
  );

// Formats such as `uint` and `double` are the generator's annotations; the
// types and bounds beside them carry the rules.
const ajv = new Ajv2020({ allErrors: true, validateFormats: false });
for (const name of SCHEMAS)
  ajv.addSchema(JSON.parse(file(`${name}.json`)), name);

/** The vendored contract's version and hash, as its VERSION file says. */
export const AGENT_CONTRACT_VERSION = file("VERSION").trim();

/**
 * Why `value` doesn't fit a schema or one of its definitions, a line per
 * problem; none when it fits.
 *
 * @example
 * contractErrors("read_request#/$defs/ReadItem", { entity: { type: "Customer", id: "cus_1" } }); // []
 * contractErrors("ask_event", { event: "started" }); // ["/ must have required property 'model'", …]
 */
export function contractErrors(schema: AgentSchema, value: unknown): string[] {
  const validate = ajv.getSchema(schema);
  if (!validate) throw new Error(`the contract has no schema ${schema}`);
  return validate(value)
    ? []
    : (validate.errors || []).map(
        (e) => `${e.instancePath || "/"} ${e.message || e.keyword}`,
      );
}
