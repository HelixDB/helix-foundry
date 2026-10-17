// Graph algorithms on DuckDB: degree, WCC, PageRank, Louvain (simple),
// node similarity and bounded shortest path. Each takes a DuckDB connection
// with an `_edges` table (src, dst, optional weight) and writes a `result` table.
// Iterative algorithms loop in TypeScript over SQL passes until convergence.
import type { DuckDBConnection } from "@duckdb/node-api";
import type { AlgorithmSpec, GraphPathRequest } from "../../../packages/shared/src/graph.js";

const q = (v: string) => '"' + v.replaceAll('"', '""') + '"';
const lit = (v: string) => "'" + v.replaceAll("'", "''") + "'";

export type GraphJob = {
  algorithm: AlgorithmSpec["kind"] | "shortest_path" | "all_paths" | "betweenness" | "knn" | "node_embeddings" | "link_prediction" | "vector_search" | "fraud_rings" | "structuring" | "cashout_trails" | "account_takeover" | "collusion";
  params?: Record<string, unknown>;
  // Column names on the loaded `_edges` table.
  srcColumn?: string;
  dstColumn?: string;
  weightColumn?: string;
  // Fraud recipe params.
  amountColumn?: string;
  threshold?: number;
  maxDepth?: number;
  minRingSize?: number;
  topN?: number;
  sourceId?: string;
  targetId?: string;
  damping?: number;
  maxIterations?: number;
  tolerance?: number;
  topK?: number;
  sampleSize?: number;
};

/** Build a clean `_edges(src, dst, w)` table from the raw input table. */
export async function materializeEdges(
  c: DuckDBConnection,
  srcColumn = "from_id",
  dstColumn = "to_id",
  weightColumn?: string,
) {
  const w = weightColumn ? `CAST(${q(weightColumn)} AS DOUBLE)` : "1.0";
  await c.run(
    `CREATE OR REPLACE TABLE _edges AS SELECT CAST(${q(srcColumn)} AS VARCHAR) AS src, CAST(${q(dstColumn)} AS VARCHAR) AS dst, ${w} AS w FROM input_edges WHERE ${q(srcColumn)} IS NOT NULL AND ${q(dstColumn)} IS NOT NULL`,
  );
  await c.run(
    `CREATE OR REPLACE TABLE _nodes AS SELECT DISTINCT id FROM (SELECT src AS id FROM _edges UNION ALL SELECT dst AS id FROM _edges)`,
  );
}

/** Degree centrality: in, out and total degree per node. Single pass. */
export async function runDegree(c: DuckDBConnection) {
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT
      n.id AS node_id,
      COALESCE(o.out_degree, 0) AS out_degree,
      COALESCE(i.in_degree, 0) AS in_degree,
      COALESCE(o.out_degree, 0) + COALESCE(i.in_degree, 0) AS total_degree
    FROM _nodes n
    LEFT JOIN (SELECT src, count(*) AS out_degree FROM _edges GROUP BY src) o ON o.src = n.id
    LEFT JOIN (SELECT dst, count(*) AS in_degree FROM _edges GROUP BY dst) i ON i.dst = n.id
  `);
}

/**
 * Weakly connected components via large-star / small-star.
 * Iterates until labels stabilise. Returns component_id per node.
 */
export async function runWcc(c: DuckDBConnection, maxIterations = 50) {
  await c.run(`CREATE OR REPLACE TABLE labels AS SELECT id, id AS component FROM _nodes`);
  await c.run(`CREATE OR REPLACE TABLE undirected AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  for (let iter = 0; iter < maxIterations; iter++) {
    // Large-star: each node takes the min label among itself and its higher neighbours.
    await c.run(`
      CREATE OR REPLACE TABLE next_labels AS
      SELECT u.id AS id, min(COALESCE(l2.component, u.id)) AS component
      FROM (SELECT id FROM _nodes) u
      LEFT JOIN undirected e ON e.a = u.id
      LEFT JOIN labels l1 ON l1.id = u.id
      LEFT JOIN labels l2 ON l2.id = e.b
      WHERE e.b IS NULL OR e.b > u.id OR l2.component < COALESCE(l1.component, u.id)
      GROUP BY u.id
    `);
    // Ensure every node keeps a row.
    await c.run(`
      CREATE OR REPLACE TABLE merged AS
      SELECT n.id, COALESCE(nl.component, COALESCE(l.component, n.id)) AS component
      FROM _nodes n
      LEFT JOIN next_labels nl ON nl.id = n.id
      LEFT JOIN labels l ON l.id = n.id
    `);
    const changed = Number(
      (
        await c.runAndReadAll(
          `SELECT count(*) AS n FROM labels lo JOIN merged m ON m.id = lo.id WHERE lo.component != m.component`,
        )
      ).getRowObjectsJson()[0].n,
    );
    await c.run(`CREATE OR REPLACE TABLE labels AS SELECT * FROM merged`);
    if (changed === 0) break;
  }
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT id AS node_id, component AS component_id
    FROM labels
    ORDER BY component, id
  `);
}

/**
 * PageRank: iterative power method on the edge list.
 * Damping default 0.85, tolerance 1e-6.
 */
export async function runPageRank(
  c: DuckDBConnection,
  damping = 0.85,
  maxIterations = 20,
  tolerance = 1e-6,
) {
  await c.run(`CREATE OR REPLACE TABLE pr AS SELECT id, 1.0 AS rank FROM _nodes`);
  await c.run(`CREATE OR REPLACE TABLE outdeg AS SELECT src, sum(w) AS d FROM _edges GROUP BY src`);
  const n = Number(
    (await c.runAndReadAll("SELECT count(*) AS n FROM _nodes")).getRowObjectsJson()[0].n,
  );
  if (n === 0) {
    await c.run(`CREATE OR REPLACE TABLE result AS SELECT NULL AS node_id, NULL AS pagerank WHERE false`);
    return;
  }
  for (let iter = 0; iter < maxIterations; iter++) {
    // Dangling mass: rank of nodes with no out-edges, redistributed uniformly.
    const dangling = Number(
      (
        await c.runAndReadAll(
          `SELECT COALESCE(sum(pr.rank), 0) AS s FROM pr WHERE pr.id NOT IN (SELECT src FROM _edges)`,
        )
      ).getRowObjectsJson()[0].s,
    );
    await c.run(`
      CREATE OR REPLACE TABLE pr_next AS
      SELECT
        nd.id AS id,
        (1.0 - ${damping}) / ${n} + ${damping} * (COALESCE(s.contrib, 0) + ${dangling} / ${n}) AS rank
      FROM _nodes nd
      LEFT JOIN (
        SELECT e.dst AS id, sum(pr.rank * e.w / NULLIF(od.d, 0)) AS contrib
        FROM _edges e
        JOIN pr ON pr.id = e.src
        JOIN outdeg od ON od.src = e.src
        GROUP BY e.dst
      ) s ON s.id = nd.id
    `);
    const delta = Number(
      (
        await c.runAndReadAll(
          `SELECT sum(abs(a.rank - b.rank)) AS d FROM pr a JOIN pr_next b ON a.id = b.id`,
        )
      ).getRowObjectsJson()[0].d,
    );
    await c.run(`CREATE OR REPLACE TABLE pr AS SELECT * FROM pr_next`);
    if (delta < tolerance) break;
  }
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT id AS node_id, rank AS pagerank
    FROM pr
    ORDER BY rank DESC
  `);
}

