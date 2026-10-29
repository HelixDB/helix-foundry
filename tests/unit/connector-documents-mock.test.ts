import { createHash } from "node:crypto";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { FoundryDocument, FoundryDocumentSet } from "@/shared/wfe";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import type { MockStream } from "../../mock/routes/data-sources";
import { writeConnectorDocuments } from "../../mock/routes/documents";
import { config, db, resetState } from "../../mock/state";

// A connector's documents in the mock WFE, as the reader release serves
// them and the writer release writes them: a Granola, Slack, Notion or
// Google Workspace sync fills a set per stream, newest first and capped; those sets and documents are the
// connector's alone; the upload rules, the workspace's list and the set cap
// leave them out; `ids` reads them by id; and deleting the connection
// deletes them.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
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
      principal: { user: db.users.get("usr_ada")!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const refusal = (r: MockReply & { body: any }) => [r.body.code, r.body.message];
const FAILED_PRECONDITION = 9,
  INVALID_ARGUMENT = 3;

const WS = "/v1/workspaces/ws_acme/foundry",
  DOCS = `${WS}/documents`,
  SETS = `${WS}/document-sets`,
  GENERAL = "set_acme_general";
const sha = (text: string) =>
  createHash("sha256").update(Buffer.from(text)).digest("hex");

/** A Granola connection, synced once; the sync takes 3 s. */
async function granola(transcripts = false, name = "Team Granola") {
  const created = await call("POST", `${WS}/connectors`, {
    name,
    config: { granola: { apiKey: "grn_team1234", transcripts } },
  });
  expect(created.status).toBe(200);
  const id: string = created.body.connector.id;
  await sync(id);
  return id;
}
async function sync(connectorId: string) {
  const queued = await call("POST", `${WS}/connectors/${connectorId}:sync`, {});
  expect(queued.status, JSON.stringify(queued.body)).toBe(200);
  vi.setSystemTime(Date.now() + 3000);
}
const sets = async (): Promise<FoundryDocumentSet[]> =>
  (await call("GET", SETS)).body.sets;
const setOf = async (stream: string) =>
  (await sets()).find((s) => s.connector?.stream === stream);
const page = async (
  query: string,
): Promise<{ documents: FoundryDocument[]; nextPageToken: string }> => {
  const r = await call("GET", `${DOCS}?${query}`);
  expect(r.status, JSON.stringify(r.body)).toBe(200);
  return r.body;
};
const byIds = async (ids: string[]) =>
  (await page(ids.map((id) => `ids=${id}`).join("&"))).documents;
const externalIds = (docs: FoundryDocument[]) =>
  docs.map((d) => d.source?.externalId);
const upload = (setId: string, text = "# Minutes\n", token = "tok") =>
  call("POST", DOCS, {
    name: "minutes.md",
    mediaType: "text/markdown",
    sizeBytes: String(Buffer.byteLength(text)),
    sha256: sha(text),
    clientToken: token,
    setId,
  });

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-23T12:00:00Z"));
  resetState();
});
afterEach(() => vi.useRealTimers());

