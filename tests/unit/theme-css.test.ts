import { readFileSync, readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";
import { THEME_BACKGROUNDS } from "@/shared/theme";
import { LABEL_PALETTE } from "@/web/areas/explore/explorer/lib/theme";
import { TYPE_HUES } from "@/web/areas/ontology/colors";

// The color contract: every color is a --hf-* token in src/web/workspace.css,
// light on :root and dark in two identical blocks (the OS's dark unless the
// user chose light, and the user's dark). Other CSS reads only tokens, and
// the dark theme keeps WCAG AA text and 3:1 UI contrast on every surface.

const WEB = join(__dirname, "../../src/web");
const SHEET = join(WEB, "workspace.css");
const files = (dir: string): string[] =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? files(join(dir, e.name)) : [join(dir, e.name)],
  );
const cssFiles = files(WEB).filter((f) => f.endsWith(".css"));
const sheet = readFileSync(SHEET, "utf8");
const stripComments = (css: string) => css.replace(/\/\*[\s\S]*?\*\//g, "");

/** The declarations of a block of `css` that starts with `head` and `{`. */
function block(css: string, head: string): Map<string, string> {
  const start = css.indexOf(head + " {");
  expect(start, head).toBeGreaterThanOrEqual(0);
  const body = css.slice(start + head.length + 2, css.indexOf("\n}", start));
  return new Map(
    [...stripComments(body).matchAll(/(--hf-[\w-]+)\s*:\s*([^;]+);/g)].map(
      (m) => [m[1], m[2].replace(/\s+/g, " ").trim()],
    ),
  );
}
const light = block(sheet, ":root");
const darkMedia = block(sheet, '  :root:not([data-theme="light"])');
const darkChosen = block(sheet, ':root[data-theme="dark"]');
const dark = new Map([...light, ...darkChosen]);
const isColor = (v: string) => /^(#|rgba?\()/.test(v);
// Tokens that are the same in both themes, so the dark blocks leave them out.
const SAME_IN_BOTH = ["--hf-on-accent"];

// A color literal: hex, a color function, or a named color.
const LITERAL =
  /#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\(|\b(?:white|black|red|green|blue|gray|grey|silver|orange|yellow|purple|pink|navy|teal)\b/i;

describe("color literals", () => {
  it("appear only as token values in workspace.css", () => {
    const found: string[] = [];
    for (const file of cssFiles) {
      const css = stripComments(readFileSync(file, "utf8"));
      for (const body of css.matchAll(/\{([^{}]*)\}/g))
        for (const decl of body[1].split(";")) {
          const colon = decl.indexOf(":");
          if (colon < 0) continue;
          const prop = decl.slice(0, colon).trim(),
            value = decl.slice(colon + 1);
          if (file === SHEET && prop.startsWith("--hf-")) continue;
          if (/^(white-space|font-family|grid-template-areas)$/.test(prop))
            continue;
          if (LITERAL.test(value))
            found.push(`${relative(WEB, file)}: ${prop}:${value.trim()}`);
        }
    }
    expect(found).toEqual([]);
  });

  it("are not used through a token nothing defines", () => {
    // Tokens come from the sheet; a few layout variables are set by the
    // components (style={{ "--hf-grid-template": ... }}) or other CSS.
    const defined = new Set<string>();
    for (const file of files(WEB).filter((f) => /\.(css|tsx?)$/.test(f)))
      for (const m of readFileSync(file, "utf8").matchAll(
        file.endsWith(".css") ? /(--hf-[\w-]+)\s*:/g : /["'](--hf-[\w-]+)["']/g,
      ))
        defined.add(m[1]);
    const missing = new Set<string>();
    for (const file of cssFiles)
      for (const m of readFileSync(file, "utf8").matchAll(
        /var\((--hf-[\w-]+)/g,
      ))
        if (!defined.has(m[1])) missing.add(`${relative(WEB, file)} ${m[1]}`);
    expect([...missing]).toEqual([]);
  });
});

describe("the dark set", () => {
  it("is the same for the OS's dark and the user's", () => {
    expect([...darkMedia]).toEqual([...darkChosen]);
  });

  it("gives every light color a dark one, or says it is the same", () => {
    const lightColors = [...light].filter(([, v]) => isColor(v));
    expect(lightColors.length).toBeGreaterThan(50);
    for (const [token] of lightColors)
      if (!SAME_IN_BOTH.includes(token))
        expect(darkChosen.has(token), token).toBe(true);
    for (const token of SAME_IN_BOTH)
      expect(darkChosen.has(token), token).toBe(false);
    for (const token of darkChosen.keys())
      expect(light.has(token), token).toBe(true);
  });

  it("sets color-scheme for the browser's own controls", () => {
    expect(stripComments(sheet)).toMatch(
      /:root \{[^}]*color-scheme: light;[^}]*\}/,
    );
    expect(stripComments(sheet).match(/color-scheme: dark;/g)?.length).toBe(2);
  });
});

describe("the canvas and the page", () => {
  it("share the background the browser chrome takes", () => {
    expect(light.get("--hf-bg")).toBe(THEME_BACKGROUNDS.light);
    expect(dark.get("--hf-bg")).toBe(THEME_BACKGROUNDS.dark);
  });

  it("draw entity types in the same hues", () => {
    for (const [theme, tokens] of [
      ["light", light],
      ["dark", dark],
    ] as const)
      expect(
        Array.from({ length: TYPE_HUES }, (_, i) =>
          tokens.get(`--hf-type-${i + 1}`)?.toUpperCase(),
        ),
      ).toEqual([...LABEL_PALETTE[theme]]);
  });
});

// WCAG relative luminance and contrast of two #rrggbb colors.
const channel = (v: number) =>
  (v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
const luminance = (hex: string) => {
  const n = parseInt(hex.slice(1), 16);
  return (
    0.2126 * channel((n >> 16) & 255) +
    0.7152 * channel((n >> 8) & 255) +
    0.0722 * channel(n & 255)
  );
};
const contrast = (a: string, b: string) => {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

describe("contrast", () => {
  const surfaces = ["bg", "raised", "sunken", "sidebar", "hover", "selected"];
  const text = [
    "text",
    "text-2",
    "muted",
    "accent-text",
    "success-text",
    "warning-text",
    "danger-text",
    "info-text",
  ];
  const ui = ["faint", "focus", "accent", "success", "warning", "danger"];
  const pairs = (tokens: Map<string, string>, fg: string[], bg: string[]) =>
    fg.flatMap((f) =>
      bg.map((b) => ({
        pair: `${f} on ${b}`,
        ratio: contrast(tokens.get(`--hf-${f}`)!, tokens.get(`--hf-${b}`)!),
      })),
    );
  const below = (list: { pair: string; ratio: number }[], min: number) =>
    list.filter((p) => p.ratio < min).map((p) => p.pair);

  it("keeps dark text at 4.5:1 on every surface", () => {
    expect(below(pairs(dark, text, [...surfaces, "field"]), 4.5)).toEqual([]);
  });

  it("keeps dark icons, marks and focus rings at 3:1", () => {
    expect(below(pairs(dark, ui, [...surfaces, "field"]), 3)).toEqual([]);
    expect(below(pairs(dark, ["info"], surfaces), 3)).toEqual([]);
    expect(
      below(
        pairs(
          dark,
          Array.from({ length: TYPE_HUES }, (_, i) => `type-${i + 1}`),
          ["bg", "raised"],
        ),
        3,
      ),
    ).toEqual([]);
  });

  it("keeps light text at 4.5:1 where text sits", () => {
    expect(
      below(pairs(light, text, ["bg", "raised", "sunken", "field"]), 4.5),
    ).toEqual([]);
    expect(below(pairs(light, ["focus"], surfaces), 3)).toEqual([]);
  });

  it("keeps text on fills, tints and popovers at 4.5:1 in both themes", () => {
    for (const tokens of [light, dark]) {
      const on = (f: string, b: string) =>
        contrast(tokens.get(`--hf-${f}`)!, tokens.get(`--hf-${b}`)!);
      for (const [f, b] of [
        ["on-ink", "ink"],
        ["on-accent", "accent"],
        ["on-danger", "danger"],
        ["accent-text", "accent-soft"],
        ["warning-text", "warning-soft"],
        ["danger-text", "danger-soft"],
        ["info-text", "info-soft"],
        ["on-tooltip", "tooltip"],
        ["on-tooltip-2", "tooltip"],
        ["on-tooltip-muted", "tooltip"],
        ["on-tooltip-accent", "tooltip"],
        ["on-tooltip-warning", "tooltip"],
      ])
        expect(on(f, b), `${f} on ${b}`).toBeGreaterThanOrEqual(4.5);
    }
  });
});