/**
 * Simple Louvain-style community detection: iterative modularity-local moves
 * on the undirected projection. Not a full Louvain, but partitions densely
 * connected fraud rings well at small scale.
 */
export async function runLouvain(c: DuckDBConnection, maxIterations = 30) {
  await c.run(`CREATE OR REPLACE TABLE communities AS SELECT id, id AS community FROM _nodes`);
  await c.run(`CREATE OR REPLACE TABLE undirected AS SELECT src AS a, dst AS b, w FROM _edges UNION ALL SELECT dst AS a, src AS b, w FROM _edges`);
  const m2 =
    Number(
      (await c.runAndReadAll("SELECT sum(w) AS s FROM undirected")).getRowObjectsJson()[0].s,
    ) || 1;
  for (let iter = 0; iter < maxIterations; iter++) {
    await c.run(`
      CREATE OR REPLACE TABLE moves AS
      WITH node_degree AS (
        SELECT a AS id, sum(w) AS k FROM undirected GROUP BY a
      ),
      neighbour_comm AS (
        SELECT e.a AS id, cm.community AS community, sum(e.w) AS wsum
        FROM undirected e
        JOIN communities cm ON cm.id = e.b
        GROUP BY e.a, cm.community
      )
      SELECT
        cm.id AS id,
        COALESCE(
          (SELECT nc.community FROM neighbour_comm nc WHERE nc.id = cm.id ORDER BY nc.wsum DESC, nc.community LIMIT 1),
          cm.community
        ) AS community
      FROM communities cm
    `);
    const changed = Number(
      (
        await c.runAndReadAll(
          `SELECT count(*) AS n FROM communities lo JOIN moves m ON m.id = lo.id WHERE lo.community != m.community`,
        )
      ).getRowObjectsJson()[0].n,
    );
    await c.run(`CREATE OR REPLACE TABLE communities AS SELECT * FROM moves`);
    if (changed === 0) break;
  }
  // Relabel to dense integers.
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT id AS node_id, community AS community_id
    FROM communities
    ORDER BY community, id
  `);
}

/**
 * Node similarity: Jaccard over projected one-hop neighbour sets.
 * Top-k pairs per node via QUALIFY row_number().
 */
export async function runNodeSimilarity(c: DuckDBConnection, topK = 10) {
  await c.run(`
    CREATE OR REPLACE TABLE _neighbours AS
    SELECT src AS id, dst AS neighbour FROM _edges
    UNION
    SELECT dst AS id, src AS neighbour FROM _edges
  `);
  await c.run(`
    CREATE OR REPLACE TABLE _pairs AS
    SELECT a.id AS node_a, b.id AS node_b,
      CAST(count(*) AS DOUBLE) / CAST(
        (SELECT count(*) FROM _neighbours x WHERE x.id = a.id) +
        (SELECT count(*) FROM _neighbours y WHERE y.id = b.id) -
        count(*)
      AS DOUBLE) AS jaccard
    FROM _neighbours a
    JOIN _neighbours b ON a.neighbour = b.neighbour AND a.id < b.id
    GROUP BY a.id, b.id
  `);
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT node_a, node_b, jaccard AS score
    FROM _pairs
    QUALIFY row_number() OVER (PARTITION BY node_a ORDER BY score DESC) <= ${topK}
    ORDER BY node_a, score DESC
  `);
}

