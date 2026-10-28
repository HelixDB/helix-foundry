import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fileProblem } from "@/web/areas/documents/model";
import { MAX_GOAL, MAX_TABLES } from "@/web/areas/ontology/model";
import { SAMPLE } from "@/web/areas/ontology/sample";
import { readPdfText } from "../../mock/pdf-text";

// The sample company the first ontology's wizard offers is what
// scripts/sample-company.mjs wrote: every document it lists is served and
// uploadable, its tables are acme.sql's, and its tables and documents name
// each other's rows, so a draft from them has links to find.

const ROOT = join(__dirname, "..", "..");
const DIR = join(ROOT, "public", "samples", "acme");
const bytes = (name: string) => readFileSync(join(DIR, name));
const SQL = readFileSync(join(ROOT, "local", "acme", "acme.sql"), "utf8");

type Row = Record<string, string | number | null>;

/** A table's rows as acme.sql inserts them: text, a number or NULL each. */
function table(name: string): Row[] {
  const insert = new RegExp(
    `INSERT INTO ${name} \\(([^)]+)\\) VALUES\\n  \\(([\\s\\S]*?)\\);\\n`,
  ).exec(SQL);
  if (!insert) throw new Error(`acme.sql inserts no ${name}`);
  const cols = insert[1]!.split(", ");
  return insert[2]!.split("),\n  (").map((line) => {
    const values = [
      ...line.matchAll(/'((?:[^']|'')*)'|(NULL)|(-?\d+(?:\.\d+)?)/g),
    ].map(([, text, , number]) =>
      text !== undefined
        ? text.replaceAll("''", "'")
        : number !== undefined
          ? Number(number)
          : null,
    );
    expect(values, `${name}: ${line}`).toHaveLength(cols.length);
    return Object.fromEntries(cols.map((c, i) => [c, values[i] ?? null]));
  });
}

describe("the sample company", () => {
  it("lists exactly the documents served, each one the WFE takes", () => {
    expect(readdirSync(DIR).sort()).toEqual([...SAMPLE.documents].sort());
    expect(SAMPLE.base).toBe("/samples/acme/");
    for (const name of SAMPLE.documents)
      expect(fileProblem({ name, size: statSync(join(DIR, name)).size })).toBe(
        undefined,
      );
  });

  it("is the tables acme-db serves and the backend's ready-made draft maps", () => {
    const draft = readFileSync(
      join(ROOT, "backend", "sample", "acme.json"),
      "utf8",
    );
    const named = new Set(
      [...draft.matchAll(/\{\{table:([^{}]+)\}\}/g)].map((m) => m[1]),
    );
    expect([...named].sort()).toEqual([...SAMPLE.database.tables].sort());
    // acme.sql creates and fills each of them, and no other table.
    const created = [...SQL.matchAll(/CREATE TABLE (\w+) \(/g)].map(
      (m) => m[1],
    );
    expect(created.sort()).toEqual([...SAMPLE.database.tables].sort());
    expect(SAMPLE.database.tables.map((t) => table(t).length)).toEqual([
      40, 121, 45, 363, 327, 225,
    ]);
    expect(SAMPLE.id).toBe("acme");
  });

  it("fits one generation, and its set and goal fit the WFE's limits", () => {
    expect(SAMPLE.database.tables).toHaveLength(6);
    expect(SAMPLE.database.tables.length).toBeLessThanOrEqual(MAX_TABLES);
    expect([...SAMPLE.set.name].length).toBeLessThanOrEqual(120);
    expect([...SAMPLE.set.description].length).toBeLessThanOrEqual(1000);
    expect([...SAMPLE.goal].length).toBeLessThanOrEqual(MAX_GOAL);
  });

  it("reads every PDF's text", async () => {
    for (const name of SAMPLE.documents.filter((f) => f.endsWith(".pdf"))) {
      const read = await readPdfText(bytes(name));
      expect(read.ok, name).toBe(true);
      expect(read.ok && read.text, name).toContain("Acme Corporation");
    }
  });

  it("has tables whose ids are unique and whose references resolve", () => {
    const [accounts, contacts, subscriptions, invoices, tickets, usage] = [
      "accounts",
      "contacts",
      "subscriptions",
      "invoices",
      "support_tickets",
      "product_usage",
    ].map(table);
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
    const [contacts, invoices, tickets] = [
      "contacts",
      "invoices",
      "support_tickets",
    ].map(table);
    const text = (
      await Promise.all(
        SAMPLE.documents.map(async (name) => {
          if (name.endsWith(".md")) return bytes(name).toString("utf8");
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

// Where the sample's database runs: a local stack's acme-db with no TLS,
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

  it("is the stack's acme-db in a local build, with no TLS", async () => {
    const sample = await load({
      NEXT_PUBLIC_FOUNDRY_MODE: "local",
      NEXT_PUBLIC_SAMPLE_DB_HOST: "acme-db",
    });
    expect(await sample.sampleDatabaseConfig()).toEqual({
      postgres: {
        host: "acme-db",
        port: 5432,
        database: "acme",
        user: "acme_reader",
        password: "acme",
        disableTls: true,
      },
    });
  });

  it("is the stamp's RDS when hosted, trusting Amazon's RDS roots", async () => {
    const host = "helix-dev-sample.example.us-east-2.rds.amazonaws.com";
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

  it("is set per build: local, dev and prod", async () => {
    const host = async (env: Record<string, string>) => {
      vi.unstubAllEnvs();
      for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
      vi.resetModules();
      const config = (await import("../../next.config")).default;
      return config.env?.NEXT_PUBLIC_SAMPLE_DB_HOST;
    };
    expect(await host({ NEXT_PUBLIC_FOUNDRY_MODE: "local" })).toBe("acme-db");
    expect(await host({ NEXT_PUBLIC_APP_ENV: "prod" })).toMatch(
      /^helix-prod-sample\.[a-z0-9]+\.us-east-1\.rds\.amazonaws\.com$/,
    );
    expect(await host({ NEXT_PUBLIC_APP_ENV: "dev" })).toMatch(
      /^helix-dev-sample\.[a-z0-9]+\.us-east-2\.rds\.amazonaws\.com$/,
    );
  });
});
