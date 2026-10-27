import type { NextResponse } from "next/server";
import type { AuthConfig } from "./config";

// Foundry's own cookie names, never the dashboard's: __Host-foundry-session
// (WORKOS_COOKIE_NAME, sealed by AuthKit), __Host-foundry-csrf and one
// __Host-foundry-login-<flow> per sign-in in progress. All are host-only
// (no Domain), HttpOnly and SameSite=Lax. On loopback http (dev:mock, e2e)
// browsers refuse the __Host- prefix, so the prefix and Secure are dropped.

type CookiePolicy = Pick<AuthConfig, "secure">;
const prefix = ({ secure }: CookiePolicy) => (secure ? "__Host-" : "");

export const csrfCookieName = (c: CookiePolicy) => `${prefix(c)}foundry-csrf`;
export const loginCookiePrefix = (c: CookiePolicy) =>
  `${prefix(c)}foundry-login-`;

export const cookieOptions = ({ secure }: CookiePolicy) =>
  ({ httpOnly: true, secure, sameSite: "lax", path: "/" }) as const;

/** Sets `name` to expire now. */
export function expireCookie(
  response: NextResponse,
  name: string,
  config: CookiePolicy,
) {
  response.cookies.set({
    name,
    value: "",
    ...cookieOptions(config),
    expires: new Date(0),
    maxAge: 0,
  });
}
