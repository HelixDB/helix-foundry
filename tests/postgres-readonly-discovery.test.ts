import { randomUUID } from "node:crypto";
import pg from "pg";
import { expect, it } from "vitest";
import { discover } from "../apps/api/src/connectors.js";

// Run against a disposable PostgreSQL database with a superuser test connection:
// TEST_POSTGRES_DISCOVERY=1 TEST_POSTGRES_ADMIN_URL=postgresql://... pnpm exec vitest run tests/postgres-readonly-discovery.test.ts
it.skipIf(process.env.TEST_POSTGRES_DISCOVERY !== "1")(
  "discovers PostgreSQL keys and composite foreign keys with a SELECT-only role",
  async () => {
    const adminUrl = new URL(
      process.env.TEST_POSTGRES_ADMIN_URL ||
        "postgresql://postgres:foundry-test-only@127.0.0.1:54877/foundry_test?sslmode=disable",
    );
    const suffix = randomUUID().replaceAll("-", "").slice(0, 12);
    const schema = `discovery_${suffix}`;
    const role = `reader_${suffix}`;
    const password = "fixture-only-readonly";
    const quote = (id: string) => '"' + id.replaceAll('"', '""') + '"';
    const admin = new pg.Client({ connectionString: adminUrl.toString() });
    let reader: pg.Client | undefined;
    await admin.connect();

    try {
      await admin.query(`CREATE SCHEMA ${quote(schema)}`);
      await admin.query(
        `CREATE TABLE ${quote(schema)}.customers (
          tenant_id INT, id INT, PRIMARY KEY (tenant_id, id)
        )`,
      );
      await admin.query(
        `CREATE TABLE ${quote(schema)}.orders (
          id INT PRIMARY KEY, tenant_id INT, customer_id INT,
          FOREIGN KEY (tenant_id, customer_id)
            REFERENCES ${quote(schema)}.customers (tenant_id, id)
        )`,
      );
      await admin.query(
        `CREATE TABLE ${quote(schema)}.hidden (id INT PRIMARY KEY)`,
      );
      await admin.query(
        `CREATE ROLE ${quote(role)} LOGIN PASSWORD '${password}'`,
      );
      await admin.query(
        `GRANT USAGE ON SCHEMA ${quote(schema)} TO ${quote(role)}`,
      );
      await admin.query(
        `GRANT SELECT ON ${quote(schema)}.customers, ${quote(schema)}.orders TO ${quote(role)}`,
      );

      const readerUrl = new URL(adminUrl);
      readerUrl.username = role;
      readerUrl.password = password;
      reader = new pg.Client({ connectionString: readerUrl.toString() });
      await reader.connect();

      const oldQuery = await reader.query(
        "SELECT table_name FROM information_schema.table_constraints WHERE table_schema=$1",
        [schema],
      );
      expect(oldQuery.rows).toEqual([]);

      const found: any = await discover("postgres", {
        connectionString: readerUrl.toString(),
      });
      const keys = found.keys
        .filter((k: any) => k.table_schema === schema)
        .map((k: any) => [k.table_name, k.column_name, k.constraint_type]);
      expect(keys).toEqual(
        expect.arrayContaining([
          ["customers", "tenant_id", "PRIMARY KEY"],
          ["customers", "id", "PRIMARY KEY"],
          ["orders", "id", "PRIMARY KEY"],
          ["orders", "tenant_id", "FOREIGN KEY"],
          ["orders", "customer_id", "FOREIGN KEY"],
        ]),
      );
      expect(keys).toHaveLength(5);

      const foreignKeys = found.foreignKeys
        .filter((k: any) => k.table_schema === schema)
        .map((k: any) => [
          k.table_name,
          k.column_name,
          k.referenced_schema,
          k.referenced_table,
          k.referenced_column,
        ]);
      expect(foreignKeys).toEqual([
        ["orders", "tenant_id", schema, "customers", "tenant_id"],
        ["orders", "customer_id", schema, "customers", "id"],
      ]);
      expect(
        found.tables
          .filter((t: any) => t.schema === schema)
          .map((t: any) => t.name)
          .sort(),
      ).toEqual(["customers", "orders"]);
      expect(found.keys.some((k: any) => k.table_name === "hidden")).toBe(
        false,
      );
      expect(
        found.foreignKeys.some((k: any) => k.table_name === "hidden"),
      ).toBe(false);
    } finally {
      await reader?.end();
      await admin.query(`DROP SCHEMA IF EXISTS ${quote(schema)} CASCADE`);
      await admin.query(`DROP ROLE IF EXISTS ${quote(role)}`);
      await admin.end();
    }
  },
);
