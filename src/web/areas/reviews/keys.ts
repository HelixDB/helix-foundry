import { useEffect, useRef } from "react";

// The review walkthrough's keys. A digit saves its choice at once (Z undoes
// it); Enter accepts Founder's suggestion; nothing is preselected. Keys are
// off while a dialog is open, in a field, and while a key is held down (its
// repeats would decide one card after another); Enter on a control and the
// arrows in a radio group or tabs are that control's; ⌘J, ⌘K and every
// other modified key stay with the shell and the browser.

export type KeyCommand =
  | { kind: "accept" }
  | { kind: "choose"; index: number }
  | { kind: "not_in_data" }
  | { kind: "reject" }
  | { kind: "skip" }
  | { kind: "back" }
  | { kind: "undo" }
  | { kind: "batch" }
  | { kind: "open" }
  | { kind: "founder" }
  | { kind: "help" };

/**
 * Where a key was pressed: on the page; in a dialog, any of the app's, or a
 * field (a text box, a select, a list box, a menu, a tree, a grid), which
 * take every key; on a composite control (a radio group, tabs, a slider),
 * whose arrows and Enter are its own; or on a control (a button, a link),
 * whose Enter is its own.
 */
export type KeyPlace = "page" | "dialog" | "field" | "composite" | "control";

const LETTERS: Record<string, KeyCommand> = {
  n: { kind: "not_in_data" },
  x: { kind: "reject" },
  s: { kind: "skip" },
  z: { kind: "undo" },
  g: { kind: "batch" },
  o: { kind: "open" },
  f: { kind: "founder" },
};

/**
 * The command a key press means on a card offering `choices` numbered
 * choices, pressed at `place`; null for none, for every key in a dialog or
 * a field, and for a held key's repeats. ⌘Z or Ctrl+Z is Undo; any other
 * modified key is no command, and so is Shift with anything but `?`.
 *
 * @example
 * keyCommand({ key: "2", metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, repeat: false }, "page", 3); // { kind: "choose", index: 1 }
 * keyCommand({ key: "k", metaKey: true, ctrlKey: false, altKey: false, shiftKey: false, repeat: false }, "page", 3); // null
 */
export function keyCommand(
  e: Pick<
    KeyboardEvent,
    "key" | "metaKey" | "ctrlKey" | "altKey" | "shiftKey" | "repeat"
  >,
  place: KeyPlace,
  choices: number,
): KeyCommand | null {
  if (e.repeat || e.altKey || place === "dialog" || place === "field")
    return null;
  if (e.metaKey || e.ctrlKey)
    return e.key.toLowerCase() === "z" && !e.shiftKey ? { kind: "undo" } : null;
  if (e.key === "?") return { kind: "help" };
  if (e.shiftKey) return null;
  if (/^[1-9]$/.test(e.key)) {
    const n = Number(e.key);
    return n <= choices ? { kind: "choose", index: n - 1 } : null;
  }
  switch (e.key) {
    case "Enter":
      return place === "page" ? { kind: "accept" } : null;
    case "ArrowRight":
      return place === "composite" ? null : { kind: "skip" };
    case "ArrowLeft":
      return place === "composite" ? null : { kind: "back" };
  }
  return LETTERS[e.key.toLowerCase()] ?? null;
}

/**
 * The elements of each place, by selector, in the order they win: a dialog
 * over all it holds, a field, a composite control (whose items are
 * buttons), then a control.
 */
export const PLACES: readonly (readonly [KeyPlace, string])[] = [
  ["dialog", "dialog, [role=dialog], [role=alertdialog]"],
  [
    "field",
    "input, textarea, select, [contenteditable]:not([contenteditable=false]), [role=textbox], [role=combobox], [role=listbox], [role=menu], [role=menuitem], [role=tree], [role=treeitem], [role=grid]",
  ],
  [
    "composite",
    "[role=radiogroup], [role=radio], [role=tablist], [role=tab], [role=slider]",
  ],
  ["control", "button, a[href], [role=button], summary"],
];

/**
 * Where a key press landed: in a dialog whenever one of the app's modal
 * dialogs is open (`modalOpen`), wherever focus is; else the first place
 * whose elements hold the target (PLACES); else the page.
 *
 * @example
 * placeOf(document.activeElement, !!document.querySelector("dialog[open]"));
 */
export function placeOf(
  target: Pick<Element, "closest"> | null,
  modalOpen: boolean,
): KeyPlace {
  if (modalOpen) return "dialog";
  return (
    PLACES.find(([, selector]) => !!target?.closest(selector))?.[0] ?? "page"
  );
}

/**
 * Calls `on` with each key command on the page while `enabled`, for a card
 * offering `choices`.
 */
export function useWalkKeys(
  on: (c: KeyCommand) => void,
  enabled: boolean,
  choices: number,
): void {
  const handler = useRef(on);
  useEffect(() => {
    handler.current = on;
  });
  useEffect(() => {
    if (!enabled) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing) return;
      const command = keyCommand(
        e,
        placeOf(
          e.target instanceof Element ? e.target : null,
          !!document.querySelector("dialog[open]"),
        ),
        choices,
      );
      if (!command) return;
      e.preventDefault();
      handler.current(command);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [enabled, choices]);
}