describe("a Granola sync's documents", () => {
  it("fills a Notes set with every note, newest at the source first, 50 a page", async () => {
    const connectorId = await granola();
    const notes = await setOf("notes");
    expect(notes).toMatchObject({
      name: "Notes",
      documents: 418,
      connector: {
        connectorId,
        stream: "notes",
        maxDocuments: 1000,
        connectorName: "Team Granola",
      },
    });
    expect(notes!.description).toContain("summary");
    // Transcripts are off: no set for them.
    expect(await setOf("transcripts")).toBeUndefined();
    // A person's set has no connector.
    expect((await sets())[0]).toMatchObject({ id: GENERAL, connector: null });

    const first = await page(`setId=${notes!.id}`);
    expect(first.documents).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    expect(first.documents[0]).toMatchObject({
      name: "Weekly pipeline review",
      mediaType: "text/markdown",
      status: "uploaded",
      setId: notes!.id,
      source: {
        externalId: "not_mock0001",
        version: "1|2026-10-20T16:00:00Z",
        url: "https://notes.granola.ai/d/not_mock0001",
        at: "2026-10-20T15:00:00.000Z",
      },
    });
    const at = first.documents.map((d) => d.source!.at!);
    expect(at).toEqual([...at].sort().reverse());
    const second = await page(
      `setId=${notes!.id}&pageToken=${first.nextPageToken}`,
    );
    expect(second.documents[0]!.source!.at! < at.at(-1)!).toBe(true);
    // The note's text, as Markdown, with its header.
    const text = (
      await call("GET", `${DOCS}/${first.documents[0]!.id}:content`)
    ).body.text;
    expect(text).toMatch(
      /^# Weekly pipeline review\n\nDate: 2026-10-20 15:00 UTC\nAttendees: Ada Lovelace <ada@example\.com>, Alan Turing <alan@example\.com>\nNote id: not_mock0001\n\n## Summary\n/,
    );
  });

  it("adds a Transcripts set when they are turned on, and empties it when they are off", async () => {
    const connectorId = await granola(true);
    const transcripts = await setOf("transcripts");
    expect(transcripts).toMatchObject({ name: "Transcripts", documents: 418 });
    const [newest] = (await page(`setId=${transcripts!.id}`)).documents;
    expect(newest).toMatchObject({ mediaType: "text/plain" });
    expect(
      (await call("GET", `${DOCS}/${newest!.id}:content`)).body.text,
    ).toContain("\nAda Lovelace: Let's start with weekly pipeline review.\n");
    // The connection's settings say so, and come back without the key.
    const read = await call("GET", `${WS}/connectors/${connectorId}`);
    expect(read.body.connector.config).toEqual({
      granola: { apiKey: "", transcripts: true },
    });

    // Turned off: the next sync (queued by the change) empties the set and
    // keeps it, with the same id.
    const changed = await call("PATCH", `${WS}/connectors/${connectorId}`, {
      config: { granola: { transcripts: false } },
    });
    expect(changed.body.connector.config.granola.transcripts).toBe(false);
    vi.setSystemTime(Date.now() + 3000);
    expect(await setOf("transcripts")).toMatchObject({
      id: transcripts!.id,
      documents: 0,
    });
    expect((await setOf("notes"))!.documents).toBe(418);
    expect((await byIds([newest!.id]))[0]!.status).toBe("deleted");
  });

  it("keeps the newest documentsPerStream, and replaces an edited note on each sync", async () => {
    Object.assign(config, { documentsPerStream: 5 });
    const connectorId = await granola();
    const notes = (await setOf("notes"))!;
    expect(notes.documents).toBe(5);
    expect(notes.connector!.maxDocuments).toBe(5);
    const before = (await page(`setId=${notes.id}`)).documents;
    expect(externalIds(before)).toEqual([
      "not_mock0001",
      "not_mock0002",
      "not_mock0003",
      "not_mock0004",
      "not_mock0005",
    ]);
    expect(before[2]!.name).toBe("Pricing review (edited)");

    // Its title says it is edited at the source: the next sync reads a new
    // version and new text, a new document in its place. The rest are kept.
    await sync(connectorId);
    const after = (await page(`setId=${notes.id}`)).documents;
    expect(externalIds(after)).toEqual(externalIds(before));
    expect(after.map((d) => d.id)).toEqual(
      before.map((d, i) => (i === 2 ? after[2]!.id : d.id)),
    );
    expect(after[2]!.id).not.toBe(before[2]!.id);
    expect(after[2]!.source!.version).not.toBe(before[2]!.source!.version);
    expect((await byIds([before[2]!.id]))[0]).toMatchObject({
      status: "deleted",
      source: { externalId: "not_mock0003" },
    });

    // A lower cap removes the oldest; a higher one brings them back as new
    // documents.
    Object.assign(config, { documentsPerStream: 2 });
    await sync(connectorId);
    expect(externalIds((await page(`setId=${notes.id}`)).documents)).toEqual([
      "not_mock0001",
      "not_mock0002",
    ]);
    expect((await byIds([before[4]!.id]))[0]!.status).toBe("deleted");
    Object.assign(config, { documentsPerStream: 3 });
    await sync(connectorId);
    const again = (await page(`setId=${notes.id}`)).documents;
    expect(again).toHaveLength(3);
    expect(again[2]!.id).not.toBe(before[2]!.id);
  });

  it("keeps the same version, changes a new version of the same text in place, and counts an id once", async () => {
    const stream: MockStream = {
      name: "notes",
      table: "notes",
      title: "Notes",
      description: "",
      mediaType: "text/markdown",
    };
    const doc = (externalId: string, version: string, text = externalId) => ({
      externalId,
      version,
      at: "2026-10-01T00:00:00.000Z",
      title: `Note ${externalId}`,
      url: "https://notes.granola.ai/d/" + externalId,
      text,
    });
    const write = (...documents: ReturnType<typeof doc>[]) =>
      writeConnectorDocuments("ws_acme", "conn_x", [{ stream, documents }]);
    write(doc("a", "1"), doc("b", "1"));
    const setId = (await setOf("notes"))!.id;
    const first = (await page(`setId=${setId}`)).documents;
    // The same version is kept as it is; a new one with the same text takes
    // the new version, name and URL; an id offered again counts once; a URL
    // that isn't https is none.
    write(
      { ...doc("a", "1"), title: "Ignored" },
      { ...doc("b", "2"), title: "Renamed", url: "http://notes/b" },
      doc("b", "3", "other text"),
    );
    const next = (await page(`setId=${setId}`)).documents;
    expect(next.map((d) => d.id).sort()).toEqual(first.map((d) => d.id).sort());
    const b = next.find((d) => d.source!.externalId === "b")!;
    expect(b).toMatchObject({
      name: "Renamed",
      source: { version: "2", url: "" },
    });
    expect(next.find((d) => d.source!.externalId === "a")!.name).toBe("Note a");
    // A stream that was never on makes no set.
    writeConnectorDocuments("ws_acme", "conn_x", [
      { stream: { ...stream, name: "transcripts" }, documents: null },
    ]);
    expect(await setOf("transcripts")).toBeUndefined();
  });
});

