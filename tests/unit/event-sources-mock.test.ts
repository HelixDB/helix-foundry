import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { FoundryDocument, FoundryDocumentSet } from "@/shared/wfe";
import * as rules from "../../mock/event-pieces";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { sendPieces } from "../../mock/routes/event-sources";
import { config, db, resetState } from "../../mock/state";

// API sources (connector kind events) in the mock WFE behave as
// the backend's do: the piece and text-field rules are the backend's
// own tables (__fixtures__/event-pieces.json, a copy of the events
// package's testdata/pieces.json), a piece's document is its
// goldens' (__fixtures__/event-render, copies of testdata/render), and
// sends, syncs, send tokens, recent sends and
// generation refuse and answer as the backend does. Above all, a stored send
// is never lost or doubled: a retry with its client_token stores nothing,
// and a sync merges every batch by piece id, the later piece replacing the
// earlier. A source with a text field keeps its newest 500 pieces as
// documents of its own set, never more, one per piece, and pauses while its
// allowance is spent; a generation reads that set only by extraction.

type FixturePiece = {
  id: string;
  repeat?: number;
  occurred_at?: string;
  data_json?: string;
  data_base64?: string;
  data_size?: number;
};
type Fixture = {
  limits: {
    max_pieces_per_send: number;
    max_send_bytes: number;
    max_piece_bytes: number;
    max_fields: number;
  };
  cases: {
    name: string;
    schema?: Record<string, rules.FieldType>;
    schema_fields?: number;
    pieces: FixturePiece[];
    want: {
      refused?: "invalid" | "fields";
      // The refused piece's place, from 1 (0: the send as a whole): the
      // HTTP endpoint's `piece`. No mock route answers with it, so the
      // cases below leave it out.
      piece?: number;
      message?: string;
      bytes?: number;
      new_fields?: rules.Field[];
      schema?: Record<string, rules.FieldType>;
      schema_fields?: number;
    };
  }[];
  // A source's text field (Go's Schema.WithText, schema_test.go): on a
  // source holding schema and schema_fields number fields f1 to fN.
  text_fields: {
    name: string;
    schema?: Record<string, rules.FieldType>;
    schema_fields?: number;
    text_field: string;
    want: {
      refused?: "invalid" | "fields";
      message?: string;
      schema?: Record<string, rules.FieldType>;
      schema_fields?: number;
    };
  }[];
};
const fixture = JSON.parse(
  readFileSync(
    new URL("./__fixtures__/event-pieces.json", import.meta.url),
    "utf8",
  ),
) as Fixture;
const utf8 = (s: string) => new TextEncoder().encode(s);

describe("the backend's piece rules", () => {
  const limits: rules.Limits = {
    maxPieces: fixture.limits.max_pieces_per_send,
    maxBytes: fixture.limits.max_send_bytes,
    maxPieceBytes: fixture.limits.max_piece_bytes,
    maxFields: fixture.limits.max_fields,
  };

  it("are foundry.events' defaults", () => {
    expect(rules.LIMITS).toEqual(limits);
  });

  // As envelope_test.go runs each case: the pieces' times are read first,
  // then the send is validated and its fields merged into the schema.
  it.each(fixture.cases.map((c) => [c.name, c] as const))("%s", (_, c) => {
    const schema: rules.Schema = c.schema_fields
      ? new Map(
          Array.from({ length: c.schema_fields }, (_, i) => [
            `f${i + 1}`,
            "number" as const,
          ]),
        )
      : new Map(Object.entries(c.schema || {}));
    const pieces: rules.Piece[] = [];
    let refused: rules.Refusal | undefined;
    for (const p of c.pieces) {
      const data =
        p.data_json !== undefined
          ? utf8(p.data_json)
          : p.data_base64 !== undefined
            ? new Uint8Array(Buffer.from(p.data_base64, "base64"))
            : utf8(`{"s":"${"a".repeat(p.data_size! - 8)}"}`);
      const ids = p.repeat
        ? Array.from({ length: p.repeat }, (_, i) => `${p.id}${i + 1}`)
        : [p.id];
      for (const id of ids) {
        const at =
          p.occurred_at !== undefined && !refused
            ? rules.parseOccurredAt(pieces.length, id, p.occurred_at)
            : undefined;
        if (rules.isRefusal(at)) refused = at;
        pieces.push({
          id,
          ...(typeof at === "string" ? { occurredAt: at } : {}),
          data,
        });
      }
    }
    const send = refused || rules.validate(pieces, limits);
    const merged = rules.isRefusal(send)
      ? send
      : rules.mergeSchema(schema, send, limits.maxFields);
    if (c.want.refused) {
      expect(merged).toEqual({
        refused: c.want.refused,
        message: c.want.message,
      });
      return;
    }
    if (rules.isRefusal(send) || rules.isRefusal(merged))
      throw new Error(`refused: ${JSON.stringify(merged)}`);
    expect(send.bytes).toBe(c.want.bytes);
    expect(merged.newFields).toEqual(c.want.new_fields);
    if (c.want.schema_fields)
      expect(merged.schema.size).toBe(c.want.schema_fields);
    else expect(Object.fromEntries(merged.schema)).toEqual(c.want.schema || {});
  });

  it("writes each line as the backend's batch object does", () => {
    const at = rules.parseOccurredAt(0, "o-17", "2026-10-23T20:20:00+02:00");
    const send = rules.validate([
      {
        id: "o-17",
        occurredAt: at as string,
        data: utf8('{ "amount": 12.50, "customer": "c_1" }'),
      },
    ]) as rules.Send;
    const line = rules.lineOf(send.pieces[0]!, "2026-10-23T18:25:01.123456Z");
    expect(line).toBe(
      '{"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00Z","data":{"amount":12.50,"customer":"c_1"}}\n',
    );
    expect(utf8(line).length).toBe(send.bytes);
    // The row: id, the times, then every field by name, null where absent,
    // each value as sent.
    const schema = new Map<string, rules.FieldType>([
      ["note", "string"],
      ["amount", "number"],
      ["customer", "string"],
    ]);
    expect(
      rules.rowOf(send.pieces[0]!, "2026-10-23T18:25:01.123456Z", schema),
    ).toBe(
      '{"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00Z","amount":12.50,"customer":"c_1","note":null}',
    );
  });

  it("reads times as Go's RFC 3339 parser does, to the microsecond", () => {
    const at = (text: string) => rules.parseOccurredAt(0, "p", text);
    expect(at("2026-10-23T18:20:00.1234567Z")).toBe(
      "2026-10-23T18:20:00.123456Z",
    );
    expect(at("2026-10-23T8:20:00,5Z")).toBe("2026-10-23T08:20:00.500000Z");
    expect(at("2026-10-23T23:30:00-01:00")).toBe("2026-10-24T00:30:00.000000Z");
    expect(at("0999-12-31T23:30:00-01:00")).toBe("1000-01-01T00:30:00.000000Z");
    for (const text of [
      "2026-02-29T00:00:00Z",
      "2026-13-01T00:00:00Z",
      "2026-10-23T24:00:00Z",
      "2026-10-23T18:60:00Z",
      "2026-10-23T18:20:60Z",
      "2026-10-23t18:20:00Z",
      "2026-10-23T18:20:00z",
      "2026-10-23T18:20:00.Z",
      "2026-10-23T18:20:00+0200",
    ])
      expect(at(text), text).toEqual({
        refused: "invalid",
        message: 'piece 1 (id "p"): occurred_at is not an RFC 3339 time',
      });
    expect(at("2024-02-29T00:00:00Z")).toBe("2024-02-29T00:00:00.000000Z");
    expect(rules.rfc3339("2026-10-23T18:20:00.500000Z")).toBe(
      "2026-10-23T18:20:00.5Z",
    );
  });

  it("quotes ids and keys as Go's %q does", () => {
    const send = rules.validate([
      { id: "o-1", data: utf8('{"a\u00adb":1}') },
    ]) as rules.Send;
    expect(rules.mergeSchema(new Map([["a\u00adb", "string"]]), send)).toEqual({
      refused: "invalid",
      message:
        'piece 1 (id "o-1"): "a\\u00adb" is a number; this source holds "a\\u00adb" as a string',
    });
  });
});

describe("a text field's rules", () => {
  it("are the backend's table", () => {
    expect(fixture.text_fields.length).toBeGreaterThan(0);
  });

  // As schema_test.go runs each case: a source holding schema and, beside
  // it, schema_fields fields f1 to fN that are all numbers.
  it.each(fixture.text_fields.map((c) => [c.name, c] as const))(
    "%s",
    (_, c) => {
      const schema = new Map<string, rules.FieldType>([
        ...Array.from(
          { length: c.schema_fields || 0 },
          (_, i): [string, rules.FieldType] => [`f${i + 1}`, "number"],
        ),
        ...Object.entries(c.schema || {}),
      ]);
      const before = new Map(schema);
      const got = rules.withText(
        schema,
        c.text_field,
        fixture.limits.max_fields,
      );
      // The schema it was given is unchanged.
      expect(schema).toEqual(before);
      if (c.want.refused) {
        expect(got).toEqual({
          refused: c.want.refused,
          message: c.want.message,
        });
        return;
      }
      if (rules.isRefusal(got)) throw new Error(got.message);
      expect(got.get(c.text_field)).toBe("string");
      if (c.want.schema_fields) expect(got.size).toBe(c.want.schema_fields);
      else expect(Object.fromEntries(got)).toEqual(c.want.schema);
    },
  );

  it("quotes a field as Go's %q does, and checks a name as the backend's Prepare does", () => {
    expect(rules.withText(new Map([['a "b', "number"]]), 'a "b')).toEqual({
      refused: "invalid",
      message:
        'a text field holds strings; this source holds "a \\"b" as a number',
    });
    expect(rules.textFieldProblem("reply")).toBeUndefined();
    expect(rules.textFieldProblem("id")).toBe(
      "id is a column of every piece, not a field of its data",
    );
  });
});

