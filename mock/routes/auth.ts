import type {
  ExchangeAuthCodeRequest,
  ExchangeAuthCodeResponse,
  StartLoginRequest,
  StartLoginResponse,
  WhoamiResponse,
} from "../../src/shared/wfe";
import { FOUNDRY_APPLICATION } from "../../src/shared/wfe";
import { fail, now, ok, type MockRoute } from "../http";
import { config, db, displayName, stats } from "../state";
import { randomToken, startSession } from "../tokens";

// Sign-in as the WFE brokers it: login:start returns an authorize URL (the
// mock's own sign-in page, mock/workos.ts) and exchange trades the code for
// WorkOS tokens. Foundry must sign in as the Foundry application.

const providers = new Set([
  "AUTH_PROVIDER_GITHUB",
  "AUTH_PROVIDER_GOOGLE",
  "AUTH_PROVIDER_MICROSOFT",
]);

export const authRoutes: MockRoute[] = [
  {
    method: "POST",
    path: "/v1/auth/login:start",
    public: true,
    handle: ({ body }) => {
      const req = (body || {}) as Partial<StartLoginRequest>;
      if (req.application !== FOUNDRY_APPLICATION)
        return fail(
          "INVALID_ARGUMENT",
          "application must be AUTH_APPLICATION_FOUNDRY",
        );
      if (!req.provider || !providers.has(req.provider))
        return fail("INVALID_ARGUMENT", "unsupported provider");
      if (!req.redirectUri) return fail("INVALID_ARGUMENT", "redirect_uri");
      stats.loginStarts += 1;
      const state = randomToken() + randomToken(),
        sessionId = randomToken() + randomToken();
      db.logins.set(state, {
        sessionId,
        state,
        redirectUri: req.redirectUri,
        provider: req.provider,
      });
      const url = new URL("/mock/authorize", process.env.MOCK_ORIGIN);
      url.searchParams.set("state", state);
      url.searchParams.set("provider", req.provider);
      return ok({
        url: url.toString(),
        sessionId,
      } satisfies StartLoginResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/auth/exchange",
    public: true,
    handle: async ({ body }) => {
      const req = (body || {}) as Partial<ExchangeAuthCodeRequest>;
      const login = req.state ? db.logins.get(req.state) : undefined;
      if (
        !login ||
        !login.code ||
        login.code !== req.code ||
        login.sessionId !== req.sessionId
      )
        return fail("INVALID_ARGUMENT", "invalid or expired authorization");
      db.logins.delete(login.state);
      const user = db.users.get(login.userId!)!;
      const email = user.email.toLowerCase(),
        rules = config.allowedEmails.map((r) => r.toLowerCase());
      if (
        rules.length &&
        !rules.includes(email) &&
        !rules.includes(email.slice(email.lastIndexOf("@")))
      )
        return fail(
          "PERMISSION_DENIED",
          "email not permitted in this environment",
        );
      const tokens = await startSession(user);
      return ok({
        accessToken: tokens.accessToken,
        refreshToken: tokens.refreshToken,
        email: user.email,
      } satisfies ExchangeAuthCodeResponse);
    },
  },
  {
    method: "GET",
    path: "/v1/whoami",
    handle: ({ principal: { user } }) =>
      ok({
        user: {
          id: user.id,
          email: user.email,
          displayName: displayName(user),
          timestamps: { createdAt: user.createdAt, updatedAt: user.createdAt },
        },
        workspaces: db.memberships
          .filter((m) => m.userId === user.id)
          .map((m) => ({
            id: m.id,
            workspaceId: m.workspaceId,
            role: m.role,
            timestamps: { createdAt: m.createdAt, updatedAt: now() },
          })),
      } satisfies WhoamiResponse),
  },
];
