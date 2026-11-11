// The landing page as Markdown (/index.md, and / for a client that asks for
// Markdown: src/proxy.ts), for agents. Its copy is landing.tsx's, word for
// word, which tests/unit/agent-context.test.ts checks; the figures are
// described, not drawn. Then the prompt “Set up with your agent” copies,
// and where to read more.

/** The landing page's words, in its order. */
export const LANDING_COPY = {
  heading: "Your company’s data, connected into one ontology",
  sub: "Connect your database, Stripe, WorkOS, PostHog and your files. Foundry suggests one ontology across them that you explore and ask questions of.",
  schema:
    "A suggested ontology that matches the same records across sources.",
  explorer: [
    "Everything a load wrote: its entities and facts.",
    "Open a record to see its properties and every relationship, in and out, with the load and the version that wrote it.",
  ],
  ontology: "One model of your business that every app builds on.",
  analyst: "Ask in plain words, and get an answer you can check.",
  point: [
    "Nobody has to agree on a schema first.",
    "Connect a source. Foundry suggests the ontology, you keep or change it, and nothing is published until you say so.",
  ],
  connect: [
    "Connect the data you already run on, and the models you’re already approved to use.",
    "Bring your own keys for Claude, OpenAI, Azure or Bedrock, any OpenAI-compatible endpoint, or a model that runs locally. Self-hosted, your data stays on your own infrastructure, and columns that look sensitive are masked before samples reach a hosted model, so you stay within the policies you already have.",
  ],
  cloud: [
    "Now there is nothing to host.",
    "Helix Cloud runs the same build we publish, so you connect a source and explore the ontology without standing up HelixDB, DuckDB and Ollama yourself.",
    "Self-hosted Foundry listens on 127.0.0.1 only and has no sign-in: anyone on that machine is already in.",
  ],
  footer:
    "Helix Foundry is Apache 2.0 on your own machine, and Helix Cloud when you would rather we ran it.",
} as const;

const c = LANDING_COPY;

/**
 * The landing page in Markdown, on `origin`, ending with `prompt`: the
 * setup prompt for someone without a workspace.
 */
export const landingMarkdown = (
  origin: string,
  source: string,
  prompt: string,
): string => `# Helix Foundry

## ${c.heading}

${c.sub}

- [Start on Helix Cloud](${origin}/login): Foundry Free is $0.
- Set up with your agent: give it the prompt at the end of this page.
- Run it on your own machine: \`git clone ${source}.git && cd helix-foundry && docker compose up -d --wait\`
- For agents: [llms.txt](${origin}/llms.txt) says when to use Foundry, how to connect and its tools; [llms-full.txt](${origin}/llms-full.txt) adds its docs.

## Schema

${c.schema}

## Explorer

${c.explorer.join("\n\n")}

## 01 Ontology

${c.ontology}

## 02 Analyst

${c.analyst}

## The point

${c.point.join("\n\n")}

## Connect

${c.connect.join("\n\n")}

## Cloud

${c.cloud.join("\n\n")}

${c.footer} [Source on GitHub](${source}).

## Set up with your agent

The prompt to give an agent with a terminal (Claude Code, Codex): it adds the Helix MCP, signs you up or in, and sets up your first ontology with you.

\`\`\`\`markdown
${prompt}
\`\`\`\`
`;
