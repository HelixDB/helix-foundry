// Class names the explorer's panels share (styled in ../explore.css with the
// kit's tokens), and its number formatting. Ported from helix-website's
// explorer, whose dark dashboard palette these replace.
export { cx as cn } from "../../../kit";

export const MONO = "ex-mono";
export const FOCUS_RING = "";
export const ICON_BUTTON = "ex-icon-btn";
export const SMALL_ICON_BUTTON = "ex-icon-btn is-sm";
export const OUTLINE_BUTTON = "ex-btn";
export const PRIMARY_BUTTON = "ex-btn is-primary";
export const TEXT_BUTTON = "ex-text-btn";
export const SECTION_TITLE = "ex-section-title";
export const ACTIVE_ROW = "is-active";
export const MENU_SURFACE = "ex-menu";
export const MENU_ITEM = "ex-menu-item";
export const KBD = "ex-kbd";

/** `1,234`: counts use en-US grouping everywhere. */
export function formatCount(value: number): string {
  return value.toLocaleString("en-US");
}

/** `1.2k` for tight spots like the label rail. */
export function compactCount(value: number): string {
  if (value < 1000) return String(value);
  if (value < 10_000)
    return `${(value / 1000).toFixed(1).replace(/\.0$/, "")}k`;
  if (value < 1_000_000) return `${Math.round(value / 1000)}k`;
  return `${(value / 1_000_000).toFixed(1).replace(/\.0$/, "")}M`;
}

export function plural(count: number, one: string, many = `${one}s`): string {
  return `${formatCount(count)} ${count === 1 ? one : many}`;
}
