import { APP_ORIGIN, expect, test } from "./support/mock";

// What the hosted site serves agents and crawlers (src/app/llms.txt,
// llms-full.txt, .well-known and robots.ts). The mock is no prod stamp: it
// reads no docs and asks crawlers to read nothing.

const MCP = "https://mcp.dev.helix-db.com/mcp";
const DOCS_LINK =
  "- [Helix Foundry docs](https://docs.helix-db.com/foundry/start-here/introduction)";

test("tells agents what Foundry is for and how to connect", async ({
  request,
}) => {
  const llms = await request.get("/llms.txt");
  expect(llms.status()).toBe(200);
  expect(llms.headers()["content-type"]).toBe("text/plain; charset=utf-8");
  expect(llms.headers()["x-robots-tag"]).toBe("noindex");
  const text = await llms.text();
  expect(text).toMatch(/^# Helix Foundry\n\n> Helix Foundry turns/);
  for (const line of [
    `- Sign in: ${APP_ORIGIN}/login (Foundry Free is $0)`,
    `- Helix MCP: ${MCP}`,
    `\`claude mcp add --transport http helix ${MCP}\``,
    "## When to use Helix Foundry",
    "`helix_foundry_ask`",
    "- Foundry Pro, $100 a month",
    DOCS_LINK,
  ])
    expect(text).toContain(line);

  const full = await (await request.get("/llms-full.txt")).text();
  expect(full.startsWith(text.slice(0, text.indexOf("## Plans")))).toBe(true);
  expect(full).toContain("## Setup prompt for an agent");
  expect(full).toContain(new URL("/auth.md", MCP).href);

  // The same files where some scanners look for them.
  expect(await (await request.get("/.well-known/llms.txt")).text()).toBe(text);
  expect(await (await request.get("/.well-known/llms-full.txt")).text()).toBe(
    full,
  );

  const mcp = await request.get("/.well-known/mcp.json");
  expect(mcp.headers()["content-type"]).toBe("application/json");
  expect(await mcp.json()).toMatchObject({
    name: "Helix Foundry",
    mcp: { public_server: MCP, transport: "streamable-http" },
    links: { llms: `${APP_ORIGIN}/llms.txt` },
  });
  const catalog = await request.get("/.well-known/ai-catalog.json");
  expect(catalog.headers()["content-type"]).toBe("application/ai-catalog+json");
  expect((await catalog.json()).entries).toHaveLength(2);
});

test("asks crawlers to read nothing off prod", async ({ request }) => {
  const robots = await request.get("/robots.txt");
  expect(robots.headers()["content-type"]).toContain("text/plain");
  expect((await robots.text()).trim()).toBe("User-Agent: *\nDisallow: /");
});
