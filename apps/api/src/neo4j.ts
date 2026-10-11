import { AppError, assert } from "./security.js";
import { checkedURL, type ConnectorConfig } from "./connectors.js";

// Neo4j over its HTTP Query API (Neo4j 5 and later), so no driver is needed.
// Each label becomes a dataset of its nodes and each relationship type a
// dataset of its relationships, keyed by Neo4j's element ids in columns named
// for the labels: a PLACED relationship from Customer to Order has customer_id
// and order_id columns that match customer_id in Customer and order_id in
// Order, which is how the suggested ontology links them.

// Tests shrink the page to exercise paging.
export const neo4jPaging = { size: 5000 };

type Shape = { from?: string; to?: string; ambiguous?: boolean };
type Column = {
  tableName: string;
  table_schema: string;
  name: string;
  type: string;
};

const slug = (s: string) =>
  s
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean)
    .join("_") || "node";
export const idColumn = (label: string) => slug(label) + "_id";
const quoted = (name: string) => "`" + name.replaceAll("`", "``") + "`";
// db.schema.relTypeProperties names types as :`PLACED`.
const unquoted = (s: string) =>
  /^:`(.*)`$/s.test(s) ? s.slice(2, -1).replaceAll("``", "`") : s;

// The Query API's typed format (application/vnd.neo4j.query) to JSON values.
// Integers beyond the safe range and temporal values stay strings, as the
// MySQL connector keeps big numbers and timestamps.
function plain(v: any): unknown {
  if (v === null || typeof v !== "object" || !("$type" in v)) return v;
  const { $type: type, _value: value } = v;
  switch (type) {
    case "Null":
      return null;
    case "Integer": {
      const n = Number(value);
      return Number.isSafeInteger(n) ? n : String(value);
    }
    case "Float": {
      const n = Number(value);
      return Number.isFinite(n) ? n : String(value);
    }
    case "Boolean":
      return Boolean(value);
    case "String":
      return String(value);
    case "List":
      return (value as unknown[]).map(plain);
    case "Map":
      return Object.fromEntries(
        Object.entries(value as Record<string, unknown>).map(([k, x]) => [
          k,
          plain(x),
        ]),
      );
    default:
      // Date, Time, DateTime, Duration, Point, Bytes and anything newer.
      return typeof value === "string" ? value : JSON.stringify(value);
  }
}

export async function neo4jQuery(
  c: ConnectorConfig,
  statement: string,
  parameters: Record<string, unknown> = {},
  options: { typed?: boolean; timeoutMs?: number } = {},
): Promise<Record<string, any>[]> {
  assert(
    c.url,
    "Enter the HTTP URL of the Neo4j server, such as http://localhost:7474",
  );
  let u: URL;
  try {
    u = checkedURL(String(c.url));
  } catch (error) {
    if (error instanceof AppError) throw error;
    throw new AppError(
      400,
      `"${c.url}" is not a valid URL. Use the HTTP URL of the Neo4j server, such as http://localhost:7474`,
    );
  }
  const database = String(c.database || "neo4j"),
    typed = options.typed !== false;
  const r = await fetch(
    `${u.origin}${u.pathname.replace(/\/+$/, "")}/db/${encodeURIComponent(database)}/query/v2`,
    {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: typed ? "application/vnd.neo4j.query" : "application/json",
        authorization:
          "Basic " +
          Buffer.from(`${c.user || ""}:${c.password || ""}`).toString("base64"),
      },
      body: JSON.stringify({ statement, parameters }),
      redirect: "error",
      signal: AbortSignal.timeout(options.timeoutMs || 30000),
    },
  );
  const text = await r.text();
  let body: any = null;
  try {
    body = JSON.parse(text);
  } catch {
    /* Not JSON: reported by status below. */
  }
  const detail = body?.errors?.[0]?.message;
  if (r.status === 401 || r.status === 403)
    throw new Error("Authentication failed: " + (detail || r.status));
  if (r.status === 404)
    throw new AppError(
      400,
      `Database "${database}" was not found. Check the database name, and that this is Neo4j 5 or later with the Query API enabled.`,
    );
  if (detail) throw new AppError(400, "Neo4j: " + detail);
  assert(r.ok, `Neo4j returned HTTP ${r.status}`);
  const fields: string[] = body?.data?.fields || [],
    values: unknown[][] = body?.data?.values || [];
  return values.map((row) =>
    Object.fromEntries(fields.map((f, i) => [f, plain(row[i])])),
  );
}

