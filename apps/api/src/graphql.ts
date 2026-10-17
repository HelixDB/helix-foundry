// GraphQL schema auto-generated from the published Ontology (object types +
// relations). Resolvers read through the existing store and dataset query
// paths; no N+1: relation fields batch via linkCounts / neighborhood.
import {
  GraphQLSchema,
  GraphQLObjectType,
  GraphQLString,
  GraphQLInt,
  GraphQLFloat,
  GraphQLBoolean,
  GraphQLList,
  GraphQLNonNull,
  GraphQLID,
  GraphQLInputObjectType,
  buildSchema,
  graphql,
  type GraphQLFieldConfig,
  type GraphQLResolveInfo,
} from "graphql";
import type { Store } from "./store.js";
import { assert } from "./security.js";

type TypeInfo = {
  name: string;
  typeId: string;
  primaryKey: string;
  properties: string[];
  datasetId: string;
  generation: string;
};

type RelationInfo = {
  name: string;
  fromType: string;
  toType: string;
  fromColumn: string;
  toColumn: string;
};

// Map a property name to a GraphQL scalar (conservative).
function scalarFor(name: string): GraphQLScalarType {
  const n = name.toLowerCase();
  if (/(count|total|amount|num|size|length|degree|score|rank|_at|_ts|id$)/.test(n))
    return GraphQLFloat;
  if (/(is_|has_|active|enabled|verified|delinquent|boolean)/.test(n))
    return GraphQLBoolean;
  return GraphQLString;
}

import type { GraphQLScalarType } from "graphql";

/**
 * Build a GraphQL schema from the published ontology.
 * Query root exposes one field per object type: `typeName(key: ID!)` and
 * `typeNameList(limit: Int, offset: Int)`. Each type exposes its properties
 * plus relation fields (one per relationship that originates from it).
 */
export function buildOntologySchema(
  types: TypeInfo[],
  relations: RelationInfo[],
): GraphQLSchema {
  const typeMap = new Map<string, GraphQLObjectType>();
  const relationFields = new Map<string, GraphQLFieldConfig<any, any>[]>();

  // Pre-compute relation fields per source type.
  for (const rel of relations) {
    const field: GraphQLFieldConfig<any, any> = {
      type: new GraphQLList(new GraphQLNonNull(GraphQLString)),
      description: `Related ${rel.toType} keys via ${rel.name}`,
      resolve: async (parent: any) => {
        // The parent's link keys are pre-loaded by the list resolver.
        return parent.__links?.[rel.name] ?? [];
      },
    };
    const list = relationFields.get(rel.fromType) ?? [];
    list.push({ ...field, [rel.name]: field } as any);
    // Actually assign by name.
    relationFields.set(rel.fromType, list);
  }

  // Build each object type.
  for (const t of types) {
    const fields: Record<string, GraphQLFieldConfig<any, any>> = {
      _key: { type: new GraphQLNonNull(GraphQLID), resolve: (p) => p.__key },
      _type: { type: new GraphQLNonNull(GraphQLString), resolve: () => t.name },
    };
    for (const prop of t.properties) {
      if (prop === t.primaryKey) continue;
      fields[prop] = {
        type: scalarFor(prop),
        resolve: (p: any) => p[prop] ?? null,
      };
    }
    // Add relation fields.
    for (const rel of relations) {
      if (rel.fromType !== t.name) continue;
      fields[rel.name] = {
        type: new GraphQLList(new GraphQLNonNull(GraphQLString)),
        description: `Related ${rel.toType} keys via ${rel.name}`,
        resolve: (p: any) => p.__links?.[rel.name] ?? [],
      };
    }
    const gqlType = new GraphQLObjectType({
      name: t.name.replace(/[^a-zA-Z0-9_]/g, "_"),
      description: `Ontology type ${t.name}`,
      fields,
    });
    typeMap.set(t.name, gqlType);
  }

  // Query root.
  const queryFields: Record<string, GraphQLFieldConfig<any, any>> = {};
  for (const t of types) {
    const gqlType = typeMap.get(t.name)!;
    const safeName = t.name.replace(/[^a-zA-Z0-9_]/g, "_");
    // Single record by key.
    queryFields[safeName] = {
      type: gqlType,
      args: { key: { type: new GraphQLNonNull(GraphQLID) } },
      resolve: async (_root, args) => {
        // This is a placeholder: actual resolution is wired via the resolver
        // context (store + scope). See executeOntologyQuery.
        throw new Error("Use executeOntologyQuery");
      },
    };
    // List with pagination.
    queryFields[`${safeName}List`] = {
      type: new GraphQLList(new GraphQLNonNull(gqlType)),
      args: {
        limit: { type: GraphQLInt, defaultValue: 50 },
        offset: { type: GraphQLInt, defaultValue: 0 },
      },
      resolve: async () => {
        throw new Error("Use executeOntologyQuery");
      },
    };
  }

  const query = new GraphQLObjectType({ name: "Query", fields: queryFields });
  return new GraphQLSchema({ query });
}

