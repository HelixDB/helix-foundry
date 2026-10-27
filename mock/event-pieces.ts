// The rules and encodings of API sources' pieces (connector kind events), as
// the backend's events package holds them, so the mock refuses
// and stores what the backend does, with its messages. Pure: no state and no
// clock. tests/unit/event-sources-mock.test.ts runs the backend's rule table
// (the events package's testdata/pieces.json, copied to
// tests/unit/__fixtures__/event-pieces.json) through it.
//
// A send goes through it as the backend's does:
//  1. parseOccurredAt reads each piece's time as a front end receives it;
//  2. validate checks every rule that needs no state and returns a Send;
//  3. mergeSchema adds the send's fields to the source's, refusing a type
//     conflict or a field past the cap;
//  4. project refuses a send that would take the source's table past a cap.
// A sync turns each stored piece into its table row with rowOf. A source
// with a text field (withText locks it as a string) also keeps its newest
// rows (newest) as Markdown documents, one per piece (render).
//
// Strings here are JavaScript's (UTF-16); every size is counted in UTF-8
// bytes, as the backend counts it.

/** The JSON type a field holds; "null" until its first non-null value locks one. */
export type FieldType =
  | "null"
  | "string"
  | "number"
  | "boolean"
  | "object"
  | "array";
/** One top-level field of a source's pieces. */
export type Field = { name: string; type: FieldType };
/**
 * The fields a source's pieces have carried, each with its type. Fields only
 * grow, and a locked type never changes. A Map, because a field may be named
 * anything a JSON key may, "__proto__" included.
 */
export type Schema = ReadonlyMap<string, FieldType>;

/** What one send may hold (foundry.events), and the fields a source may have. */
export type Limits = {
  maxPieces: number;
  maxBytes: number;
  maxPieceBytes: number;
  maxFields: number;
};
/** foundry.events' defaults: the backend's on every plan. */
export const LIMITS: Limits = {
  maxPieces: 500,
  maxBytes: 1 << 20,
  maxPieceBytes: 256 << 10,
  maxFields: 200,
};

/**
 * A send the rules refuse, with the backend's message: `invalid` is
 * INVALID_ARGUMENT, `fields` and `full` FAILED_PRECONDITION with reason
 * FOUNDRY_EVENT_SOURCE_FIELDS and FOUNDRY_EVENT_SOURCE_FULL.
 */
export type Refusal = {
  refused: "invalid" | "fields" | "full";
  message: string;
};
export const isRefusal = (x: unknown): x is Refusal =>
  typeof x === "object" && x !== null && "refused" in x;

/**
 * A time as a piece or a send stores it: UTC to the microsecond, always with
 * six fractional digits ("2026-10-23T18:20:00.500000Z"), which is how a
 * batch line writes received_at and sorts as text.
 */
export type Instant = string;

/** One piece as its sender gave it, its time already read (parseOccurredAt). */
export type Piece = { id: string; occurredAt?: Instant; data: Uint8Array };

/** A top-level member of a piece's data: its key, type and value as sent. */
export type Member = Field & { value: string };
/** A piece every stateless rule accepts. */
export type SentPiece = {
  id: string;
  occurredAt?: Instant;
  // The data after JSON compaction: its literals and escapes as sent.
  data: string;
  members: readonly Member[];
};
/**
 * A batch every stateless rule accepts: only validate makes one, so it holds
 * 1 to maxPieces pieces. `bytes` is its batch object's size uncompressed.
 */
export type Send = { pieces: readonly SentPiece[]; bytes: number };

const MAX_ID_BYTES = 128,
  MAX_KEY_BYTES = 64,
  // How deep data may nest, its own object counting as 1.
  MAX_DEPTH = 64,
  MIN_YEAR = 1000,
  MAX_YEAR = 9999;

// The row's own columns: data may not carry them.
const RESERVED = new Map([
  ["id", "send it as the piece's id"],
  ["received_at", "Foundry sets it when the piece arrives"],
  ["occurred_at", "send it as the piece's occurred_at"],
]);

