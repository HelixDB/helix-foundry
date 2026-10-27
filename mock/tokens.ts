import { randomBytes } from "node:crypto";
import {
  exportJWK,
  generateKeyPair,
  jwtVerify,
  SignJWT,
  type JWK,
  type KeyLike,
} from "jose";
import { config, db, type MockSession, type MockUser } from "./state";

// Access tokens shaped like WorkOS AuthKit's (RS256, sub, sid, client_id,
// exp), signed by a key made when the mock starts. Refresh tokens are single
// use: each refresh spends one and issues the next, as WorkOS does.

const KID = "mock-key-1";
let keys: { privateKey: KeyLike; publicKey: KeyLike; jwk: JWK } | undefined;

async function keyPair() {
  if (!keys) {
    const { privateKey, publicKey } = await generateKeyPair("RS256");
    keys = {
      privateKey,
      publicKey,
      jwk: {
        ...(await exportJWK(publicKey)),
        kid: KID,
        alg: "RS256",
        use: "sig",
      },
    };
  }
  return keys;
}

export async function jwks() {
  return { keys: [(await keyPair()).jwk] };
}

export const randomToken = () => randomBytes(24).toString("base64url");

/**
 * A token that lives at least `accessTokenTtlSeconds`. `exp` is in whole
 * seconds and a token is out once the clock's whole second reaches it, so it
 * is rounded up: rounded down, a one-second token minted late in a second
 * would die at the next one, possibly before the WFE read it.
 */
export async function accessToken(user: MockUser, session: MockSession) {
  const { privateKey } = await keyPair();
  return new SignJWT({
    sid: session.id,
    client_id: process.env.WORKOS_CLIENT_ID || "client_mock_foundry",
  })
    .setProtectedHeader({ alg: "RS256", kid: KID })
    .setIssuer(process.env.MOCK_ORIGIN || "http://127.0.0.1")
    .setSubject(user.workosId)
    .setIssuedAt()
    .setExpirationTime(
      Math.ceil(Date.now() / 1000) + config.accessTokenTtlSeconds,
    )
    .sign(privateKey);
}

/** The session and user behind a bearer token, or null if it is not valid. */
export async function verify(token: string) {
  try {
    const { payload } = await jwtVerify(token, (await keyPair()).publicKey);
    const session = db.sessions.get(String(payload.sid));
    const user = session && db.users.get(session.userId);
    return session && user && user.workosId === payload.sub
      ? { session, user }
      : null;
  } catch {
    return null;
  }
}

/** Starts a WorkOS session for a user and returns its first tokens. */
export async function startSession(user: MockUser) {
  const session: MockSession = {
    id: "session_" + randomToken(),
    userId: user.id,
    refreshToken: randomToken(),
  };
  db.sessions.set(session.id, session);
  return {
    session,
    accessToken: await accessToken(user, session),
    refreshToken: session.refreshToken,
  };
}

/** Spends a refresh token; null when it is unknown or already spent. */
export async function refresh(refreshToken: string) {
  const session = [...db.sessions.values()].find(
    (s) => s.refreshToken === refreshToken,
  );
  const user = session && db.users.get(session.userId);
  if (!session || !user) return null;
  session.refreshToken = randomToken();
  return {
    user,
    accessToken: await accessToken(user, session),
    refreshToken: session.refreshToken,
  };
}