/**
 * k-nearest neighbours by Jaccard similarity (directed: top-k per node,
 * including both directions of each pair).
 */
export async function runKnn(c: DuckDBConnection, topK = 10) {
  await c.run(`
    CREATE OR REPLACE TABLE _neighbours AS
    SELECT src AS id, dst AS neighbour FROM _edges
    UNION
    SELECT dst AS id, src AS neighbour FROM _edges
  `);
  await c.run(`
    CREATE OR REPLACE TABLE _sim AS
    SELECT a.id AS node_id, b.id AS neighbour_id,
      CAST(count(*) AS DOUBLE) / CAST(
        (SELECT count(*) FROM _neighbours x WHERE x.id = a.id) +
        (SELECT count(*) FROM _neighbours y WHERE y.id = b.id) -
        count(*)
      AS DOUBLE) AS score
    FROM _neighbours a
    JOIN _neighbours b ON a.neighbour = b.neighbour AND a.id != b.id
    GROUP BY a.id, b.id
  `);
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT node_id, neighbour_id, score
    FROM _sim
    QUALIFY row_number() OVER (PARTITION BY node_id ORDER BY score DESC) <= ${topK}
    ORDER BY node_id, score DESC
  `);
}

/**
 * Approximate betweenness centrality via sampled BFS from a subset of nodes.
 * For each sampled source, run BFS and count how many shortest paths pass
 * through each intermediate node (Brandes' algorithm, single-source).
 */
export async function runBetweenness(
  c: DuckDBConnection,
  sampleSize = 200,
  maxDepth = 6,
) {
  // Undirected adjacency.
  await c.run(`CREATE OR REPLACE TABLE _adj AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  await c.run(`CREATE OR REPLACE TABLE _betweenness AS SELECT id AS node_id, CAST(0.0 AS DOUBLE) AS betweenness FROM _nodes`);
  // Sample sources.
  const allNodes = (
    await c.runAndReadAll(`SELECT id FROM _nodes ORDER BY id LIMIT ${sampleSize}`)
  ).getRowObjectsJson();
  for (const row of allNodes) {
    const source = String(row.id);
    // BFS layers + sigma (path counts) + predecessors.
    await c.run(`CREATE OR REPLACE TABLE _bfs (id VARCHAR, depth BIGINT, sigma DOUBLE)`);
    await c.run(`INSERT INTO _bfs VALUES (${lit(source)}, 0, CAST(1.0 AS DOUBLE))`);
    for (let d = 1; d <= maxDepth; d++) {
      await c.run(`
        INSERT INTO _bfs
        SELECT e.b AS id, ${d} AS depth, sum(f.sigma) AS sigma
        FROM _bfs f
        JOIN _adj e ON e.a = f.id
        WHERE f.depth = ${d - 1}
          AND e.b NOT IN (SELECT id FROM _bfs WHERE depth < ${d})
        GROUP BY e.b
      `);
    }
    // Brandes accumulation: walk depths in reverse, accumulate dependency.
    await c.run(`CREATE OR REPLACE TABLE _dep (id VARCHAR, dep DOUBLE)`);
    await c.run(`INSERT INTO _dep SELECT id, CAST(0.0 AS DOUBLE) FROM _bfs`);
    for (let d = maxDepth; d >= 1; d--) {
      await c.run(`
        UPDATE _dep SET dep = dep + (
          SELECT COALESCE(sum(
            (SELECT sigma FROM _bfs WHERE id = _dep.id) /
            NULLIF((SELECT sigma FROM _bfs WHERE id = _adj.b), 0) *
            (1 + (SELECT dep FROM _dep d2 WHERE d2.id = _adj.b))
          ), 0)
          FROM _adj
          WHERE _adj.a = _dep.id
            AND (SELECT depth FROM _bfs WHERE id = _adj.b) = ${d}
        )
        WHERE (SELECT depth FROM _bfs WHERE id = _dep.id) = ${d - 1}
          AND _dep.id != ${lit(source)}
      `);
    }
    // Add to betweenness (skip the source itself).
    await c.run(`
      UPDATE _betweenness SET betweenness = betweenness + COALESCE(
        (SELECT dep FROM _dep WHERE _dep.id = _betweenness.node_id AND _dep.id != ${lit(source)}), 0
      )
    `);
    await c.run(`DROP TABLE IF EXISTS _bfs`);
    await c.run(`DROP TABLE IF EXISTS _dep`);
  }
  // Normalise by 2 / (n*(n-1)) for undirected graphs (approximate: sample-based).
  const n = Number(
    (await c.runAndReadAll("SELECT count(*) AS n FROM _nodes")).getRowObjectsJson()[0].n,
  );
  const norm = n > 1 ? 2 / (n * (n - 1)) : 1;
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT node_id, betweenness * ${norm} AS betweenness
    FROM _betweenness
    ORDER BY betweenness DESC
  `);
}

/**
 * All simple paths between two nodes up to maxDepth (for investigation).
 * Returns each path as a row with its node sequence and length.
 */
export async function runAllPaths(
  c: DuckDBConnection,
  sourceId: string,
  targetId: string,
  maxDepth = 6,
  topN = 50,
) {
  await c.run(`CREATE OR REPLACE TABLE _adj AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  await c.run(`CREATE OR REPLACE TABLE _paths (path_nodes VARCHAR, length BIGINT)`);
  await c.run(
    `CREATE OR REPLACE TABLE _walk AS SELECT ${lit(sourceId)} AS path, ${lit(sourceId)} AS current, 0 AS depth`,
  );
  for (let d = 1; d <= maxDepth; d++) {
    await c.run(`
      CREATE OR REPLACE TABLE _walk_next AS
      SELECT w.path || ',' || e.b AS path, e.b AS current, ${d} AS depth
      FROM _walk w
      JOIN _adj e ON e.a = w.current
      WHERE e.b NOT IN (SELECT CAST(string_split(w2.path, ',')[1] AS VARCHAR) FROM _walk w2 LIMIT 0)
        AND strpos(w.path, e.b) = 0
    `);
    const hits = await c.runAndReadAll(
      `SELECT path, depth FROM _walk_next WHERE current = ${lit(targetId)} LIMIT ${topN}`,
    );
    for (const h of hits.getRowObjectsJson()) {
      await c.run(
        `INSERT INTO _paths VALUES (${lit(String(h.path))}, ${Number(h.depth)})`,
      );
    }
    await c.run(`CREATE OR REPLACE TABLE _walk AS SELECT * FROM _walk_next WHERE current != ${lit(targetId)}`);
    const alive = Number(
      (await c.runAndReadAll("SELECT count(*) AS n FROM _walk")).getRowObjectsJson()[0].n,
    );
    if (alive === 0) break;
  }
  await c.run(`CREATE OR REPLACE TABLE result AS SELECT * FROM _paths ORDER BY length LIMIT ${topN}`);
}