// Which labels each relationship type connects, from Neo4j's own schema
// summary. A type that connects several label pairs gets generic from_id and
// to_id columns instead of columns named for one pair.
async function relationshipShapes(c: ConnectorConfig) {
  const [row] = await neo4jQuery(
    c,
    "CALL db.schema.visualization() YIELD nodes, relationships RETURN nodes, relationships",
    {},
    { typed: false },
  );
  const labelOf = new Map<string, string>(
    ((row?.nodes || []) as any[]).map((n) => [
      n.elementId,
      String(n.properties?.name ?? n.labels?.[0] ?? ""),
    ]),
  );
  const shapes = new Map<string, Shape>();
  for (const r of (row?.relationships || []) as any[]) {
    const from = labelOf.get(r.startNodeElementId),
      to = labelOf.get(r.endNodeElementId),
      seen = shapes.get(r.type);
    if (!seen) shapes.set(r.type, { from, to });
    else if (seen.from !== from || seen.to !== to) seen.ambiguous = true;
  }
  return shapes;
}
function relationshipColumns(type: string, shape: Shape = {}) {
  const fromLabel = shape.from,
    toLabel = shape.to;
  if (!fromLabel || !toLabel || shape.ambiguous)
    return { id: idColumn(type), from: "from_id", to: "to_id" };
  const from = idColumn(fromLabel),
    to = toLabel === fromLabel ? "to_" + idColumn(toLabel) : idColumn(toLabel);
  let id = idColumn(type);
  if (id === from || id === to) id = slug(type) + "_relationship_id";
  return { id, from, to };
}
// Ids first; a property with the same name as an id column keeps its value
// under a _property suffix rather than being dropped.
function merge(ids: Record<string, unknown>, props: unknown) {
  const row: Record<string, unknown> = { ...ids };
  if (props && typeof props === "object")
    for (const [k, v] of Object.entries(props as Record<string, unknown>))
      row[k in ids ? k + "_property" : k] = v;
  return row;
}

// One page of nodes with a label, or of relationships of a type, ordered by
// element id so the next page starts after the last one seen.
async function page(
  c: ConnectorConfig,
  name: string,
  relationship: boolean,
  after: string,
  limit: number,
  shapes: Map<string, Shape>,
) {
  if (relationship) {
    const cols = relationshipColumns(name, shapes.get(name));
    const rows = await neo4jQuery(
      c,
      `MATCH (a)-[r:${quoted(name)}]->(b) WHERE elementId(r) > $after RETURN elementId(r) AS id, elementId(a) AS from, elementId(b) AS to, r{.*} AS props ORDER BY id LIMIT $limit`,
      { after, limit },
      { timeoutMs: Number(c.timeoutMs || 120000) },
    );
    return {
      last: rows.at(-1)?.id as string | undefined,
      rows: rows.map((r) =>
        merge(
          { [cols.id]: r.id, [cols.from]: r.from, [cols.to]: r.to },
          r.props,
        ),
      ),
    };
  }
  const id = idColumn(name);
  const rows = await neo4jQuery(
    c,
    `MATCH (n:${quoted(name)}) WHERE elementId(n) > $after RETURN elementId(n) AS id, n{.*} AS props ORDER BY id LIMIT $limit`,
    { after, limit },
    { timeoutMs: Number(c.timeoutMs || 120000) },
  );
  return {
    last: rows.at(-1)?.id as string | undefined,
    rows: rows.map((r) => merge({ [id]: r.id }, r.props)),
  };
}