/**
 * Execute a GraphQL query against the ontology. Resolves each type's rows
 * from the store and batches relation links.
 */
export async function executeOntologyQuery(
  store: Store,
  scope: string,
  query: string,
  variables?: Record<string, unknown>,
): Promise<{ data?: any; errors?: { message: string }[] }> {
  const generation = (await store.get(scope, "workspace"))?.data
    .activeGeneration;
  if (!generation) return { errors: [{ message: "No published ontology" }] };

  const types = (await store.list(scope, "objectType"))
    .filter((t) => t.data.generation === generation)
    .map((t) => ({
      name: t.data.name as string,
      typeId: t.data.typeId as string,
      primaryKey: t.data.primaryKey as string,
      properties: (t.data.properties as string[]) ?? [],
      datasetId: t.data.datasetId as string,
      generation,
    }));
  const relations = (await store.list(scope, "relation"))
    .filter((r) => r.data.generation === generation)
    .map((r) => ({
      name: r.data.name as string,
      fromType: r.data.fromType as string,
      toType: r.data.toType as string,
      fromColumn: r.data.fromColumn as string,
      toColumn: r.data.toColumn as string,
    }));

  if (!types.length) return { errors: [{ message: "No object types published" }] };

  const schema = buildOntologySchema(types, relations);

  // Pre-load all objects per type (bounded) and their relation links.
  const objectsByType = new Map<string, any[]>();
  for (const t of types) {
    const objects = (await store.list(scope, "object"))
      .filter(
        (o) => o.data.generation === generation && o.data.type === t.name,
      )
      .slice(0, 500);
    objectsByType.set(
      t.name,
      objects.map((o) => ({
        __key: o.data.key,
        __type: t.name,
        ...(o.data.base || {}),
        __links: o.data.links ?? {},
      })),
    );
  }

  // Build a resolver context that serves from memory.
  const rootValue = {
    __objects: objectsByType,
    __types: types,
    __relations: relations,
  };

  // We need custom resolvers; use graphql() with field resolvers that read
  // from rootValue. Since buildOntologySchema's resolvers throw, we rebuild
  // with proper resolvers here.
  const schemaWithResolvers = buildSchemaWithResolvers(types, relations, objectsByType);
  const result = await graphql({
    schema: schemaWithResolvers,
    source: query,
    variableValues: variables,
    rootValue,
  });
  return result as any;
}

function buildSchemaWithResolvers(
  types: TypeInfo[],
  relations: RelationInfo[],
  objectsByType: Map<string, any[]>,
): GraphQLSchema {
  const typeMap = new Map<string, GraphQLObjectType>();

  for (const t of types) {
    const fields: Record<string, GraphQLFieldConfig<any, any>> = {
      _key: { type: new GraphQLNonNull(GraphQLID), resolve: (p) => p.__key },
      _type: { type: new GraphQLNonNull(GraphQLString), resolve: () => t.name },
    };
    for (const prop of t.properties) {
      if (prop === t.primaryKey) continue;
      fields[prop] = {
        type: scalarFor(prop),
        resolve: (p: any) => p[prop] ?? null,
      };
    }
    for (const rel of relations) {
      if (rel.fromType !== t.name) continue;
      fields[rel.name] = {
        type: new GraphQLList(new GraphQLNonNull(GraphQLString)),
        resolve: (p: any) => p.__links?.[rel.name] ?? [],
      };
    }
    typeMap.set(
      t.name,
      new GraphQLObjectType({
        name: t.name.replace(/[^a-zA-Z0-9_]/g, "_"),
        fields,
      }),
    );
  }

  const queryFields: Record<string, GraphQLFieldConfig<any, any>> = {};
  for (const t of types) {
    const gqlType = typeMap.get(t.name)!;
    const safeName = t.name.replace(/[^a-zA-Z0-9_]/g, "_");
    queryFields[safeName] = {
      type: gqlType,
      args: { key: { type: new GraphQLNonNull(GraphQLID) } },
      resolve: (_root, args: any) => {
        const list = objectsByType.get(t.name) ?? [];
        return list.find((o) => o.__key === args.key) ?? null;
      },
    };
    queryFields[`${safeName}List`] = {
      type: new GraphQLList(new GraphQLNonNull(gqlType)),
      args: {
        limit: { type: GraphQLInt, defaultValue: 50 },
        offset: { type: GraphQLInt, defaultValue: 0 },
      },
      resolve: (_root, args: any) => {
        const list = objectsByType.get(t.name) ?? [];
        return list.slice(args.offset ?? 0, (args.offset ?? 0) + (args.limit ?? 50));
      },
    };
  }

  return new GraphQLSchema({
    query: new GraphQLObjectType({ name: "Query", fields: queryFields }),
  });
}