const utf8 = new TextEncoder();
const bytesOf = (s: string) => utf8.encode(s).length;
/** UTF-8 byte order, which is code point order: how Go sorts names. */
export const byBytes = (a: string, b: string) => {
  const x = [...a],
    y = [...b];
  for (let i = 0; i < Math.min(x.length, y.length); i++)
    if (x[i] !== y[i]) return x[i]!.codePointAt(0)! - y[i]!.codePointAt(0)!;
  return x.length - y.length;
};
/** A count with commas between thousands, as the backend writes it. */
const grouped = (n: number) => n.toLocaleString("en-US");
/**
 * Go's %q (strconv.Quote) for the strings messages quote: ids, keys and
 * client tokens, none with a control character.
 */
const quote = (s: string) =>
  '"' +
  [...s]
    .map((c) =>
      c === '"' || c === "\\"
        ? "\\" + c
        : /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]$/u.test(c)
          ? c
          : c.codePointAt(0)! < 0x10000
            ? "\\u" + c.codePointAt(0)!.toString(16).padStart(4, "0")
            : "\\U" + c.codePointAt(0)!.toString(16).padStart(8, "0"),
    )
    .join("") +
  '"';
/** A string as Go's encoding/json writes it without HTML escaping. */
export const jsonString = (s: string) =>
  JSON.stringify(s).replace(/[\u2028\u2029]/g, (c) =>
    c === "\u2028" ? "\\u2028" : "\\u2029",
  );

const invalid = (message: string): Refusal => ({ refused: "invalid", message });
/** Names a piece by its place in the send, from 1, and by its id when it is one. */
const refusePiece = (index: number, id: string, message: string) =>
  invalid(
    validId(id)
      ? `piece ${index + 1} (id ${quote(id)}): ${message}`
      : `piece ${index + 1}: ${message}`,
  );

/** 1 to 128 bytes, each printable ASCII other than space. */
export const validId = (id: string) => /^[\x21-\x7e]{1,128}$/.test(id);
/** A send's client_token: 16 to 64 bytes, each printable ASCII other than space. */
export const validClientToken = (token: string) =>
  /^[\x21-\x7e]{16,64}$/.test(token);

// ---------------------------------------------------------------- Times

const RFC3339 =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{1,2}):(\d{2}):(\d{2})(?:[.,](\d+))?(?:Z|([+-])(\d{2}):(\d{2}))$/;

/**
 * Reads a piece's occurred_at, RFC 3339 text, for the piece at index (from
 * 0) with this id, as Go's time.Parse(time.RFC3339Nano) reads it (an hour
 * of one digit, a comma before the fraction and an offset up to 24:60
 * included), in UTC and truncated to the microsecond. A time outside the
 * years 1000 to 9999 in UTC is refused: a DATETIME holds no other.
 */
export function parseOccurredAt(
  index: number,
  id: string,
  text: string,
): Instant | Refusal {
  const garbled = refusePiece(index, id, "occurred_at is not an RFC 3339 time");
  const m = RFC3339.exec(text);
  if (!m) return garbled;
  const [year, month, day, hour, minute, second] = m
      .slice(1, 7)
      .map(Number) as [number, number, number, number, number, number],
    [oh, om] = [Number(m[9]), Number(m[10])];
  const days = new Date(0);
  days.setUTCFullYear(year, month, 0);
  if (
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > days.getUTCDate() ||
    hour > 23 ||
    minute > 59 ||
    second > 59 ||
    oh > 24 ||
    om > 60
  )
    return garbled;
  const at = new Date(0);
  at.setUTCFullYear(year, month - 1, day);
  at.setUTCHours(hour, minute, second, 0);
  const offset = m[8] ? (m[8] === "-" ? -1 : 1) * (oh * 60 + om) : 0;
  const utc = new Date(at.getTime() - offset * 60_000);
  if (utc.getUTCFullYear() < MIN_YEAR || utc.getUTCFullYear() > MAX_YEAR)
    return refusePiece(
      index,
      id,
      `occurred_at must be in the years ${MIN_YEAR} to ${MAX_YEAR} (UTC)`,
    );
  // The fraction's first nine digits are nanoseconds; six are microseconds.
  return (
    utc.toISOString().slice(0, 19) +
    "." +
    (m[7] || "").padEnd(6, "0").slice(0, 6) +
    "Z"
  );
}

/** A time as a send stores it: now, to the microsecond. */
export const instant = (at: Date): Instant =>
  at.toISOString().slice(0, 23) + "000Z";

