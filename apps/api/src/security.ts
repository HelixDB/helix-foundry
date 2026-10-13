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
