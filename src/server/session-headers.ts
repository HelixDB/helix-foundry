// Puts the session's response headers (a rotated session cookie, Cache-Control,
// Vary) on a route's response. A cookie the route itself set wins over
// AuthKit's cookie of the same name, e.g. sign-out expiring a just-rotated
// session, whatever the header order.

const cookieName = (cookie: string) => {
  const i = cookie.indexOf("=");
  return i > 0 ? cookie.slice(0, i).trim() : null;
};

export function withSessionHeaders(
  response: Response,
  session: Headers,
): Response {
  const headers = new Headers(response.headers),
    own = headers.getSetCookie(),
    ownNames = new Set(own.map(cookieName));
  headers.delete("Set-Cookie");
  for (const c of session.getSetCookie())
    if (!ownNames.has(cookieName(c))) headers.append("Set-Cookie", c);
  for (const c of own) headers.append("Set-Cookie", c);
  const cache = session.get("Cache-Control");
  if (cache) headers.set("Cache-Control", cache);
  // Merged without repeats, keeping the first spelling of each name.
  const vary = new Map<string, string>();
  for (const v of [headers.get("Vary"), session.get("Vary")]
    .flatMap((x) => (x ? x.split(",") : []))
    .map((x) => x.trim())
    .filter(Boolean))
    if (!vary.has(v.toLowerCase())) vary.set(v.toLowerCase(), v);
  if (vary.size) headers.set("Vary", [...vary.values()].join(", "));
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}
