import { STAR_REPOSITORY, type StarCheck } from "@/shared/github-star";

// GitHub's record of the token's account starring the repository: GET
// answers 204 when it has and 404 when it hasn't, PUT 204 once it has.
const STARRED_URL = `https://api.github.com/user/starred/${STAR_REPOSITORY}`;
// How long the site waits for GitHub before it answers the page anyway.
const UPSTREAM_TIMEOUT_MS = 5000;

/**
 * The GitHub token a local install stars with: GH_TOKEN, else GITHUB_TOKEN,
 * as `gh` reads them, which local/compose.yaml passes to the site from the
 * shell that started it. "" when neither is set. Read per request, as the
 * site's environment is fixed at its start.
 *
 * @example
 * githubToken({ GH_TOKEN: " ghp_a ", GITHUB_TOKEN: "ghp_b" }); // "ghp_a"
 * githubToken({ GH_TOKEN: "", GITHUB_TOKEN: "ghp_b" }); // "ghp_b"
 * githubToken({}); // ""
 */
export function githubToken(env: Record<string, string | undefined>): string {
  return env.GH_TOKEN?.trim() || env.GITHUB_TOKEN?.trim() || "";
}

/** A local install's site, as /github-star reads it. */
export type GitHubStarConfig = {
  /** The loopback origin the browser uses (LocalConfig's). */
  origin: string;
  /** githubToken, read for this request; "" for none. */
  token: string;
};

/** GitHub's status for the star with the token, or 0 when it didn't answer. */
const github = (
  method: "GET" | "PUT",
  token: string,
  upstream: typeof fetch,
): Promise<number> =>
  upstream(STARRED_URL, {
    method,
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "user-agent": "helix-foundry",
      "x-github-api-version": "2022-11-28",
    },
    signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
  }).then(
    (res) => res.status,
    () => 0,
  );

const NO_STORE = { headers: { "Cache-Control": "no-store" } };
const ownHost = (request: Request, config: GitHubStarConfig) =>
  request.headers.get("host") === new URL(config.origin).host;

/**
 * GET /github-star in a local build: `{star}` (StarCheck), whether the
 * install's GitHub token has starred the repository, can star it, or
 * neither: no token, a token GitHub refuses, or no answer in time. Only the
 * site's own address is answered.
 */
export async function checkStar(
  request: Request,
  config: GitHubStarConfig,
  upstream: typeof fetch,
): Promise<Response> {
  if (!ownHost(request, config)) return new Response(null, { status: 403 });
  const status = config.token ? await github("GET", config.token, upstream) : 0;
  const star: StarCheck =
    status === 204 ? "starred" : status === 404 ? "in_place" : "on_github";
  return Response.json({ star }, NO_STORE);
}

/**
 * POST /github-star in a local build: stars the repository with the
 * install's GitHub token, answering `{starred}`, whether GitHub did. Only the
 * site's own pages may post (its Host and Origin), so another site open in
 * the browser can't star through it. The token never leaves the site.
 */
export async function starRepository(
  request: Request,
  config: GitHubStarConfig,
  upstream: typeof fetch,
): Promise<Response> {
  if (
    !ownHost(request, config) ||
    request.headers.get("origin") !== config.origin
  )
    return new Response(null, { status: 403 });
  const status = config.token ? await github("PUT", config.token, upstream) : 0;
  return Response.json({ starred: status === 204 }, NO_STORE);
}