/** An Instant as Go's time.RFC3339Nano writes it: no trailing zeros. */
export const rfc3339 = (at: Instant) => {
  const fraction = at.slice(20, 26).replace(/0+$/, "");
  return at.slice(0, 19) + (fraction ? "." + fraction : "") + "Z";
};

// ---------------------------------------------------------------- Data

// Drops JSON whitespace outside strings, as Go's json.Compact does, from
// text that is valid JSON: every literal and escape stays as sent.
function compact(text: string): string {
  let out = "",
    from = 0,
    inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      if (c === "\\") i++;
      else if (c === '"') inString = false;
    } else if (c === '"') inString = true;
    else if (c === " " || c === "\t" || c === "\n" || c === "\r") {
      out += text.slice(from, i);
      from = i + 1;
    }
  }
  return out + text.slice(from);
}

// The \uD800-\uDBFF escape not followed by a \uDC00-\uDFFF one, or the
// latter alone, in a string's text; undefined when every escape pairs. Go
// decodes either to U+FFFD, and the engine's serde_json refuses both.
function loneSurrogate(text: string): string | undefined {
  for (let i = 0; i < text.length; i++) {
    if (text[i] !== "\\") continue;
    if (text[i + 1] !== "u") {
      i++; // a one-character escape, such as \\ or \"
      continue;
    }
    const code = parseInt(text.slice(i + 2, i + 6), 16);
    let lone = code >= 0xdc00 && code <= 0xdfff;
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = text.slice(i + 6, i + 12),
        low =
          next.length === 6 && next.startsWith("\\u")
            ? parseInt(next.slice(2), 16)
            : 0;
      lone = low < 0xdc00 || low > 0xdfff;
      if (!lone) i += 6; // the pair's low half
    }
    if (lone) return text.slice(i, i + 6);
    i += 5;
  }
  return undefined;
}

// The rules of a top-level key, which becomes a column: 1 to 64 bytes, no
// control character, no backtick (prompts show field names in code spans),
// and not one of the row's own columns.
function keyProblem(key: string): string | undefined {
  const size = bytesOf(key);
  if (size < 1 || size > MAX_KEY_BYTES)
    return `data has a top-level key of ${size} bytes; a key is 1 to ${MAX_KEY_BYTES} bytes`;
  // Go's unicode.IsControl: C0, DEL and C1, which are Cc.
  if (/\p{Cc}/u.test(key))
    return "data has a top-level key with a control character";
  if (key.includes("`")) return "data has a top-level key with a backtick";
  const fix = RESERVED.get(key);
  return fix && `data may not have a top-level ${key}; ${fix}`;
}

// A number, true, false or null: up to the next separator.
const LITERAL = /[^,:}\]]*/y;

// Where the string starting at i ends, past its closing quote.
function stringEnd(data: string, i: number) {
  let j = i + 1;
  while (data[j] !== '"') j += data[j] === "\\" ? 2 : 1;
  return j + 1;
}

/**
 * Checks valid, compacted JSON object text in one token pass for what the
 * engine refuses and JSON.parse accepts (nesting past 64, a number out of
 * float64's range, a lone surrogate escape) and for the rules of top-level
 * keys, and returns the top-level members in the order written.
 */
