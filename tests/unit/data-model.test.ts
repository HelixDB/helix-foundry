import { describe, expect, it } from "vitest";
import type {
  Connector,
  ConnectorSync,
  EventsConnectorConfig,
  FoundryDocumentSet,
  ListFoundryEventBatchesResponse,
} from "@/shared/wfe";
import {
  EVENT_CAPACITY_WARNING,
  EVENT_SET_DEFAULTS,
  KINDS,
  MAX_NAME_BYTES,
  canManage,
  configKind,
  emptyForm,
  eventCapacity,
  eventSourceLine,
  eventSourceState,
  eventSourceWaiting,
  eventsConfig,
  eventsTable,
  formFromConfig,
  formatBytes,
  githubRepos,
  hasCredential,
  hasTableChoice,
  isDatabase,
  isEventSource,
  isKnownKind,
  isLiveSync,
  keyLabel,
  keySummary,
  kindFolders,
  lastSyncedAt,
  parseS3Uri,
  picked,
  piecesSet,
  previewGrid,
  referencesTo,
  relatedTables,
  rowTotal,
  s3PermissionsPolicy,
  setFolders,
  s3TrustPolicy,
  setHolding,
  setSpend,
  sourceSummary,
  suggestedName,
  syncBadge,
  tableSelection,
  textFieldChoices,
  textFieldConfirm,
  textFieldProblem,
  toConfig,
  type ConnectionForm,
} from "@/web/areas/data/model";
import * as rules from "../../mock/event-pieces";

// The Data area's logic: kinds, sync badges, API sources' states,
// capacity, text fields and documents, the credential forms and the configs
// they send, and how synced tables link.

const sync = (s: Partial<ConnectorSync>): ConnectorSync => ({ id: "s", ...s });
const connector = (c: Partial<Connector>): Connector => ({
  id: "c",
  kind: "stripe",
  ...c,
});

describe("kinds and roles", () => {
  it("knows the WFE's fourteen kinds, which are databases and which offer tables", () => {
    expect(KINDS).toEqual([
      "stripe",
      "workos",
      "posthog",
      "github",
      "linear",
      "calcom",
      "granola",
      "slack",
      "notion",
      "google",
      "postgres",
      "mysql",
      "s3",
      "events",
    ]);
    expect(isKnownKind("events")).toBe(true);
    expect(isKnownKind("google")).toBe(true);
    expect(isKnownKind("gdrive")).toBe(false);
    expect(isDatabase("notion")).toBe(false);
    expect(isKnownKind("posthog")).toBe(true);
    expect(isKnownKind("s3")).toBe(true);
    expect(isKnownKind("calcom")).toBe(true);
    expect(isKnownKind("cal_com")).toBe(false);
    expect(isDatabase("granola")).toBe(false);
    expect(isKnownKind("hubspot")).toBe(false);
    expect(isDatabase("mysql")).toBe(true);
    expect(isDatabase("postgres")).toBe(true);
    expect(isDatabase("stripe")).toBe(false);
    expect(isDatabase("s3")).toBe(false);
    expect(KINDS.filter(hasTableChoice)).toEqual(["postgres", "mysql", "s3"]);
  });

  it("knows an API source by its kind alone", () => {
    expect(KINDS.filter(isEventSource)).toEqual(["events"]);
    expect(isEventSource("event")).toBe(false);
    expect(isEventSource("Events")).toBe(false);
    expect(isEventSource("")).toBe(false);
  });

  it("lets owners and admins manage connectors, not members", () => {
    expect(canManage("owner")).toBe(true);
    expect(canManage("admin")).toBe(true);
    expect(canManage("member")).toBe(false);
  });
});

