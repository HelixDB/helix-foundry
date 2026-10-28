# Helix Foundry

Connect your company's data, let an agent draft an ontology over it, and load it into [HelixDB](https://docs.helix-db.com) as one connected graph you can explore and ask questions of.

- Connect PostgreSQL, MySQL, Amazon S3, Stripe, PostHog, WorkOS, GitHub, Linear, Granola and Cal.com, and upload documents (Markdown, text, PDF) and table files (CSV, TSV, JSON, JSON Lines, Parquet).
- An agent drafts the ontology: types, links, the tables and documents each comes from, and rules. You review it, edit it and make a version current.
- A load writes the version into HelixDB. Explore it as a graph or as records, follow every value back to its row or its quote, and ask questions with cited answers.

Use it hosted at [foundry.helix-db.com](https://foundry.helix-db.com), or run it on your own computer.

## Run it on your computer

You need Docker with Compose v2.

```sh
git clone https://github.com/helixdb/helix-foundry.git
cd helix-foundry
docker compose up -d --wait
```

Open http://localhost:3000. There is no sign-in and nothing to set up: one owner, every connector and feature on, and a workspace to start in. The first start writes the install's secrets into a Docker volume; keep it (`docker compose down -v` deletes it with your data).

No data to hand? The first page offers **Use the sample**: Acme Corp, a made-up software company, as tables (customers, subscriptions, invoices, support tickets, usage) and documents (contracts, call notes, an incident report). One click adds it and fills in your first ontology, ready to draft.

AI runs where it finds it: Claude or OpenAI when the shell you ran `docker compose up` from has an `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`, else a model that calls tools in Ollama (`docker compose --profile ollama up -d` runs one beside Foundry, or use Ollama on your computer), else LM Studio on your computer. **Settings › AI models** says which it chose and why, and switches to Ollama, Claude, OpenAI, Azure OpenAI or Amazon Bedrock with your own key, or any OpenAI-compatible server (vLLM, a gateway). [local/README.md](local/README.md) says what runs, where data lives, what may be overridden, how each provider is set up and how to reach databases on this machine.

## Develop

The site, the Foundry app and its same-origin API proxy (Next.js), is at the root. It needs Node 24 and pnpm.

```sh
pnpm install
pnpm dev:mock        # the hosted site against a mock API, on http://localhost:3000
pnpm dev:mock:local  # the local build against the mock
```

The mock stands in for the API and sign-in, so no account or secret is needed. Before sending a change, run `pnpm typecheck`, `pnpm lint`, `pnpm test` and `pnpm e2e` (Playwright; needs `pnpm exec playwright install chromium`). [AGENTS.md](AGENTS.md) describes the layout and the rules.

## Backend

[`backend/`](backend) is Foundry's backend, the Go module `github.com/helixdb/helix-foundry/backend`: the API, the workers that sync data, draft ontologies, load them into HelixDB and answer questions, and their MySQL store. [backend/README.md](backend/README.md) says how to build, test and run it. Hosted Helix runs the same code as a library.

## License

Everything in this repository, the site and the backend, is [Apache 2.0](LICENSE), and so are Foundry's three images: `foundry-site` and `foundry`, built from it, and `ontology-agent`, the ontology agent's. HelixDB, which `local/compose.yaml` runs beside them, is [Apache 2.0](https://github.com/HelixDB/helix-db) too.
