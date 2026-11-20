// Where to land after signing in: a path on this site, never another origin,
// and never the sign-in machinery itself (that would loop).

const ORIGIN = "https://foundry.invalid";
const MAX_LENGTH = 2048;
const NEVER = /^\/(?:login|auth|papi)(?:[/?#]|$)/;

/**
 * The normalized return path, or null when `value` is unsafe.
 *
 * @example
 * parseReturnPath("/data?tab=tables"); // "/data?tab=tables"
 * parseReturnPath("//evil.example"); // null
 */
export function parseReturnPath(value: string | null | undefined) {
  if (
    !value ||
    value.length > MAX_LENGTH ||
    !value.startsWith("/") ||
    value.startsWith("//") ||
    value.includes("\\")
  )
    return null;
  let url: URL;
  try {
    url = new URL(value, ORIGIN);
  } catch {
    return null;
  }
  const path = url.pathname + url.search + url.hash;
  if (url.origin !== ORIGIN || path !== value || NEVER.test(path)) return null;
  return path;
}

/** The sign-in page, returning to `path` afterwards when it is safe. */
export function signInPath(path?: string | null) {
  const target = parseReturnPath(path);
  return target && target !== "/"
    ? `/login?return_to=${encodeURIComponent(target)}`
    : "/login";
}