// The columns of every label and relationship type: the id columns, then the
// properties Neo4j has seen on them.
async function allColumns(
  c: ConnectorConfig,
  labels: string[],
  types: string[],
  shapes: Map<string, Shape>,
) {
  const nodeProps = await neo4jQuery(
    c,
    "CALL db.schema.nodeTypeProperties() YIELD nodeLabels, propertyName, propertyTypes RETURN nodeLabels, propertyName, propertyTypes",
  );
  const relProps = await neo4jQuery(
    c,
    "CALL db.schema.relTypeProperties() YIELD relType, propertyName, propertyTypes RETURN relType, propertyName, propertyTypes",
  );
  const typeOf = (p: any) =>
    Array.isArray(p.propertyTypes) ? p.propertyTypes.join(" | ") : "Any";
  const columns: Column[] = [];
  for (const label of labels) {
    columns.push({
      tableName: label,
      table_schema: "node",
      name: idColumn(label),
      type: "String",
    });
    for (const p of nodeProps)
      if (p.propertyName && (p.nodeLabels as string[])?.includes(label))
        columns.push({
          tableName: label,
          table_schema: "node",
          name: String(p.propertyName),
          type: typeOf(p),
        });
  }
  for (const type of types) {
    const cols = relationshipColumns(type, shapes.get(type));
    for (const name of [cols.id, cols.from, cols.to])
      columns.push({
        tableName: type,
        table_schema: "relationship",
        name,
        type: "String",
      });
    for (const p of relProps)
      if (p.propertyName && unquoted(String(p.relType)) === type)
        columns.push({
          tableName: type,
          table_schema: "relationship",
          name: String(p.propertyName),
          type: typeOf(p),
        });
  }
  return columns;
}

export async function discoverNeo4j(c: ConnectorConfig) {
  const labels = (
    await neo4jQuery(
      c,
      "CALL db.labels() YIELD label RETURN label ORDER BY label",
    )
  ).map((r) => String(r.label));
  const types = (
    await neo4jQuery(
      c,
      "CALL db.relationshipTypes() YIELD relationshipType RETURN relationshipType AS type ORDER BY type",
    )
  ).map((r) => String(r.type));
  assert(
    labels.length + types.length <= 500,
    "More than 500 labels and relationship types. Choose a database with fewer of them.",
  );
  const shapes = await relationshipShapes(c);
  const tables: Record<string, unknown>[] = [];
  // Counts come from Neo4j's count store, so each is immediate.
  for (const label of labels) {
    const [row] = await neo4jQuery(
      c,
      `MATCH (n:${quoted(label)}) RETURN count(n) AS n`,
    );
    tables.push({ name: label, schema: "node", estimatedRows: row?.n ?? 0 });
  }
  for (const type of types) {
    const [row] = await neo4jQuery(
      c,
      `MATCH ()-[r:${quoted(type)}]->() RETURN count(r) AS n`,
    );
    const shape = shapes.get(type) || {};
    tables.push({
      name: type,
      schema: "relationship",
      estimatedRows: row?.n ?? 0,
      ...(shape.from && !shape.ambiguous ? { from: shape.from } : {}),
      ...(shape.to && !shape.ambiguous ? { to: shape.to } : {}),
    });
  }
  const keys = labels.map((label) => ({
    tableName: label,
    table_schema: "node",
    columnName: idColumn(label),
    constraintName: "PRIMARY",
  }));
  const foreignKeys = [];
  for (const type of types) {
    const shape = shapes.get(type);
    if (!shape?.from || !shape.to || shape.ambiguous) continue;
    const cols = relationshipColumns(type, shape);
    for (const [column, label] of [
      [cols.from, shape.from],
      [cols.to, shape.to],
    ] as const)
      foreignKeys.push({
        tableName: type,
        table_schema: "relationship",
        columnName: column,
        referencedTable: label,
        referenced_schema: "node",
        referencedColumn: idColumn(label),
      });
  }
  const columns = await allColumns(c, labels, types, shapes);
  const sample = c.table
    ? (
        await page(
          c,
          String(c.table),
          c.schema === "relationship",
          "",
          10,
          shapes,
        )
      ).rows
    : [];
  return { tables, keys, foreignKeys, columns, sample };
}

export async function importNeo4j(
  c: ConnectorConfig,
  write: (row: unknown) => Promise<void>,
) {
  assert(c.table, "Select a label or relationship type");
  const name = String(c.table),
    relationship = c.schema === "relationship";
  const shapes = await relationshipShapes(c);
  let after = "";
  while (true) {
    const { rows, last } = await page(
      c,
      name,
      relationship,
      after,
      neo4jPaging.size,
      shapes,
    );
    for (const row of rows) await write(row);
    if (rows.length < neo4jPaging.size || !last) break;
    after = last;
  }
  return (
    await allColumns(
      c,
      relationship ? [] : [name],
      relationship ? [name] : [],
      shapes,
    )
  ).map(({ name, type }) => ({ name, type }));
}
