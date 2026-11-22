import { describe, expect, it } from "vitest";
import {
  PLACES,
  keyCommand,
  placeOf,
  type KeyCommand,
  type KeyPlace,
} from "@/web/areas/reviews/keys";

// The review walkthrough's keys (reviews/keys.ts keyCommand, placeOf).

const press = (
  key: string,
  mods: Partial<{
    metaKey: boolean;
    ctrlKey: boolean;
    altKey: boolean;
    shiftKey: boolean;
    repeat: boolean;
  }> = {},
  { place = "page" as KeyPlace, choices = 3 } = {},
) =>
  keyCommand(
    {
      key,
      metaKey: false,
      ctrlKey: false,
      altKey: false,
      shiftKey: false,
      repeat: false,
      ...mods,
    },
    place,
    choices,
  );

describe("keyCommand", () => {
  it("reads every key of the walk", () => {
    const keys: [string, KeyCommand][] = [
      ["Enter", { kind: "accept" }],
      ["1", { kind: "choose", index: 0 }],
      ["3", { kind: "choose", index: 2 }],
      ["n", { kind: "not_in_data" }],
      ["x", { kind: "reject" }],
      ["s", { kind: "skip" }],
      ["ArrowRight", { kind: "skip" }],
      ["ArrowLeft", { kind: "back" }],
      ["z", { kind: "undo" }],
      ["g", { kind: "batch" }],
      ["o", { kind: "open" }],
      ["f", { kind: "founder" }],
      // Caps Lock on.
      ["F", { kind: "founder" }],
    ];
    for (const [key, command] of keys) expect(press(key)).toEqual(command);
    expect(press("?", { shiftKey: true })).toEqual({ kind: "help" });
  });
  it("reads no digit past the card's choices, nor 0", () => {
    expect(press("4")).toBeNull();
    expect(press("9", {}, { choices: 9 })).toEqual({
      kind: "choose",
      index: 8,
    });
    expect(press("1", {}, { choices: 0 })).toBeNull();
    expect(press("0")).toBeNull();
  });
  it("reads no key held down: its repeats would decide one card after another", () => {
    for (const key of ["Enter", "1", "n", "x", "s", "ArrowRight", "z", "g"])
      expect(press(key, { repeat: true })).toBeNull();
    expect(press("z", { metaKey: true, repeat: true })).toBeNull();
  });
  it("reads nothing in a field or while a dialog is open", () => {
    for (const place of ["field", "dialog"] as const)
      for (const key of ["Enter", "1", "n", "x", "ArrowRight", "?"]) {
        expect(press(key, {}, { place })).toBeNull();
        expect(press("z", { metaKey: true }, { place })).toBeNull();
      }
  });
  it("leaves Enter to a control, and the arrows and Enter to a radio group or tabs", () => {
    expect(press("Enter", {}, { place: "control" })).toBeNull();
    expect(press("ArrowRight", {}, { place: "control" })).toEqual({
      kind: "skip",
    });
    expect(press("1", {}, { place: "control" })).toEqual({
      kind: "choose",
      index: 0,
    });
    for (const key of ["Enter", "ArrowRight", "ArrowLeft"])
      expect(press(key, {}, { place: "composite" })).toBeNull();
    expect(press("s", {}, { place: "composite" })).toEqual({ kind: "skip" });
    expect(press("z", { metaKey: true }, { place: "composite" })).toEqual({
      kind: "undo",
    });
  });
  it("leaves ⌘J, ⌘K and other modified keys to the shell", () => {
    expect(press("j", { metaKey: true })).toBeNull();
    expect(press("k", { metaKey: true })).toBeNull();
    expect(press("k", { ctrlKey: true })).toBeNull();
    expect(press("s", { altKey: true })).toBeNull();
    expect(press("S", { shiftKey: true })).toBeNull();
    expect(press("Z", { metaKey: true, shiftKey: true })).toBeNull();
    expect(press("q")).toBeNull();
    expect(press("Tab")).toBeNull();
  });
  it("reads ⌘Z and Ctrl+Z as Undo", () => {
    expect(press("z", { metaKey: true })).toEqual({ kind: "undo" });
    expect(press("z", { ctrlKey: true })).toEqual({ kind: "undo" });
  });
});

describe("placeOf", () => {
  /** An element inside these, each a selector part as PLACES write it. */
  const inside = (...parts: string[]) => ({
    closest: (selector: string) =>
      selector
        .split(",")
        .map((p) => p.trim())
        .some((p) => parts.includes(p))
        ? ({} as Element)
        : null,
  });
  it("is a dialog whenever one is open, wherever focus is", () => {
    expect(placeOf(null, true)).toBe("dialog");
    expect(placeOf(inside("button"), true)).toBe("dialog");
  });
  it("tells each place, a dialog over a field over a composite over a control", () => {
    const cases: [string[], KeyPlace][] = [
      [["[role=dialog]", "button"], "dialog"],
      [["textarea"], "field"],
      [["input"], "field"],
      [["select"], "field"],
      [["[contenteditable]:not([contenteditable=false])"], "field"],
      [["[role=combobox]"], "field"],
      [["[role=tree]", "button"], "field"],
      [["[role=menu]"], "field"],
      [["[role=radiogroup]", "button"], "composite"],
      [["[role=radio]", "button"], "composite"],
      [["[role=tab]", "button"], "composite"],
      [["button"], "control"],
      [["a[href]"], "control"],
      [["summary"], "control"],
      [[], "page"],
    ];
    for (const [parts, place] of cases)
      expect(placeOf(inside(...parts), false)).toBe(place);
    expect(placeOf(null, false)).toBe("page");
  });
  it("names each place once, in the order they win", () => {
    expect(PLACES.map(([place]) => place)).toEqual([
      "dialog",
      "field",
      "composite",
      "control",
    ]);
  });
});
