import { expect, it } from "vitest";
import { modelError } from "../apps/api/src/security.js";

// DuckDB 1.5's own messages for failures on these row values, from queries
// against a users table: id 1, email alice@example.com, ssn 123-45-6789,
// password hunter2, phone 5551234567, birth 1990-13-01, doc {"a":1 and
// address Flat 3, 1 Main Street.
const secrets = [
  "alice",
  "example",
  "123",
  "6789",
  "hunter2",
  "5551234567",
  "1990",
  "Flat",
  "Main Street",
];
const cases: [sql: string, error: string, expected: string][] = [
  [
    "SELECT count(*) FROM d_users WHERE ssn = 123456789",
    "Conversion Error: Could not convert string '123-45-6789' to INT32 when casting from source column ssn\n\nLINE 1: SELECT count(*) FROM d_users WHERE ssn = 123456789\n                                           ^",
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column ssn",
  ],
  [
    "SELECT CAST(password AS INTEGER) FROM d_users",
    "Conversion Error: Could not convert string 'hunter2' to INT32 when casting from source column password\n\nLINE 1: SELECT CAST(password AS INTEGER) FROM d_users\n               ^",
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column password",
  ],
  [
    "SELECT address::INTEGER FROM d_users",
    "Conversion Error: Could not convert string 'Flat 3, 1 Main Street' to INT32 when casting from source column address\n\nLINE 1: SELECT address::INTEGER FROM d_users\n                      ^",
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column address",
  ],
  [
    "SELECT phone::INTEGER FROM d_users",
    "Conversion Error: Type INT64 with value 5551234567 can't be cast because the value is out of range for the destination type INT32 when casting from source column phone\n\nLINE 1: SELECT phone::INTEGER FROM d_users\n                    ^",
    "Conversion Error: Type INT64 with value [value] can't be cast because the value is out of range for the destination type INT32 when casting from source column phone",
  ],
  [
    "SELECT birth::DATE FROM d_users",
    'Conversion Error: date field value out of range: "1990-13-01" when casting from source column birth\n\nLINE 1: SELECT birth::DATE FROM d_users\n                    ^',
    "Conversion Error: date field value out of range: [value] when casting from source column birth",
  ],
  [
    "SELECT email::DECIMAL(10,2) FROM d_users",
    'Conversion Error: Could not convert string "alice@example.com" to DECIMAL(10,2) when casting from source column email\n\nLINE 1: SELECT email::DECIMAL(10,2) FROM d_users\n                    ^',
    "Conversion Error: Could not convert string [value] to DECIMAL(10,2) when casting from source column email",
  ],
  [
    "SELECT email::TIMESTAMP FROM d_users",
    'Conversion Error: invalid timestamp field format: "alice@example.com", expected format is (YYYY-MM-DD HH:MM:SS[.US][±HH[:MM[:SS]]| ZONE]) when casting from source column email',
    "Conversion Error: invalid timestamp field format: [value], expected format is (YYYY-MM-DD HH:MM:SS[.US][±HH[:MM[:SS]]| ZONE]) when casting from source column email",
  ],
  [
    "SELECT email::JSON FROM d_users",
    'Conversion Error: Malformed JSON at byte 0 of input: unexpected character.  Input: "alice@example.com" when casting from source column email',
    "Conversion Error: Malformed JSON at byte [value] of input: unexpected character.  Input: [value] when casting from source column email",
  ],
  [
    "SELECT json_extract(doc, '$.a') FROM d_users",
    'Invalid Input Error: Malformed JSON at byte 6 of input: unexpected end of data.  Input: "{"a":1"',
    "Invalid Input Error: Malformed JSON at byte [value] of input: unexpected end of data.  Input: [value]",
  ],
  [
    "SELECT strptime(birth, '%Y-%m-%d') FROM d_users",
    'Invalid Input Error: Could not parse string "1990-13-01" according to format specifier "%Y-%m-%d"\n1990-13-01\n     ^\nError: Month out of range, expected a value between 1 and 12',
    'Invalid Input Error: Could not parse string [value] according to format specifier "%Y-%m-%d"\n[value]\n     ^\nError: Month out of range, expected a value between [value] and [value]',
  ],
  [
    "SELECT phone * 9999999999999 FROM d_users",
    "Out of Range Error: Overflow in multiplication of INT64 (5551234567 * 9999999999999)!",
    "Out of Range Error: Overflow in multiplication of INT64 [value] * 9999999999999)!",
  ],
  [
    "SELECT make_date(phone, 1, 1) FROM d_users",
    "Invalid Input Error: Type INT64 with value 5551234567 can't be cast because the value is out of range for the destination type INT32",
    "Invalid Input Error: Type INT64 with value [value] can't be cast because the value is out of range for the destination type INT32",
  ],
  // error() raises whatever text it is given, unquoted.
  [
    "SELECT error(email) FROM d_users",
    "Invalid Input Error: alice@example.com",
    "Invalid Input Error: [value]",
  ],
  [
    "SELECT error(address) FROM d_users",
    "Invalid Input Error: Flat 3, 1 Main Street",
    "Invalid Input Error: [value]",
  ],
];