describe("a piece as its document", () => {
  // As render_test.go's pieceRow: piece p, received at 09:00 with no
  // occurred_at, holding these fields (`"name":value,...`, or "").
  const pieceRow = (fields: string) =>
    `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null${fields ? "," + fields : ""}}`;
  const render = (row: string, textField = "reply") =>
    rules.render(rules.readRow(row), textField);
  // The lines between "- Piece: p" and the text, as render_test.go's fieldLines.
  const fieldLines = (fields: string) =>
    render(pieceRow(fields)).body.split("- Piece: p\n")[1]!.split("\n\n")[0];
  const golden = (name: string) =>
    readFileSync(
      new URL(`./__fixtures__/event-render/${name}.md`, import.meta.url),
      "utf8",
    );
  const pad = (n: number) => String(n).padStart(2, "0");

  // render_test.go's TestRenderGoldens: each golden is the document the
  // backend's extraction reads for a piece, and every row of its case must
  // render to it byte for byte, or a version the mock gives moves where
  // dev's doesn't.
  const keys = Array.from(
      { length: 60 },
      (_, i) => `"k${pad(59 - i)}":${59 - i}`,
    ),
    tags = Array.from({ length: 25 }, (_, i) => `"t${pad(i + 1)}"`),
    titleLine = "Café ☕ — 日本語のテキスト \u{1f389} ",
    long =
      "## Weekly summary\n\n" +
      Array.from(
        { length: 120 },
        (_, i) =>
          `${i + 1}. Item ${i + 1}: the customer asked about invoice INV-${String(i).padStart(4, "0")} and was told it ships soon.\n`,
      ).join("") +
      '\n```json\n{"a": 1}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n';
  it.each([
    {
      name: "plain",
      title: "Thanks for the order.",
      rows: [
        String.raw`{"id":"o-17","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00Z","amount":12.50,"customer":"c_1","note":null,"paid":true,"reply":"Thanks for the order.\n\nIt ships Monday.\n","status":"shipped\nlate"}`,
        // The same piece sent again later, in a row whose keys and padding
        // differ, as a snapshot's row may.
        String.raw`{"id":"o-17","received_at":"2026-10-24T10:30:00.123456Z","occurred_at":"2026-10-23T18:20:00Z","status":"shipped\nlate","reply":"Thanks for the order.\n\nIt ships Monday.\n","paid":true,"customer":"c_1","amount":12.50}`,
      ],
    },
    {
      name: "nested",
      title: "Nested data",
      rows: [
        `{"id":"p-1","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"f":true,"b":{"e":"deep","c":{"d":1}}},"m":{${keys.join(",")}},"reply":"Nested data","z":"last","zz":{"q":1}}`,
      ],
    },
    {
      name: "arrays",
      title: "Arrays",
      rows: [
        String.raw`{"id":"p-2","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"empty":[],"flags":[true,false,null],"meta":{"tags":["a","b"],"none":[]},"nums":[9223372036854775807,1e-7,-0.5,2.50],"one":["only\none"],"reply":"Arrays","tags":[${tags.join(",")}]}`,
      ],
    },
    {
      name: "records",
      title: "Records",
      rows: [
        `{"id":"p-3","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"items":[{"sku":"a"},{"sku":"b"},{"sku":"c"}],"lines":{"rows":[{"x":1},{"x":2}]},"matrix":[[1,2],[3]],"mixed":[1,{"a":1}],"reply":"Records","single":[{"sku":"a"}]}`,
      ],
    },
    {
      name: "long-value",
      title: "Long value",
      rows: [
        String.raw`{"id":"p-4","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":"Long value","summary":"First line\r\nsecond line\n\n\nthird\u2028fourth ${"é".repeat(600)}"}`,
      ],
    },
    {
      name: "unicode-title",
      title: [...titleLine.repeat(7)].slice(0, 119).join("") + "…",
      rows: [
        `{"id":"p-5","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":${rules.jsonString("\n \n\t " + titleLine.repeat(7) + "\nSecond line")}}`,
      ],
    },
    {
      name: "no-text",
      title: "p-6",
      rows: [
        `{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":null}`,
        `{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1}`,
        `{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":""}`,
        String.raw`{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":" \n\t\u2028\u00a0 "}`,
      ],
    },
    {
      name: "long-text",
      title: "## Weekly summary",
      rows: [
        `{"id":"p-7","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":${rules.jsonString(long)},"source":"agent"}`,
      ],
    },
  ])("renders the backend's $name golden", ({ name, title, rows }) => {
    const want = golden(name);
    for (const row of rows) expect(render(row)).toEqual({ title, body: want });
  });

  it("ends with the text verbatim", () => {
    const row = `{"id":"p-7","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":${rules.jsonString(long)}}`;
    expect(render(row).body.endsWith("\n\n" + long)).toBe(true);
  });

  // render_test.go's TestRenderStability: an identical resend is kept, no
  // write and no extraction, only if nothing but the piece's data reaches
  // its document.
  const base = render(
    String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`,
  ).body;
  it.each([
    [
      "a later arrival",
      String.raw`{"id":"p","received_at":"2026-10-25T11:12:13.141516Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "padded nulls",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"note":null,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "no occurred_at key",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "keys in another order",
      String.raw`{"tags":["a"],"reply":"Hi","n":1,"a":{"y":{"z":2},"x":1},"occurred_at":null,"received_at":"2026-10-24T09:00:00.000000Z","id":"p"}`,
    ],
    [
      "data deeper than shown",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2,"deeper":{"w":3}}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
  ])("keeps its body with %s", (_, row) => {
    expect(render(row).body).toBe(base);
  });
  it.each([
    [
      "another text",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi!","tags":["a"]}`,
    ],
    [
      "another value",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1.0,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "a nested value",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":3}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "an occurred_at",
      String.raw`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00Z","a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
    [
      "another id",
      String.raw`{"id":"q","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`,
    ],
  ])("changes its body with %s", (_, row) => {
    expect(render(row).body).not.toBe(base);
  });

  // render_test.go's TestRenderNumbers: a number reaches the document as
  // sent, in a field, an object or an array.
  it.each([
    "9223372036854775807",
    "-9223372036854775808",
    "18446744073709551617",
    "1e-7",
    "1E+2",
    "-0",
    "0.10",
    "1.50e300",
    "12.50",
  ])("shows %s as sent", (n) => {
    expect(fieldLines(`"l":[${n},${n}],"n":${n},"o":{"n":${n}}`)).toBe(
      `- l: ${n}, ${n}\n- n: ${n}\n- o.n: ${n}\n`,
    );
  });

  // render_test.go's TestRenderLines: each case pins one rule of a field's
  // lines.
  const items = Array.from({ length: 21 }, (_, i) => String(i + 1)),
    wide = Array.from({ length: 30 }, (_, i) => `"a${pad(i)}":${i}`),
    wider = Array.from({ length: 30 }, (_, i) => `"b${pad(i)}":${i}`),
    keyLines = (prefix: string, from: number, to: number) =>
      Array.from(
        { length: to - from },
        (_, i) => `- ${prefix}${pad(from + i)}: ${from + i}\n`,
      ).join("");
  it.each([
    [
      "orders fields by name byte order",
      `"é":3,"a":2,"B":1`,
      "- B: 1\n- a: 2\n- é: 3\n",
    ],
    [
      "folds every kind of line break",
      String.raw`"s":"a\r\nb\n\nc\u2028d\u2029e\u0085f\u000bg\u000ch\ri"`,
      "- s: a b c d e f g h i\n",
    ],
    ["keeps other spaces", String.raw`"s":"a\tb  c"`, "- s: a\tb  c\n"],
    ["drops line breaks at either end", String.raw`"s":"\n\na\n"`, "- s: a\n"],
    [
      "keeps a string of 500 runes whole",
      `"s":"${"é".repeat(500)}"`,
      `- s: ${"é".repeat(500)}\n`,
    ],
    [
      "cuts a string of 501 runes",
      `"s":"${"é".repeat(501)}"`,
      `- s: ${"é".repeat(499)}…\n`,
    ],
    [
      "cuts a string after folding it",
      `"s":"${"\\n".repeat(400)}${"x".repeat(500)}"`,
      `- s: ${"x".repeat(500)}\n`,
    ],
    [
      "leaves out what has no value",
      String.raw`"a":null,"b":"","c":[],"d":{},"e":"\n\n"`,
      "",
    ],
    ["shows booleans", `"f":false,"t":true`, "- f: false\n- t: true\n"],
    [
      "lists 20 scalars whole",
      `"l":[${items.slice(0, 20).join(",")}]`,
      `- l: ${items.slice(0, 20).join(", ")}\n`,
    ],
    [
      "lists the first 20 of 21",
      `"l":[${items.join(",")}]`,
      `- l: ${items.slice(0, 20).join(", ")}, and 1 more\n`,
    ],
    [
      "lists the first 20 of 1,020, the rest counted in thousands",
      `"l":[${Array.from({ length: 1020 }, () => "1").join(",")}]`,
      `- l: ${Array.from({ length: 20 }, () => "1").join(", ")}, and 1,000 more\n`,
    ],
    ["shows null in a list", `"l":[null,"",1]`, "- l: null, , 1\n"],
    [
      "folds and cuts a string in a list",
      String.raw`"l":["a\nb","${"x".repeat(501)}"]`,
      `- l: a b, ${"x".repeat(499)}…\n`,
    ],
    ["counts one record", `"l":[{}]`, "- l: 1 record\n"],
    ["counts an array of arrays as records", `"l":[[]]`, "- l: 1 record\n"],
    [
      "counts records in thousands",
      `"l":[${Array.from({ length: 1000 }, () => "{}").join(",")}]`,
      "- l: 1,000 records\n",
    ],
    [
      "names a nested key on one line",
      String.raw`"o":{"k\nx":1,"":2}`,
      "- o.: 2\n- o.k x: 1\n",
    ],
    [
      "cuts a long nested key",
      `"o":{"${"k".repeat(501)}":1}`,
      `- o.${"k".repeat(499)}…: 1\n`,
    ],
    ["shows a nested key twice once", `"o":{"k":1,"k":2}`, "- o.k: 2\n"],
    [
      "goes 3 deep and no deeper",
      `"o":{"a":{"b":1,"c":{"d":1},"l":[1,{"x":1}]}}`,
      "- o.a.b: 1\n- o.a.l: 2 records\n",
    ],
    [
      "gives fields inside objects 50 lines in all",
      `"a":{${wide.join(",")}},"b":{${wider.join(",")}},"c":1`,
      keyLines("a.a", 0, 30) + keyLines("b.b", 0, 20) + "- c: 1\n",
    ],
    [
      "counts no top-level field in the 50",
      `"a":{${wide.join(",")},${wider.join(",")}},"b":2,"c":[3]`,
      keyLines("a.a", 0, 30) + keyLines("a.b", 0, 20) + "- b: 2\n- c: 3\n",
    ],
    [
      "never cuts top-level fields",
      Array.from({ length: 60 }, (_, i) => `"f${pad(i)}":${i}`).join(","),
      keyLines("f", 0, 60),
    ],
    [
      "counts only lines shown in the 50",
      `"a":{"0":null,"1":"","2":[],"3":{},${wide.join(",")},${wider.join(",")}}`,
      keyLines("a.a", 0, 30) + keyLines("a.b", 0, 20),
    ],
  ])("%s", (_, fields, want) => {
    expect(fieldLines(fields)).toBe(want);
  });

  // render_test.go's TestRenderTitle: one line of the text, short enough to
  // read in a list.
  it.each([
    ["is the first line", "Hello\nworld", "Hello"],
    ["skips blank lines", "\n \r\n\t\nHello", "Hello"],
    ["collapses spaces", "  Hello \t  world  ", "Hello world"],
    ["collapses Unicode spaces", "Hello\u00a0\u3000world", "Hello world"],
    ["ends at any line break", "a\u2028b", "a"],
    ["keeps a heading's marks", "# Title\n\nText", "# Title"],
    ["keeps 120 runes whole", "é".repeat(120), "é".repeat(120)],
    ["cuts 121 runes", "é".repeat(121), "é".repeat(119) + "…"],
    ["cuts emoji by rune", "🎉".repeat(130) + "\nmore", "🎉".repeat(119) + "…"],
    ["is the id for a blank text", " \n\t\u00a0\u2028", "p"],
    ["is the id for an empty text", "", "p"],
  ])("a title %s", (_, text, want) => {
    const r = render(pieceRow(`"reply":${rules.jsonString(text)}`));
    expect(r.title).toBe(want);
    expect(r.body.startsWith(`# ${want}\n\n- Piece: p\n`)).toBe(true);
    expect([...r.title].length).toBeLessThanOrEqual(120);
  });

  it("keeps the newest by receipt, ties to the lower id", () => {
    const at = (id: string, receivedAt: string) => ({ id, receivedAt });
    const rows = [
      at("b", "2026-10-18T12:00:00.000000Z"),
      at("c", "2026-10-18T12:00:01.000000Z"),
      at("a", "2026-10-18T12:00:00.000000Z"),
      at("d", "2026-10-17T12:00:00.000000Z"),
    ];
    expect(rules.newest(rows, 3).map((r) => r.id)).toEqual(["c", "a", "b"]);
    expect(rules.newest(rows, 10)).toHaveLength(4);
    expect(rules.newest(rows, 0)).toEqual([]);
    // The rows it was given keep their order.
    expect(rows.map((r) => r.id)).toEqual(["b", "c", "a", "d"]);
  });
});