function scan(data: string): Member[] | string {
  // One open object or array; `key` marks an object expecting a key.
  const stack: { object: boolean; key: boolean }[] = [];
  const members: Member[] = [],
    seen = new Set<string>();
  // The top-level key whose value comes next, and where that value starts.
  let name = "",
    start = 0;
  const ended = (end: number) => {
    const top = stack.at(-1);
    if (top?.object) top.key = true;
    if (stack.length === 1) members.at(-1)!.value = data.slice(start, end);
  };
  for (let i = 0; i < data.length; ) {
    const c = data[i]!;
    if (c === "," || c === ":") {
      i++;
      continue;
    }
    const top = stack.at(-1);
    if (c === "}" || c === "]") {
      stack.pop();
      ended(++i);
      continue;
    }
    if (c === '"' && top?.object && top.key) {
      const end = stringEnd(data, i),
        text = data.slice(i, end);
      i = end;
      const lone = loneSurrogate(text);
      if (lone) return `data has a ${lone} escape with no pair`;
      top.key = false;
      if (stack.length > 1) continue;
      const key = JSON.parse(text) as string;
      const problem = keyProblem(key);
      if (problem) return problem;
      if (seen.has(key))
        return `data has the top-level key ${quote(key)} twice`;
      seen.add(key);
      name = key;
      continue;
    }
    // A value starts.
    const end =
      c === '"'
        ? stringEnd(data, i)
        : c === "{" || c === "["
          ? i + 1
          : ((LITERAL.lastIndex = i), i + LITERAL.exec(data)![0].length);
    const type: FieldType =
      c === '"'
        ? "string"
        : c === "{"
          ? "object"
          : c === "["
            ? "array"
            : c === "t" || c === "f"
              ? "boolean"
              : c === "n"
                ? "null"
                : "number";
    if (stack.length === 1) {
      members.push({ name, type, value: "" });
      start = i;
    }
    const text = data.slice(i, end);
    i = end;
    if (type === "object" || type === "array") {
      if (stack.length === MAX_DEPTH)
        return `data nests deeper than ${MAX_DEPTH}`;
      stack.push({ object: type === "object", key: type === "object" });
      continue;
    }
    if (type === "string") {
      const lone = loneSurrogate(text);
      if (lone) return `data has a ${lone} escape with no pair`;
    }
    if (type === "number" && !Number.isFinite(Number(text)))
      return `data holds ${text.length > 24 ? text.slice(0, 24) + "…" : text}, which is not a finite number`;
    ended(end);
  }
  return members;
}

const strictUtf8 = new TextDecoder("utf-8", { fatal: true });

// A piece's data compacted with its top-level members, if it is one
// engine-safe JSON object within maxBytes; else what is wrong with it.
function checkData(
  raw: Uint8Array,
  maxBytes: number,
): { data: string; members: Member[] } | string {
  let text: string;
  try {
    text = strictUtf8.decode(raw);
  } catch {
    return "data is not valid UTF-8";
  }
  try {
    JSON.parse(text);
  } catch {
    return "data is not valid JSON";
  }
  const data = compact(text);
  if (!data.startsWith("{")) return "data is not a JSON object";
  const size = bytesOf(data);
  if (size > maxBytes)
    return `data is ${grouped(size)} bytes compacted; a piece holds at most ${grouped(maxBytes)}`;
  const members = scan(data);
  return typeof members === "string" ? members : { data, members };
}

// ---------------------------------------------------------------- Sends

// Every received_at has this length, so the same pieces always encode to the
// same size, whenever they arrive.
const RECEIVED_PLACEHOLDER = "0".repeat("2006-01-02T15:04:05.000000Z".length);

/**
 * The piece's line of its batch object, with its newline:
 * {"id":…,"received_at":"…","occurred_at":"…","data":{…}}, occurred_at left
 * out when the sender gave none.
 */
export const lineOf = (p: SentPiece, receivedAt: Instant) =>
  `{"id":${jsonString(p.id)},"received_at":"${receivedAt}"` +
  (p.occurredAt ? `,"occurred_at":"${rfc3339(p.occurredAt)}"` : "") +
  `,"data":${p.data}}\n`;

/**
 * Checks a send against every rule that needs no state, in order (the
 * number of pieces, then each piece's id and data, then the send's size),
 * and returns the first rule broken. A send is stored whole or not at all,
 * so one bad piece refuses it. The schema's rules come after, in mergeSchema.
 */
export function validate(
  pieces: readonly Piece[],
  limits: Limits = LIMITS,
): Send | Refusal {
  if (!pieces.length || pieces.length > limits.maxPieces)
    return invalid(
      `a send holds 1 to ${grouped(limits.maxPieces)} pieces; this one holds ${pieces.length ? grouped(pieces.length) : "none"}`,
    );
  const first = new Map<string, number>(),
    sent: SentPiece[] = [];
  let data = 0,
    bytes = 0;
  for (const [i, p] of pieces.entries()) {
    if (!validId(p.id))
      return refusePiece(
        i,
        p.id,
        `id must be 1 to ${MAX_ID_BYTES} printable ASCII characters with no spaces`,
      );
    const before = first.get(p.id);
    if (before !== undefined)
      return invalid(`piece ${i + 1} repeats the id of piece ${before + 1}`);
    first.set(p.id, i);
    const checked = checkData(p.data, limits.maxPieceBytes);
    if (typeof checked === "string") return refusePiece(i, p.id, checked);
    const piece: SentPiece = {
      id: p.id,
      ...(p.occurredAt ? { occurredAt: p.occurredAt } : {}),
      ...checked,
    };
    data += bytesOf(checked.data);
    bytes += bytesOf(lineOf(piece, RECEIVED_PLACEHOLDER));
    sent.push(piece);
  }
  if (data > limits.maxBytes)
    return invalid(
      `a send holds at most ${grouped(limits.maxBytes)} bytes of data, compacted; this one holds ${grouped(data)}`,
    );
  return { pieces: sent, bytes };
}

