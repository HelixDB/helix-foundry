import { it, expect } from "vitest";
import { redact } from "../apps/api/src/security.js";

// Each message carries a credential in a form the app sends or receives, and
// the part of it that must not survive.
const credentials: [string, string][] = [
  // The credential after an Authorization scheme, or after any Bearer.
  ["Authorization: Bearer sk-proj-AbCdEf0123456789xyz", "AbCdEf0123456789"],
  ["authorization: Basic bmVvNGo6TmVvNGpQd180Mg==", "bmVvNGo6TmVvNGpQd180Mg"],
  ["{ Authorization: 'Bearer rk_live_51HxStripeKey9' }", "51HxStripeKey9"],
  ['{"authorization":"Bearer custom-provider-key-778899"}', "778899"],
  ["could not convert string 'Bearer whk_0123456789'", "whk_0123456789"],
  // fetch names the whole URL, with its user and password.
  [
    "Request cannot be constructed from a URL that includes credentials: http://user:s3cret@ollama:11434/api/tags",
    "s3cret",
  ],
  // Named secrets: JSON keys, quoted values and longer names.
  ['{"user":"reader","password":"MyPw_77aa"}', "MyPw_77aa"],
  ['{"password":"ab\\"Zq9SecretTail"}', "Zq9SecretTail"],
  ['{\\"apiKey\\":\\"key-from-escaped-json\\"}', "key-from-escaped-json"],
  ['password: "correct horse battery"', "horse battery"],
  ["PGPASSWORD=ab&Zq9SecretTail", "Zq9SecretTail"],
  ['{"serviceToken":"svc-tok-0123"}', "svc-tok-0123"],
  ["grant_type=refresh_token&client_secret=cs_0123&code=x", "cs_0123"],
  ["secretAccessKey: 'wJalrXUtnFEMI/K7MDENG'", "wJalrXUtnFEMI"],
  ["x-internal-key: 0123456789abcdef", "0123456789abcdef"],
  ["INTERNAL_KEY=0123456789abcdef", "0123456789abcdef"],
  // Keys on their own, as in fetch's error for a key with a line break.
  [
    'Headers.append: "sk-ant-api03-0123456789AbCdEf\nGhIj" is an invalid header value.',
    "0123456789AbCdEf",
  ],
  [
    "key phx_Ab3dEf6hIj9lMn2pQr5tUv8xYz1BcDeFgHiJkLm was rejected",
    "Ab3dEf6hIj9lMn2pQr5tUv8xYz1",
  ],
];

it("removes credentials from error text in the forms the app uses", () => {
  for (const [message, secret] of credentials)
    expect(redact(message), message).not.toContain(secret);
});

it("keeps the rest of the message", () => {
  expect(redact("connect ECONNREFUSED http://u:pw@ollama:11434/api/tags")).toBe(
    "connect ECONNREFUSED http://[redacted]@ollama:11434/api/tags",
  );
  expect(redact("Authorization: Bearer abc123, retrying")).toBe(
    "Authorization: [redacted], retrying",
  );
  expect(redact('{"password":"x","user":"reader"}')).toBe(
    '{"password":[redacted],"user":"reader"}',
  );
  // The Analyst repairs a query from DuckDB's error, so names that only
  // look like secrets, and casts, stay.
  const engine =
    'Binder Error: Referenced column "sbp_mm" not found in FROM clause!\nCandidate bindings: "sbp_mmhg", "phx_route", "napi_version", "sk_test_score", "sk_customer_key", "tokens", "password_hash"\n\nLINE 1: SELECT sbp_mm, "token"::INTEGER FROM d_1';
  expect(redact(engine)).toBe(engine);
});

it("takes linear time on long text", () => {
  // A long value quoted by a conversion error.
  const message = `Could not convert string '${"ab-".repeat(50000)}' to INT32`;
  const start = performance.now();
  expect(redact(message)).toBe(message);
  expect(performance.now() - start).toBeLessThan(500);
});
