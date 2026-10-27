// How long the session's access token has left, in whole seconds: the BFF
// sends it on every /papi answer that used the token. Once it runs out, the
// next call makes the BFF spend the session's single-use refresh token, so the
// SPA sends its calls one at a time (src/web/api.ts) until one reports a fresh
// token. Parallel calls would otherwise each try the refresh, possibly on
// different server instances, and every one but the first would fail and end
// the session.
export const SESSION_EXPIRES_HEADER = "x-foundry-session-expires-in";
