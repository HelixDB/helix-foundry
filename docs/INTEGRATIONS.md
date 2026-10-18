# Helix Foundry — étude d'intégrations (Neo4j et écosystème)

Document de travail : possibilités d'outils, plugins, connecteurs et extensions à intégrer
à Helix Foundry pour enrichir la plateforme (référence : Palantir Foundry). Sources :
documentation officielle Neo4j (https://neo4j.com/docs/) et code de ce dépôt.

## 1. État des lieux

**Helix Foundry aujourd'hui** : workspace local-first sur HelixDB (graphe/ontologie) +
DuckDB (analyse), snapshots Parquet versionnés, Analyst NL→SQL, SDK TypeScript, API
OpenAPI, ingestion API, connecteurs PostgreSQL/MySQL (CDC natif), Neon/Supabase/PlanetScale,
Stripe, WorkOS, PostHog, REST, S3, fichiers, Neo4j.

**Connecteur Neo4j actuel** (`apps/api/src/neo4j.ts`) :
- HTTP Query API de Neo4j 5+, mode lecture seul, sans driver (aucune dépendance Bolt).
- Chaque label devient un dataset de nœuds, chaque type de relation un dataset de relations,
  clés = `elementId`, liens cross-datasets → ontologie suggérée.
- Snapshots complets (pas de change stream), rafraîchissement horaire par défaut.
- Limites connues : types temporels/durations conservés en chaînes, entiers hors safe range
  en chaînes, pagination 1000, pas de CDC, pas de push-back (écriture) vers Neo4j.

## 2. Extensions et composants Neo4j à intégrer

### A. Fiabiliser et accélérer le connecteur (priorité 1)

| Composant | Doc | Apport pour Helix Foundry |
| --- | --- | --- |
| **Change Data Capture (CDC)** `db.cdc.*` | https://neo4j.com/docs/cdc/current/ | Le point faible actuel : "Neo4j exposes no change stream". La CDC (Neo4j 5.13+, Enterprise / Aura Business Critical) fournit des flux d'événements create/update/delete sur nœuds et relations. Permettrait un vrai mode **live** comme PostgreSQL/MySQL au lieu de snapshots horaires. Existe en procédures Cypher + exemples JS/Python. |
| **Driver officiel JS (`neo4j-driver`)** | https://neo4j.com/docs/javascript-manual/current/ | Complément du Query API : streaming de résultats, transactions, fidélité des types (temporal, duration, spatial), reconnexions. Corrige les "dates gardées en chaînes". |
| **APOC Core** | https://neo4j.com/docs/apoc/current/ | `apoc.export.*` / `apoc.import.*` (exports CSV/JSON en masse plus rapides que la pagination), `apoc.periodic.iterate` (imports/exports par lots), `apoc.meta` (discovery de schéma), `apoc.refactor`, triggers. APOC Core est livré avec Neo4j 5. |
| **Connector for Apache Kafka** | https://neo4j.com/docs/kafka/ | Alternative à la CDC Enterprise : source Kafka branchée sur Neo4j pour streamer les changements vers le worker Helix Foundry (architecture événementielle). |

### B. Analytics de graphe — le différenciateur face à Palantir (priorité 1)

| Composant | Doc | Apport |
| --- | --- | --- |
| **Graph Data Science Library (GDS)** | https://neo4j.com/docs/graph-data-science/current/ | 60+ algorithmes : centralité (PageRank, betweenness), communautés (Louvain, Leiden, WCC), chemins (shortest path, similarity), embeddings (FastRP, node2vec), kNN, pipelines ML (node classification, link prediction). Édition Community gratuite, Enterprise pour l'échelle/ACL. |
| **GDS Client (Python)** | https://neo4j.com/docs/graph-data-science-client/current/ | Piloter GDS depuis l'executor, écrire les résultats comme datasets Parquet exploitables par DuckDB et l'Analyst. |
| **Neo4j embarqué optionnel** | https://neo4j.com/docs/operations-manual/current/docker/ | Ajouter un service `neo4j` (image `neo4j:5` + plugins `apoc`, `gds`) au `compose.yaml` comme **moteur d'analyse de graphe** : y projeter l'ontologie HelixDB pour exécuter GDS, sans toucher au store principal. |

Deux modes d'exécution : (a) GDS sur une source Neo4j déjà connectée, (b) projection de
l'ontologie interne vers le Neo4j embarqué pour analyser les données non-Neo4j (Stripe,
Postgres…) — c'est l'équivalent des graph analytics de Foundry.

### C. GenAI / RAG sur le graphe (priorité 2)

