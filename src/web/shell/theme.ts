import { useSyncExternalStore } from "react";
import {
  THEME_BACKGROUNDS,
  THEME_KEY,
  parseTheme,
  resolveTheme,
  type Theme,
  type ThemePreference,
} from "../../shared/theme";
import { readStorage, writeStorage } from "../kit/util";

// Changing the theme, and following it in what draws its own colors. The
// layout applies the stored choice before first paint (THEME_BOOT in
// src/shared/theme.ts); CSS follows data-theme and the OS by itself.

export type { Theme, ThemePreference };
export const themeOptions: { value: ThemePreference; label: string }[] = [
  { value: "system", label: "System" },
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
];
const DARK_QUERY = "(prefers-color-scheme: dark)";

/** The stored preference; System when none is stored or storage is blocked. */
export const readTheme = (): ThemePreference =>
  parseTheme(readStorage(THEME_KEY, "system"));

/** Stores a preference. System is the default, so it clears the key. */
export const writeTheme = (preference: ThemePreference) =>
  writeStorage(THEME_KEY, preference === "system" ? null : preference);

/** Puts a preference on <html> and the browser chrome. */
export function applyTheme(preference: ThemePreference) {
  const root = document.documentElement;
  if (preference === "system") delete root.dataset.theme;
  else root.dataset.theme = preference;
  // Each theme-color meta keeps its media query for System and takes the
  // chosen theme's background otherwise.
  document
    .querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')
    .forEach((meta) => {
      meta.content =
        THEME_BACKGROUNDS[
          resolveTheme(preference, meta.media.includes("dark"))
        ];
    });
}

// The choice on <html> and the theme the page shows, kept by one watcher of
// data-theme and the OS setting: for the menus that offer the choice, and
// for everything that draws its own colors (canvas, SVG, logos).
let chosen: ThemePreference = "system";
let shown: Theme = "light";
let watching = false;
const choiceListeners = new Set<() => void>();
const shownListeners = new Set<() => void>();

const readChosen = () => parseTheme(document.documentElement.dataset.theme);
const readShown = () =>
  resolveTheme(readChosen(), matchMedia(DARK_QUERY).matches);
function watch() {
  if (watching) return;
  watching = true;
  chosen = readChosen();
  shown = readShown();
  const update = () => {
    const nextChosen = readChosen(),
      nextShown = readShown();
    if (nextChosen !== chosen) {
      chosen = nextChosen;
      choiceListeners.forEach((listener) => listener());
    }
    if (nextShown !== shown) {
      shown = nextShown;
      shownListeners.forEach((listener) => listener());
    }
  };
  matchMedia(DARK_QUERY).addEventListener("change", update);
  new MutationObserver(update).observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["data-theme"],
  });
  // A choice made in another tab applies here too.
  window.addEventListener("storage", (e) => {
    if (e.key === THEME_KEY) applyTheme(parseTheme(e.newValue));
  });
}
const subscribe =
  (listeners: Set<() => void>) =>
  (listener: () => void): (() => void) => {
    if (typeof document === "undefined") return () => {};
    watch();
    listeners.add(listener);
    return () => void listeners.delete(listener);
  };

/** The choice the page follows; System where there is no document. */
export function currentChoice(): ThemePreference {
  if (typeof document === "undefined") return "system";
  watch();
  return chosen;
}
/** Calls `listener` whenever the choice changes; returns the unsubscribe. */
export const subscribeChoice = subscribe(choiceListeners);

/** The theme the page shows; light where there is no document. */
export function currentTheme(): Theme {
  if (typeof document === "undefined") return "light";
  watch();
  return shown;
}
/** Calls `listener` whenever the page's theme changes; returns the unsubscribe. */
export const subscribeTheme = subscribe(shownListeners);

/** The page's theme, re-rendering when it changes. */
export const useCurrentTheme = (): Theme =>
  useSyncExternalStore(subscribeTheme, currentTheme, () => "light");

/**
 * The choice and a setter that stores and applies it. Every menu offering it
 * shows the same choice, also one made in another tab.
 */
export function useTheme() {
  const theme = useSyncExternalStore(
    subscribeChoice,
    currentChoice,
    () => "system" as const,
  );
  return [
    theme,
    (next: ThemePreference) => {
      writeTheme(next);
      applyTheme(next);
    },
  ] as const;
}