// ---------------------------------------------------------------- Routes

const ACME = "ws_acme",
  GLOBEX = "ws_globex",
  STRIPE = "conn_acme_stripe";
const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));

async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    return (await r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const ok = (reply: MockReply & { body: any }) => {
  expect(reply.status, JSON.stringify(reply.body)).toBe(200);
  return reply.body;
};
const refusal = (reply: MockReply & { body: any }) => ({
  status: reply.status,
  message: reply.body.message as string,
  reason: reply.body.details?.[0]?.reason as string | undefined,
});
const base = (ws = ACME) => `/v1/workspaces/${ws}/foundry`;
const create = async (name = "Orders", description = "One order line") =>
  ok(
    await call("POST", `${base()}/connectors`, {
      name,
      config: { events: { description } },
    }),
  ).connector;
const connector = async (id: string, ws = ACME) =>
  ok(await call("GET", `${base(ws)}/connectors/${id}`)).connector;
const batches = async (id: string, query = "") =>
  ok(await call("GET", `${base()}/connectors/${id}/event-batches${query}`));
const send = (id: string, pieces: unknown[], clientToken?: string) =>
  sendPieces(id, { pieces, ...(clientToken ? { clientToken } : {}) }) as {
    status: number;
    body: any;
  };
const sent = (id: string, pieces: unknown[], clientToken?: string) =>
  ok(send(id, pieces, clientToken));
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const T0 = "2026-10-18T12:00:00";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(`${T0}Z`));
  resetState();
  // Globex: a Free workspace Ada administers.
  db.workspaces.set(GLOBEX, { id: GLOBEX, slug: "globex" });
  db.memberships.push({
    id: "m",
    workspaceId: GLOBEX,
    userId: "usr_ada",
    role: "WORKSPACE_ROLE_ADMIN",
    createdAt: "",
  });
});
afterEach(() => vi.useRealTimers());

describe("an API source in the Data routes", () => {
  it("is a kind on every plan, made with a description and no credential", async () => {
    for (const ws of [ACME, GLOBEX])
      expect(
        ok(await call("GET", `${base(ws)}/connector-kinds`)).kinds.at(-1),
      ).toEqual({
        kind: "events",
        displayName: "API source",
        requiresPro: false,
        available: true,
      });
    const made = ok(
      await call("POST", `${base(GLOBEX)}/connectors`, {
        name: " Orders ",
        // Fields are learned from pieces, never taken from a request.
        config: {
          events: {
            description: "  One order line  ",
            fields: [{ name: "x", type: "string" }],
          },
        },
      }),
    );
    expect(made.connector).toMatchObject({
      kind: "events",
      name: "Orders",
      config: { events: { description: "One order line", fields: [] } },
      secretHint: "",
      tables: [],
      lastSync: null,
    });
    // Discovery has nothing to dial: the one table, keyed by piece id.
    expect(made.discovery.tables).toEqual([
      expect.objectContaining({
        name: "events",
        primaryKey: ["id"],
        columns: [
          { name: "id", type: "string", nullable: false },
          { name: "received_at", type: "string", nullable: false },
          { name: "occurred_at", type: "string", nullable: false },
        ],
      }),
    ]);
  });

  it("refuses a long description, a taken name and a 21st source", async () => {
    expect(
      refusal(
        await call("POST", `${base()}/connectors`, {
          name: "Orders",
          config: { events: { description: "é".repeat(1001) } },
        }),
      ),
    ).toMatchObject({
      status: 400,
      message:
        "a description holds at most 1000 characters; this one holds 1001",
    });
    await create("Orders", "é".repeat(1000));
    const taken = await create("Refunds");
    expect(
      refusal(
        await call("POST", `${base()}/connectors`, {
          name: "Refunds",
          config: { events: {} },
        }),
      ),
    ).toMatchObject({
      status: 409,
      message: `the API source ${taken.id} already has this name; choose another, or send to that source`,
    });
    // Names compare as the WFE's column compares them, blind to case and
    // accents.
    for (const name of ["refunds", "REFUNDS", "Réfunds"])
      expect(
        refusal(
          await call("POST", `${base()}/connectors`, {
            name,
            config: { events: {} },
          }),
        ),
      ).toMatchObject({
        status: 409,
        message: `the API source ${taken.id} already has this name; choose another, or send to that source`,
      });
    // Another kind may share the name.
    expect(
      (
        await call("POST", `${base()}/connectors`, {
          name: "Stripe",
          config: { events: {} },
        })
      ).status,
    ).toBe(200);
    for (let i = 3; i < 20; i++) await create(`Source ${i}`);
    expect(
      refusal(
        await call("POST", `${base()}/connectors`, {
          name: "One too many",
          config: { events: {} },
        }),
      ),
    ).toMatchObject({
      status: 429,
      message: "a workspace has at most 20 API sources: delete one first",
    });
    // A member reads but doesn't make one.
    expect(
      (
        await call(
          "POST",
          `${base()}/connectors`,
          { name: "Mine", config: { events: {} } },
          "usr_alan",
        )
      ).status,
    ).toBe(403);
  });

  it("edits the name and description alone, keeping the fields, even mid-sync", async () => {
    const c = await create();
    const other = await create("Refunds");
    sent(c.id, [{ id: "o-1", data: { amount: 1 } }]);
    expect((await connector(c.id)).lastSync.status).toBe(
      "CONNECTOR_SYNC_STATUS_QUEUED",
    );
    const edited = ok(
      await call("PATCH", `${base()}/connectors/${c.id}`, {
        name: "Order lines",
        config: { events: { description: "A line", fields: [] } },
      }),
    );
    expect(edited.connector).toMatchObject({
      name: "Order lines",
      config: {
        events: {
          description: "A line",
          fields: [{ name: "amount", type: "number" }],
        },
      },
    });
    expect(edited.discovery).toBeNull();
    // Nothing is tested or queued: the one sync the send queued runs.
    later(1000);
    const syncs = ok(
      await call("GET", `${base()}/connectors/${c.id}/syncs`),
    ).syncs;
    expect(syncs.map((s: any) => s.status)).toEqual([
      "CONNECTOR_SYNC_STATUS_SUCCEEDED",
    ]);
    expect(
      refusal(
        await call("PATCH", `${base()}/connectors/${c.id}`, {
          name: "Refunds",
        }),
      ),
    ).toMatchObject({
      status: 409,
      message: `the API source ${other.id} already has this name; choose another`,
    });
    expect(
      refusal(
        await call("PATCH", `${base()}/connectors/${c.id}`, {
          name: "refunds",
        }),
      ).message,
    ).toBe(`the API source ${other.id} already has this name; choose another`);
    // Keeping its own name, in any case, is no clash.
    for (const name of ["Order lines", "Order Lines"])
      expect(
        (await call("PATCH", `${base()}/connectors/${c.id}`, { name })).status,
      ).toBe(200);
    expect(
      refusal(
        await call("PATCH", `${base()}/connectors/${c.id}`, {
          config: { stripe: { apiKey: "sk_test_x" } },
        }),
      ).message,
    ).toBe("a connector's kind cannot change; create a new connector");
  });

  it("syncs only once pieces arrive", async () => {
    const c = await create();
    expect(
      refusal(await call("POST", `${base()}/connectors/${c.id}:sync`, {})),
    ).toMatchObject({
      status: 400,
      message: "an API source syncs when pieces arrive; send some first",
    });
    sent(c.id, [{ id: "o-1", data: { amount: 1 } }]);
    expect(
      (await call("POST", `${base()}/connectors/${c.id}:sync`, {})).status,
    ).toBe(409);
    later(1000);
    // A sync asked for again merges nothing new.
    ok(await call("POST", `${base()}/connectors/${c.id}:sync`, {}));
    later(1000);
    expect((await connector(c.id)).tables[0].rowCount).toBe("1");
  });

  it("goes with what was sent to it once deleted", async () => {
    const c = await create();
    sent(c.id, [{ id: "o-1", data: {} }]);
    expect((await call("DELETE", `${base()}/connectors/${c.id}`)).status).toBe(
      400,
    );
    later(1000);
    ok(await call("DELETE", `${base()}/connectors/${c.id}`));
    expect(send(c.id, [{ id: "o-2", data: {} }]).status).toBe(404);
    expect(
      (await call("GET", `${base()}/connectors/${c.id}/tokens`)).status,
    ).toBe(404);
    // Its name is free again, and the new source starts empty.
    const again = await create();
    expect((await batches(again.id)).batches).toEqual([]);
  });
});

