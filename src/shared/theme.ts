// The site's theme, shared by the root layout (which applies it before first
// paint) and the SPA (which lets the user change it: src/web/shell/theme.ts).
// The user picks System, Light or Dark, kept per browser in localStorage.
// data-theme on <html> carries an explicit choice and the CSS tokens
// (src/web/workspace.css) follow it; without it they follow the OS through
// prefers-color-scheme.

/** What the user chose: follow the OS, or one theme always. */
export type ThemePreference = "system" | "light" | "dark";
/** The theme a page shows. */
export type Theme = "light" | "dark";

export const THEME_KEY = "foundry.theme";

/** Each theme's --hf-bg, for the browser chrome (meta theme-color). */
export const THEME_BACKGROUNDS: Record<Theme, string> = {
  light: "#f8f7f3",
  dark: "#161713",
};

/** A stored value as a preference: anything but light or dark is System. */
export const parseTheme = (raw: string | null | undefined): ThemePreference =>
  raw === "light" || raw === "dark" ? raw : "system";

/** The theme a preference shows, given whether the OS prefers dark. */
export const resolveTheme = (
  preference: ThemePreference,
  prefersDark: boolean,
): Theme =>
  preference === "system" ? (prefersDark ? "dark" : "light") : preference;

/**
 * The inline script the root layout runs in <head>, before anything paints:
 * an explicit choice sets data-theme, and System leaves it to the media query.
 * Storage can throw (blocked, private mode); then the OS decides.
 */
export const THEME_BOOT = `try{var t=localStorage.getItem(${JSON.stringify(THEME_KEY)});if(t==="light"||t==="dark")document.documentElement.dataset.theme=t}catch(e){}`;
