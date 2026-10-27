import { currentTheme } from "../../../../shell/theme";

// Colors for the explorer canvas, in Foundry's light and dark themes: the
// kit's tokens (src/web/workspace.css) as hex, since the canvas draws with
// them directly. Every read follows the page's theme (shell/theme.ts watches
// data-theme and the OS setting), and the canvas draws a new frame when it
// changes, so a switch shows at once.

const THEMES = {
  light: {
    background: "#F8F7F3",
    grid: "#E4E2DA",
    edge: "#B7B5AB",
    edgeArrow: "#9B9D93",
    edgeDim: "#ECEAE2",
    edgeHover: "#65675E",
    edgeHighlight: "#343932",
    selection: "#343932",
    nodeOutline: "#F8F7F3",
    pinned: "#292A26",
    caption: "#4F534A",
    captionStrong: "#292A26",
    captionHalo: "#F8F7F3",
    tooltipBg: "#FDFBF9",
    tooltipBorder: "#D4D2C7",
    tooltipText: "#292A26",
    tooltipMeta: "#65675E",
    internalNode: "#9B9D93",
    dimAlpha: 0.16,
    hoverRingAlpha: 0.35,
  },
  dark: {
    background: "#161713",
    grid: "#2E3029",
    edge: "#4A4D43",
    edgeArrow: "#797B71",
    edgeDim: "#22241E",
    edgeHover: "#9C9D92",
    edgeHighlight: "#E9E8DF",
    selection: "#E9E8DF",
    nodeOutline: "#161713",
    pinned: "#ECEBE4",
    caption: "#C9C8BE",
    captionStrong: "#ECEBE4",
    captionHalo: "#161713",
    tooltipBg: "#1E1F1A",
    tooltipBorder: "#3D4036",
    tooltipText: "#ECEBE4",
    tooltipMeta: "#9C9D92",
    internalNode: "#797B71",
    dimAlpha: 0.14,
    hoverRingAlpha: 0.4,
  },
} as const;

type Theme = {
  -readonly [K in keyof (typeof THEMES)["light"]]: (typeof THEMES)["light"][K] extends number
    ? number
    : string;
};

/** The current theme's canvas colors; every read follows a theme switch. */
export const CANVAS_THEME: Readonly<Theme> = Object.defineProperties(
  {} as Theme,
  Object.fromEntries(
    Object.keys(THEMES.light).map((key) => [
      key,
      {
        enumerable: true,
        get: () => THEMES[currentTheme()][key as keyof Theme],
      },
    ]),
  ),
);

/**
 * Node colors by label: the kit's categorical hues (validated for both
 * themes), assigned in registration order, so the first eight labels never
 * share a color. Selection draws in the ink color, which none of them is.
 */
export const LABEL_PALETTE = {
  light: [
    "#2A78D6",
    "#EB6834",
    "#1BAF7A",
    "#EDA100",
    "#E87BA4",
    "#008300",
    "#4A3AA7",
    "#E34948",
  ],
  dark: [
    "#3987E5",
    "#D95926",
    "#199E70",
    "#C98500",
    "#D55181",
    "#008300",
    "#9085E9",
    "#E66767",
  ],
} as const;

/**
 * The color for the label registered at `index`, in the current theme. Past
 * the palette the colors repeat, lighter on the second lap and deeper on the
 * third, so neighbors in a large schema are still told apart.
 */
export function labelColorAt(
  index: number,
  theme: "light" | "dark" = currentTheme(),
): string {
  const palette = LABEL_PALETTE[theme],
    size = palette.length;
  const i = Math.max(0, Math.floor(index));
  const base = palette[i % size]!;
  const lap = Math.floor(i / size) % 3;
  if (lap === 1) return mixHex(base, "#FFFFFF", 0.35);
  if (lap === 2) return mixHex(base, "#000000", 0.22);
  return base;
}

/**
 * A label → color assignment that never changes once made (its color then
 * follows the theme). Register the overview's sorted labels first; labels
 * seen later get the next free slot.
 */
export function createLabelColors(): {
  colorFor(label: string): string;
  register(labels: string[]): void;
} {
  const assigned = new Map<string, number>();
  const register = (labels: string[]) => {
    for (const label of labels) {
      if (!assigned.has(label)) assigned.set(label, assigned.size);
    }
  };
  return {
    colorFor(label: string) {
      let slot = assigned.get(label);
      if (slot === undefined) {
        slot = assigned.size;
        assigned.set(label, slot);
      }
      return labelColorAt(slot);
    },
    register,
  };
}

/** `#RRGGBB` → `rgba(r, g, b, alpha)`. Other inputs are returned unchanged. */
export function withAlpha(hex: string, alpha: number): string {
  const rgb = parseHex(hex);
  if (!rgb) return hex;
  return `rgba(${rgb[0]}, ${rgb[1]}, ${rgb[2]}, ${alpha})`;
}

/** Linear mix of two `#RRGGBB` colors; `t` = 0 gives `a`, 1 gives `b`. */
export function mixHex(a: string, b: string, t: number): string {
  const ca = parseHex(a);
  const cb = parseHex(b);
  if (!ca || !cb) return a;
  const channel = (i: number) =>
    Math.round(ca[i]! + (cb[i]! - ca[i]!) * t)
      .toString(16)
      .padStart(2, "0");
  return `#${channel(0)}${channel(1)}${channel(2)}`.toUpperCase();
}

function parseHex(hex: string): [number, number, number] | null {
  const match = /^#([0-9a-f]{6})$/i.exec(hex);
  if (!match) return null;
  const value = parseInt(match[1]!, 16);
  return [(value >> 16) & 255, (value >> 8) & 255, value & 255];
}