describe("sending pieces", () => {
  it("stores a send as a batch and syncs it into the events table about a second later", async () => {
    const c = await create();
    const answer = sent(c.id, [
      { id: "o-1", data: { amount: 12.5, customer: "c_1" } },
      {
        id: "o-2",
        occurredAt: "2026-10-01T10:00:00+02:00",
        data: { amount: 3 },
      },
    ]);
    expect(answer).toEqual({
      batch: {
        id: expect.any(String),
        seq: "1",
        pieces: 2,
        bytes: expect.any(String),
        firstAt: "2026-10-01T08:00:00Z",
        lastAt: `${T0}Z`,
        createdAt: `${T0}Z`,
        synced: false,
      },
      replayed: false,
      newFields: [
        { name: "amount", type: "number" },
        { name: "customer", type: "string" },
      ],
      capacity: {
        rows: "2",
        maxRows: "2000000",
        bytes: expect.any(String),
        maxBytes: String(2 * 2 ** 30),
      },
      warning: "",
    });
    // Its fields show at once; its table once its sync ends.
    const before = await connector(c.id);
    expect(before.config.events.fields).toEqual(answer.newFields);
    expect(before.tables).toEqual([]);
    expect((await batches(c.id)).pendingPieces).toBe("2");
    later(999);
    expect((await connector(c.id)).lastSync.status).toBe(
      "CONNECTOR_SYNC_STATUS_RUNNING",
    );
    later(1);
    const after = await connector(c.id);
    expect(after.lastSync).toMatchObject({
      status: "CONNECTOR_SYNC_STATUS_SUCCEEDED",
      rowsSynced: "2",
    });
    expect(after.tables).toEqual([
      {
        name: "events",
        rowCount: "2",
        syncedAt: "2026-10-18T12:00:01.000Z",
        syncId: after.lastSync.id,
        columns: [
          { name: "id", type: "string", nullable: false },
          { name: "received_at", type: "string", nullable: false },
          { name: "occurred_at", type: "string", nullable: false },
          { name: "amount", type: "number", nullable: false },
          { name: "customer", type: "string", nullable: false },
        ],
        primaryKey: ["id"],
        foreignKeys: [],
      },
    ]);
    const listed = await batches(c.id);
    expect(listed.pendingPieces).toBe("0");
    expect(listed.batches.map((b: any) => [b.seq, b.synced])).toEqual([
      ["1", true],
    ]);
  });

  it("pads rows to fields learned later and replaces a piece by id, never losing or doubling one", async () => {
    Object.assign(config, { eventSyncMs: 300 });
    const c = await create();
    sent(c.id, [{ id: "o-1", data: { b: 1 } }]);
    later(300);
    sent(c.id, [{ id: "o-2", data: { a: "x" } }]);
    // A send while its sync is queued joins that sync.
    sent(c.id, [{ id: "o-3", data: {} }]);
    later(300);
    const rows = [
      // Padded: the field learned later goes at the end.
      `{"id":"o-1","received_at":"${T0}.000000Z","occurred_at":null,"b":1,"a":null}`,
      `{"id":"o-2","received_at":"2026-10-18T12:00:00.300000Z","occurred_at":null,"a":"x","b":null}`,
      `{"id":"o-3","received_at":"2026-10-18T12:00:00.300000Z","occurred_at":null,"a":null,"b":null}`,
    ];
    const size = (rs: string[]) => rs.reduce((n, r) => n + r.length + 1, 0);
    let listed = await batches(c.id);
    expect(listed.capacity).toEqual({
      rows: "3",
      maxRows: "2000000",
      bytes: String(size(rows)),
      maxBytes: String(2 * 2 ** 30),
    });
    expect(listed.batches.map((b: any) => [b.seq, b.synced])).toEqual([
      ["3", true],
      ["2", true],
      ["1", true],
    ]);
    // The same id again replaces its row.
    sent(c.id, [{ id: "o-1", data: { b: 2 } }]);
    later(300);
    expect((await connector(c.id)).tables[0].rowCount).toBe("3");
    rows[0] = `{"id":"o-1","received_at":"2026-10-18T12:00:00.600000Z","occurred_at":null,"a":null,"b":2}`;
    listed = await batches(c.id);
    expect(listed.capacity.bytes).toBe(String(size(rows)));
    expect(
      (await connector(c.id)).tables[0].columns.map((x: any) => x.name),
    ).toEqual(["id", "received_at", "occurred_at", "a", "b"]);
  });

  it("keeps each field's first type, and stores nothing of a refused send", async () => {
    const c = await create();
    sent(c.id, [{ id: "o-1", data: { amount: 1, note: null } }]);
    expect(
      refusal(send(c.id, [{ id: "o-2", data: { amount: "12" } }])),
    ).toEqual({
      status: 400,
      message:
        'piece 1 (id "o-2"): "amount" is a string; this source holds "amount" as a number',
      reason: undefined,
    });
    // A field only ever null takes its first type.
    expect(sent(c.id, [{ id: "o-3", data: { note: "hi" } }]).newFields).toEqual(
      [],
    );
    expect((await connector(c.id)).config.events.fields).toEqual([
      { name: "amount", type: "number" },
      { name: "note", type: "string" },
    ]);
    expect(
      refusal(send(c.id, [{ id: "o-4", data: { note: 1 } }])).message,
    ).toBe(
      'piece 1 (id "o-4"): "note" is a number; this source holds "note" as a string',
    );
    expect((await batches(c.id)).batches.map((b: any) => b.seq)).toEqual([
      "2",
      "1",
    ]);
  });

  it("refuses what the envelope rules refuse, before it looks the source up", async () => {
    const c = await create();
    expect(refusal(send("conn_nope", []))).toMatchObject({
      status: 400,
      message: "a send holds 1 to 500 pieces; this one holds none",
    });
    for (const [pieces, message] of [
      [
        [{ id: "a b", data: {} }],
        "piece 1: id must be 1 to 128 printable ASCII characters with no spaces",
      ],
      [
        [{ id: "o-1", data: [1] }],
        'piece 1 (id "o-1"): data is not a JSON object',
      ],
      [[{ id: "o-1" }], 'piece 1 (id "o-1"): data is not valid JSON'],
      [
        [{ id: "o-1", data: { id: 1 } }],
        'piece 1 (id "o-1"): data may not have a top-level id; send it as the piece\'s id',
      ],
      [
        [{ id: "o-1", data: { "a`b": 1 } }],
        'piece 1 (id "o-1"): data has a top-level key with a backtick',
      ],
      [
        [
          { id: "o-1", data: {} },
          { id: "o-2", occurredAt: "yesterday", data: {} },
        ],
        'piece 2 (id "o-2"): occurred_at is not an RFC 3339 time',
      ],
    ] as const)
      expect(refusal(send(c.id, [...pieces])).message).toBe(message);
    expect(refusal(send("conn_nope", [{ id: "o-1", data: {} }]))).toMatchObject(
      {
        status: 404,
        message: "connector not found",
      },
    );
    expect(refusal(send(STRIPE, [{ id: "o-1", data: {} }]))).toEqual({
      status: 400,
      message: `connector ${STRIPE} is not an API source`,
      reason: "FOUNDRY_NOT_EVENT_SOURCE",
    });
    expect(await batches(c.id)).toMatchObject({
      batches: [],
      pendingPieces: "0",
    });
  });

  it("returns a retried send with its client_token and stores nothing, and refuses the token for other pieces", async () => {
    const c = await create();
    const token = "4f1c2a9e-0d6b-4c55-9f0e-6a1b2c3d4e5f";
    const pieces = [{ id: "o-1", data: { amount: 1 } }];
    const first = sent(c.id, pieces, token);
    later(5000);
    // Even once the source is full, and after its sync.
    Object.assign(config, { eventCaps: { maxRows: 1, maxBytes: 1 << 20 } });
    const retry = sent(c.id, pieces, token);
    expect(retry).toMatchObject({
      replayed: true,
      newFields: [],
      batch: { ...first.batch, synced: true },
      capacity: { rows: "1", maxRows: "1" },
      warning:
        "This source is 100% full (1 of 1 pieces). Once it is full, sends are refused: send new pieces to a new source before then.",
    });
    expect(
      refusal(send(c.id, [{ id: "o-1", data: { amount: 2 } }], token)),
    ).toMatchObject({
      status: 400,
      message: `client_token "${token}" was already used for a different send; use a new client_token for new pieces`,
    });
    expect(
      refusal(
        send(
          c.id,
          [
            { id: "o-1", data: { amount: 1 } },
            { id: "o-2", data: {} },
          ],
          token,
        ),
      ).message,
    ).toContain("was already used for a different send");
    for (const bad of ["short", "x".repeat(65), "with a space in it!!"])
      expect(refusal(send(c.id, pieces, bad)).message).toBe(
        "client_token must be 16 to 64 printable ASCII characters with no spaces: a new random value, such as a UUID, for each send",
      );
    expect((await batches(c.id)).batches).toHaveLength(1);
    // A send without a token stores again, and merges to the same row.
    Object.assign(config, { eventCaps: undefined });
    sent(c.id, pieces);
    later(1000);
    expect((await connector(c.id)).tables[0].rowCount).toBe("1");
  });

  it("refuses a send past either cap, and warns from 80% full", async () => {
    const c = await create();
    Object.assign(config, { eventCaps: { maxRows: 5, maxBytes: 1 << 20 } });
    expect(
      sent(
        c.id,
        [1, 2, 3, 4].map((n) => ({ id: `o-${n}`, data: {} })),
      ).warning,
    ).toBe(
      "This source is 80% full (4 of 5 pieces). Once it is full, sends are refused: send new pieces to a new source before then.",
    );
    expect(
      refusal(
        send(
          c.id,
          [5, 6].map((n) => ({ id: `o-${n}`, data: {} })),
        ),
      ),
    ).toEqual({
      status: 400,
      message:
        "This source holds 4 pieces, and one source holds at most 5: this send's 2 would pass that. Send to a new source.",
      reason: "FOUNDRY_EVENT_SOURCE_FULL",
    });
    // A line of 72 bytes, and a row of 19 more at most for {"a":…}.
    const small = await create("Small");
    Object.assign(config, { eventCaps: { maxRows: 5, maxBytes: 90 } });
    expect(refusal(send(small.id, [{ id: "o-1", data: { a: 1 } }]))).toEqual({
      status: 400,
      message:
        "This source's table would hold 91 bytes with this send, and one source holds at most 90. Send to a new source.",
      reason: "FOUNDRY_EVENT_SOURCE_FULL",
    });
    Object.assign(config, { eventCaps: { maxRows: 5, maxBytes: 91 } });
    expect(sent(small.id, [{ id: "o-1", data: { a: 1 } }]).capacity).toEqual({
      rows: "1",
      maxRows: "5",
      bytes: "91",
      maxBytes: "91",
    });
  });

  it("refuses fields past the 200 a source may have", async () => {
    const c = await create();
    const fields = (from: number, n: number) =>
      Object.fromEntries(
        Array.from({ length: n }, (_, i) => [`f${from + i}`, 1]),
      );
    sent(c.id, [{ id: "o-1", data: fields(1, 200) }]);
    expect(refusal(send(c.id, [{ id: "o-2", data: fields(200, 2) }]))).toEqual({
      status: 400,
      message:
        "this source has 200 fields and this send adds 1, past the 200 a source may have: send pieces without new fields, or make another source",
      reason: "FOUNDRY_EVENT_SOURCE_FIELDS",
    });
    sent(c.id, [{ id: "o-3", data: fields(1, 3) }]);
  });
});