describe("syncs", () => {
  const now = Date.parse("2026-10-18T12:00:00Z");

  it("is live while queued or running", () => {
    expect(isLiveSync(sync({ status: "CONNECTOR_SYNC_STATUS_QUEUED" }))).toBe(
      true,
    );
    expect(isLiveSync(sync({ status: "CONNECTOR_SYNC_STATUS_RUNNING" }))).toBe(
      true,
    );
    expect(
      isLiveSync(sync({ status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" })),
    ).toBe(false);
    expect(isLiveSync(sync({ status: "CONNECTOR_SYNC_STATUS_FAILED" }))).toBe(
      false,
    );
    expect(isLiveSync(null)).toBe(false);
    expect(isLiveSync(undefined)).toBe(false);
  });

  it("badges every state", () => {
    expect(syncBadge(null)).toEqual({ status: "draft", label: "Not synced" });
    expect(syncBadge(sync({}))).toEqual({
      status: "draft",
      label: "Not synced",
    });
    expect(
      syncBadge(sync({ status: "CONNECTOR_SYNC_STATUS_UNSPECIFIED" })),
    ).toEqual({ status: "draft", label: "Not synced" });
    expect(
      syncBadge(sync({ status: "CONNECTOR_SYNC_STATUS_QUEUED" }), now),
    ).toEqual({ status: "queued", label: "Queued" });
    expect(
      syncBadge(sync({ status: "CONNECTOR_SYNC_STATUS_RUNNING" })),
    ).toEqual({ status: "running", label: "Syncing" });
    expect(
      syncBadge(sync({ status: "CONNECTOR_SYNC_STATUS_SUCCEEDED" })),
    ).toEqual({ status: "succeeded", label: "Synced" });
    expect(syncBadge(sync({ status: "CONNECTOR_SYNC_STATUS_FAILED" }))).toEqual(
      { status: "failed", label: "Failed" },
    );
  });

  it("calls a queued sync that already ran and waits for later a retry", () => {
    const waiting = sync({
      status: "CONNECTOR_SYNC_STATUS_QUEUED",
      attempts: 1,
      nextAttemptAt: "2026-10-18T12:01:00Z",
    });
    expect(syncBadge(waiting, now).label).toBe("Retrying");
    // Due now, or never attempted: queued.
    expect(
      syncBadge({ ...waiting, nextAttemptAt: "2026-10-18T11:59:00Z" }, now)
        .label,
    ).toBe("Queued");
    expect(syncBadge({ ...waiting, attempts: 0 }, now).label).toBe("Queued");
    expect(syncBadge({ ...waiting, nextAttemptAt: null }, now).label).toBe(
      "Queued",
    );
  });

  it("dates a connector by its newest table and totals its rows", () => {
    const c = connector({
      tables: [
        { name: "a", rowCount: "10", syncedAt: "2026-10-01T00:00:00Z" },
        { name: "b", rowCount: "5", syncedAt: "2026-10-03T00:00:00Z" },
        { name: "c", syncedAt: null },
      ],
    });
    expect(lastSyncedAt(c)).toBe("2026-10-03T00:00:00Z");
    expect(rowTotal(c)).toBe(15);
    expect(lastSyncedAt(connector({}))).toBeUndefined();
    expect(rowTotal(connector({ tables: [] }))).toBe(0);
    expect(rowTotal(connector({}))).toBe(0);
  });
});

describe("API sources", () => {
  const now = Date.parse("2026-10-23T10:05:00Z");
  const source = (c: Partial<Connector> = {}) =>
    connector({ id: "src", kind: "events", ...c });
  const events = (rowCount?: string, syncedAt?: string | null) =>
    source({ tables: [{ name: "events", rowCount, syncedAt }] });
  const sends = (
    s: Partial<ListFoundryEventBatchesResponse> = {},
  ): ListFoundryEventBatchesResponse => ({ batches: [], ...s });
  const batch = (pieces: number) => ({ id: `b${pieces}`, pieces });

  it("reads its description and fields, and nothing of another kind", () => {
    const fields = [{ name: "amount", type: "number" }];
    expect(
      eventsConfig(
        source({ config: { events: { description: "Orders", fields } } }),
      ),
    ).toEqual({ description: "Orders", fields });
    expect(eventsConfig(source({ config: { events: {} } }))).toEqual({});
    expect(eventsConfig(source({ config: null }))).toEqual({});
    expect(eventsConfig(source())).toEqual({});
    expect(
      eventsConfig(connector({ config: { stripe: { apiKey: "" } } })),
    ).toEqual({});
  });

  it("finds its events table once a sync wrote it", () => {
    expect(eventsTable(events("3"))).toEqual({
      name: "events",
      rowCount: "3",
      syncedAt: undefined,
    });
    expect(eventsTable(source())).toBeUndefined();
    expect(eventsTable(source({ tables: [] }))).toBeUndefined();
    expect(
      eventsTable(source({ tables: [{ name: "orders", rowCount: "1" }] })),
    ).toBeUndefined();
  });

  it("tells its state from its table and its sends", () => {
    const at = "2026-10-23T10:00:00Z";
    const cases: [
      string,
      Connector,
      ListFoundryEventBatchesResponse,
      unknown,
    ][] = [
      [
        "nothing sent",
        source(),
        sends({ pendingPieces: "0" }),
        { type: "empty" },
      ],
      ["nothing sent, sparse", source(), {}, { type: "empty" }],
      ["an empty table", events("0", at), sends(), { type: "empty" }],
      [
        "sent, not synced",
        source(),
        sends({ batches: [batch(3)], pendingPieces: "3" }),
        { type: "received", pieces: 3 },
      ],
      // The connector was read before the sync that took them ended.
      [
        "synced since the connector was read",
        source(),
        sends({ batches: [batch(2), batch(1)], pendingPieces: "0" }),
        { type: "received", pieces: 3 },
      ],
      [
        "synced",
        events("1200", at),
        sends({ batches: [batch(5)], pendingPieces: "0" }),
        { type: "synced", rows: 1200, syncedAt: at, arriving: 0 },
      ],
      [
        "synced, more arriving",
        events("1200", at),
        sends({ batches: [batch(5)], pendingPieces: "5" }),
        { type: "synced", rows: 1200, syncedAt: at, arriving: 5 },
      ],
      [
        "synced at no time the WFE gave",
        events("12", null),
        {},
        { type: "synced", rows: 12, arriving: 0 },
      ],
    ];
    for (const [what, c, s, want] of cases)
      expect(eventSourceState(c, s), what).toEqual(want);
  });

  it("says its state in a line", () => {
    const cases: [Parameters<typeof eventSourceLine>[0], string][] = [
      [{ type: "empty" }, "Waiting for its first pieces"],
      [
        { type: "received", pieces: 1 },
        "1 piece received; its first sync is about a minute away",
      ],
      [
        { type: "received", pieces: 12000 },
        "12,000 pieces received; its first sync is about a minute away",
      ],
      [
        {
          type: "synced",
          rows: 1,
          syncedAt: "2026-10-23T10:04:40Z",
          arriving: 0,
        },
        "1 piece, synced just now",
      ],
      [
        {
          type: "synced",
          rows: 1200,
          syncedAt: "2026-10-23T10:00:00Z",
          arriving: 5,
        },
        "1,200 pieces, synced 5m ago, 5 more arriving",
      ],
      [
        { type: "synced", rows: 3, arriving: 1500 },
        "3 pieces, 1,500 more arriving",
      ],
    ];
    for (const [state, line] of cases)
      expect(eventSourceLine(state, now)).toBe(line);
  });

  it("waits while its table has no rows or pieces are pending", () => {
    const cases: [
      string,
      Connector,
      ListFoundryEventBatchesResponse | undefined,
      boolean,
    ][] = [
      ["no table, sends not read", source(), undefined, true],
      ["no table, nothing sent", source(), sends({ pendingPieces: "0" }), true],
      ["an empty table", events("0"), sends({ pendingPieces: "0" }), true],
      [
        "no table, synced since",
        source(),
        sends({ batches: [batch(1)], pendingPieces: "0" }),
        true,
      ],
      ["rows, sends not read", events("5"), undefined, false],
      [
        "rows, nothing pending",
        events("5"),
        sends({ pendingPieces: "0" }),
        false,
      ],
      ["rows, pending left out", events("5"), sends(), false],
      [
        "rows, pieces pending",
        events("5"),
        sends({ pendingPieces: "2" }),
        true,
      ],
    ];
    for (const [what, c, s, want] of cases)
      expect(eventSourceWaiting(c, s), what).toBe(want);
  });

  it("says how full it is, amber from 80%", () => {
    const GiB = 2 ** 30,
      cap = (
        rows: number,
        bytes: number,
        maxRows = 2_000_000,
        maxBytes = 2 * GiB,
      ) => ({
        rows: String(rows),
        maxRows: String(maxRows),
        bytes: String(bytes),
        maxBytes: String(maxBytes),
      });
    expect(EVENT_CAPACITY_WARNING).toBe(80);
    // The doc example.
    expect(
      eventCapacity({
        rows: "240000",
        maxRows: "2000000",
        bytes: "268435456",
        maxBytes: "2147483648",
      }),
    ).toEqual({
      percent: 12,
      warn: false,
      text: "12% full: 240,000 of 2,000,000 pieces, 0.3 of 2 GiB",
    });
    const cases: [string, ReturnType<typeof cap>, number, boolean, string][] = [
      [
        "empty",
        cap(0, 0),
        0,
        false,
        "0% full: 0 of 2,000,000 pieces, 0 of 2 GiB",
      ],
      // Rounded down, as the WFE's warning counts it.
      [
        "just under",
        cap(1_599_999, 0),
        79,
        false,
        "79% full: 1,599,999 of 2,000,000 pieces, 0 of 2 GiB",
      ],
      [
        "at 80% of its rows",
        cap(1_600_000, 0),
        80,
        true,
        "80% full: 1,600,000 of 2,000,000 pieces, 0 of 2 GiB",
      ],
      [
        "at 85% of its bytes",
        cap(10, 1_825_361_101),
        85,
        true,
        "85% full: 10 of 2,000,000 pieces, 1.7 of 2 GiB",
      ],
      [
        "full",
        cap(2_000_000, GiB),
        100,
        true,
        "100% full: 2,000,000 of 2,000,000 pieces, 1 of 2 GiB",
      ],
      // Its bytes in the unit of their cap.
      [
        "a cap in KiB",
        cap(2, 5000, 10, 100_000),
        20,
        false,
        "20% full: 2 of 10 pieces, 4.9 of 97.7 KiB",
      ],
      [
        "a cap in bytes",
        cap(1, 500, 10, 1000),
        50,
        false,
        "50% full: 1 of 10 pieces, 500 of 1000 bytes",
      ],
      [
        "a cap of 1 KiB",
        cap(0, 512, 10, 1024),
        50,
        false,
        "50% full: 0 of 10 pieces, 0.5 of 1 KiB",
      ],
    ];
    for (const [what, c, percent, warn, text] of cases)
      expect(eventCapacity(c), what).toEqual({ percent, warn, text });
  });

  it("says nothing without both caps", () => {
    expect(eventCapacity(undefined)).toBeUndefined();
    expect(eventCapacity(null)).toBeUndefined();
    expect(eventCapacity({})).toBeUndefined();
    expect(
      eventCapacity({ rows: "1", maxRows: "10", bytes: "1" }),
    ).toBeUndefined();
    expect(
      eventCapacity({ rows: "1", maxRows: "0", bytes: "1", maxBytes: "10" }),
    ).toBeUndefined();
    expect(
      eventCapacity({ rows: "1", maxRows: "x", bytes: "1", maxBytes: "10" }),
    ).toBeUndefined();
  });
});

describe("API sources' documents", () => {
  const fields = [
    { name: "amount", type: "number" },
    { name: "body", type: "string" },
    { name: "customer", type: "object" },
    { name: "note", type: "null" },
    { name: "paid", type: "boolean" },
    { name: "tags", type: "array" },
    { name: "untyped" },
  ];

  it("offers the fields held as a string or only as null, in the WFE's order, and the one it has", () => {
    const cases: [string, EventsConnectorConfig, string[]][] = [
      ["no config", {}, []],
      ["no fields", { fields: [] }, []],
      ["strings and nulls", { fields }, ["body", "note", "untyped"]],
      ["none chosen", { fields, textField: "" }, ["body", "note", "untyped"]],
      [
        "its text field, listed once",
        { fields, textField: "body" },
        ["body", "note", "untyped"],
      ],
      // A WFE that left it out of the fields still has it.
      [
        "its text field, unlisted",
        { fields, textField: "reply" },
        ["reply", "body", "note", "untyped"],
      ],
      ["its text field alone", { textField: "reply" }, ["reply"]],
      [
        "no string or null field",
        { fields: [{ name: "qty", type: "number" }] },
        [],
      ],
    ];
    for (const [what, config, want] of cases)
      expect(textFieldChoices(config), what).toEqual(want);
  });

  it("checks a typed name as the WFE checks it", () => {
    const columnOf = (name: string) =>
      `${name} is a column of every piece, not a field of its data.`;
    const holds = (name: string, held: string) =>
      `A text field holds strings; this API source holds ${name} as ${held}.`;
    const cases: [string, string, string | undefined][] = [
      ["a string field", "body", undefined],
      ["a field only ever null", "note", undefined],
      ["a field with no type yet", "untyped", undefined],
      ["an unseen field", "subject", undefined],
      ["a name of 64 bytes", "é".repeat(32), undefined],
      ["a name with a space and a quote", 'a "b', undefined],
      ["a name with a leading space", " body", undefined],
      ["any case of a column", "ID", undefined],
      ["no name", "", "A field’s name is 1 to 64 bytes; this one is 0."],
      [
        "a name of 65 bytes",
        "é".repeat(32) + "x",
        "A field’s name is 1 to 64 bytes; this one is 65.",
      ],
      [
        "a control character",
        "a\u0007b",
        "A field’s name has no control characters.",
      ],
      ["DEL", "a\u007fb", "A field’s name has no control characters."],
      [
        "a C1 control character",
        "a\u0085b",
        "A field’s name has no control characters.",
      ],
      ["a newline", "a\nb", "A field’s name has no control characters."],
      ["a backtick", "a`b", "A field’s name has no backticks."],
      ["id", "id", columnOf("id")],
      ["received_at", "received_at", columnOf("received_at")],
      ["occurred_at", "occurred_at", columnOf("occurred_at")],
      ["a number", "amount", holds("amount", "a number")],
      ["a boolean", "paid", holds("paid", "a boolean")],
      ["an object", "customer", holds("customer", "an object")],
      ["an array", "tags", holds("tags", "an array")],
    ];
    for (const [what, name, want] of cases) {
      expect(textFieldProblem(name, fields), what).toBe(want);
      // The mock's lock of the backend's rules agrees on each one.
      const schema = new Map(
        fields.map((f) => [f.name, (f.type || "null") as rules.FieldType]),
      );
      expect(rules.isRefusal(rules.withText(schema, name)), what).toBe(
        want !== undefined,
      );
    }
    // Without fields, only the name is checked.
    expect(textFieldProblem("amount")).toBeUndefined();
    expect(textFieldProblem("id")).toBe(columnOf("id"));
  });

  it("finds a source's own set of pieces, and no other", () => {
    const sets = [
      { id: "upload", name: "General" },
      {
        id: "notes",
        name: "Notes",
        connector: { connectorId: "granola", stream: "notes" },
      },
      {
        id: "other",
        name: "Pieces",
        connector: { connectorId: "elsewhere", stream: "pieces" },
      },
      {
        id: "mine",
        name: "Pieces",
        connector: { connectorId: "src", stream: "pieces" },
      },
    ];
    expect(piecesSet(sets, "src")?.id).toBe("mine");
    expect(piecesSet(sets, "elsewhere")?.id).toBe("other");
    expect(piecesSet(sets, "granola")).toBeUndefined();
    expect(piecesSet(sets, "nobody")).toBeUndefined();
    expect(piecesSet([], "src")).toBeUndefined();
    expect(piecesSet(undefined, "src")).toBeUndefined();
  });

  it("says how many documents a set holds of how many", () => {
    const set = (documents?: number, maxDocuments?: number) => ({
      id: "s",
      documents,
      connector: { connectorId: "src", stream: "pieces", maxDocuments },
    });
    const cases: [string, FoundryDocumentSet, string][] = [
      ["some", set(120, 500), "120 of 500"],
      ["none", set(0, 500), "0 of 500"],
      ["none, sparse", set(undefined, 500), "0 of 500"],
      ["full", set(500, 500), "500 of 500"],
      ["thousands", set(1000, 1000), "1,000 of 1,000"],
      ["no most", set(120), "120"],
      ["a set people made", { id: "g", documents: 7 }, "7"],
    ];
    for (const [what, s, want] of cases) expect(setHolding(s), what).toBe(want);
  });

  it("says this month's spend of the allowance, amber from 80%, and paused while the WFE says so until the 1st", () => {
    const oct = Date.parse("2026-10-24T12:00:00Z");
    const spend = (spent?: string, max?: string, paused?: boolean) => ({
      connectorId: "src",
      stream: "pieces",
      monthSystemUsdMicros: spent,
      maxMonthSystemUsdMicros: max,
      paused,
    });
    expect(EVENT_CAPACITY_WARNING).toBe(80);
    const cases: [string, ReturnType<typeof spend>, number, unknown][] = [
      [
        "nothing spent",
        spend("0", "5000000"),
        oct,
        { level: "room", text: "$0.00 of $5.00 this month" },
      ],
      [
        "nothing spent, sparse",
        spend(undefined, "5000000"),
        oct,
        { level: "room", text: "$0.00 of $5.00 this month" },
      ],
      [
        "some",
        spend("1200000", "5000000"),
        oct,
        { level: "room", text: "$1.20 of $5.00 this month" },
      ],
      // Cents rounded down: it never says more than was spent.
      [
        "just under 80%",
        spend("3999999", "5000000"),
        oct,
        { level: "room", text: "$3.99 of $5.00 this month" },
      ],
      [
        "at 80%",
        spend("4000000", "5000000"),
        oct,
        { level: "warn", text: "$4.00 of $5.00 this month" },
      ],
      [
        "a µ$ short",
        spend("4999999", "5000000"),
        oct,
        { level: "warn", text: "$4.99 of $5.00 this month" },
      ],
      [
        "spent",
        spend("5000000", "5000000", true),
        oct,
        {
          level: "paused",
          text: "$5.00 of $5.00 this month",
          until: "November 1",
        },
      ],
      // The documents in flight when it ran out may pass it.
      [
        "past it",
        spend("5031000", "5000000", true),
        oct,
        {
          level: "paused",
          text: "$5.03 of $5.00 this month",
          until: "November 1",
        },
      ],
      [
        "spent in December",
        spend("5000000", "5000000", true),
        Date.parse("2026-12-15T00:00:00Z"),
        {
          level: "paused",
          text: "$5.00 of $5.00 this month",
          until: "January 1",
        },
      ],
      // A UTC month, whatever the browser's zone.
      [
        "spent in the month's last second",
        spend("5000000", "5000000", true),
        Date.parse("2026-10-31T23:59:59Z"),
        {
          level: "paused",
          text: "$5.00 of $5.00 this month",
          until: "November 1",
        },
      ],
      [
        "spent in the month's first second",
        spend("5000000", "5000000", true),
        Date.parse("2026-11-01T00:00:00Z"),
        {
          level: "paused",
          text: "$5.00 of $5.00 this month",
          until: "December 1",
        },
      ],
      // Spent, but it holds no documents, so its next sync writes them.
      [
        "spent, not paused",
        spend("5000000", "5000000", false),
        oct,
        { level: "warn", text: "$5.00 of $5.00 this month" },
      ],
      [
        "spent, paused sparse",
        spend("5000000", "5000000"),
        oct,
        { level: "warn", text: "$5.00 of $5.00 this month" },
      ],
      // The workspace's allowance for its API sources is spent.
      [
        "paused with some of its own left",
        spend("1200000", "5000000", true),
        oct,
        {
          level: "paused",
          text: "$1.20 of $5.00 this month",
          until: "November 1",
        },
      ],
      [
        "another allowance",
        spend("6100000", "7500000"),
        oct,
        { level: "warn", text: "$6.10 of $7.50 this month" },
      ],
      [
        "a spend it can't read",
        spend("x", "5000000"),
        oct,
        { level: "room", text: "$0.00 of $5.00 this month" },
      ],
    ];
    for (const [what, c, now, want] of cases)
      expect(setSpend(c, now), what).toEqual(want);
  });

  it("says nothing of a set without an allowance", () => {
    expect(setSpend(undefined)).toBeUndefined();
    expect(setSpend(null)).toBeUndefined();
    // A Granola set: "0" spent, no allowance.
    expect(
      setSpend({
        connectorId: "g",
        stream: "notes",
        monthSystemUsdMicros: "0",
      }),
    ).toBeUndefined();
    expect(setSpend({ maxMonthSystemUsdMicros: "0" })).toBeUndefined();
    expect(setSpend({ maxMonthSystemUsdMicros: "x" })).toBeUndefined();
    expect(setSpend({ maxMonthSystemUsdMicros: "-1" })).toBeUndefined();
    // Paused says nothing without an allowance to say it of.
    expect(
      setSpend({ monthSystemUsdMicros: "0", paused: true }),
    ).toBeUndefined();
  });

  it("says what saving a text field costs before it saves, naming who reads the set", () => {
    const set = (
      maxDocuments?: number,
      max?: string,
      spent?: string,
      paused?: boolean,
    ): FoundryDocumentSet => ({
      id: "s",
      connector: {
        connectorId: "src",
        stream: "pieces",
        maxDocuments,
        maxMonthSystemUsdMicros: max,
        monthSystemUsdMicros: spent,
        paused,
      },
    });
    const oct = Date.parse("2026-10-24T12:00:00Z");
    const reread = (n: string, usd: string) =>
      `Foundry re-reads this API source’s newest ${n} pieces. Extracting them comes out of its ${usd} for the month.`;
    // A person's sync writes while the set is paused; extracting what it
    // wrote waits for the month, or a person's load.
    const later = (n: string, until: string) =>
      `Foundry re-reads this API source’s newest ${n} pieces. Its extraction is paused until ${until}, so ontologies read the new text from then, or from a load a person starts.`;
    const removed =
      "Foundry removes this API source’s pieces as documents at its next sync.";
    const save = (name: string) => ({
      title: `Use ${name} as the text field?`,
      action: "Save text field",
    });
    const cases: [
      string,
      Parameters<typeof textFieldConfirm>,
      ReturnType<typeof textFieldConfirm>,
    ][] = [
      // Before its first sync with one, it has no set: the defaults stand in.
      [
        "a first one",
        ["", "reply", undefined, []],
        { ...save("reply"), text: reread("500", "$5") },
      ],
      [
        "a change",
        ["reply", "body", set(500, "5000000"), []],
        { ...save("body"), text: reread("500", "$5") },
      ],
      [
        "a change, read by one",
        ["reply", "body", set(500, "5000000"), ["Support"]],
        { ...save("body"), text: `${reread("500", "$5")} Support reads them.` },
      ],
      [
        "a change, read by two",
        ["reply", "body", set(500, "5000000"), ["Support", "Sales"]],
        {
          ...save("body"),
          text: `${reread("500", "$5")} Support and Sales read them.`,
        },
      ],
      [
        "a change, read by three",
        ["reply", "body", set(500, "5000000"), ["Support", "Sales", "Ops"]],
        {
          ...save("body"),
          text: `${reread("500", "$5")} Support, Sales, and Ops read them.`,
        },
      ],
      // The set's own numbers win.
      [
        "another set's numbers",
        ["reply", "body", set(1000, "7500000"), []],
        { ...save("body"), text: reread("1,000", "$7.50") },
      ],
      [
        "a set that says none",
        ["reply", "body", set(), []],
        { ...save("body"), text: reread("500", "$5") },
      ],
      [
        "clearing",
        ["reply", "", set(500, "5000000"), []],
        {
          title: "Stop using reply as the text field?",
          text: removed,
          action: "Clear text field",
        },
      ],
      [
        "clearing, read by one",
        ["reply", "", set(500, "5000000"), ["Support"]],
        {
          title: "Stop using reply as the text field?",
          text: `${removed} Support reads them.`,
          action: "Clear text field",
        },
      ],
      [
        "a change with some of the month left",
        ["reply", "body", set(500, "5000000", "4990000", false), [], oct],
        { ...save("body"), text: reread("500", "$5") },
      ],
      [
        "a change while paused",
        ["reply", "body", set(500, "5000000", "5000000", true), [], oct],
        { ...save("body"), text: later("500", "November 1") },
      ],
      [
        "a change while paused, read by one",
        [
          "reply",
          "body",
          set(500, "5000000", "6000000", true),
          ["Support"],
          oct,
        ],
        {
          ...save("body"),
          text: `${later("500", "November 1")} Support reads them.`,
        },
      ],
      // The workspace's allowance for its API sources is spent.
      [
        "a change while paused with some of its own left",
        ["reply", "body", set(500, "5000000", "1000000", true), [], oct],
        { ...save("body"), text: later("500", "November 1") },
      ],
      [
        "a change while another month is paused, by another set's numbers",
        [
          "reply",
          "body",
          set(1000, "7500000", "7500000", true),
          [],
          Date.parse("2026-12-31T23:59:59Z"),
        ],
        { ...save("body"), text: later("1,000", "January 1") },
      ],
      // A set whose field was cleared holds no documents, so it is not
      // paused.
      [
        "a first one once the month is spent",
        ["", "reply", set(500, "5000000", "5000000", false), [], oct],
        { ...save("reply"), text: reread("500", "$5") },
      ],
      // Clearing spends nothing, so it never waits.
      [
        "clearing once the month is spent",
        ["reply", "", set(500, "5000000", "5000000", true), [], oct],
        {
          title: "Stop using reply as the text field?",
          text: removed,
          action: "Clear text field",
        },
      ],
    ];
    for (const [what, args, want] of cases)
      expect(textFieldConfirm(...args), what).toEqual(want);
    expect(EVENT_SET_DEFAULTS).toEqual({
      maxDocuments: 500,
      maxMonthUsdMicros: 5_000_000,
    });
  });
});

describe("the Data tree", () => {
  const ids = (folders: ReturnType<typeof kindFolders>) =>
    folders.map((f) => [f.kind, f.items.map((i) => i.connector.id)]);
  const table = (name: string) => ({ name });

  it("is the doc example", () => {
    expect(
      ids(
        kindFolders(
          [
            connector({ id: "a", kind: "postgres", name: "Shop" }),
            connector({ id: "b", kind: "stripe" }),
            connector({ id: "c", kind: "postgres", name: "Billing" }),
          ],
          ["stripe", "postgres"],
        ),
      ),
    ).toEqual([
      ["stripe", ["b"]],
      ["postgres", ["a", "c"]],
    ]);
  });

  it("puts folders in the kinds' order, then kinds it doesn't list", () => {
    const folders = kindFolders(
      [
        connector({ id: "x", kind: "hubspot" }),
        connector({ id: "a", kind: "s3" }),
        connector({ id: "b", kind: "stripe" }),
        connector({ id: "y", kind: "salesforce" }),
        connector({ id: "z", kind: "hubspot" }),
      ],
      KINDS,
    );
    expect(ids(folders)).toEqual([
      ["stripe", ["b"]],
      ["s3", ["a"]],
      ["hubspot", ["x", "z"]],
      ["salesforce", ["y"]],
    ]);
    // Kinds without a connector have no folder, and nothing gives none.
    expect(kindFolders([], KINDS)).toEqual([]);
  });

  it("keeps what a search finds, by name, kind or tables", () => {
    const shop = connector({
        id: "shop",
        kind: "postgres",
        name: "Shop database",
        tables: [table("orders"), table("order_items"), table("customers")],
      }),
      stripe = connector({
        id: "stripe",
        kind: "stripe",
        tables: [table("customers"), table("charges")],
      }),
      list = [shop, stripe],
      label = (kind: string) =>
        ({ postgres: "PostgreSQL", stripe: "Stripe" })[kind] || kind;
    // By tables: only those, in their connector.
    expect(kindFolders(list, KINDS, " ORDER", label)).toEqual([
      {
        kind: "postgres",
        items: [
          {
            connector: shop,
            tables: [table("orders"), table("order_items")],
            sets: [],
          },
        ],
      },
    ]);
    expect(ids(kindFolders(list, KINDS, "customers", label))).toEqual([
      ["stripe", ["stripe"]],
      ["postgres", ["shop"]],
    ]);
    // By name, or by kind (an unnamed one is called by its kind): no tables.
    expect(kindFolders(list, KINDS, "shop", label)).toEqual([
      { kind: "postgres", items: [{ connector: shop, tables: [], sets: [] }] },
    ]);
    expect(kindFolders(list, KINDS, "postgresql", label)).toEqual([
      { kind: "postgres", items: [{ connector: shop, tables: [], sets: [] }] },
    ]);
    expect(kindFolders(list, KINDS, "stri", label)).toEqual([
      { kind: "stripe", items: [{ connector: stripe, tables: [], sets: [] }] },
    ]);
    // Nothing found, and a blank search keeps everything.
    expect(kindFolders(list, KINDS, "nothing", label)).toEqual([]);
    expect(ids(kindFolders(list, KINDS, "  ", label))).toEqual([
      ["stripe", ["stripe"]],
      ["postgres", ["shop"]],
    ]);
    // Without a label, a kind is its own name.
    expect(ids(kindFolders(list, KINDS, "postgres"))).toEqual([
      ["postgres", ["shop"]],
    ]);
  });

  it("lists each connector's tables and its own sets, or those a search finds", () => {
    const notes = {
        id: "n",
        name: "Notes",
        connector: { connectorId: "gr", stream: "notes" },
      },
      transcripts = {
        id: "t",
        name: "Transcripts",
        connector: { connectorId: "gr", stream: "transcripts" },
      },
      general = { id: "g", name: "General" },
      sets = [general, notes, transcripts],
      gr = connector({
        id: "gr",
        kind: "granola",
        name: "Team",
        tables: [table("notes"), table("folders")],
      }),
      other = connector({ id: "o", kind: "granola", name: "Other" });
    const folders = (term = "") =>
      kindFolders([gr, other], KINDS, term, undefined, sets);
    expect(folders()).toEqual([
      {
        kind: "granola",
        items: [
          { connector: gr, tables: gr.tables, sets: [notes, transcripts] },
          { connector: other, tables: [], sets: [] },
        ],
      },
    ]);
    // A search finds a set by its name, and a table by its; a connector's
    // name lists nothing.
    expect(folders("notes")[0]!.items).toEqual([
      { connector: gr, tables: [table("notes")], sets: [notes] },
    ]);
    expect(folders("transcr")[0]!.items).toEqual([
      { connector: gr, tables: [], sets: [transcripts] },
    ]);
    expect(folders("team")[0]!.items).toEqual([
      { connector: gr, tables: [], sets: [] },
    ]);
    // Without sets given, a connector lists none.
    expect(kindFolders([gr], KINDS)[0]!.items[0]!.sets).toEqual([]);
  });

  it("lays out documents/: a folder per set, its documents oldest first", () => {
    const sets = [
      { id: "g", name: "General" },
      { id: "b", name: "Board minutes" },
      { id: "e", name: "Empty" },
    ];
    const docs = [
      { id: "d3", name: "q3-minutes.md", setId: "b", createdAt: "2026-10-03" },
      { id: "d1", name: "contract.md", setId: "g", createdAt: "2026-10-01" },
      { id: "d2", name: "q2-minutes.md", setId: "b", createdAt: "2026-10-01" },
      // Its set isn't listed yet: it waits for the next read.
      { id: "d4", name: "stray.md", setId: "new", createdAt: "2026-10-04" },
    ];
    const ids = (term?: string) =>
      setFolders(sets, docs, term).map((f) => [
        f.set.id,
        f.documents.map((d) => d.id),
      ]);
    // Every set, in the WFE's order, empty ones too; documents as uploaded.
    expect(ids()).toEqual([
      ["g", ["d1"]],
      ["b", ["d2", "d3"]],
      ["e", []],
    ]);
    // A set's name keeps all its documents; else the documents matched.
    expect(ids("BOARD")).toEqual([["b", ["d2", "d3"]]]);
    expect(ids("q3")).toEqual([["b", ["d3"]]]);
    expect(ids("minutes")).toEqual([["b", ["d2", "d3"]]]);
    expect(ids("empty")).toEqual([["e", []]]);
    expect(ids("stray")).toEqual([]);
    expect(ids("  ")).toEqual(ids());
    expect(setFolders([], docs)).toEqual([]);
  });

  it("keeps setFolders' example", () => {
    expect(
      setFolders(
        [
          { id: "g", name: "General" },
          { id: "b", name: "Board" },
        ],
        [{ id: "d1", name: "minutes.md", setId: "b", createdAt: "2026-10-01" }],
        "minutes",
      ).map((f) => [f.set.id, f.documents.map((d) => d.id)]),
    ).toEqual([["b", ["d1"]]]);
  });

  it("says how much of a group is picked", () => {
    expect(picked(["a", "b"], new Set(["a"]))).toBe("some");
    expect(picked(["a", "b"], new Set(["a", "b", "c"]))).toBe("all");
    expect(picked(["a", "b"], new Set(["c"]))).toBe("none");
    expect(picked([], new Set(["a"]))).toBe("none");
  });
});

describe("credential forms", () => {
  it("starts each kind empty, with the database's default port", () => {
    expect(emptyForm("stripe")).toEqual({ kind: "stripe", apiKey: "" });
    expect(emptyForm("workos")).toEqual({ kind: "workos", apiKey: "" });
    expect(emptyForm("linear")).toEqual({ kind: "linear", apiKey: "" });
    expect(emptyForm("calcom")).toEqual({ kind: "calcom", apiKey: "" });
    // Granola's transcripts start off: each costs several notes to read.
    expect(emptyForm("granola")).toEqual({
      kind: "granola",
      apiKey: "",
      transcripts: false,
    });
    expect(emptyForm("github")).toEqual({
      kind: "github",
      token: "",
      repos: "",
      reviewDays: "90",
    });
    expect(emptyForm("posthog")).toMatchObject({
      region: "us",
      eventDays: "30",
    });
    expect(emptyForm("postgres")).toMatchObject({
      port: "5432",
      tls: "verify",
    });
    expect(emptyForm("mysql")).toMatchObject({ port: "3306" });
    // Slack keeps 90 days of history and Google Workspace 90 days of past
    // events, as the WFE's defaults do.
    expect(emptyForm("slack")).toEqual({
      kind: "slack",
      botToken: "",
      historyDays: "90",
    });
    expect(emptyForm("notion")).toEqual({ kind: "notion", token: "" });
    expect(emptyForm("google")).toEqual({
      kind: "google",
      serviceAccountKey: "",
      subject: "",
      eventDays: "90",
    });
  });

  it("sends Slack's, Notion's and Google Workspace's credentials trimmed, with their days", () => {
    expect(
      toConfig({ kind: "slack", botToken: " xoxb-1-2 \n", historyDays: "30" }),
    ).toEqual({ slack: { botToken: "xoxb-1-2", historyDays: 30 } });
    expect(toConfig(emptyForm("slack"))).toEqual({
      slack: { botToken: "", historyDays: 90 },
    });
    // Blank days are left to the WFE's default.
    expect(
      toConfig({ kind: "slack", botToken: "xoxb-1", historyDays: "" }),
    ).toEqual({ slack: { botToken: "xoxb-1", historyDays: 0 } });
    expect(toConfig({ kind: "notion", token: "\tntn_1 " })).toEqual({
      notion: { token: "ntn_1" },
    });
    const key = '{"type": "service_account", "client_email": "sa@p.iam"}';
    expect(
      toConfig({
        kind: "google",
        serviceAccountKey: `\n${key}\n`,
        subject: " ada@example.com ",
        eventDays: "14",
      }),
    ).toEqual({
      google: {
        serviceAccountKey: key,
        subject: "ada@example.com",
        eventDays: 14,
      },
    });
    expect(
      toConfig({ ...emptyForm("google"), eventDays: "" } as ConnectionForm),
    ).toEqual({
      google: { serviceAccountKey: "", subject: "", eventDays: 0 },
    });
  });

  it("sends an edit's blank credential blank beside new settings, so the WFE keeps the stored one", () => {
    expect(
      toConfig({
        ...formFromConfig("slack", {
          slack: { botToken: "", historyDays: 30 },
        }),
        historyDays: "60",
      } as ConnectionForm),
    ).toEqual({ slack: { botToken: "", historyDays: 60 } });
    expect(
      toConfig(formFromConfig("notion", { notion: { token: "" } })),
    ).toEqual({ notion: { token: "" } });
    expect(
      toConfig({
        ...formFromConfig("google", {
          google: {
            serviceAccountKey: "",
            subject: "ada@example.com",
            eventDays: 30,
          },
        }),
        subject: "alan@example.com",
      } as ConnectionForm),
    ).toEqual({
      google: {
        serviceAccountKey: "",
        subject: "alan@example.com",
        eventDays: 30,
      },
    });
  });

  it("sends API keys trimmed and nothing else", () => {
    expect(toConfig({ kind: "stripe", apiKey: " sk_test_1 " })).toEqual({
      stripe: { apiKey: "sk_test_1" },
    });
    expect(toConfig({ kind: "workos", apiKey: "sk_1\n" })).toEqual({
      workos: { apiKey: "sk_1" },
    });
    expect(toConfig({ kind: "linear", apiKey: " lin_api_1 " })).toEqual({
      linear: { apiKey: "lin_api_1" },
    });
    expect(toConfig({ kind: "calcom", apiKey: " cal_live_1\n" })).toEqual({
      calcom: { apiKey: "cal_live_1" },
    });
    expect(
      toConfig({ kind: "granola", apiKey: "\tgrn_1 ", transcripts: false }),
    ).toEqual({ granola: { apiKey: "grn_1", transcripts: false } });
    // An edit's blank key is sent blank: the WFE keeps the stored one.
    expect(toConfig(emptyForm("granola"))).toEqual({
      granola: { apiKey: "", transcripts: false },
    });
  });

  it("sends Granola's transcripts either way, so turning them off is sent", () => {
    expect(
      toConfig({ kind: "granola", apiKey: "grn_1", transcripts: true }),
    ).toEqual({ granola: { apiKey: "grn_1", transcripts: true } });
    // An edit that only turns them off keeps the stored key.
    expect(
      toConfig({
        ...emptyForm("granola"),
        transcripts: false,
      } as ConnectionForm),
    ).toEqual({ granola: { apiKey: "", transcripts: false } });
  });

  it("sends GitHub's token trimmed, its repositories as owner/name and its review days", () => {
    expect(
      toConfig({
        kind: "github",
        token: " github_pat_1 ",
        repos:
          "acme/api\n https://github.com/acme/web.git ,www.github.com/x/y/",
        reviewDays: "30",
      }),
    ).toEqual({
      github: {
        token: "github_pat_1",
        repos: ["acme/api", "acme/web", "x/y"],
        reviewDays: 30,
      },
    });
    // Blank repositories read every one; blank days are the WFE's default.
    expect(
      toConfig({ ...emptyForm("github"), reviewDays: "" } as ConnectionForm),
    ).toEqual({ github: { token: "", repos: [], reviewDays: 0 } });
    expect(githubRepos(" \n, ")).toEqual([]);
    expect(githubRepos("HTTP://GitHub.com/Acme/Api//")).toEqual(["Acme/Api"]);
  });

  it("sends PostHog settings, the host only for a custom region", () => {
    const form = emptyForm("posthog") as Extract<
      ConnectionForm,
      { kind: "posthog" }
    >;
    expect(
      toConfig({ ...form, apiKey: "phx_1", host: "https://ignored" }),
    ).toEqual({ posthog: { apiKey: "phx_1", region: "us", eventDays: 30 } });
    expect(
      toConfig({
        ...form,
        apiKey: "phx_1",
        region: "custom",
        host: " https://ph.example.com ",
        projectId: " 42 ",
        eventDays: "90",
      }),
    ).toEqual({
      posthog: {
        apiKey: "phx_1",
        region: "custom",
        host: "https://ph.example.com",
        projectId: "42",
        eventDays: 90,
      },
    });
    // Blank days are left to the WFE's default.
    expect(toConfig({ ...form, apiKey: "phx_1", eventDays: "" })).toMatchObject(
      { posthog: { eventDays: 0 } },
    );
  });

  it("sends database settings with the password as typed", () => {
    const pg = {
      ...emptyForm("postgres"),
      host: " db.example.com ",
      database: "app",
      user: "ro",
      password: " p w ",
    } as ConnectionForm;
    expect(toConfig(pg)).toEqual({
      postgres: {
        host: "db.example.com",
        port: 5432,
        database: "app",
        user: "ro",
        password: " p w ",
      },
    });
    expect(
      toConfig(
        { ...pg, schema: "sales", tls: "disable", ca: "PEM" } as ConnectionForm,
        ["orders"],
      ),
    ).toEqual({
      postgres: {
        host: "db.example.com",
        port: 5432,
        database: "app",
        user: "ro",
        password: " p w ",
        schema: "sales",
        tables: ["orders"],
        disableTls: true,
        ca: "PEM",
      },
    });
    // MySQL has no schema; a blank port is the WFE's default.
    const my = {
      ...emptyForm("mysql"),
      host: "h",
      port: "",
      database: "d",
      user: "u",
      password: "p",
      schema: "ignored",
    } as ConnectionForm;
    expect(toConfig(my)).toEqual({
      mysql: { host: "h", port: 0, database: "d", user: "u", password: "p" },
    });
  });

  it("knows whether a credential was entered", () => {
    expect(hasCredential({ kind: "stripe", apiKey: "  " })).toBe(false);
    expect(hasCredential({ kind: "stripe", apiKey: "sk" })).toBe(true);
    expect(hasCredential({ kind: "calcom", apiKey: " " })).toBe(false);
    expect(hasCredential({ kind: "calcom", apiKey: "cal_1" })).toBe(true);
    expect(hasCredential(emptyForm("granola"))).toBe(false);
    expect(
      hasCredential({ kind: "granola", apiKey: "grn_1", transcripts: true }),
    ).toBe(true);
    expect(
      hasCredential({ ...emptyForm("github"), token: " " } as ConnectionForm),
    ).toBe(false);
    expect(
      hasCredential({
        ...emptyForm("github"),
        token: "ghp_1",
      } as ConnectionForm),
    ).toBe(true);
    expect(hasCredential(emptyForm("slack"))).toBe(false);
    expect(
      hasCredential({ kind: "slack", botToken: "xoxb-1", historyDays: "" }),
    ).toBe(true);
    expect(hasCredential({ kind: "notion", token: " " })).toBe(false);
    expect(hasCredential({ kind: "notion", token: "ntn_1" })).toBe(true);
    // An edit's Google form holds the subject, never the key.
    expect(
      hasCredential({
        ...emptyForm("google"),
        subject: "ada@example.com",
      } as ConnectionForm),
    ).toBe(false);
    expect(
      hasCredential({
        ...emptyForm("google"),
        serviceAccountKey: "{}",
      } as ConnectionForm),
    ).toBe(true);
    expect(hasCredential(emptyForm("postgres"))).toBe(false);
    expect(
      hasCredential({ ...emptyForm("mysql"), password: " " } as ConnectionForm),
    ).toBe(true);
  });

  it("names a connection after its database or Stripe mode", () => {
    expect(suggestedName({ kind: "stripe", apiKey: "rk_live_abc" })).toBe(
      "Stripe (live)",
    );
    expect(suggestedName({ kind: "stripe", apiKey: "sk_test_abc" })).toBe(
      "Stripe (test)",
    );
    expect(suggestedName({ kind: "stripe", apiKey: "" })).toBe("Stripe");
    expect(suggestedName({ kind: "workos", apiKey: "" })).toBe("WorkOS");
    expect(suggestedName(emptyForm("posthog"))).toBe("PostHog");
    expect(suggestedName(emptyForm("linear"))).toBe("Linear");
    expect(suggestedName({ kind: "calcom", apiKey: "cal_live_1" })).toBe(
      "Cal.com",
    );
    expect(suggestedName(emptyForm("granola"))).toBe("Granola");
    expect(suggestedName(emptyForm("slack"))).toBe("Slack");
    expect(suggestedName(emptyForm("notion"))).toBe("Notion");
    expect(suggestedName(emptyForm("google"))).toBe("Google Workspace");
    expect(suggestedName(emptyForm("github"))).toBe("GitHub");
    const github = emptyForm("github") as Extract<
      ConnectionForm,
      { kind: "github" }
    >;
    expect(suggestedName({ ...github, repos: " acme/api " })).toBe("acme/api");
    expect(suggestedName({ ...github, repos: "acme/api\nacme/web" })).toBe(
      "GitHub",
    );
    expect(suggestedName(emptyForm("postgres"))).toBe("PostgreSQL");
    expect(suggestedName(emptyForm("mysql"))).toBe("MySQL");
    expect(
      suggestedName({
        ...emptyForm("mysql"),
        database: " shop ",
      } as ConnectionForm),
    ).toBe("shop");
    expect(
      suggestedName({
        ...emptyForm("postgres"),
        database: "app",
      } as ConnectionForm),
    ).toBe("app");
  });
});

describe("editing a stored connector", () => {
  it("starts from its settings with the credential blank", () => {
    expect(
      formFromConfig("postgres", {
        postgres: {
          host: "db",
          port: 6543,
          database: "app",
          user: "ro",
          password: "",
          schema: "sales",
          tables: ["a"],
          disableTls: true,
          ca: "PEM",
        },
      }),
    ).toEqual({
      kind: "postgres",
      host: "db",
      port: "6543",
      database: "app",
      user: "ro",
      password: "",
      schema: "sales",
      tls: "disable",
      ca: "PEM",
    });
    expect(
      formFromConfig("mysql", {
        mysql: { host: "db", port: 0, schema: "ignored" },
      }),
    ).toMatchObject({ port: "3306", schema: "", tls: "verify" });
    expect(
      formFromConfig("posthog", {
        posthog: {
          apiKey: "",
          region: "eu",
          projectId: "7",
          eventDays: 0,
        },
      }),
    ).toEqual({
      kind: "posthog",
      apiKey: "",
      region: "eu",
      host: "",
      projectId: "7",
      eventDays: "30",
    });
    expect(
      formFromConfig("posthog", {
        posthog: { region: "custom", host: "https://ph" },
      }),
    ).toMatchObject({ region: "custom", host: "https://ph" });
    expect(
      formFromConfig("posthog", { posthog: { region: "" } }),
    ).toMatchObject({ region: "us" });
    expect(
      formFromConfig("github", {
        github: { token: "", repos: ["acme/api", "acme/web"], reviewDays: 30 },
      }),
    ).toEqual({
      kind: "github",
      token: "",
      repos: "acme/api\nacme/web",
      reviewDays: "30",
    });
    expect(formFromConfig("github", { github: {} })).toEqual(
      emptyForm("github"),
    );
    expect(formFromConfig("linear", { linear: { apiKey: "" } })).toEqual(
      emptyForm("linear"),
    );
    expect(formFromConfig("calcom", { calcom: { apiKey: "" } })).toEqual(
      emptyForm("calcom"),
    );
    expect(formFromConfig("granola", { granola: {} })).toEqual(
      emptyForm("granola"),
    );
    // A Granola connection's transcripts, as stored.
    expect(
      formFromConfig("granola", { granola: { apiKey: "", transcripts: true } }),
    ).toEqual({ kind: "granola", apiKey: "", transcripts: true });
    expect(
      formFromConfig("granola", { granola: { transcripts: false } }),
    ).toEqual(emptyForm("granola"));
    // Slack's history and Google Workspace's subject and days, as stored,
    // with the credential blank; a sparse config takes the defaults.
    expect(
      formFromConfig("slack", { slack: { botToken: "", historyDays: 30 } }),
    ).toEqual({ kind: "slack", botToken: "", historyDays: "30" });
    expect(formFromConfig("slack", { slack: { historyDays: 0 } })).toEqual(
      emptyForm("slack"),
    );
    expect(formFromConfig("notion", { notion: { token: "" } })).toEqual(
      emptyForm("notion"),
    );
    expect(
      formFromConfig("google", {
        google: {
          serviceAccountKey: "",
          subject: "ada@example.com",
          eventDays: 14,
        },
      }),
    ).toEqual({
      kind: "google",
      serviceAccountKey: "",
      subject: "ada@example.com",
      eventDays: "14",
    });
    expect(formFromConfig("google", { google: {} })).toEqual(
      emptyForm("google"),
    );
    // Another kind's config is not this form's.
    expect(formFromConfig("granola", { calcom: { apiKey: "" } })).toEqual(
      emptyForm("granola"),
    );
    expect(formFromConfig("google", { slack: { historyDays: 30 } })).toEqual(
      emptyForm("google"),
    );
    expect(formFromConfig("stripe", { stripe: { apiKey: "" } })).toEqual({
      kind: "stripe",
      apiKey: "",
    });
  });

  it("fills what a sparse stored config leaves out", () => {
    expect(formFromConfig("postgres", { postgres: {} })).toEqual(
      emptyForm("postgres"),
    );
  });

  it("ignores a missing or mismatched config", () => {
    expect(formFromConfig("postgres", null)).toEqual(emptyForm("postgres"));
    expect(formFromConfig("postgres", { mysql: { host: "x" } })).toEqual(
      emptyForm("postgres"),
    );
    expect(configKind(null)).toBeUndefined();
    expect(configKind({ workos: {} })).toBe("workos");
  });
});

describe("source summaries", () => {
  it("says where a connector reads from, never its credential", () => {
    expect(
      sourceSummary(
        connector({
          kind: "postgres",
          config: {
            postgres: {
              host: "db",
              port: 5432,
              database: "app",
              user: "ro",
              password: "",
            },
          },
        }),
      ),
    ).toBe("ro@db:5432/app");
    expect(
      sourceSummary(
        connector({
          kind: "mysql",
          config: { mysql: { host: "h", database: "d" } },
        }),
      ),
    ).toBe("h/d");
    expect(
      sourceSummary(
        connector({
          kind: "posthog",
          config: { posthog: { region: "eu", projectId: "7" } },
        }),
      ),
    ).toBe("EU Cloud · project 7");
    expect(
      sourceSummary(
        connector({
          kind: "posthog",
          config: { posthog: { region: "custom", host: "https://ph.example" } },
        }),
      ),
    ).toBe("ph.example");
    expect(
      sourceSummary(
        connector({
          kind: "posthog",
          config: { posthog: { region: "custom" } },
        }),
      ),
    ).toBe("");
    expect(
      sourceSummary(
        connector({ kind: "posthog", config: { posthog: { region: "us" } } }),
      ),
    ).toBe("US Cloud");
    expect(
      sourceSummary(
        connector({
          config: { stripe: { apiKey: "" } },
          secretHint: "sk…4f2a",
        }),
      ),
    ).toBe("sk…4f2a");
    expect(sourceSummary(connector({ config: null, secretHint: "h" }))).toBe(
      "h",
    );
    expect(sourceSummary(connector({ config: null }))).toBe("");
    expect(sourceSummary(connector({ config: { workos: {} } }))).toBe("");
    const github = (repos?: string[]) =>
      sourceSummary(
        connector({ kind: "github", config: { github: { repos } } }),
      );
    expect(github()).toBe("Every repository");
    expect(github([])).toBe("Every repository");
    expect(github(["acme/api", "acme/web"])).toBe("acme/api, acme/web");
    expect(github(["a/1", "a/2", "a/3"])).toBe("3 repositories");
    expect(
      sourceSummary(
        connector({
          kind: "linear",
          config: { linear: { apiKey: "" } },
          secretHint: "lin_api…abcd",
        }),
      ),
    ).toBe("lin_api…abcd");
    expect(
      sourceSummary(
        connector({
          kind: "calcom",
          config: { calcom: { apiKey: "" } },
          secretHint: "cal_live…4f2a",
        }),
      ),
    ).toBe("cal_live…4f2a");
    expect(
      sourceSummary(
        connector({
          kind: "granola",
          config: { granola: { apiKey: "" } },
          secretHint: "grn…4f2a",
        }),
      ),
    ).toBe("grn…4f2a");
  });
});

describe("table selection and keys", () => {
  const all = ["accounts", "orders", "users"];

  it("sends every table as an empty list and none as nothing to send", () => {
    expect(tableSelection(all, new Set(all))).toEqual([]);
    expect(tableSelection(all, new Set())).toBe("none");
    expect(tableSelection(all, new Set(["users", "accounts"]))).toEqual([
      "accounts",
      "users",
    ]);
    // A stale choice not in the discovered list is dropped.
    expect(tableSelection(all, new Set(["gone"]))).toBe("none");
  });

  const tables = [
    { name: "accounts", foreignKeys: [] },
    {
      name: "users",
      foreignKeys: [
        {
          column: "account_id",
          referencedTable: "accounts",
          referencedColumn: "id",
        },
      ],
    },
    {
      name: "orders",
      foreignKeys: [
        {
          column: "account_id",
          referencedTable: "accounts",
          referencedColumn: "id",
        },
        { column: "user_id", referencedTable: "users", referencedColumn: "id" },
      ],
    },
    { name: "audit" },
  ];

  it("finds the tables linked to a selection", () => {
    expect([...relatedTables(tables, new Set(["users"]))].sort()).toEqual([
      "accounts",
      "orders",
    ]);
    expect([...relatedTables(tables, new Set(["accounts"]))].sort()).toEqual([
      "orders",
      "users",
    ]);
    expect(relatedTables(tables, new Set(all)).size).toBe(0);
    expect(relatedTables(tables, new Set()).size).toBe(0);
  });

  it("lists the foreign keys that point at a table", () => {
    expect(referencesTo(tables, "accounts")).toEqual([
      {
        table: "users",
        column: "account_id",
        referencedTable: "accounts",
        referencedColumn: "id",
      },
      {
        table: "orders",
        column: "account_id",
        referencedTable: "accounts",
        referencedColumn: "id",
      },
    ]);
    expect(referencesTo(tables, "audit")).toEqual([]);
  });

  it("names a table's primary key", () => {
    expect(keyLabel({ primaryKey: ["order_id", "line"] })).toBe(
      "Key order_id, line",
    );
    expect(keyLabel({ primaryKey: [] })).toBe("No primary key");
    expect(keyLabel({})).toBe("No primary key");
  });

  it("summarises a table's keys", () => {
    expect(keySummary({ primaryKey: ["id"], foreignKeys: [] })).toBe("Key id");
    expect(
      keySummary({
        primaryKey: ["order_id", "line"],
        foreignKeys: tables[2]!.foreignKeys,
      }),
    ).toBe("Key order_id, line · 2 links");
    expect(keySummary({ foreignKeys: tables[1]!.foreignKeys })).toBe(
      "No primary key · 1 link",
    );
    expect(keySummary({})).toBe("No primary key");
  });
});

describe("S3", () => {
  type S3Form = Extract<ConnectionForm, { kind: "s3" }>;
  const blank = emptyForm("s3") as S3Form;
  const form = (f: Partial<S3Form>): S3Form => ({ ...blank, ...f });

  it("starts in role mode with every field blank", () => {
    expect(emptyForm("s3")).toEqual({
      kind: "s3",
      bucket: "",
      prefix: "",
      region: "",
      access: "role",
      roleArn: "",
      accessKeyId: "",
      secretAccessKey: "",
      endpoint: "",
    });
  });

  it("sends a role, trimmed, with only the settings filled in", () => {
    expect(
      toConfig(
        form({
          bucket: " acme-exports ",
          roleArn: " arn:aws:iam::123456789012:role/helix-foundry ",
          // Key fields typed before switching to a role are never sent.
          accessKeyId: "AKIA1",
          secretAccessKey: "s3cret",
          endpoint: "https://r2.example.com",
        }),
      ),
    ).toEqual({
      s3: {
        bucket: "acme-exports",
        role: { roleArn: "arn:aws:iam::123456789012:role/helix-foundry" },
      },
    });
    expect(
      toConfig(
        form({
          bucket: "b",
          prefix: " foundry/ ",
          region: " eu-west-2 ",
          roleArn: "arn",
        }),
        ["orders", "customers"],
      ),
    ).toEqual({
      s3: {
        bucket: "b",
        prefix: "foundry/",
        region: "eu-west-2",
        tables: ["orders", "customers"],
        role: { roleArn: "arn" },
      },
    });
  });

  it("sends a key with its secret and endpoint, never the role", () => {
    expect(
      toConfig(
        form({
          bucket: "b",
          access: "keys",
          roleArn: "arn:ignored",
          accessKeyId: " AKIAEXAMPLE ",
          secretAccessKey: " s3cret\n",
          endpoint: " https://abc.r2.cloudflarestorage.com ",
        }),
      ),
    ).toEqual({
      s3: {
        bucket: "b",
        keys: {
          accessKeyId: "AKIAEXAMPLE",
          secretAccessKey: "s3cret",
          endpoint: "https://abc.r2.cloudflarestorage.com",
        },
      },
    });
    // An edit's blank secret is sent blank: the WFE keeps the stored one.
    expect(
      toConfig(form({ bucket: "b", access: "keys", accessKeyId: "AKIA" })),
    ).toEqual({
      s3: { bucket: "b", keys: { accessKeyId: "AKIA", secretAccessKey: "" } },
    });
  });

  it("edits a stored role or key, with the secret blank", () => {
    expect(
      formFromConfig("s3", {
        s3: {
          bucket: "acme-exports",
          prefix: "foundry/",
          region: "eu-west-2",
          tables: ["orders"],
          role: { roleArn: "arn:aws:iam::123456789012:role/r" },
        },
      }),
    ).toEqual(
      form({
        bucket: "acme-exports",
        prefix: "foundry/",
        region: "eu-west-2",
        roleArn: "arn:aws:iam::123456789012:role/r",
      }),
    );
    expect(
      formFromConfig("s3", {
        s3: {
          bucket: "b",
          prefix: "",
          region: "",
          tables: [],
          keys: {
            accessKeyId: "AKIAEXAMPLE",
            secretAccessKey: "",
            endpoint: "https://minio.example.com",
          },
        },
      }),
    ).toEqual(
      form({
        bucket: "b",
        access: "keys",
        accessKeyId: "AKIAEXAMPLE",
        endpoint: "https://minio.example.com",
      }),
    );
    // Sparse configs, as a gateway omits empty fields.
    expect(formFromConfig("s3", { s3: { keys: {} } })).toEqual(
      form({ access: "keys" }),
    );
    expect(formFromConfig("s3", { s3: { role: {} } })).toEqual(blank);
    expect(formFromConfig("s3", { stripe: {} })).toEqual(blank);
  });

  it("needs a role's ARN or a key's secret as its credential, also when editing", () => {
    expect(hasCredential(blank)).toBe(false);
    expect(hasCredential(form({ roleArn: " " }))).toBe(false);
    expect(hasCredential(form({ roleArn: "arn" }))).toBe(true);
    // A key ID alone is not a credential: the secret is.
    expect(hasCredential(form({ access: "keys", accessKeyId: "AKIA" }))).toBe(
      false,
    );
    expect(hasCredential(form({ access: "keys", secretAccessKey: "s" }))).toBe(
      true,
    );
    // Editing: a stored role is ready to test; a stored key needs its secret
    // again, because a test never reads the stored one.
    const storedRole = formFromConfig("s3", {
      s3: { bucket: "b", role: { roleArn: "arn" } },
    });
    const storedKey = formFromConfig("s3", {
      s3: { bucket: "b", keys: { accessKeyId: "AKIA", secretAccessKey: "" } },
    });
    expect(hasCredential(storedRole)).toBe(true);
    expect(hasCredential(storedKey)).toBe(false);
    expect(
      hasCredential({ ...storedKey, secretAccessKey: "new" } as ConnectionForm),
    ).toBe(true);
  });

  it("names a connection after its bucket and folder", () => {
    expect(suggestedName(blank)).toBe("Amazon S3");
    expect(suggestedName(form({ bucket: " acme-exports " }))).toBe(
      "acme-exports",
    );
    expect(
      suggestedName(
        form({ bucket: "acme-exports", prefix: "/foundry/daily/" }),
      ),
    ).toBe("acme-exports/foundry/daily");
    expect(suggestedName(form({ prefix: "foundry" }))).toBe("Amazon S3");
  });

  it("keeps a suggested name within the WFE's 120 bytes", () => {
    const bytes = (s: string) => new TextEncoder().encode(s).length;
    // The longest bucket name S3 allows.
    const bucket = "b".repeat(63);
    expect(MAX_NAME_BYTES).toBe(120);
    // Exactly 120 bytes stay whole.
    const exact = form({ bucket, prefix: "f".repeat(56) });
    expect(suggestedName(exact)).toBe(`${bucket}/${"f".repeat(56)}`);
    expect(bytes(suggestedName(exact))).toBe(120);
    // One byte more keeps the bucket and the folder's last segments that fit.
    expect(
      suggestedName(form({ bucket, prefix: "f".repeat(53) + "/abc" })),
    ).toBe(`${bucket}/…/abc`);
    const name = suggestedName(
      form({
        bucket,
        prefix:
          "warehouse/exports/foundry/daily/2026-10-19/orders-partitioned-by-region/",
      }),
    );
    // With foundry/ it would be 121 bytes.
    expect(name).toBe(
      `${bucket}/…/daily/2026-10-19/orders-partitioned-by-region`,
    );
    expect(bytes(name)).toBe(113);
    // A folder as long as S3 allows keeps only what fits.
    const deep = suggestedName(
      form({ bucket, prefix: "a/".repeat(511) + "last" }),
    );
    expect(deep.startsWith(`${bucket}/…/`)).toBe(true);
    expect(deep.endsWith("/a/last")).toBe(true);
    expect(bytes(deep)).toBeLessThanOrEqual(120);
    // A last segment too long for any tail is cut short.
    const long = suggestedName(form({ bucket, prefix: "x".repeat(200) }));
    expect(long).toBe(`${bucket}/${"x".repeat(53)}…`);
    expect(bytes(long)).toBe(120);
    // Bytes, not characters: é is two and 😀 four, never split.
    const accents = suggestedName(form({ bucket, prefix: "é".repeat(40) }));
    expect(accents).toBe(`${bucket}/${"é".repeat(26)}…`);
    expect(bytes(accents)).toBe(119);
    const emoji = suggestedName(form({ bucket, prefix: "😀".repeat(20) }));
    expect(emoji).toBe(`${bucket}/${"😀".repeat(13)}…`);
    expect(bytes(emoji)).toBe(119);
    // A bucket typed too long for a name (S3 refuses it at test) is cut too.
    expect(bytes(suggestedName(form({ bucket: "c".repeat(300) })))).toBe(120);
  });

  it("summarises a connector as its s3:// folder", () => {
    expect(
      sourceSummary(
        connector({
          kind: "s3",
          config: {
            s3: { bucket: "acme-exports", prefix: "foundry/", role: {} },
          },
          secretHint: "IAM role",
        }),
      ),
    ).toBe("s3://acme-exports/foundry/");
    expect(
      sourceSummary(connector({ kind: "s3", config: { s3: { keys: {} } } })),
    ).toBe("s3:///");
  });

  it("splits a pasted s3:// URI and leaves plain names alone", () => {
    expect(parseS3Uri("s3://b")).toEqual({ bucket: "b", prefix: "" });
    expect(parseS3Uri("s3://b/")).toEqual({ bucket: "b", prefix: "" });
    expect(parseS3Uri("s3://b/p")).toEqual({ bucket: "b", prefix: "p" });
    expect(parseS3Uri(" S3://b/p/q/ ")).toEqual({
      bucket: "b",
      prefix: "p/q/",
    });
    expect(parseS3Uri("acme-exports")).toBeNull();
    expect(parseS3Uri("acme-exports/foundry")).toBeNull();
    expect(parseS3Uri("https://acme.s3.amazonaws.com/x")).toBeNull();
  });

  it("writes the trust policy of the guide exactly", () => {
    const setup = {
      principalArn: "arn:aws:iam::000000000000:role/helix-foundry-connector",
      externalId: "helix-foundry-ws_acme",
    };
    const expected = {
      Version: "2012-10-17",
      Statement: [
        {
          Effect: "Allow",
          Principal: { AWS: setup.principalArn },
          Action: ["sts:AssumeRole", "sts:TagSession"],
          Condition: {
            StringEquals: { "sts:ExternalId": setup.externalId },
          },
        },
      ],
    };
    // Exact, key order included, as the customer pastes it.
    expect(s3TrustPolicy(setup)).toBe(JSON.stringify(expected, null, 2));
    expect(JSON.parse(s3TrustPolicy({})).Statement[0]).toMatchObject({
      Principal: { AWS: "" },
      Condition: { StringEquals: { "sts:ExternalId": "" } },
    });
  });

  it("writes the permissions policy for a folder or the whole bucket", () => {
    const withFolder = {
      Version: "2012-10-17",
      Statement: [
        {
          Sid: "List",
          Effect: "Allow",
          Action: "s3:ListBucket",
          Resource: "arn:aws:s3:::acme-exports",
          Condition: { StringLike: { "s3:prefix": ["foundry/*"] } },
        },
        {
          Sid: "Read",
          Effect: "Allow",
          Action: "s3:GetObject",
          Resource: "arn:aws:s3:::acme-exports/foundry/*",
        },
      ],
    };
    expect(s3PermissionsPolicy("acme-exports", "foundry/")).toBe(
      JSON.stringify(withFolder, null, 2),
    );
    // The folder as the WFE stores it: no leading slash, one trailing.
    for (const prefix of ["foundry", " /foundry ", "foundry/"])
      expect(s3PermissionsPolicy(" acme-exports ", prefix)).toBe(
        JSON.stringify(withFolder, null, 2),
      );
    expect(s3PermissionsPolicy("acme-exports", " ")).toBe(
      JSON.stringify(
        {
          Version: "2012-10-17",
          Statement: [
            {
              Sid: "List",
              Effect: "Allow",
              Action: "s3:ListBucket",
              Resource: "arn:aws:s3:::acme-exports",
            },
            {
              Sid: "Read",
              Effect: "Allow",
              Action: "s3:GetObject",
              Resource: "arn:aws:s3:::acme-exports/*",
            },
          ],
        },
        null,
        2,
      ),
    );
  });

  it("formats int64 byte counts in binary units", () => {
    expect(formatBytes(undefined)).toBe("0 B");
    expect(formatBytes("")).toBe("0 B");
    expect(formatBytes("nope")).toBe("0 B");
    expect(formatBytes("-5")).toBe("0 B");
    expect(formatBytes("1023")).toBe("1023 B");
    expect(formatBytes("1024")).toBe("1 KB");
    expect(formatBytes("1536")).toBe("1.5 KB");
    // Rounding up to 1024 moves to the next unit.
    expect(formatBytes("1048575")).toBe("1 MB");
    expect(formatBytes("1288490189")).toBe("1.2 GB");
    expect(formatBytes(String(20 * 2 ** 30))).toBe("20 GB");
    expect(formatBytes(String(14.2 * 2 ** 30))).toBe("14.2 GB");
    expect(formatBytes(String(3 * 2 ** 50))).toBe("3 PB");
    expect(formatBytes(String(2 ** 62))).toBe("4096 PB");
  });

  it("previews a table's rows as cells, cut to 80 characters", () => {
    const long = "x".repeat(81),
      emoji = "😀".repeat(81);
    const grid = previewGrid({
      columns: [
        { name: "id", type: "integer" },
        { name: "name", type: "string" },
        { name: "tags", type: "json" },
      ],
      preview: [
        '{"id":1,"name":"Ada","tags":["a","b"]}',
        `{"id":2,"name":"${long}","tags":null,"extra":true}`,
        `{"id":3,"name":"${emoji}"}`,
        "not json",
        "[1,2]",
        "null",
        '"a string"',
      ],
    });
    expect(grid.columns).toEqual(["id", "name", "tags", "extra"]);
    expect(grid.rows).toEqual([
      ["1", "Ada", '["a","b"]', null],
      ["2", "x".repeat(79) + "…", null, "true"],
      ["3", "😀".repeat(79) + "…", null, null],
    ]);
    expect(grid.rows[1]![1]).toHaveLength(80);
    // Exactly 80 characters stay whole.
    expect(
      previewGrid({ preview: [`{"a":"${"y".repeat(80)}"}`] }).rows[0]![0],
    ).toBe("y".repeat(80));
    // No columns (a bare table): the rows' keys, first seen first.
    expect(
      previewGrid({ preview: ['{"b":1}', '{"a":{"x":1},"b":2}'] }),
    ).toEqual({
      columns: ["b", "a"],
      rows: [
        ["1", null],
        ["2", '{"x":1}'],
      ],
    });
    expect(previewGrid({})).toEqual({ columns: [], rows: [] });
    expect(previewGrid({ columns: [{ name: "id" }] })).toEqual({
      columns: ["id"],
      rows: [],
    });
  });

  it("previews numbers exactly as the WFE wrote them", () => {
    // The WFE keeps big integers exact and decimals as their text.
    const row = [
      '{"id":9223372036854775807,"min":-9223372036854775808',
      ',"u64":18446744073709551615,"amount":12.30,"tiny":1e-7,"huge":1e400',
      ',"nested":{"n":12.30,"id":9223372036854775807,"s":"}],{\\"x\\":1"}',
      ',"list":[1.50,[2.0,{"a":[]}]],"text":"say \\"hi\\" \\\\","word":"null"',
      ',"none":null,"ok":true,"no":false,"empty":{}}',
    ].join("");
    expect(Object.keys(JSON.parse(row))).toHaveLength(14);
    const grid = previewGrid({ preview: [row] });
    expect(grid.columns).toEqual([
      "id",
      "min",
      "u64",
      "amount",
      "tiny",
      "huge",
      "nested",
      "list",
      "text",
      "word",
      "none",
      "ok",
      "no",
      "empty",
    ]);
    expect(grid.rows).toEqual([
      [
        "9223372036854775807",
        "-9223372036854775808",
        "18446744073709551615",
        "12.30",
        "1e-7",
        "1e400",
        '{"n":12.30,"id":9223372036854775807,"s":"}],{\\"x\\":1"}',
        '[1.50,[2.0,{"a":[]}]]',
        'say "hi" \\',
        // A string "null" is text; only JSON null is empty.
        "null",
        null,
        "true",
        "false",
        "{}",
      ],
    ]);
    // Space between tokens is allowed, an escaped key is decoded, a repeated
    // key keeps its last value, and an empty object is a row of nulls.
    expect(
      previewGrid({
        columns: [{ name: "ab" }, { name: "c" }],
        preview: [
          ' { "\\u0061b" : 1.10 , "c" : [ 1 , 2 ] , "c" : 3.00 }\n',
          "{}",
        ],
      }).rows,
    ).toEqual([
      ["1.10", "3.00"],
      [null, null],
    ]);
  });
});
