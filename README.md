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
cd helix-foundry/local
./setup.sh                  # once: writes .env with this installation's secrets, and asks how AI should run
docker compose build        # the site's and the backend's images, from this checkout
docker compose up -d --wait
```

Foundry's first release, 0.1.0, will publish its images. Until then the ontology agent's image is not public, and the stack does not start without it.

Open http://localhost:3000. There is no sign-in: one owner, every connector and feature on.

AI runs where you choose: a local model with Ollama (in Docker or on your computer), Claude, OpenAI, Azure OpenAI or Amazon Bedrock with your own key, or any OpenAI-compatible server (LM Studio, vLLM, a gateway). `setup.sh` asks once; **Settings › AI models** tests and switches it later. [local/README.md](local/README.md) says what runs, where data lives, how each provider is set up and how to reach databases on this machine.

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

This repository, the site and the backend, is [Apache 2.0](LICENSE), and the `foundry-site` and `foundry` images are built from it. Of the other images `local/compose.yaml` runs, HelixDB is open source ([Apache 2.0](https://github.com/HelixDB/helix-db)); `ontology-agent` is not open source yet and is free to run under the [Helix Foundry Images License](LICENSE-IMAGES.md).