describe("send tokens", () => {
  const tokens = (id: string, ws = ACME) =>
    `${base(ws)}/connectors/${id}/tokens`;

  it("are made once, by owners and admins, as hx_ev_ and 64 hex digits", async () => {
    const c = await create();
    const made = ok(await call("POST", tokens(c.id), {}));
    expect(made.secret).toMatch(/^hx_ev_[0-9a-f]{64}$/);
    expect(made.token).toEqual({
      id: expect.any(String),
      connectorId: c.id,
      prefix: made.secret.slice(0, 12),
      createdByUserId: "usr_ada",
      createdAt: `${T0}.000Z`,
      revokedAt: null,
      lastUsedAt: null,
    });
    // Only made once: no other answer carries it.
    const listed = ok(await call("GET", tokens(c.id), undefined, "usr_alan"));
    expect(listed.tokens).toEqual([made.token]);
    expect(JSON.stringify(listed)).not.toContain(made.secret);
    expect(JSON.stringify(await connector(c.id))).not.toContain(made.secret);
    // Members read them, and make or revoke none.
    expect((await call("POST", tokens(c.id), {}, "usr_alan")).status).toBe(403);
    expect(
      (
        await call(
          "POST",
          `${tokens(c.id)}/${made.token.id}:revoke`,
          {},
          "usr_alan",
        )
      ).status,
    ).toBe(403);
    // The generator is seeded: the same tokens after a reset.
    const second = ok(await call("POST", tokens(c.id), {})).secret;
    expect(second).not.toBe(made.secret);
    resetState();
    const again = await create();
    expect(ok(await call("POST", tokens(again.id), {})).secret).toBe(
      made.secret,
    );
  });

  it("are at most 10 live, listed newest first with revoked ones, at most 50", async () => {
    const c = await create();
    const ids: string[] = [];
    for (let i = 0; i < 10; i++) {
      ids.push(ok(await call("POST", tokens(c.id), {})).token.id);
      later(1);
    }
    expect(refusal(await call("POST", tokens(c.id), {}))).toMatchObject({
      status: 429,
      message:
        "an API source has at most 10 live send tokens: revoke one first",
    });
    const revoked = ok(
      await call("POST", `${tokens(c.id)}/${ids[0]}:revoke`, {}),
    ).token;
    expect(revoked.revokedAt).toBe(`${T0}.010Z`);
    // Twice is no error, and keeps the first time.
    later(1000);
    expect(
      ok(await call("POST", `${tokens(c.id)}/${ids[0]}:revoke`, {})).token,
    ).toEqual(revoked);
    ids.push(ok(await call("POST", tokens(c.id), {})).token.id);
    const listed = ok(await call("GET", tokens(c.id))).tokens;
    expect(listed.map((t: any) => t.id)).toEqual([...ids].reverse());
    // Sixty made in all: the list holds the newest fifty.
    for (let made = ids.length; made < 60; made++) {
      const live = ok(await call("GET", tokens(c.id))).tokens.filter(
        (t: any) => !t.revokedAt,
      );
      if (live.length === 10)
        for (const t of live)
          ok(await call("POST", `${tokens(c.id)}/${t.id}:revoke`, {}));
      ids.push(ok(await call("POST", tokens(c.id), {})).token.id);
    }
    expect(
      ok(await call("GET", tokens(c.id))).tokens.map((t: any) => t.id),
    ).toEqual(ids.slice(-50).reverse());
  });

  it("belong to one API source of one workspace", async () => {
    const c = await create(),
      other = await create("Refunds");
    const t = ok(await call("POST", tokens(c.id), {})).token;
    expect(
      refusal(await call("POST", `${tokens(other.id)}/${t.id}:revoke`, {})),
    ).toMatchObject({ status: 404, message: "send token not found" });
    expect(ok(await call("GET", tokens(other.id))).tokens).toEqual([]);
    // Another workspace's path doesn't reach it.
    expect((await call("GET", tokens(c.id, GLOBEX))).status).toBe(404);
    expect((await call("GET", tokens(c.id, "ws_nope"))).status).toBe(403);
    expect(refusal(await call("POST", tokens(STRIPE), {}))).toEqual({
      status: 400,
      message: `connector ${STRIPE} is not an API source`,
      reason: "FOUNDRY_NOT_EVENT_SOURCE",
    });
    expect(
      refusal(await call("GET", `${base()}/connectors/${STRIPE}/event-batches`))
        .reason,
    ).toBe("FOUNDRY_NOT_EVENT_SOURCE");
  });
});

describe("recent sends", () => {
  it("lists 20 by default, at most 100, newest first", async () => {
    const c = await create();
    for (let i = 1; i <= 105; i++) sent(c.id, [{ id: `o-${i}`, data: {} }]);
    const seqs = async (query: string) =>
      (await batches(c.id, query)).batches.map((b: any) => Number(b.seq));
    expect(await seqs("")).toEqual(
      Array.from({ length: 20 }, (_, i) => 105 - i),
    );
    expect(await seqs("?limit=3")).toEqual([105, 104, 103]);
    expect(await seqs("?limit=500")).toHaveLength(100);
    expect((await batches(c.id)).pendingPieces).toBe("105");
    // Members read them.
    expect(
      (
        await call(
          "GET",
          `${base()}/connectors/${c.id}/event-batches`,
          undefined,
          "usr_alan",
        )
      ).status,
    ).toBe(200);
  });
});