| Composant | Doc | Apport |
| --- | --- | --- |
| **Index vectoriels Cypher** | https://neo4j.com/docs/cypher-manual/current/indexes/semantic-indexes/vector-indexes/ | Index de similarité natifs + fonctions vector (`db.index.vector.queryNodes`). |
| **GenAI plugin** | https://neo4j.com/docs/genai/plugin/current/ | Génération d'embeddings via un endpoint OpenAI-compatible (donc aussi Ollama local, cohérent avec l'IA locale par défaut de Helix Foundry). |
| **Neo4j GraphRAG for Python** | https://neo4j.com/docs/neo4j-graphrag-python/current/ | Retrieval augmenté par le graphe : enrichir l'Analyst avec du GraphRAG (recherche hybride vectorielle + structurelle sur l'ontologie). |
| **LLM Knowledge Graph Builder** | https://neo4j.com/labs/genai-ecosystem/llm-graph-builder/ | Construction de knowledge graph depuis des documents non structurés → nouvelle classe de sources pour Helix Foundry (PDF, notes, tickets). |
| **Intégrations LangChain / LlamaIndex / Haystack / DSPy** | https://neo4j.com/labs/genai-ecosystem/ | Écosystème prêt à l'emploi si un jour Helix Foundry expose un "chain builder". |

### D. API, visualisation, agents (priorité 2)

| Composant | Doc | Apport |
| --- | --- | --- |
| **Neo4j GraphQL Library (`@neo4j/graphql`)** | https://neo4j.com/docs/graphql/current/ | API GraphQL générée depuis le schéma du graphe : accès développeur direct à l'ontologie, à côté de l'OpenAPI existante. |
| **Neo4j Visualization Library (NVL)** | https://neo4j.com/docs/nvl/current/ | Composants web de visualisation de graphe (panoramique, sous-graphes, patterns) pour l'Explorer — même famille de rendu que Bloom, sans licence Desktop. |
| **NeoDash** | https://neo4j.com/labs/neodash/ | Dashboards de graphe paramétrables (cartes, barres, sunburst, requêtes Cypher). |
| **Serveurs MCP Neo4j (`mcp-neo4j`)** | https://github.com/neo4j-contrib/mcp-neo4j | Cypher, data modeling, memory : donner à l'assistant Helix Foundry des outils d'agent capables de requêter/modéliser un graphe. Réciproquement, un **serveur MCP Helix Foundry** exposerait datasets, ontologie et Analyst aux agents externes (Claude Code, Codex…). |
| **HTTP API / Query API** | https://neo4j.com/docs/query-api/current/ | Déjà utilisés ; à documenter comme surface publique de la connectique Neo4j. |

### E. Data engineering, gouvernance, interop (priorité 3)

| Composant | Doc | Apport |
| --- | --- | --- |
| **Neosemantics (n10s)** | https://neo4j.com/labs/neosemantics/ | Import/export RDF/OWL/RDFS : échanger l'ontologie avec le monde sémantique (alignement ontologies d'entreprise, vocabulaires standards). Vrai plus "Foundry ontology". |
| **Neo4j Migrations / plugin Liquibase** | https://neo4j.com/labs/neo4j-migrations/ , https://neo4j.com/labs/liquibase/ | Versioning et migrations de schéma de graphe, reproductibles. |
| **Connector for Apache Spark** | https://neo4j.com/docs/spark/current/ | ETL batch + Structured Streaming entre Neo4j et formats Delta/Parquet/Iceberg → passerelle vers un lakehouse. |
| **BigQuery / Google Cloud Dataflow → Neo4j** | https://neo4j.com/docs/dataflow-bigquery/ | Pipelines managés GCP. |
| **ETL Tool (Labs)** | https://neo4j.com/labs/etl-tool/ | Import RDBMS → graphe assisté, utile au guided setup. |
| **neo4j-admin import / backup-restore** | https://neo4j.com/docs/operations-manual/current/tools/neo4j-admin/ | BULK import et sauvegardes du moteur embarqué. |
| **JDBC Driver** | https://neo4j.com/docs/jdbc-manual/current/ | Connecter Power BI / Tableau / outils BI au graphe. |
| **Ops Manager / monitoring** | https://neo4j.com/docs/ops-manager/current/ | Supervision du Neo4j embarqué (métriques, alertes). |
| **neomodel (Python OGM)** | https://neo4j.com/labs/neomodel/ | Modélisation objet du graphe côté executor Python. |
| **arrows.app / Cypher Workbench** | https://neo4j.com/labs/arrows/ , https://neo4j.com/labs/cypher-workbench/ | Outils de modélisation à intégrer au guided setup "Ontologie". |

### F. Voiries / éviter

- **Neo4j Bloom** : très bon mais dépend de Desktop/Enterprise Studio (licence) → NVL + NeoDash en priorité.
- **APOC Extended** (Labs) : puissant mais non supporté officiellement → usage au cas par cas.
- **Graph Analytics for Snowflake** : pertinent uniquement si un connecteur Snowflake arrive.

