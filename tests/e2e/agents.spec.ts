import { APP_ORIGIN, expect, test } from "./support/mock";

// What the hosted site serves agents and crawlers (src/app/llms.txt,
// llms-full.txt, index.md, .well-known, robots.ts and sitemap.ts, and the
// landing page's metadata). The mock is no prod stamp: it reads no docs and
// asks crawlers to read nothing.

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
  expect(full).toContain(`${MCP.replace("/mcp", "")}/auth.md`);

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
  expect(catalog.headers()["content-type"]).toBe(
    "application/ai-catalog+json",
  );
  expect((await catalog.json()).entries).toHaveLength(2);
});

test("serves the landing page as Markdown to a client that asks for it", async ({
  request,
}) => {
  const asked = await request.get("/", {
    headers: { accept: "text/markdown, text/html;q=0.9" },
  });
  expect(asked.status()).toBe(200);
  expect(asked.headers()["content-type"]).toBe("text/markdown; charset=utf-8");
  expect(asked.headers().vary).toContain("Accept");
  const md = await asked.text();
  expect(md).toMatch(
    /^# Helix Foundry\n\n## Your company’s data, connected into one ontology\n/,
  );
  expect(md).toContain("## Set up with your agent");
  expect(await (await request.get("/index.md")).text()).toBe(md);

  // A browser gets the page.
  const html = await request.get("/", {
    headers: { accept: "text/html,application/xhtml+xml,*/*;q=0.8" },
  });
  expect(html.headers()["content-type"]).toContain("text/html");
});

test("asks crawlers to read nothing off prod", async ({ request }) => {
  const robots = await request.get("/robots.txt");
  expect(robots.headers()["content-type"]).toContain("text/plain");
  expect((await robots.text()).trim()).toBe("User-Agent: *\nDisallow: /");
  const sitemap = await (await request.get("/sitemap.xml")).text();
  expect(sitemap).toContain("<urlset");
  expect(sitemap).not.toContain("<url>");
});

test("describes Foundry on the landing page", async ({ page }) => {
  await page.goto("/");
  const ld = JSON.parse(
    (await page
      .locator('script[type="application/ld+json"]')
      .textContent()) as string,
  );
  expect(ld).toMatchObject({
    "@type": "SoftwareApplication",
    name: "Helix Foundry",
  });
  expect(ld.offers.map((o: { price: string }) => o.price)).toEqual([
    "0",
    "100",
    "500",
  ]);
  await expect(
    page.locator('link[rel="alternate"][type="text/markdown"]'),
  ).toHaveAttribute("href", "https://foundry.helix-db.com/index.md");
  await expect(page.locator('link[rel="canonical"]')).toHaveAttribute(
    "href",
    /^https:\/\/foundry\.helix-db\.com\/?$/,
  );
  await expect(page.getByRole("link", { name: "For agents" })).toHaveAttribute(
    "href",
    "/llms.txt",
  );
});