/**
 * Two sends with the same pieces, in the same order: each the same id,
 * occurred_at instant and compacted data. A client_token's retry must be.
 */
export const sameSend = (a: Send, b: Send) =>
  a.bytes === b.bytes &&
  a.pieces.length === b.pieces.length &&
  a.pieces.every(
    (p, i) =>
      p.id === b.pieces[i]!.id &&
      p.occurredAt === b.pieces[i]!.occurredAt &&
      p.data === b.pieces[i]!.data,
  );

// ---------------------------------------------------------------- Schema

const phrase = (t: FieldType) =>
  t === "object" || t === "array" ? `an ${t}` : `a ${t}`;

/** A schema's fields by byte order of name, as rows and responses order them. */
export const fieldsOf = (s: Schema): Field[] =>
  [...s.keys()].sort(byBytes).map((name) => ({ name, type: s.get(name)! }));

/** A schema after a send's fields joined it. */
export type Merged = {
  schema: Schema;
  // The fields the send added, by name, each with its type after the send.
  newFields: Field[];
};

/**
 * Adds a send's fields to the schema, in piece order, without changing it. A
 * non-null value must have its field's locked type; null is always allowed,
 * and a field seen only as null takes its first non-null value's type. A
 * send that adds fields past maxFields is refused; one that adds none never
 * is.
 */
export function mergeSchema(
  schema: Schema,
  send: Send,
  maxFields = LIMITS.maxFields,
): Merged | Refusal {
  const types = new Map(schema),
    added: string[] = [];
  for (const [i, p] of send.pieces.entries())
    for (const f of p.members) {
      const held = types.get(f.name);
      if (held === undefined) {
        types.set(f.name, f.type);
        added.push(f.name);
      } else if (f.type !== "null" && f.type !== held) {
        if (held !== "null")
          return refusePiece(
            i,
            p.id,
            `${quote(f.name)} is ${phrase(f.type)}; this source holds ${quote(f.name)} as ${phrase(held)}`,
          );
        types.set(f.name, f.type);
      }
    }
  if (added.length && types.size > maxFields)
    return {
      refused: "fields",
      message: `this source has ${schema.size} fields and this send adds ${added.length}, past the ${maxFields} a source may have: send pieces without new fields, or make another source`,
    };
  return {
    schema: types,
    newFields: added
      .sort(byBytes)
      .map((name) => ({ name, type: types.get(name)! })),
  };
}

// ---------------------------------------------------------------- Rows

/** What a row that lacks a field spends on it: `,"<name>":null`. */
const nullFill = (name: string) => bytesOf(jsonString(name)) + 6;

/**
 * The piece as a row of its source's events table: id, received_at and
 * occurred_at (null when not sent), then every field of the schema by byte
 * order of name, null where the piece lacks it, each value as sent. The
 * schema holds every field of the piece: a send's fields join it with the
 * send.
 */
export function rowOf(p: SentPiece, receivedAt: Instant, schema: Schema) {
  const values = new Map(p.members.map((m) => [m.name, m.value]));
  return (
    `{"id":${jsonString(p.id)},"received_at":"${receivedAt}","occurred_at":` +
    (p.occurredAt ? `"${rfc3339(p.occurredAt)}"` : "null") +
    fieldsOf(schema)
      .map((f) => `,${jsonString(f.name)}:${values.get(f.name) ?? "null"}`)
      .join("") +
    "}"
  );
}

/** A stored row padded with null for each of these fields, at its end. */
export const padRow = (row: string, fields: readonly string[]) =>
  row.slice(0, -1) +
  fields.map((name) => `,${jsonString(name)}:null`).join("") +
  "}";

