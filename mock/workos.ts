import type { IncomingMessage, ServerResponse } from "node:http";
import { id, now } from "./http";
import { db, SEED_EMAIL, stats, type MockUser } from "./state";
import { jwks, randomToken, refresh } from "./tokens";

// The few WorkOS endpoints AuthKit calls from the BFF (JWKS, refresh-token
// grant, get user, logout), and a stand-in for the hosted sign-in page.

const clientId = () => process.env.WORKOS_CLIENT_ID || "";
const apiKey = () => process.env.WORKOS_API_KEY || "";

function send(
  res: ServerResponse,
  status: number,
  body: unknown,
  type = "application/json",
) {
  res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
  res.end(typeof body === "string" ? body : JSON.stringify(body));
}

const workosUser = (u: MockUser) => ({
  object: "user",
  id: u.workosId,
  email: u.email,
  email_verified: true,
  first_name: u.firstName,
  last_name: u.lastName,
  profile_picture_url: null,
  last_sign_in_at: now(),
  created_at: u.createdAt,
  updated_at: u.createdAt,
  external_id: null,
  metadata: {},
});

const escape = (s: string) =>
  s.replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ]!,
  );

function authorizePage(state: string, provider: string) {
  const label = provider.replace("AUTH_PROVIDER_", "").toLowerCase();
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Mock sign-in</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#f8f7f3;color:#292a26;font:14px Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
form{display:grid;gap:12px;width:340px}h1{margin:0 0 4px;font:400 26px Georgia,serif}p{margin:0 0 8px;color:#65675e}
label{display:grid;gap:6px;color:#4f534a;font-size:13px}input{height:34px;padding:0 10px;border:1px solid #d4d2c7;border-radius:8px;background:#fcfbf8;font:inherit}
button{height:40px;border:0;border-radius:8px;background:#343932;color:#fbfaf6;font:500 14px inherit;cursor:pointer}</style></head>
<body><form method="post" action="/mock/authorize">
<h1>Mock ${escape(label)} sign-in</h1><p>The mock WFE stands in for WorkOS. Any email signs in; ${SEED_EMAIL} already has a workspace.</p>
<input type="hidden" name="state" value="${escape(state)}">
<label>Email<input name="email" type="email" required value="${SEED_EMAIL}"></label>
<label>Name<input name="name" placeholder="From the email if left blank"></label>
<button type="submit">Continue</button></form></body></html>`;
}

function userByEmail(email: string, name: string): MockUser {
  const existing = [...db.users.values()].find((u) => u.email === email);
  if (existing) return existing;
  // A blank name comes from the email: grace@example.com is Grace.
  const given =
    name.trim() || email.split("@")[0]!.replace(/^./, (c) => c.toUpperCase());
  const [firstName = "", ...rest] = given.split(/\s+/);
  const user: MockUser = {
    id: id("usr"),
    workosId: id("user"),
    email,
    firstName,
    lastName: rest.join(" "),
    createdAt: now(),
  };
  db.users.set(user.id, user);
  return user;
}

/** Handles a WorkOS or sign-in page request; false if it is not one. */
export async function handleWorkos(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  body: string,
): Promise<boolean> {
  const { pathname } = url;
  if (req.method === "GET" && pathname === `/sso/jwks/${clientId()}`) {
    send(res, 200, await jwks());
    return true;
  }
  if (req.method === "POST" && pathname === "/user_management/authenticate") {
    const grant = JSON.parse(body || "{}");
    if (grant.client_id !== clientId() || grant.client_secret !== apiKey()) {
      send(res, 401, { error: "invalid_client" });
      return true;
    }
    if (grant.grant_type !== "refresh_token") {
      send(res, 400, { error: "unsupported_grant_type" });
      return true;
    }
    stats.refreshes += 1;
    const next = await refresh(String(grant.refresh_token || ""));
    if (!next) {
      send(res, 400, {
        error: "invalid_grant",
        error_description: "Refresh token is invalid or already used.",
      });
      return true;
    }
    send(res, 200, {
      user: workosUser(next.user),
      access_token: next.accessToken,
      refresh_token: next.refreshToken,
      authentication_method: "OAuth",
    });
    return true;
  }
  const user = /^\/user_management\/users\/([^/]+)$/.exec(pathname);
  if (req.method === "GET" && user) {
    if (req.headers.authorization !== `Bearer ${apiKey()}`) {
      send(res, 401, { message: "Unauthorized" });
      return true;
    }
    const found = [...db.users.values()].find((u) => u.workosId === user[1]);
    send(
      res,
      found ? 200 : 404,
      found ? workosUser(found) : { message: "Not found" },
    );
    return true;
  }
  if (req.method === "GET" && pathname === "/user_management/sessions/logout") {
    const session = db.sessions.get(url.searchParams.get("session_id") || "");
    if (session) db.sessions.delete(session.id);
    const back = url.searchParams.get("return_to") || "/";
    res.writeHead(302, { Location: back, "Cache-Control": "no-store" });
    res.end();
    return true;
  }
  if (pathname === "/mock/authorize") {
    if (req.method === "GET") {
      const state = url.searchParams.get("state") || "";
      if (!db.logins.has(state))
        send(res, 400, "Unknown sign-in.", "text/plain");
      else
        send(
          res,
          200,
          authorizePage(state, url.searchParams.get("provider") || ""),
          "text/html; charset=utf-8",
        );
      return true;
    }
    const form = new URLSearchParams(body);
    const login = db.logins.get(form.get("state") || "");
    const email = (form.get("email") || "").trim().toLowerCase();
    if (!login || !email) {
      send(res, 400, "Unknown sign-in.", "text/plain");
      return true;
    }
    login.userId = userByEmail(email, form.get("name") || "").id;
    login.code = randomToken();
    const back = new URL(login.redirectUri);
    back.searchParams.set("code", login.code);
    back.searchParams.set("state", login.state);
    res.writeHead(303, { Location: back.toString() });
    res.end();
    return true;
  }
  return false;
}
