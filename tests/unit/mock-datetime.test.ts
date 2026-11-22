import { describe, expect, it } from "vitest";
import { runQuery } from "../../mock/graph/query";
import { HelixDateTime, MockGraph } from "../../mock/graph/store";

// The mock's DateTime and typed parameters, as helix-db a29a8d4 reads them:
// a DateTime is no string, so it equals, orders against and compares with
// DateTimes only, renders as `…mmmZ`, and is made by a load, a `date_time`
// parameter or a `{"date_time": ms}` constant.

const AT = Date.UTC(2026, 2, 14, 8, 30);
const graph = () => {
  const g = new MockGraph();
  g.addNode("T", { at: new HelixDateTime(AT) }); // 0
  g.addNode("T", { at: "2026-03-14T08:30:00Z" }); // 1: text of the instant
  g.addNode("T", { at: new HelixDateTime(AT - 60_000) }); // 2
  g.addNode("T", { at: 5 }); // 3
  g.addNode("T", {}); // 4
  g.addNode("T", { at: "12.5" }); // 5
  return g;
};
const read = (
  root: unknown,
  params?: Record<string, unknown>,
  types?: Record<string, unknown>,
) => ({
  request_type: "read",
  query: {
    read: { entries: [{ query: { name: "r", root } }], returns: ["r"] },
  },
  ...(params ? { parameters: params } : {}),
  ...(types ? { parameter_types: types } : {}),
});
const where = (predicate: unknown) => ({
  values: { input: { nodes_where: { predicate } }, properties: ["$id"] },
});
const ids = (body: unknown) =>
  ((body as { r: { $id: number }[] }).r || []).map((r) => r.$id);
const dt = { constant: { date_time: AT } };
const text = { constant: { string: "2026-03-14T08:30:00Z" } };
const cmp = (op: string, right: unknown) =>
  ids(
    runQuery(
      graph(),
      read(where({ [op]: { left: { property: "at" }, right } })),
    ).body,
  );
const all = { nodes: { reference: "all" } };

describe("mock DateTime", () => {
  it("renders a DateTime as RFC 3339 to the millisecond", () => {
    const answer = runQuery(
      graph(),
      read({ values: { input: all, properties: ["at"] } }),
    );
    expect(answer).toEqual({
      status: 200,
      body: {
        r: [
          { at: "2026-03-14T08:30:00.000Z" },
          { at: "2026-03-14T08:30:00Z" },
          { at: "2026-03-14T08:29:00.000Z" },
          { at: 5 },
          {},
          { at: "12.5" },
        ],
      },
    });
    expect(JSON.stringify(new HelixDateTime(AT))).toBe(
      '"2026-03-14T08:30:00.000Z"',
    );
    expect(String(new HelixDateTime(AT))).toBe("2026-03-14T08:30:00.000Z");
    expect(() => new HelixDateTime(1.5)).toThrow();
  });

  it("parses text as a date_time parameter does, or not at all", () => {
    expect(HelixDateTime.parse("2026-03-14T09:30:00.0009+01:00")?.ms).toBe(AT);
    expect(HelixDateTime.parse("2026-03-14")).toBeUndefined();
  });

  it("equals only a DateTime of its millisecond, never its text", () => {
    expect(cmp("eq", dt)).toEqual([0]);
    expect(cmp("eq", text)).toEqual([1]);
    expect(cmp("eq", { constant: { date_time: AT + 1 } })).toEqual([]);
    // neq holds across kinds, an absent value's null included.
    expect(cmp("neq", dt)).toEqual([1, 2, 3, 4, 5]);
    expect(cmp("neq", text)).toEqual([0, 2, 3, 4, 5]);
    const isIn = (values: unknown[], type: unknown) =>
      ids(
        runQuery(
          graph(),
          read(
            where({
              is_in: { value: { property: "at" }, values: { param: "xs" } },
            }),
            { xs: values },
            { xs: type },
          ),
        ).body,
      );
    expect(isIn(["2026-03-14T08:30:00Z"], { array: "string" })).toEqual([1]);
  });

  it("compares a DateTime with DateTimes only", () => {
    expect(cmp("lt", dt)).toEqual([2]);
    expect(cmp("lte", dt)).toEqual([0, 2]);
    expect(cmp("gt", { constant: { date_time: AT - 1 } })).toEqual([0]);
    expect(cmp("gte", { constant: { date_time: AT - 60_000 } })).toEqual([
      0, 2,
    ]);
    // Text compares with text: a "" bound never reaches a DateTime.
    expect(cmp("gte", { constant: { string: "" } })).toEqual([1, 5]);
    expect(cmp("lt", { constant: { i64: 10 } })).toEqual([3]);
  });

  it("never finds text in a DateTime", () => {
    for (const op of ["contains", "starts_with"])
      expect(
        ids(
          runQuery(
            graph(),
            read(
              where({
                [op]: {
                  value: { property: "at" },
                  [op === "contains" ? "substring" : "prefix"]: {
                    constant: { string: "2026" },
                  },
                },
              }),
            ),
          ).body,
        ),
        op,
      ).toEqual([1]);
  });

  it("takes a date_time constant only as milliseconds", () => {
    for (const ms of ["2026", 1.5])
      expect(
        runQuery(
          graph(),
          read(
            where({
              eq: {
                left: { property: "at" },
                right: { constant: { date_time: ms } },
              },
            }),
          ),
        ),
      ).toMatchObject({ status: 400, body: { error: "invalid_query_json" } });
  });

  it("orders numbers before DateTimes before text, a missing value first", () => {
    const by = (order: string) =>
      ids(
        runQuery(
          graph(),
          read({
            values: {
              input: { order_by: { input: all, property: "at", order } },
              properties: ["$id"],
            },
          }),
        ).body,
      );
    expect(by("asc")).toEqual([4, 3, 2, 0, 5, 1]);
    expect(by("desc")).toEqual([1, 5, 0, 2, 3, 4]);
  });

  it("groups by instant, a DateTime apart from its text", () => {
    expect(
      runQuery(graph(), read({ group_count: { input: all, property: "at" } }))
        .body,
    ).toEqual({
      r: expect.arrayContaining([
        { at: 5, count: 1 },
        { at: "2026-03-14T08:29:00.000Z", count: 1 },
        { at: "2026-03-14T08:30:00.000Z", count: 1 },
        { at: "2026-03-14T08:30:00Z", count: 1 },
        { at: "12.5", count: 1 },
        { at: null, count: 1 },
      ]),
    });
  });

  it("aggregates numbers and numeric text, never a DateTime", () => {
    const agg = (fn: string) =>
      runQuery(
        graph(),
        read({ aggregate_by: { input: all, function: fn, property: "at" } }),
      ).body;
    expect(agg("count")).toEqual({ r: [{ at_Count: 2 }] });
    expect(agg("sum")).toEqual({ r: [{ at_Sum: 17.5 }] });
    expect(agg("min")).toEqual({ r: [{ at_Min: 5 }] });
    expect(agg("max")).toEqual({ r: [{ at_Max: 12.5 }] });
    expect(agg("mean")).toEqual({ r: [{ at_Mean: 8.75 }] });
    // Of none, a min, max or mean is 0.
    const none = (fn: string) =>
      runQuery(
        graph(),
        read({
          aggregate_by: { input: all, function: fn, property: "missing" },
        }),
      ).body;
    expect([none("min"), none("max"), none("mean"), none("count")]).toEqual([
      { r: [{ missing_Min: 0 }] },
      { r: [{ missing_Max: 0 }] },
      { r: [{ missing_Mean: 0 }] },
      { r: [{ missing_Count: 0 }] },
    ]);
    expect(
      runQuery(
        graph(),
        read({ aggregate_by: { input: all, function: "median", property: "at" } }),
      ).status,
    ).toBe(400);
  });
});

