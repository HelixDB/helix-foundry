import { APP_ORIGIN, expect, test } from "./support/mock";

// What the hosted site serves agents and crawlers (src/app/llms.txt,
// llms-full.txt, setup.md, .well-known and robots.ts) and foundry.new. The mock is no prod stamp: it
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

test("serves the setup prompt on foundry.new, and sends the rest to the site", async ({
  request,
}) => {
  const host = { host: "foundry.new" };
  const res = await request.get("/", { headers: host, maxRedirects: 0 });
  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toBe("text/markdown; charset=utf-8");
  const text = await res.text();
  expect(text).toMatch(/^> Helix Foundry’s setup, for an agent/);
  expect(text).toContain("\n# Set up Helix Foundry with me\n");
  expect(text).toContain(`\`claude mcp add --transport http helix ${MCP}\``);
  // Its links are the product's site: foundry.new signs no one in.
  expect(text).toContain("https://foundry.helix-db.com");
  expect(text).not.toContain("foundry.new/");

  for (const path of ["/login", "/ask?new=1", "/setup.md"]) {
    const away = await request.get(path, { headers: host, maxRedirects: 0 });
    expect(away.status()).toBe(307);
    expect(away.headers().location).toBe(`https://foundry.helix-db.com${path}`);
  }

  // The site's own copy, with its own links.
  const own = await request.get("/setup.md");
  expect(own.headers()["content-type"]).toBe("text/markdown; charset=utf-8");
  expect(await own.text()).toContain(`${APP_ORIGIN}`);
});

test("asks crawlers to read nothing off prod", async ({ request }) => {
  const robots = await request.get("/robots.txt");
  expect(robots.headers()["content-type"]).toContain("text/plain");
  expect((await robots.text()).trim()).toBe("User-Agent: *\nDisallow: /");
});
