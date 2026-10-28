import { useEffect } from "react";
import { attach, start, type Anchor } from "./field/field";
import { page } from "./page";

// The elements the field pins its formations to, besides the schema and the
// Explorer, which pin their own.
const ANCHORS: [Anchor, string][] = [
  ["hero", '[data-field="hero"]'],
  ["cloud", "#cloud"],
  ["floor", '[data-field="floor"]'],
  ["analyst", '[data-field="analyst"]'],
  ["shout", '[data-field="shout"]'],
];

// Starts the field behind the page and the page's small behaviours, and
// stops them when the page goes.
export function Behaviours() {
  useEffect(() => {
    ANCHORS.forEach(([name, selector]) =>
      attach(name, document.querySelector<HTMLElement>(selector)),
    );
    // The field draws text, so it waits for the fonts before its first frame.
    let stop: (() => void) | undefined,
      gone = false;
    document.fonts.ready.then(() => {
      if (gone) return;
      document.documentElement.classList.add("has-field");
      stop = start(
        document.querySelector<HTMLCanvasElement>("[data-field-canvas]")!,
      );
    });
    const undo = page();
    return () => {
      gone = true;
      stop?.();
      undo();
      ANCHORS.forEach(([name]) => attach(name, null));
    };
  }, []);
  return null;
}
