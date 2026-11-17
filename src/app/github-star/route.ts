import { localConfig } from "@/server/config";
import {
  checkStar,
  type GitHubStarConfig,
  githubToken,
  starRepository,
} from "@/server/github-star";
import { LOCAL } from "@/shared/deployment";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// /github-star: the star prompt's (src/web/shell/star-prompt.tsx), in a local
// build only, which stars with the install's GitHub token. The hosted site
// holds no GitHub token of the user's: its prompt opens the repository.
function config(): GitHubStarConfig | null {
  const local = LOCAL ? localConfig() : null;
  return local && { origin: local.origin, token: githubToken(process.env) };
}

export function GET(request: Request) {
  const c = config();
  return c ? checkStar(request, c, fetch) : new Response(null, { status: 404 });
}

export function POST(request: Request) {
  const c = config();
  return c
    ? starRepository(request, c, fetch)
    : new Response(null, { status: 404 });
}
