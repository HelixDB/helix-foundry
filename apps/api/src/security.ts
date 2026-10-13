import {
  createHash,
  randomBytes,
  createCipheriv,
  createDecipheriv,
} from "node:crypto";
import { config } from "./config.js";
export const digest = (v: string) =>
  createHash("sha256").update(v).digest("hex");
export const token = () => randomBytes(32).toString("base64url");
export function seal(data: unknown) {
  const iv = randomBytes(12),
    cipher = createCipheriv(
      "aes-256-gcm",
      createHash("sha256").update(config.encryptionKey).digest(),
      iv,
    );
  const encrypted = Buffer.concat([
    cipher.update(JSON.stringify(data), "utf8"),
    cipher.final(),
  ]);
  return [
    iv.toString("base64"),
    cipher.getAuthTag().toString("base64"),
    encrypted.toString("base64"),
  ].join(".");
}
export function unseal<T = Record<string, any>>(value: string): T {
  const [iv, tag, body] = value.split(".").map((x) => Buffer.from(x, "base64"));
  const cipher = createDecipheriv(
    "aes-256-gcm",
    createHash("sha256").update(config.encryptionKey).digest(),
    iv,
  );
  cipher.setAuthTag(tag);
  return JSON.parse(
    Buffer.concat([cipher.update(body), cipher.final()]).toString(),
  );
}
export class AppError extends Error {
  constructor(
    public statusCode: number,
    message: string,
  ) {
    super(message);
  }
}
export function assert(
  condition: unknown,
  message: string,
  status = 400,
): asserts condition {
  if (!condition) throw new AppError(status, message);
}
// Error text can reach the AI provider, so credentials in it are replaced.
// A database URL goes whole; any other URL keeps its host but not its user
// and password. An Authorization value goes up to the end of the line or a
// comma, semicolon or brace, since Basic and PlanetScale's ID:TOKEN have no
// fixed shape, and so does the word after any Bearer. A named secret's value
// goes, quoted or not, also under a JSON key; a :: cast is not a separator.
// The keys of the services Foundry connects to go by their prefix only when
// a long token follows, so DuckDB names such as sk_customer_key or sbp_mmhg
// stay and a failed query can still be repaired. Callers cut the result to
// length afterwards, so a match is never split.
export function redact(message: string) {
  return message
    .replace(
      /\b(postgres(?:ql)?|mysql|neo4j(?:\+s)?|bolt(?:\+s)?):\/\/[^\s]+/gi,
      "$1://[redacted]",
    )
    .replace(
      /(?<![a-z0-9+.-])([a-z][a-z0-9+.-]*:\/\/)[^\s/?#]*@/gi,
      "$1[redacted]@",
    )
    .replace(
      /(authorization\\?["']?\s*(?:=|:(?!:))\s*)(?:"(?:[^"\\]|\\.)*"|\\"[^"]*"|'[^']*'|[^\r\n,;}]+)/gi,
      "$1[redacted]",
    )
    .replace(
      /((?:password|passwd|secret|token|(?:api|access|secret|private|internal|encryption)[_-]?key)\\?["']?\s*(?:=|:(?!:))\s*)(?:"(?:[^"\\]|\\.)*"|\\"[^"]*"|'[^']*'|[^\s,;]+)/gi,
      "$1[redacted]",
    )
    .replace(/\b(bearer)\s+[^\s,;"']+/gi, "$1 [redacted]")
    .replace(
      /\b(?:[sr]k_(?:live|test)|phx|napi|sbp|pscale_(?:tkn|pw|oauth))_[a-zA-Z0-9]{20}[\w-]*|\bsk-[\w-]{20,}/g,
      "[redacted]",
    );
}
// DuckDB's own wording in errors raised while reading rows, and its type
// names. With the words a model wrote itself, these are all an error keeps
// when the model may not see row values.
const engineWords = new Set(
  `a according after allowed an and any are argument arguments array at be
  because before between byte by characters decoding digits element encoding
  exceeded found maximum minimum offset part position separator sequence
  timezone unit units valid
  can cannot cast casting character characters column columns contain convert
  could count data date day destination division does doesn error escape exceeds
  expected failed field fields for format from function got greater hour in
  index input integer invalid is json key keys larger length less limit list
  malformed map memory minute missing month multiplication must negative no not
  null number numeric of only or out overflow parse pattern positive precision
  range regular result row rows scale second seconds single size source
  specifier string struct subtraction addition supported t than the time to too
  type types unexpected unimplemented unknown use value values when with year
  zero zone epoch end of
  bigint bit blob bool boolean date decimal double enum float float4 float8
  hugeint int int1 int2 int4 int8 int16 int32 int64 int128 integer interval
  real smallint text time timestamp timestamptz timetz tinyint ubigint uhugeint
  uint8 uint16 uint32 uint64 uint128 uinteger union usmallint utinyint uuid
  varchar varint bignum yyyy mm dd hh ss us`.split(/\s+/),
);
const wordsOf = (text: string) =>
  text.toLowerCase().match(/[\p{L}\p{N}_]+/gu) || [];
// Errors from the query engine quote the values they fail on: "Could not
// convert string '123-45-6789' to INT32 when casting from source column ssn".
// When a hosted model may not see row values (`raw` false), such an error keeps
// only its kind, DuckDB's wording and the words in `known` (what the model
// wrote, such as its SQL), and the rest becomes [value]. Parser, binder and
// catalog errors are raised before any row is read and name only tables,
// columns and functions, so they pass unchanged, as does the app's own text.
export function modelError(message: string, raw: boolean, known = "") {
  const text = redact(message);
  const engine = /\b([A-Z][A-Za-z]*(?: [A-Za-z]+){0,3}) Error: /.exec(text);
  if (raw || !engine || /^(Parser|Binder|Catalog)$/.test(engine[1]))
    return text;
  const allowed = new Set([...engineWords, ...wordsOf(known)]);
  const clear = (s: string) => wordsOf(s).every((w) => allowed.has(w));
  const start = engine.index + engine[0].length;
  const scrubbed = text
    .slice(start)
    // The query's own line, which DuckDB echoes; the model has the query.
    .replace(/\n+LINE \d+:[\s\S]*$/, "")
    .replace(
      /(?<![\p{L}\p{N}_])('(?:[^']|'')*'|"(?:[^"]|"")*")(?![\p{L}\p{N}_])/gu,
      (quoted) => (clear(quoted) ? quoted : "\0"),
    )
    .replace(/[^\s\0]+/g, (chunk) => (clear(chunk) ? chunk : "\0"))
    .replace(/\0(?:[^\S\n]*\0)*/g, "[value]");
  return text.slice(0, start) + scrubbed;
}
