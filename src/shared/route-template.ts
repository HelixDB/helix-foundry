// WFE paths are written the way the swagger writes them:
// "/v1/workspaces/{id}" or "/v1/workspaces/{workspaceId}/foundry/connectors/{id}:sync".
// A {param} matches exactly one path segment, and never a ":verb" suffix;
// everything else is literal. There are no wildcards. The /papi allowlist and
// the mock WFE both match paths through here.

export type RouteTemplate = `/v1/${string}`;
export type HttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

// One path segment's value: ids, slugs and names, never "/" or ":", and
// never the dot segments "." or "..", which a URL would resolve away.
const SEGMENT_VALUE = "(?!\\.\\.?(?:[/:]|$))[A-Za-z0-9._-]+";
const PARAM = /\{([A-Za-z][A-Za-z0-9_]*)\}/g;
const TEMPLATE =
  /^\/v1(?:\/(?:[A-Za-z0-9._-]+|\{[A-Za-z][A-Za-z0-9_]*\})+(?::[A-Za-z][A-Za-z0-9-]*)?)+$/;

export type CompiledTemplate = {
  template: RouteTemplate;
  regex: RegExp;
  params: string[];
};

/**
 * Compiles a template, refusing anything that is not a plain WFE path.
 *
 * @example
 * compileTemplate("/v1/workspaces/{id}").regex.test("/v1/workspaces/ws_1"); // true
 * compileTemplate("/v1/*"); // throws
 */
export function compileTemplate(template: RouteTemplate): CompiledTemplate {
  if (!TEMPLATE.test(template))
    throw new Error(`Not a WFE path template: ${template}`);
  const params: string[] = [];
  const source = template
    .split(PARAM)
    .map((part, i) => {
      if (i % 2) {
        params.push(part);
        return `(${SEGMENT_VALUE})`;
      }
      return part.replace(/[.]/g, "\\.");
    })
    .join("");
  return { template, regex: new RegExp(`^${source}$`), params };
}

/** The template's parameters for `path`, or null when it does not match. */
export function matchTemplate(
  compiled: CompiledTemplate,
  path: string,
): Record<string, string> | null {
  const m = compiled.regex.exec(path);
  if (!m) return null;
  return Object.fromEntries(compiled.params.map((p, i) => [p, m[i + 1]!]));
}