/** A connection of any kind, synced once; the sync takes 3 s. */
async function connect(name: string, config: unknown) {
  const created = await call("POST", `${WS}/connectors`, { name, config });
  expect(created.status, JSON.stringify(created.body)).toBe(200);
  const id: string = created.body.connector.id;
  await sync(id);
  return id;
}
const googleKey = JSON.stringify({
  type: "service_account",
  private_key_id: "0123456789abcdef9f3a",
  private_key:
    "-----BEGIN PRIVATE KEY-----\nMIIEmock\n-----END PRIVATE KEY-----\n",
  client_email: "foundry@acme-data.iam.gserviceaccount.com",
});

describe("Slack's, Notion's and Google Workspace's documents", () => {
  it("fills Slack's Conversations with a document per channel and day, newest first", async () => {
    const connectorId = await connect("Acme Slack", {
      slack: { botToken: "xoxb-1-team1234" },
    });
    const conversations = await setOf("conversations");
    // 89 days before the sync's, two of every three in each of four
    // channels: the app isn't in #random.
    expect(conversations).toMatchObject({
      name: "Conversations",
      documents: 237,
      connector: {
        connectorId,
        stream: "conversations",
        maxDocuments: 1000,
        connectorName: "Acme Slack",
      },
    });
    expect(conversations!.description).toContain("thread");
    const first = await page(`setId=${conversations!.id}`);
    expect(first.documents).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    const [newest] = first.documents;
    expect(newest).toMatchObject({
      name: "#support, 2026-10-22",
      mediaType: "text/markdown",
      source: {
        externalId: "C0MOCKSUP:2026-10-22",
        url: "https://app.slack.com/client/T0MOCKACME/C0MOCKSUP",
        at: "2026-10-22T14:21:00.000Z",
      },
    });
    // The version is "1|" and 40 hex digits of a hash of its messages.
    expect(newest!.source!.version).toMatch(/^1\|[0-9a-f]{40}$/);
    const at = first.documents.map((d) => d.source!.at!);
    expect(at).toEqual([...at].sort().reverse());
    expect(first.documents.some((d) => d.name!.startsWith("#random"))).toBe(
      false,
    );
    // Each message after a blank line, its thread's replies listed under it.
    const text = (await call("GET", `${DOCS}/${newest!.id}:content`)).body.text;
    expect(text).toBe(
      [
        "# #support, 2026-10-22",
        "",
        "Channel: #support (C0MOCKSUP)",
        "",
        "**Hedy Lamarr** 09:23: Umbrella Holdings asked how to rotate their API key.",
        "  - **Margaret Hamilton** 09:35: Thanks, on it.",
        "",
        "**Ada Lovelace** 14:21: Initech can't reset passwords for two users, looking now.",
        "",
      ].join("\n"),
    );
    // A day's messages are the same at the next sync: nothing changes.
    await sync(connectorId);
    expect((await page(`setId=${conversations!.id}`)).documents).toEqual(
      first.documents,
    );
  });

  it("keeps fewer days for a shorter history and the newest documentsPerStream", async () => {
    await connect("Week", {
      slack: { botToken: "xoxb-1-week1234", historyDays: 7 },
    });
    // The 6 days before the sync's.
    const week = (await setOf("conversations"))!;
    expect(week.documents).toBe(16);
    const days = (await page(`setId=${week.id}`)).documents.map((d) =>
      d.name!.slice(-10),
    );
    expect(days.at(-1)! >= "2026-10-17").toBe(true);
    Object.assign(config, { documentsPerStream: 30 });
    await connect("Capped", { notion: { token: "ntn_team1234" } });
    expect(await setOf("pages")).toMatchObject({
      documents: 30,
      connector: { maxDocuments: 30 },
    });
  });

  it("writes no Slack document without channels:history, and none from an app in no channel", async () => {
    await connect("No history", { slack: { botToken: "xoxb-1-noscope1" } });
    expect(await setOf("conversations")).toBeUndefined();
    await connect("Lonely", { slack: { botToken: "xoxb-1-lonely1" } });
    expect(await setOf("conversations")).toMatchObject({
      name: "Conversations",
      documents: 0,
      connector: { connectorName: "Lonely" },
    });
  });

  it("fills Notion's Pages with each page that has a body", async () => {
    const connectorId = await connect("Acme Notion", {
      notion: { token: "ntn_nousers1234" },
    });
    const pages = await setOf("pages");
    // A secret that can't read users still reads the pages.
    expect(pages).toMatchObject({
      name: "Pages",
      documents: 60,
      connector: { connectorId, stream: "pages" },
    });
    const [newest] = (await page(`setId=${pages!.id}`)).documents;
    const id = "2a1f0001-6c3b-4d2e-9f1a-7b8c9d0e1f2a";
    expect(newest).toMatchObject({
      name: "Onboarding playbook",
      mediaType: "text/markdown",
      source: {
        externalId: id,
        version: "1|2026-10-27T14:05:00Z",
        url: `https://www.notion.so/Onboarding-playbook-${id.replaceAll("-", "")}`,
        at: "2026-10-27T14:05:00.000Z",
      },
    });
    expect(
      (await call("GET", `${DOCS}/${newest!.id}:content`)).body.text,
    ).toMatch(
      new RegExp(
        `^# Onboarding playbook\\n\\nLast edited: 2026-10-27 14:05 UTC\\nPage id: ${id}\\n\\n## Overview\\n`,
      ),
    );
  });

  it("fills Google Workspace's Docs with each Google Doc the user can open", async () => {
    const connectorId = await connect("Acme Drive", {
      google: {
        serviceAccountKey: googleKey,
        subject: "ada.member@example.com",
      },
    });
    const docs = await setOf("docs");
    // A subject who is no admin still reads Drive.
    expect(docs).toMatchObject({
      name: "Docs",
      documents: 14,
      connector: { connectorId, stream: "docs" },
    });
    const [newest] = (await page(`setId=${docs!.id}`)).documents;
    const id = "1mockDoc0001aBcDeFgHiJkLmNoPq";
    expect(newest).toMatchObject({
      name: "Q4 planning",
      source: {
        externalId: id,
        // Its modified time and Drive's version.
        version: "1|2026-10-27T16:20:00.000Z|40",
        url: `https://docs.google.com/document/d/${id}/edit?usp=drivesdk`,
      },
    });
    expect(
      (await call("GET", `${DOCS}/${newest!.id}:content`)).body.text,
    ).toMatch(
      new RegExp(
        `^# Q4 planning\\n\\nOwner: ada@example\\.com\\nLast modified: 2026-10-27 16:20 UTC\\nFile id: ${id}\\n\\n## Summary\\n`,
      ),
    );
  });
});