/** The events table's columns: id, received_at and occurred_at, then the fields. */
export const columnsOf = (s: Schema): Field[] => [
  { name: "id", type: "string" },
  { name: "received_at", type: "string" },
  { name: "occurred_at", type: "string" },
  ...fieldsOf(s),
];

// ---------------------------------------------------------------- Text field

/**
 * Why `name` can't be a source's text field (events.ValidFieldName), or
 * undefined when it can, in the backend's words: the rules of a top-level
 * key, so any field a piece may carry can be one. "" is no name: it clears
 * the text field, and is never checked.
 */
export function textFieldProblem(name: string): string | undefined {
  const size = bytesOf(name);
  if (size < 1 || size > MAX_KEY_BYTES)
    return `a field name is 1 to ${MAX_KEY_BYTES} bytes; this one is ${size}`;
  if (/\p{Cc}/u.test(name)) return "a field name has no control characters";
  if (name.includes("`")) return "a field name has no backticks";
  return RESERVED.has(name)
    ? `${name} is a column of every piece, not a field of its data`
    : undefined;
}

/**
 * The schema with `name` locked as the source's text field
 * (events.Schema.WithText), without changing `schema`: a field held as a
 * string stays, one held only as null becomes a string, and an unseen one
 * joins as a string, so later pieces must send it as one. A field of another
 * type is refused as a send's type conflict is (invalid), and an unseen one
 * past maxFields as a send's new fields are (fields), each with the
 * backend's message. tests/unit/event-sources-mock.test.ts runs the
 * backend's text_fields table through it.
 */
export function withText(
  schema: Schema,
  name: string,
  maxFields = LIMITS.maxFields,
): Schema | Refusal {
  const problem = textFieldProblem(name);
  if (problem) return invalid(problem);
  const held = schema.get(name);
  if (held === "string") return schema;
  if (held !== undefined && held !== "null")
    return invalid(
      `a text field holds strings; this source holds ${quote(name)} as ${phrase(held)}`,
    );
  if (held === undefined && schema.size >= maxFields)
    return {
      refused: "fields",
      message: `this source has ${schema.size} fields and a source may have ${maxFields}: choose a text field its pieces already carry`,
    };
  return new Map(schema).set(name, "string");
}

// ---------------------------------------------------------------- Documents

/**
 * The most pieces a source with a text field keeps as documents
 * (foundry.events.max_extracted): its set never holds more.
 */
export const MAX_EXTRACTED = 500;

/** A number as it was written: JSON.parse alone would round it. */
class Literal {
  constructor(readonly text: string) {}
}
/** A row read back: its own columns, then its data's fields, numbers exact. */
export type Row = {
  id: string;
  receivedAt: Instant;
  // As the row holds it (RFC 3339), or null when the sender gave none.
  occurredAt: string | null;
  fields: Record<string, unknown>;
};

/** Reads a row of the events table, as rowOf writes one. */
export function readRow(row: string): Row {
  const parsed = JSON.parse(
    row,
    // Source text access (Node 21 and later) keeps each number as written.
    (_, value: unknown, context?: { source?: string }) =>
      typeof value === "number" ? new Literal(context!.source!) : value,
  ) as Record<string, unknown>;
  const { id, received_at, occurred_at, ...fields } = parsed;
  return {
    id: id as string,
    receivedAt: received_at as Instant,
    occurredAt: (occurred_at as string | null) ?? null,
    fields,
  };
}

/**
 * The newest n rows by receipt (events.Newest): received_at descending,
 * then id ascending, so each sync keeps the same ones.
 */
export const newest = <R extends Pick<Row, "id" | "receivedAt">>(
  rows: readonly R[],
  n: number,
): R[] =>
  [...rows]
    .sort((a, b) =>
      a.receivedAt !== b.receivedAt
        ? a.receivedAt < b.receivedAt
          ? 1
          : -1
        : a.id < b.id
          ? -1
          : a.id > b.id
            ? 1
            : 0,
    )
    .slice(0, n);

// The most a string's line shows and a title holds, each "…" included; how
// many of an array's values a line shows; how deep a line's path goes, the
// top-level field counting as 1 (field.key.key); and how many lines all the
// fields inside objects get.
const MAX_VALUE = 500,
  MAX_TITLE = 120,
  MAX_LISTED = 20,
  MAX_PATH = 3,
  MAX_NESTED_LINES = 50;