## 3. Au-delà de Neo4j — élargir Helix Foundry vers la couverture Foundry

1. **Connecteurs manquants** : Snowflake, BigQuery, Redshift, Databricks, MongoDB,
   Kafka, Elasticsearch/OpenSearch, Salesforce, HubSpot, Jira, Zendesk, GA4. Deux voies :
   connecteurs natifs (comme Postgres/MySQL) ou passerelle via **Airbyte**/**dlt**
   (catalogue open-source de 300+ connecteurs).
2. **Transformation** : intégration **dbt** ou **SQLMesh** (modèles SQL versionnés sur les
   snapshots) — Foundry a ses pipelines transformés.
3. **Qualité de données** : **Great Expectations** / tests **dbt** / Deequ → règles de
   validation par dataset, alertes sur schema drift (le drift est déjà détecté, il faut le
   rendre "actionnable").
4. **Lineage enrichi** : **OpenLineage** (Marquez) — le lineage basique existe (GUIDE.md),
   OpenLineage le rend standard et exportable.
5. **Orchestration** : **Prefect**, **Dagster** ou **Airflow** pour les workflows multi-sources
   (au-delà des schedules actuels).
6. **Formats ouverts & BI** : snapshots **Iceberg**/**Delta** en option, exposition
   **Arrow Flight SQL** ou ODBC/JDBC → Power BI, Tableau, **Apache Superset**, **Metabase**.
7. **Semantic layer** : **Cube** ou **dbt Semantic Layer** pour des métriques gouvernées
   (les "key metrics" du Home deviennent un référentiel partagé).
8. **MLOps** : **MLflow** (registre de modèles, expériences) et **Feast** (feature store)
   branchés sur les datasets versionnés.
9. **Reverse ETL** : pousser les objets de l'ontologie vers les outils métier
   (équivalent Foundry "write-back").
10. **Gouvernance** : audit logs, policies par rôle (déjà partiel via tokens scopés),
    export de catalogues type DataHub/OpenMetadata.

## 4. Feuille de route proposée (valeur / effort)

| # | Chantier | Valeur | Effort |
| --- | --- | --- | --- |
| 1 | Driver Bolt + fidélité des types dans le connecteur Neo4j | élevée | faible |
| 2 | Service Neo4j embarqué (profil compose `graph`) avec APOC + GDS | très élevée | moyen |
| 3 | GDS exposé comme "analyses de graphe" (résultats en datasets) | très élevée | moyen |
| 4 | CDC Neo4j (Enterprise) ou polling incrémental / Kafka sinon | élevée | moyen |
| 5 | GraphRAG + index vectoriels dans l'Analyst | élevée | moyen |
| 6 | NVL dans l'Explorer + NeoDash | moyenne | faible |
| 7 | Serveur MCP Helix Foundry (datasets + ontologie pour agents) | élevée | faible |
| 8 | n10s (RDF/OWL) pour l'import/export d'ontologies | moyenne | moyen |
| 9 | dbt + Great Expectations + OpenLineage | élevée | élevé |
| 10 | Connecteur Snowflake/BigQuery/Kafka ou passerelle Airbyte | élevée | élevé |

## 5. Prochaines étapes concrètes

1. [fait] Profil `graph` dans `compose.yaml` (image `neo4j:5-community`, plugins
   `apoc`, `gds`, volume dédié), et page **Data → Graph compute** (`/data/graph`):
   algorithmes, recettes anti-fraude et pathfinding sur tout dataset d'arêtes,
   onglet **Pipelines** pour composer les scores persistés en pipelines
   (`graph-score` → `graph-filter` → `graph-embeddings`), et export de l'ontologie
   publiée en GraphML / JSON-LD / DOT depuis l'Explorer.
2. [partiel] `apps/api/src/neo4j.ts` : la discovery (`Test connection`) rapporte
   désormais les extensions du serveur (`extensions: { apoc, gds, cdc }`).
   Reste : driver Bolt en option, fidélité des types temporels, export de masse APOC.
3. [fait] CDC `db.cdc.*` branchée sur le même mécanisme que le CDC
   PostgreSQL/MySQL (`source-sync.ts`) : la connexion devient `mode: "cdc"`,
   les changements marquent la source sale et déclenchent un import snapshot ;
   sans CDC (Community), retour aux rafraîchissements horaires.
4. Prototype GraphRAG : embeddings des objets de l'ontologie (Ollama local) + recherche
   hybride dans l'Analyst. [en cours] L'onglet Algorithms expose déjà `node_embeddings`
   et `vector_search` (auto-embeddings du projet d'arêtes), et le node pipeline
   `graph-embeddings` joint les embeddings aux lignes ; la recherche hybride
   dans l'Analyst reste à construire.
