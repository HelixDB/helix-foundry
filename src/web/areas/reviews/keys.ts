import { useEffect, useRef } from "react";

// The review walkthrough's keys. A digit saves its choice at once (Z undoes
// it); Enter accepts Founder's suggestion; nothing is preselected. Keys are
// off while the person types, and ⌘J, ⌘K and every other modified key stay
// with the shell and the browser.

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
 * choices; null for none, and for every key while the person types
 * (`editable`). ⌘Z or Ctrl+Z is Undo; any other modified key is no command,
 * and so is Shift with anything but `?`.
 *
 * @example
 * keyCommand({ key: "2", metaKey: false, ctrlKey: false, altKey: false, shiftKey: false }, false, 3); // { kind: "choose", index: 1 }
 * keyCommand({ key: "k", metaKey: true, ctrlKey: false, altKey: false, shiftKey: false }, false, 3); // null
 */
export function keyCommand(
  e: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "altKey" | "shiftKey">,
  editable: boolean,
  choices: number,
): KeyCommand | null {
  if (editable || e.altKey) return null;
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
      return { kind: "accept" };
    case "ArrowRight":
      return { kind: "skip" };
    case "ArrowLeft":
      return { kind: "back" };
  }
  return LETTERS[e.key.toLowerCase()] ?? null;
}

/**
 * Calls `on` with each key command on the page while `enabled` (off while a
 * dialog of the walk is open), for a card offering `choices`. A field takes
 * its own keys, and Enter on a button or link is that control's.
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
      const target = e.target instanceof Element ? e.target : null;
      const editable =
        !!target &&
        ((target instanceof HTMLElement && target.isContentEditable) ||
          !!target.closest(
            "input, textarea, select, [role=textbox], [role=combobox], [role=listbox]",
          ) ||
          (e.key === "Enter" &&
            !!target.closest("button, a[href], [role=button], summary")));
      const command = keyCommand(e, editable, choices);
      if (!command) return;
      e.preventDefault();
      handler.current(command);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [enabled, choices]);
}