/** s when it has at most n code points, else its first n - 1 and "…". */
const cut = (s: string, n: number) => {
  const runes = [...s];
  return runes.length > n ? runes.slice(0, n - 1).join("") + "…" : s;
};
// A run of what ends a line (events.lineBreak): LF, VT, FF, CR, NEL, and
// the line and paragraph separators.
const LINE_BREAKS = /[\n\v\f\r\u0085\u2028\u2029]+/;
/** s with each run of line breaks one space, none at either end, cut at MAX_VALUE. */
const oneLine = (s: string) =>
  cut(s.split(LINE_BREAKS).filter(Boolean).join(" "), MAX_VALUE);
/**
 * The text's first line that isn't blank, its spaces (Go's unicode.IsSpace,
 * the White_Space property) collapsed, cut at MAX_TITLE; "" when every line
 * is blank.
 */
const title = (text: string) => {
  const words = text
    .split(LINE_BREAKS)
    .map((line) => line.split(/\p{White_Space}+/u).filter(Boolean))
    .find((w) => w.length);
  return words ? cut(words.join(" "), MAX_TITLE) : "";
};

const isObject = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" &&
  v !== null &&
  !Array.isArray(v) &&
  !(v instanceof Literal);

// A scalar or an array as its line shows it (events.shown): a string on one
// line, a number as written, a boolean, an array's first MAX_LISTED values
// (null as "null") then how many more, or how many records it holds when
// one is an object or array. "" for null, an empty string, an empty array
// and an object: none has a line of its own.
function shown(v: unknown): string {
  if (v instanceof Literal) return v.text;
  if (typeof v === "boolean") return String(v);
  if (typeof v === "string") return oneLine(v);
  if (!Array.isArray(v) || !v.length) return "";
  if (
    v.some(
      (x) => typeof x === "object" && x !== null && !(x instanceof Literal),
    )
  )
    return v.length === 1 ? "1 record" : `${grouped(v.length)} records`;
  const more = v.length - MAX_LISTED;
  return (
    v
      .slice(0, MAX_LISTED)
      .map((x) => (x === null ? "null" : shown(x)))
      .join(", ") + (more > 0 ? `, and ${grouped(more)} more` : "")
  );
}

// The lines of the value at path, depth deep (events.listing.field): one
// for a value with something to show, and those of an object's keys, by
// byte order of key, each on one line, while depth is below MAX_PATH.
// `nested` marks a line of a field inside an object.
function fieldLines(
  path: string,
  v: unknown,
  depth: number,
): { line: string; nested: boolean }[] {
  if (isObject(v))
    return depth < MAX_PATH
      ? Object.keys(v)
          .sort(byBytes)
          .flatMap((key) =>
            fieldLines(`${path}.${oneLine(key)}`, v[key], depth + 1),
          )
      : [];
  const value = shown(v);
  return value ? [{ line: `- ${path}: ${value}`, nested: depth > 1 }] : [];
}

/**
 * A row as its piece's Markdown document (events.Render), byte for byte:
 *
 *     # <title>
 *
 *     - Piece: <id>
 *     - Occurred: <occurred_at>   (when the piece has one)
 *     - <field>: <value>          (every other field, by byte order of name)
 *     - <field>.<key>: <value>    (inside objects, by byte order of key)
 *
 *     <the text, verbatim>        (when it isn't blank)
 *
 * The title is the text's first line that isn't blank, its spaces
 * collapsed, at most MAX_TITLE characters ("…" ending one cut), else the
 * piece's id. A field's line is shown(value) after its path, each part on
 * one line; null, "" and [] have none, paths go MAX_PATH deep, and the
 * fields inside objects get MAX_NESTED_LINES lines in all, top-level ones
 * never cut. received_at is never in it, so an identical resend renders the
 * same bytes, as does a row padded with null. The unit tests hold it to the
 * backend's goldens: tests/unit/__fixtures__/event-render holds copies of
 * the events package's testdata/render.
 */