describe("a connector's sets and documents", () => {
  it("are the connector's: a person's changes are refused with where to make them", async () => {
    await granola();
    const notes = (await setOf("notes"))!;
    const [note] = (await page(`setId=${notes.id}`)).documents;
    const mine = await upload(GENERAL);
    expect(mine.status).toBe(200);
    const set =
        "this set belongs to the Team Granola connection; change its settings there",
      doc =
        "this document belongs to the Team Granola connection; change its settings there";
    const refused: [string, Promise<MockReply & { body: any }>, string][] = [
      ["upload into", upload(notes.id, "# Other\n", "tok-2"), set],
      [
        "move an upload into",
        call("POST", `${DOCS}/${mine.body.document.id}:move`, {
          setId: notes.id,
        }),
        set,
      ],
      ["rename", call("PATCH", `${SETS}/${notes.id}`, { name: "Mine" }), set],
      [
        "describe",
        call("PATCH", `${SETS}/${notes.id}`, { description: "Mine" }),
        set,
      ],
      ["delete", call("DELETE", `${SETS}/${notes.id}`), set],
      [
        "move a note out",
        call("POST", `${DOCS}/${note!.id}:move`, { setId: GENERAL }),
        doc,
      ],
      [
        "rename a note",
        call("PATCH", `${DOCS}/${note!.id}`, { name: "Mine.md" }),
        doc,
      ],
      ["delete a note", call("DELETE", `${DOCS}/${note!.id}`), doc],
    ];
    for (const [what, reply, message] of refused)
      expect(refusal(await reply), what).toEqual([
        FAILED_PRECONDITION,
        message,
      ]);
    // Nothing changed.
    expect(await setOf("notes")).toMatchObject({
      name: "Notes",
      description: notes.description,
      documents: 418,
    });
    expect((await byIds([note!.id]))[0]).toMatchObject({
      name: note!.name,
      setId: notes.id,
      status: "uploaded",
    });
  });

  it("take no place among the uploads, the 20 sets or their names", async () => {
    await granola();
    const notes = (await setOf("notes"))!;
    const [note] = (await page(`setId=${notes.id}`)).documents;
    // 418 notes, and an upload of a note's very bytes is a new upload.
    const text = (await call("GET", `${DOCS}/${note!.id}:content`)).body.text;
    const same = await upload(GENERAL, text);
    expect(same.status).toBe(200);
    expect(same.body).toMatchObject({ existing: false });
    expect(same.body.document.id).not.toBe(note!.id);
    // The workspace's list is its uploads.
    expect((await page("")).documents.map((d) => d.id)).toEqual([
      same.body.document.id,
    ]);
    // A set may take the name Notes, and 19 more sets fit beside General.
    expect((await call("POST", SETS, { name: "notes" })).status).toBe(200);
    for (let i = 0; i < 18; i++)
      expect((await call("POST", SETS, { name: `Set ${i}` })).status).toBe(200);
    expect(refusal(await call("POST", SETS, { name: "One more" }))).toEqual([
      FAILED_PRECONDITION,
      "a workspace holds up to 20 document sets; delete an empty one first",
    ]);
  });

  it("are read by id, either origin, deleted ones too, in the order asked", async () => {
    await granola();
    const notes = (await setOf("notes"))!;
    const [a, b] = (await page(`setId=${notes.id}`)).documents;
    const mine = (await upload(GENERAL)).body.document;
    await call("DELETE", `${DOCS}/${mine.id}`);
    expect(
      (await byIds([b!.id, "doc_nowhere", mine.id, a!.id, b!.id])).map((d) => [
        d.id,
        d.status,
      ]),
    ).toEqual([
      [b!.id, "uploaded"],
      [mine.id, "deleted"],
      [a!.id, "uploaded"],
    ]);
    expect((await page(`ids=${a!.id}`)).nextPageToken).toBe("");
    const many = Array.from({ length: 101 }, (_, i) => `ids=d${i}`).join("&");
    expect(refusal(await call("GET", `${DOCS}?${many}`))).toEqual([
      INVALID_ARGUMENT,
      "ids may name at most 100 documents",
    ]);
    for (const extra of [`setId=${notes.id}`, "pageToken=50"])
      expect(
        refusal(await call("GET", `${DOCS}?ids=${a!.id}&${extra}`)),
      ).toEqual([
        INVALID_ARGUMENT,
        "ids takes no set_id or page_token: it lists those documents only",
      ]);
  });

  it("follow the connection's name, and go with the connection", async () => {
    const connectorId = await granola(true);
    await call("PATCH", `${WS}/connectors/${connectorId}`, {
      name: "Sales calls",
    });
    expect((await setOf("notes"))!.connector!.connectorName).toBe(
      "Sales calls",
    );
    const notes = (await setOf("notes"))!;
    const [note] = (await page(`setId=${notes.id}`)).documents;
    expect(refusal(await call("DELETE", `${DOCS}/${note!.id}`))).toEqual([
      FAILED_PRECONDITION,
      "this document belongs to the Sales calls connection; change its settings there",
    ]);

    expect(
      (await call("DELETE", `${WS}/connectors/${connectorId}`)).status,
    ).toBe(200);
    expect((await sets()).map((s) => s.id)).toEqual([GENERAL]);
    expect((await byIds([note!.id]))[0]!.status).toBe("deleted");
    expect(refusal(await call("DELETE", `${DOCS}/${note!.id}`))).toEqual([
      FAILED_PRECONDITION,
      "this document belongs to a connection; change its settings there",
    ]);
  });
});