describe("mock typed parameters", () => {
  const eqParam = (value: unknown, type?: unknown) =>
    runQuery(
      graph(),
      read(
        where({ eq: { left: { property: "at" }, right: { param: "x" } } }),
        { x: value },
        type === undefined ? undefined : { x: type },
      ),
    );

  it("reads a date_time parameter as a DateTime, in UTC to the millisecond", () => {
    for (const v of [
      "2026-03-14T08:30:00.000Z",
      "2026-03-14T08:30:00Z",
      "2026-03-14T09:30:00+01:00",
      "2026-03-14 08:30:00.000900Z",
    ])
      expect(ids(eqParam(v, "date_time").body), v).toEqual([0]);
  });

  it("keeps an untyped string a string, which never equals a DateTime", () => {
    expect(ids(eqParam("2026-03-14T08:30:00Z").body)).toEqual([1]);
    expect(ids(eqParam("2026-03-14T08:30:00.000Z").body)).toEqual([]);
    expect(ids(eqParam("2026-03-14T08:30:00Z", "string").body)).toEqual([1]);
  });

  it("refuses a value that isn't its declared type, as Helix does", () => {
    for (const [value, type] of [
      ["2026-03-14", "date_time"],
      [" 2026-03-14T08:30:00Z", "date_time"],
      ["9999-12-31T23:30:00-01:00", "date_time"],
      [AT, "date_time"],
      [5, "string"],
      ["5", "i64"],
      [1.5, "i64"],
      ["x", "f64"],
      [1, "bool"],
      ["a", { array: "string" }],
      [[1, "a"], { array: "i64" }],
    ] as const)
      expect(eqParam(value, type), JSON.stringify([value, type])).toEqual({
        status: 400,
        body: { error: "invalid_query_json", msg: expect.any(String) },
      });
    expect(eqParam("2026-03-14", "date_time").body).toEqual({
      error: "invalid_query_json",
      msg: "parameter 'x' expected date_time, but received string",
    });
  });

  it("refuses types the mock doesn't model", () => {
    for (const type of [
      { array: "date_time" },
      { nullable: "date_time" },
      "f32",
      "value",
      { array: "string", extra: 1 },
    ])
      expect(eqParam("2026-03-14T08:30:00Z", type).status, JSON.stringify(type)).toBe(400);
  });

  it("takes every other type it models", () => {
    for (const [value, type] of [
      [true, "bool"],
      [3, "i64"],
      [2.5, "f64"],
      [3, "f64"],
      [["a"], { array: "string" }],
      [[1, 2], { array: "i64" }],
    ] as const)
      expect(eqParam(value, type).status, JSON.stringify(type)).toBe(200);
  });

  it("declares every parameter once parameter_types is sent", () => {
    const named = (
      params: Record<string, unknown>,
      types: Record<string, unknown>,
    ) => runQuery(graph(), read(where({ eq: { left: { property: "at" }, right: dt } }), params, types));
    expect(named({ x: "a" }, {}).body).toEqual({
      error: "invalid_query_json",
      msg: 'parameter schema names do not match values (missing values: [], extra values: ["x"])',
    });
    expect(named({}, { y: "string", z: "i64" }).body).toEqual({
      error: "invalid_query_json",
      msg: 'parameter schema names do not match values (missing values: ["y", "z"], extra values: [])',
    });
    expect(named({ x: "a" }, { x: "string" }).status).toBe(200);
    expect(named({}, {}).status).toBe(200);
  });
});
