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
export function redact(message: string) {
  return message
    .replace(
      /\b(postgres(?:ql)?|mysql|neo4j(?:\+s)?|bolt(?:\+s)?):\/\/[^\s]+/gi,
      "$1://[redacted]",
    )
    .replace(
      /(password|api[_-]?key|authorization|token)\s*[:=]\s*[^\s,;]+/gi,
      "$1=[redacted]",
    )
    .slice(0, 1000);
}
