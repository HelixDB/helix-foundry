import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath, pathToFileURL } from "node:url";
import { build } from "esbuild";

// Builds Foundry's MCP Apps views (src/mcp-apps) into the Go module
// (backend/integration/apps), one HTML document a view, its script and
// styles inline: a host draws it in a sandboxed frame that loads nothing
// else (the resource declares no CSP domains). The documents are committed,
// since hosted Helix builds the module from its source:
// `pnpm mcp-apps` writes them, and tests/unit/mcp-apps.test.ts fails while a
// committed one differs from what the sources build.

const ROOT = fileURLToPath(new URL("..", import.meta.url));

/** Each view: its entry and the document the Go module embeds. */
export const VIEWS = [
  {
    entry: "src/mcp-apps/main.tsx",
    out: "backend/integration/apps/answer.html",
    title: "Foundry answer",
  },
];

/** A view's document, built from its entry. */
export async function buildView(view: (typeof VIEWS)[number]): Promise<string> {
  const result = await build({
    absWorkingDir: ROOT,
    entryPoints: [view.entry],
    bundle: true,
    write: false,
    outdir: "out",
    format: "iife",
    platform: "browser",
    target: "es2022",
    minify: true,
    legalComments: "none",
    charset: "utf8",
    jsx: "automatic",
    // The website reads these at `next build`; a view is the hosted site's,
    // whichever Foundry serves it, and React runs its production build.
    define: {
      "process.env.NODE_ENV": '"production"',
      "process.env": "{}",
    },
    logLevel: "silent",
  });
  const file = (ext: string) =>
    result.outputFiles.find((f) => f.path.endsWith(ext))?.text ?? "";
  const script = file(".js"),
    style = file(".css");
  // esbuild escapes these in what it writes; a library's own would end the
  // element early.
  if (/<\/script/i.test(script) || /<\/style/i.test(style))
    throw new Error(`${view.entry}: its output closes its own element`);
  return [
    "<!doctype html>",
    '<html lang="en">',
    "<head>",
    '<meta charset="utf-8">',
    '<meta name="viewport" content="width=device-width, initial-scale=1">',
    `<title>${view.title}</title>`,
    `<style>${style}</style>`,
    "</head>",
    "<body>",
    '<div id="root"></div>',
    `<script>${script}</script>`,
    "</body>",
    "</html>",
    "",
  ].join("\n");
}

/** The committed document of a view, or "" before there is one. */
export const committed = (view: (typeof VIEWS)[number]) =>
  readFile(new URL(view.out, pathToFileURL(ROOT + "/")), "utf8").catch(
    () => "",
  );

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  for (const view of VIEWS) {
    const html = await buildView(view);
    await writeFile(new URL(view.out, pathToFileURL(ROOT + "/")), html);
    console.log(`${view.out}: ${(html.length / 1024).toFixed(0)} KiB`);
  }
}
