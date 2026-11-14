// The GitHub star prompt's contract: src/web/shell/star-prompt.tsx asks, and
// in a local build src/server/github-star.ts answers at /github-star.

/** The repository the prompt, and the setup prompts' agents, ask a star for. */
export const STAR_REPOSITORY = "helixdb/helix-foundry";
export const STAR_REPOSITORY_URL = `https://github.com/${STAR_REPOSITORY}`;

/**
 * GET /github-star's `star`: how the prompt stars the repository.
 *
 * - `starred`: the install's GitHub token has starred it, so nothing asks.
 * - `in_place`: the token can star it: the prompt's button has the site
 *   star it (POST /github-star, `{starred}`).
 * - `on_github`: no token, or GitHub didn't say: the button opens the
 *   repository on GitHub, as the hosted site's always does.
 */
export type StarCheck = "starred" | "in_place" | "on_github";

/**
 * The setup prompts' last ask (src/web/areas/ontology/onboarding-prompt.ts,
 * local-onboarding-prompt.ts), once the ontology is handed back: a star, only
 * if the user says yes, with their own `gh`, else a link.
 */
export const STAR_ASK = `Then ask me once whether I’d like to star Helix Foundry on GitHub. Only if I say yes, run \`gh api -X PUT user/starred/${STAR_REPOSITORY}\`; if \`gh\` isn’t installed or signed in, or that fails, give me ${STAR_REPOSITORY_URL} instead.`;