export function render(
  row: Row,
  textField: string,
): { title: string; body: string } {
  const value = row.fields[textField],
    text = typeof value === "string" ? value : "",
    heading = title(text);
  let nested = 0;
  const lines = Object.keys(row.fields)
    .filter((name) => name !== textField)
    .sort(byBytes)
    .flatMap((name) => fieldLines(oneLine(name), row.fields[name], 1))
    // In order, so the first MAX_NESTED_LINES nested lines are kept.
    .flatMap((l) =>
      !l.nested || nested++ < MAX_NESTED_LINES ? [`${l.line}\n`] : [],
    );
  return {
    title: heading || row.id,
    body:
      `# ${heading || row.id}\n\n- Piece: ${row.id}\n` +
      (row.occurredAt ? `- Occurred: ${row.occurredAt}\n` : "") +
      lines.join("") +
      // Blank text is no text.
      (heading ? `\n${text}` : ""),
  };
}

// ---------------------------------------------------------------- Caps

// The most a row exceeds its line by beyond the schema's null fill: a null
// occurred_at (19 bytes) less the data wrapper (9).
const ROW_SLACK = 10;

/** The limits every table has (foundry.sync max_rows_per_table, max_table_bytes). */
export type Caps = { maxRows: number; maxBytes: number };
export const CAPS: Caps = { maxRows: 2_000_000, maxBytes: 2 * 2 ** 30 };

/**
 * How full a source's table will be once every waiting batch is merged:
 * upper bounds, since a resent id counts again and every waiting piece
 * counts every field's padding.
 */
export type Capacity = Caps & { rows: number; bytes: number };

/** What a source holds, as the caps count it. */
export type Holding = {
  // Its events table's rows and uncompressed bytes; 0 before its first sync.
  tableRows: number;
  tableBytes: number;
  // The table's stored columns: a schema field missing here pads every row.
  tableFields: readonly string[];
  // The batches no sync has merged yet.
  pendingPieces: number;
  pendingBytes: number;
};

/** The source's capacity once its waiting batches are merged under schema s. */
export function capacity(h: Holding, s: Schema, caps: Caps): Capacity {
  const unpadded = [...s.keys()]
    .filter((name) => !h.tableFields.includes(name))
    .reduce((n, name) => n + nullFill(name), 0);
  const fill = [...s.keys()].reduce((n, name) => n + nullFill(name), 0);
  return {
    ...caps,
    rows: h.tableRows + h.pendingPieces,
    bytes:
      h.tableBytes +
      h.tableRows * unpadded +
      h.pendingBytes +
      h.pendingPieces * (fill + ROW_SLACK),
  };
}

/**
 * The capacity once the send is stored, with `after` the schema its merge
 * gives, or the refusal when that passes a cap: refusing the send keeps
 * every later sync of the source within the caps.
 */
export function project(
  h: Holding,
  send: Send,
  after: Schema,
  caps: Caps,
): Capacity | Refusal {
  const c = capacity(
    {
      ...h,
      pendingPieces: h.pendingPieces + send.pieces.length,
      pendingBytes: h.pendingBytes + send.bytes,
    },
    after,
    caps,
  );
  if (c.rows > caps.maxRows)
    return {
      refused: "full",
      message: `This source holds ${grouped(h.tableRows + h.pendingPieces)} pieces, and one source holds at most ${grouped(caps.maxRows)}: this send's ${grouped(send.pieces.length)} would pass that. Send to a new source.`,
    };
  if (c.bytes > caps.maxBytes)
    return {
      refused: "full",
      message: `This source's table would hold ${grouped(c.bytes)} bytes with this send, and one source holds at most ${grouped(caps.maxBytes)}. Send to a new source.`,
    };
  return c;
}

/**
 * What every send, list and page says from 80% full, or "" below: how full
 * the fuller count is (rows on a tie) and what happens at 100%.
 */
export function warning(c: Capacity): string {
  const rows = Math.floor((c.rows * 100) / c.maxRows),
    bytes = Math.floor((c.bytes * 100) / c.maxBytes),
    percent = Math.max(rows, bytes);
  if (percent < 80) return "";
  const held =
    bytes > rows
      ? `${grouped(c.bytes)} of ${grouped(c.maxBytes)} bytes`
      : `${grouped(c.rows)} of ${grouped(c.maxRows)} pieces`;
  return `This source is ${percent}% full (${held}). Once it is full, sends are refused: send new pieces to a new source before then.`;
}
