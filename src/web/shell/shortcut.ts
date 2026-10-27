const isMac =
  typeof navigator !== "undefined" &&
  /Mac|iPhone|iPad/.test(navigator.platform);
// The palette shortcut as shown to the user, and for aria-keyshortcuts.
export const shortcut = isMac ? "⌘K" : "Ctrl K";
export const shortcutKeys = isMac ? ["⌘", "K"] : ["Ctrl", "K"];
export const ariaShortcut = "Meta+K Control+K";
