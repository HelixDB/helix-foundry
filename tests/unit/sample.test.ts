import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fileProblem, formatOfName } from "@/web/areas/documents/model";
import { MAX_GOAL, MAX_TABLES } from "@/web/areas/ontology/model";
import { SAMPLE } from "@/web/areas/ontology/sample";
import { readPdfText } from "../../mock/pdf-text";
import { readTableFile, type Row } from "../../mock/table-files";

// The sample company the first ontology's wizard offers is what
// scripts/sample-company.mjs wrote: every file it lists is served and
// uploadable, and its tables and documents name each other's rows, so a
// draft from them has links to find.

const DIR = join(__dirname, "..", "..", "public", "samples", "kestrel");
const bytes = (name: string) => readFileSync(join(DIR, name));

async function table(name: string): Promise<Row[]> {
  const read = await readTableFile("text/csv", bytes(name));
  if (!read.ok) throw new Error(read.message);
  return read.rows;
}

describe("the sample company", () => {
  it("lists exactly the files served, each one the WFE takes", () => {
    expect(readdirSync(DIR).sort()).toEqual([...SAMPLE.files].sort());
    expect(SAMPLE.base).toBe("/samples/kestrel/");
    for (const name of SAMPLE.files)
      expect(fileProblem({ name, size: statSync(join(DIR, name)).size })).toBe(
        undefined,
      );
  });

  it("is the tables kestrel-db serves and the backend's ready-made draft maps", async () => {
    const root = join(__dirname, "..", "..");
    const draft = readFileSync(
      join(root, "backend", "sample", "kestrel.json"),
      "utf8",
    );
    const named = new Set(
      [...draft.matchAll(/\{\{table:([^{}]+)\}\}/g)].map((m) => m[1]),
    );
    expect([...named].sort()).toEqual([...SAMPLE.database.tables].sort());
    // kestrel.sql holds each CSV's rows, and no other table.
    const sql = readFileSync(
      join(root, "local", "kestrel", "kestrel.sql"),
      "utf8",
    );
    const created = [...sql.matchAll(/CREATE TABLE (\w+) \(/g)].map(
      (m) => m[1],
    );
    expect(created.sort()).toEqual([...SAMPLE.database.tables].sort());
    for (const t of SAMPLE.database.tables) {
      const inserted = sql
        .slice(sql.indexOf(`INSERT INTO ${t} `))
        .split(";")[0]!
        .split("\n")
        .filter((l) => l.startsWith("  (")).length;
      expect(inserted, t).toBe((await table(`${t}.csv`)).length);
    }
    expect(SAMPLE.files.filter((f) => !f.endsWith(".csv"))).toEqual([
      ...SAMPLE.documents,
    ]);
    expect(SAMPLE.id).toBe("kestrel");
  });

  it("fits one generation, and its set and goal fit the WFE's limits", () => {
    const tables = SAMPLE.files.filter(
      (f) => formatOfName(f)?.kind === "table",
    );
    expect(tables).toHaveLength(6);
    expect(tables.length).toBeLessThanOrEqual(MAX_TABLES);
    expect([...SAMPLE.set.name].length).toBeLessThanOrEqual(120);
    expect([...SAMPLE.set.description].length).toBeLessThanOrEqual(1000);
    expect([...SAMPLE.goal].length).toBeLessThanOrEqual(MAX_GOAL);
  });

  it("reads every PDF's text", async () => {
    for (const name of SAMPLE.files.filter((f) => f.endsWith(".pdf"))) {
      const read = await readPdfText(bytes(name));
      expect(read.ok, name).toBe(true);
      expect(read.ok && read.text, name).toContain("Kestrel Software, Inc.");
    }
  });

  it("has tables whose ids are unique and whose references resolve", async () => {
    const [accounts, contacts, subscriptions, invoices, tickets, usage] =
      await Promise.all(
        [
          "accounts.csv",
          "contacts.csv",
          "subscriptions.csv",
          "invoices.csv",
          "support_tickets.csv",
          "product_usage.csv",
        ].map(table),
      );
    const ids = (rows: Row[], key: string) => {
      const all = rows.map((r) => String(r[key]));
      expect(new Set(all).size, key).toBe(all.length);
      return new Set(all);
    };
    const accountIds = ids(accounts!, "account_id"),
      contactIds = ids(contacts!, "contact_id"),
      subscriptionIds = ids(subscriptions!, "subscription_id");
    ids(invoices!, "invoice_id");
    ids(tickets!, "ticket_id");
    expect(accountIds.size).toBe(40);

    const contactAccount = new Map(
      contacts!.map((c) => [c.contact_id, c.account_id]),
    );
    const subscriptionAccount = new Map(
      subscriptions!.map((s) => [s.subscription_id, s.account_id]),
    );
    for (const c of contacts!) expect(accountIds).toContain(c.account_id);
    for (const s of subscriptions!) expect(accountIds).toContain(s.account_id);
    for (const u of usage!) expect(accountIds).toContain(u.account_id);
    for (const v of invoices!) {
      expect(subscriptionIds).toContain(v.subscription_id);
      expect(subscriptionAccount.get(v.subscription_id)).toBe(v.account_id);
    }
    for (const t of tickets!) {
      expect(contactIds).toContain(t.requester_id);
      expect(contactAccount.get(t.requester_id)).toBe(t.account_id);
    }
    // Every account has its people and one subscription in force, unless
    // it left.
    for (const id of accountIds) {
      expect(
        contacts!.some((c) => c.account_id === id),
        id,
      ).toBe(true);
      const own = subscriptions!.filter((s) => s.account_id === id);
      expect(own.filter((s) => s.status !== "canceled").length, id).toBe(
        own.some(
          (s) => s.cancel_reason && s.cancel_reason !== "Upgraded to Growth",
        )
          ? 0
          : 1,
      );
    }
  });

  it("names in its documents only tickets, invoices and people its tables hold", async () => {
    const [contacts, invoices, tickets] = await Promise.all(
      ["contacts.csv", "invoices.csv", "support_tickets.csv"].map(table),
    );
    const text = (
      await Promise.all(
        SAMPLE.files.map(async (name) => {
          if (name.endsWith(".md")) return bytes(name).toString("utf8");
          if (!name.endsWith(".pdf")) return "";
          const read = await readPdfText(bytes(name));
          return read.ok ? read.text : "";
        }),
      )
    ).join("\n");
    const ticketIds = new Set(tickets!.map((t) => t.ticket_id)),
      invoiceIds = new Set(invoices!.map((v) => v.invoice_id));
    const named = (pattern: RegExp) =>
      [...text.matchAll(pattern)].map((m) => m[0]);
    expect(named(/\bT-\d{4}\b/g).length).toBeGreaterThan(10);
    for (const id of named(/\bT-\d{4}\b/g)) expect(ticketIds).toContain(id);
    expect(named(/\bINV-\d{5}\b/g)).not.toHaveLength(0);
    for (const id of named(/\bINV-\d{5}\b/g)) expect(invoiceIds).toContain(id);
    for (const person of [
      "Dana Okafor",
      "Miguel Serrano",
      "Ingrid Halvorsen",
      "Tom Becker",
      "Priya Natarajan",
      "Gareth Lloyd",
      "Marcus Bell",
    ]) {
      expect(text).toContain(person);
      expect(contacts!.map((c) => c.full_name)).toContain(person);
    }
  });
});

// Where the sample's database runs: a local stack's kestrel-db with no TLS,
// or a stamp's RDS, trusted through public/samples/rds-ca.pem.
describe("the sample company's database", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
    vi.resetModules();
  });
  const load = async (env: Record<string, string>) => {
    for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
    vi.resetModules();
    return import("@/web/areas/ontology/sample");
  };

  it("is the stack's kestrel-db in a local build, with no TLS", async () => {
    const sample = await load({
      NEXT_PUBLIC_FOUNDRY_MODE: "local",
      NEXT_PUBLIC_SAMPLE_DB_HOST: "kestrel-db",
    });
    expect(sample.SAMPLE_TOUR).toBe(true);
    expect(await sample.sampleDatabaseConfig()).toEqual({
      postgres: {
        host: "kestrel-db",
        port: 5432,
        database: "kestrel",
        user: "kestrel_reader",
        password: "kestrel",
        disableTls: true,
      },
    });
  });

  it("is the stamp's RDS when hosted, trusting Amazon's RDS roots", async () => {
    const host = "helix-dev-kestrel.example.us-east-2.rds.amazonaws.com";
    const sample = await load({ NEXT_PUBLIC_SAMPLE_DB_HOST: host });
    const pem = readFileSync(join(DIR, "..", "rds-ca.pem"), "utf8");
    const fetch = vi.fn(async (url: string) =>
      url === "/samples/rds-ca.pem"
        ? new Response(pem)
        : new Response("", { status: 404 }),
    );
    vi.stubGlobal("fetch", fetch);
    const config = await sample.sampleDatabaseConfig();
    if (!("postgres" in config)) throw new Error("not a PostgreSQL config");
    expect(config.postgres).toMatchObject({ host, ca: pem });
    expect(config.postgres).not.toHaveProperty("disableTls");
    // us-east-1's and us-east-2's roots: RSA2048, RSA4096 and ECC384 each.
    expect(pem.match(/BEGIN CERTIFICATE/g)).toHaveLength(6);
  });

  it("says so when the certificate can't be read", async () => {
    const sample = await load({ NEXT_PUBLIC_SAMPLE_DB_HOST: "db.example" });
    vi.stubGlobal("fetch", async () => new Response("", { status: 503 }));
    await expect(sample.sampleDatabaseConfig()).rejects.toThrow(
      "couldn’t read the sample database’s certificate",
    );
  });

  it("isn't offered on a stamp with none", async () => {
    const sample = await load({ NEXT_PUBLIC_SAMPLE_DB_HOST: "" });
    expect(sample.SAMPLE_TOUR).toBe(false);
  });

  it("is set per build: local, dev and none yet for prod", async () => {
    const host = async (env: Record<string, string>) => {
      vi.unstubAllEnvs();
      for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
      vi.resetModules();
      const config = (await import("../../next.config")).default;
      return config.env?.NEXT_PUBLIC_SAMPLE_DB_HOST;
    };
    expect(await host({ NEXT_PUBLIC_FOUNDRY_MODE: "local" })).toBe(
      "kestrel-db",
    );
    expect(await host({ NEXT_PUBLIC_APP_ENV: "prod" })).toBe("");
    expect(await host({ NEXT_PUBLIC_APP_ENV: "dev" })).toMatch(
      /^helix-dev-kestrel\.[a-z0-9]+\.us-east-2\.rds\.amazonaws\.com$/,
    );
  });
});