describe("generating from an API source", () => {
  const O = `${base()}/ontologies/ont_acme_contracts`;
  const synced = async (name = "Orders") => {
    const c = await create(name);
    sent(c.id, [{ id: "o-1", data: { amount: 1 } }]);
    later(1000);
    return c;
  };
  const structured = (connectorId: string) => ({
    connectorId,
    mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
  });

  it("reads a structured source's table beside the connectors chosen", async () => {
    const c = await synced();
    const g = ok(
      await call("POST", `${O}:generate`, {
        connectorIds: [STRIPE],
        eventSources: [structured(c.id)],
      }),
    ).generation;
    expect(g.tables).toContainEqual({ connectorId: c.id, table: "events" });
    expect(g.tables.filter((t: any) => t.connectorId === STRIPE).length).toBe(
      (await connector(STRIPE)).tables.length,
    );
    // The draft reads it.
    later(10_000);
    expect(
      ok(await call("GET", `${O}/generations/${g.id}`)).generation.status,
    ).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
  });

  it("drafts from API sources alone with noConnectors, and never from one by default", async () => {
    const c = await synced();
    const alone = ok(
      await call("POST", `${O}:generate`, {
        noConnectors: true,
        eventSources: [structured(c.id)],
      }),
    ).generation;
    expect(alone.tables).toEqual([{ connectorId: c.id, table: "events" }]);
    later(10_000);
    const every = ok(await call("POST", `${O}:generate`, {})).generation;
    expect(every.tables.length).toBeGreaterThan(0);
    expect(every.tables.some((t: any) => t.connectorId === c.id)).toBe(false);
  });

  it("refuses each wrong pick with the backend's message", async () => {
    const c = await synced(),
      empty = await create("Empty");
    const generate = async (body: object) =>
      refusal(await call("POST", `${O}:generate`, body));
    expect(await generate({ connectorIds: [c.id] })).toMatchObject({
      status: 400,
      message: `connector ${c.id} is an API source: choose it in event_sources, with a mode`,
    });
    expect(
      await generate({ eventSources: [structured("conn_nope")] }),
    ).toMatchObject({ status: 404, message: "connector conn_nope not found" });
    expect(
      await generate({ eventSources: [structured(STRIPE)] }),
    ).toMatchObject({
      status: 400,
      message: `connector ${STRIPE} is not an API source: choose it in connector_ids`,
    });
    expect(
      await generate({ eventSources: [structured(c.id), structured(c.id)] }),
    ).toMatchObject({
      status: 400,
      message: `event_sources names the API source ${c.id} twice`,
    });
    expect(
      await generate({
        eventSources: [
          { connectorId: c.id, mode: "FOUNDRY_EVENT_SOURCE_MODE_UNSPECIFIED" },
        ],
      }),
    ).toMatchObject({
      status: 400,
      message: `choose how this ontology reads the API source ${c.id}: structured mappings or entity extraction`,
    });
    expect(
      await generate({
        eventSources: [
          { connectorId: c.id, mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION" },
        ],
      }),
    ).toMatchObject({
      status: 400,
      message:
        'the API source "Orders" has no text field: choose the field that holds each piece\'s text on its page in Foundry, then wait for its next sync',
    });
    const noPieces = {
      status: 400,
      message:
        'the API source "Empty" has no pieces in its table yet: send some, and wait for its first sync (about a minute)',
    };
    expect(
      await generate({ eventSources: [structured(empty.id)] }),
    ).toMatchObject(noPieces);
    // Pieces sent but not synced yet are not in its table.
    sent(empty.id, [{ id: "o-1", data: {} }]);
    expect(
      await generate({ eventSources: [structured(empty.id)] }),
    ).toMatchObject(noPieces);
  });
});

// ---------------------------------------------------------------- Extraction

const reply = (id: string, text: string, extra: object = {}) => ({
  id,
  data: { reply: text, ...extra },
});
const sha = (text: string) => createHash("sha256").update(text).digest("hex");
/** An API source made with a text field (and pieces sent by `reply`). */
const replies = async (name = "Replies", events: object = {}) =>
  ok(
    await call("POST", `${base()}/connectors`, {
      name,
      config: {
        events: {
          description: "A support reply",
          textField: "reply",
          ...events,
        },
      },
    }),
  ).connector;
const patch = (id: string, events: object) =>
  call("PATCH", `${base()}/connectors/${id}`, { config: { events } });
const syncsOf = async (id: string) =>
  ok(await call("GET", `${base()}/connectors/${id}/syncs`)).syncs as {
    status: string;
  }[];
const setsOf = async (ws = ACME): Promise<FoundryDocumentSet[]> =>
  ok(await call("GET", `${base(ws)}/document-sets`)).sets;
const piecesSet = async (connectorId: string, ws = ACME) =>
  (await setsOf(ws)).find((s) => s.connector?.connectorId === connectorId);
/** Every live document of the set, newest first, all pages. */
async function documentsOf(setId: string): Promise<FoundryDocument[]> {
  const all: FoundryDocument[] = [];
  for (let token = ""; ; ) {
    const page = ok(
      await call(
        "GET",
        `${base()}/documents?setId=${setId}${token ? `&pageToken=${token}` : ""}`,
      ),
    );
    all.push(...page.documents);
    if (!page.nextPageToken) return all;
    token = page.nextPageToken;
  }
}
const byIds = async (ids: string[]): Promise<FoundryDocument[]> =>
  ok(
    await call(
      "GET",
      `${base()}/documents?${ids.map((id) => `ids=${id}`).join("&")}`,
    ),
  ).documents;
const content = async (documentId: string): Promise<string> =>
  ok(await call("GET", `${base()}/documents/${documentId}:content`)).text;
/** The set's documents by piece id. */
const byPiece = (docs: FoundryDocument[]) =>
  new Map(docs.map((d) => [d.source!.externalId!, d]));

describe("an API source's text field", () => {
  it("is taken at create and locked as a string, so later pieces must send one", async () => {
    const c = await replies();
    expect(c.config).toEqual({
      events: {
        description: "A support reply",
        fields: [{ name: "reply", type: "string" }],
        textField: "reply",
      },
    });
    expect((await connector(c.id)).config.events.textField).toBe("reply");
    expect(refusal(send(c.id, [{ id: "o-1", data: { reply: 5 } }]))).toEqual({
      status: 400,
      message:
        'piece 1 (id "o-1"): "reply" is a number; this source holds "reply" as a string',
      reason: undefined,
    });
    sent(c.id, [{ id: "o-2", data: { reply: null } }]);
    // A source made without one has none.
    expect((await create()).config.events.textField).toBe("");
  });

  it("refuses a name a field can't have at create, and makes nothing", async () => {
    for (const [textField, message] of [
      ["id", "id is a column of every piece, not a field of its data"],
      ["a`b", "a field name has no backticks"],
      ["x".repeat(65), "a field name is 1 to 64 bytes; this one is 65"],
    ])
      expect(
        refusal(
          await call("POST", `${base()}/connectors`, {
            name: "Replies",
            config: { events: { textField } },
          }),
        ),
      ).toEqual({ status: 400, message, reason: undefined });
    const listed = ok(await call("GET", `${base()}/connectors`)).connectors;
    expect(listed.some((x: any) => x.kind === "events")).toBe(false);
  });

  it("stays when an update leaves it out, and a description alone queues nothing", async () => {
    const c = await replies();
    sent(c.id, [reply("t-1", "Hi")]);
    later(1000);
    const set = await piecesSet(c.id);
    const before = await documentsOf(set!.id);
    expect(await syncsOf(c.id)).toHaveLength(1);
    expect(
      ok(await patch(c.id, { description: "A reply we sent" })).connector.config
        .events,
    ).toEqual({
      description: "A reply we sent",
      fields: [{ name: "reply", type: "string" }],
      textField: "reply",
    });
    // An update that sets neither, nor a name, is refused, as the backend's
    // updateEventSource refuses it; a name alone is one.
    for (const body of [
      { config: { events: {} } },
      { config: { events: { description: null, textField: null } } },
      { config: null },
      {},
    ])
      expect(
        refusal(await call("PATCH", `${base()}/connectors/${c.id}`, body)),
      ).toEqual({
        status: 400,
        message: "set a name, description or text field to update",
        reason: undefined,
      });
    expect(
      ok(
        await call("PATCH", `${base()}/connectors/${c.id}`, {
          name: "Replies sent",
          config: { events: {} },
        }),
      ).connector,
    ).toMatchObject({
      name: "Replies sent",
      config: {
        events: { description: "A reply we sent", textField: "reply" },
      },
    });
    expect(await syncsOf(c.id)).toHaveLength(1);
    expect(await documentsOf(set!.id)).toEqual(before);
    // Its own value again is no change, even mid-sync.
    sent(c.id, [reply("t-2", "Hello")]);
    expect(
      ok(await patch(c.id, { textField: "reply", description: "Mid-sync" }))
        .connector.config.events,
    ).toMatchObject({ description: "Mid-sync", textField: "reply" });
    expect(await syncsOf(c.id)).toHaveLength(2);
  });

  it("changed, queues one sync that re-renders the newest pieces, and is refused while a sync is live", async () => {
    const c = await replies();
    sent(c.id, [
      reply("t-1", "Hi", { body: "Long form" }),
      reply("t-2", "Hello"),
    ]);
    // The send's sync is queued: a change waits for it, and nothing is
    // written.
    expect(refusal(await patch(c.id, { textField: "body" }))).toEqual({
      status: 400,
      message:
        "a sync of this API source is running; change its text field in a minute",
      reason: undefined,
    });
    later(500);
    expect((await connector(c.id)).lastSync.status).toBe(
      "CONNECTOR_SYNC_STATUS_RUNNING",
    );
    expect((await patch(c.id, { textField: "body" })).status).toBe(400);
    later(500);
    const set = (await piecesSet(c.id))!;
    const before = byPiece(await documentsOf(set.id));
    expect((await connector(c.id)).config.events).toMatchObject({
      textField: "reply",
      description: "A support reply",
    });

    const changed = ok(await patch(c.id, { textField: "body" })).connector;
    expect(changed.config.events).toMatchObject({
      description: "A support reply",
      textField: "body",
    });
    const syncs = await syncsOf(c.id);
    expect(syncs.map((s) => s.status)).toEqual([
      "CONNECTOR_SYNC_STATUS_QUEUED",
      "CONNECTOR_SYNC_STATUS_SUCCEEDED",
    ]);
    later(1000);
    // Every piece is rendered again: new text is a new document in its
    // place, and the old one is deleted.
    const after = byPiece(await documentsOf(set.id));
    expect([...after.keys()].sort()).toEqual(["t-1", "t-2"]);
    expect(after.get("t-1")!.name).toBe("Long form");
    expect(after.get("t-2")!.name).toBe("t-2");
    expect(await content(after.get("t-1")!.id)).toBe(
      "# Long form\n\n- Piece: t-1\n- reply: Hi\n\nLong form",
    );
    for (const id of ["t-1", "t-2"])
      expect(after.get(id)!.id).not.toBe(before.get(id)!.id);
    expect(
      (await byIds([...before.values()].map((d) => d.id))).map((d) => d.status),
    ).toEqual(["deleted", "deleted"]);
  });

  it("refuses a field of another type and one past the field cap, with the send's codes, writing nothing", async () => {
    const c = await replies();
    sent(c.id, [reply("t-1", "Hi", { amount: 2, meta: { a: 1 } })]);
    later(1000);
    for (const [field, phrase] of [
      ["amount", "a number"],
      ["meta", "an object"],
    ])
      expect(refusal(await patch(c.id, { textField: field }))).toEqual({
        status: 400,
        message: `a text field holds strings; this source holds "${field}" as ${phrase}`,
        reason: undefined,
      });
    const full = await replies("Full");
    sent(full.id, [
      {
        id: "t-1",
        data: Object.fromEntries(
          Array.from({ length: 199 }, (_, i) => [`f${i + 1}`, 1]),
        ),
      },
    ]);
    later(1000);
    expect(refusal(await patch(full.id, { textField: "f200" }))).toEqual({
      status: 400,
      message:
        "this source has 200 fields and a source may have 200: choose a text field its pieces already carry",
      reason: "FOUNDRY_EVENT_SOURCE_FIELDS",
    });
    expect(refusal(await patch(full.id, { textField: "a`b" })).message).toBe(
      "a field name has no backticks",
    );
    for (const id of [c.id, full.id]) {
      expect((await connector(id)).config.events.textField).toBe("reply");
      expect(await syncsOf(id)).toHaveLength(1);
    }
    expect(
      (await connector(full.id)).config.events.fields.map((f: any) => f.name),
    ).not.toContain("f200");
  });

  it("cleared, queues a sync that empties its set and keeps it", async () => {
    const c = await replies();
    sent(c.id, [reply("t-1", "Hi"), reply("t-2", "Hello")]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    const held = await documentsOf(set.id);
    expect(held).toHaveLength(2);
    expect(
      ok(await patch(c.id, { textField: "" })).connector.config.events,
    ).toMatchObject({
      textField: "",
      // The field stays locked: pieces already sent it as a string.
      fields: [{ name: "reply", type: "string" }],
    });
    expect(await syncsOf(c.id)).toHaveLength(2);
    later(1000);
    expect(await piecesSet(c.id)).toMatchObject({ id: set.id, documents: 0 });
    expect((await byIds(held.map((d) => d.id))).map((d) => d.status)).toEqual([
      "deleted",
      "deleted",
    ]);
    // Its table keeps every piece.
    expect((await connector(c.id)).tables[0].rowCount).toBe("2");
  });
});

describe("an API source's Pieces set", () => {
  it("names each document by its title as the syncer cleans one", async () => {
    const c = await replies();
    sent(c.id, [
      reply("t-1", "  Order 12/3\u0001 shipped\\back  \nmore"),
      reply("t-2", "x".repeat(300)),
      reply("t-3", "/"),
    ]);
    later(1000);
    const docs = byPiece(await documentsOf((await piecesSet(c.id))!.id));
    // Its slashes a "-", its control characters spaces, trimmed; the title
    // is already one line of at most 120 characters.
    expect(docs.get("t-1")!.name).toBe("Order 12-3  shipped-back");
    expect(docs.get("t-2")!.name).toBe("x".repeat(119) + "…");
    expect(docs.get("t-3")!.name).toBe("-");
  });

  it("holds each piece as a Markdown document, from the first sync", async () => {
    const c = await replies();
    sent(c.id, [
      reply("t-1", "Refund sent\nThanks", { customer: "c_1" }),
      { id: "t-2", occurredAt: "2026-10-01T10:00:00+02:00", data: {} },
    ]);
    expect(await piecesSet(c.id)).toBeUndefined();
    later(1000);
    const set = await piecesSet(c.id);
    expect(set).toEqual({
      id: expect.any(String),
      name: "Pieces",
      description: "A support reply",
      documents: 2,
      // Made by the sync, which ends a second after the send.
      createdAt: "2026-10-18T12:00:01.000Z",
      readBy: [],
      connector: {
        connectorId: c.id,
        stream: "pieces",
        maxDocuments: 500,
        connectorName: "Replies",
        connectorKind: "events",
        monthSystemUsdMicros: "0",
        maxMonthSystemUsdMicros: "5000000",
        paused: false,
      },
    });
    const docs = byPiece(await documentsOf(set!.id));
    const text =
      "# Refund sent\n\n- Piece: t-1\n- customer: c_1\n\nRefund sent\nThanks";
    expect(docs.get("t-1")).toMatchObject({
      name: "Refund sent",
      mediaType: "text/markdown",
      status: "uploaded",
      setId: set!.id,
      sha256: sha(text),
      source: {
        externalId: "t-1",
        version: sha(text),
        url: "",
        at: `${T0}.000Z`,
      },
    });
    expect(await content(docs.get("t-1")!.id)).toBe(text);
    expect(await content(docs.get("t-2")!.id)).toBe(
      "# t-2\n\n- Piece: t-2\n- Occurred: 2026-10-01T08:00:00Z\n",
    );
    // The set is the source's: a person can't upload into it.
    expect(
      refusal(
        await call("POST", `${base()}/documents`, {
          name: "x.md",
          mediaType: "text/markdown",
          sizeBytes: "1",
          sha256: sha("x"),
          clientToken: "tok",
          setId: set!.id,
        }),
      ).message,
    ).toBe(
      "this set belongs to the Replies connection; change its settings there",
    );
  });

  it("keeps the newest 500 by receipt at every sync, trimming the oldest, never more", async () => {
    const c = await replies();
    const ids = (prefix: string, n: number) =>
      Array.from(
        { length: n },
        (_, i) => `${prefix}-${String(i + 1).padStart(3, "0")}`,
      );
    sent(
      c.id,
      ids("p", 500).map((id) => reply(id, `Reply ${id}`)),
    );
    later(1000);
    const set = (await piecesSet(c.id))!;
    const first = await documentsOf(set.id);
    expect(first).toHaveLength(500);
    later(10);
    sent(c.id, [reply("q-1", "Later"), reply("q-2", "Later still")]);
    later(1000);
    const after = await documentsOf(set.id);
    expect((await piecesSet(c.id))!.documents).toBe(500);
    expect(after).toHaveLength(500);
    expect(new Set(after.map((d) => d.source!.externalId)).size).toBe(500);
    // Newest first: the two new ones, then the first send's, which ties on
    // its receipt and keeps its lower ids.
    expect(
      after
        .slice(0, 2)
        .map((d) => d.source!.externalId)
        .sort(),
    ).toEqual(["q-1", "q-2"]);
    const kept = new Set(after.map((d) => d.source!.externalId));
    expect(kept.has("p-498")).toBe(true);
    expect(kept.has("p-499") || kept.has("p-500")).toBe(false);
    const trimmed = first.filter(
      (d) =>
        d.source!.externalId === "p-499" || d.source!.externalId === "p-500",
    );
    expect(
      (await byIds(trimmed.map((d) => d.id))).map((d) => d.status),
    ).toEqual(["deleted", "deleted"]);
    // The rest are the same documents, untouched.
    const firstIds = new Map(first.map((d) => [d.source!.externalId, d.id]));
    expect(
      after
        .filter((d) => d.source!.externalId!.startsWith("p-"))
        .every((d) => firstIds.get(d.source!.externalId) === d.id),
    ).toBe(true);
    // Its table keeps every piece.
    expect((await connector(c.id)).tables[0].rowCount).toBe("502");
  });

  it("moves a piece sent again as it was up in place, and replaces one whose text changed", async () => {
    const c = await replies();
    sent(c.id, [reply("a", "A"), reply("b", "B")]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    const first = byPiece(await documentsOf(set.id));

    // The same piece again: the same document, at its new time, newest.
    later(10);
    sent(c.id, [reply("a", "A")]);
    later(1000);
    let docs = await documentsOf(set.id);
    expect(docs.map((d) => d.source!.externalId)).toEqual(["a", "b"]);
    expect(docs[0]).toMatchObject({
      id: first.get("a")!.id,
      source: {
        version: first.get("a")!.source!.version,
        at: "2026-10-18T12:00:01.010Z",
      },
    });

    // New text: a new document in its place, extracted again.
    sent(c.id, [reply("b", "B2")]);
    later(1000);
    docs = await documentsOf(set.id);
    const b = byPiece(docs).get("b")!;
    expect(b.id).not.toBe(first.get("b")!.id);
    expect(await content(b.id)).toBe("# B2\n\n- Piece: b\n\nB2");
    expect((await byIds([first.get("b")!.id]))[0]!.status).toBe("deleted");

    // The same id twice in one sync: the later text.
    sent(c.id, [reply("c", "C1")]);
    sent(c.id, [reply("c", "C2")]);
    later(1000);
    docs = await documentsOf(set.id);
    expect(docs.filter((d) => d.source!.externalId === "c")).toHaveLength(1);
    expect(await content(byPiece(docs).get("c")!.id)).toBe(
      "# C2\n\n- Piece: c\n\nC2",
    );

    // A field learned since pads the other rows with null: their documents
    // stay as they are.
    const held = byPiece(docs);
    sent(c.id, [reply("d", "D", { extra: 1 })]);
    later(1000);
    const padded = byPiece(await documentsOf(set.id));
    for (const id of ["a", "b", "c"])
      expect(padded.get(id)).toEqual(held.get(id));
    expect(padded.size).toBe(4);
  });

  it("pauses while its allowance is spent, and catches up at the next sync with room", async () => {
    const c = await replies();
    sent(c.id, [reply("a", "A")]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    const before = await documentsOf(set.id);

    Object.assign(config, { eventSetAllowanceSpent: true });
    expect((await piecesSet(c.id))!.connector).toMatchObject({
      monthSystemUsdMicros: "5000000",
      maxMonthSystemUsdMicros: "5000000",
      paused: true,
    });
    sent(c.id, [reply("a", "A2"), reply("b", "B")]);
    later(1000);
    // Its table merges as ever; its documents are neither added, changed
    // nor trimmed.
    expect((await connector(c.id)).tables[0].rowCount).toBe("2");
    expect(await documentsOf(set.id)).toEqual(before);

    Object.assign(config, { eventSetAllowanceSpent: false });
    expect((await piecesSet(c.id))!.connector).toMatchObject({
      monthSystemUsdMicros: "0",
      paused: false,
    });
    ok(await call("POST", `${base()}/connectors/${c.id}:sync`, {}));
    later(1000);
    const caught = byPiece(await documentsOf(set.id));
    expect([...caught.keys()].sort()).toEqual(["a", "b"]);
    expect(await content(caught.get("a")!.id)).toBe("# A2\n\n- Piece: a\n\nA2");

    // A text field cleared while paused still empties the set: removing
    // spends nothing.
    Object.assign(config, { eventSetAllowanceSpent: true });
    ok(await patch(c.id, { textField: "" }));
    later(1000);
    expect((await piecesSet(c.id))!.documents).toBe(0);
  });

  it("never pauses a set that holds no documents: its first sync writes them", async () => {
    Object.assign(config, { eventSetAllowanceSpent: true });
    const c = await replies();
    sent(c.id, [reply("a", "A")]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    expect(set.documents).toBe(1);
    // Now it holds one, so the next sync no person started writes nothing.
    expect(set.connector).toMatchObject({ paused: true });
    sent(c.id, [reply("b", "B")]);
    later(1000);
    expect([...byPiece(await documentsOf(set.id)).keys()]).toEqual(["a"]);
    // Emptied by a cleared text field, it is not paused, and the next
    // field's first sync writes again.
    ok(await patch(c.id, { textField: "" }));
    later(1000);
    expect((await piecesSet(c.id))!.connector).toMatchObject({ paused: false });
    ok(await patch(c.id, { textField: "reply" }));
    later(1000);
    expect([...byPiece(await documentsOf(set.id)).keys()].sort()).toEqual([
      "a",
      "b",
    ]);
  });

  it("writes a sync a person started into a paused set: a text field's change re-reads it", async () => {
    const c = await replies();
    sent(c.id, [reply("a", "A", { body: "Body A" })]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    Object.assign(config, { eventSetAllowanceSpent: true });
    expect((await piecesSet(c.id))!.connector).toMatchObject({ paused: true });
    // A send's sync is no one's: paused.
    sent(c.id, [reply("b", "B", { body: "Body B" })]);
    later(1000);
    expect([...byPiece(await documentsOf(set.id)).keys()]).toEqual(["a"]);
    // The change's full sync is the person's: it writes the newest, by the
    // new field, the pieces sent while paused too.
    ok(await patch(c.id, { textField: "body" }));
    later(1000);
    const docs = byPiece(await documentsOf(set.id));
    expect([...docs.keys()].sort()).toEqual(["a", "b"]);
    expect(await content(docs.get("a")!.id)).toContain("Body A");
    expect(await content(docs.get("b")!.id)).toContain("Body B");
    // And the next send's is paused again.
    sent(c.id, [reply("c", "C", { body: "Body C" })]);
    later(1000);
    expect([...byPiece(await documentsOf(set.id)).keys()].sort()).toEqual([
      "a",
      "b",
    ]);
  });

  it("has no allowance for another kind's set", async () => {
    const g = ok(
      await call("POST", `${base()}/connectors`, {
        name: "Team Granola",
        config: { granola: { apiKey: "grn_team1234" } },
      }),
    ).connector;
    ok(await call("POST", `${base()}/connectors/${g.id}:sync`, {}));
    later(3000);
    Object.assign(config, { eventSetAllowanceSpent: true });
    const notes = (await piecesSet(g.id))!;
    expect(notes.connector).toEqual({
      connectorId: g.id,
      stream: "notes",
      maxDocuments: 1000,
      connectorName: "Team Granola",
      connectorKind: "granola",
      monthSystemUsdMicros: "0",
      paused: false,
    });
    // Its syncs are never paused: the note edited before each sync is a new
    // document after it.
    const edited = byPiece(await documentsOf(notes.id)).get("not_mock0003")!;
    ok(await call("POST", `${base()}/connectors/${g.id}:sync`, {}));
    later(3000);
    const after = byPiece(await documentsOf(notes.id));
    expect(after.size).toBe(418);
    expect(after.get("not_mock0003")!.id).not.toBe(edited.id);
  });

  it("stays in its workspace, and goes with its source", async () => {
    const c = await replies();
    sent(c.id, [reply("a", "A")]);
    later(1000);
    const set = (await piecesSet(c.id))!;
    const [doc] = await documentsOf(set.id);
    expect(await piecesSet(c.id, GLOBEX)).toBeUndefined();
    expect(
      ok(await call("GET", `${base(GLOBEX)}/documents?ids=${doc!.id}`))
        .documents,
    ).toEqual([]);
    ok(await call("DELETE", `${base()}/connectors/${c.id}`));
    expect(await piecesSet(c.id)).toBeUndefined();
    expect((await byIds([doc!.id]))[0]!.status).toBe("deleted");
  });
});

describe("generating from an API source by extraction", () => {
  const O = `${base()}/ontologies/ont_acme_contracts`;
  const extraction = (connectorId: string) => ({
    connectorId,
    mode: "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION",
  });
  const generate = (body: object) => call("POST", `${O}:generate`, body);
  /** A source with a text field and a piece in its set. */
  const extractable = async (name = "Replies") => {
    const c = await replies(name);
    sent(c.id, [reply("t-1", "Refund sent")]);
    later(1000);
    return { c, setId: (await piecesSet(c.id))!.id };
  };

  it("reads the source's own set, once, alone with noConnectors", async () => {
    const { c, setId } = await extractable();
    const g = ok(
      await generate({ noConnectors: true, eventSources: [extraction(c.id)] }),
    ).generation;
    expect(g).toMatchObject({ tables: [], documentSetIds: [setId] });
    later(10_000);
    expect(
      ok(await call("GET", `${O}/generations/${g.id}`)).generation.status,
    ).toBe("ONTOLOGY_GENERATION_STATUS_SUCCEEDED");
  });

  it("reads one source by mappings and another by extraction, beside the sets chosen", async () => {
    const { c, setId } = await extractable();
    const orders = await create("Orders");
    sent(orders.id, [{ id: "o-1", data: { amount: 1 } }]);
    later(1000);
    const g = ok(
      await generate({
        noConnectors: true,
        documentSetIds: ["set_acme_general"],
        eventSources: [
          extraction(c.id),
          {
            connectorId: orders.id,
            mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED",
          },
        ],
      }),
    ).generation;
    expect(g.tables).toEqual([{ connectorId: orders.id, table: "events" }]);
    // Oldest set first, as the WFE records them.
    expect(g.documentSetIds).toEqual(["set_acme_general", setId]);
  });

  it("refuses a source with no text field, or no piece as a document yet", async () => {
    const plain = await create("Orders");
    sent(plain.id, [{ id: "o-1", data: { reply: "x" } }]);
    later(1000);
    expect(
      refusal(await generate({ eventSources: [extraction(plain.id)] })),
    ).toEqual({
      status: 400,
      message:
        'the API source "Orders" has no text field: choose the field that holds each piece\'s text on its page in Foundry, then wait for its next sync',
      reason: undefined,
    });
    const noPieces = (name: string) => ({
      status: 400,
      message: `the API source "${name}" has no pieces as documents yet: send some, and wait for its next sync (about a minute)`,
      reason: undefined,
    });
    // Never synced: no set.
    const fresh = await replies("Fresh");
    expect(
      refusal(await generate({ eventSources: [extraction(fresh.id)] })),
    ).toEqual(noPieces("Fresh"));
    // Its pieces sent but not synced yet.
    sent(fresh.id, [reply("t-1", "Hi")]);
    expect(
      refusal(await generate({ eventSources: [extraction(fresh.id)] })),
    ).toEqual(noPieces("Fresh"));
    // A text field chosen before any piece: its sync makes the set, empty.
    const empty = await create("Empty");
    ok(await patch(empty.id, { textField: "reply" }));
    later(1000);
    expect(await piecesSet(empty.id)).toMatchObject({ documents: 0 });
    expect(
      refusal(await generate({ eventSources: [extraction(empty.id)] })),
    ).toEqual(noPieces("Empty"));
    // A piece without the field is still a document, titled by its id.
    ok(await patch(plain.id, { textField: "subject" }));
    later(1000);
    const [doc] = await documentsOf((await piecesSet(plain.id))!.id);
    expect(doc!.name).toBe("o-1");
    ok(await generate({ eventSources: [extraction(plain.id)] }));
  });

  it("refuses the source's set among the sets, a source picked twice, and another workspace's", async () => {
    const { c, setId } = await extractable();
    expect(refusal(await generate({ documentSetIds: [setId] }))).toEqual({
      status: 400,
      message: `document set ${setId} holds the API source ${c.id}'s pieces: choose that source in event_sources with mode extraction`,
      reason: undefined,
    });
    expect(
      refusal(
        await generate({
          documentSetIds: [setId],
          eventSources: [extraction(c.id)],
        }),
      ).message,
    ).toContain("holds the API source");
    expect(
      refusal(
        await generate({
          eventSources: [
            extraction(c.id),
            { connectorId: c.id, mode: "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED" },
          ],
        }),
      ).message,
    ).toBe(`event_sources names the API source ${c.id} twice`);
    const theirs = ok(
      await call("POST", `${base(GLOBEX)}/connectors`, {
        name: "Theirs",
        config: { events: { textField: "reply" } },
      }),
    ).connector;
    expect(
      refusal(await generate({ eventSources: [extraction(theirs.id)] })),
    ).toEqual({
      status: 404,
      message: `connector ${theirs.id} not found`,
      reason: undefined,
    });
  });

  it("counts the source's set among the 20 sets a generation reads", async () => {
    const { c, setId } = await extractable();
    const made: string[] = [];
    for (let i = 1; i <= 19; i++)
      made.push(
        ok(await call("POST", `${base()}/document-sets`, { name: `Set ${i}` }))
          .set.id,
      );
    const all = ["set_acme_general", ...made];
    expect(
      refusal(
        await generate({
          documentSetIds: all,
          eventSources: [extraction(c.id)],
        }),
      ).message,
    ).toBe("a generation reads at most 20 document sets; choose fewer");
    const g = ok(
      await generate({
        documentSetIds: all.slice(1),
        eventSources: [extraction(c.id)],
      }),
    ).generation;
    expect(g.documentSetIds).toHaveLength(20);
    expect(g.documentSetIds.filter((id: string) => id === setId)).toHaveLength(
      1,
    );
  });
});
