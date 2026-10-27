// The CSRF handshake between the SPA and the BFF: GET /papi/csrf returns a
// token (also set as the HttpOnly CSRF cookie), and every unsafe request
// sends it back in this header. Both must match and verify.
export const CSRF_HEADER_NAME = "x-helix-csrf";
export const CSRF_TOKEN_TTL_SECONDS = 60 * 60;
export const CSRF_RENEWAL_WINDOW_SECONDS = 60;
export const CSRF_REJECTED_CODE = "CSRF_REJECTED";

export type CsrfTokenResponse = {
  token: string;
  expiresInSeconds: number;
};