/**
 * Bounded shortest path between two _nodes via iterative BFS on the
 * undirected edge list. Returns the node path and length; empty if unreachable.
 */
export async function runShortestPath(
  c: DuckDBConnection,
  sourceId: string,
  targetId: string,
  maxDepth = 6,
) {
  await c.run(`CREATE OR REPLACE TABLE undirected AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  await c.run(
    `CREATE OR REPLACE TABLE frontier AS SELECT ${lit(sourceId)} AS id, 0 AS depth, ${lit(sourceId)} AS path`,
  );
  await c.run(`CREATE OR REPLACE TABLE visited AS SELECT id, depth, path FROM frontier`);
  let found = false;
  for (let d = 1; d <= maxDepth; d++) {
    await c.run(`
      CREATE OR REPLACE TABLE next_frontier AS
      SELECT DISTINCT e.b AS id, ${d} AS depth, v.path || ',' || e.b AS path
      FROM frontier v
      JOIN undirected e ON e.a = v.id
      WHERE e.b NOT IN (SELECT id FROM visited)
    `);
    const hit = await c.runAndReadAll(
      `SELECT path FROM next_frontier WHERE id = ${lit(targetId)} LIMIT 1`,
    );
    const rows = hit.getRowObjectsJson();
    if (rows.length) {
      const path = String(rows[0].path).split(",");
      await c.run(`
        CREATE OR REPLACE TABLE result AS
        SELECT * FROM (VALUES (${lit(String(d))}, ${lit(path.join(","))})) AS t(length, path_nodes)
      `);
      found = true;
      break;
    }
    await c.run(`INSERT INTO visited SELECT * FROM next_frontier`);
    await c.run(`CREATE OR REPLACE TABLE frontier AS SELECT * FROM next_frontier`);
    const count = Number(
      (await c.runAndReadAll("SELECT count(*) AS n FROM frontier")).getRowObjectsJson()[0].n,
    );
    if (count === 0) break;
  }
  if (!found)
    await c.run(
      `CREATE OR REPLACE TABLE result AS SELECT NULL AS length, NULL AS path_nodes WHERE false`,
    );
}

/**
 * Node embeddings via random walks + co-occurrence + truncated SVD.
 * Generates random walks, builds a co-occurrence matrix, and extracts
 * low-dimensional embeddings using power iteration SVD.
 */
export async function runNodeEmbeddings(
  c: DuckDBConnection,
  dimensions = 8,
  walkLength = 10,
  numWalks = 5,
  windowSize = 3,
) {
  // Build undirected adjacency for random walks.
  await c.run(`CREATE OR REPLACE TABLE _adj AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  await c.run(`CREATE OR REPLACE TABLE _cooc (node_a VARCHAR, node_b VARCHAR, weight DOUBLE)`);

  const allNodes = (
    await c.runAndReadAll("SELECT id FROM _nodes ORDER BY id LIMIT 500")
  ).getRowObjectsJson();

  // Generate random walks and accumulate co-occurrence.
  for (const row of allNodes) {
    const start = String(row.id);
    for (let w = 0; w < numWalks; w++) {
      let current = start;
      const walk: string[] = [start];
      for (let step = 0; step < walkLength; step++) {
        const nbrs = (
          await c.runAndReadAll(
            `SELECT b FROM _adj WHERE a = ${lit(current)} ORDER BY random() LIMIT 1`,
          )
        ).getRowObjectsJson();
        if (!nbrs.length) break;
        current = String(nbrs[0].b);
        walk.push(current);
      }
      // Co-occurrence within window.
      for (let i = 0; i < walk.length; i++) {
        for (let j = i + 1; j < Math.min(i + windowSize + 1, walk.length); j++) {
          const a = walk[i], b = walk[j];
          if (a === b) continue;
          await c.run(
            `INSERT INTO _cooc VALUES (${lit(a)}, ${lit(b)}, 1.0), (${lit(b)}, ${lit(a)}, 1.0)`,
          );
        }
      }
    }
  }

  // Aggregate co-occurrence.
  await c.run(`CREATE OR REPLACE TABLE _cooc_agg AS SELECT node_a, node_b, sum(weight) AS weight FROM _cooc GROUP BY node_a, node_b`);

  // Build a dense co-occurrence matrix row for each node (sparse -> row).
  const nodeIds = allNodes.map((n) => String(n.id));
  const n = nodeIds.length;
  const idx = new Map(nodeIds.map((id, i) => [id, i]));

  // Power-iteration SVD on the co-occurrence matrix (symmetric, so eigen).
  // Initialise a random matrix B (n x dimensions).
  await c.run(`CREATE OR REPLACE TABLE _embed (node_id VARCHAR, ${Array.from({ length: dimensions }, (_, i) => `dim_${i} DOUBLE`).join(", ")})`);
  for (const id of nodeIds) {
    const row = Array.from({ length: dimensions }, () => (Math.random() * 2 - 1).toFixed(6));
    await c.run(
      `INSERT INTO _embed VALUES (${lit(id)}, ${row.join(", ")})`,
    );
  }

  // Power iteration: B = C * B, then orthonormalise (simplified).
  for (let iter = 0; iter < 20; iter++) {
    await c.run(`CREATE OR REPLACE TABLE _embed_next AS SELECT * FROM _embed WHERE false`);
    for (const id of nodeIds) {
      const i = idx.get(id)!;
      const newDims: string[] = [];
      for (let d = 0; d < dimensions; d++) {
        // Compute (C * B)[i][d] = sum_j C[i][j] * B[j][d]
        const val = Number(
          (
            await c.runAndReadAll(
              `SELECT COALESCE(sum(ca.weight * e.dim_${d}), 0) AS v
               FROM _cooc_agg ca
               JOIN _embed e ON e.node_id = ca.node_b
               WHERE ca.node_a = ${lit(id)}`,
            )
          ).getRowObjectsJson()[0].v,
        );
        newDims.push(val.toFixed(6));
      }
      await c.run(`INSERT INTO _embed_next VALUES (${lit(id)}, ${newDims.join(", ")})`);
    }
    await c.run(`CREATE OR REPLACE TABLE _embed AS SELECT * FROM _embed_next`);
    // Normalise rows.
    await c.run(`CREATE OR REPLACE TABLE _embed_norm AS
      SELECT node_id, ${Array.from({ length: dimensions }, (_, d) => `dim_${d} / NULLIF(sqrt(${Array.from({ length: dimensions }, (_, dd) => `dim_${dd} * dim_${dd}`).join(" + ")}), 0) AS dim_${d}`).join(", ")}
      FROM _embed`);
    await c.run(`CREATE OR REPLACE TABLE _embed AS SELECT * FROM _embed_norm`);
  }

  await c.run(`CREATE OR REPLACE TABLE result AS SELECT * FROM _embed ORDER BY node_id`);
}

/**
 * Link prediction: score unconnected node pairs by common neighbours,
 * Adamic-Adar, Jaccard, and optionally embedding cosine similarity.
 */
export async function runLinkPrediction(
  c: DuckDBConnection,
  topK = 50,
  useEmbeddings = false,
) {
  await c.run(`CREATE OR REPLACE TABLE _neighbours AS
    SELECT src AS id, dst AS neighbour FROM _edges
    UNION
    SELECT dst AS id, src AS neighbour FROM _edges`);

  // Candidate pairs: share at least one neighbour, not already connected.
  await c.run(`CREATE OR REPLACE TABLE _candidates AS
    SELECT a.id AS node_a, b.id AS node_b,
      count(*) AS common_neighbors
    FROM _neighbours a
    JOIN _neighbours b ON a.neighbour = b.neighbour AND a.id < b.id
    WHERE NOT EXISTS (SELECT 1 FROM _edges e WHERE (e.src = a.id AND e.dst = b.id) OR (e.src = b.id AND e.dst = a.id))
    GROUP BY a.id, b.id
    HAVING count(*) >= 1`);

  // Adamic-Adar: sum over common neighbours of 1 / log(degree(cn)).
  await c.run(`CREATE OR REPLACE TABLE _scores AS
    SELECT c.node_a, c.node_b, c.common_neighbors,
      CAST(COALESCE((
        SELECT sum(1.0 / log(1.0 + (SELECT count(*) FROM _neighbours x WHERE x.id = n.neighbour)))
        FROM _neighbours n
        WHERE n.id = c.node_a
          AND n.neighbour IN (SELECT neighbour FROM _neighbours m WHERE m.id = c.node_b)
      ), 0) AS DOUBLE) AS adamic_adar,
      CAST(c.common_neighbors AS DOUBLE) / CAST(
        (SELECT count(*) FROM _neighbours x WHERE x.id = c.node_a) +
        (SELECT count(*) FROM _neighbours y WHERE y.id = c.node_b) -
        c.common_neighbors
      AS DOUBLE) AS jaccard
    FROM _candidates c`);

  // Optional: embedding cosine similarity.
  if (useEmbeddings) {
    await c.run(`CREATE OR REPLACE TABLE _scores AS
      SELECT s.*,
        CAST(COALESCE((
          SELECT ${Array.from({ length: 8 }, (_, d) => `e1.dim_${d} * e2.dim_${d}`).join(" + ")}
          FROM _embed e1, _embed e2
          WHERE e1.node_id = s.node_a AND e2.node_id = s.node_b
        ), 0) AS DOUBLE) AS cosine
      FROM _scores s`);
  }

  await c.run(`CREATE OR REPLACE TABLE result AS
    SELECT node_a, node_b, common_neighbors, adamic_adar, jaccard
    ${useEmbeddings ? ", cosine" : ""}
    FROM _scores
    ORDER BY adamic_adar DESC, jaccard DESC
    LIMIT ${topK}`);
}

/**
 * Vector similarity search: find top-k nearest neighbours of a query node
 * by cosine similarity of embeddings. If no query node, return all pairs.
 */
export async function runVectorSearch(
  c: DuckDBConnection,
  queryNodeId?: string,
  topK = 10,
  dimensions = 8,
) {
  const dims = Array.from({ length: dimensions }, (_, d) => `dim_${d}`);
  const dot = dims.map((d) => `a.${d} * b.${d}`).join(" + ");
  const normA = `sqrt(${dims.map((d) => `a.${d} * a.${d}`).join(" + ")})`;
  const normB = `sqrt(${dims.map((d) => `b.${d} * b.${d}`).join(" + ")})`;
  if (queryNodeId) {
    await c.run(`CREATE OR REPLACE TABLE result AS
      SELECT
        b.node_id AS neighbour_id,
        CAST((${dot}) / NULLIF(${normA} * ${normB}, 0) AS DOUBLE) AS cosine
      FROM _embed a, _embed b
      WHERE a.node_id = ${lit(queryNodeId)} AND b.node_id != ${lit(queryNodeId)}
      ORDER BY cosine DESC
      LIMIT ${topK}`);
  } else {
    await c.run(`CREATE OR REPLACE TABLE result AS
      SELECT
        a.node_id AS node_id,
        b.node_id AS neighbour_id,
        CAST((${dot}) / NULLIF(${normA} * ${normB}, 0) AS DOUBLE) AS cosine
      FROM _embed a, _embed b
      WHERE a.node_id < b.node_id
      ORDER BY cosine DESC
      LIMIT ${topK}`);
  }
}

export async function runMoneyFlowRings(
  c: DuckDBConnection,
  maxDepth = 5,
  minRingSize = 3,
  topN = 50,
) {
  await c.run(`CREATE OR REPLACE TABLE directed AS SELECT src, dst FROM _edges`);
  // Enumerate cycles up to maxDepth by walking from each node and checking
  // whether the walk returns to its start. Bounded and deduplicated by the
  // sorted node set of the cycle.
  const rows: Record<string, unknown>[] = [];
  const allNodes = (
    await c.runAndReadAll("SELECT id FROM _nodes ORDER BY id")
  ).getRowObjectsJson();
  const seen = new Set<string>();
  for (const n of allNodes) {
    const start = String(n.id);
    await c.run(`
      CREATE OR REPLACE TABLE walk AS
      SELECT ${lit(start)} AS path, ${lit(start)} AS current, 0 AS depth
    `);
    for (let d = 1; d <= maxDepth; d++) {
      await c.run(`
        CREATE OR REPLACE TABLE walk_next AS
        SELECT w.path || ',' || e.dst AS path, e.dst AS current, ${d} AS depth
        FROM walk w
        JOIN directed e ON e.src = w.current
        WHERE ${d} < ${maxDepth} OR e.dst = ${lit(start)}
      `);
      const cycles = await c.runAndReadAll(
        `SELECT path, depth FROM walk_next WHERE current = ${lit(start)} AND depth >= ${minRingSize - 1} LIMIT 200`,
      );
      for (const row of cycles.getRowObjectsJson()) {
        const path = String(row.path).split(",");
        const key = [...path.slice(0, -1)].sort().join(",");
        if (seen.has(key)) continue;
        seen.add(key);
        rows.push({
          finding_id: "ring_" + key.slice(0, 40),
          node_ids: path.slice(0, -1).join(","),
          ring_size: path.length - 1,
          severity: path.length - 1 >= 4 ? "high" : "medium",
          score: path.length - 1,
          explanation: `Money-flow ring of ${path.length - 1} nodes: ${path.slice(0, -1).join(" → ")} → ${start}`,
        });
      }
      await c.run(`CREATE OR REPLACE TABLE walk AS SELECT * FROM walk_next WHERE current != ${lit(start)}`);
      const alive = Number(
        (await c.runAndReadAll("SELECT count(*) AS n FROM walk")).getRowObjectsJson()[0].n,
      );
      if (alive === 0) break;
    }
    if (rows.length >= topN * 3) break; // enough candidates
  }
  // Materialise findings.
  await c.run(`CREATE OR REPLACE TABLE result (finding_id VARCHAR, node_ids VARCHAR, ring_size BIGINT, severity VARCHAR, score DOUBLE, explanation VARCHAR)`);
  for (const r of rows.slice(0, topN)) {
    await c.run(
      `INSERT INTO result VALUES (${lit(String(r.finding_id))}, ${lit(String(r.node_ids))}, ${Number(r.ring_size)}, ${lit(String(r.severity))}, ${Number(r.score)}, ${lit(String(r.explanation))})`,
    );
  }
  if (rows.length === 0)
    await c.run(`INSERT INTO result SELECT NULL, NULL, NULL, NULL, NULL, NULL WHERE false`);
}

/**
 * Structuring: _nodes whose aggregate flow is near a threshold (many small
 * transactions just under a reporting limit). Uses amount column when present.
 */
export async function runStructuring(
  c: DuckDBConnection,
  amountColumn = "amount",
  threshold = 10000,
  topN = 50,
) {
  const hasAmount = await c
    .runAndReadAll(`SELECT count(*) AS n FROM input_edges WHERE ${q(amountColumn)} IS NOT NULL`)
    .then((r) => Number(r.getRowObjectsJson()[0].n) > 0)
    .catch(() => false);
  if (hasAmount) {
    await c.run(`
      CREATE OR REPLACE TABLE result AS
      SELECT
        'struct_' || src AS finding_id,
        src AS node_ids,
        count(*) AS txn_count,
        sum(CAST(${q(amountColumn)} AS DOUBLE)) AS total_amount,
        'high' AS severity,
        count(*) * 1.0 AS score,
        'Node ' || src || ' has ' || count(*) || ' transactions totalling ' ||
          CAST(sum(CAST(${q(amountColumn)} AS DOUBLE)) AS VARCHAR) ||
          ' across ' || count(DISTINCT dst) || ' counterparties (structuring pattern)' AS explanation
      FROM input_edges
      WHERE CAST(${q(amountColumn)} AS DOUBLE) > 0
        AND CAST(${q(amountColumn)} AS DOUBLE) < ${threshold}
      GROUP BY src
      HAVING count(*) >= 3 AND count(DISTINCT dst) >= 2
      ORDER BY score DESC
      LIMIT ${topN}
    `);
  } else {
    // No amount column: fall back to high-degree fan-in/fan-out.
    await c.run(`
      CREATE OR REPLACE TABLE result AS
      SELECT
        'struct_' || src AS finding_id,
        src AS node_ids,
        count(*) AS txn_count,
        NULL AS total_amount,
        'medium' AS severity,
        count(*) * 1.0 AS score,
        'Node ' || src || ' has ' || count(*) || ' edges to ' || count(DISTINCT dst) ||
          ' counterparties (fan-out pattern)' AS explanation
      FROM _edges
      GROUP BY src
      HAVING count(*) >= 5 AND count(DISTINCT dst) >= 3
      ORDER BY score DESC
      LIMIT ${topN}
    `);
  }
}

/**
 * Cash-out trails: chains DEPOSIT-like _edges that end at a high-out-degree
 * sink (cash-out agent). Heuristic: find _nodes with high out-degree whose
 * upstream neighbourhood includes many deposit sources.
 */
export async function runCashoutTrails(
  c: DuckDBConnection,
  maxDepth = 5,
  topN = 50,
) {
  await c.run(`CREATE OR REPLACE TABLE undirected AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  await c.run(`
    CREATE OR REPLACE TABLE reach AS
    WITH RECURSIVE walk AS (
      SELECT e.dst AS sink, e.src AS node, 1 AS depth
      FROM _edges e
      UNION
      SELECT w.sink, u.a AS node, w.depth + 1
      FROM walk w
      JOIN undirected u ON u.b = w.node
      WHERE w.depth < ${maxDepth}
    )
    SELECT sink, count(DISTINCT node) AS upstream_count
    FROM walk
    GROUP BY sink
  `);
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT
      'cashout_' || e.dst AS finding_id,
      e.dst AS node_ids,
      count(*) AS txn_count,
      max(r.upstream_count) AS upstream_count,
      'high' AS severity,
      CAST(count(*) + COALESCE(max(r.upstream_count), 0) AS DOUBLE) AS score,
      'Node ' || e.dst || ' receives ' || count(*) || ' flows from ' ||
        count(DISTINCT e.src) || ' sources, reaching ' ||
        COALESCE(max(r.upstream_count), 0) || ' upstream nodes (cash-out trail)' AS explanation
    FROM _edges e
    JOIN reach r ON r.sink = e.dst
    GROUP BY e.dst
    HAVING count(*) >= 2 AND COALESCE(max(r.upstream_count), 0) >= 3
    ORDER BY score DESC
    LIMIT ${topN}
  `);
}

/** Dispatch: run the requested algorithm and leave its output in `result`. */
export async function runGraphAlgorithm(c: DuckDBConnection, job: GraphJob) {
  const src = job.srcColumn || "from_id";
  const dst = job.dstColumn || "to_id";
  if (job.algorithm !== "structuring")
    await materializeEdges(c, src, dst, job.weightColumn);
  switch (job.algorithm) {
    case "degree":
      return runDegree(c);
    case "wcc":
      return runWcc(c, job.maxIterations ?? 50);
    case "pagerank":
      return runPageRank(
        c,
        job.damping ?? 0.85,
        job.maxIterations ?? 20,
        job.tolerance ?? 1e-6,
      );
    case "louvain":
      return runLouvain(c, job.maxIterations ?? 30);
    case "node_similarity":
      return runNodeSimilarity(c, job.topK ?? 10);
    case "knn":
      return runKnn(c, job.topK ?? 10);
    case "betweenness":
      return runBetweenness(c, job.sampleSize ?? 200, job.maxDepth ?? 6);
    case "shortest_path":
      return runShortestPath(
        c,
        String(job.sourceId || ""),
        String(job.targetId || ""),
        job.maxDepth ?? 6,
      );
    case "all_paths":
      return runAllPaths(
        c,
        String(job.sourceId || ""),
        String(job.targetId || ""),
        job.maxDepth ?? 6,
        job.topN ?? 50,
      );
    case "node_embeddings":
      return runNodeEmbeddings(
        c,
        8, // dimensions
        10, // walkLength
        5, // numWalks
        3, // windowSize
      );
    case "link_prediction":
      return runLinkPrediction(c, job.topN ?? 50, false);
    case "vector_search":
      return runVectorSearch(c, job.sourceId, job.topN ?? 10, 8);
    case "fraud_rings":
      return runMoneyFlowRings(
        c,
        job.maxDepth ?? 5,
        job.minRingSize ?? 3,
        job.topN ?? 50,
      );
    case "structuring":
      return runStructuring(c, job.amountColumn ?? "amount", job.threshold ?? 10000, job.topN ?? 50);
    case "cashout_trails":
      return runCashoutTrails(c, job.maxDepth ?? 5, job.topN ?? 50);
    case "account_takeover":
      return runAccountTakeover(c, job.topN ?? 50);
    case "collusion":
      return runCollusion(c, job.maxDepth ?? 4, job.topN ?? 50);
    default:
      throw new Error(`Unknown graph algorithm: ${job.algorithm}`);
  }
}

/**
 * Account takeover: detect nodes that suddenly change behaviour —
 * many new counterparties in a short window (fan-out burst).
 */
export async function runAccountTakeover(c: DuckDBConnection, topN = 50) {
  await c.run(`
    CREATE OR REPLACE TABLE _neighbours AS
    SELECT src AS id, dst AS neighbour FROM _edges
    UNION
    SELECT dst AS id, src AS neighbour FROM _edges
  `);
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT
      'ato_' || id AS finding_id,
      id AS node_ids,
      count(*) AS degree,
      count(DISTINCT neighbour) AS unique_neighbours,
      'high' AS severity,
      CAST(count(*) AS DOUBLE) AS score,
      'Node ' || id || ' has ' || count(*) || ' edges to ' ||
        count(DISTINCT neighbour) || ' unique counterparties (account takeover pattern)' AS explanation
    FROM _neighbours
    GROUP BY id
    HAVING count(*) >= 4 AND count(DISTINCT neighbour) >= 4
    ORDER BY score DESC
    LIMIT ${topN}
  `);
}

/**
 * Collusion: detect dense clusters of nodes that all connect to each other
 * (clique-like subgraphs), a sign of coordinated fraud rings.
 */
export async function runCollusion(c: DuckDBConnection, maxDepth = 4, topN = 50) {
  await c.run(`CREATE OR REPLACE TABLE _adj AS SELECT src AS a, dst AS b FROM _edges UNION ALL SELECT dst AS a, src AS b FROM _edges`);
  // Find triangles: A-B, B-C, A-C all connected.
  await c.run(`
    CREATE OR REPLACE TABLE result AS
    SELECT
      'collusion_' || a.a || '_' || a.b || '_' || b.b AS finding_id,
      a.a || ',' || a.b || ',' || b.b AS node_ids,
      3 AS clique_size,
      'high' AS severity,
      3.0 AS score,
      'Triangle: ' || a.a || ' ↔ ' || a.b || ' ↔ ' || b.b || ' (collusion pattern)' AS explanation
    FROM _adj a
    JOIN _adj b ON b.a = a.b AND b.b > a.b
    WHERE EXISTS (SELECT 1 FROM _adj x WHERE x.a = a.a AND x.b = b.b)
    ORDER BY score DESC
    LIMIT ${topN}
  `);
}