it("keeps the values an engine error quotes from a model that may not see rows", () => {
  for (const [sql, error, expected] of cases) {
    const safe = modelError(error, false, sql);
    expect(safe, error).toBe(expected);
    for (const secret of secrets)
      if (!sql.includes(secret)) expect(safe, error).not.toContain(secret);
  }
});

it("passes errors raised before any row is read, and the app's own text", () => {
  for (const error of [
    'Binder Error: Referenced column "emial" not found in FROM clause!\nCandidate bindings: "email"\n\nLINE 1: SELECT emial FROM d_users\n               ^',
    "Binder Error: No function matches the given name and argument types 'lower(INTEGER)'. You might need to add explicit type casts.\n\tCandidate functions:\n\tlower(VARCHAR) -> VARCHAR\n",
    'Catalog Error: Table with name d_userz does not exist!\nDid you mean "d_users"?',
    'Parser Error: syntax error at or near "WHERE"',
    "The query doesn't use any of your datasets.",
    "Quality check failed: every order has a customer",
    "Stopped at the executor's time limit (2 minutes, QUERY_TIMEOUT)",
  ])
    expect(modelError(error, false, "")).toBe(error);
});

it("leaves engine errors whole when the model may see rows, and always redacts credentials", () => {
  const error =
    "Conversion Error: Could not convert string 'alice@example.com' to INT32 when casting from source column email";
  expect(modelError(error, true, "")).toBe(error);
  const header =
    "Conversion Error: Could not convert string 'Bearer sk-proj-0123456789abcdefghij' to INT32";
  expect(modelError(header, true)).toBe(
    "Conversion Error: Could not convert string 'Bearer [redacted]' to INT32",
  );
  expect(modelError(header, false)).toBe(
    "Conversion Error: Could not convert string [value] to INT32",
  );
});

it("keeps only what the model wrote itself, never part of a value", () => {
  // The model guessed part of the value: the error still says nothing more.
  const error =
    "Conversion Error: Could not convert string '123-45-6789' to INT32 when casting from source column ssn";
  expect(
    modelError(
      error,
      false,
      "SELECT * FROM d WHERE ssn = '123-45' AND ssn::INT > 0",
    ),
  ).toBe(
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column ssn",
  );
  // Without the query, the column name goes too.
  expect(modelError(error, false)).toBe(
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column [value]",
  );
  // An app prefix before the engine's error is kept.
  expect(
    modelError("Pipeline orders failed. Invalid Input Error: hunter2", false),
  ).toBe("Pipeline orders failed. Invalid Input Error: [value]");
});

it("takes linear time on long errors", () => {
  const value = "x'".repeat(40_000) + '"'.repeat(40_000) + "a ".repeat(40_000);
  const start = performance.now();
  modelError(`Conversion Error: Could not convert string '${value}'`, false);
  expect(performance.now() - start).toBeLessThan(1000);
});
