/** A JSON object: not null and not an array. The parsers of JSON the site
 *  doesn't trust (the WFE's, the agent's, query results) check with it. */
export const isRecord = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
